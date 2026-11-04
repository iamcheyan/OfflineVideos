package media

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogWritesMetadataAndUpdatesExistingItem(t *testing.T) {
	root := t.TempDir()
	mediaFile := filepath.Join(root, "video-1.mp4")
	if err := os.WriteFile(mediaFile, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "video-1.jpg"), []byte("thumb"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: Config{DownloadDir: root}}
	j := &job{AccountID: "youtube", VideoID: "video-1", URL: "https://youtube.example/watch?v=video-1"}
	meta := map[string]any{"id": "video-1", "title": "Fixture title", "description": "Fixture description", "thumbnail": "https://img.example/thumb.jpg", "uploader": "Fixture channel", "duration": 42.0, "view_count": 123.0, "webpage_url": j.URL}
	if err := s.updateCatalog(j, meta, mediaFile); err != nil {
		t.Fatal(err)
	}
	if err := s.updateCatalog(j, meta, mediaFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, catalogName))
	if err != nil {
		t.Fatal(err)
	}
	var got catalog
	if err = json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.Items) != 1 {
		t.Fatalf("catalog version/items: %d/%d", got.Version, len(got.Items))
	}
	item := got.Items[0]
	if item.Source != "youtube" || item.Title != "Fixture title" || item.Description != "Fixture description" || item.File != "video-1.mp4" || item.ThumbnailFile != "video-1.jpg" || !item.ThumbnailDownloaded || item.Duration != 42 || item.ViewCount != 123 {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestCatalogRefusesOutputOutsideDownloadRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mediaFile := filepath.Join(outside, "video.mp4")
	if err := os.WriteFile(mediaFile, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: Config{DownloadDir: root}}
	if err := s.updateCatalog(&job{AccountID: "bilibili", VideoID: "BVfixture"}, map[string]any{"title": "fixture"}, mediaFile); err == nil {
		t.Fatal("expected outside-root path rejection")
	}
}

func TestCatalogRebuildIgnoresLegacyOutputPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "old.mp4")
	if err := os.WriteFile(outside, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, db := newCatalogTestService(t, root)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO downloads(account_id,video_id,url,state,output_path) VALUES('youtube','old','https://youtube.example/watch?v=old','completed',?)`, outside); err != nil {
		t.Fatal(err)
	}
	if err := service.RebuildCatalogFromDB(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, catalogName))
	if err != nil {
		t.Fatal(err)
	}
	var got catalog
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("legacy path must be skipped: %+v", got.Items)
	}
}

func TestCatalogRebuildKeepsDownloadTime(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"old.mp4", "new.mp4"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service, db := newCatalogTestService(t, root)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO downloads(account_id,video_id,url,state,output_path,created_at) VALUES
		('youtube','old','https://youtube.example/watch?v=old','completed',?,'2026-01-02 03:04:05'),
		('youtube','new','https://youtube.example/watch?v=new','completed',?,'2026-02-03 04:05:06')`,
		filepath.Join(root, "old.mp4"), filepath.Join(root, "new.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := service.RebuildCatalogFromDB(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, catalogName))
	if err != nil {
		t.Fatal(err)
	}
	var got catalog
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	stamps := map[string]string{}
	for _, item := range got.Items {
		stamps[item.ID] = item.DownloadedAt
	}
	if stamps["old"] != "2026-01-02T03:04:05Z" || stamps["new"] != "2026-02-03T04:05:06Z" {
		t.Fatalf("rebuild must keep queue time, got %+v", stamps)
	}
}

func newCatalogTestService(t *testing.T, root string) (*Service, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`CREATE TABLE accounts(id TEXT PRIMARY KEY)`,
		`CREATE TABLE channels(id TEXT,account_id TEXT,title TEXT)`,
		`CREATE TABLE videos(id TEXT,account_id TEXT,channel_id TEXT,title TEXT,description TEXT,thumbnail TEXT,duration INTEGER,published_at TEXT)`,
		`CREATE TABLE downloads(id INTEGER PRIMARY KEY,account_id TEXT,video_id TEXT,url TEXT,mode TEXT DEFAULT 'video',state TEXT,output_path TEXT,created_at TEXT DEFAULT CURRENT_TIMESTAMP)`,
	}
	for _, stmt := range stmts {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return &Service{db: db, cfg: Config{DownloadDir: root}}, db
}
