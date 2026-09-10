//go:build unix

package review

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- fixtures -------------------------------------------------------------

// gitFixture is a real on-disk repository plus the sanitized runner scope
// resolution consumes. Its helpers build the exact commit graphs the scope
// contract distinguishes: ordinary/root/merge commits and merge-base topologies.
type gitFixture struct {
	root   string
	runner GitRunner
}

func fixtureRunner(t *testing.T) GitRunner {
	t.Helper()
	return ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	gitBin(t)
	root := t.TempDir()
	rawGit(t, root, "init", "-q")
	return &gitFixture{root: root, runner: fixtureRunner(t)}
}

// newSHA256Fixture builds a SHA-256 repository, skipping when the installed Git
// cannot honour --object-format=sha256.
func newSHA256Fixture(t *testing.T) *gitFixture {
	t.Helper()
	gitBin(t)
	root := t.TempDir()
	if out, err := rawGitEnv(root, nil, "init", "-q", "--object-format=sha256"); err != nil {
		t.Skipf("git lacks --object-format=sha256: %v\n%s", err, out)
	}
	return &gitFixture{root: root, runner: fixtureRunner(t)}
}

func (f *gitFixture) writeCommit(t *testing.T, name, body, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, f.root, "add", name)
	rawGit(t, f.root, "commit", "-qm", msg)
	return f.revParse(t, "HEAD")
}

func (f *gitFixture) revParse(t *testing.T, rev string) string {
	t.Helper()
	out, err := rawGitEnv(f.root, nil, "rev-parse", "--verify", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v\n%s", rev, err, out)
	}
	return string(bytes.TrimSpace(out))
}

// divergedBranches builds a common ancestor and two branches that each add a
// commit on top of it, yielding a single best merge base at the ancestor.
func (f *gitFixture) divergedBranches(t *testing.T) (base, head string, wantMergeBase []byte) {
	t.Helper()
	f.writeCommit(t, "seed.txt", "s\n", "seed")
	ancestor := f.revParse(t, "HEAD")
	rawGit(t, f.root, "checkout", "-qb", "left", ancestor)
	left := f.writeCommit(t, "a.txt", "a\n", "left work")
	rawGit(t, f.root, "checkout", "-qb", "right", ancestor)
	right := f.writeCommit(t, "b.txt", "b\n", "right work")
	return left, right, mustHex(t, ancestor)
}

// crissCross builds a criss-cross history whose two tips share two best merge
// bases, the ambiguous topology scope resolution must reject.
func (f *gitFixture) crissCross(t *testing.T) (left, right string) {
	t.Helper()
	f.writeCommit(t, "seed.txt", "s\n", "seed")
	ancestor := f.revParse(t, "HEAD")
	rawGit(t, f.root, "checkout", "-qb", "cc-a", ancestor)
	a := f.writeCommit(t, "a.txt", "a\n", "A")
	rawGit(t, f.root, "checkout", "-qb", "cc-b", ancestor)
	b := f.writeCommit(t, "b.txt", "b\n", "B")
	// cc-b (at B) merges A -> M1 with parents {B, A}.
	rawGit(t, f.root, "merge", "-q", "--no-ff", "--no-edit", a)
	right = f.revParse(t, "HEAD")
	// cc-a (at A) merges B -> M2 with parents {A, B}; merge-base(M1, M2) = {A, B}.
	rawGit(t, f.root, "checkout", "-q", "cc-a")
	rawGit(t, f.root, "merge", "-q", "--no-ff", "--no-edit", b)
	left = f.revParse(t, "HEAD")
	return left, right
}

// unrelatedRoots builds two root commits with no shared history, so they have
// zero merge bases.
func (f *gitFixture) unrelatedRoots(t *testing.T) (a, b string) {
	t.Helper()
	first := f.writeCommit(t, "a.txt", "a\n", "root a")
	rawGit(t, f.root, "checkout", "-q", "--orphan", "orphan")
	second := f.writeCommit(t, "b.txt", "b\n", "root b")
	return first, second
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return raw
}

// scriptRunner is a GitRunner test double whose Output is scripted, letting a
// test drive object-format handling without a real repository. Any Pipe call is
// unexpected in scope resolution and fails the test.
type scriptRunner struct {
	t      *testing.T
	output func(args []string) ([]byte, error)
}

func (r scriptRunner) Output(_ context.Context, _ string, _ int64, args ...string) ([]byte, error) {
	return r.output(args)
}

