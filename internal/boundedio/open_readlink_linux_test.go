//go:build linux

package boundedio

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReadlinkNoFollowDoesNotOpenTarget(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.Symlink("missing-target", filepath.Join(rootDir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := ReadlinkNoFollow(root, "link")
	if err != nil || got != "missing-target" {
		t.Fatalf("%q %v", got, err)
	}
}

// TestReadlinkNoFollowFinalSymlinkSwapRace hammers a final symlink that an
// attacker swaps between two distinct targets and a regular file. The
// descriptor-tied read (O_PATH|O_NOFOLLOW) must only ever return one of the
// legitimate symlink targets or a typed error; it must never return truncated
// garbage nor the regular file's content (readlink never opens the target).
func TestReadlinkNoFollowFinalSymlinkSwapRace(t *testing.T) {
	rootDir := t.TempDir()
	const targetA = "target-alpha"
	const targetB = "target-bravo-longer-value"
	link := filepath.Join(rootDir, "lnk")
	reg := filepath.Join(rootDir, "lnk.reg")
	if err := os.Symlink(targetA, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.WriteFile(reg, []byte("REGULAR-FILE-CONTENT"), 0o600); err != nil {
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
		toggle := false
		for !stop.Load() {
			_ = os.Remove(link)
			if toggle {
				_ = os.Symlink(targetB, link)
			} else {
				_ = os.Link(reg, link) // briefly a regular file
			}
			toggle = !toggle
			_ = os.Remove(link)
			_ = os.Symlink(targetA, link)
		}
	}()

	for range 20000 {
		got, err := ReadlinkNoFollow(root, "lnk")
		if err != nil {
			continue
		}
		if got != targetA && got != targetB {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("readlink returned unexpected value %q (not a legitimate target)", got)
		}
	}
	stop.Store(true)
	wg.Wait()
}
