package media

import (
	"context"
	"database/sql"
	"net/http"

	"kanade/videod/internal/store"
)

// resolveAccount picks the account a request operates on.
//
// An explicit ?account_id= wins, because YouTube and Bilibili keep separate
// accounts and therefore separate libraries and download queues. When the
// parameter is absent the historical behaviour is kept: the single active
// account. The caller to use this has already parsed the request.
func resolveAccount(r *http.Request, db *sql.DB) (string, error) {
	if id := r.URL.Query().Get("account_id"); id != "" {
		return id, nil
	}
	return store.ActiveAccount(r.Context(), db)
}

// resolveAccountCtx is the same lookup for callers that only have a context,
// used when a background task needs to know which account it serves.
func resolveAccountCtx(ctx context.Context, db *sql.DB, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return store.ActiveAccount(ctx, db)
}