func (r scriptRunner) Pipe(_ context.Context, _ string, _ int64, _ io.Reader, _ io.Writer, args ...string) error {
	r.t.Fatalf("unexpected Pipe(%v)", args)
	return nil
}

// refusingRunner fails the test if Git is invoked at all, proving a rejection
// happened before any subprocess ran.
type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Output(_ context.Context, _ string, _ int64, args ...string) ([]byte, error) {
	r.t.Fatalf("unexpected Output(%v): git must not run", args)
	return nil, nil
}

func (r refusingRunner) Pipe(_ context.Context, _ string, _ int64, _ io.Reader, _ io.Writer, args ...string) error {
	r.t.Fatalf("unexpected Pipe(%v): git must not run", args)
	return nil
}

// ---- tests ----------------------------------------------------------------

func TestResolveWorkspaceUsesHeadAndProvisionalHead(t *testing.T) {
	repo := newGitFixture(t)
	head := repo.writeCommit(t, "payload.txt", "l1\nl2\n", "seed")

	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeWorkspace {
		t.Fatalf("kind=%q, want workspace", got.Kind)
	}
	if got.ObjectFormat != "sha1" {
		t.Fatalf("object_format=%q, want sha1", got.ObjectFormat)
	}
	if got.Base.Kind != SideGitOID || !bytes.Equal(got.Base.Value, mustHex(t, head)) {
		t.Fatalf("base=%+v, want git_oid %s", got.Base, head)
	}
	if got.Head.Kind != SideWorkspaceSHA256 || len(got.Head.Value) != 32 {
		t.Fatalf("head=%+v, want 32-byte workspace_sha256", got.Head)
	}
	if !bytes.Equal(got.Head.Value, make([]byte, 32)) {
		t.Fatalf("provisional workspace head must be zero-filled, got %x", got.Head.Value)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("resolved scope invalid: %v", err)
	}
}

func TestResolveCommitAgainstFirstParent(t *testing.T) {
	repo := newGitFixture(t)
	parent := repo.writeCommit(t, "a.txt", "1\n", "first")
	child := repo.writeCommit(t, "a.txt", "2\n", "second")

	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Commit: child})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeCommit {
		t.Fatalf("kind=%q, want commit", got.Kind)
	}
	if !bytes.Equal(got.Base.Value, mustHex(t, parent)) {
		t.Fatalf("base=%x, want parent %s", got.Base.Value, parent)
	}
	if !bytes.Equal(got.Head.Value, mustHex(t, child)) {
		t.Fatalf("head=%x, want child %s", got.Head.Value, child)
	}
}

func TestResolveRootCommitUsesEmptyTree(t *testing.T) {
	repo := newGitFixture(t)
	root := repo.writeCommit(t, "a.txt", "1\n", "root")

	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Commit: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeCommit {
		t.Fatalf("kind=%q, want commit", got.Kind)
	}
	if !bytes.Equal(got.Base.Value, mustHex(t, emptyTreeSHA1)) {
		t.Fatalf("base=%x, want empty tree %s", got.Base.Value, emptyTreeSHA1)
	}
	if !bytes.Equal(got.Head.Value, mustHex(t, root)) {
		t.Fatalf("head=%x, want root %s", got.Head.Value, root)
	}
}

func TestResolveRejectsMergeCommit(t *testing.T) {
	repo := newGitFixture(t)
	repo.writeCommit(t, "seed.txt", "s\n", "seed")
	ancestor := repo.revParse(t, "HEAD")
	rawGit(t, repo.root, "checkout", "-qb", "topic", ancestor)
	side := repo.writeCommit(t, "topic.txt", "t\n", "topic")
	rawGit(t, repo.root, "checkout", "-q", "-")
	repo.writeCommit(t, "main.txt", "m\n", "mainline")
	rawGit(t, repo.root, "merge", "-q", "--no-ff", "--no-edit", side)
	mergeOID := repo.revParse(t, "HEAD")

	_, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Commit: mergeOID})
	if !errors.Is(err, ErrMergeCommitScope) {
		t.Fatalf("err=%v, want ErrMergeCommitScope", err)
	}
}

