//go:build unix

package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/config"
	"github.com/prowl-agent/prowl-agent/internal/index"
	"github.com/prowl-agent/prowl-agent/internal/store"
)

// fakeTreeRunner is a GitRunner whose ls-tree/cat-file responses are scripted,
// so a crafted hostile stream Git would never emit can be fed to the writer.
type fakeTreeRunner struct {
	onOutput func(args []string) ([]byte, error)
	onPipe   func(stdin io.Reader, args []string) ([]byte, error)
}

func (f fakeTreeRunner) Output(_ context.Context, _ string, _ int64, args ...string) ([]byte, error) {
	if f.onOutput == nil {
		return nil, fmt.Errorf("unexpected Output %v", args)
	}
	return f.onOutput(args)
}

func (f fakeTreeRunner) Pipe(_ context.Context, _ string, _ int64, stdin io.Reader, stdout io.Writer, args ...string) error {
	if f.onPipe == nil {
		return fmt.Errorf("unexpected Pipe %v", args)
	}
	out, err := f.onPipe(stdin, args)
	if err != nil {
		return err
	}
	_, werr := stdout.Write(out)
	return werr
}

// fortyHex is a syntactically valid full SHA-1 object id for crafted streams.
const fortyHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// treeRecord frames one ls-tree -z record.
func treeRecord(mode, typ, oid, path string) string {
	return mode + " " + typ + " " + oid + "\t" + path + "\x00"
}

