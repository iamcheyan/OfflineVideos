package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

type Handlers struct{ Service *AuthService }
type cookieRequest struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Cookie  string `json:"cookie"`
}
type oauthBeginRequest struct {
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Scopes  []string `json:"scopes"`
}

func NewHandler(db *sql.DB, credentials CredentialStore, config OAuthConfig, exchanger OAuthExchanger) *Handlers {
	return NewHandlers(NewService(db, credentials, config, exchanger))
}
func NewHandlers(service *AuthService) *Handlers { return &Handlers{Service: service} }
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/accounts/login", h.Login)
	mux.HandleFunc("POST /api/auth/cookie", h.ImportCookie)
	mux.HandleFunc("POST /api/auth/oauth", h.BeginOAuth)
	mux.HandleFunc("DELETE /api/accounts/{id}", h.RemoveAccount)
	mux.HandleFunc("POST /api/accounts/{id}/activate", h.ActivateAccount)
	mux.HandleFunc("POST /api/accounts/logout", h.Logout)
}
func RegisterRoutes(mux *http.ServeMux, h *Handlers) { h.RegisterRoutes(mux) }

// Login is the unified entry point: {kind:"cookie"|"oauth", payload:{...}}.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind    string         `json:"kind"`
		Payload map[string]any `json:"payload"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "kind and payload are required")
		return
	}
	payload := in.Payload
	str := func(key string) string {
		if v, ok := payload[key].(string); ok {
			return v
		}
		return ""
	}
	switch in.Kind {
	case "cookie":
		req := cookieRequest{Name: str("name"), BaseURL: str("base_url"), Cookie: str("cookie")}
		if req.Name == "" || req.BaseURL == "" || req.Cookie == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "cookie login requires name, base_url and cookie")
			return
		}
		if h == nil || h.Service == nil {
			writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication unavailable")
			return
		}
		id, err := h.Service.ImportCookie(r.Context(), req.Name, req.BaseURL, req.Cookie)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "login_failed", "unable to import cookie")
			return
		}
		writeSafeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": req.Name, "base_url": req.BaseURL, "auth_type": "cookie"})
	case "oauth":
		scopes, _ := payload["scopes"].([]any)
		list := make([]string, 0, len(scopes))
		for _, v := range scopes {
			if scope, ok := v.(string); ok {
				list = append(list, scope)
			}
		}
		h.beginOAuth(w, r, oauthBeginRequest{Name: str("name"), BaseURL: str("base_url"), Scopes: list})
	default:
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be oauth or cookie")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeSafeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func (h *Handlers) ImportCookie(w http.ResponseWriter, r *http.Request) {
	var req cookieRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.Name == "" || req.BaseURL == "" || req.Cookie == "" {
		writeError(w, 400, "invalid_request", "cookie login requires name, base_url and cookie")
		return
	}
	if h == nil || h.Service == nil {
		writeError(w, 503, "auth_unavailable", "authentication unavailable")
		return
	}
	id, err := h.Service.ImportCookie(r.Context(), req.Name, req.BaseURL, req.Cookie)
	if err != nil {
		writeError(w, 500, "login_failed", "unable to import cookie")
		return
	}
	writeSafeJSON(w, 201, map[string]string{"id": id, "name": req.Name, "base_url": req.BaseURL, "auth_type": "cookie"})
}

func (h *Handlers) BeginOAuth(w http.ResponseWriter, r *http.Request) {
	var req oauthBeginRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil || req.Name == "" || req.BaseURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "OAuth login requires name and base_url")
		return
	}
	h.beginOAuth(w, r, req)
}

func (h *Handlers) beginOAuth(w http.ResponseWriter, r *http.Request, req oauthBeginRequest) {
	if req.Name == "" || req.BaseURL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "OAuth login requires name and base_url")
		return
	}
	if h == nil || h.Service == nil || h.Service.OAuth == nil {
		writeError(w, 503, "auth_unavailable", "authentication unavailable")
		return
	}
	session, err := h.Service.OAuth.Begin(r.Context(), req.Scopes...)
	if err != nil {
		writeError(w, 500, "oauth_failed", "unable to start authorization")
		return
	}
	callbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	callbackURL := session.Listener.Addr().String()
	writeSafeJSON(w, 202, map[string]string{"url": session.URL, "callback": "http://" + callbackURL + "/oauth/callback"})
	go func() {
		defer cancel()
		defer session.Listener.Close()
		_ = ServeCallback(callbackCtx, session, func(callback *url.URL) error {
			_, err := h.Service.CompleteOAuth(callbackCtx, session, callback, req.Name, req.BaseURL)
			return err
		})
	}()
}

func (h *Handlers) RemoveAccount(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Service == nil || h.Service.Store == nil {
		writeError(w, 503, "auth_unavailable", "authentication unavailable")
		return
	}
	if err := h.Service.Store.RemoveAccount(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, 404, "not_found", "account not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handlers) ActivateAccount(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Service == nil || h.Service.Store == nil {
		writeError(w, 503, "auth_unavailable", "authentication unavailable")
		return
	}
	if err := h.Service.Store.ActivateAccount(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, 404, "not_found", "account not found")
		return
	}
	writeSafeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Service == nil || h.Service.Store == nil {
		writeError(w, 503, "auth_unavailable", "authentication unavailable")
		return
	}
	if err := h.Service.Store.Logout(r.Context()); err != nil {
		writeError(w, 500, "logout_failed", "unable to log out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func writeSafeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
