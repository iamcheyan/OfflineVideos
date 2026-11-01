package main

import (
	"context"
	"errors"
	"kanade/videod/internal/api"
	"kanade/videod/internal/auth"
	"kanade/videod/internal/media"
	"kanade/videod/internal/provider"
	"kanade/videod/internal/store"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "sync" {
		syncCommand(os.Args[2:])
		return
	}
	home, e := os.UserHomeDir()
	if e != nil {
		log.Fatal(e)
	}
	data := env("OFFLINEVIDEOS_DATA_DIR", filepath.Join(home, ".local/share/offlinevideos"))
	downloads := env("OFFLINEVIDEOS_DOWNLOAD_DIR", filepath.Join(home, "Videos/Offline"))
	addr := env("OFFLINEVIDEOS_LISTEN", "127.0.0.1:8732")
	ytDlp := env("OFFLINEVIDEOS_YTDLP", "yt-dlp")
	interval := 60
	if raw := os.Getenv("OFFLINEVIDEOS_SYNC_INTERVAL_MIN"); raw != "" {
		interval, e = strconv.Atoi(raw)
		if e != nil || interval < 0 || interval > 1440 {
			log.Fatal("OFFLINEVIDEOS_SYNC_INTERVAL_MIN must be 0-1440")
		}
	}
	if e = os.MkdirAll(data, 0700); e != nil {
		log.Fatal(e)
	}
	if e = os.MkdirAll(downloads, 0750); e != nil {
		log.Fatal(e)
	}
	db, e := store.Open(filepath.Join(data, "offlinevideos.db"))
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	for _, s := range []string{"youtube", "bilibili"} {
		if e = store.EnsureAccount(context.Background(), db, s); e != nil {
			log.Fatal(e)
		}
	}
	credentials, e := auth.NewFileCredentialStore(filepath.Join(data, "credentials.key"))
	if e != nil {
		log.Fatal(e)
	}
	workers := 2
	if raw := os.Getenv("OFFLINEVIDEOS_CONCURRENCY"); raw != "" {
		workers, e = strconv.Atoi(raw)
		if e != nil || workers < 1 || workers > 8 {
			log.Fatal("OFFLINEVIDEOS_CONCURRENCY must be 1-8")
		}
	}
	ms, e := media.New(db, media.Config{YtDlpPath: ytDlp, MpvPath: env("OFFLINEVIDEOS_MPV", "mpv"), DownloadDir: downloads, IPCDir: filepath.Join(data, "ipc"), Concurrency: workers, Headless: true})
	if e != nil {
		log.Fatal(e)
	}
	defer ms.Close()
	yt := provider.NewYouTubeProvider(provider.YouTubeConfig{Binary: ytDlp, CookiePath: filepath.Join(data, "cookies.txt")})
	bi := provider.NewBilibiliProvider(provider.BilibiliConfig{Binary: ytDlp, CookiePath: filepath.Join(data, "bilibili-cookies.txt")})
	h := api.New(db)
	h.DataDir = data
	h.AutoDownload = env("OFFLINEVIDEOS_AUTO_DOWNLOAD", "on") != "off"
	h.SetSyncSources(map[string]api.SyncProvider{"youtube": yt, "bilibili": bi})
	h.EnqueueDownload = ms.Enqueue
	h.DeleteDownload = func(account, videoID string) (bool, error) {
		return ms.DeleteDownload(context.Background(), account, videoID)
	}
	h.SetHealth(func(ctx context.Context) (map[string]any, error) {
		return map[string]any{"mock": false, "database": "sqlite", "download_dir": downloads}, nil
	})
	mux := http.NewServeMux()
	h.Register(mux)
	media.RegisterRoutes(mux, ms)
	auth.NewHandlers(auth.NewService(db, credentials, auth.OAuthConfig{}, nil)).RegisterRoutes(mux)
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		log.Fatal(e)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("offlinevideos listening on %s", addr)
		if x := srv.Serve(listener); x != nil && !errors.Is(x, http.ErrServerClosed) {
			log.Printf("server: %v", x)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	if interval > 0 {
		go periodic(time.Duration(interval)*time.Minute, addr)
	}
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e = srv.Shutdown(ctx); e != nil {
		log.Printf("shutdown: %v", e)
	}
}