// TestMaterializeParseTreeRejectsHostileStreams proves the writer's validation
// refuses every crafted stream Git normally refuses to create, with no writes.
func TestMaterializeParseTreeRejectsHostileStreams(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		max    int
		want   error
	}{
		{"absolute path", treeRecord("100644", "blob", fortyHex, "/etc/passwd"), 10, ErrSnapshotUnsafeEntry},
		{"parent traversal", treeRecord("100644", "blob", fortyHex, "../escape"), 10, ErrSnapshotUnsafeEntry},
		{"dot component", treeRecord("100644", "blob", fortyHex, "a/./b"), 10, ErrSnapshotUnsafeEntry},
		{"duplicate entry", treeRecord("100644", "blob", fortyHex, "a") + treeRecord("100644", "blob", fortyHex, "a"), 10, ErrSnapshotUnsafeEntry},
		{"parent file conflict", treeRecord("100644", "blob", fortyHex, "a") + treeRecord("100644", "blob", fortyHex, "a/b"), 10, ErrSnapshotUnsafeEntry},
		{"unsupported mode", treeRecord("120001", "blob", fortyHex, "x"), 10, ErrSnapshotUnsafeEntry},
		{"short object id", treeRecord("100644", "blob", "abcd", "x"), 10, ErrSnapshotUnsafeEntry},
		{"tree short object id", treeRecord("040000", "tree", "abcd", "d"), 10, ErrSnapshotUnsafeEntry},
		{"malformed record", "100644 blob " + fortyHex + "no-tab\x00", 10, ErrSnapshotMalformedStream},
		{"missing terminal NUL", "100644 blob " + fortyHex + "\tx", 10, ErrSnapshotMalformedStream},
		{"empty record", treeRecord("100644", "blob", fortyHex, "a") + "\x00", 10, ErrSnapshotMalformedStream},
		{"too many entries", treeRecord("100644", "blob", fortyHex, "a") + treeRecord("100644", "blob", fortyHex, "b"), 1, ErrSnapshotTooManyEntries},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTreeEntries([]byte(tc.stream), 20, tc.max)
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseTreeEntries err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestMaterializeHostileStreamWritesNothingOutsideRoot proves an absolute-path
// entry fed through OpenHeadView fails without creating the escaped file.
func TestMaterializeHostileStreamWritesNothingOutsideRoot(t *testing.T) {
	sentinelDir := t.TempDir()
	escaped := filepath.Join(sentinelDir, "PWNED")
	stream := treeRecord("100644", "blob", fortyHex, escaped)

	runner := fakeTreeRunner{
		onPipe: func(_ io.Reader, args []string) ([]byte, error) {
			if len(args) > 0 && args[0] == "ls-tree" {
				return []byte(stream), nil
			}
			return nil, fmt.Errorf("unexpected Pipe %v", args)
		},
	}
	_, err := OpenHeadView(context.Background(), HeadViewOptions{
		Runner:         runner,
		RepoRoot:       t.TempDir(),
		ObjectFormat:   "sha1",
		HeadTreeish:    fortyHex,
		SnapshotParent: t.TempDir(),
	})
	if !errors.Is(err, ErrSnapshotUnsafeEntry) {
		t.Fatalf("OpenHeadView err=%v, want ErrSnapshotUnsafeEntry", err)
	}
	if _, statErr := os.Stat(escaped); statErr == nil {
		t.Fatalf("hostile absolute path wrote outside the private root: %s", escaped)
	}
}

// TestHeadViewMaterializesCommittedTree materializes a real committed head,
// proves the private index answers a query, and resolves head and unchanged
// base source paths (including one outside the diff).
func TestHeadViewMaterializesCommittedTree(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "pkg/foo.go", "package pkg\n\nfunc Foo() int { return 1 }\n")
	f.write(t, "pkg/keep.go", "package pkg\n\nfunc Keep() int { return 9 }\n")
	f.commit(t, "c1")
	base := f.revParse(t, "HEAD")
	f.write(t, "pkg/foo.go", "package pkg\n\nfunc FooV2() int { return 2 }\n")
	f.commit(t, "c2")
	head := f.revParse(t, "HEAD")

	hv := openMaterialized(t, f, "sha1", base, head)
	defer hv.Close()

	if hits, err := hv.Query.FindSymbol("FooV2"); err != nil || len(hits) == 0 {
		t.Fatalf("FindSymbol(FooV2)=%v err=%v; want a hit from the private index", hits, err)
	}
	if hits, err := hv.Query.FindSymbol("Keep"); err != nil || len(hits) == 0 {
		t.Fatalf("FindSymbol(Keep)=%v err=%v; want a hit", hits, err)
	}

	ctx := context.Background()
	headFoo, err := hv.Sources.Read(ctx, SideHead, "pkg/foo.go", 1<<20)
	if err != nil || !strings.Contains(string(headFoo.Bytes), "FooV2") {
		t.Fatalf("head foo.go=%q err=%v; want head content", headFoo.Bytes, err)
	}
	if headFoo.Side != SideHead || headFoo.TextClass != TextClassText {
		t.Fatalf("head foo.go side=%q class=%q", headFoo.Side, headFoo.TextClass)
	}
	baseFoo, err := hv.Sources.Read(ctx, SideBase, "pkg/foo.go", 1<<20)
	if err != nil || !strings.Contains(string(baseFoo.Bytes), "func Foo(") {
		t.Fatalf("base foo.go=%q err=%v; want base content", baseFoo.Bytes, err)
	}
	// A path unchanged between base and head (outside the diff) still resolves.
	baseKeep, err := hv.Sources.Read(ctx, SideBase, "pkg/keep.go", 1<<20)
	if err != nil || !strings.Contains(string(baseKeep.Bytes), "func Keep(") {
		t.Fatalf("outside-diff base keep.go=%q err=%v", baseKeep.Bytes, err)
	}
	// An absent path resolves to a non-present entry, not an error.
	missing, err := hv.Sources.Read(ctx, SideHead, "pkg/nope.go", 1<<20)
	if err != nil || missing.Present {
		t.Fatalf("absent path present=%v err=%v; want absent", missing.Present, err)
	}
}

// TestHeadViewMaterializesSHA256Tree proves object-format handling for a
// SHA-256 repository.
func TestHeadViewMaterializesSHA256Tree(t *testing.T) {
	f := newSHA256Fixture(t)
	f.write(t, "pkg/foo.go", "package pkg\n\nfunc Sha256Func() int { return 3 }\n")
	f.commit(t, "c1")
	head := f.revParse(t, "HEAD")

	hv := openMaterialized(t, f, "sha256", "", head)
	defer hv.Close()

	if hits, err := hv.Query.FindSymbol("Sha256Func"); err != nil || len(hits) == 0 {
		t.Fatalf("FindSymbol(Sha256Func)=%v err=%v", hits, err)
	}
	entry, err := hv.Sources.Read(context.Background(), SideHead, "pkg/foo.go", 1<<20)
	if err != nil || !strings.Contains(string(entry.Bytes), "Sha256Func") {
		t.Fatalf("sha256 head read=%q err=%v", entry.Bytes, err)
	}
	if len(entry.OID) != 64 {
		t.Fatalf("sha256 oid width=%d, want 64", len(entry.OID))
	}
}

