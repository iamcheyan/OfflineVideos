package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"kanade/videod/internal/domain"
	"kanade/videod/internal/provider"
)

// SyncProvider is the subset of a content provider the sync endpoint needs.
//
// It is an interface so the handler can be tested without a live yt-dlp and so
// the provider package does not leak into the HTTP layer's other handlers.
type SyncProvider interface {
	SyncSubscriptions(ctx context.Context, account string) ([]domain.Channel, error)
	SyncPlaylists(ctx context.Context, account string) ([]domain.Playlist, error)
	SyncWatchLater(ctx context.Context, account string) ([]domain.Video, error)
}

// SetSyncProvider attaches a provider used by POST /api/sync when the request
// names no source. It keeps the single-provider call site working.
func (h *Handler) SetSyncProvider(p SyncProvider) {
	h.Sync = p
	if p != nil {
		h.SyncSources = map[string]SyncProvider{"youtube": p}
	}
}

// SetSyncSources registers one provider per source name. The sync endpoint
// selects one with {"source":"bilibili"}; an empty source uses the provider
// registered by SetSyncProvider.
func (h *Handler) SetSyncSources(sources map[string]SyncProvider) {
	h.SyncSources = sources
	for name, p := range sources {
		if name == "youtube" {
			h.Sync = p
		}
	}
}

// syncSource returns the provider for name and the source actually used.
func (h *Handler) syncSource(name string) (SyncProvider, string) {
	if name != "" {
		if p, ok := h.SyncSources[name]; ok {
			return p, name
		}
		return nil, name
	}
	if p, ok := h.SyncSources["youtube"]; ok {
		return p, "youtube"
	}
	return h.Sync, ""
}

type syncOptions struct {
	Subscriptions bool `json:"subscriptions"`
	Playlists     bool `json:"playlists"`
	WatchLater    bool `json:"watch_later"`
}

type syncResult struct {
	Subscriptions int      `json:"subscriptions"`
	Playlists     int      `json:"playlists"`
	WatchLater    int      `json:"watch_later"`
	Videos        int      `json:"videos"`
	Errors        []string `json:"errors,omitempty"`
}

