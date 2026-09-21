package query

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// The regression this whole phase exists for: prose that answers the query lives
// in a doc comment inside a chunk dominated by code, and must still rank. On the
// real corpus, agent/redact.py -- whose docstring reads "secret redaction for
// logs ... before they reach log files" -- was absent from all 50 results for
// "keep secrets out of the log file", because BM25 averaged its short docstring
// against its long import block. Fusing a doc-comment signal into the ranked
// chunk list surfaces it without displacing the code hits that already answered.
func TestSearchSurfacesDocOnlyMatches(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Ten decoys whose code mentions "log" but whose purpose is unrelated, plus one
	// file whose doc comment is the answer and whose code shares no query token.
	mkDocFixture(t, s)
	hits, err := New(s).SimilarCode(context.Background(), "keep secrets out of the log file")
	if err != nil {
		t.Fatal(err)
	}
	rank := -1
	for i, h := range hits {
		if h.File == "redact.py" {
			rank = i + 1
			break
		}
	}
	// The one behavioural guarantee: the doc-only answer, which lexical and vector
	// search miss entirely (its code shares no query token), goes from absent to
	// present. The down-weighted doc signal is recall, not re-ranking, so redact
	// legitimately sorts below the ten lexical decoys rather than above them --
	// asserting a top rank here would contradict the C5-respecting weighting.
	if rank == -1 {
		t.Fatalf("redact.py absent: the doc signal failed to surface a doc-only answer in %s", fmtHits(hits))
	}
	// The doc signal is additive, not a filter: the ten decoys whose code lexically
	// answers the query must still appear, and no result may be blank (the prior
	// whitespace-chunk bug).
	var decoys int
	for _, h := range hits {
		if h.File == "" {
			t.Fatalf("blank result in %s", fmtHits(hits))
		}
		if h.File != "redact.py" {
			decoys++
		}
	}
	if decoys < 10 {
		t.Fatalf("doc signal displaced lexical hits: only %d decoys survived in %s", decoys, fmtHits(hits))
	}
}