// TestHeadViewMalformedSymlinkOmittedNotCreated proves a committed 120000 blob
// whose target bytes contain NUL is kept verbatim by the SourceResolver,
// classified binary, recorded as an omission, and never turned into a link.
func TestHeadViewMalformedSymlinkOmittedNotCreated(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "keep.go", "package pkg\n\nfunc Anchor() {}\n")
	f.commit(t, "seed")
	f.commitBlobEntry(t, "120000", "good", "keep.go")
	f.commitBlobEntry(t, "120000", "evil", "a\x00b")
	head := f.revParse(t, "HEAD")

	var links []string
	seam := func(root *os.Root, target, name string) error {
		links = append(links, name)
		return root.Symlink(target, name)
	}
	hv, err := OpenHeadView(context.Background(), HeadViewOptions{
		Runner:         f.runner,
		RepoRoot:       f.root,
		ObjectFormat:   "sha1",
		HeadTreeish:    head,
		SnapshotParent: t.TempDir(),
		symlink:        seam,
	})
	if err != nil {
		t.Fatalf("OpenHeadView: %v", err)
	}
	defer hv.Close()

	entry, err := hv.Sources.Read(context.Background(), SideHead, "evil", 1<<20)
	if err != nil {
		t.Fatalf("read evil: %v", err)
	}
	if string(entry.Bytes) != "a\x00b" {
		t.Fatalf("evil bytes=%q, want exact NUL target", entry.Bytes)
	}
	if !entry.IsSymlink() || !entry.Binary() || !entry.MalformedSymlink() {
		t.Fatalf("evil classification: symlink=%v binary=%v malformed=%v", entry.IsSymlink(), entry.Binary(), entry.MalformedSymlink())
	}
	if !hasOmission(hv.Omissions, "evil") {
		t.Fatalf("evil not recorded as an omission: %+v", hv.Omissions)
	}
	for _, name := range links {
		if name == "evil" {
			t.Fatal("malformed symlink was materialized as a filesystem link")
		}
	}
	if !contains(links, "good") {
		t.Fatalf("good symlink was not materialized: links=%v", links)
	}
}

