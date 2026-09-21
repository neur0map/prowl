package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const tokenFileName = "token"

// tokenHeader carries the local authorisation token.
const tokenHeader = "X-Prowl-Gateway-Token"

// EnsureToken returns the machine-local bootstrap credential, creating it on
// first use. It is deliberately separate from a dashboard session and from the
// unified inference key: this one exists so a freshly launched harness can
// open its own dashboard without an account.
func EnsureToken(dir string) (string, error) { return loadOrCreateToken(dir) }

func loadOrCreateToken(dir string) (string, error) {
	secret, err := firstWriterSecretFile(dir, tokenFileName, loadToken, generateToken)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(secret)), nil
}

// loadToken reports a usable token, or (nil, false, nil) when the file is
// missing or empty. An empty file is the crash-window artifact of an
// interrupted create: it carries no secret, so it is recoverable rather than
// fatal - reported as "not usable" so the caller re-mints one under the lock
// instead of wedging on it forever.
func loadToken(path string) ([]byte, bool, error) {
	token, ok, err := readToken(path)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return []byte(token), true, nil
}

func generateToken() ([]byte, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate gateway token: %w", err)
	}
	return []byte(hex.EncodeToString(raw)), nil
}

// readToken returns the stored token and whether a non-empty one was present.
func readToken(path string) (string, bool, error) {
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read gateway token: %w", err)
	}
	token := strings.TrimSpace(string(blob))
	return token, token != "", nil
}

// secretFileMu serialises first-writer secret creation within this process; the
// per-file flock beneath it serialises creation across processes. It mirrors
// the pid-record locking convention (pidfile.go) rather than inventing a second
// mechanism.
var secretFileMu sync.Mutex

// firstWriterSecretFile returns the durable secret at dir/name, minting it once
// with crash-safe first-writer-wins semantics when none is usable yet.
//
// load inspects the current file: (b, true, nil) is a usable secret, returned
// verbatim; (nil, false, nil) means the file is missing or recoverably unusable
// (e.g. a stale empty token) and should be (re)created; a non-nil error is
// propagated unchanged, so a caller can refuse to silently regenerate corrupt
// fixed-length material. gen mints the bytes to persist when none is usable.
//
// Creators are serialised by a per-file flock, so exactly one process writes
// and every other re-reads that winner: the store never holds divergent
// secrets. The bytes are staged in a temp file and atomically renamed into
// place, so a crash can never publish a half-written secret, and a stale
// empty/partial file left by an interrupted create is replaced under the lock
// instead of wedging the store forever.
func firstWriterSecretFile(dir, name string, load func(path string) ([]byte, bool, error), gen func() ([]byte, error)) ([]byte, error) {
	path := filepath.Join(dir, name)
	if b, ok, err := load(path); err != nil || ok {
		return b, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create gateway dir: %w", err)
	}

	secretFileMu.Lock()
	defer secretFileMu.Unlock()

	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("lock gateway %s: %w", name, err)
	}
	if !locked {
		return nil, fmt.Errorf("gateway %s lock was not acquired", name)
	}
	defer func() { _ = lock.Unlock() }()

	// Re-check under the lock: a peer creator may have won between our first
	// read and acquiring the lock. Reusing its secret keeps every caller on one
	// value, so no divergent secret is ever minted.
	if b, ok, err := load(path); err != nil || ok {
		return b, err
	}

	secret, err := gen()
	if err != nil {
		return nil, err
	}
	if err := writeSecretFileAtomic(path, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// writeSecretFileAtomic publishes data at path through a fully-written temp file
// renamed into place, so no reader ever observes a half-written or empty secret
// and a crash before the rename leaves the durable path untouched.
func writeSecretFileAtomic(path string, data []byte) error {
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", base, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // harmless no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure %s: %w", base, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", base, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush %s: %w", base, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", base, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish %s: %w", base, err)
	}
	return nil
}