func TestResolveRangeUsesUniqueMergeBase(t *testing.T) {
	repo := newGitFixture(t)
	base, head, wantMergeBase := repo.divergedBranches(t)

	got, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Base: base, Head: head})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeRange || !bytes.Equal(got.Base.Value, wantMergeBase) {
		t.Fatalf("scope=%+v", got)
	}
	if !bytes.Equal(got.Head.Value, mustHex(t, repo.revParse(t, head))) {
		t.Fatalf("head=%x, want %s", got.Head.Value, head)
	}
}

func TestResolveRangeDefaultsHeadToHEAD(t *testing.T) {
	repo := newGitFixture(t)
	repo.writeCommit(t, "seed.txt", "s\n", "seed")
	ancestor := repo.revParse(t, "HEAD")
	rawGit(t, repo.root, "checkout", "-qb", "feature", ancestor)
	repo.writeCommit(t, "f.txt", "f\n", "feature")
	head := repo.revParse(t, "HEAD")

	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Base: ancestor})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeRange {
		t.Fatalf("kind=%q, want range", got.Kind)
	}
	if !bytes.Equal(got.Base.Value, mustHex(t, ancestor)) {
		t.Fatalf("base=%x, want ancestor %s", got.Base.Value, ancestor)
	}
	if !bytes.Equal(got.Head.Value, mustHex(t, head)) {
		t.Fatalf("head=%x, want HEAD %s", got.Head.Value, head)
	}
}

func TestResolveRejectsMultipleMergeBases(t *testing.T) {
	repo := newGitFixture(t)
	left, right := repo.crissCross(t)

	_, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Base: left, Head: right})
	if !errors.Is(err, ErrMultipleMergeBases) {
		t.Fatalf("err=%v, want ErrMultipleMergeBases", err)
	}
}

func TestResolveRejectsNoMergeBase(t *testing.T) {
	repo := newGitFixture(t)
	a, b := repo.unrelatedRoots(t)

	_, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Base: a, Head: b})
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("err=%v, want ErrNoMergeBase", err)
	}
}

func TestResolveRejectsOptionLikeRefs(t *testing.T) {
	for _, req := range []PlanRequest{
		{Commit: "--output=/tmp/x"},
		{Base: "-base"},
		{Base: "ok", Head: "--upload-pack=evil"},
	} {
		_, err := ResolveScope(context.Background(), refusingRunner{t}, "", req)
		if err == nil {
			t.Fatalf("req=%+v accepted an option-like ref", req)
		}
	}
}

func TestResolveRejectsInvalidFlagCombinations(t *testing.T) {
	for _, req := range []PlanRequest{
		{Commit: "abc", Base: "def"},
		{Commit: "abc", Head: "def"},
		{Head: "def"},
	} {
		_, err := ResolveScope(context.Background(), refusingRunner{t}, "", req)
		if err == nil {
			t.Fatalf("req=%+v accepted an invalid flag combination", req)
		}
	}
}

func TestResolveRejectsMissingRef(t *testing.T) {
	repo := newGitFixture(t)
	repo.writeCommit(t, "a.txt", "1\n", "seed")

	if _, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Commit: "does-not-exist"}); err == nil {
		t.Fatal("missing commit ref was accepted")
	}
	if _, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Base: "nope"}); err == nil {
		t.Fatal("missing base ref was accepted")
	}
}

func TestResolveRejectsNonCommitRef(t *testing.T) {
	repo := newGitFixture(t)
	repo.writeCommit(t, "a.txt", "1\n", "seed")
	blob := repo.revParse(t, "HEAD:a.txt")

	if _, err := ResolveScope(context.Background(), repo.runner, repo.root,
		PlanRequest{Commit: blob}); err == nil {
		t.Fatalf("blob %s was accepted as a commit", blob)
	}
}

func TestResolveDetachedHead(t *testing.T) {
	repo := newGitFixture(t)
	first := repo.writeCommit(t, "a.txt", "1\n", "first")
	repo.writeCommit(t, "a.txt", "2\n", "second")
	rawGit(t, repo.root, "checkout", "-q", first) // detached HEAD at the first commit

	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != ScopeWorkspace {
		t.Fatalf("kind=%q, want workspace", got.Kind)
	}
	if !bytes.Equal(got.Base.Value, mustHex(t, first)) {
		t.Fatalf("base=%x, want detached head %s", got.Base.Value, first)
	}
}

