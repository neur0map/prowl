package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateResolve(t *testing.T) {
	root := t.TempDir()
	w, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf(".prowl not created: %v", err)
	}
	if w.Knowledge != filepath.Join(w.Path, "knowledge") || w.Proposals != filepath.Join(w.Path, "proposals") {
		t.Fatalf("canonical paths not exposed: %+v", w)
	}
	if w.Derived != w.Path || w.DB != filepath.Join(w.Path, "index.db") || w.Cache != filepath.Join(w.Path, "cache") || w.Logs != filepath.Join(w.Path, "logs") {
		t.Fatalf("primary derived paths changed: %+v", w)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	w2, err := Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if w2.Root != root {
		t.Fatalf("resolved root = %q, want %q", w2.Root, root)
	}
	if _, err := Resolve(t.TempDir()); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestResolveLinkedWorktreeUsesActiveRootSharedCanonicalAndIsolatedDerivedState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	primary := filepath.Join(t.TempDir(), "primary")
	if err := os.Mkdir(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{
			"-c", "user.name=Workspace Test",
			"-c", "user.email=workspace@example.com",
		}, args...)...)
		command.Dir = primary
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(primary, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--", "tracked.txt")
	runGit("commit", "-qm", "base")

	shared, err := Create(primary)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(primary, ".worktrees", "linked")
	runGit("worktree", "add", "-q", "-b", "linked", linked, "HEAD")
	linkedTwo := filepath.Join(primary, ".worktrees", "linked-two")
	runGit("worktree", "add", "-q", "-b", "linked-two", linkedTwo, "HEAD")
	start := filepath.Join(linked, "nested")
	if err := os.Mkdir(start, 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(start)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root != linked {
		t.Fatalf("resolved root = %q, want linked worktree %q", resolved.Root, linked)
	}
	if resolved.Path != shared.Path || resolved.Knowledge != shared.Knowledge || resolved.Proposals != shared.Proposals {
		t.Fatalf("resolved shared state = %+v, want canonical state %+v", resolved, shared)
	}
	firstDerived := filepath.Join(primary, ".git", "worktrees", "linked", "prowl")
	if resolved.Derived != firstDerived || resolved.DB != filepath.Join(firstDerived, "index.db") || resolved.Cache != filepath.Join(firstDerived, "cache") || resolved.Logs != filepath.Join(firstDerived, "logs") {
		t.Fatalf("first linked derived paths = %+v, want root %q", resolved, firstDerived)
	}
	startTwo := filepath.Join(linkedTwo, "nested")
	if err := os.Mkdir(startTwo, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedTwo, err := Resolve(startTwo)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedTwo.Root != linkedTwo {
		t.Fatalf("second resolved root = %q, want linked worktree %q", resolvedTwo.Root, linkedTwo)
	}
	if resolvedTwo.Path != shared.Path || resolvedTwo.Knowledge != shared.Knowledge || resolvedTwo.Proposals != shared.Proposals {
		t.Fatalf("second resolved shared state = %+v, want canonical state %+v", resolvedTwo, shared)
	}
	secondDerived := filepath.Join(primary, ".git", "worktrees", "linked-two", "prowl")
	if resolvedTwo.Derived != secondDerived || resolvedTwo.DB != filepath.Join(secondDerived, "index.db") || resolvedTwo.Cache != filepath.Join(secondDerived, "cache") || resolvedTwo.Logs != filepath.Join(secondDerived, "logs") {
		t.Fatalf("second linked derived paths = %+v, want root %q", resolvedTwo, secondDerived)
	}
	if resolved.DB == shared.DB || resolvedTwo.DB == shared.DB || resolved.DB == resolvedTwo.DB {
		t.Fatalf("linked DB paths are not isolated: primary=%q first=%q second=%q", shared.DB, resolved.DB, resolvedTwo.DB)
	}
	if resolved.Cache == shared.Cache || resolvedTwo.Cache == shared.Cache || resolved.Cache == resolvedTwo.Cache {
		t.Fatalf("linked cache paths are not isolated: primary=%q first=%q second=%q", shared.Cache, resolved.Cache, resolvedTwo.Cache)
	}
	if resolved.Logs == shared.Logs || resolvedTwo.Logs == shared.Logs || resolved.Logs == resolvedTwo.Logs {
		t.Fatalf("linked log paths are not isolated: primary=%q first=%q second=%q", shared.Logs, resolved.Logs, resolvedTwo.Logs)
	}

	local, err := Create(linked)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = Resolve(start)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Root != linked || resolved.Path != local.Path || resolved.Derived != local.Derived {
		t.Fatalf("resolved local state = %+v, want linked state %+v", resolved, local)
	}
}

func TestResolveRejectsForgedLinkedWorktreeBacklink(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	admin := filepath.Join(primary, ".git", "worktrees", "registered")
	linked := filepath.Join(primary, ".worktrees", "forged")
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
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherMarker := filepath.Join(primary, ".worktrees", "other", ".git")
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(otherMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if workspace, err := Resolve(linked); err != ErrNotFound || workspace != nil {
		t.Fatalf("forged backlink resolved workspace=%+v err=%v, want ErrNotFound", workspace, err)
	}
}

func TestResolveRejectsNestedLinkedAdminDirectory(t *testing.T) {
	primary := filepath.Join(t.TempDir(), "primary")
	admin := filepath.Join(primary, ".git", "worktrees", "group", "nested")
	linked := filepath.Join(primary, ".worktrees", "nested")
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
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(linked, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if workspace, err := Resolve(linked); err != ErrNotFound || workspace != nil {
		t.Fatalf("nested admin directory resolved workspace=%+v err=%v, want ErrNotFound", workspace, err)
	}
}

func TestResolveTreatsBareRepositoryAsBoundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	outer := t.TempDir()
	if _, err := Create(outer); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(outer, "repository.git")
	command := exec.Command("git", "init", "--bare", "-q", bare)
	command.Dir = outer
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}

	if workspace, err := Resolve(filepath.Join(bare, "objects")); err != ErrNotFound || workspace != nil {
		t.Fatalf("bare repository resolved workspace=%+v err=%v, want ErrNotFound", workspace, err)
	}
}

func TestResolveContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	workspace, err := ResolveContext(ctx, t.TempDir())
	if workspace != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("workspace=%+v error=%v want context canceled", workspace, err)
	}
}

func TestRegistry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Register("/x/y", true); err != nil {
		t.Fatal(err)
	}
	if err := Register("/x/y", false); err != nil { // upsert, not duplicate
		t.Fatal(err)
	}
	if err := Register("/a/b", true); err != nil {
		t.Fatal(err)
	}
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("entries = %d, want 2", len(list))
	}
	for _, e := range list {
		if e.Root == "/x/y" && e.AI {
			t.Fatal("ai flag should have been updated to false")
		}
	}
}

func TestEnsureIgnored(t *testing.T) {
	root := t.TempDir()
	if err := EnsureIgnored(root, ".prowl/"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureIgnored(root, ".prowl/"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if strings.Count(string(data), ".prowl/") != 1 {
		t.Fatalf("gitignore should contain .prowl/ once: %q", data)
	}
}
