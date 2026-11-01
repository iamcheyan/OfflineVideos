package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kanade/videod/internal/domain"
)

// BilibiliConfig controls the Bilibili provider.
//
// Credentials: Bilibili's private lists (稍后再看 / watch later, favourites)
// are readable with the Netscape cookie jar written by the browser or by
// yt-dlp's --cookies-from-browser. The jar must contain SESSDATA and
// DedeUserID; removing an item from 稍后再看 additionally needs bili_jct,
// which is the CSRF token.
//
// Downloading and list enumeration both go through yt-dlp, whose
// BilibiliWatchlater extractor calls api.bilibili.com/x/v2/history/toview/web.
type BilibiliConfig struct {
	Binary     string
	MaxResults int
	CookiePath string
}

type BilibiliProvider struct{ config BilibiliConfig }

// watchLaterURL is the exact URL matched by yt-dlp's BilibiliWatchlaterIE.
const watchLaterURL = "https://www.bilibili.com/watchlater"

// watchLaterDeleteURL removes one entry from 稍后再看.
const watchLaterDeleteURL = "https://api.bilibili.com/x/v2/history/toview/del"

// watchLaterListURL is the JSON endpoint BilibiliWatchlaterIE reads. Calling it
// directly is how a stale cookie is detected before yt-dlp silently reports an
// empty list.
const watchLaterListURL = "https://api.bilibili.com/x/v2/history/toview/web"

// browserUserAgent is sent with every direct API call. Bilibili rejects some
// endpoints for clients that look automated, and yt-dlp uses a browser string
// for the same reason.
const browserUserAgent = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36"

func NewBilibiliProvider(c BilibiliConfig) *BilibiliProvider {
	if c.Binary == "" {
		c.Binary = "yt-dlp"
	}
	if c.MaxResults <= 0 {
		c.MaxResults = 20
	}
	return &BilibiliProvider{config: c}
}

func (*BilibiliProvider) Name() string { return "bilibili" }

// Login is not supported: Bilibili cookies are pasted or imported, never
// obtained through an OAuth exchange this provider performs.
func (*BilibiliProvider) Login(context.Context, string, LoginPayload) (domain.Account, error) {
	return domain.Account{}, ErrUnauthenticated
}

func (*BilibiliProvider) Logout(context.Context, string) error { return nil }

func (p *BilibiliProvider) authenticated() bool {
	if p.config.CookiePath == "" {
		return false
	}
	st, err := os.Stat(p.config.CookiePath)
	return err == nil && st.Size() > 0
}

func (p *BilibiliProvider) authArgs() []string {
	if !p.authenticated() {
		return nil
	}
	return []string{"--cookies", p.config.CookiePath}
}

func (p *BilibiliProvider) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, p.config.Binary, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("bilibili: %s", msg)
	}
	return out.String(), nil
}

