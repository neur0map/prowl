package boundedio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOpenRegularNoFollowRejectsEverySymlinkComponent(t *testing.T) {
	rootDir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootDir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := OpenRegularNoFollow(root, "link/secret"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("err=%v, want ErrSymlink", err)
	}
}

func TestOpenRegularNoFollowReadsRegular(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("legitimate content")
	if err := os.WriteFile(filepath.Join(rootDir, "sub", "file"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := OpenRegularNoFollow(root, "sub/file")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close()
	got := make([]byte, len(want)+1)
	n, _ := file.Read(got)
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("read %q, want %q", got[:n], want)
	}
}

func TestOpenRegularNoFollowRejectsFinalSymlink(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "real"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(rootDir, "alias")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := OpenRegularNoFollow(root, "alias"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("err=%v, want ErrSymlink", err)
	}
}

func TestOpenRegularNoFollowRejectsNonRegular(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := OpenRegularNoFollow(root, "adir"); !errors.Is(err, ErrNonRegular) {
		t.Fatalf("err=%v, want ErrNonRegular", err)
	}
}

func TestOpenRegularNoFollowRejectsTraversal(t *testing.T) {
	rootDir := t.TempDir()
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../escape", "/etc/passwd", "sub/../../escape", ""} {
		if _, err := OpenRegularNoFollow(root, name); err == nil {
			t.Fatalf("name %q: expected rejection", name)
		}
	}
}

// TestOpenRegularNoFollowIntermediateSwapRace hammers a directory component that
// an attacker repeatedly swaps between a legitimate directory and a symlink to
// an outside directory holding a decoy secret. The verified no-follow walk must
// never resolve the outside secret: a successful open only ever yields the
// legitimate bytes; any concurrent swap surfaces as a typed rejection.
func TestOpenRegularNoFollowIntermediateSwapRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("swap race exercises POSIX rename semantics")
	}
	rootDir := t.TempDir()
	outside := t.TempDir()
	const secret = "SECRET-OUTSIDE-THE-ROOT"
	const legit = "legit-inside"
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(rootDir, "a")
	stashed := filepath.Join(rootDir, "a.real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "file"), []byte(legit), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			// a -> a.real ; a := symlink(outside) ; drop symlink ; a.real -> a
			if os.Rename(realDir, stashed) != nil {
				continue
			}
			_ = os.Symlink(outside, realDir)
			_ = os.Remove(realDir)
			_ = os.Rename(stashed, realDir)
		}
	}()

	for i := range 20000 {
		file, err := OpenRegularNoFollow(root, "a/file")
		if err != nil {
			continue
		}
		buf := make([]byte, len(secret)+len(legit))
		n, _ := file.Read(buf)
		file.Close()
		if bytes.Contains(buf[:n], []byte(secret)) {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("no-follow walk leaked outside secret on iteration %d", i)
		}
	}
	stop.Store(true)
	wg.Wait()
	// Restore a clean state so t.TempDir cleanup does not trip over the stash.
	if _, statErr := os.Lstat(realDir); statErr != nil {
		_ = os.Rename(stashed, realDir)
	}
}

// TestOpenRegularNoFollowBackslashIsOrdinaryFilename proves a backslash is an
// ordinary filename byte on Unix, not a path separator: a file literally named
// "a\b" must be reached by the single component "a\b" and must be distinct from
// the nested path "a/b".
func TestOpenRegularNoFollowBackslashIsOrdinaryFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a separator on Windows")
	}
	rootDir := t.TempDir()
	const litName = "a\\b" // one component whose name contains a backslash
	if err := os.WriteFile(filepath.Join(rootDir, litName), []byte("literal-backslash"), 0o600); err != nil {
		t.Skipf("cannot create backslash filename: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "a", "b"), []byte("nested-slash"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	read := func(name string) string {
		t.Helper()
		f, err := OpenRegularNoFollow(root, name)
		if err != nil {
			t.Fatalf("open %q: %v", name, err)
		}
		defer f.Close()
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		return string(buf[:n])
	}
	if got := read("a\\b"); got != "literal-backslash" {
		t.Fatalf(`OpenRegularNoFollow("a\\b") read %q, want the literal-backslash file`, got)
	}
	if got := read("a/b"); got != "nested-slash" {
		t.Fatalf(`OpenRegularNoFollow("a/b") read %q, want the nested file`, got)
	}
}
