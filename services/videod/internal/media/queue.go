package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Service) concurrency(accountID string) int {
	limit := s.cfg.Concurrency
	var configured string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE account_id=? AND key='concurrency'`, accountID).Scan(&configured); err == nil {
		if n, err := strconv.Atoi(configured); err == nil && n >= 1 && n <= 8 {
			limit = n
		}
	}
	return limit
}

func (s *Service) kick() {
	s.mu.Lock()
	if s.paused || s.closed {
		s.mu.Unlock()
		return
	}
	running := 0
	queued := []*job{}
	for _, j := range s.jobs {
		if j.State == "downloading" {
			running++
		} else if j.State == "queued" {
			queued = append(queued, j)
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].Priority == queued[j].Priority {
			return queued[i].ID < queued[j].ID
		}
		return queued[i].Priority > queued[j].Priority
	})
	limit := s.cfg.Concurrency
	if len(queued) > 0 {
		limit = s.concurrency(queued[0].AccountID)
	}
	capacity := limit - running
	if capacity > len(queued) {
		capacity = len(queued)
	}
	if capacity < 0 {
		capacity = 0
	}
	selected := append([]*job(nil), queued[:max(capacity, 0)]...)
	s.mu.Unlock()
	for _, j := range selected {
		s.start(j)
	}
}
func (s *Service) start(j *job) {
	s.mu.Lock()
	if s.closed || s.paused || j.State != "queued" {
		s.mu.Unlock()
		return
	}
	running := 0
	for _, x := range s.jobs {
		if x.State == "downloading" {
			running++
		}
	}
	if running >= s.concurrency(j.AccountID) {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.State = "downloading"
	_ = s.persistLocked(j)
	s.mu.Unlock()
	go s.run(ctx, j)
}

// downloadFormat builds a yt-dlp selector with an explicit fallback chain.
//
// The old chain ended at "best", which only matches a single stream that is
// already muxed. On videos whose only 720p-or-below option is a separate
// video-only stream, the first two alternatives fail and "best" does not
// satisfy "one file containing audio and video" either, so yt-dlp reported
// "Requested format is not available". Adding an explicit
// "bestvideo*+bestaudio/best*" stage before the final "best" keeps the
// preferred resolution but always lands on something playable.
func downloadFormat(j *job) string {
	height := strings.TrimSuffix(j.Quality, "p")
	switch j.Mode {
	case "audio":
		return "bestaudio[abr<=" + j.AudioBitrate + "]/bestaudio/best"
	case "both":
		return "bestvideo*[height<=" + height + "]+bestaudio/best[height<=" + height +
			"]/bestvideo*+bestaudio/best"
	default:
		return "bestvideo*[height<=" + height + "]+bestaudio/best[height<=" + height +
			"]/bestvideo*+bestaudio/best"
	}
}

// retryWithoutCookies re-runs the job with the anonymous player response.
//
// Returns (true, true) when the retry was started, and (false, false) when the
// job cannot be retried (already anonymous, or no cookie file).
// hasAudioOnlyWithoutCookies reports whether the anonymous player response
// exposes an audio-only stream. That distinguishes "the cookie session broke
// format selection" from "the video has no audio at all / is gone".
func (s *Service) hasAudioOnlyWithoutCookies(j *job) bool {
	if j.AccountID != "youtube" || j.URL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.YtDlpPath,
		"--no-warnings", "--skip-download", "--list-formats", j.URL)
	if j.AccountID == "youtube" && s.cfg.YouTubeBrowser != "" {
		cmd.Args = append(cmd.Args[:1], append([]string{"--js-runtimes", "node"}, cmd.Args[1:]...)...)
		cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "--cookies-from-browser", s.cfg.YouTubeBrowser, "--js-runtimes", "node", cmd.Args[len(cmd.Args)-1])
	}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "audio only") {
			return true
		}
	}
	return false
}

// retryWithoutCookies requeues the job so the next attempt skips the cookie jar.
func (s *Service) retryWithoutCookies(j *job) bool {
	s.mu.Lock()
	j.State = "queued"
	j.Progress = 0
	j.Error = ""
	j.anonymous = true
	s.mu.Unlock()
	if err := s.persistLocked(j); err != nil {
		return false
	}
	s.kick()
	return true
}

func (s *Service) run(ctx context.Context, j *job) {
	if err := os.MkdirAll(s.cfg.DownloadDir, 0750); err != nil {
		s.finish(j, "failed", err)
		return
	}
	format := downloadFormat(j)
	template := filepath.Join(s.cfg.DownloadDir, "%(id)s.%(ext)s")
	args := []string{"--newline", "--no-warnings", "-f", format, "-o", template, "--write-thumbnail", "--convert-thumbnails", "jpg", "--print-json", "--print", "after_move:filepath", "--continue"}
	if j.AccountID == "youtube" && s.cfg.YouTubeBrowser != "" {
		args = append(args, "--js-runtimes", "node")
	}
	dataDir := filepath.Dir(s.cfg.DownloadDir)
	cookieFile := "cookies.txt"
	if j.AccountID == "bilibili" {
		cookieFile = "bilibili-cookies.txt"
	}
	cookiePath := filepath.Join(dataDir, cookieFile)
	if j.AccountID == "youtube" && s.cfg.YouTubeBrowser != "" {
		args = append(args, "--cookies-from-browser", s.cfg.YouTubeBrowser, "--js-runtimes", "node")
	} else if _, err := os.Stat(cookiePath); err == nil && !j.anonymous {
		args = append(args, "--cookies", cookiePath)
	}
	args = append(args, j.URL)
	cmd := exec.Command(s.cfg.YtDlpPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.finish(j, "failed", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.finish(j, "failed", err)
		return
	}
	if err = cmd.Start(); err != nil {
		s.finish(j, "failed", err)
		return
	}
	s.mu.Lock()
	j.cmd = cmd
	s.mu.Unlock()
	var stderrBuf strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t := strings.TrimSpace(sc.Text())
			if t != "" && !strings.HasPrefix(t, "WARNING:") {
				if stderrBuf.Len() > 0 {
					stderrBuf.WriteString("; ")
				}
				stderrBuf.WriteString(t)
			}
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var outputPath string
	var metadata map[string]any
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "{") {
			var item map[string]any
			if json.Unmarshal([]byte(line), &item) == nil {
				metadata = item
			}
			continue
		}
		if m := progressPattern.FindStringSubmatch(line); len(m) == 2 {
			if v, e := strconv.ParseFloat(m[1], 64); e == nil {
				s.mu.Lock()
				j.Progress = v
				_ = s.persistLocked(j)
				s.mu.Unlock()
			}
			continue
		}
		if strings.HasPrefix(line, "/") {
			outputPath = line
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	<-stderrDone
	s.mu.Lock()
	j.cmd = nil
	s.mu.Unlock()
	if ctx.Err() != nil {
		s.mu.Lock()
		if j.State == "downloading" {
			j.State = "queued"
		}
		_ = s.persistLocked(j)
		s.mu.Unlock()
		s.kick()
		return
	}
	if scanErr != nil {
		s.finish(j, "failed", scanErr)
		return
	}
	if waitErr != nil {
		msg := stderrBuf.String()
		if msg == "" {
			msg = "yt-dlp download failed"
		}
		// A signed-in cookie jar can make YouTube answer with a player response
		// that has video-only formats, so "bestvideo+bestaudio" has no audio to
		// pair with. Only retry when that is provably the cause: a video that is
		// genuinely gone would otherwise cost a second failed attempt.
		if strings.Contains(msg, "Requested format is not available") && s.hasAudioOnlyWithoutCookies(j) {
			if retried := s.retryWithoutCookies(j); retried {
				return
			}
		}
		s.finish(j, "failed", errors.New(msg))
		return
	}
	if err := s.updateCatalog(j, metadata, outputPath); err != nil {
		s.finish(j, "failed", fmt.Errorf("write media catalog: %w", err))
		return
	}
	j.Progress = 100
	j.OutputPath = outputPath
	s.mu.Lock()
	j.State = "completed"
	_ = s.persistLocked(j)
	s.mu.Unlock()
	if s.cfg.Notifier != nil {
		var title string
		if metadata != nil {
			title, _ = metadata["title"].(string)
		}
		s.queueCompletionNotice(j.AccountID, title)
	}
	s.kick()
}

func (s *Service) queueCompletionNotice(account, title string) {
	s.mu.Lock()
	s.notifyPending[account] = append(s.notifyPending[account], title)
	pending := len(s.notifyPending[account])
	s.mu.Unlock()
	if pending != 1 {
		return
	}
	go func() {
		time.Sleep(10 * time.Second)
		s.mu.Lock()
		titles := s.notifyPending[account]
		delete(s.notifyPending, account)
		s.mu.Unlock()
		if len(titles) == 0 {
			return
		}
		message := fmt.Sprintf("✅ OfflineVideos 缓存摘要：本批次成功下载 %d 个视频（%s）。", len(titles), account)
		if len(titles) <= 8 {
			for _, t := range titles {
				message += "\n• " + t
			}
		} else {
			message += "（视频数量较多，仅显示数量）"
		}
		message += "\nNAS：" + s.cfg.DownloadDir
		if err := s.cfg.Notifier.Send(context.Background(), message); err != nil {
			log.Printf("download summary notification failed account=%s: %v", account, err)
		}
	}()
}
func (s *Service) finish(j *job, state string, err error) {
	s.mu.Lock()
	j.State = state
	if err != nil {
		j.Error = err.Error()
	}
	j.cmd = nil
	_ = s.persistLocked(j)
	s.mu.Unlock()
	s.kick()
}
func (s *Service) persistLocked(j *job) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`UPDATE downloads SET state=?,progress=?,error=?,output_path=? WHERE id=? AND account_id=?`, j.State, j.Progress, j.Error, j.OutputPath, j.ID, j.AccountID)
	return err
}

func (s *Service) downloadAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		apiError(w, 404, "not_found", "download action not found")
		return
	}
	action := parts[2]
	if action == "pause" || action == "resume" {
		s.globalDownloadAction(w, r, action)
		return
	}
	id, err := strconv.ParseInt(action, 10, 64)
	if err != nil {
		apiError(w, 400, "invalid_id", "invalid download id")
		return
	}
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	s.mu.Lock()
	j := s.jobs[id]
	if j == nil || j.AccountID != account {
		s.mu.Unlock()
		apiError(w, 404, "download_not_found", "download not found")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.cancelJobLocked(j)
		j.State = "cancelled"
		_ = s.persistLocked(j)
		s.mu.Unlock()
		writeJSON(w, 200, j)
	case http.MethodPost:
		var in struct {
			Action   string `json:"action"`
			Priority int    `json:"priority"`
		}
		if decodeJSON(r, &in) != nil {
			s.mu.Unlock()
			apiError(w, 400, "invalid_request", "invalid action request")
			return
		}
		switch in.Action {
		case "pause":
			if j.cmd != nil && j.cmd.Process != nil {
				_ = j.cmd.Process.Signal(syscall.SIGSTOP)
			}
			j.State = "paused"
		case "resume":
			if j.State != "paused" {
				s.mu.Unlock()
				apiError(w, 409, "invalid_state", "download is not paused")
				return
			}
			if j.cmd != nil && j.cmd.Process != nil {
				_ = j.cmd.Process.Signal(syscall.SIGCONT)
			}
			j.State = "downloading"
		case "retry":
			if j.State != "failed" && j.State != "cancelled" {
				s.mu.Unlock()
				apiError(w, 409, "invalid_state", "only failed or cancelled downloads can retry")
				return
			}
			j.State = "queued"
			j.Progress = 0
			j.Error = ""
		case "cancel":
			s.cancelJobLocked(j)
			j.State = "cancelled"
		case "priority":
			j.Priority = in.Priority
		default:
			s.mu.Unlock()
			apiError(w, 400, "unknown_action", "unknown download action")
			return
		}
		_ = s.persistLocked(j)
		s.mu.Unlock()
		if in.Action == "retry" || in.Action == "resume" {
			s.kick()
		}
		writeJSON(w, 200, j)
	default:
		s.mu.Unlock()
		apiError(w, 405, "method_not_allowed", "method not allowed")
	}
}
func (s *Service) cancelJobLocked(j *job) {
	if j.cmd != nil && j.cmd.Process != nil {
		_ = j.cmd.Process.Signal(syscall.SIGTERM)
	}
	if j.cancel != nil {
		j.cancel()
	}
}
func (s *Service) globalDownloadAction(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		apiError(w, 405, "method_not_allowed", "method not allowed")
		return
	}
	s.mu.Lock()
	s.paused = action == "pause"
	for _, j := range s.jobs {
		if action == "pause" && j.State == "downloading" {
			if j.cmd != nil && j.cmd.Process != nil {
				_ = j.cmd.Process.Signal(syscall.SIGSTOP)
			}
			j.State = "paused"
			_ = s.persistLocked(j)
		} else if action == "resume" && j.State == "paused" {
			if j.cmd != nil && j.cmd.Process != nil {
				_ = j.cmd.Process.Signal(syscall.SIGCONT)
			}
			j.State = "downloading"
			_ = s.persistLocked(j)
		}
	}
	s.mu.Unlock()
	if action == "resume" {
		s.kick()
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Service) downloadOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if decodeJSON(r, &in) != nil {
		apiError(w, 400, "invalid_request", "ids array required")
		return
	}
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "database_error", "unable to reorder downloads")
		return
	}
	defer tx.Rollback()
	for rank, id := range in.IDs {
		if j := s.jobs[id]; j != nil && j.AccountID == account {
			j.Priority = len(in.IDs) - rank
			_, err = tx.ExecContext(r.Context(), `UPDATE downloads SET priority=? WHERE id=? AND account_id=?`, j.Priority, id, account)
			if err != nil {
				apiError(w, 500, "database_error", "unable to reorder downloads")
				return
			}
		}
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "database_error", "unable to reorder downloads")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
