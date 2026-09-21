package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

// indexedAt indexes root into a fresh temp-db store and returns a querier over
// it. Unlike indexed(t), the caller supplies the source root, so tests can edit
// files on disk and reindex the same store in place via index.Index(q.s, root,
// nil). Shared with history_test.go (Task 9); defined here only.
func indexedAt(t *testing.T, root string) *Querier {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := index.Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	return New(s)
}

func writeSpanFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func spanFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module spanfixture\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An agent's line numbers go stale the moment it edits. span answers "where is
// this now", and the digest lets a caller detect drift without re-reading.
func TestSpanReportsCurrentRangeAndDigest(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	writeSpanFile(t, dir, "clusters.go", "package widget\n\n// Clusters groups files.\nfunc Clusters() int {\n\treturn 42\n}\n\nfunc Other() {}\n")
	q := indexedAt(t, dir)

	spans, err := q.Span(dir, "Clusters")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) == 0 {
		t.Fatal("no span for a symbol that exists")
	}
	s := spans[0]
	if s.Name != "Clusters" || s.File != "clusters.go" {
		t.Fatalf("span = %+v, want Clusters in clusters.go", s)
	}
	if s.LineStart <= 0 || s.LineEnd < s.LineStart {
		t.Fatalf("bad range %d-%d", s.LineStart, s.LineEnd)
	}
	if len(s.Digest) != SpanDigestLen {
		t.Fatalf("digest = %q, want %d hex chars", s.Digest, SpanDigestLen)
	}

	// A reindex that changed nothing leaves the same bytes: the digest and the
	// range must both be stable.
	if _, err := index.Index(q.s, dir, nil); err != nil {
		t.Fatal(err)
	}
	again, err := q.Span(dir, "Clusters")
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Digest != s.Digest {
		t.Fatalf("digest changed on a no-op reindex: %s -> %s", s.Digest, again[0].Digest)
	}
	if again[0].LineStart != s.LineStart || again[0].LineEnd != s.LineEnd {
		t.Fatalf("range moved on a no-op reindex: %d-%d -> %d-%d", s.LineStart, s.LineEnd, again[0].LineStart, again[0].LineEnd)
	}
}

// A real edit to the symbol's body must move the digest, while the name still
// resolves to the same file: that is exactly the drift a caller wants to catch.
func TestSpanDigestChangesWhenTheSpanChanges(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	const path = "calc.go"
	writeSpanFile(t, dir, path, "package calc\n\nfunc Sum() int {\n\treturn 1\n}\n\nfunc Product() int {\n\treturn 2\n}\n")
	q := indexedAt(t, dir)

	before, err := q.Span(dir, "Sum")
	if err != nil || len(before) == 0 {
		t.Fatalf("span Sum = %v, %v", before, err)
	}

	// Rewrite Sum's body on disk; Product is untouched.
	writeSpanFile(t, dir, path, "package calc\n\nfunc Sum() int {\n\treturn 1 + 2 + 3\n}\n\nfunc Product() int {\n\treturn 2\n}\n")
	if _, err := index.Index(q.s, dir, nil); err != nil {
		t.Fatal(err)
	}
	after, err := q.Span(dir, "Sum")
	if err != nil || len(after) == 0 {
		t.Fatalf("span Sum after edit = %v, %v", after, err)
	}
	if after[0].Name != "Sum" || after[0].File != before[0].File {
		t.Fatalf("Sum no longer resolves the same after the edit: %+v", after[0])
	}
	if after[0].Digest == before[0].Digest {
		t.Fatalf("digest unchanged after a body edit: %s", after[0].Digest)
	}
}

