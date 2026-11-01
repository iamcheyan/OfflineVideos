package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type GoogleTokenExchanger struct {
	ClientID     string
	ClientSecret string
	Client       *http.Client
}

func (g GoogleTokenExchanger) Exchange(ctx context.Context, code, verifier, redirectURI string) (OAuthTokens, error) {
	if g.ClientID == "" || verifier == "" || code == "" || redirectURI == "" {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{"client_id": {g.ClientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}, "grant_type": {"authorization_code"}}
	if g.ClientSecret != "" {
		form.Set("client_secret", g.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil || body.AccessToken == "" {
		return OAuthTokens{}, errors.New("Google token exchange failed")
	}
	return NewTokens(body.AccessToken, body.RefreshToken, time.Now().Add(time.Duration(body.ExpiresIn)*time.Second)), nil
}
