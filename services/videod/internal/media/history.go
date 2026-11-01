package media

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type historyEntry struct {
	VideoID           string  `json:"id"`
	VideoIDAlias      string  `json:"video_id"`
	Title             string  `json:"title"`
	ChannelID         string  `json:"channel_id"`
	Thumbnail         string  `json:"thumbnail"`
	DurationSec       float64 `json:"duration_sec"`
	Duration          int     `json:"duration"`
	PositionSec       float64 `json:"position_sec"`
	WatchedSec        float64 `json:"watched_sec"`
	CompletionPercent float64 `json:"completion_percent"`
	LastWatchedAt     string  `json:"watched_at"`
}

// history serves watch history, progress updates and resume positions.
// GET /api/history, POST /api/history, POST /api/history/progress,
// DELETE /api/history (clear all), DELETE /api/history/{video_id}.
func (s *Service) history(w http.ResponseWriter, r *http.Request) {
	account, err := resolveAccount(r, s.db)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "account_unavailable", "active account unavailable")
		return
	}
	pathID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/history"), "/")
	switch r.Method {
	case http.MethodDelete:
		if pathID == "" || pathID == "progress" {
			if _, err = s.db.ExecContext(r.Context(), `DELETE FROM history WHERE account_id=?`, account); err != nil {
				apiError(w, http.StatusInternalServerError, "database_error", "unable to clear history")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		res, err := s.db.ExecContext(r.Context(), `DELETE FROM history WHERE account_id=? AND video_id=?`, account, pathID)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to remove history entry")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			apiError(w, http.StatusNotFound, "history_not_found", "history entry not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		var in struct {
			VideoID  string  `json:"video_id"`
			Position float64 `json:"position_sec"`
			Duration float64 `json:"duration_sec"`
		}
		if decodeJSON(r, &in) != nil || in.VideoID == "" {
			apiError(w, http.StatusBadRequest, "invalid_request", "video_id and progress are required")
			return
		}
		if in.Position < 0 {
			apiError(w, http.StatusBadRequest, "invalid_request", "position_sec must not be negative")
			return
		}
		res, err := s.db.ExecContext(r.Context(), `INSERT INTO history(account_id,video_id,watched_at,position,duration)
			SELECT ?,id,CURRENT_TIMESTAMP,?,MAX(?,duration) FROM videos WHERE id=? AND account_id=?
			ON CONFLICT(account_id,video_id) DO UPDATE SET
				watched_at=CURRENT_TIMESTAMP,
				position=excluded.position,
				duration=CASE WHEN excluded.duration>history.duration THEN excluded.duration ELSE history.duration END`,
			account, in.Position, int64(in.Duration), in.VideoID, account)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to save history")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			apiError(w, http.StatusNotFound, "video_not_found", "video not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		out, err := s.historyEntries(r, account)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "database_error", "unable to read history")
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Service) historyEntries(r *http.Request, account string) ([]historyEntry, error) {
	limit, offset := pageParams(r)
	rows, err := s.db.QueryContext(r.Context(), `SELECT h.video_id,h.watched_at,h.position,v.title,v.channel_id,v.thumbnail,
		CASE WHEN h.duration>0 THEN h.duration ELSE v.duration END
		FROM history h JOIN videos v ON v.id=h.video_id AND v.account_id=h.account_id
		WHERE h.account_id=? ORDER BY h.watched_at DESC, h.video_id LIMIT ? OFFSET ?`, account, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []historyEntry{}
	for rows.Next() {
		var x historyEntry
		var duration float64
		if err := rows.Scan(&x.VideoID, &x.LastWatchedAt, &x.PositionSec, &x.Title, &x.ChannelID, &x.Thumbnail, &duration); err != nil {
			return nil, err
		}
		x.VideoIDAlias = x.VideoID
		x.Duration = int(duration)
		x.DurationSec = duration
		x.WatchedSec = x.PositionSec
		if x.DurationSec > 0 {
			x.CompletionPercent = x.PositionSec / x.DurationSec * 100
			if x.CompletionPercent > 100 {
				x.CompletionPercent = 100
			}
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// pageParams mirrors the catalogue paging contract: default 20, max 100.
func pageParams(r *http.Request) (int, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	// Clamp instead of silently falling back to the default page size: a caller
	// asking for more than the cap still gets the largest page we serve, rather
	// than 20 rows it did not ask for.
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// resumePosition returns the stored position for a video, or zero.
func (s *Service) resumePosition(account, videoID string) float64 {
	if videoID == "" {
		return 0
	}
	var pos float64
	err := s.db.QueryRow(`SELECT position FROM history WHERE account_id=? AND video_id=?`, account, videoID).Scan(&pos)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return 0
	}
	if pos < 0 {
		return 0
	}
	return pos
}
