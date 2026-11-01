package domain

import "time"

type Account struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	AuthType  string    `json:"auth_type"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}
type Channel struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Thumbnail   string `json:"thumbnail"`
	Description string `json:"description"`
}
type Video struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	ChannelID   string    `json:"channel_id"`
	Title       string    `json:"title"`
	// ChannelName carries the uploader's display name when the list endpoint
	// already knows it. Writing the id as the channel title instead would show
	// a numeric mid to the user.
	ChannelName string `json:"channel_name,omitempty"`
	URL         string    `json:"url"`
	Thumbnail   string    `json:"thumbnail"`
	Duration    int       `json:"duration"`
	PublishedAt time.Time `json:"published_at"`
	Description string    `json:"description"`
}
type Playlist struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	VideoCount  int    `json:"video_count"`
}
type HistoryItem struct {
	Video     Video     `json:"video"`
	WatchedAt time.Time `json:"watched_at"`
	Position  int       `json:"position"`
}
