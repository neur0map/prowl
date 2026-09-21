package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/store"
)

func TestWalkIgnores(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write(".gitignore", "ignored/\n*.log\n")
	write("a.lua", "x")
	write("ignored/secret.lua", "x")
	write("debug.log", "x")
	write(".prowl/index.db", "x")
	write("sub/b.sh", "x")

	got, err := Walk(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.lua": true, "sub/b.sh": true, ".gitignore": true}
	if len(got) != len(want) {
		t.Fatalf("walk = %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected walked file %q in %v", g, got)
		}
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIndexFixture(t *testing.T) {
	s := openStore(t)
	root := filepath.Join("..", "..", "testdata", "sample-config")
	sum, err := Index(s, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Indexed != 11 || sum.Parsed != 11 || sum.Skipped != 0 {
		t.Fatalf("summary=%+v want Indexed=11 Parsed=11 Skipped=0", sum)
	}

	// Connectivity from resolution.
	mustResolve := func(rel, kind string) {
		id, err := s.FileID(rel)
		if err != nil {
			t.Fatalf("file %s: %v", rel, err)
		}
		in, _ := s.IncomingEdges("file", id, kind)
		if len(in) == 0 {
			t.Fatalf("%s has no incoming %s edges", rel, kind)
		}
	}
	mustResolve("hypr/colors.conf", "includes")        // sourced by hyprland.conf
	mustResolve("nvim/lua/opts.lua", "includes")       // require("opts")
	mustResolve("hypr/scripts/screenshot.sh", "binds") // bind exec script
	mustResolve("waybar/colors.css", "includes")       // @import
	mustResolve("scripts/power.sh", "references")      // waybar on-click

	// 'kitty' bind is an external bare command -> dangling.
	dang, _ := s.UnresolvedEdges("binds")
	foundKitty := false
	for _, e := range dang {
		if e.Raw == "kitty" {
			foundKitty = true
		}
	}
	if !foundKitty {
		t.Fatalf("expected dangling bind to 'kitty', got %+v", dang)
	}

	// Re-indexing unchanged content reparses nothing.
	sum2, err := Index(s, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Parsed != 0 || sum2.Skipped != 11 {
		t.Fatalf("reindex summary=%+v want Parsed=0 Skipped=11", sum2)
	}
}

func TestIndexIncremental(t *testing.T) {
	s := openStore(t)
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.lua", "function one() end\n")
	write("b.lua", "function two() end\n")

	if sum, err := Index(s, root, nil); err != nil || sum.Parsed != 2 {
		t.Fatalf("initial sum=%+v err=%v", sum, err)
	}
	// No changes -> nothing reparsed.
	if sum, err := Index(s, root, nil); err != nil || sum.Parsed != 0 || sum.Skipped != 2 {
		t.Fatalf("noop sum=%+v err=%v", sum, err)
	}
	// Change one file -> exactly one reparse.
	write("a.lua", "function one() end\nfunction three() end\n")
	if sum, err := Index(s, root, nil); err != nil || sum.Parsed != 1 || sum.Skipped != 1 {
		t.Fatalf("change sum=%+v err=%v", sum, err)
	}
	if hits, _ := s.SymbolsByName("three", 5); len(hits) != 1 {
		t.Fatalf("new symbol 'three' not indexed: %v", hits)
	}
	// Delete one file -> removed from index.
	os.Remove(filepath.Join(root, "b.lua"))
	if sum, err := Index(s, root, nil); err != nil || sum.Deleted != 1 {
		t.Fatalf("delete sum=%+v err=%v", sum, err)
	}
	if _, ok, _ := s.GetFileByPath("b.lua"); ok {
		t.Fatal("b.lua still indexed after deletion")
	}
}

func TestIndexLanguagesFilterAndAuto(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tool.py"), []byte("def tool():\n    pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	sum, err := IndexWithOptions(s, root, Options{Languages: []string{"go"}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Indexed != 1 {
		t.Fatalf("go-only indexed = %d, want 1", sum.Indexed)
	}
	if _, ok, _ := s.GetFileByPath("main.go"); !ok {
		t.Fatal("main.go was not indexed")
	}
	if _, ok, _ := s.GetFileByPath("tool.py"); ok {
		t.Fatal("tool.py was indexed despite languages=[go]")
	}

	sum, err = IndexWithOptions(s, root, Options{Languages: []string{"auto"}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Indexed != 2 {
		t.Fatalf("auto indexed = %d, want 2", sum.Indexed)
	}
}

func TestIndexStripsGeneratedAgentsBlockButKeepsUserGuidance(t *testing.T) {
	root := t.TempDir()
	agents := "# House rules\n\nRun the focused tests.\n\n<!-- prowl-agent -->\ngenerated routing instructions\n<!-- /prowl-agent -->\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	if _, err := IndexWithOptions(s, root, Options{Languages: []string{"auto"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetFileByPath("AGENTS.md"); !ok {
		t.Fatal("user-authored AGENTS.md content should remain indexed")
	}
	hits, err := s.SearchChunks("focused tests", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("user guidance not searchable: %v %v", hits, err)
	}
	generated, err := s.SearchChunks("generated routing instructions", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 0 {
		t.Fatalf("generated Prowl block leaked into index: %+v", generated)
	}
}

func TestSignatureIncludesLanguageSelection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goSig, err := SignatureWithOptions(root, Options{Languages: []string{"go"}})
	if err != nil {
		t.Fatal(err)
	}
	pySig, err := SignatureWithOptions(root, Options{Languages: []string{"python"}})
	if err != nil {
		t.Fatal(err)
	}
	if goSig == pySig {
		t.Fatal("language selection must affect freshness signature")
	}
}

func TestSignatureIncludesIgnorePolicy(t *testing.T) {
	root := t.TempDir()
	first, err := SignatureWithOptions(root, Options{Ignore: []string{"vendor/**"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := SignatureWithOptions(root, Options{Ignore: []string{"generated/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("ignore policy must affect freshness signature even when the walked file set is unchanged")
	}
}

func TestSignatureDetectsSameSizeSameMTimeReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "same.go")
	original := []byte("package a\nfunc Alpha() {}\n")
	replacement := []byte("package a\nfunc Bravo() {}\n")
	if len(original) != len(replacement) {
		t.Fatal("test fixture must preserve size")
	}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 123456789)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, err := SignatureWithOptions(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	after, err := SignatureWithOptions(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("same-size same-mtime content replacement did not change signature")
	}
}

func TestSourceSnapshotCandidateLimitAllowsExactLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2000; i++ {
		path := filepath.Join(root, fmt.Sprintf("file-%04d.go", i))
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Paths) != 2000 {
		t.Fatalf("snapshot paths = %d, want 2000", len(snapshot.Paths))
	}
}

func TestSourceSnapshotCandidateLimitRejectsCandidateAfterLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2001; i++ {
		path := filepath.Join(root, fmt.Sprintf("file-%04d.go", i))
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, 2000)
	var limitErr CandidateLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("snapshot error = %v, want CandidateLimitError", err)
	}
	if limitErr.Limit != 2000 {
		t.Fatalf("candidate limit = %d, want 2000", limitErr.Limit)
	}
}

func TestSourceSnapshotCandidateLimitRejectsNonpositiveLimit(t *testing.T) {
	root := t.TempDir()
	for _, limit := range []int{0, -1} {
		if _, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, limit); err == nil {
			t.Fatalf("limit %d: got nil error", limit)
		}
	}
}

func TestSourceSnapshotCandidateLimitMatchesCanonicalRootSymlinkBehavior(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical, err := SourceSnapshotWithOptionsContext(context.Background(), link, Options{})
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := SourceSnapshotWithOptionsLimitContext(context.Background(), link, Options{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Signature != bounded.Signature || !reflect.DeepEqual(canonical.Paths, bounded.Paths) {
		t.Fatalf("bounded snapshot %+v differs from canonical %+v", bounded, canonical)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SourceSnapshotWithOptionsLimitContext(canceled, link, Options{}, 2000); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled symlink-root snapshot error = %v", err)
	}
}

func TestSourceSnapshotCandidateLimitMatchesCanonicalInRootSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.go", filepath.Join(root, "alias.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonical, err := SourceSnapshotWithOptionsContext(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Signature != bounded.Signature || !reflect.DeepEqual(canonical.Paths, bounded.Paths) {
		t.Fatalf("bounded snapshot %+v differs from canonical %+v", bounded, canonical)
	}
}

func TestSourceSnapshotCandidateLimitDoesNotCountIgnoredFilesOrDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored-root.go\nignored-dir/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("ignored-nested.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1998; i++ {
		path := filepath.Join(root, fmt.Sprintf("visible-%04d.go", i))
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"ignored-root.go", "nested/ignored-nested.go", "ignored-dir/ignored.go"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("ignored\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	snapshot, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Paths) != 2000 {
		t.Fatalf("snapshot paths = %d, want 2000", len(snapshot.Paths))
	}
}

func TestSourceSnapshotCandidateAfterLimitIsNotInspected(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2001; i++ {
		path := filepath.Join(root, fmt.Sprintf("file-%04d.go", i))
		if err := os.WriteFile(path, []byte("package fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	inspected := 0
	inspect := func(_ context.Context, _ sourceCandidate, rel string, _ os.DirEntry) (string, error) {
		inspected++
		if inspected > 2000 {
			return "", errors.New("candidate 2001 was inspected")
		}
		return rel, nil
	}
	_, err := sourceSnapshotWithOptionsInspectContext(context.Background(), root, Options{}, 2000, inspect)
	var limitErr CandidateLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("snapshot error = %v, want CandidateLimitError", err)
	}
	if inspected != 2000 {
		t.Fatalf("inspected candidates = %d, want 2000", inspected)
	}
}

func TestSourceSnapshotCandidateLimitSkipsFIFOWithoutBlocking(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := t.TempDir()
	fifo := filepath.Join(root, "blocked.go")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	snapshot, err := SourceSnapshotWithOptionsLimitContext(ctx, root, Options{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Paths, []string{"visible.go"}) {
		t.Fatalf("snapshot paths = %v, want [visible.go]", snapshot.Paths)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("FIFO candidate blocked for %v", elapsed)
	}
}

func TestSourceSnapshotCandidateLimitPinsRootAcrossRenameReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("ORIGINAL-SENTINEL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, err := SourceSnapshotWithOptionsLimitContext(context.Background(), root, Options{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.go"), []byte("OUTSIDE-SENTINEL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "project-moved")
	swapped := false
	inspect := func(ctx context.Context, candidate sourceCandidate, rel string, entry os.DirEntry) (string, error) {
		if !swapped {
			swapped = true
			if err := os.Rename(root, moved); err != nil {
				return "", err
			}
			if err := os.Symlink(outside, root); err != nil {
				return "", err
			}
		}
		return snapshotCandidateEntry(ctx, candidate, rel, entry)
	}
	pinned, err := sourceSnapshotWithOptionsInspectContext(context.Background(), root, Options{}, 2000, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Signature != baseline.Signature || !reflect.DeepEqual(pinned.Paths, baseline.Paths) {
		t.Fatalf("replacement-root snapshot %+v differs from original %+v", pinned, baseline)
	}
}

func TestSourceSnapshotCandidateLimitCancelsDuringTraversal(t *testing.T) {
	base := t.TempDir()
	root := base
	for i := 0; i < 20; i++ {
		root = filepath.Join(root, "empty")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := newCancelAfterChecks(8)
	if _, err := SourceSnapshotWithOptionsLimitContext(ctx, base, Options{}, 2000); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot error = %v, want context.Canceled", err)
	}
}

func TestWalkContextCancelsDuringDirectoryOnlyTraversal(t *testing.T) {
	base := t.TempDir()
	root := base
	for i := 0; i < 20; i++ {
		root = filepath.Join(root, "empty")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := newCancelAfterChecks(8)
	if _, err := WalkContext(ctx, base, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkContext error = %v, want context.Canceled", err)
	}
}

func TestInterruptedIndexIsMarkedIncompleteAndRecovers(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		name := filepath.Join(root, fmt.Sprintf("file_%02d.go", i))
		if err := os.WriteFile(name, []byte("package fixture\nfunc Value() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	database := openStore(t)
	ctx := newCancelAfterChecks(35)
	if _, err := IndexWithOptionsContext(ctx, database, root, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted index error = %v, want context.Canceled", err)
	}
	state, err := database.GetMeta("index_state")
	if err != nil {
		t.Fatal(err)
	}
	if state != "incomplete" {
		t.Fatalf("index_state = %q, want incomplete", state)
	}
	if _, err := IndexWithOptionsContext(context.Background(), database, root, Options{}); err != nil {
		t.Fatal(err)
	}
	state, err = database.GetMeta("index_state")
	if err != nil || state != "complete" {
		t.Fatalf("recovered index_state = %q, %v; want complete", state, err)
	}
}

type cancelAfterChecks struct{ remaining atomic.Int32 }

func newCancelAfterChecks(count int32) *cancelAfterChecks {
	ctx := &cancelAfterChecks{}
	ctx.remaining.Store(count)
	return ctx
}

func (*cancelAfterChecks) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelAfterChecks) Done() <-chan struct{}       { return nil }
func (*cancelAfterChecks) Value(any) any               { return nil }
func (ctx *cancelAfterChecks) Err() error {
	if ctx.remaining.Add(-1) <= 0 {
		return context.Canceled
	}
	return nil
}

func TestValidateSnapshotAcceptsGeneratedAgentsMarker(t *testing.T) {
	root := t.TempDir()
	body := []byte("# Project instructions\n\n<!-- prowl-agent -->\ngenerated material\n<!-- /prowl-agent -->\n\nKeep this human text.\n")
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t)
	defer s.Close()
	if _, err := IndexWithOptionsContext(context.Background(), s, root, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSnapshotContext(context.Background(), s, root); err != nil {
		t.Fatalf("generated marker snapshot: %v", err)
	}
}

func TestIndexWithProgressReportsBoundedPhases(t *testing.T) {
	root := t.TempDir()
	for i := range 65 {
		name := filepath.Join(root, fmt.Sprintf("file_%02d.go", i))
		if err := os.WriteFile(name, []byte("package fixture\nfunc Value() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var snapshots []Progress
	if _, err := IndexWithOptionsProgressContext(context.Background(), openStore(t), root, Options{}, func(snapshot Progress) error {
		snapshots = append(snapshots, snapshot)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) < 4 {
		t.Fatalf("progress snapshots=%v, want walking, indexing, resolving, complete", snapshots)
	}
	if snapshots[0] != (Progress{Phase: "walking", Percent: 0}) {
		t.Fatalf("first progress=%+v, want walking at 0", snapshots[0])
	}
	if snapshots[len(snapshots)-1] != (Progress{Phase: "complete", Percent: 100}) {
		t.Fatalf("last progress=%+v, want complete at 100", snapshots[len(snapshots)-1])
	}
	indexing := 0
	seenResolving := false
	phaseRank := map[string]int{"walking": 0, "indexing": 1, "resolving": 2, "complete": 3}
	lastRank := -1
	for i, snapshot := range snapshots {
		if snapshot.Percent < 0 || snapshot.Percent > 100 {
			t.Fatalf("progress[%d]=%+v is outside 0..100", i, snapshot)
		}
		if i > 0 && snapshot.Percent < snapshots[i-1].Percent {
			t.Fatalf("progress decreased: %v", snapshots)
		}
		rank, ok := phaseRank[snapshot.Phase]
		if !ok || rank < lastRank {
			t.Fatalf("progress phases are not ordered: %v", snapshots)
		}
		lastRank = rank
		switch snapshot.Phase {
		case "indexing":
			indexing++
		case "resolving":
			seenResolving = true
		}
	}
	if !seenResolving {
		t.Fatalf("progress=%v missing resolving phase", snapshots)
	}
	if indexing > 1+(65+31)/32 {
		t.Fatalf("indexing reports=%d, want at most initial + every 32 files + final; snapshots=%v", indexing, snapshots)
	}
}

func TestIndexWithProgressReporterFailureLeavesGenerationIncomplete(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database := openStore(t)
	if _, err := IndexWithOptionsContext(context.Background(), database, root, Options{}); err != nil {
		t.Fatal(err)
	}
	reportFailure := errors.New("durable progress update failed")
	if _, err := IndexWithOptionsProgressContext(context.Background(), database, root, Options{}, func(snapshot Progress) error {
		if snapshot.Phase == "complete" {
			return reportFailure
		}
		return nil
	}); !errors.Is(err, reportFailure) {
		t.Fatalf("index error=%v, want reporter failure", err)
	}
	state, err := database.GetMeta("index_state")
	if err != nil {
		t.Fatal(err)
	}
	if state != "incomplete" {
		t.Fatalf("index_state=%q, want incomplete after reporter failure", state)
	}
}

// TestIndexParallelDeterministicIDs pins the ordered-futures guarantee: because
// files are parsed concurrently but written in traversal order, a fresh index of
// the same tree must assign each file the same ID every run. Consuming results in
// completion order instead would make IDs vary run-to-run; this guards against that.
func TestIndexParallelDeterministicIDs(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "sample-config")
	ids := func() map[string]int64 {
		s := openStore(t)
		if _, err := Index(s, root, nil); err != nil {
			t.Fatal(err)
		}
		all, err := s.AllFiles()
		if err != nil {
			t.Fatal(err)
		}
		m := make(map[string]int64, len(all))
		for _, f := range all {
			id, err := s.FileID(f.RelPath)
			if err != nil {
				t.Fatal(err)
			}
			m[f.RelPath] = id
		}
		return m
	}
	a, b := ids(), ids()
	if len(a) == 0 {
		t.Fatal("no files indexed")
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("file IDs not deterministic across runs:\n a=%v\n b=%v", a, b)
	}
}

// TestIndexSkipsGeneratedArtifacts proves lockfiles and minified bundles are not
// indexed: a package-lock.json would otherwise emit thousands of "setting"
// symbols that flood the index, while the real source beside it is still indexed.
func TestIndexSkipsGeneratedArtifacts(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package-lock.json", `{"packages":{"a":{"version":"1"},"b":{"version":"2"}}}`)
	write("web/app.min.js", `var a=1,b=2;function d(){return a+b}`)
	write("main.go", "package main\nfunc Main() {}\n")

	s := openStore(t)
	if _, err := Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.GetFileByPath("main.go"); err != nil || !ok {
		t.Fatalf("main.go should be indexed (ok=%v err=%v)", ok, err)
	}
	for _, rel := range []string{"package-lock.json", "web/app.min.js"} {
		if _, ok, _ := s.GetFileByPath(rel); ok {
			t.Errorf("%s is a generated artifact and must be skipped, but was indexed", rel)
		}
	}
}

// symbolDoc reads the stored doc of the single symbol named `sym` in `rel`,
// through the public store surface (SymbolsByName + SymbolDocsInFile), so the
// assertion reflects what actually landed in symbols.doc after indexing.
func symbolDoc(t *testing.T, s *store.Store, rel, sym string) string {
	t.Helper()
	fh, ok, err := s.GetFileByPath(rel)
	if err != nil || !ok {
		t.Fatalf("GetFileByPath(%s): ok=%v err=%v", rel, ok, err)
	}
	docs, err := s.SymbolDocsInFile(fh.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range mustSymbols(t, s, sym) {
		if h.File == rel {
			return docs[h.ID]
		}
	}
	return ""
}

func mustSymbols(t *testing.T, s *store.Store, name string) []store.SymbolHit {
	t.Helper()
	hits, err := s.SymbolsByName(name, 50)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// Reindexing a file whose doc comment changed must replace the stored doc, not
// leave the old one. This exercises the full path: PopulateDocs reads the new
// comment, ReplaceFileGraph deletes the old symbol (symbols_ad clears its
// fts_docs entry) and inserts the new one (symbols_ai indexes the new doc).
func TestReindexUpdatesStoredDoc(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "svc.go")
	write := func(doc string) {
		t.Helper()
		src := "package svc\n\n// Serve " + doc + "\nfunc Serve() {}\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	write("alphaunique brings the listener online")
	if _, err := Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	if got := symbolDoc(t, s, "svc.go", "Serve"); got != "// Serve alphaunique brings the listener online" {
		t.Fatalf("stored doc after first index = %q", got)
	}

	write("betaunique takes the listener offline")
	if _, err := Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	if got := symbolDoc(t, s, "svc.go", "Serve"); got != "// Serve betaunique takes the listener offline" {
		t.Fatalf("stored doc not updated on reindex (stale?): %q", got)
	}
}