// SyncWatchLater reads 稍后再看.
//
// This uses Bilibili's own JSON endpoint rather than yt-dlp's
// BilibiliWatchlater extractor. With --flat-playlist that extractor emits NA
// for title, duration and uploader, which is useless for a library view, and
// the endpoint additionally carries cover art, uploader name and the watched
// flag that yt-dlp never reports.
func (p *BilibiliProvider) SyncWatchLater(ctx context.Context, id string) ([]domain.Video, error) {
	if !p.authenticated() {
		return nil, ErrUnauthenticated
	}
	jar, err := ReadNetscapeCookies(p.config.CookiePath)
	if err != nil {
		return nil, err
	}
	sessdata := jar.Value(".bilibili.com", "SESSDATA")
	if sessdata == "" {
		return nil, fmt.Errorf("bilibili: cookie jar needs SESSDATA")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchLaterListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", "SESSDATA="+sessdata)
	req.Header.Set("Referer", "https://www.bilibili.com/watchlater")
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List []struct {
				Bvid     string `json:"bvid"`
				Aid      int64  `json:"aid"`
				Title    string `json:"title"`
				Duration int    `json:"duration"`
				Pic      string `json:"pic"`
				Desc     string `json:"desc"`
				Pubdate  int64  `json:"pubdate"`
				Progress int    `json:"progress"`
				Viewed   bool   `json:"viewed"`
				Owner    struct {
					Mid  int64  `json:"mid"`
					Name string `json:"name"`
				} `json:"owner"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := decodeJSONBody(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("bilibili: watch later rejected (code %d %s)", payload.Code, payload.Message)
	}

	videos := make([]domain.Video, 0, len(payload.Data.List))
	for _, item := range payload.Data.List {
		if item.Bvid == "" {
			continue
		}
		// "Unknown" owners must stay blank so the caller picks a placeholder
		// channel; a synthetic name here would look like real data.
		channelID := ""
		if item.Owner.Mid != 0 {
			channelID = fmt.Sprintf("%d", item.Owner.Mid)
		}
		published := time.Time{}
		if item.Pubdate > 0 {
			published = time.Unix(item.Pubdate, 0).UTC()
		}
		videos = append(videos, domain.Video{
			ID:          item.Bvid,
			AccountID:   id,
			ChannelID:   channelID,
			ChannelName: item.Owner.Name,
			Title:       item.Title,
			URL:         "https://www.bilibili.com/video/" + item.Bvid,
			Thumbnail:   httpsImageURL(item.Pic),
			Duration:    item.Duration,
			PublishedAt: published,
			Description: item.Desc,
		})
	}
	return videos, nil
}

// SyncSubscriptions and SyncPlaylists are unsupported: the reserved account
// model covers 稍后再看 only. Returning an explicit error is better than
// silently reporting zero items.
func (*BilibiliProvider) SyncSubscriptions(context.Context, string) ([]domain.Channel, error) {
	return nil, fmt.Errorf("bilibili: subscriptions are not supported")
}

func (*BilibiliProvider) SyncPlaylists(context.Context, string) ([]domain.Playlist, error) {
	return nil, fmt.Errorf("bilibili: playlists are not supported")
}

// Search uses yt-dlp's bilisearch prefix.
func (p *BilibiliProvider) Search(ctx context.Context, q Query) ([]domain.Video, error) {
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
		fmt.Sprintf("bilisearch%d:%s", limit, q.Text),
	}
	out, err := p.run(ctx, 90*time.Second, args...)
	if err != nil {
		return nil, err
	}
	videos := make([]domain.Video, 0)
	for _, f := range parseTSV(out) {
		if len(f) < 2 || f[0] == "" || f[0] == "NA" {
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
			URL:      "https://www.bilibili.com/video/" + f[0],
			Duration: dur,
		})
	}
	return videos, nil
}

// GetVideo fetches metadata for a single BV id.
func (p *BilibiliProvider) GetVideo(ctx context.Context, id string) (domain.Video, error) {
	if id == "" {
		return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
	}
	args := append([]string{
		"--skip-download", "--no-warnings",
		"--print", "%(id)s\t%(title)s\t%(duration)s\t%(uploader_id)s",
	}, p.authArgs()...)
	args = append(args, "https://www.bilibili.com/video/"+id)

	out, err := p.run(ctx, 60*time.Second, args...)
	if err != nil {
		return domain.Video{}, err
	}
	for _, f := range parseTSV(out) {
		if len(f) < 2 || f[0] == "" || f[0] == "NA" {
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
		return domain.Video{
			ID:        f[0],
			ChannelID: chID,
			Title:     f[1],
			URL:       "https://www.bilibili.com/video/" + f[0],
			Duration:  dur,
		}, nil
	}
	return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
}

// GetChannel is unsupported; the account model stores only a channel id.
func (*BilibiliProvider) GetChannel(context.Context, string) (domain.Channel, error) {
	return domain.Channel{}, ErrNotFound{Kind: "channel", ID: ""}
}

// Download fetches one video using yt-dlp.
func (p *BilibiliProvider) Download(ctx context.Context, r DownloadRequest, progress Progress) error {
	if r.VideoID == "" {
		return ErrNotFound{Kind: "video", ID: r.VideoID}
	}
	if err := os.MkdirAll(filepath.Dir(r.Destination), 0o755); err != nil {
		return err
	}
	args := append([]string{
		"--no-warnings", "--newline",
		"-f", "bv*+ba/b",
		"-o", r.Destination,
	}, p.authArgs()...)
	args = append(args, "https://www.bilibili.com/video/"+r.VideoID)
	_, err := p.run(ctx, 30*time.Minute, args...)
	return err
}

// RemoveFromWatchLater deletes one entry from 稍后再看.
//
// This is the write-back that makes "watched, so it disappears" possible.
// Bilibili accepts it with cookies alone (no OAuth), but it needs bili_jct as
// the CSRF token and DedeUserID to identify the account.
func (p *BilibiliProvider) RemoveFromWatchLater(ctx context.Context, bvid string) error {
	if !p.authenticated() {
		return ErrUnauthenticated
	}
	if bvid == "" {
		return ErrNotFound{Kind: "video", ID: bvid}
	}
	jar, err := ReadNetscapeCookies(p.config.CookiePath)
	if err != nil {
		return err
	}
	sessdata := jar.Value(".bilibili.com", "SESSDATA")
	csrf := jar.Value(".bilibili.com", "bili_jct")
	if sessdata == "" || csrf == "" {
		return fmt.Errorf("bilibili: cookie jar needs SESSDATA and bili_jct")
	}
	// The API takes the numeric aid, so resolve BV -> aid first.
	aid, err := p.aidForBV(ctx, bvid)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("aid", aid)
	form.Set("csrf", csrf)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, watchLaterDeleteURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "SESSDATA="+sessdata+"; bili_jct="+csrf)
	req.Header.Set("Referer", "https://www.bilibili.com/watchlater")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := decodeJSONBody(resp.Body, &payload); err != nil {
		return err
	}
	if payload.Code != 0 {
		return fmt.Errorf("bilibili: delete failed code=%d message=%s", payload.Code, payload.Message)
	}
	return nil
}

// aidForBV resolves the numeric aid the remove API expects. yt-dlp prints
// only the BV id, so the value comes from Bilibili's view endpoint.
func (p *BilibiliProvider) aidForBV(ctx context.Context, bvid string) (string, error) {
	endpoint := "https://api.bilibili.com/x/web-interface/view?bvid=" + url.QueryEscape(bvid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	jar, err := ReadNetscapeCookies(p.config.CookiePath)
	if err != nil {
		return "", err
	}
	if v := jar.Value(".bilibili.com", "SESSDATA"); v != "" {
		req.Header.Set("Cookie", "SESSDATA="+v)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Aid  int64 `json:"aid"`
			Bvid string `json:"bvid"`
		} `json:"data"`
	}
	if err := decodeJSONBody(resp.Body, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 {
		return "", fmt.Errorf("bilibili: view lookup failed code=%d", payload.Code)
	}
	return fmt.Sprintf("%d", payload.Data.Aid), nil
}

// VerifyCredentials asks Bilibili whether the jar is actually signed in.
//
// yt-dlp reports an empty watch-later list for an expired cookie with exit
// code 0, so a sync that trusted it would look successful while seeing
// nothing. The API distinguishes the cases: -101 means anonymous, -400 means
// malformed or stale credentials, 0 means signed in.
func (p *BilibiliProvider) VerifyCredentials(ctx context.Context) error {
	if !p.authenticated() {
		return ErrUnauthenticated
	}
	jar, err := ReadNetscapeCookies(p.config.CookiePath)
	if err != nil {
		return err
	}
	sessdata := jar.Value(".bilibili.com", "SESSDATA")
	dede := jar.Value(".bilibili.com", "DedeUserID")
	if sessdata == "" || dede == "" {
		return fmt.Errorf("bilibili: cookie jar needs SESSDATA and DedeUserID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchLaterListURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", "SESSDATA="+sessdata+"; DedeUserID="+dede)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := decodeJSONBody(resp.Body, &payload); err != nil {
		return err
	}
	switch payload.Code {
	case 0:
		return nil
	case -101:
		return fmt.Errorf("bilibili: not signed in (code -101 %s); re-import the cookie", payload.Message)
	default:
		return fmt.Errorf("bilibili: cookie rejected (code %d %s); re-import the cookie", payload.Code, payload.Message)
	}
}

var _ VideoProvider = (*BilibiliProvider)(nil)

// httpsImageURL upgrades a Bilibili image URL to https.
//
// The API returns cover links over http, and Android blocks cleartext by
// default. The CDN serves the same object over https, so upgrading here keeps
// the app's network policy strict instead of punching a hole for the image
// hosts.
func httpsImageURL(raw string) string {
	if strings.HasPrefix(raw, "http://") {
		return "https://" + strings.TrimPrefix(raw, "http://")
	}
	return raw
}