// mkDocFixture inserts eleven files: ten decoys whose chunk text contains "log"
// (so lexical search ranks them) with docs unrelated to the query, and redact.py
// whose code shares no query token but whose doc comment is the answer.
func mkDocFixture(t *testing.T, s *store.Store) {
	t.Helper()
	put := func(rel, lang, code, doc string) {
		fid, err := s.UpsertFile(store.File{RelPath: rel, Lang: lang, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		var syms []store.Symbol
		if doc != "" {
			syms = []store.Symbol{{Name: "sym", Kind: "function", StartLine: 1, EndLine: 2, Doc: doc}}
		}
		if err := s.ReplaceFileGraph(fid, syms, nil, nil, []store.Chunk{{StartLine: 1, EndLine: 2, Text: code}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		put(fmt.Sprintf("mod%d.go", i), "go",
			fmt.Sprintf("func handle%d() {\n    log.Printf(\"event %d handled\")\n}", i, i),
			"Handles inbound request routing for a subsystem.")
	}
	put("redact.py", "python",
		"def _sub(p, s):\n    return p.sub(_m, s)",
		"Regex-based secret redaction for logs and tool output. Masks API keys, tokens and credentials before they reach log files.")
}

func fmtHits(hits []store.ChunkHit) string {
	var b []string
	for i, h := range hits {
		b = append(b, fmt.Sprintf("%d:%s:%d", i+1, h.File, h.StartLine))
	}
	return fmt.Sprintf("%v", b)
}

// --- mergeDocRecall contract tests ---------------------------------------
//
// TestSearchSurfacesDocOnlyMatches above only proves the doc-only answer becomes
// reachable; on its 11-file fixture it holds for docRecallFloor anywhere from 1
// to 11 and never exercises mergeDocRecall directly. These tests pin the actual
// C5 contract: the top ten is element-wise identical to the base (the VALUE 11,
// not merely the base's preserved relative order, is what shields the guard
// targets), appended misses are tier-sorted and capped at docRecallCap, and the
// sub-ten, empty, and limit boundaries neither gap, mis-order, drop the top ten,
// nor panic on the out[:ins] slice.

func mkHit(file string, line int) store.ChunkHit {
	return store.ChunkHit{File: file, StartLine: line, EndLine: line + 1}
}

func sameHit(a, b store.ChunkHit) bool {
	return a.File == b.File && a.StartLine == b.StartLine
}

// srcBase returns n source-tier base hits with distinct file:start_line keys, in
// a fixed relative order the merge must preserve.
func srcBase(n int) []store.ChunkHit {
	base := make([]store.ChunkHit, n)
	for i := range base {
		base[i] = mkHit(fmt.Sprintf("src/f%02d.go", i), i+1)
	}
	return base
}

// TestMergeDocRecallKeepsTopTen is the pin the top-ten guarantee has lacked: a
// base of 12 code hits plus 2 doc-only misses must leave result[:10]
// element-wise identical to base[:10], with the misses inserted at indices 10
// and 11 and the two displaced base hits shifted down intact. It fails the
// instant docRecallFloor drops below 11 (misses invade the read window) or the
// append order flips.
func TestMergeDocRecallKeepsTopTen(t *testing.T) {
	base := srcBase(12)
	// Source-tier miss first, test-tier second: after the tier sort this is the
	// order they must retain at the append site.
	misses := []store.ChunkHit{mkHit("recall.go", 200), mkHit("recall_test.go", 100)}

	res := mergeDocRecall(base, misses, 50)

	if len(res) != 14 {
		t.Fatalf("want 14 results (12 base + 2 misses), got %d: %s", len(res), fmtHits(res))
	}
	for i := range 10 {
		if !sameHit(res[i], base[i]) {
			t.Fatalf("top ten changed at index %d: want %s:%d, got %s:%d\n%s",
				i, base[i].File, base[i].StartLine, res[i].File, res[i].StartLine, fmtHits(res))
		}
	}
	if res[10].File != "recall.go" || res[11].File != "recall_test.go" {
		t.Fatalf("misses not appended at 10,11: got %s then %s\n%s", res[10].File, res[11].File, fmtHits(res))
	}
	if !sameHit(res[12], base[10]) || !sameHit(res[13], base[11]) {
		t.Fatalf("displaced base hits reordered below the misses: %s", fmtHits(res))
	}
}

// TestMergeDocRecallTierSortsMisses pins the tier ordering of the appended block
// in isolation: passed a test-tier miss ahead of a source-tier miss, the merge
// must still surface the source-tier one first, so a future edit cannot append a
// test-tier hit above a source-tier one (the C5 tiering, applied to recall).
func TestMergeDocRecallTierSortsMisses(t *testing.T) {
	base := srcBase(12)
	misses := []store.ChunkHit{mkHit("b_test.go", 1), mkHit("a.go", 2)}

	res := mergeDocRecall(base, misses, 50)

	if searchTier("a.go") >= searchTier("b_test.go") {
		t.Fatalf("fixture invalid: a.go must be a stronger tier than b_test.go")
	}
	if res[10].File != "a.go" || res[11].File != "b_test.go" {
		t.Fatalf("misses not tier-sorted at append site: got %s then %s\n%s",
			res[10].File, res[11].File, fmtHits(res))
	}
}

// TestMergeDocRecallCapsAppend pins docRecallCap: given more misses than the cap,
// the merge appends exactly docRecallCap of them and no more. Position-independent
// on purpose, so it isolates the cap from the floor.
func TestMergeDocRecallCapsAppend(t *testing.T) {
	base := srcBase(12)
	misses := make([]store.ChunkHit, 4)
	for i := range misses {
		misses[i] = mkHit(fmt.Sprintf("miss%02d.go", i), 500+i)
	}

	res := mergeDocRecall(base, misses, 50)

	if len(res) != len(base)+docRecallCap {
		t.Fatalf("want %d results (base + cap %d), got %d: %s",
			len(base)+docRecallCap, docRecallCap, len(res), fmtHits(res))
	}
	var appended int
	for _, h := range res {
		if strings.HasPrefix(h.File, "miss") {
			appended++
		}
	}
	if appended != docRecallCap {
		t.Fatalf("appended %d misses, want cap %d: %s", appended, docRecallCap, fmtHits(res))
	}
}

// TestMergeDocRecallSubTenBase answers the question the top-ten measurement never
// covered: a floor of 11 on a five-result page. ins clamps to len(base), so the
// two misses land at indices 5 and 6 (positions 6 and 7) -- appended at the tail,
// no gap, no off-by-one, no out-of-range panic on out[:ins].
func TestMergeDocRecallSubTenBase(t *testing.T) {
	base := srcBase(5)
	misses := []store.ChunkHit{mkHit("doc0.go", 90), mkHit("doc1.go", 91)}

	res := mergeDocRecall(base, misses, 50)

	if len(res) != 7 {
		t.Fatalf("want 7 results (5 base + 2 misses), got %d: %s", len(res), fmtHits(res))
	}
	for i := range 5 {
		if !sameHit(res[i], base[i]) {
			t.Fatalf("base hit %d moved: %s", i, fmtHits(res))
		}
	}
	if res[5].File != "doc0.go" || res[6].File != "doc1.go" {
		t.Fatalf("misses did not append at 5,6: %s", fmtHits(res))
	}
}

// TestMergeDocRecallBoundaries walks the base-length boundaries the constant 11
// could break -- 0, 1, exactly 10, exactly 11, fewer misses than the cap, no
// misses, and over the cap -- asserting for each that the insertion point is
// min(docRecallFloor-1, len(base)), every base hit survives in its original
// relative order around a single contiguous miss block, and nothing panics.
func TestMergeDocRecallBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		baseLen  int
		missLen  int
		wantLen  int
		wantIns  int // index of the first appended miss (-1 = none)
		wantMiss int // number of misses appended after the cap
	}{
		{"empty base", 0, 2, 2, 0, 2},
		{"single base", 1, 2, 3, 1, 2},
		{"exactly ten", 10, 2, 12, 10, 2},
		{"exactly eleven", 11, 2, 13, 10, 2},
		{"fewer than cap", 3, 1, 4, 3, 1},
		{"no misses", 4, 0, 4, -1, 0},
		{"over cap", 12, 4, 15, 10, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := srcBase(tc.baseLen)
			misses := make([]store.ChunkHit, tc.missLen)
			for i := range misses {
				misses[i] = mkHit(fmt.Sprintf("miss%02d.go", i), 500+i)
			}
			res := mergeDocRecall(base, misses, 50)
			if len(res) != tc.wantLen {
				t.Fatalf("len=%d want %d: %s", len(res), tc.wantLen, fmtHits(res))
			}
			bi := 0
			for i, h := range res {
				if strings.HasPrefix(h.File, "miss") {
					if tc.wantIns == -1 || i < tc.wantIns || i >= tc.wantIns+tc.wantMiss {
						t.Fatalf("miss at index %d, expected block [%d,%d): %s",
							i, tc.wantIns, tc.wantIns+tc.wantMiss, fmtHits(res))
					}
					continue
				}
				if bi >= len(base) || !sameHit(h, base[bi]) {
					t.Fatalf("base out of order at result[%d]: %s", i, fmtHits(res))
				}
				bi++
			}
			if bi != len(base) {
				t.Fatalf("lost base hits: matched %d of %d: %s", bi, len(base), fmtHits(res))
			}
		})
	}
}

// TestMergeDocRecallRespectsLimit pins C5 under limit pressure: when appending
// misses would overflow the page, the top ten still survives and the misses
// still land at the floor -- it is mid-page code matches that fall off the tail,
// never a guard target.
func TestMergeDocRecallRespectsLimit(t *testing.T) {
	base := srcBase(12)
	misses := []store.ChunkHit{mkHit("doc0.go", 90), mkHit("doc1.go", 91)}

	res := mergeDocRecall(base, misses, 12)

	if len(res) != 12 {
		t.Fatalf("result not truncated to limit: got %d: %s", len(res), fmtHits(res))
	}
	for i := range 10 {
		if !sameHit(res[i], base[i]) {
			t.Fatalf("top ten changed at index %d under limit pressure: %s", i, fmtHits(res))
		}
	}
	if res[10].File != "doc0.go" || res[11].File != "doc1.go" {
		t.Fatalf("misses not at floor under limit pressure: %s", fmtHits(res))
	}
}
