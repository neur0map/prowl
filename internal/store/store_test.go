package store

import (
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert a file
	fid, err := s.UpsertFile("src/auth.ts", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if fid == 0 {
		t.Fatal("expected non-zero file ID")
	}

	// Insert symbols
	err = s.InsertSymbols(fid, []graph.Symbol{
		{Name: "login", Kind: "function", StartLine: 10, EndLine: 20, IsExported: true, Signature: "func login()"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert another file and an edge
	fid2, _ := s.UpsertFile("src/login.ts", "hash2")
	err = s.UpsertEdge(fid2, fid, "IMPORTS")
	if err != nil {
		t.Fatal(err)
	}

	// Query upstream
	upstream, err := s.UpstreamOf("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(upstream) != 1 || upstream[0] != "src/login.ts" {
		t.Errorf("expected upstream [src/login.ts], got %v", upstream)
	}

	// Query file hash
	hash, err := s.FileHash("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash1" {
		t.Errorf("expected hash 'hash1', got %q", hash)
	}
}

func TestDeleteFileSymbolsCascade(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid, _ := s.UpsertFile("src/auth.ts", "h")
	s.InsertSymbols(fid, []graph.Symbol{
		{Name: "login", Kind: "function", StartLine: 1, EndLine: 5},
	})

	err = s.DeleteSymbolsForFile(fid)
	if err != nil {
		t.Fatal(err)
	}

	syms, _ := s.SymbolsForFile("src/auth.ts")
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols after delete, got %d", len(syms))
	}
}
