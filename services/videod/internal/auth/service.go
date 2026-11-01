package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"time"
)

type EncryptedStore struct {
	DB          *sql.DB
	Credentials CredentialStore
}

func NewEncryptedStore(db *sql.DB, credentials CredentialStore) *EncryptedStore {
	return &EncryptedStore{DB: db, Credentials: credentials}
}
func (s *EncryptedStore) CreateAccount(ctx context.Context, name, baseURL, authType string, secret Secret, expiry time.Time) (string, error) {
	if s == nil || s.DB == nil || s.Credentials == nil {
		return "", errors.New("authentication storage unavailable")
	}
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		return "", errors.New("unable to create account")
	}
	accountID := base64.RawURLEncoding.EncodeToString(id)
	ciphertext, err := s.Credentials.Put(accountID, secret)
	if err != nil {
		return "", errors.New("unable to store credentials")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", errors.New("unable to create account")
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,name,base_url,auth_type,active,created_at) VALUES(?,?,?,?,0,?)`, accountID, name, baseURL, authType, now); err != nil {
		return "", errors.New("unable to create account")
	}
	packed := packCiphertext(ciphertext)
	var access, refresh, cookie, expires any
	if secret.Tok != "" {
		access = packed
	}
	if secret.Refresh != "" {
		refresh = packed
	}
	if secret.Cookie != "" {
		cookie = packed
	}
	if !expiry.IsZero() {
		expires = expiry.UTC().Format(time.RFC3339Nano)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO oauth_tokens(account_id,encrypted_access_token,encrypted_refresh_token,encrypted_cookie,encrypted_credentials,token_expires_at,updated_at) VALUES(?,?,?,?,?,?,?)`, accountID, access, refresh, cookie, packed, expires, now); err != nil {
		return "", errors.New("unable to store credentials")
	}
	if err = tx.Commit(); err != nil {
		return "", errors.New("unable to create account")
	}
	return accountID, nil
}
func packCiphertext(c Ciphertext) []byte {
	out := make([]byte, 2+len(c.Nonce)+len(c.Data))
	out[0] = byte(len(c.Nonce) >> 8)
	out[1] = byte(len(c.Nonce))
	copy(out[2:], c.Nonce)
	copy(out[2+len(c.Nonce):], c.Data)
	return out
}
func (s *EncryptedStore) RemoveAccount(ctx context.Context, id string) error {
	if s == nil || s.DB == nil || id == "" {
		return errors.New("unable to remove account")
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	if err != nil {
		return errors.New("unable to remove account")
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return errors.New("unable to remove account")
	}
	return nil
}
func (s *EncryptedStore) ActivateAccount(ctx context.Context, id string) error {
	if s == nil || s.DB == nil || id == "" {
		return errors.New("unable to activate account")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("unable to activate account")
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id=?`, id).Scan(&exists); err != nil {
		return errors.New("unable to activate account")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET active=(id=?)`, id); err != nil {
		return errors.New("unable to activate account")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("unable to activate account")
	}
	return nil
}
func (s *EncryptedStore) Logout(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("unable to log out")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET active=0 WHERE active=1`)
	if err != nil {
		return errors.New("unable to log out")
	}
	return nil
}

type AuthService struct {
	Store *EncryptedStore
	OAuth *GoogleOAuth
}

func (s *AuthService) ImportCookie(ctx context.Context, name, baseURL, cookie string) (string, error) {
	if s == nil || s.Store == nil || name == "" || baseURL == "" || cookie == "" {
		return "", errors.New("invalid cookie import")
	}
	return s.Store.CreateAccount(ctx, name, baseURL, "cookie", Secret{Cookie: cookie}, time.Time{})
}
func (s *AuthService) CompleteOAuth(ctx context.Context, session *OAuthSession, callback *url.URL, name, baseURL string) (string, error) {
	if s == nil || s.Store == nil || s.OAuth == nil {
		return "", errors.New("authentication unavailable")
	}
	tokens, err := s.OAuth.Complete(ctx, session, callback)
	if err != nil {
		return "", err
	}
	return s.Store.CreateAccount(ctx, name, baseURL, "oauth", secretFromTokens(tokens), tokens.Expiry)
}
func NewService(db *sql.DB, credentials CredentialStore, config OAuthConfig, exchanger OAuthExchanger) *AuthService {
	return &AuthService{Store: NewEncryptedStore(db, credentials), OAuth: &GoogleOAuth{Config: config, Exchanger: exchanger}}
}

func secretFromTokens(t OAuthTokens) Secret {
	return Secret{Tok: t.Tok, Refresh: t.Refresh, ExpiresAt: t.Expiry}
}
