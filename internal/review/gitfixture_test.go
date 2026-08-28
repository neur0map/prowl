//go:build unix

package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file adds the capture-oriented helpers on top of the gitFixture defined
// in scope_test.go: worktree writes, commits, workspace captures, raw plumbing
// for hostile committed blobs, and record lookups. It never redeclares the
// shared fixture, its constructor, or the raw Git helpers from git_test.go.

// execRunner is the sanitized process-backed CaptureRunner the capture tests
// drive. A generous timeout keeps large-fixture diffs from flaking.
func execRunner(t *testing.T) ExecGit {
	t.Helper()
	return ExecGit{HooksDir: t.TempDir(), Timeout: 60 * time.Second, MaxStderr: 1 << 20}
}

// write writes body to name in the worktree without staging it, creating any
// missing parent directories.
func (f *gitFixture) write(t *testing.T, name, body string) {
	t.Helper()
	full := filepath.Join(f.root, name)
	if dir := filepath.Dir(full); dir != f.root {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// commitFile writes name with body, stages it, and commits it.
func (f *gitFixture) commitFile(t *testing.T, name, body string) {
	t.Helper()
	f.write(t, name, body)
	rawGit(t, f.root, "add", "--", name)
	rawGit(t, f.root, "commit", "-qm", "commit "+name)
}

// commit stages every worktree change and commits it.
func (f *gitFixture) commit(t *testing.T, msg string) {
	t.Helper()
	rawGit(t, f.root, "add", "-A")
	rawGit(t, f.root, "commit", "-qm", msg)
}

// commitBlobEntry commits a raw blob at path with the given git file mode,
// bypassing the worktree. It is the only way to plant a hostile committed blob
// such as a symlink whose target bytes contain NUL, which no filesystem symlink
// could hold.
func (f *gitFixture) commitBlobEntry(t *testing.T, mode, path, content string) {
	t.Helper()
	oid := f.hashObject(t, content)
	rawGit(t, f.root, "update-index", "--add", "--cacheinfo", mode+","+oid+","+path)
	rawGit(t, f.root, "commit", "-qm", "plant "+path)
}

// hashObject writes content as a loose blob and returns its object id.
func (f *gitFixture) hashObject(t *testing.T, content string) string {
	t.Helper()
	cmd := gitCommand(f.root, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// gitCommand builds a raw git *exec.Cmd rooted at root with the deterministic
// author/committer identity, for callers that must set stdin or read clean
// stdout (e.g. hash-object).
func gitCommand(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	return cmd
}

// runCapture resolves the workspace scope and runs a single capture, returning
// any error so fail-closed tests can assert on it.
func runCapture(t *testing.T, f *gitFixture, c *Capturer) (Capture, error) {
	t.Helper()
	ctx := context.Background()
	runner := execRunner(t)
	scope, err := ResolveScope(ctx, runner, f.root, PlanRequest{})
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	c.Root = f.root
	c.Runner = runner
	return c.CaptureOnce(ctx, scope)
}

// captureWorkspace runs a single default capture and fails on any error.
func captureWorkspace(t *testing.T, f *gitFixture) Capture {
	t.Helper()
	cap, err := runCapture(t, f, &Capturer{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return cap
}

// captureWorkspaceWith runs a single capture with a caller-configured Capturer
// and fails on any error.
func captureWorkspaceWith(t *testing.T, f *gitFixture, c *Capturer) Capture {
	t.Helper()
	cap, err := runCapture(t, f, c)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return cap
}

// recordByNewPath returns the changed-path record whose new path is p.
func recordByNewPath(t *testing.T, cap Capture, p string) RawPathRecord {
	t.Helper()
	for _, r := range cap.Paths {
		if r.NewPath == p {
			return r
		}
	}
	t.Fatalf("no record with new path %q; have %v", p, capturePaths(cap))
	return RawPathRecord{}
}

// recordByOldPath returns the changed-path record whose old path is p.
func recordByOldPath(t *testing.T, cap Capture, p string) RawPathRecord {
	t.Helper()
	for _, r := range cap.Paths {
		if r.OldPath == p {
			return r
		}
	}
	t.Fatalf("no record with old path %q; have %v", p, capturePaths(cap))
	return RawPathRecord{}
}

// capturePaths renders the captured old/new/status triples for failure output.
func capturePaths(cap Capture) []string {
	var out []string
	for _, r := range cap.Paths {
		out = append(out, r.Status+" "+r.OldPath+"->"+r.NewPath)
	}
	return out
}
