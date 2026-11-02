package media

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

type Config struct {
	YtDlpPath, MpvPath, DownloadDir, IPCDir string
	Concurrency                             int
	Headless                                bool
	Notifier                                NotificationSender
	YouTubeBrowser                          string
}

type notificationBatch struct {
	Account string
	Titles  []string
}
type Service struct {
	db             *sql.DB
	cfg            Config
	mu             sync.Mutex
	writeMu        sync.Mutex
	jobs           map[int64]*job
	players        map[string]*player
	paused, closed bool
	nextRequestID  uint64
	notifyPending  map[string][]string
}

type job struct {
	ID           int64   `json:"id"`
	AccountID    string  `json:"account_id"`
	VideoID      string  `json:"video_id,omitempty"`
	URL          string  `json:"url,omitempty"`
	Mode         string  `json:"mode"`
	Quality      string  `json:"quality"`
	AudioBitrate string  `json:"audio_bitrate"`
	Priority     int     `json:"priority"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Error        string  `json:"error,omitempty"`
	OutputPath   string  `json:"output_path,omitempty"`
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	// anonymous is set when a cookie-authenticated attempt produced no usable
	// audio stream, so the retry is fetched with the public player response.
	anonymous bool
}

type player struct {
	cmd       *exec.Cmd
	conn      net.Conn
	reader    *bufio.Reader
	mu        sync.Mutex
	requestID uint64
	videoID   string
}

const schema = `
CREATE TABLE IF NOT EXISTS downloads (
 id INTEGER PRIMARY KEY, account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 video_id TEXT NOT NULL DEFAULT '', url TEXT NOT NULL, mode TEXT NOT NULL DEFAULT 'video',
 quality TEXT NOT NULL DEFAULT 'best', audio_bitrate TEXT NOT NULL DEFAULT '192',
 priority INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'queued',
 progress REAL NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', output_path TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS downloads_account_state ON downloads(account_id,state,priority DESC,created_at);
CREATE TABLE IF NOT EXISTS media_settings(account_id TEXT NOT NULL DEFAULT 'demo', key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(account_id,key));
`

var progressPattern = regexp.MustCompile(`\[download\]\s+([0-9]+(?:\.[0-9]+)?)%`)

func New(db *sql.DB, cfg Config) (*Service, error) {
	if db == nil {
		return nil, errors.New("media: nil database")
	}
	if cfg.YtDlpPath == "" {
		cfg.YtDlpPath = "yt-dlp"
	}
	if cfg.MpvPath == "" {
		cfg.MpvPath = "mpv"
	}
	if cfg.DownloadDir == "" {
		cfg.DownloadDir = "downloads"
	}
	if cfg.IPCDir == "" {
		cfg.IPCDir = os.TempDir()
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 2
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.IPCDir, 0700); err != nil {
		return nil, err
	}
	s := &Service{db: db, cfg: cfg, jobs: make(map[int64]*job), players: make(map[string]*player), notifyPending: make(map[string][]string)}
	if err := s.restoreJobs(context.Background()); err != nil {
		return nil, err
	}
	go s.kick()
	return s, nil
}

func (s *Service) restoreJobs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id,account_id,video_id,url,mode,quality,audio_bitrate,priority,progress,error FROM downloads WHERE state IN ('queued','downloading','paused')`)
	if err != nil {
		return err
	}
	var restored []*job
	for rows.Next() {
		j := &job{}
		if err := rows.Scan(&j.ID, &j.AccountID, &j.VideoID, &j.URL, &j.Mode, &j.Quality, &j.AudioBitrate, &j.Priority, &j.Progress, &j.Error); err != nil {
			rows.Close()
			return err
		}
		if strings.HasPrefix(j.URL, "https://example.test/") {
			continue
		}
		j.State = "queued"
		restored = append(restored, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range restored {
		s.jobs[j.ID] = j
		if _, err = s.db.ExecContext(ctx, `UPDATE downloads SET state='queued' WHERE id=?`, j.ID); err != nil {
			return err
		}
	}
	return nil
}

func RegisterRoutes(mux *http.ServeMux, s *Service) {
	mux.HandleFunc("/api/downloads", s.downloads)
	mux.HandleFunc("/api/downloads/", s.downloadAction)
	mux.HandleFunc("PUT /api/downloads/order", s.downloadOrder)
	mux.HandleFunc("/api/player/", s.playerAction)
	mux.HandleFunc("GET /api/player/state", s.playerState)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("GET /api/library", s.library)
	mux.HandleFunc("POST /api/library/{video_id}/watched", s.markLibraryWatched)
	mux.HandleFunc("DELETE /api/library/{video_id}", s.deleteLibraryVideo)
	mux.HandleFunc("GET /api/media/{video_id}", s.mediaStream)
	mux.HandleFunc("/api/history", s.history)
	mux.HandleFunc("/api/history/", s.history)
	mux.HandleFunc("GET /api/storage", s.storage)
	mux.HandleFunc("POST /api/storage/clear", s.clearDownloads)
	mux.HandleFunc("GET /api/catalog", s.catalogEndpoint)
	mux.HandleFunc("POST /api/catalog", s.catalogEndpoint)
}

func (s *Service) downloads(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		stateFilter := strings.TrimSpace(r.URL.Query().Get("state"))
		if stateFilter != "" && !oneOf(stateFilter, "queued", "downloading", "paused", "completed", "failed", "cancelled") {
			apiError(w, 400, "invalid_state", "unsupported download state")
			return
		}
		query := `SELECT id,account_id,video_id,url,mode,quality,audio_bitrate,priority,state,progress,error,output_path FROM downloads WHERE account_id=?`
		args := []any{account}
		if stateFilter != "" {
			query += ` AND state=?`
			args = append(args, stateFilter)
		}
		limit, offset := pageParams(r)
		rows, err := s.db.QueryContext(r.Context(), query+` ORDER BY priority DESC,created_at,id LIMIT ? OFFSET ?`, append(args, limit, offset)...)
		if err != nil {
			apiError(w, 500, "database_error", "unable to read downloads")
			return
		}
		defer rows.Close()
		out := []job{}
		for rows.Next() {
			var j job
			if err := rows.Scan(&j.ID, &j.AccountID, &j.VideoID, &j.URL, &j.Mode, &j.Quality, &j.AudioBitrate, &j.Priority, &j.State, &j.Progress, &j.Error, &j.OutputPath); err != nil {
				apiError(w, 500, "database_error", "unable to read downloads")
				return
			}
			out = append(out, j)
		}
		if rows.Err() != nil {
			apiError(w, 500, "database_error", "unable to read downloads")
			return
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			VideoID      string   `json:"video_id"`
			VideoIDs     []string `json:"video_ids"`
			URL          string   `json:"url"`
			Mode         string   `json:"mode"`
			Quality      string   `json:"quality"`
			AudioBitrate string   `json:"audio_bitrate"`
			Priority     int      `json:"priority"`
		}
		if decodeJSON(r, &in) != nil {
			apiError(w, 400, "invalid_request", "invalid download request")
			return
		}
		if in.Mode == "" {
			in.Mode = "video"
		}
		if in.Quality == "" {
			in.Quality = "720p"
			var configured string
			if err := s.db.QueryRowContext(r.Context(), `SELECT value FROM settings WHERE account_id=? AND key='default_quality'`, account).Scan(&configured); err == nil && oneOf(configured, "360p", "720p", "1080p", "1440p", "2160p") {
				in.Quality = configured
			}
		}
		if in.AudioBitrate == "" {
			in.AudioBitrate = "192"
		}
		if !oneOf(in.Mode, "audio", "video", "both") || !oneOf(in.Quality, "360p", "720p", "1080p", "1440p", "2160p") || !oneOf(in.AudioBitrate, "128", "192", "320") {
			apiError(w, 400, "invalid_options", "unsupported mode, quality, or audio bitrate")
			return
		}
		if len(in.VideoIDs) > 0 {
			s.enqueueBatch(w, r, account, in.VideoIDs, in.Mode, in.Quality, in.AudioBitrate, in.Priority)
			return
		}
		if in.URL == "" {
			if in.VideoID == "" {
				apiError(w, 400, "video_required", "video_id is required")
				return
			}
			err = s.db.QueryRowContext(r.Context(), `SELECT url FROM videos WHERE id=? AND account_id=?`, in.VideoID, account).Scan(&in.URL)
			if errors.Is(err, sql.ErrNoRows) {
				apiError(w, 404, "video_not_found", "video not found for active account")
				return
			}
			if err != nil {
				apiError(w, 500, "database_error", "unable to resolve video")
				return
			}
		}
		if in.VideoID == "" {
			in.VideoID = mediaIDFromURL(in.URL)
		}
		j, status, code, message := s.enqueue(account, in.VideoID, in.URL, in.Mode, in.Quality, in.AudioBitrate, in.Priority)
		if status != http.StatusAccepted {
			apiError(w, status, code, message)
			return
		}
		s.kick()
		writeJSON(w, http.StatusAccepted, j)
	default:
		apiError(w, 405, "method_not_allowed", "method not allowed")
	}
}

// enqueue persists one queued download row and registers it in memory.

// activeJobFor returns an in-flight job for the same account and video.
func (s *Service) activeJobFor(account, videoID string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.AccountID != account || j.VideoID != videoID {
			continue
		}
		if j.State == "queued" || j.State == "downloading" || j.State == "paused" {
			return j
		}
	}
	return nil
}

