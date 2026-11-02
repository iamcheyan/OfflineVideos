package media

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrPathOutsideLibrary reports a download row whose stored path escapes the
// download root. Callers treat it as a client-visible rejection, not a storage
// failure: the row is wrong, the disk is fine.
var ErrPathOutsideLibrary = errors.New("media path outside library")

func (s *Service) library(w http.ResponseWriter, r *http.Request) {
	// An empty account_id means "every source". The offline library page shows
	// one merged list, so it does not need to know which services exist before
	// it can ask for anything.
	accountParam := r.URL.Query().Get("account_id")
	account := accountParam
	if account == "" {
		resolved, err := resolveAccount(r, s.db)
		if err != nil {
			apiError(w, 500, "account_unavailable", "active account unavailable")
			return
		}
		account = resolved
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filter := r.URL.Query().Get("filter")
	sortBy := r.URL.Query().Get("sort")
	order := "d.created_at DESC"
	if sortBy == "title" {
		order = "COALESCE(v.title,d.video_id) COLLATE NOCASE ASC"
	} else if sortBy == "duration" {
		order = "COALESCE(v.duration,0) DESC"
	}
	base := `SELECT COALESCE(v.id,d.video_id),d.account_id,COALESCE(v.channel_id,''),COALESCE(v.title,d.video_id),COALESCE(v.url,d.url),COALESCE(v.thumbnail,''),COALESCE(v.duration,0),COALESCE(v.published_at,''),COALESCE(v.description,''),d.mode,d.quality,d.output_path,COALESCE(c.title,''),COALESCE((SELECT a.output_path FROM downloads a WHERE a.account_id=d.account_id AND a.video_id=d.video_id AND a.mode='audio' AND a.state='completed' AND a.output_path<>'' ORDER BY a.created_at DESC,a.id DESC LIMIT 1),''),COALESCE(m.since,'') FROM downloads d LEFT JOIN videos v ON v.id=d.video_id AND v.account_id=d.account_id LEFT JOIN channels c ON c.id=v.channel_id AND c.account_id=v.account_id LEFT JOIN remote_missing m ON m.video_id=d.video_id AND m.account_id=d.account_id WHERE d.state='completed' AND d.output_path<>'' AND d.mode='video'`
	args := []any{}
	if accountParam != "" {
		base += ` AND d.account_id=?`
		args = append(args, account)
	}
	if q != "" {
		base += ` AND (COALESCE(v.title,d.video_id) LIKE ? OR COALESCE(v.description,'') LIKE ?)`
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if filter != "" {
		base += ` AND d.mode=?`
		args = append(args, filter)
	}
	rows, err := s.db.QueryContext(r.Context(), base+" ORDER BY "+order, args...)
	if err != nil {
		apiError(w, 500, "database_error", "unable to read offline library")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, aid, cid, title, url, thumb, pub, desc, mode, quality, path, uploader, audioPath, missingSince string
		var duration int
		if err := rows.Scan(&id, &aid, &cid, &title, &url, &thumb, &duration, &pub, &desc, &mode, &quality, &path, &uploader, &audioPath, &missingSince); err != nil {
			apiError(w, 500, "database_error", "unable to read offline library")
			return
		}
		// remote_missing_since is only set when the source's list no longer
		// contains the video, which is how a watched item is recognised. The
		// app labels it and offers a delete; the file is never removed here.
		out = append(out, map[string]any{"id": id, "account_id": aid, "channel_id": cid, "title": title, "url": url, "thumbnail": thumb, "duration": duration, "published_at": pub, "description": desc, "uploader": uploader, "mode": mode, "quality": quality, "path": path, "audio_path": audioPath, "remote_missing_since": missingSince})
	}
	if err := rows.Err(); err != nil {
		apiError(w, 500, "database_error", "unable to read offline library")
		return
	}
	writeJSON(w, 200, out)
}

// DeleteDownload removes all local audio/video files and job rows for one video.
// Path containment is enforced because stored paths come from the database.
func (s *Service) DeleteDownload(ctx context.Context, account, videoID string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT output_path FROM downloads WHERE account_id=? AND video_id=? AND state='completed' AND output_path<>''`, account, videoID)
	if err != nil {
		return false, err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return false, err
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	root, err := filepath.Abs(s.cfg.DownloadDir)
	if err != nil {
		return false, err
	}
	removed := false
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil || !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return removed, fmt.Errorf("%w: %s", ErrPathOutsideLibrary, path)
		}
		if err = os.Remove(abs); err != nil {
			if !os.IsNotExist(err) {
				return removed, err
			}
		} else {
			removed = true
		}
	}
	// Drop every row for this video, not just the completed one. A leftover
	// failed job would otherwise make the app treat the video as "already
	// tried" and never queue it again, which breaks both the retry button and
	// the delete-then-resync workflow.
	if _, err = s.db.ExecContext(ctx, `DELETE FROM downloads WHERE account_id=? AND video_id=?`, account, videoID); err != nil {
		return removed, err
	}
	return removed, nil
}

func (s *Service) deleteLibraryVideo(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	id := r.PathValue("video_id")
	removed, err := s.DeleteDownload(r.Context(), account, id)
	if errors.Is(err, ErrPathOutsideLibrary) {
		apiError(w, 400, "invalid_media_path", "media path outside library")
		return
	}
	if err != nil {
		apiError(w, 500, "storage_error", "unable to remove media")
		return
	}
	// The row cleanup succeeded even when no file was on disk (a failed or
	// cancelled job leaves rows behind), and the client's job is to end up with
	// the video unblocked, so this is not a 404.
	_ = removed
	w.WriteHeader(http.StatusNoContent)
}
func (s *Service) mediaStream(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "video"
	}
	if mode != "video" && mode != "audio" {
		apiError(w, 400, "invalid_mode", "unsupported media mode")
		return
	}
	var path string
	err = s.db.QueryRowContext(r.Context(), `SELECT output_path FROM downloads WHERE account_id=? AND video_id=? AND mode=? AND state='completed' AND output_path<>'' ORDER BY created_at DESC LIMIT 1`, account, r.PathValue("video_id"), mode).Scan(&path)
	if err != nil {
		apiError(w, 404, "media_not_found", "offline media not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		apiError(w, 404, "media_not_found", "offline media file missing")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		apiError(w, 500, "storage_error", "unable to read media")
		return
	}
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

func (s *Service) markLibraryWatched(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, 500, "account_unavailable", "active account unavailable")
		return
	}
	id := r.PathValue("video_id")
	if id == "" {
		apiError(w, 400, "invalid_video_id", "video id is required")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO remote_missing(account_id, video_id, since) VALUES(?,?,?)
		 ON CONFLICT(account_id, video_id) DO NOTHING`,
		account, id, now)
	if err != nil {
		apiError(w, 500, "database_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"video_id":   id,
		"account_id": account,
		"watched":    true,
	})
}
