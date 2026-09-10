//go:build unix && !linux

package boundedio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReadlinkNoFollowUnsupported asserts the ruling for non-Linux Unix: without
// a no-follow symlink descriptor read, ReadlinkNoFollow fails closed with
// ErrUnsupported rather than returning an ABA-vulnerable name-bracketed target.
func TestReadlinkNoFollowUnsupported(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.Symlink("some-target", filepath.Join(rootDir, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := ReadlinkNoFollow(root, "link"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v, want ErrUnsupported", err)
	}
}
