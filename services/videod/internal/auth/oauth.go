package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type OAuthConfig struct{ ClientID, ClientSecret, RedirectURL string }
type OAuthTokens struct {
	Tok, Refresh string
	Expiry       time.Time
}

func NewTokens(a, r string, e time.Time) OAuthTokens {
	return OAuthTokens{Tok: a, Refresh: r, Expiry: e}
}

type OAuthExchanger interface {
	Exchange(context.Context, string, string, string) (OAuthTokens, error)
}
type GoogleOAuth struct {
	Config    OAuthConfig
	Exchanger OAuthExchanger
}
type OAuthSession struct {
	URL      string       `json:"url"`
	State    string       `json:"-"`
	Verifier string       `json:"-"`
	Listener net.Listener `json:"-"`
	used     atomic.Bool
}

func (g *GoogleOAuth) Begin(ctx context.Context, scopes ...string) (*OAuthSession, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("unable to start local authorization callback")
	}
	state := randomURLToken(32)
	verifier := randomURLToken(48)
	if state == "" || verifier == "" {
		listener.Close()
		return nil, errors.New("unable to create authorization request")
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	u, err := url.Parse("https://accounts.google.com/o/oauth2/v2/auth")
	if err != nil {
		listener.Close()
		return nil, errors.New("unable to create authorization request")
	}
	q := u.Query()
	q.Set("client_id", g.Config.ClientID)
	q.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", listener.Addr().(*net.TCPAddr).Port))
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	u.RawQuery = q.Encode()
	return &OAuthSession{URL: u.String(), State: state, Verifier: verifier, Listener: listener}, nil
}
func (g *GoogleOAuth) Complete(ctx context.Context, session *OAuthSession, callback *url.URL) (OAuthTokens, error) {
	if session == nil || session.Listener == nil || callback == nil || session.State == "" || callback.Query().Get("state") != session.State {
		return OAuthTokens{}, errors.New("authorization callback rejected")
	}
	if callback.Query().Get("error") != "" {
		return OAuthTokens{}, errors.New("Google authorization failed")
	}
	code := callback.Query().Get("code")
	if code == "" || g.Exchanger == nil {
		return OAuthTokens{}, errors.New("authorization callback rejected")
	}
	if !session.used.CompareAndSwap(false, true) {
		return OAuthTokens{}, errors.New("authorization callback rejected")
	}
	tokens, err := g.Exchanger.Exchange(ctx, code, session.Verifier, fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", session.Listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	return tokens, nil
}
func randomURLToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func ServeCallback(ctx context.Context, session *OAuthSession, handler func(*url.URL) error) error {
	if session == nil || session.Listener == nil {
		return errors.New("authorization callback unavailable")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || host != "127.0.0.1" {
			http.Error(w, "Authorization failed", 400)
			return
		}
		if _, _, err := net.SplitHostPort(r.Host); err != nil {
			http.Error(w, "Authorization failed", 400)
			return
		}
		if err := handler(r.URL); err != nil {
			http.Error(w, "Authorization failed", 400)
			return
		}
		_, _ = w.Write([]byte("Authorization complete. You may close this window."))
		go session.Listener.Close()
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(session.Listener) }()
	select {
	case <-ctx.Done():
		_ = server.Close()
		return errors.New("authorization callback timed out")
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return errors.New("authorization callback failed")
	}
}