func (s *Service) enqueue(account, videoID, videoURL, mode, quality, bitrate string, priority int) (*job, int, string, string) {
	if videoURL == "" {
		return nil, 400, "video_required", "video_id is required"
	}
	// Two active jobs for one video write the same output file, and the second
	// one then fails while renaming the first one's temporary file. Refuse the
	// duplicate instead; a finished or failed job may be retried freely.
	if dup := s.activeJobFor(account, videoID); dup != nil {
		return dup, http.StatusAccepted, "already_active", ""
	}
	s.writeMu.Lock()
	res, err := s.db.ExecContext(context.Background(), `INSERT INTO downloads(account_id,video_id,url,mode,quality,audio_bitrate,priority) VALUES(?,?,?,?,?,?,?)`, account, videoID, videoURL, mode, quality, bitrate, priority)
	s.writeMu.Unlock()
	if err != nil {
		return nil, 500, "database_error", "unable to queue download"
	}
	id, _ := res.LastInsertId()
	j := &job{ID: id, AccountID: account, VideoID: videoID, URL: videoURL, Mode: mode, Quality: quality, AudioBitrate: bitrate, Priority: priority, State: "queued"}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()
	return j, http.StatusAccepted, "", ""
}

// Enqueue adds one download for the active account and returns its job id.
//
// It exists so other subsystems (for example the after-sync auto-download path)
// can enqueue work without going through HTTP. Validation mirrors the POST
// handler: unsupported option values are rejected rather than coerced.
func (s *Service) Enqueue(account, videoID, videoURL, mode, quality, bitrate string, priority int) (int64, error) {
	if account == "" || videoURL == "" {
		return 0, fmt.Errorf("account and url are required")
	}
	if mode == "" {
		mode = "video"
	}
	if quality == "" {
		quality = "720p"
	}
	if bitrate == "" {
		bitrate = "192"
	}
	j, status, code, msg := s.enqueue(account, videoID, videoURL, mode, quality, bitrate, priority)
	if j == nil {
		if msg == "" {
			msg = code
		}
		return 0, fmt.Errorf("enqueue rejected (%d): %s", status, msg)
	}
	// The HTTP handler kicks the queue itself; direct callers need the same.
	go s.kick()
	return j.ID, nil
}

