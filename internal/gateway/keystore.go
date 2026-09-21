package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Provider API keys are the one thing in this system a user would be upset to

const (
	keyFileName    = "keys.enc"
	masterFileName = "master.key"
	masterKeySize  = 32
)

// KeyEntry is a stored credential's metadata. It never carries the secret.
type KeyEntry struct {
	Provider string            `json:"provider"`
	Masked   string            `json:"masked"`
	AddedAt  time.Time         `json:"added_at"`
	Vars     map[string]string `json:"vars,omitempty"`
}

// storedKey is the on-disk shape, inside the sealed blob.
type storedKey struct {
	Key     string            `json:"key"`
	AddedAt time.Time         `json:"added_at"`
	Vars    map[string]string `json:"vars,omitempty"`
}

// KeyStore holds provider credentials encrypted at rest.
type KeyStore struct {
	dir string

	mu   sync.RWMutex
	keys map[string]storedKey
}

// OpenKeyStore loads the store in dir, creating the directory, master key,
// and an empty sealed file on first use.
func OpenKeyStore(dir string) (*KeyStore, error) {
	if dir == "" {
		return nil, errors.New("gateway key store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create key store directory: %w", err)
	}
	s := &KeyStore{dir: dir, keys: map[string]storedKey{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// masterKey returns the local encryption key, generating it on first use.
func (s *KeyStore) masterKey() ([]byte, error) {
	path := filepath.Join(s.dir, masterFileName)
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != masterKeySize {
			return nil, fmt.Errorf("master key at %s is %d bytes, want %d; move it aside to reset the store", path, len(key), masterKeySize)
		}
		return key, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read master key: %w", err)
	}

	key = make([]byte, masterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	// 0600 before any content exists: a key file that is briefly world
	// readable is a key file that leaked.
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	return key, nil
}

func (s *KeyStore) aead() (cipher.AEAD, error) {
	key, err := s.masterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (s *KeyStore) load() error {
	path := filepath.Join(s.dir, keyFileName)
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read key store: %w", err)
	}
	if len(blob) == 0 {
		return nil
	}

	aead, err := s.aead()
	if err != nil {
		return err
	}
	if len(blob) < aead.NonceSize() {
		return fmt.Errorf("key store at %s is truncated", path)
	}
	nonce, sealed := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		// A wrong or replaced master key must not look like "no keys
		// configured", or the gateway would silently route nowhere.
		return fmt.Errorf("decrypt key store: %w (the master key does not match this file)", err)
	}
	keys := map[string]storedKey{}
	if err := json.Unmarshal(plain, &keys); err != nil {
		return fmt.Errorf("parse key store: %w", err)
	}
	s.keys = keys
	return nil
}

// save seals the store to disk atomically so an interrupted write cannot
// leave a half-encrypted file that fails to open.
func (s *KeyStore) save() error {
	plain, err := json.Marshal(s.keys)
	if err != nil {
		return fmt.Errorf("encode key store: %w", err)
	}
	aead, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	blob := append(nonce, aead.Seal(nil, nonce, plain, nil)...)

	path := filepath.Join(s.dir, keyFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write key store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit key store: %w", err)
	}
	return nil
}

// Put stores a credential for a provider, replacing any existing one. vars
// carries endpoint placeholders such as Cloudflare's account id.
func (s *KeyStore) Put(provider, key string, vars map[string]string) error {
	provider = strings.TrimSpace(provider)
	key = strings.TrimSpace(key)
	if provider == "" {
		return errors.New("provider is required")
	}
	if key == "" {
		return errors.New("api key is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[provider] = storedKey{Key: key, AddedAt: time.Now().UTC(), Vars: vars}
	return s.save()
}

// Get returns a stored credential. Callers that also accept an environment
// variable should use Resolve instead.
func (s *KeyStore) Get(provider string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.keys[provider]
	return entry.Key, ok && entry.Key != ""
}

// Vars returns the endpoint placeholder values stored with a credential.
func (s *KeyStore) Vars(provider string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys[provider].Vars
}

// Resolve returns a credential from the store, falling back to the provider's
func (s *KeyStore) Resolve(provider, envVar string) (string, string, bool) {
	if key, ok := s.Get(provider); ok {
		return key, "store", true
	}
	if envVar != "" {
		if key := strings.TrimSpace(os.Getenv(envVar)); key != "" {
			return key, "env:" + envVar, true
		}
	}
	return "", "", false
}

// Delete removes a stored credential.
func (s *KeyStore) Delete(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[provider]; !ok {
		return nil
	}
	delete(s.keys, provider)
	return s.save()
}

// List returns metadata for every stored credential, newest first. The secret
// is never included: the dashboard shows a masked preview so a user can tell
// which key is installed without the page being worth screenshotting.
func (s *KeyStore) List() []KeyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]KeyEntry, 0, len(s.keys))
	for provider, entry := range s.keys {
		out = append(out, KeyEntry{
			Provider: provider,
			Masked:   mask(entry.Key),
			AddedAt:  entry.AddedAt,
			Vars:     entry.Vars,
		})
	}
	slices.SortFunc(out, func(a, b KeyEntry) int {
		if a.AddedAt.Equal(b.AddedAt) {
			return strings.Compare(a.Provider, b.Provider)
		}
		if a.AddedAt.After(b.AddedAt) {
			return -1
		}
		return 1
	})
	return out
}

// mask renders a credential as a recognisable but unusable preview.
func mask(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("•", len(key))
	}
	return key[:4] + strings.Repeat("•", 6) + key[len(key)-4:]
}
