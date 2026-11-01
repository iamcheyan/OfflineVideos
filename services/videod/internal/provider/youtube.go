package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kanade/videod/internal/domain"
)

// YouTubeConfig controls yt-dlp invocation.
//
// Credentials: this provider does not use Google OAuth API calls. Instead it
// relies on yt-dlp's Netscape cookie jar, which is the only mechanism that can
// reach private lists (Watch Later, private playlists) without a Google Cloud
// project. CookiePath must point to a Netscape-format file; when empty the
// provider can only read public content.
type YouTubeConfig struct {
	Binary     string
	MaxResults int
	CookiePath string
}

type YouTubeProvider struct{ config YouTubeConfig }

func NewYouTubeProvider(c YouTubeConfig) *YouTubeProvider {
	if c.Binary == "" {
		c.Binary = "yt-dlp"
	}
	if c.MaxResults <= 0 {
		c.MaxResults = 20
	}
	return &YouTubeProvider{config: c}
}

func (*YouTubeProvider) Name() string { return "youtube" }

func (*YouTubeProvider) Login(ctx context.Context, kind string, payload LoginPayload) (domain.Account, error) {
	return domain.Account{}, ErrUnauthenticated
}

func (*YouTubeProvider) Logout(ctx context.Context, id string) error { return nil }

// authenticated reports whether a cookie jar is configured and readable.
func (p *YouTubeProvider) authenticated() bool {
	if p.config.CookiePath == "" {
		return false
	}
	st, err := os.Stat(p.config.CookiePath)
	return err == nil && st.Size() > 0
}

// authArgs returns the cookie flags appended to every yt-dlp call.
func (p *YouTubeProvider) authArgs() []string {
	if !p.authenticated() {
		return nil
	}
	return []string{"--cookies", p.config.CookiePath}
}

