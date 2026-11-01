package provider

import (
	"context"
	"strings"
	"time"

	"kanade/videod/internal/domain"
)

// MockProvider exposes stable public demo content and a synthetic account.
// It does not represent or authenticate a real provider account.
type MockProvider struct {
	videos    []domain.Video
	channels  []domain.Channel
	playlists []domain.Playlist
}

func NewMockProvider() *MockProvider {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	channels := []domain.Channel{{ID: "demo-channel", AccountID: "mock", Title: "Pocket Video Demo", URL: "https://example.invalid/channel/demo", Thumbnail: "", Description: "Deterministic sample channel"}}
	videos := []domain.Video{
		{ID: "demo-video-1", AccountID: "mock", ChannelID: channels[0].ID, Title: "A quiet morning walk", URL: "https://example.invalid/watch/demo-video-1", Duration: 324, PublishedAt: now, Description: "Sample video for exercising provider search and metadata."},
		{ID: "demo-video-2", AccountID: "mock", ChannelID: channels[0].ID, Title: "Making a small paper boat", URL: "https://example.invalid/watch/demo-video-2", Duration: 517, PublishedAt: now.Add(-24 * time.Hour), Description: "A second deterministic sample."},
	}
	return &MockProvider{videos: videos, channels: channels, playlists: []domain.Playlist{{ID: "demo-playlist", AccountID: "mock", Title: "Demo playlist", Description: "Sample playlist", VideoCount: len(videos)}}}
}
func (p *MockProvider) Name() string { return "mock" }
func (p *MockProvider) Login(ctx context.Context, kind string, payload LoginPayload) (domain.Account, error) {
	if err := ctx.Err(); err != nil {
		return domain.Account{}, err
	}
	return domain.Account{ID: "mock", Name: "Demo account", BaseURL: "mock://", Active: true, CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}
func (p *MockProvider) Logout(ctx context.Context, accountID string) error { return ctx.Err() }
func (p *MockProvider) Search(ctx context.Context, q Query) ([]domain.Video, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	out := make([]domain.Video, 0, len(p.videos))
	for _, v := range p.videos {
		if needle == "" || strings.Contains(strings.ToLower(v.Title+" "+v.Description), needle) {
			out = append(out, v)
			if q.Limit > 0 && len(out) >= q.Limit {
				break
			}
		}
	}
	return out, nil
}
func (p *MockProvider) SyncSubscriptions(ctx context.Context, accountID string) ([]domain.Channel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]domain.Channel(nil), p.channels...), nil
}
func (p *MockProvider) SyncPlaylists(ctx context.Context, accountID string) ([]domain.Playlist, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]domain.Playlist(nil), p.playlists...), nil
}
func (p *MockProvider) SyncWatchLater(ctx context.Context, accountID string) ([]domain.Video, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]domain.Video(nil), p.videos...), nil
}
func (p *MockProvider) Download(ctx context.Context, req DownloadRequest, progress Progress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnauthenticated
}
func (p *MockProvider) GetVideo(ctx context.Context, id string) (domain.Video, error) {
	if err := ctx.Err(); err != nil {
		return domain.Video{}, err
	}
	for _, v := range p.videos {
		if v.ID == id {
			return v, nil
		}
	}
	return domain.Video{}, ErrNotFound{Kind: "video", ID: id}
}
func (p *MockProvider) GetChannel(ctx context.Context, id string) (domain.Channel, error) {
	if err := ctx.Err(); err != nil {
		return domain.Channel{}, err
	}
	for _, v := range p.channels {
		if v.ID == id {
			return v, nil
		}
	}
	return domain.Channel{}, ErrNotFound{Kind: "channel", ID: id}
}

var _ VideoProvider = (*MockProvider)(nil)
