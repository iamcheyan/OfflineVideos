package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Secret struct {
	Tok       string    `json:"-"`
	Refresh   string    `json:"-"`
	Cookie    string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}
type Ciphertext struct {
	Nonce []byte
	Data  []byte
}

// CredentialStore is the encryption boundary for at-rest secrets. Ciphertext is
// bound to its account ID; implementations must never return plaintext to callers
// that only need metadata.
type CredentialStore interface {
	Put(accountID string, secret Secret) (Ciphertext, error)
	Get(accountID string, ciphertext Ciphertext) (Secret, error)
	Delete(accountID string) error
	List() ([]string, error)
}

// FileEncryptedStore is the default CredentialStore: AES-256-GCM ciphertext with a
// 0600 key file on local disk. It is local encryption, not an OS keyring.
type FileEncryptedStore struct {
	aead cipher.AEAD
	keys []string
	mu   sync.RWMutex
}

// FileCredentialStore is kept as an alias for the previous exported name.
type FileCredentialStore = FileEncryptedStore

// SystemKeyringStore is reserved for a PJOS keyring-backed implementation.
// It is intentionally unimplemented rather than silently falling back to plaintext.
type SystemKeyringStore struct{}

func (SystemKeyringStore) Put(string, Secret) (Ciphertext, error) {
	return Ciphertext{}, errors.New("system keyring store is not implemented")
}
func (SystemKeyringStore) Get(string, Ciphertext) (Secret, error) {
	return Secret{}, errors.New("system keyring store is not implemented")
}
func (SystemKeyringStore) Delete(string) error {
	return errors.New("system keyring store is not implemented")
}
func (SystemKeyringStore) List() ([]string, error) {
	return nil, errors.New("system keyring store is not implemented")
}

func NewFileCredentialStore(keyPath string) (*FileEncryptedStore, error) {
	if keyPath == "" {
		keyPath = os.Getenv("POCKET_YOUTUBE_AUTH_KEY")
	}
	if keyPath == "" {
		return nil, errors.New("credential key path is not configured")
	}
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, errors.New("unable to initialize credential encryption")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("unable to initialize credential encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("unable to initialize credential encryption")
	}
	return &FileEncryptedStore{aead: aead}, nil
}
func loadOrCreateKey(path string) ([]byte, error) {
	if key, err := os.ReadFile(path); err == nil {
		info, e := os.Stat(path)
		if e != nil || len(key) != 32 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("invalid or insecure key")
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return loadOrCreateKey(path)
		}
		return nil, err
	}
	_, we := f.Write(key)
	ce := f.Close()
	if we != nil {
		_ = os.Remove(path)
		return nil, we
	}
	if ce != nil {
		return nil, ce
	}
	return key, nil
}
func (s *FileEncryptedStore) Put(id string, v Secret) (Ciphertext, error) {
	p := encodeSecret(v)
	nonce := make([]byte, s.aead.NonceSize())
	if _, e := io.ReadFull(rand.Reader, nonce); e != nil {
		clear(p)
		return Ciphertext{}, errors.New("unable to encrypt credentials")
	}
	data := s.aead.Seal(nil, nonce, p, []byte(id))
	s.track(id)
	clear(p)
	return Ciphertext{Nonce: nonce, Data: data}, nil
}

// Delete removes the account from the in-memory index. Ciphertext lives in SQLite,
// so the caller deletes the row; this clears any cached key material reference.
func (s *FileEncryptedStore) Delete(accountID string) error {
	if accountID == "" {
		return errors.New("invalid account id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k == accountID {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			return nil
		}
	}
	return nil
}

// List returns the account IDs this store has issued ciphertext for in-process.
func (s *FileEncryptedStore) List() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.keys...), nil
}

func (s *FileEncryptedStore) track(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k == accountID {
			return
		}
	}
	s.keys = append(s.keys, accountID)
}

func (s *FileEncryptedStore) Get(id string, c Ciphertext) (Secret, error) {
	p, e := s.aead.Open(nil, c.Nonce, c.Data, []byte(id))
	if e != nil {
		return Secret{}, errors.New("unable to decrypt credentials")
	}
	defer clear(p)
	return decodeSecret(p)
}
func encodeSecret(s Secret) []byte {
	fields := []string{s.Tok, s.Refresh, s.Cookie}
	n := 12
	for _, v := range fields {
		n += 4 + len(v)
	}
	out := make([]byte, 0, n)
	for _, v := range fields {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(v)))
		out = append(out, size[:]...)
		out = append(out, v...)
	}
	var expiry [12]byte
	if !s.ExpiresAt.IsZero() {
		binary.BigEndian.PutUint64(expiry[:8], uint64(s.ExpiresAt.Unix()))
		binary.BigEndian.PutUint32(expiry[8:], uint32(s.ExpiresAt.Nanosecond()))
	}
	return append(out, expiry[:]...)
}
func decodeSecret(b []byte) (Secret, error) {
	values := make([]string, 0, 3)
	for range 3 {
		if len(b) < 4 {
			return Secret{}, errors.New("invalid credential payload")
		}
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if len(b) < n {
			return Secret{}, errors.New("invalid credential payload")
		}
		values = append(values, string(b[:n]))
		b = b[n:]
	}
	if len(b) != 12 {
		return Secret{}, errors.New("invalid credential payload")
	}
	sec := int64(binary.BigEndian.Uint64(b[:8]))
	ns := int64(binary.BigEndian.Uint32(b[8:]))
	var expiry time.Time
	if sec != 0 || ns != 0 {
		expiry = time.Unix(sec, ns).UTC()
	}
	return Secret{Tok: values[0], Refresh: values[1], Cookie: values[2], ExpiresAt: expiry}, nil
}