func (h *Handler) upsertChannels(ctx context.Context, account string, in []domain.Channel) (int, error) {
	n := 0
	for _, c := range in {
		if c.ID == "" {
			continue
		}
		_, err := h.DB.ExecContext(ctx,
			`INSERT INTO channels(id,account_id,title,url,thumbnail,description) VALUES(?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET title=excluded.title, url=excluded.url,
			   thumbnail=excluded.thumbnail, description=excluded.description`,
			c.ID, account, c.Title, c.URL, c.Thumbnail, c.Description)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (h *Handler) upsertPlaylists(ctx context.Context, account string, in []domain.Playlist) (int, error) {
	n := 0
	for _, p := range in {
		if p.ID == "" {
			continue
		}
		_, err := h.DB.ExecContext(ctx,
			`INSERT INTO playlists(id,account_id,title,description) VALUES(?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET title=excluded.title, description=excluded.description`,
			p.ID, account, p.Title, p.Description)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (h *Handler) upsertVideos(ctx context.Context, account string, in []domain.Video) (int, error) {
	n := 0
	for _, v := range in {
		if v.ID == "" {
			continue
		}
		// videos.channel_id has a foreign key onto channels(id); Watch Later rows
		// often carry a channel that was never synced, and Bilibili's list
		// endpoint often reports no uploader at all. Both cases need a channel
		// row, so an empty id gets a per-account placeholder rather than being
		// written as the empty string (which fails the constraint).
		// yt-dlp prints the literal string NA for a missing field, and that
		// value reaches here as a real channel id. Treating it as a channel
		// creates a foreign key pointing at a row that does not exist, so the
		// whole video insert fails and the item silently disappears from the
		// library while still sitting in watch_later.
		channelID := strings.TrimSpace(v.ChannelID)
		if channelID == "NA" {
			channelID = ""
		}
		if channelID == "" {
			channelID = "unknown-" + account
		}
		channelTitle := v.ChannelName
		if channelTitle == "" {
			channelTitle = channelID
		}
		// DO UPDATE on the title only: a later sync that knows the real
		// uploader name should replace the placeholder, but an existing name
		// must not be blanked by a row that carries none.
		if _, err := h.DB.ExecContext(ctx,
			`INSERT INTO channels(id,account_id,title,url) VALUES(?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET title=CASE
			   WHEN excluded.title<>'' AND excluded.title<>excluded.id
			   THEN excluded.title ELSE channels.title END`,
			channelID, account, channelTitle, channelURLFor(account, channelID)); err != nil {
			return n, err
		}
		published := v.PublishedAt
		if published.IsZero() {
			published = time.Now().UTC()
		}
		_, err := h.DB.ExecContext(ctx,
			`INSERT INTO videos(id,account_id,channel_id,title,url,duration,published_at,description,thumbnail)
			 VALUES(?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET account_id=excluded.account_id,
			   channel_id=excluded.channel_id, title=excluded.title, url=excluded.url,
			   duration=excluded.duration, description=excluded.description,
			   thumbnail=excluded.thumbnail`,
			v.ID, account, channelID, v.Title, v.URL, v.Duration,
			published.Format(time.RFC3339), v.Description, v.Thumbnail)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// upsertPlaylistVideos writes a synthetic playlist (e.g. WL) and its ordered rows.
func (h *Handler) upsertPlaylistVideos(ctx context.Context, account, playlistID, title string, videos []domain.Video) (int, error) {
	if _, err := h.DB.ExecContext(ctx,
		`INSERT INTO playlists(id,account_id,title,description) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET title=excluded.title`,
		playlistID, account, title, ""); err != nil {
		return 0, err
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_videos WHERE playlist_id=?`, playlistID); err != nil {
		return 0, err
	}
	n := 0
	// The watch-later playlist is also mirrored into the legacy watch_later
	// table: GET /api/watch_later reads that table, while playlist detail reads
	// playlist_videos. Writing only one of them makes the same list look empty
	// through the other endpoint.
	isWatchLater := playlistID == watchLaterPlaylistID(account)
	if isWatchLater {
		if _, err := tx.ExecContext(ctx, `DELETE FROM watch_later WHERE account_id=?`, account); err != nil {
			return 0, err
		}
	}
	for i, v := range videos {
		if v.ID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO playlist_videos(playlist_id,video_id,position) VALUES(?,?,?)`,
			playlistID, v.ID, i); err != nil {
			return n, err
		}
		if isWatchLater {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO watch_later(account_id,video_id,added_at) VALUES(?,?,?)`,
				account, v.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, tx.Commit()
}

// sourceMirrorChanges compares a successful upstream response to its prior snapshot.
// Before the first snapshot, only the local Watch Later list is treated as baseline.
func (h *Handler) sourceMirrorChanges(ctx context.Context, account string, current []domain.Video) ([]string, error) {
	rows, err := h.DB.QueryContext(ctx, `SELECT video_id FROM source_mirror WHERE account_id=?`, account)
	if err != nil {
		return nil, err
	}
	var previous []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		previous = append(previous, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(previous) == 0 {
		rows, err = h.DB.QueryContext(ctx, `SELECT video_id FROM watch_later WHERE account_id=?`, account)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		keep := map[string]bool{}
		for _, v := range current {
			keep[v.ID] = true
		}
		var removed []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			if !keep[id] {
				removed = append(removed, id)
			}
		}
		return removed, rows.Err()
	}
	keep := map[string]bool{}
	for _, v := range current {
		if v.ID != "" {
			keep[v.ID] = true
		}
	}
	var removed []string
	for _, id := range previous {
		if id != "" && !keep[id] {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

func (h *Handler) commitSourceMirror(ctx context.Context, account string, current []domain.Video) error {
	if len(current) == 0 {
		return nil
	}
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM source_mirror WHERE account_id=?`, account); err != nil {
		return err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, v := range current {
		if v.ID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_mirror(account_id,video_id,seen_at) VALUES(?,?,?)`, account, v.ID, stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// changedWatchLater reports which videos were in the local watch-later list but
// are absent from the list the provider just returned.
//
// Like reconcileRemoteMissing it has to run before upsertPlaylistVideos, which
// replaces every watch_later row with the incoming set.
func (h *Handler) changedWatchLater(ctx context.Context, account string, current []domain.Video) ([]string, error) {
	keepIDs := make(map[string]bool, len(current))
	for _, v := range current {
		if v.ID != "" {
			keepIDs[v.ID] = true
		}
	}
	rows, err := h.DB.QueryContext(ctx,
		`SELECT video_id FROM watch_later WHERE account_id=?`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !keepIDs[id] {
			stale = append(stale, id)
		}
	}
	return stale, rows.Err()
}

// reconcileRemoteMissing records which downloaded videos the source's
// watch-later list no longer contains, and clears the mark for videos that came
// back.
//
// Both services remove an entry once the video has been watched, so absence is
// the only signal available for "the user finished this upstream". The mark is
// metadata only: nothing is deleted here. Files are removed when the user asks,
// which is the whole point of keeping the flag instead of acting on it.
//
// A video is only considered if the user's own list used to hold it, so a file
// downloaded from a search or a playlist is never labelled as watched. It also
// needs a completed download, since the flag exists to describe files still
// occupying space.
func (h *Handler) reconcileRemoteMissing(ctx context.Context, account string, current []domain.Video) ([]string, error) {
	keepIDs := make(map[string]bool, len(current))
	for _, v := range current {
		if v.ID != "" {
			keepIDs[v.ID] = true
		}
	}

	// Candidates: what the list held before this sync, plus anything already
	// marked, so a mark survives repeated syncs, plus any completed downloads
	// for this account (so if a video finished downloading and is no longer
	// in watch_later upstream, it gets marked as remote_missing/watched).
	rows, err := h.DB.QueryContext(ctx, `
		SELECT video_id FROM watch_later WHERE account_id=?
		UNION
		SELECT video_id FROM remote_missing WHERE account_id=?
		UNION
		SELECT video_id FROM downloads WHERE account_id=? AND state='completed'`,
		account, account, account)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var marked []string
	since := time.Now().UTC().Format(time.RFC3339)
	for _, id := range candidates {
		if keepIDs[id] {
			// Present upstream again: the user re-added it, or the earlier
			// absence was a transient provider hiccup. Drop the mark.
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM remote_missing WHERE account_id=? AND video_id=?`,
				account, id); err != nil {
				return nil, err
			}
			continue
		}
		var downloaded int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM downloads WHERE account_id=? AND video_id=? AND state='completed'`,
			account, id).Scan(&downloaded); err != nil {
			return nil, err
		}
		if downloaded == 0 {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM remote_missing WHERE account_id=? AND video_id=?`,
			account, id).Scan(&exists); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO remote_missing(account_id,video_id,since) VALUES(?,?,?)
			 ON CONFLICT(account_id,video_id) DO NOTHING`,
			account, id, since); err != nil {
			return nil, err
		}
		if exists == 0 {
			marked = append(marked, id)
		}
	}
	return marked, tx.Commit()
}

// compile-time guard: YouTubeProvider must satisfy SyncProvider.
var _ SyncProvider = (*provider.YouTubeProvider)(nil)

// unusedSQL keeps the database/sql import meaningful if helpers change.
var _ = sql.ErrNoRows

// autoEnqueue queues a download for each video that has no existing completed or
// in-flight download. Duplicates are skipped rather than re-queued so repeated
// syncs stay idempotent.
func (h *Handler) autoEnqueue(ctx context.Context, account string, videos []domain.Video) (queued, skipped int, errs []string) {
	if h.EnqueueDownload == nil {
		return 0, len(videos), []string{"auto download: enqueue not configured"}
	}
	for _, v := range videos {
		if v.ID == "" {
			skipped++
			continue
		}
		var state string
		err := h.DB.QueryRowContext(ctx,
			`SELECT state FROM downloads WHERE account_id=? AND video_id=?
			 ORDER BY id DESC LIMIT 1`, account, v.ID).Scan(&state)
		if err == nil {
			switch state {
			case "completed", "downloading", "queued", "paused", "failed":
				skipped++
				continue
			}
		} else if err != sql.ErrNoRows {
			errs = append(errs, "auto download lookup: "+err.Error())
			continue
		}
		if _, err := h.EnqueueDownload(account, v.ID, v.URL, "video", "720p", "192", 0); err != nil {
			errs = append(errs, "auto download "+v.ID+": "+err.Error())
			continue
		}
		queued++
	}
	return queued, skipped, errs
}

// watchLaterPlaylistID returns the local playlist id that holds one source's
// watch-later list. The id is scoped by account so a YouTube "WL" and a
// Bilibili "WL" stay distinct rows.
func watchLaterPlaylistID(account string) string {
	if account == "" || account == "youtube" {
		return "WL"
	}
	return account + ":WL"
}

// watchLaterPlaylistTitle is the human-readable name for that playlist.
func watchLaterPlaylistTitle(account string) string {
	if account == "bilibili" {
		return "稍后再看"
	}
	return "Watch Later"
}

// channelURLFor builds a placeholder channel URL for the right service.
// YouTube channel ids and Bilibili mid values live in different URL spaces;
// emitting a youtube.com link for a Bilibili uploader would be wrong.
func channelURLFor(account, channelID string) string {
	if account == "bilibili" {
		return "https://space.bilibili.com/" + channelID
	}
	return "https://www.youtube.com/channel/" + channelID
}

// autoDownloadSetting reports whether new videos should be queued.
//
// The value lives in the settings table so the app can flip it without root:
// wifi_sync.sh POSTs without an auto_download field and lets the service
// decide. An unreadable or absent row falls back to the compiled default.
func (h *Handler) autoDownloadSetting(r *http.Request, account string, fallback bool) bool {
	var v string
	err := h.DB.QueryRowContext(r.Context(),
		`SELECT value FROM settings WHERE account_id=? AND key='auto_download'`,
		account).Scan(&v)
	if err != nil {
		return fallback
	}
	switch v {
	case "on", "1", "true":
		return true
	case "off", "0", "false":
		return false
	}
	return fallback
}