func TestResolveSHA256Repository(t *testing.T) {
	repo := newSHA256Fixture(t)
	root := repo.writeCommit(t, "a.txt", "1\n", "root")
	child := repo.writeCommit(t, "a.txt", "2\n", "child")

	// Ordinary commit: 32-byte OIDs against the first parent.
	got, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Commit: child})
	if err != nil {
		t.Fatal(err)
	}
	if got.ObjectFormat != "sha256" {
		t.Fatalf("object_format=%q, want sha256", got.ObjectFormat)
	}
	if len(got.Base.Value) != 32 || len(got.Head.Value) != 32 {
		t.Fatalf("sha256 OIDs must be 32 bytes: base=%d head=%d", len(got.Base.Value), len(got.Head.Value))
	}
	if !bytes.Equal(got.Base.Value, mustHex(t, root)) {
		t.Fatalf("base=%x, want parent %s", got.Base.Value, root)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("resolved sha256 scope invalid: %v", err)
	}

	// Root commit: SHA-256 empty tree.
	rootScope, err := ResolveScope(context.Background(), repo.runner, repo.root, PlanRequest{Commit: root})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootScope.Base.Value, mustHex(t, emptyTreeSHA256)) {
		t.Fatalf("base=%x, want sha256 empty tree %s", rootScope.Base.Value, emptyTreeSHA256)
	}
}

func TestResolveRejectsUnsupportedObjectFormat(t *testing.T) {
	runner := scriptRunner{t: t, output: func(args []string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-object-format" {
			return []byte("sha512\n"), nil
		}
		t.Fatalf("unexpected git call after bad object format: %v", args)
		return nil, nil
	}}

	_, err := ResolveScope(context.Background(), runner, "/tmp", PlanRequest{})
	if !errors.Is(err, ErrUnsupportedObjectFormat) {
		t.Fatalf("err=%v, want ErrUnsupportedObjectFormat", err)
	}
}

// rangeMergeBaseRunner scripts a range resolution: it answers the object-format
// and both ref verifications with valid values, then delegates the merge-base
// step to mergeBase, letting a test inject a specific merge-base outcome.
func rangeMergeBaseRunner(t *testing.T, mergeBase func() ([]byte, error)) scriptRunner {
	t.Helper()
	const baseOID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const headOID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	return scriptRunner{t: t, output: func(args []string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-object-format":
			return []byte("sha1\n"), nil
		case len(args) >= 1 && args[0] == "rev-parse":
			if strings.Contains(args[len(args)-1], "head") {
				return []byte(headOID + "\n"), nil
			}
			return []byte(baseOID + "\n"), nil
		case len(args) >= 1 && args[0] == "merge-base":
			return mergeBase()
		}
		t.Fatalf("unexpected git call: %v", args)
		return nil, nil
	}}
}

func TestResolveRangeCancellationNotMislabeled(t *testing.T) {
	runner := rangeMergeBaseRunner(t, func() ([]byte, error) { return nil, context.Canceled })

	_, err := ResolveScope(context.Background(), runner, "", PlanRequest{Base: "baseref", Head: "headref"})
	if errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("cancellation mislabeled as ErrNoMergeBase: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled preserved in the chain", err)
	}
}

func TestResolveRangeNon1MergeBaseFailurePreserved(t *testing.T) {
	want := &GitExitError{Subcommand: "merge-base", Code: 128, Stderr: "fatal: bad revision"}
	runner := rangeMergeBaseRunner(t, func() ([]byte, error) { return nil, want })

	_, err := ResolveScope(context.Background(), runner, "", PlanRequest{Base: "baseref", Head: "headref"})
	if errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("non-1 merge-base exit mislabeled as ErrNoMergeBase: %v", err)
	}
	var exit *GitExitError
	if !errors.As(err, &exit) || exit.Code != 128 {
		t.Fatalf("err=%v, want *GitExitError code 128 preserved in the chain", err)
	}
}

func TestResolveRangeExit1MapsToNoMergeBase(t *testing.T) {
	exitErr := &GitExitError{Subcommand: "merge-base", Code: 1, Stderr: ""}
	runner := rangeMergeBaseRunner(t, func() ([]byte, error) { return nil, exitErr })

	_, err := ResolveScope(context.Background(), runner, "", PlanRequest{Base: "baseref", Head: "headref"})
	if !errors.Is(err, ErrNoMergeBase) {
		t.Fatalf("exit status 1 must map to ErrNoMergeBase: %v", err)
	}
	var exit *GitExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("exit-1 chain not preserved: %v", err)
	}
}
