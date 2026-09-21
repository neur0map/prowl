package workspace

import (
	"os"
	"sync"
	"testing"
)

// indexedRoot builds a workspace whose derived index file exists so that
// liveEntries keeps it.
func indexedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ws, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws.DB, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRegisterConcurrentDoesNotLose drives many simultaneous registrations of
// distinct live projects. The read-modify-write cycle in Register must be
// serialized, or an interleaved read/write pair drops a project that another
// goroutine registered.
func TestRegisterConcurrentDoesNotLose(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const n = 16
	roots := make([]string, n)
	for i := range roots {
		roots[i] = indexedRoot(t)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range roots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = Register(roots[i], true)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("registry has %d entries, want all %d concurrent registrations", len(list), n)
	}
	seen := make(map[string]bool, len(list))
	for _, e := range list {
		seen[e.Root] = true
	}
	for _, root := range roots {
		if !seen[root] {
			t.Fatalf("registration lost project %s", root)
		}
	}
}

// TestWriteRegistryPreservesMode confirms a freshly created registry is private
// and that an atomic rewrite keeps whatever permission bits the file already
// carries instead of forcing 0644.
func TestWriteRegistryPreservesMode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, err := registryPath()
	if err != nil {
		t.Fatal(err)
	}

	if err := Register(indexedRoot(t), true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new registry mode = %o, want 0600 (private default)", got)
	}

	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	// A second registration rewrites the file atomically; the mode must survive.
	if err := Register(indexedRoot(t), true); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("rewritten registry mode = %o, want 0640 preserved", got)
	}
}