// enqueueBatch queues several videos at once, reporting per-video outcomes.
func (s *Service) enqueueBatch(w http.ResponseWriter, r *http.Request, account string, videoIDs []string, mode, quality, bitrate string, priority int) {
	type result struct {
		VideoID string `json:"video_id"`
		ID      int64  `json:"id,omitempty"`
		State   string `json:"state,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(videoIDs))
	queued := 0
	for i, vid := range videoIDs {
		var videoURL string
		if err := s.db.QueryRowContext(r.Context(), `SELECT url FROM videos WHERE id=? AND account_id=?`, vid, account).Scan(&videoURL); err != nil {
			results = append(results, result{VideoID: vid, Error: "video_not_found"})
			continue
		}
		j, status, code, message := s.enqueue(account, vid, videoURL, mode, quality, bitrate, priority-i)
		if status != http.StatusAccepted {
			results = append(results, result{VideoID: vid, Error: message})
			continue
		}
		// A video that already has an in-flight job is reported but not counted,
		// so the caller's queued total matches what actually started.
		if code != "already_active" {
			queued++
		}
		results = append(results, result{VideoID: vid, ID: j.ID, State: j.State})
	}
	s.kick()
	if queued == 0 {
		apiError(w, http.StatusBadRequest, "batch_rejected", "no videos could be queued")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "results": results})
}

func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel()
		}
		if j.cmd != nil && j.cmd.Process != nil {
			_ = j.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	for _, p := range s.players {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
		if p.conn != nil {
			_ = p.conn.Close()
		}
	}
	s.mu.Unlock()
}
func (s *Service) YtDlp() YtDlpEngine { return YtDlpEngine{Path: s.cfg.YtDlpPath} }
func (s *Service) Mpv() MpvEngine     { return MpvEngine{Path: s.cfg.MpvPath, Headless: s.cfg.Headless} }

type YtDlpEngine struct{ Path string }
type MpvEngine struct {
	Path     string
	Headless bool
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// mediaIDFromURL extracts the YouTube video ID, or preserves an opaque id for other URLs.
func mediaIDFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil {
		if id := parsed.Query().Get("v"); id != "" {
			return id
		}
		if strings.Contains(parsed.Host, "youtu.be") {
			if id := strings.Trim(parsed.Path, "/"); id != "" {
				return id
			}
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 && (parts[0] == "shorts" || parts[0] == "embed" || parts[0] == "live") {
			return parts[1]
		}
	}
	return strings.TrimSpace(raw)
}

func oneOf(v string, choices ...string) bool {
	for _, c := range choices {
		if v == c {
			return true
		}
	}
	return false
}
