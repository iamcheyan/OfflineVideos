package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"kanade/videod/internal/store"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Handler struct {
	DB          *sql.DB
	Accounts    store.AccountStore
	HealthProbe func(context.Context) (map[string]any, error)
	Sync        SyncProvider
	// SyncSources maps a source name ("youtube", "bilibili") to its provider.
	// POST /api/sync selects one with {"source": "..."}.
	SyncSources map[string]SyncProvider
	// DataDir is where provider cookie jars live. Set by main so the cookie
	// import endpoint writes the same file the provider reads.
	DataDir string

	// AutoDownload enables queuing a download for every newly synced video.
	AutoDownload bool
	// EnqueueDownload queues one download and returns its job id. Wired by main
	// so the HTTP layer does not depend on the media package.
	EnqueueDownload func(account, videoID, url, mode, quality, bitrate string, priority int) (int64, error)
	// DeleteDownload removes one completed download (file plus row) and reports
	// whether a file was actually removed. Wired by main so the HTTP layer does
	// not depend on the media package.
	DeleteDownload func(account, videoID string) (removed bool, err error)
}

func New(db *sql.DB) *Handler { return &Handler{DB: db, Accounts: store.AccountStore{DB: db}} }
func (h *Handler) SetHealth(probe func(context.Context) (map[string]any, error)) {
	h.HealthProbe = probe
}
func (h *Handler) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/health", h.health)
	m.HandleFunc("GET /api/accounts", h.accounts)
	m.HandleFunc("GET /api/channels", h.channels)
	m.HandleFunc("GET /api/channels/{id}", h.channel)
	m.HandleFunc("GET /api/videos", h.videos)
	m.HandleFunc("GET /api/videos/{id}", h.video)
	m.HandleFunc("GET /api/playlists", h.playlistCollection)
	m.HandleFunc("POST /api/playlists", h.playlistCollection)
	m.HandleFunc("GET /api/playlists/{id}", h.playlistDetail)
	m.HandleFunc("POST /api/playlists/{id}", h.playlistDetail)
	m.HandleFunc("DELETE /api/playlists/{id}", h.playlistDetail)
	m.HandleFunc("DELETE /api/playlists/{id}/videos/{video_id}", h.playlistVideoDelete)
	m.HandleFunc("GET /api/home", h.home)
	m.HandleFunc("POST /api/sync", h.sync)
	m.HandleFunc("GET /api/subscriptions", h.channels)
	m.HandleFunc("GET /api/watch_later", h.listVideos("watch_later"))
	m.HandleFunc("POST /api/watch_later/{id}", h.watchLaterAdd)
	m.HandleFunc("DELETE /api/watch_later/{id}", h.watchLaterDelete)
	m.HandleFunc("GET /api/favorites", h.listVideos("favorites"))
	m.HandleFunc("POST /api/favorites/{id}", h.favoriteAdd)
	m.HandleFunc("DELETE /api/favorites/{id}", h.favoriteDelete)
	// POST /api/watch_later/{id}/watched removes the item locally and, when the
	// source provider can write back, from the remote list too.
	m.HandleFunc("POST /api/watch_later/{id}/watched", h.markWatched)
	// POST /api/auth/cookies writes a provider cookie jar from a Cookie header
	// value copied out of a browser.
	m.HandleFunc("POST /api/auth/cookies", h.importCookies)
	m.HandleFunc("GET /api/search", h.search)
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	slug := map[int]string{400: "bad_request", 404: "not_found", 405: "method_not_allowed", 409: "conflict", 500: "internal_error", 503: "unavailable"}[code]
	if slug == "" {
		slug = "request_failed"
	}
	w.WriteHeader(code)
	jsonOut(w, map[string]any{"error": map[string]string{"code": slug, "message": msg}})
}
func (h *Handler) account(r *http.Request) string {
	if id := r.URL.Query().Get("account_id"); id != "" {
		return id
	}
	id, _ := store.ActiveAccount(r.Context(), h.DB)
	return id
}
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if e := h.DB.PingContext(r.Context()); e != nil {
		fail(w, 503, "database unavailable")
		return
	}
	var active int
	_ = h.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM accounts WHERE active=1 AND auth_type NOT IN ('demo','')`).Scan(&active)
	payload := map[string]any{"status": "ok", "version": "0.1.0", "tools": map[string]any{}, "mock": true, "database": "sqlite"}
	var schemaVersion int
	_ = h.DB.QueryRowContext(r.Context(), `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&schemaVersion)
	payload["schema_version"] = schemaVersion
	if h.HealthProbe != nil {
		if values, err := h.HealthProbe(r.Context()); err == nil {
			for k, v := range values {
				payload[k] = v
			}
		}
	}
	payload["auth"] = map[string]any{"configured": active > 0, "status": map[bool]string{true: "configured", false: "not_configured"}[active > 0]}
	jsonOut(w, payload)
}
func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	v, e := h.Accounts.List(r.Context())
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, v)
}
func (h *Handler) activate(w http.ResponseWriter, r *http.Request) {
	e := h.Accounts.Activate(r.Context(), r.PathValue("id"))
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "account not found")
		return
	}
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}
func paging(r *http.Request) (int, int) {
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

type videoRow struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	ChannelID   string `json:"channel_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Thumbnail   string `json:"thumbnail"`
	Duration    int    `json:"duration"`
	PublishedAt string `json:"published_at"`
	Description string `json:"description"`
}

const videoCols = `v.id,v.account_id,v.channel_id,v.title,v.url,v.thumbnail,v.duration,v.published_at,v.description`

func scanVideos(rows *sql.Rows) ([]videoRow, error) {
	out := []videoRow{}
	for rows.Next() {
		var v videoRow
		if e := rows.Scan(&v.ID, &v.AccountID, &v.ChannelID, &v.Title, &v.URL, &v.Thumbnail, &v.Duration, &v.PublishedAt, &v.Description); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (h *Handler) videos(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	rows, e := h.DB.QueryContext(r.Context(), `SELECT `+videoCols+` FROM videos v WHERE v.account_id=? ORDER BY v.published_at DESC,v.id LIMIT ? OFFSET ?`, h.account(r), l, o)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer rows.Close()
	v, e := scanVideos(rows)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, v)
}
func (h *Handler) video(w http.ResponseWriter, r *http.Request) {
	var v videoRow
	e := h.DB.QueryRowContext(r.Context(), `SELECT `+videoCols+` FROM videos v WHERE v.account_id=? AND v.id=?`, h.account(r), r.PathValue("id")).Scan(&v.ID, &v.AccountID, &v.ChannelID, &v.Title, &v.URL, &v.Thumbnail, &v.Duration, &v.PublishedAt, &v.Description)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "video not found")
		return
	}
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, v)
}
func (h *Handler) channels(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	if page, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && page > 1 {
		o = (page - 1) * l
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	query := `SELECT id,account_id,title,url,thumbnail,description FROM channels WHERE account_id=?`
	args := []any{h.account(r)}
	if q != "" {
		query += ` AND (title LIKE ? OR description LIKE ?)`
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	rows, e := h.DB.QueryContext(r.Context(), query+` ORDER BY title,id LIMIT ? OFFSET ?`, append(args, l, o)...)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, a, t, u, th, d string
		if e = rows.Scan(&id, &a, &t, &u, &th, &d); e != nil {
			fail(w, 500, "database error")
			return
		}
		out = append(out, map[string]any{"id": id, "account_id": a, "title": t, "url": u, "thumbnail": th, "description": d})
	}
	if rows.Err() != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, out)
}
func (h *Handler) channel(w http.ResponseWriter, r *http.Request) {
	var id, a, t, u, th, d string
	e := h.DB.QueryRowContext(r.Context(), `SELECT id,account_id,title,url,thumbnail,description FROM channels WHERE account_id=? AND id=?`, h.account(r), r.PathValue("id")).Scan(&id, &a, &t, &u, &th, &d)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "channel not found")
		return
	}
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, map[string]any{"id": id, "account_id": a, "title": t, "url": u, "thumbnail": th, "description": d})
}
func (h *Handler) playlists(w http.ResponseWriter, r *http.Request) {
	l, o := paging(r)
	rows, e := h.DB.QueryContext(r.Context(), `SELECT p.id,p.account_id,p.title,p.description,COUNT(pv.video_id) FROM playlists p LEFT JOIN playlist_videos pv ON pv.playlist_id=p.id WHERE p.account_id=? GROUP BY p.id ORDER BY p.title LIMIT ? OFFSET ?`, h.account(r), l, o)
	if e != nil {
		fail(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, a, t, d string
		var n int
		if e = rows.Scan(&id, &a, &t, &d, &n); e != nil {
			fail(w, 500, "database error")
			return
		}
		out = append(out, map[string]any{"id": id, "account_id": a, "title": t, "description": d, "video_count": n})
	}
	if rows.Err() != nil {
		fail(w, 500, "database error")
		return
	}
	jsonOut(w, out)
}
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	a := h.account(r)
	load := func(query string, args ...any) []videoRow {
		rows, err := h.DB.QueryContext(r.Context(), query, args...)
		if err != nil {
			return []videoRow{}
		}
		defer rows.Close()
		items, err := scanVideos(rows)
		if err != nil {
			return []videoRow{}
		}
		return items
	}
	jsonOut(w, map[string]any{
		"continue_watching": load(`SELECT `+videoCols+` FROM history x JOIN videos v ON v.id=x.video_id AND v.account_id=x.account_id WHERE x.account_id=? AND x.position>0 ORDER BY x.watched_at DESC LIMIT 8`, a),
		"watch_later":       load(`SELECT `+videoCols+` FROM watch_later x JOIN videos v ON v.id=x.video_id AND v.account_id=x.account_id WHERE x.account_id=? ORDER BY x.added_at DESC LIMIT 8`, a),
		"subscriptions":     load(`SELECT `+videoCols+` FROM videos v WHERE v.account_id=? ORDER BY v.published_at DESC LIMIT 8`, a),
		"recent_downloads":  load(`SELECT `+videoCols+` FROM downloads d JOIN videos v ON v.id=d.video_id AND v.account_id=d.account_id WHERE d.account_id=? ORDER BY d.created_at DESC LIMIT 8`, a),
		"recently_added":    load(`SELECT `+videoCols+` FROM videos v WHERE v.account_id=? ORDER BY v.published_at DESC LIMIT 8`, a),
		"recommended":       load(`SELECT `+videoCols+` FROM videos v WHERE v.account_id=? ORDER BY v.id LIMIT 8`, a),
	})
}
func (h *Handler) listVideos(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l, o := paging(r)
		sortCol := "x.added_at"
		if table == "history" {
			sortCol = "x.watched_at"
		}
		rows, e := h.DB.QueryContext(r.Context(), `SELECT `+videoCols+` FROM `+table+` x JOIN videos v ON v.id=x.video_id AND v.account_id=x.account_id WHERE x.account_id=? ORDER BY `+sortCol+` DESC LIMIT ? OFFSET ?`, h.account(r), l, o)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		defer rows.Close()
		v, e := scanVideos(rows)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		jsonOut(w, v)
	}
}
func (h *Handler) mutate(table string, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, id := h.account(r), r.PathValue("id")
		var e error
		if add {
			var n int
			e = h.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM videos WHERE account_id=? AND id=?`, a, id).Scan(&n)
			if e != nil || n == 0 {
				fail(w, 404, "video not found")
				return
			}
			_, e = h.DB.ExecContext(r.Context(), `INSERT OR IGNORE INTO `+table+`(account_id,video_id,added_at) VALUES(?,?,datetime('now'))`, a, id)
		} else {
			_, e = h.DB.ExecContext(r.Context(), `DELETE FROM `+table+` WHERE account_id=? AND video_id=?`, a, id)
		}
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		if add {
			w.WriteHeader(201)
			jsonOut(w, map[string]bool{"ok": true})
		} else {
			w.WriteHeader(204)
		}
	}
}
func (h *Handler) watchLaterAdd(w http.ResponseWriter, r *http.Request) {
	h.mutate("watch_later", true)(w, r)
}
func (h *Handler) watchLaterDelete(w http.ResponseWriter, r *http.Request) {
	h.mutate("watch_later", false)(w, r)
}
func (h *Handler) favoriteAdd(w http.ResponseWriter, r *http.Request) {
	h.mutate("favorites", true)(w, r)
}
func (h *Handler) favoriteDelete(w http.ResponseWriter, r *http.Request) {
	h.mutate("favorites", false)(w, r)
}

