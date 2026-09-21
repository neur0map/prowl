package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Entry records one initialized project in the global registry.
type Entry struct {
	Root      string `json:"root"`
	CreatedAt int64  `json:"created_at"`
	AI        bool   `json:"ai"`
}

// registryMu serializes registry read-modify-write cycles within this process;
// the flock beneath withRegistryLock serializes them across processes.
var registryMu sync.Mutex

// registryPath returns $XDG_STATE_HOME/prowl-agent/registry.json (or the
// ~/.local/state fallback).
func registryPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "prowl-agent", "registry.json"), nil
}

// withRegistryLock runs fn while holding an exclusive lock on the registry so
// that a concurrent listing or registration cannot lose a project. It reuses
// the gofrs/flock convention already used for the index-refresh lock rather
// than inventing a second cross-process mechanism.
func withRegistryLock(path string, fn func() error) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("workspace registry lock was not acquired")
	}
	return errors.Join(fn(), lock.Unlock())
}

// Register upserts a project (by absolute root) into the global registry.
func Register(root string, ai bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	p, err := registryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return withRegistryLock(p, func() error {
		entries, err := loadEntries(p)
		if err != nil {
			return err
		}
		entries, _ = liveEntries(entries)
		found := false
		for i := range entries {
			if entries[i].Root == abs {
				entries[i].AI = ai
				found = true
			}
		}
		if !found {
			entries = append(entries, Entry{Root: abs, CreatedAt: time.Now().Unix(), AI: ai})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Root < entries[j].Root })
		return writeRegistry(p, entries)
	})
}

// List returns registered projects whose indexes still exist. It also compacts
// stale and duplicate entries so temporary projects do not accumulate forever.
func List() ([]Entry, error) {
	p, err := registryPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var live []Entry
	err = withRegistryLock(p, func() error {
		entries, err := loadEntries(p)
		if err != nil {
			return err
		}
		var changed bool
		live, changed = liveEntries(entries)
		if changed {
			return writeRegistry(p, live)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return live, nil
}

// loadEntries reads and decodes the raw registry file. A missing file is not an
// error; it yields no entries.
func loadEntries(path string) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func liveEntries(entries []Entry) ([]Entry, bool) {
	byRoot := make(map[string]Entry, len(entries))
	changed := false
	for _, entry := range entries {
		root := filepath.Clean(entry.Root)
		if root == "." || !filepath.IsAbs(root) {
			changed = true
			continue
		}
		if root != entry.Root {
			entry.Root = root
			changed = true
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			changed = true
			continue
		}
		ws, err := Resolve(root)
		if err != nil || filepath.Clean(ws.Root) != root {
			changed = true
			continue
		}
		if info, err := os.Stat(ws.DB); err != nil || info.IsDir() {
			changed = true
			continue
		}
		if previous, ok := byRoot[root]; ok {
			changed = true
			if previous.CreatedAt >= entry.CreatedAt {
				continue
			}
		}
		byRoot[root] = entry
	}
	live := make([]Entry, 0, len(byRoot))
	for _, entry := range byRoot {
		live = append(live, entry)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Root < live[j].Root })
	return live, changed || len(live) != len(entries)
}

func writeRegistry(path string, entries []Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Preserve the existing file's permission bits across the atomic rewrite;
	// fall back to a private default for a file we are creating.
	perm := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	file, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
