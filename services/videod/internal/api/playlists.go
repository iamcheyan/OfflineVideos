package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type playlistItem struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	VideoCount  int    `json:"video_count"`
	Kind        string `json:"kind"`
}

// playlistCollection serves list/create for user and synced playlists.
func (h *Handler) playlistCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.playlists(w, r)
	case http.MethodPost:
		var in struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil || strings.TrimSpace(in.Title) == "" {
			fail(w, 400, "title is required")
			return
		}
		account := h.account(r)
		if account == "" {
			fail(w, 409, "no active account")
			return
		}
		id := "user-" + randomID()
		if _, err := h.DB.ExecContext(r.Context(), `INSERT INTO playlists(id,account_id,title,description) VALUES(?,?,?,?)`, id, account, strings.TrimSpace(in.Title), in.Description); err != nil {
			fail(w, 500, "database error")
			return
		}
		jsonOut(w, playlistItem{ID: id, AccountID: account, Title: strings.TrimSpace(in.Title), Description: in.Description, Kind: "user"})
	default:
		fail(w, 405, "method not allowed")
	}
}

// playlistDetail serves one playlist with its ordered videos, and supports
// adding/removing videos from user playlists.
func (h *Handler) playlistDetail(w http.ResponseWriter, r *http.Request) {
	account, id := h.account(r), r.PathValue("id")
	if account == "" {
		fail(w, 409, "no active account")
		return
	}
	var kind string
	if err := h.DB.QueryRowContext(r.Context(), `SELECT CASE WHEN id LIKE 'user-%' THEN 'user' ELSE 'synced' END FROM playlists WHERE id=? AND account_id=?`, id, account).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "playlist not found")
			return
		}
		fail(w, 500, "database error")
		return
	}
	switch r.Method {
	case http.MethodGet:
		var p playlistItem
		p.ID, p.AccountID, p.Kind = id, account, kind
		if err := h.DB.QueryRowContext(r.Context(), `SELECT title,description FROM playlists WHERE id=? AND account_id=?`, id, account).Scan(&p.Title, &p.Description); err != nil {
			fail(w, 500, "database error")
			return
		}
		rows, err := h.DB.QueryContext(r.Context(), `SELECT `+videoCols+` FROM playlist_videos pv JOIN videos v ON v.id=pv.video_id AND v.account_id=? WHERE pv.playlist_id=? ORDER BY pv.position, v.id`, account, id)
		if err != nil {
			fail(w, 500, "database error")
			return
		}
		defer rows.Close()
		videos, err := scanVideos(rows)
		if err != nil {
			fail(w, 500, "database error")
			return
		}
		p.VideoCount = len(videos)
		jsonOut(w, map[string]any{"id": p.ID, "account_id": p.AccountID, "title": p.Title, "description": p.Description, "video_count": p.VideoCount, "kind": p.Kind, "videos": videos})
	case http.MethodPost:
		if kind != "user" {
			fail(w, 409, "synced playlists are read-only")
			return
		}
		var in struct {
			VideoID string `json:"video_id"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil || in.VideoID == "" {
			fail(w, 400, "video_id is required")
			return
		}
		res, err := h.DB.ExecContext(r.Context(), `INSERT OR IGNORE INTO playlist_videos(playlist_id,video_id,position)
			SELECT ?,id,COALESCE((SELECT MAX(position)+1 FROM playlist_videos WHERE playlist_id=?),0) FROM videos WHERE id=? AND account_id=?`, id, id, in.VideoID, account)
		if err != nil {
			fail(w, 400, "video not found")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(w, 409, "video already in playlist or not found")
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonOut(w, map[string]bool{"ok": true})
	case http.MethodDelete:
		if kind != "user" {
			fail(w, 409, "synced playlists are read-only")
			return
		}
		if _, err := h.DB.ExecContext(r.Context(), `DELETE FROM playlists WHERE id=? AND account_id=?`, id, account); err != nil {
			fail(w, 500, "database error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, 405, "method not allowed")
	}
}

// playlistVideoDelete removes a single video from a user playlist.
func (h *Handler) playlistVideoDelete(w http.ResponseWriter, r *http.Request) {
	account, id, videoID := h.account(r), r.PathValue("id"), r.PathValue("video_id")
	var kind string
	if err := h.DB.QueryRowContext(r.Context(), `SELECT CASE WHEN id LIKE 'user-%' THEN 'user' ELSE 'synced' END FROM playlists WHERE id=? AND account_id=?`, id, account).Scan(&kind); err != nil {
		fail(w, 404, "playlist not found")
		return
	}
	if kind != "user" {
		fail(w, 409, "synced playlists are read-only")
		return
	}
	res, err := h.DB.ExecContext(r.Context(), `DELETE FROM playlist_videos WHERE playlist_id=? AND video_id=?`, id, videoID)
	if err != nil {
		fail(w, 500, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, 404, "video not in playlist")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
