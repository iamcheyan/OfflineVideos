package provider

import (
	"context"

	"kanade/videod/internal/domain"
)

// Query describes a provider search. Limit <= 0 means the provider default.
type Query struct {
	Text  string
	Limit int
}

// LoginPayload carries provider-specific credentials. Providers must document
// which fields they accept and must not claim authentication they did not do.
type LoginPayload map[string]string

// DownloadRequest identifies a video and destination. Progress receives
// completed and total byte counts; total may be zero when unknown.
type DownloadRequest struct {
	VideoID     string
	Destination string
}
type Progress func(completed, total int64)

// VideoProvider is the domain boundary for account and content operations.
type VideoProvider interface {
	Name() string
	Login(context.Context, string, LoginPayload) (domain.Account, error)
	Logout(context.Context, string) error
	Search(context.Context, Query) ([]domain.Video, error)
	SyncSubscriptions(context.Context, string) ([]domain.Channel, error)
	SyncPlaylists(context.Context, string) ([]domain.Playlist, error)
	SyncWatchLater(context.Context, string) ([]domain.Video, error)
	Download(context.Context, DownloadRequest, Progress) error
	GetVideo(context.Context, string) (domain.Video, error)
	GetChannel(context.Context, string) (domain.Channel, error)
}

// ErrUnauthenticated indicates an operation requires credentials that are not
// available to this provider instance.
var ErrUnauthenticated = errUnauthenticated{}

type errUnauthenticated struct{}

func (errUnauthenticated) Error() string { return "provider operation requires authenticated account" }

// ErrNotFound is returned when a requested item is absent.
type ErrNotFound struct{ Kind, ID string }

func (e ErrNotFound) Error() string { return e.Kind + " not found: " + e.ID }