// RemoteWatchLaterRemover is implemented by providers that can delete an item
// from the account's remote watch-later list. Bilibili supports this with
// cookies alone; YouTube needs OAuth and is not wired yet.
type RemoteWatchLaterRemover interface {
	RemoveFromWatchLater(ctx context.Context, videoID string) error
}

// markWatched records that the user finished a video and removes it from the
// list so the next sync does not bring it back.
//
// The local rows are always deleted first: the user's intent is "this is
// done", and a remote failure must not leave the item showing up again. A
// remote error is reported in the response under remote_error, not as an
// HTTP failure.
func (h *Handler) markWatched(w http.ResponseWriter, r *http.Request) {
	account := h.account(r)
	id := r.PathValue("id")
	if id == "" {
		fail(w, 400, "video id is required")
		return
	}
	var duration int
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT COALESCE(duration,0) FROM videos WHERE account_id=? AND id=?`, account, id).Scan(&duration)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "video not found")
		return
	}
	if err != nil {
		fail(w, 500, "database error")
		return
	}

	if _, err := h.DB.ExecContext(r.Context(),
		`INSERT INTO history(account_id,video_id,watched_at,position,duration) VALUES(?,?,?,?,?)
		 ON CONFLICT(account_id,video_id) DO UPDATE SET watched_at=excluded.watched_at,
		   position=excluded.position, duration=excluded.duration`,
		account, id, time.Now().UTC().Format(time.RFC3339), float64(duration), float64(duration)); err != nil {
		fail(w, 500, "database error")
		return
	}
	if _, err := h.DB.ExecContext(r.Context(),
		`DELETE FROM watch_later WHERE account_id=? AND video_id=?`, account, id); err != nil {
		fail(w, 500, "database error")
		return
	}
	if _, err := h.DB.ExecContext(r.Context(),
		`DELETE FROM playlist_videos WHERE playlist_id=? AND video_id=?`, watchLaterPlaylistID(account), id); err != nil {
		fail(w, 500, "database error")
		return
	}
	// "Watched" means the video should stop occupying space and stop showing in
	// the library, so the downloaded file goes too. A failure here is reported
	// in the response but does not undo the list removal above: the user said
	// this is done, and re-adding the row would be worse.
	fileRemoved := false
	fileError := ""
	if h.DeleteDownload != nil {
		removed, err := h.DeleteDownload(account, id)
		if err != nil {
			fileError = err.Error()
		} else {
			fileRemoved = removed
		}
	}

	result := map[string]any{
		"status": "ok", "video_id": id, "account_id": account,
		"remote": "skipped", "file_removed": fileRemoved,
	}
	if fileError != "" {
		result["file_error"] = fileError
	}
	provider, _ := h.syncSource(account)
	if remover, ok := provider.(RemoteWatchLaterRemover); ok {
		if err := remover.RemoveFromWatchLater(r.Context(), id); err != nil {
			result["remote"] = "failed"
			result["remote_error"] = err.Error()
		} else {
			result["remote"] = "removed"
		}
	} else if provider != nil {
		result["remote"] = "unsupported"
	}
	jsonOut(w, result)
}
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("type")
	if kind == "" {
		kind = "video"
	}
	if q == "" {
		jsonOut(w, []map[string]any{})
		return
	}
	if kind != "video" && kind != "channel" && kind != "playlist" && kind != "library" {
		fail(w, 400, "invalid_search_type")
		return
	}
	account := h.account(r)
	l, o := paging(r)
	out := []map[string]any{}
	switch kind {
	case "video":
		rows, e := h.DB.QueryContext(r.Context(), `SELECT `+videoCols+` FROM video_search s JOIN videos v ON v.id=s.video_id AND v.account_id=s.account_id WHERE s.account_id=? AND video_search MATCH ? ORDER BY rank LIMIT ? OFFSET ?`, account, q, l, o)
		if e != nil {
			fail(w, 400, "invalid search query")
			return
		}
		defer rows.Close()
		v, e := scanVideos(rows)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		for _, x := range v {
			out = append(out, map[string]any{"type": "video", "id": x.ID, "title": x.Title, "thumbnail": x.Thumbnail, "duration": x.Duration, "channel_id": x.ChannelID})
		}
	case "channel":
		rows, e := h.DB.QueryContext(r.Context(), `SELECT id,title,thumbnail FROM channels WHERE account_id=? AND title LIKE ? ORDER BY title LIMIT ? OFFSET ?`, account, "%"+q+"%", l, o)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, thumb string
			if e = rows.Scan(&id, &title, &thumb); e != nil {
				fail(w, 500, "database error")
				return
			}
			out = append(out, map[string]any{"type": "channel", "id": id, "title": title, "thumbnail": thumb})
		}
	case "playlist":
		rows, e := h.DB.QueryContext(r.Context(), `SELECT id,title,description FROM playlists WHERE account_id=? AND title LIKE ? ORDER BY title LIMIT ? OFFSET ?`, account, "%"+q+"%", l, o)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, desc string
			if e = rows.Scan(&id, &title, &desc); e != nil {
				fail(w, 500, "database error")
				return
			}
			out = append(out, map[string]any{"type": "playlist", "id": id, "title": title, "description": desc})
		}
	case "library":
		rows, e := h.DB.QueryContext(r.Context(), `SELECT v.id,v.title,v.thumbnail,v.duration FROM downloads d JOIN videos v ON v.id=d.video_id AND v.account_id=d.account_id WHERE d.account_id=? AND d.state='completed' AND d.output_path<>'' AND v.title LIKE ? ORDER BY v.title LIMIT ? OFFSET ?`, account, "%"+q+"%", l, o)
		if e != nil {
			fail(w, 500, "database error")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, thumb string
			var duration int
			if e = rows.Scan(&id, &title, &thumb, &duration); e != nil {
				fail(w, 500, "database error")
				return
			}
			out = append(out, map[string]any{"type": "library", "id": id, "title": title, "thumbnail": thumb, "duration": duration})
		}
	}
	jsonOut(w, out)
}

// sync pulls provider-backed lists into the local database, then reports counts.
//
// Scope values mirror the pre-existing contract ("all", "subscriptions",
// "playlists", "watch_later") so existing clients keep working. When no provider
// is configured the endpoint still refreshes synced_at timestamps and reports
// provider_sync=not_configured instead of implying a remote fetch happened.
func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	var in struct {
		What         string `json:"what"`
		Source       string `json:"source"`
		AutoDownload *bool  `json:"auto_download"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in)
	if in.What == "" {
		in.What = "all"
	}
	if in.What != "all" && in.What != "subscriptions" && in.What != "playlists" && in.What != "watch_later" {
		fail(w, 400, "unsupported sync scope")
		return
	}
	account := h.account(r)
	now := time.Now().UTC().Format(time.RFC3339)

	// The source selects both the remote provider and the local account whose
	// rows this sync owns. YouTube and Bilibili videos share one table, so
	// keeping separate accounts is what stops a Bilibili id from overwriting a
	// YouTube row with the same string.
	provider, effectiveSource := h.syncSource(in.Source)
	if effectiveSource != "" {
		account = effectiveSource
		if err := store.EnsureAccount(r.Context(), h.DB, account); err != nil {
			fail(w, 500, "account unavailable")
			return
		}
	}

	result := map[string]any{"status": "ok", "account_id": account, "what": in.What, "source": effectiveSource, "synced_at": now}
	var errs []string

	if provider == nil {
		if in.Source != "" {
			fail(w, http.StatusBadRequest, "no provider registered for source "+in.Source)
			return
		}
		result["provider_sync"] = "not_configured"
		result["note"] = "no provider is configured; only local timestamps were refreshed"
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		synced := 0

		if in.What == "all" || in.What == "subscriptions" {
			if channels, err := provider.SyncSubscriptions(ctx, account); err != nil {
				errs = append(errs, "subscriptions: "+err.Error())
			} else {
				if _, err := h.upsertChannels(ctx, account, channels); err != nil {
					errs = append(errs, "subscriptions store: "+err.Error())
				} else {
					synced += len(channels)
				}
			}
		}
		if in.What == "all" || in.What == "playlists" {
			if playlists, err := provider.SyncPlaylists(ctx, account); err != nil {
				errs = append(errs, "playlists: "+err.Error())
			} else {
				if _, err := h.upsertPlaylists(ctx, account, playlists); err != nil {
					errs = append(errs, "playlists store: "+err.Error())
				} else {
					synced += len(playlists)
				}
			}
		}
		if in.What == "all" || in.What == "watch_later" {
			if videos, err := provider.SyncWatchLater(ctx, account); err != nil {
				errs = append(errs, "watch_later: "+err.Error())
			} else {
				// A successful provider read is authoritative for list
				// membership. It runs before the rows are replaced, because
				// after the rewrite there is nothing left to compare against.
				// Files are not deleted here: see reconcileRemoteMissing.
				removed, err := h.changedWatchLater(ctx, account, videos)
				if err != nil {
					errs = append(errs, "watch_later diff: "+err.Error())
					removed = nil
				}
				marked, rerr := h.reconcileRemoteMissing(ctx, account, videos)
				if rerr != nil {
					errs = append(errs, "remote missing: "+rerr.Error())
					marked = nil
				}
				if _, err := h.upsertVideos(ctx, account, videos); err != nil {
					errs = append(errs, "watch_later store: "+err.Error())
				} else if _, err := h.upsertPlaylistVideos(ctx, account, watchLaterPlaylistID(account), watchLaterPlaylistTitle(account), videos); err != nil {
					errs = append(errs, "watch_later playlist: "+err.Error())
				} else {
					synced += len(videos)
					if len(removed) > 0 {
						result["watch_later_removed"] = removed
					}
					if len(marked) > 0 {
						result["now_missing_remotely"] = marked
					}
					// Precedence: explicit request field, then the stored
					// setting the user controls in the app, then the env
					// default. The stored setting is what makes the app's
					// toggle authoritative for the loopback scheduler.
					wantAuto := h.autoDownloadSetting(r, account, h.AutoDownload)
					if in.AutoDownload != nil {
						wantAuto = *in.AutoDownload
					}
					if wantAuto {
						queued, skipped, qerrs := h.autoEnqueue(ctx, account, videos)
						result["downloads_queued"] = queued
						result["downloads_skipped"] = skipped
						errs = append(errs, qerrs...)
					}
				}
			}
		}
		result["provider_sync"] = "done"
		result["synced_items"] = synced
	}

	if _, err := h.DB.ExecContext(r.Context(), `UPDATE channels SET synced_at=? WHERE account_id=?`, now, account); err != nil {
		log.Printf("sync: update channels synced_at for %s: %v", account, err)
	}
	if _, err := h.DB.ExecContext(r.Context(), `UPDATE playlists SET synced_at=? WHERE account_id=?`, now, account); err != nil {
		log.Printf("sync: update playlists synced_at for %s: %v", account, err)
	}

	var channels, playlists, videos, downloads int
	_ = h.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM channels WHERE account_id=?`, account).Scan(&channels)
	_ = h.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM playlists WHERE account_id=?`, account).Scan(&playlists)
	_ = h.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM videos WHERE account_id=?`, account).Scan(&videos)
	_ = h.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM downloads WHERE account_id=?`, account).Scan(&downloads)
	result["counts"] = map[string]int{"channels": channels, "playlists": playlists, "videos": videos, "downloads": downloads}
	if len(errs) > 0 {
		result["errors"] = errs
	}
	jsonOut(w, result)
}

// randomID returns an opaque URL-safe identifier for user-created records.
func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
