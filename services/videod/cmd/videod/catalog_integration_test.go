package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kanade/videod/internal/media"
	"kanade/videod/internal/store"
	_ "modernc.org/sqlite"
)

func TestDownloadRunWritesCatalogFromYtDlpOutput(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	binary := filepath.Join(t.TempDir(), "yt-dlp")
	script := "#!/bin/sh\n" +
		"printf 'video fixture' > '" + filepath.Join(root, "fixture.mp4") + "'\n" +
		"printf 'cover fixture' > '" + filepath.Join(root, "fixture.jpg") + "'\n" +
		"printf '%s\\n' '{\"id\":\"fixture\",\"title\":\"Fixture title\",\"description\":\"Fixture description\",\"thumbnail\":\"https://img.example/fixture.jpg\",\"uploader\":\"Fixture uploader\",\"duration\":77,\"view_count\":99,\"webpage_url\":\"https://youtube.example/watch?v=fixture\"}'\n" +
		"printf '%s\\n' '" + filepath.Join(root, "fixture.mp4") + "'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(data, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = store.EnsureAccount(context.Background(), db, "youtube"); err != nil {
		t.Fatal(err)
	}
	svc, err := media.New(db, media.Config{YtDlpPath: binary, DownloadDir: root, IPCDir: filepath.Join(data, "ipc"), Concurrency: 1, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	mux := http.NewServeMux()
	media.RegisterRoutes(mux, svc)
	request := httptest.NewRequest(http.MethodPost, "/api/downloads", strings.NewReader(`{"video_id":"fixture","url":"https://youtube.example/watch?v=fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue status %d: %s", response.Code, response.Body.String())
	}
	var state string
	for range 100 {
		_ = db.QueryRow(`SELECT state FROM downloads WHERE video_id='fixture'`).Scan(&state)
		if state == "completed" || state == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state != "completed" {
		t.Fatalf("download state %q", state)
	}
	catalogResponse := httptest.NewRecorder()
	mux.ServeHTTP(catalogResponse, httptest.NewRequest(http.MethodGet, "/api/catalog", nil))
	if catalogResponse.Code != http.StatusOK || !strings.Contains(catalogResponse.Body.String(), "Fixture title") || !strings.Contains(catalogResponse.Body.String(), "fixture.jpg") {
		t.Fatalf("unexpected catalog response: %s", catalogResponse.Body.String())
	}
}
