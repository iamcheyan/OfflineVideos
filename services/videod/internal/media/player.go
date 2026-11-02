package media

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"kanade/videod/internal/store"
)

func (s *Service) playerAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		apiError(w, 404, "not_found", "player action not found")
		return
	}
	action := parts[2]
	if action == "play" {
		var in struct {
			VideoID string `json:"video_id"`
			URL     string `json:"url"`
			Mode    string `json:"mode"`
		}
		if decodeJSON(r, &in) != nil {
			apiError(w, 400, "invalid_request", "video_id or url required")
			return
		}
		account, accountErr := store.ActiveAccount(r.Context(), s.db)
		if accountErr != nil {
			apiError(w, 500, "account_unavailable", "active account unavailable")
			return
		}
		if in.Mode == "" {
			in.Mode = "video"
		}
		if in.Mode != "video" && in.Mode != "audio" {
			apiError(w, 400, "invalid_mode", "unsupported media mode")
			return
		}
		if in.Mode == "audio" {
			if in.VideoID == "" {
				apiError(w, 400, "invalid_request", "video_id is required for audio playback")
				return
			}
			if err := s.db.QueryRowContext(r.Context(), `SELECT output_path FROM downloads WHERE account_id=? AND video_id=? AND mode='audio' AND state='completed' AND output_path<>'' ORDER BY created_at DESC,id DESC LIMIT 1`, account, in.VideoID).Scan(&in.URL); err != nil {
				apiError(w, 404, "audio_not_found", "offline audio not found")
				return
			}
		} else if in.URL == "" {
			if in.VideoID == "" {
				apiError(w, 400, "invalid_request", "video_id or url required")
				return
			}
			if err := s.db.QueryRowContext(r.Context(), `SELECT url FROM videos WHERE id=? AND account_id=?`, in.VideoID, account).Scan(&in.URL); err != nil {
				apiError(w, 404, "video_not_found", "video not found")
				return
			}
		}
		resume := 0.0
		if in.Mode == "video" {
			resume = s.resumePosition(account, in.VideoID)
		}
		p, err := s.launch(in.URL, resume)
		if err != nil {
			apiError(w, 503, "player_unavailable", "unable to start mpv")
			return
		}
		p.videoID = in.VideoID
		s.mu.Lock()
		if old := s.players["current"]; old != nil && old.cmd.Process != nil {
			_ = old.cmd.Process.Signal(syscall.SIGTERM)
		}
		s.players["current"] = p
		s.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"state": "playing", "video_id": in.VideoID, "resume_sec": resume})
		return
	}
	s.mu.Lock()
	p := s.players["current"]
	s.mu.Unlock()
	if p == nil {
		apiError(w, 409, "player_idle", "no video is loaded")
		return
	}
	var body struct {
		Value float64 `json:"value"`
	}
	_ = decodeJSON(r, &body)
	var cmd []any
	switch action {
	case "pause":
		cmd = []any{"cycle", "pause"}
	case "stop":
		cmd = []any{"stop"}
	case "seek":
		cmd = []any{"seek", body.Value, "absolute"}
	case "volume":
		cmd = []any{"set_property", "volume", body.Value}
	case "speed":
		cmd = []any{"set_property", "speed", body.Value}
	case "fullscreen":
		cmd = []any{"cycle", "fullscreen"}
	default:
		apiError(w, 404, "not_found", "player action not found")
		return
	}
	if _, err := p.request(map[string]any{"command": cmd}); err != nil {
		apiError(w, 502, "player_ipc_error", "mpv command failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Service) playerState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	p := s.players["current"]
	s.mu.Unlock()
	if p == nil {
		writeJSON(w, 200, map[string]any{"state": "idle"})
		return
	}
	props := map[string]any{}
	for _, name := range []string{"time-pos", "duration", "pause", "volume", "speed", "fullscreen"} {
		res, err := p.request(map[string]any{"command": []any{"get_property", name}})
		if err != nil {
			continue
		}
		props[name] = res["data"]
	}
	props["video_id"] = p.videoID
	props["state"] = "playing"
	writeJSON(w, 200, props)
}
func (s *Service) launch(url string, resumeSec float64) (*player, error) {
	exe, err := exec.LookPath(s.cfg.MpvPath)
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(s.cfg.IPCDir, fmt.Sprintf("pocket-mpv-%d.sock", time.Now().UnixNano()))
	args := []string{"--no-config", "--no-terminal", "--vo=null", "--ao=null", "--idle=no", "--input-ipc-server=" + sock}
	if resumeSec > 0 {
		args = append(args, fmt.Sprintf("--start=%.3f", resumeSec))
	}
	args = append(args, url)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY=")
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	for range 100 {
		conn, e := net.DialTimeout("unix", sock, 100*time.Millisecond)
		if e == nil {
			return &player{cmd: cmd, conn: conn, reader: bufio.NewReader(conn)}, nil
		}
		if cmd.ProcessState != nil {
			return nil, errors.New("mpv exited before IPC became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	return nil, errors.New("mpv IPC socket unavailable")
}
func (p *player) request(v any) (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requestID++
	id := p.requestID
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("invalid mpv command")
	}
	m["request_id"] = id
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	_ = p.conn.SetDeadline(time.Now().Add(3 * time.Second))
	defer p.conn.SetDeadline(time.Time{})
	if _, err = p.conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	if p.reader == nil {
		p.reader = bufio.NewReader(p.conn)
	}
	for {
		line, err := p.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err = json.Unmarshal(line, &out); err != nil {
			return nil, err
		}
		if rid, ok := out["request_id"].(float64); ok && uint64(rid) == id {
			return out, nil
		}
	}
}
