package mcp

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// recordStats should count the answer's bytes once and attribute the exact size
// of the files the answer pointed at to the baseline (per-answer accuracy).
func TestRecordStatsBaseline(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, filepath.Join("..", "..", "testdata", "sample-config"), nil); err != nil {
		t.Fatal(err)
	}
	f, ok, err := s.GetFileByPath("hypr/colors.conf")
	if err != nil || !ok {
		t.Fatalf("colors.conf missing: ok=%v err=%v", ok, err)
	}

	h := &handlers{q: query.New(s), store: s}
	h.recordStats(symbolsOut{Symbols: []store.SymbolHit{{File: "hypr/colors.conf", Name: "x"}}})

	st, _ := s.Stats()
	if st.Queries != 1 {
		t.Fatalf("queries = %d, want 1", st.Queries)
	}
	if st.BaselineBytes != f.Size {
		t.Fatalf("baseline = %d, want %d (size of colors.conf)", st.BaselineBytes, f.Size)
	}
	if st.AnswerBytes == 0 {
		t.Fatal("answer bytes should be non-zero")
	}
}
