//go:build unix

package workspace

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestResolveRejectsFIFOCommondirWithoutBlocking(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	admin := filepath.Join(primary, ".git", "worktrees", "linked")
	linked := filepath.Join(primary, ".worktrees", "linked")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(primary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(admin, "commondir"), 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		workspace *Workspace
		err       error
	}
	resolved := make(chan result, 1)
	go func() {
		workspace, err := Resolve(linked)
		resolved <- result{workspace: workspace, err: err}
	}()
	select {
	case result := <-resolved:
		if result.err != ErrNotFound || result.workspace != nil {
			t.Fatalf("FIFO metadata resolved workspace=%+v err=%v, want ErrNotFound", result.workspace, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Resolve blocked opening FIFO metadata")
	}
}

func TestResolveRejectsSymlinkedCommondir(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	admin := filepath.Join(primary, ".git", "worktrees", "linked")
	linked := filepath.Join(primary, ".worktrees", "linked")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(primary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "real-commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-commondir", filepath.Join(admin, "commondir")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(linked, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if workspace, err := Resolve(linked); err != ErrNotFound || workspace != nil {
		t.Fatalf("symlinked commondir resolved workspace=%+v err=%v, want ErrNotFound", workspace, err)
	}
}

func TestResolveTreatsUnsupportedGitMarkerAsBoundary(t *testing.T) {
	outer := t.TempDir()
	if _, err := Create(outer); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(outer, "repository")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repository, ".git"), 0o600); err != nil {
		t.Fatal(err)
	}

	if workspace, err := Resolve(repository); err != ErrNotFound || workspace != nil {
		t.Fatalf("unsupported .git marker resolved workspace=%+v err=%v, want ErrNotFound", workspace, err)
	}
}