// TestHeadViewEscapingSymlinkNotFollowed proves private indexing never follows a
// snapshot symlink whose target escapes the snapshot root.
func TestHeadViewEscapingSymlinkNotFollowed(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(secret, []byte("package secret\n\nfunc SecretSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newGitFixture(t)
	f.write(t, "kept.go", "package pkg\n\nfunc KeptSymbol() {}\n")
	f.commit(t, "seed")
	f.commitBlobEntry(t, "120000", "escape.go", secret) // absolute escaping target
	head := f.revParse(t, "HEAD")

	hv := openMaterialized(t, f, "sha1", "", head)
	defer hv.Close()

	if hits, err := hv.Query.FindSymbol("KeptSymbol"); err != nil || len(hits) == 0 {
		t.Fatalf("FindSymbol(KeptSymbol)=%v err=%v; indexing did not run", hits, err)
	}
	if hits, err := hv.Query.FindSymbol("SecretSymbol"); err != nil || len(hits) != 0 {
		t.Fatalf("FindSymbol(SecretSymbol)=%v err=%v; escaping symlink was followed", hits, err)
	}
	// The exact escaping target bytes are still available through the resolver.
	entry, err := hv.Sources.Read(context.Background(), SideHead, "escape.go", 1<<20)
	if err != nil || string(entry.Bytes) != secret {
		t.Fatalf("escape.go target=%q err=%v", entry.Bytes, err)
	}
}

// TestMaterializeBlobCapBoundary proves the per-blob cap is exact.
func TestMaterializeBlobCapBoundary(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "data.bin", "0123456789") // 10 bytes
	f.commit(t, "seed")
	head := f.revParse(t, "HEAD")

	openAt := func(max int64) error {
		hv, err := OpenHeadView(context.Background(), HeadViewOptions{
			Runner:         f.runner,
			RepoRoot:       f.root,
			ObjectFormat:   "sha1",
			HeadTreeish:    head,
			SnapshotParent: t.TempDir(),
			MaxBlobBytes:   max,
		})
		if err == nil {
			hv.Close()
		}
		return err
	}
	if err := openAt(10); err != nil {
		t.Fatalf("blob of exactly the cap failed: %v", err)
	}
	if err := openAt(9); !errors.Is(err, ErrSnapshotBlobTooLarge) {
		t.Fatalf("blob above the cap err=%v, want ErrSnapshotBlobTooLarge", err)
	}
}

// TestMaterializeTotalCapBoundary proves the total content cap is exact at, below,
// and above the total size.
func TestMaterializeTotalCapBoundary(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "a.bin", "01234")
	f.write(t, "b.bin", "56789")
	f.commit(t, "seed") // 10 bytes total
	head := f.revParse(t, "HEAD")

	openAt := func(max int64) error {
		hv, err := OpenHeadView(context.Background(), HeadViewOptions{
			Runner:         f.runner,
			RepoRoot:       f.root,
			ObjectFormat:   "sha1",
			HeadTreeish:    head,
			SnapshotParent: t.TempDir(),
			MaxTotalBytes:  max,
		})
		if err == nil {
			hv.Close()
		}
		return err
	}
	if err := openAt(11); err != nil {
		t.Fatalf("total below the cap failed: %v", err)
	}
	if err := openAt(10); err != nil {
		t.Fatalf("total exactly at the cap failed: %v", err)
	}
	if err := openAt(9); !errors.Is(err, ErrSnapshotTotalTooLarge) {
		t.Fatalf("total above the cap err=%v, want ErrSnapshotTotalTooLarge", err)
	}
}

// TestSourceResolverMaxBytesBoundary proves the resolver's per-read bound is
// exact.
func TestMaterializeSourceResolverMaxBytesBoundary(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "file.txt", "hello world") // 11 bytes
	f.commit(t, "seed")
	head := f.revParse(t, "HEAD")

	hv := openMaterialized(t, f, "sha1", "", head)
	defer hv.Close()

	if entry, err := hv.Sources.Read(context.Background(), SideHead, "file.txt", 11); err != nil || string(entry.Bytes) != "hello world" {
		t.Fatalf("read at exact bound=%q err=%v", entry.Bytes, err)
	}
	if _, err := hv.Sources.Read(context.Background(), SideHead, "file.txt", 10); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("read above bound err=%v, want ErrSourceTooLarge", err)
	}
}

// TestMaterializePromisorFailsClosed proves a missing promisor object fails the
// materialization without triggering a lazy/ext fetch.
func TestMaterializePromisorFailsClosed(t *testing.T) {
	partial, markers, _ := setupPartialClone(t)
	runner := execRunner(t)
	head := strings.TrimSpace(mustOutput(t, runner, partial, "rev-parse", "HEAD"))

	hv, err := OpenHeadView(context.Background(), HeadViewOptions{
		Runner:         runner,
		RepoRoot:       partial,
		ObjectFormat:   "sha1",
		HeadTreeish:    head,
		SnapshotParent: t.TempDir(),
		Config:         config.Config{},
	})
	if err == nil {
		hv.Close()
		t.Fatal("materialization of a missing promisor object succeeded")
	}
	if !errors.Is(err, ErrSnapshotObjectMissing) {
		t.Fatalf("err=%v, want ErrSnapshotObjectMissing", err)
	}
	if markerPresent(markers, "ext") {
		t.Fatalf("materialization triggered a promisor/ext lazy fetch: %v", markerNames(markers))
	}
}

// ---- helpers --------------------------------------------------------------

