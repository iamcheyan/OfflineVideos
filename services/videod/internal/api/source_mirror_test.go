package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kanade/videod/internal/domain"
	"kanade/videod/internal/store"
)

type mirrorFixtureProvider struct{ videos []domain.Video }
func (p mirrorFixtureProvider) SyncSubscriptions(context.Context, string) ([]domain.Channel, error) {
	return nil, nil
}
func (p mirrorFixtureProvider) SyncPlaylists(context.Context, string) ([]domain.Playlist, error) {
	return nil, nil
}
func (p mirrorFixtureProvider) SyncWatchLater(context.Context, string) ([]domain.Video, error) {
	return p.videos, nil
}

func TestSyncRemovesSourceDeletedCacheAndPreservesOtherReferences(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO accounts(id,name,active,created_at) VALUES('youtube','fixture',1,'now')`); err != nil { t.Fatal(err) }
	if _, err := db.Exec(`INSERT INTO channels(id,account_id,title,url) VALUES('ch','youtube','channel','https://example.test')`); err != nil { t.Fatal(err) }
	for _, id := range []string{"keep", "deleted", "referenced"} {
		if _, err := db.Exec(`INSERT INTO videos(id,account_id,channel_id,title,url,published_at) VALUES(?, 'youtube','ch',?, ?,'')`, id, id, "https://example.test/"+id); err != nil { t.Fatal(err) }
		if _, err := db.Exec(`INSERT INTO watch_later(account_id,video_id,added_at) VALUES('youtube',?,'now')`, id); err != nil { t.Fatal(err) }
		if _, err := db.Exec(`INSERT INTO downloads(account_id,video_id,url,state,output_path) VALUES('youtube',?,?,'completed',?)`, id, "https://example.test/"+id, filepath.Join(t.TempDir(), id+".mp4")); err != nil { t.Fatal(err) }
	}
	if _, err := db.Exec(`INSERT INTO playlists(id,account_id,title) VALUES('other','youtube','other'); INSERT INTO playlist_videos(playlist_id,video_id,position) VALUES('other','referenced',0); INSERT INTO source_mirror(account_id,video_id,seen_at) VALUES('youtube','keep','old'),('youtube','deleted','old'),('youtube','referenced','old')`); err != nil { t.Fatal(err) }
	removedFiles := map[string]bool{}
	h := New(db)
	h.SetSyncSources(map[string]SyncProvider{"youtube": mirrorFixtureProvider{videos: []domain.Video{{ID: "keep", AccountID: "youtube", ChannelID: "ch", Title: "keep", URL: "https://example.test/keep"}, {ID: "referenced", AccountID: "youtube", ChannelID: "ch", Title: "referenced", URL: "https://example.test/referenced"}}}})
	h.AutoDownload = false
	h.SyncDeleteDownload = func(_ context.Context, account, id string) (bool, error) {
		if account != "youtube" { return false, errors.New("wrong account") }
		removedFiles[id] = true
		_, err := db.Exec(`DELETE FROM downloads WHERE account_id=? AND video_id=?`, account, id)
		return true, err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader(`{"source":"youtube","what":"watch_later"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.sync(rec, req)
	if rec.Code != http.StatusOK { t.Fatalf("sync status %d: %s", rec.Code, rec.Body.String()) }
	if !removedFiles["deleted"] || removedFiles["keep"] || removedFiles["referenced"] { t.Fatalf("unexpected file removals: %#v", removedFiles) }
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM downloads WHERE account_id='youtube' AND video_id='deleted'`).Scan(&n); err != nil || n != 0 { t.Fatalf("deleted cache rows remain: n=%d err=%v", n, err) }
	if err := db.QueryRow(`SELECT count(*) FROM videos WHERE account_id='youtube' AND id='referenced'`).Scan(&n); err != nil || n != 1 { t.Fatalf("referenced metadata should remain: n=%d err=%v", n, err) }
	_ = os.Stdout
}