// The digest is content-only, not position: inserting lines ABOVE a symbol moves
// its line numbers but leaves its body bytes identical, so the digest must stay
// put. Same code, new coordinates -- the caller learns the range shifted but the
// thing it planned to edit did not.
func TestSpanDigestStableWhenLinesShiftButBodyDoesNot(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	const path = "shift.go"
	writeSpanFile(t, dir, path, "package shift\n\nfunc Target() string {\n\treturn \"stable\"\n}\n")
	q := indexedAt(t, dir)

	before, err := q.Span(dir, "Target")
	if err != nil || len(before) == 0 {
		t.Fatalf("span Target = %v, %v", before, err)
	}

	// Insert a whole function above Target: Target's body is byte-identical, it
	// just starts lower in the file now.
	writeSpanFile(t, dir, path, "package shift\n\nfunc Prelude() {\n\t// pushed Target down\n\t_ = 0\n}\n\nfunc Target() string {\n\treturn \"stable\"\n}\n")
	if _, err := index.Index(q.s, dir, nil); err != nil {
		t.Fatal(err)
	}
	after, err := q.Span(dir, "Target")
	if err != nil || len(after) == 0 {
		t.Fatalf("span Target after insert = %v, %v", after, err)
	}
	if after[0].LineStart <= before[0].LineStart {
		t.Fatalf("Target's start line did not shift down: %d -> %d", before[0].LineStart, after[0].LineStart)
	}
	if after[0].Digest != before[0].Digest {
		t.Fatalf("content-only digest changed on a pure position shift: %s -> %s", before[0].Digest, after[0].Digest)
	}
}

// A name shared by several symbols must surface every match, so the caller sees
// the choice instead of a silently-picked first hit.
func TestSpanAmbiguousNameReturnsAllMatches(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	writeSpanFile(t, dir, "one.go", "package a\n\nfunc Close() { _ = 1 }\n")
	writeSpanFile(t, dir, "two.go", "package a\n\nfunc Close() { _ = 22 }\n")
	q := indexedAt(t, dir)

	spans, err := q.Span(dir, "Close")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) < 2 {
		t.Fatalf("ambiguous name returned %d spans, want >= 2: %+v", len(spans), spans)
	}
	files := map[string]bool{}
	for _, s := range spans {
		files[s.File] = true
	}
	if !files["one.go"] || !files["two.go"] {
		t.Fatalf("ambiguous span did not surface both files: %+v", spans)
	}
	// Distinct bodies must not collide on one digest.
	if spans[0].Digest == spans[1].Digest {
		t.Fatalf("distinct bodies shared a digest: %+v", spans)
	}
}

// A miss is a clean empty result, not an error or a crash.
func TestSpanMissingSymbolReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	writeSpanFile(t, dir, "x.go", "package x\n\nfunc Real() {}\n")
	q := indexedAt(t, dir)

	spans, err := q.Span(dir, "ZzzQqqNonexistentSymbol")
	if err != nil {
		t.Fatalf("missing symbol should not error: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("missing symbol returned %d spans: %+v", len(spans), spans)
	}
}

// An empty index answers cleanly too: no symbols, no error.
func TestSpanEmptyIndexReturnsEmpty(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	q := New(s)

	spans, err := q.Span(t.TempDir(), "Anything")
	if err != nil {
		t.Fatalf("empty index should not error: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("empty index returned %d spans: %+v", len(spans), spans)
	}
}

// A file deleted since indexing is not a span: its symbol is skipped rather than
// crashing on the missing read.
func TestSpanFileDeletedSinceIndexingSkipped(t *testing.T) {
	dir := t.TempDir()
	spanFixture(t, dir)
	writeSpanFile(t, dir, "gone.go", "package g\n\nfunc Vanishing() {}\n")
	q := indexedAt(t, dir)

	// Remove the file from disk WITHOUT reindexing, so the symbol still lives in
	// the index but its bytes are gone.
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil {
		t.Fatal(err)
	}
	spans, err := q.Span(dir, "Vanishing")
	if err != nil {
		t.Fatalf("deleted file should not error: %v", err)
	}
	if len(spans) != 0 {
		t.Fatalf("deleted file still produced a span: %+v", spans)
	}
}