// run executes yt-dlp and returns stdout. stderr is captured for error text.
func (p *YouTubeProvider) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, p.config.Binary, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("yt-dlp: %s", firstLine(msg))
	}
	return out.String(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseTSV turns "id|title|..." lines into fields, skipping blanks and
// yt-dlp warnings.
func parseTSV(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WARNING") || strings.HasPrefix(line, "ERROR") {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}

// SyncSubscriptions reads the channel list from the account's subscriptions.
// yt-dlp does not expose subscriptions without cookies; a cookie jar signed in
// to YouTube is required.
func (p *YouTubeProvider) SyncSubscriptions(ctx context.Context, id string) ([]domain.Channel, error) {
	if !p.authenticated() {
		return nil, ErrUnauthenticated
	}
	args := append([]string{
		"--skip-download", "--flat-playlist", "--no-warnings",
		"--print", "%(channel_id)s\t%(channel)s\t%(channel_url)s",
	}, p.authArgs()...)
	args = append(args, "https://www.youtube.com/feed/channels")

	out, err := p.run(ctx, 120*time.Second, args...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var channels []domain.Channel
	for _, f := range parseTSV(out) {
		if len(f) < 3 || f[0] == "" || f[0] == "NA" {
			continue
		}
		chID := f[0]
		if seen[chID] {
			continue
		}
		seen[chID] = true
		channels = append(channels, domain.Channel{
			ID:        chID,
			AccountID: id,
			Title:     f[1],
			URL:       f[2],
		})
	}
	return channels, nil
}

// SyncPlaylists lists the account's own playlists.
func (p *YouTubeProvider) SyncPlaylists(ctx context.Context, id string) ([]domain.Playlist, error) {
	if !p.authenticated() {
		return nil, ErrUnauthenticated
	}
	args := append([]string{
		"--skip-download", "--flat-playlist", "--no-warnings",
		"--print", "%(playlist_id)s\t%(playlist_title)s\t%(playlist_count)s",
	}, p.authArgs()...)
	args = append(args, "https://www.youtube.com/feed/playlists")

	out, err := p.run(ctx, 120*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var playlists []domain.Playlist
	for _, f := range parseTSV(out) {
		if len(f) < 2 || f[0] == "" {
			continue
		}
		n := 0
		if len(f) > 2 {
			fmt.Sscanf(f[2], "%d", &n)
		}
		playlists = append(playlists, domain.Playlist{
			ID:         f[0],
			AccountID:  id,
			Title:      f[1],
			VideoCount: n,
		})
	}
	return playlists, nil
}

// SyncWatchLater reads the Watch Later list (playlist id "WL").
//
// Watch Later is always private, so a cookie jar is mandatory.
func (p *YouTubeProvider) SyncWatchLater(ctx context.Context, id string) ([]domain.Video, error) {
	if !p.authenticated() {
		return nil, ErrUnauthenticated
	}
	// --flat-playlist avoids resolving every video. Resolving them aborts the
	// whole listing when a single item is private or removed, which is fatal
	// for a watch-later list. Flat mode returns no thumbnail, so the YouTube
	// image URL is derived from the id instead, matching what the site serves.
	args := append([]string{
		"--skip-download", "--flat-playlist", "--no-warnings",
		"--print", "%(id)s\t%(title)s\t%(duration)s\t%(uploader_id)s",
	}, p.authArgs()...)
	args = append(args, "https://www.youtube.com/playlist?list=WL")

	out, err := p.run(ctx, 120*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var videos []domain.Video
	for _, f := range parseTSV(out) {
		if len(f) < 2 || f[0] == "" || f[0] == "NA" {
			continue
		}
		title := strings.TrimSpace(f[1])
		if title == "" || title == "NA" ||
			title == "[Private video]" || title == "[Deleted video]" ||
			title == "[Unavailable video]" || strings.HasPrefix(title, "[Private video") ||
			strings.HasPrefix(title, "[Deleted video") {
			continue
		}
		dur := 0
		if len(f) > 2 {
			if d, e := parseInt(f[2]); e == nil {
				dur = d
			}
		}
		chID := ""
		if len(f) > 3 && f[3] != "NA" {
			chID = f[3]
		}
		videos = append(videos, domain.Video{
			ID:        f[0],
			AccountID: id,
			ChannelID: chID,
			Title:     f[1],
			URL:       "https://www.youtube.com/watch?v=" + f[0],
			Thumbnail: youtubeThumbnail(f[0]),
			Duration:  dur,
		})
	}
	return videos, nil
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// Search performs a yt-dlp search. No credentials required.
func (p *YouTubeProvider) Search(ctx context.Context, q Query) ([]domain.Video, error) {
	if strings.TrimSpace(q.Text) == "" {
		return []domain.Video{}, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > p.config.MaxResults {
		limit = p.config.MaxResults
	}
	args := []string{
		"--skip-download", "--flat-playlist", "--no-warnings",
		"--print", "%(id)s\t%(title)s\t%(duration)s\t%(uploader_id)s",
		fmt.Sprintf("ytsearch%d:%s", limit, q.Text),
	}
	out, err := p.run(ctx, 90*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var videos []domain.Video
	for _, f := range parseTSV(out) {
		if len(f) < 2 || f[0] == "" || f[0] == "NA" {
			continue
		}
		title := strings.TrimSpace(f[1])
		if title == "" || title == "NA" ||
			title == "[Private video]" || title == "[Deleted video]" ||
			title == "[Unavailable video]" || strings.HasPrefix(title, "[Private video") ||
			strings.HasPrefix(title, "[Deleted video") {
			continue
		}
		dur := 0
		if len(f) > 2 {
			if d, e := parseInt(f[2]); e == nil {
				dur = d
			}
		}
		videos = append(videos, domain.Video{
			ID:       f[0],
			Title:    f[1],
			URL:      "https://www.youtube.com/watch?v=" + f[0],
			Duration: dur,
		})
	}
	return videos, nil
}

// GetVideo fetches metadata for one video.
func (p *YouTubeProvider) GetVideo(ctx context.Context, id string) (domain.Video, error) {
	if id == "" {
		return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
	}
	args := append([]string{
		"--skip-download", "--no-warnings", "--dump-single-json",
	}, p.authArgs()...)
	args = append(args, "https://www.youtube.com/watch?v="+id)

	out, err := p.run(ctx, 60*time.Second, args...)
	if err != nil {
		return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
	}
	var raw struct {
		ID          string  `json:"id"`
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Duration    float64 `json:"duration"`
		UploaderID  string  `json:"uploader_id"`
		Thumbnail   string  `json:"thumbnail"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
	}
	return domain.Video{
		ID:          raw.ID,
		Title:       raw.Title,
		Description: raw.Description,
		Duration:    int(raw.Duration),
		ChannelID:   raw.UploaderID,
		Thumbnail:   raw.Thumbnail,
		URL:         "https://www.youtube.com/watch?v=" + raw.ID,
	}, nil
}

// GetChannel fetches metadata for one channel.
func (p *YouTubeProvider) GetChannel(ctx context.Context, id string) (domain.Channel, error) {
	if id == "" {
		return domain.Channel{}, ErrNotFound{Kind: "channel", ID: id}
	}
	u := id
	if !strings.HasPrefix(u, "http") {
		u = "https://www.youtube.com/channel/" + id
	}
	args := append([]string{
		"--skip-download", "--playlist-items", "0", "--no-warnings",
		"--dump-single-json",
	}, p.authArgs()...)
	args = append(args, u)

	out, err := p.run(ctx, 60*time.Second, args...)
	if err != nil {
		return domain.Channel{}, ErrNotFound{Kind: "channel", ID: id}
	}
	var raw struct {
		ChannelID   string `json:"channel_id"`
		Channel     string `json:"channel"`
		Description string `json:"description"`
		Thumbnail   string `json:"thumbnail"`
		WebpageURL  string `json:"webpage_url"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return domain.Channel{}, ErrNotFound{Kind: "channel", ID: id}
	}
	return domain.Channel{
		ID:          raw.ChannelID,
		Title:       raw.Channel,
		Description: raw.Description,
		Thumbnail:   raw.Thumbnail,
		URL:         raw.WebpageURL,
	}, nil
}

// Download fetches one video/audio file to Destination using yt-dlp.
func (p *YouTubeProvider) Download(ctx context.Context, r DownloadRequest, progress Progress) error {
	if r.VideoID == "" {
		return ErrNotFound{Kind: "video", ID: r.VideoID}
	}
	if err := os.MkdirAll(filepath.Dir(r.Destination), 0o755); err != nil {
		return err
	}
	args := append([]string{
		"--no-warnings", "--newline",
		"-f", "bestvideo+bestaudio/best",
		"-o", r.Destination,
	}, p.authArgs()...)
	args = append(args, "https://www.youtube.com/watch?v="+r.VideoID)
	_, err := p.run(ctx, 30*time.Minute, args...)
	return err
}

// youtubeThumbnail returns the public still for a video id.
//
// The flat listing does not include artwork, but the image host serves a
// deterministic URL per id, so building it avoids a second metadata pass.
func youtubeThumbnail(id string) string {
	if id == "" {
		return ""
	}
	return "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg"
}
