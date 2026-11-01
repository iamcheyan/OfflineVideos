package media

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Probe reports availability and version for yt-dlp and mpv without claiming media playback succeeded.
func Probe(ctx context.Context, cfg Config) map[string]map[string]string {
	paths := map[string]string{"yt-dlp": cfg.YtDlpPath, "mpv": cfg.MpvPath, "ffmpeg": "ffmpeg"}
	flags := map[string]string{"yt-dlp": "--version", "mpv": "--version", "ffmpeg": "-version"}
	if paths["yt-dlp"] == "" {
		paths["yt-dlp"] = "yt-dlp"
	}
	if paths["mpv"] == "" {
		paths["mpv"] = "mpv"
	}
	out := make(map[string]map[string]string, 3)
	for name, path := range paths {
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		b, err := exec.CommandContext(probeCtx, path, flags[name]).CombinedOutput()
		cancel()
		entry := map[string]string{"path": path, "available": "false"}
		if err == nil {
			entry["available"] = "true"
			entry["version"] = strings.TrimSpace(string(b))
		}
		out[name] = entry
	}
	return out
}
