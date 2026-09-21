package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveRustCrateImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "rust", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	store_ := mk("src/store.rs")
	mk("src/lib.rs")
	cli := mk("src/cli.rs")

	// cli uses crate::store (in-crate) and serde (external).
	if err := s.ReplaceFileGraph(cli, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "crate::store::Store", Line: 1},
		{Kind: "includes", Raw: "serde::Deserialize", Line: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// crate::store::Store resolved to the module file src/store.rs.
	if in, _ := s.IncomingEdges("file", store_, "includes"); len(in) != 1 || in[0].File != "src/cli.rs" {
		t.Fatalf("store.rs incoming = %+v, want one from cli.rs", in)
	}
	// External crate stays unresolved (informational).
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "serde::Deserialize" {
		t.Fatalf("unresolved = %+v, want only serde::Deserialize", dang)
	}
}

func TestResolveRustWorkspaceAndMod(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "rust", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// A Cargo workspace: crate "searcher" with lib.rs declaring `mod searcher;`
	// and the submodule using crate::sink.
	lib := mk("crates/searcher/src/lib.rs")
	sub := mk("crates/searcher/src/searcher.rs")
	sink := mk("crates/searcher/src/sink.rs")

	if err := s.ReplaceFileGraph(lib, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "mod::searcher", Line: 1},
		{Kind: "includes", Raw: "mod::sink", Line: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// crate:: resolves relative to the crate root (crates/searcher/src/), not repo root.
	if err := s.ReplaceFileGraph(sub, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "crate::sink::Sink", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// `mod searcher;` and `mod sink;` in lib.rs resolve to the sibling files.
	in, _ := s.IncomingEdges("file", sub, "includes")
	if len(in) != 1 || in[0].File != "crates/searcher/src/lib.rs" {
		t.Fatalf("searcher.rs incoming = %+v, want one from lib.rs (mod decl)", in)
	}
	// sink.rs is included by lib.rs (mod) and by searcher.rs (crate::sink).
	inSink, _ := s.IncomingEdges("file", sink, "includes")
	got := map[string]bool{}
	for _, e := range inSink {
		got[e.File] = true
	}
	if !got["crates/searcher/src/lib.rs"] || !got["crates/searcher/src/searcher.rs"] {
		t.Fatalf("sink.rs incoming = %+v, want lib.rs (mod) and searcher.rs (crate::)", inSink)
	}
}

func TestResolveRustCrossCrate(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "rust", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lib := mk("crates/searcher/src/lib.rs")
	mod := mk("crates/searcher/src/searcher.rs")
	printer := mk("crates/printer/src/standard.rs")

	// searcher's lib re-exports its searcher module (intra-crate).
	if err := s.ReplaceFileGraph(lib, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "mod::searcher", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// printer imports a type re-exported from the searcher crate (cross-crate).
	if err := s.ReplaceFileGraph(printer, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "grep_searcher::Searcher", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// The crate-name -> src map is recorded from Cargo.toml at index time.
	if err := s.SetMeta("rust_crates", `{"grep_searcher":"crates/searcher/src"}`); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// The re-exported type resolves to the searcher crate's lib.rs.
	in, _ := s.IncomingEdges("file", lib, "includes")
	var fromPrinter bool
	for _, e := range in {
		if e.File == "crates/printer/src/standard.rs" {
			fromPrinter = true
		}
	}
	if !fromPrinter {
		t.Fatalf("searcher lib.rs incoming = %+v, want a cross-crate edge from printer", in)
	}
	// Blast radius of the searcher module reaches the printer crate transitively.
	dep, _ := s.TransitiveDependents(mod)
	var hasPrinter bool
	for _, d := range dep {
		if d.File == "crates/printer/src/standard.rs" {
			hasPrinter = true
		}
	}
	if !hasPrinter {
		t.Fatalf("blast of searcher.rs = %+v, want crates/printer/src/standard.rs (cross-crate)", dep)
	}
}