func openMaterialized(t *testing.T, f *gitFixture, objectFormat, base, head string) *HeadView {
	t.Helper()
	hv, err := OpenHeadView(context.Background(), HeadViewOptions{
		Runner:         f.runner,
		RepoRoot:       f.root,
		ObjectFormat:   objectFormat,
		BaseTreeish:    base,
		HeadTreeish:    head,
		SnapshotParent: t.TempDir(),
		Config:         config.Config{},
	})
	if err != nil {
		t.Fatalf("OpenHeadView: %v", err)
	}
	return hv
}

func mustOutput(t *testing.T, runner GitRunner, root string, args ...string) string {
	t.Helper()
	out, err := runner.Output(context.Background(), root, 1<<20, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(bytes.TrimSpace(out))
}

func hasOmission(oms []Omission, path string) bool {
	for _, o := range oms {
		if o.Path == path {
			return true
		}
	}
	return false
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ---- verified reuse (finding 1) -------------------------------------------

// indexCurrent indexes root into a fresh store and records the signature meta a
// published project store carries, so reuse eligibility can be verified.
func indexCurrent(t *testing.T, root string) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	opt := index.Options{}
	if _, err := index.IndexWithOptionsContext(context.Background(), db, root, opt); err != nil {
		t.Fatal(err)
	}
	sig, err := index.SignatureWithOptionsContext(context.Background(), root, opt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("cli_sig", strconv.FormatUint(sig, 16)); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestHeadViewReuseVerified proves a clean worktree at the resolved head with a
// matching published index is reused, and that a dirty tree, a mismatched head,
// or a stale index is materialized instead.
func TestHeadViewReuseVerified(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "pkg/foo.go", "package pkg\n\nfunc ReuseFunc() int { return 1 }\n")
	f.commit(t, "c1")
	base := f.revParse(t, "HEAD")
	f.write(t, "pkg/foo.go", "package pkg\n\nfunc ReuseFuncV2() int { return 2 }\n")
	f.commit(t, "c2")
	head := f.revParse(t, "HEAD")

	reuseOpts := func(db *store.Store, headTreeish string) HeadViewOptions {
		return HeadViewOptions{
			Runner:         f.runner,
			RepoRoot:       f.root,
			ObjectFormat:   "sha1",
			BaseTreeish:    base,
			HeadTreeish:    headTreeish,
			SnapshotParent: t.TempDir(),
			Reuse:          &ReusableView{Root: f.root, Store: db},
		}
	}

	t.Run("clean exact head reused", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		hv, err := OpenHeadView(context.Background(), reuseOpts(db, head))
		if err != nil {
			t.Fatal(err)
		}
		defer hv.Close()
		if hv.Store != db || hv.Root != f.root {
			t.Fatalf("eligible reuse materialized instead: root=%s", hv.Root)
		}
		if hits, err := hv.Query.FindSymbol("ReuseFuncV2"); err != nil || len(hits) == 0 {
			t.Fatalf("reused index query=%v err=%v", hits, err)
		}
		// The query/context services are constructed from the verified store/root,
		// never accepted from the caller.
		if hv.Context == nil || hv.Context.Store != db || hv.Context.Root != f.root {
			t.Fatalf("reuse context not built from the verified store/root: %+v", hv.Context)
		}
	})

	t.Run("dirty worktree materializes", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		f.write(t, "dirty.txt", "uncommitted\n")
		defer os.Remove(filepath.Join(f.root, "dirty.txt"))
		hv, err := OpenHeadView(context.Background(), reuseOpts(db, head))
		if err != nil {
			t.Fatal(err)
		}
		defer hv.Close()
		if hv.Store == db {
			t.Fatal("dirty worktree was reused instead of materialized")
		}
	})

	t.Run("mismatched head materializes", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		hv, err := OpenHeadView(context.Background(), reuseOpts(db, base)) // resolved head != current HEAD
		if err != nil {
			t.Fatal(err)
		}
		defer hv.Close()
		if hv.Store == db {
			t.Fatal("mismatched head was reused instead of materialized")
		}
	})

	t.Run("stale index materializes", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		if err := db.SetMeta("cli_sig", "deadbeef"); err != nil { // signature no longer matches
			t.Fatal(err)
		}
		hv, err := OpenHeadView(context.Background(), reuseOpts(db, head))
		if err != nil {
			t.Fatal(err)
		}
		defer hv.Close()
		if hv.Store == db {
			t.Fatal("stale index was reused instead of materialized")
		}
	})

	t.Run("reuse services are built from the verified store", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		hv, err := OpenHeadView(context.Background(), reuseOpts(db, head))
		if err != nil {
			t.Fatal(err)
		}
		defer hv.Close()
		if hv.Store != db {
			t.Fatalf("reuse did not use the verified store")
		}
		if hv.Query == nil || hv.Context == nil || hv.Context.Store != db || hv.Context.Root != f.root {
			t.Fatalf("reuse services not constructed from the verified store/root")
		}
	})

	t.Run("stale row with same meta is caught", func(t *testing.T) {
		db := indexCurrent(t, f.root)
		defer db.Close()
		foo := filepath.Join(f.root, "pkg/foo.go")
		orig, err := os.ReadFile(foo)
		if err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(foo, orig, 0o644)
		// Change the file's bytes without re-indexing, then set cli_sig to the
		// now-current signature. The freshness meta matches, but the store rows
		// are stale; only a byte-level row validation catches it.
		if err := os.WriteFile(foo, []byte("package pkg\n\nfunc ReuseFuncV3() int { return 3 }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sig, err := index.SignatureWithOptionsContext(context.Background(), f.root, index.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetMeta("cli_sig", strconv.FormatUint(sig, 16)); err != nil {
			t.Fatal(err)
		}
		// Workspace reuse (no resolved head tree-ish) cannot fall back to a
		// materialized tree, so a stale row surfaces as unavailable.
		if _, err := OpenHeadView(context.Background(), reuseOpts(db, "")); !errors.Is(err, ErrReuseUnavailable) {
			t.Fatalf("stale-row workspace reuse err=%v, want ErrReuseUnavailable", err)
		}
	})

	t.Run("ineligible without head is unavailable", func(t *testing.T) {
		empty, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer empty.Close()
		if _, err := OpenHeadView(context.Background(), reuseOpts(empty, "")); !errors.Is(err, ErrReuseUnavailable) {
			t.Fatalf("err=%v, want ErrReuseUnavailable", err)
		}
	})
}

// ---- strict cat-file binding (finding 2) ---------------------------------

// TestSourceResolverStrictCatFileBinding proves the resolver rejects unbound or
// malformed ls-tree/cat-file output before returning bytes.
func TestMaterializeSourceResolverStrictCatFileBinding(t *testing.T) {
	const oid = fortyHex
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	lsOK := treeRecord("100644", "blob", oid, "x")

	cases := []struct {
		name  string
		ls    string
		check string // cat-file --batch-check output
		blob  string // cat-file --batch output
		want  error
	}{
		{"ls multiple records", treeRecord("100644", "blob", oid, "x") + treeRecord("100644", "blob", oid, "x"), "", "", ErrSnapshotMalformedStream},
		{"ls mode/type mismatch", treeRecord("100644", "tree", oid, "x"), "", "", ErrSnapshotUnsafeEntry},
		{"ls short id", treeRecord("100644", "blob", "abcd", "x"), "", "", ErrSnapshotUnsafeEntry},
		{"ls tree short id", treeRecord("040000", "tree", "abcd", "x"), "", "", ErrSnapshotUnsafeEntry},
		{"batch-check missing terminal newline", lsOK, oid + " blob 3", "", ErrSnapshotMalformedStream},
		{"batch-check extra blank line", lsOK, oid + " blob 3\n\n", "", ErrSnapshotMalformedStream},
		{"batch-check wrong oid", lsOK, other + " blob 3\n", "", ErrSnapshotMalformedStream},
		{"batch-check wrong type", lsOK, oid + " tree 3\n", "", ErrSnapshotMalformedStream},
		{"batch wrong oid", lsOK, oid + " blob 3\n", other + " blob 3\nabc\n", ErrSnapshotMalformedStream},
		{"batch size mismatch", lsOK, oid + " blob 5\n", oid + " blob 3\nabc\n", ErrSnapshotMalformedStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := fakeTreeRunner{
				onOutput: func(args []string) ([]byte, error) {
					if len(args) > 0 && args[0] == "ls-tree" {
						return []byte(tc.ls), nil
					}
					return nil, fmt.Errorf("unexpected Output %v", args)
				},
				onPipe: func(_ io.Reader, args []string) ([]byte, error) {
					if len(args) >= 2 && args[0] == "cat-file" && args[1] == "--batch-check" {
						return []byte(tc.check), nil
					}
					if len(args) >= 2 && args[0] == "cat-file" && args[1] == "--batch" {
						return []byte(tc.blob), nil
					}
					return nil, fmt.Errorf("unexpected Pipe %v", args)
				},
			}
			resolver := &gitTreeResolver{runner: runner, root: t.TempDir(), width: 20, treeish: fortyHex}
			if _, err := resolver.read(context.Background(), "x", 1<<20); !errors.Is(err, tc.want) {
				t.Fatalf("read err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestBatchCheckRejectsBadSizes proves overflow/negative/MaxInt64 sizes are
// rejected before any content read.
func TestMaterializeBatchCheckRejectsBadSizes(t *testing.T) {
	for _, size := range []string{"-1", strconv.FormatInt(math.MaxInt64, 10), "not-a-number"} {
		runner := fakeTreeRunner{
			onPipe: func(_ io.Reader, args []string) ([]byte, error) {
				return []byte(fortyHex + " blob " + size + "\n"), nil
			},
		}
		if _, err := batchCheck(context.Background(), runner, t.TempDir(), []string{fortyHex}); !errors.Is(err, ErrSnapshotMalformedStream) {
			t.Fatalf("size %q err=%v, want ErrSnapshotMalformedStream", size, err)
		}
	}
}

// ---- streaming caps / producer-stop (finding 3) ---------------------------

// chunkedPipeRunner writes each ls-tree record to the parser in its own Write
// and stops as soon as a write is rejected, so a test can prove the producer is
// halted at the entry cap instead of streaming every record.
type chunkedPipeRunner struct {
	records [][]byte
	written *int
}

func (r chunkedPipeRunner) Output(context.Context, string, int64, ...string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected Output")
}

func (r chunkedPipeRunner) Pipe(_ context.Context, _ string, _ int64, _ io.Reader, stdout io.Writer, args ...string) error {
	if len(args) == 0 || args[0] != "ls-tree" {
		return fmt.Errorf("unexpected Pipe %v", args)
	}
	for _, rec := range r.records {
		if _, err := stdout.Write(rec); err != nil {
			return err // the parser rejected a record: the producer stops here
		}
		*r.written++
	}
	return nil
}

// TestMaterializeStreamingParserStopsProducer proves the incremental parser stops
// consuming the ls-tree stream at maxEntries+1 and reports the cap, rather than
// buffering the whole stream.
func TestMaterializeStreamingParserStopsProducer(t *testing.T) {
	const total = 100
	records := make([][]byte, 0, total)
	for i := range total {
		records = append(records, []byte(treeRecord("100644", "blob", fortyHex, fmt.Sprintf("f%03d", i))))
	}
	written := 0
	runner := chunkedPipeRunner{records: records, written: &written}
	opts := HeadViewOptions{
		Runner:       runner,
		RepoRoot:     t.TempDir(),
		ObjectFormat: "sha1",
		HeadTreeish:  fortyHex,
		MaxEntries:   5,
	}
	_, err := materializeTree(context.Background(), opts, 20, t.TempDir())
	if !errors.Is(err, ErrSnapshotTooManyEntries) {
		t.Fatalf("materializeTree err=%v, want ErrSnapshotTooManyEntries", err)
	}
	if written != 5 {
		t.Fatalf("producer wrote %d records before stopping, want 5 (cap+stop)", written)
	}
}

// TestMaterializeBatchCheckStrictFraming proves exact newline framing is required.
func TestMaterializeBatchCheckStrictFraming(t *testing.T) {
	for _, out := range []string{fortyHex + " blob 3", fortyHex + " blob 3\n\n"} {
		runner := fakeTreeRunner{
			onPipe: func(_ io.Reader, args []string) ([]byte, error) { return []byte(out), nil },
		}
		if _, err := batchCheck(context.Background(), runner, t.TempDir(), []string{fortyHex}); !errors.Is(err, ErrSnapshotMalformedStream) {
			t.Fatalf("framing %q err=%v, want ErrSnapshotMalformedStream", out, err)
		}
	}
}

// ---- immutable revision binding (TASK5-003) --------------------------------

// TestHeadViewRejectsUnresolvedTreeish proves OpenHeadView refuses a mutable ref
// or option-like tree-ish (head or base) before running any Git command, so a
// review can never bind to content that could move.
func TestHeadViewRejectsUnresolvedTreeish(t *testing.T) {
	// A runner that fails if invoked proves validation happens before any Git call.
	runner := fakeTreeRunner{
		onOutput: func(args []string) ([]byte, error) { return nil, fmt.Errorf("git must not run: %v", args) },
		onPipe:   func(_ io.Reader, args []string) ([]byte, error) { return nil, fmt.Errorf("git must not run: %v", args) },
	}
	cases := []struct {
		name string
		base string
		head string
	}{
		{"head is a ref", "", "HEAD"},
		{"head is a short id", "", "abc123"},
		{"head is option-like", "", "--output=/etc/passwd"},
		{"base is a ref", "main", fortyHex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := OpenHeadView(context.Background(), HeadViewOptions{
				Runner:         runner,
				RepoRoot:       t.TempDir(),
				ObjectFormat:   "sha1",
				BaseTreeish:    tc.base,
				HeadTreeish:    tc.head,
				SnapshotParent: t.TempDir(),
			})
			if !errors.Is(err, ErrSnapshotTreeishNotResolved) {
				t.Fatalf("err=%v, want ErrSnapshotTreeishNotResolved", err)
			}
		})
	}
}

// ---- oversized symlink availability (TASK5-011) ----------------------------

// TestHeadViewOversizedSymlinkOmitted proves a symlink blob larger than the
// symlink cap is recorded as an omission and never read into a content wave, so
// a hostile large 120000 blob (below the total cap) cannot make the view
// unavailable; its bytes remain available through the resolver.
func TestHeadViewOversizedSymlinkOmitted(t *testing.T) {
	f := newGitFixture(t)
	f.write(t, "keep.go", "package pkg\n\nfunc Kept() {}\n")
	f.commit(t, "seed")
	f.commitBlobEntry(t, "120000", "biglink", "keep.go") // 7-byte target, over a cap of 4
	head := f.revParse(t, "HEAD")

	var links []string
	seam := func(root *os.Root, target, name string) error {
		links = append(links, name)
		return root.Symlink(target, name)
	}
	hv, err := OpenHeadView(context.Background(), HeadViewOptions{
		Runner:          f.runner,
		RepoRoot:        f.root,
		ObjectFormat:    "sha1",
		HeadTreeish:     head,
		SnapshotParent:  t.TempDir(),
		MaxSymlinkBytes: 4,
		symlink:         seam,
	})
	if err != nil {
		t.Fatalf("OpenHeadView: %v", err)
	}
	defer hv.Close()

	if !hasOmission(hv.Omissions, "biglink") {
		t.Fatalf("oversized symlink not omitted: %+v", hv.Omissions)
	}
	if contains(links, "biglink") {
		t.Fatal("oversized symlink was materialized as a filesystem link")
	}
	entry, err := hv.Sources.Read(context.Background(), SideHead, "biglink", 1<<20)
	if err != nil || string(entry.Bytes) != "keep.go" {
		t.Fatalf("biglink target=%q err=%v, want keep.go", entry.Bytes, err)
	}
	if hits, err := hv.Query.FindSymbol("Kept"); err != nil || len(hits) == 0 {
		t.Fatalf("FindSymbol(Kept)=%v err=%v; materialization aborted", hits, err)
	}
}
