package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveGoPackages(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetMeta("go_module", "example.com/proj"); err != nil {
		t.Fatal(err)
	}

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "go", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	st1 := mk("internal/store/store.go")
	st2 := mk("internal/store/queries.go")
	stTest := mk("internal/store/store_test.go") // never part of the imported surface
	cli := mk("internal/cli/run.go")

	// internal/cli/run.go imports package store (in-module) and fmt (stdlib).
	if err := s.ReplaceFileGraph(cli, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "example.com/proj/internal/store", Line: 3},
		{Kind: "includes", Raw: "fmt", Line: 4},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// The in-module import fans out to every file of package store via pkg edges.
	for _, dst := range []int64{st1, st2} {
		in, _ := s.IncomingEdges("file", dst, "pkg")
		if len(in) != 1 || in[0].File != "internal/cli/run.go" {
			t.Fatalf("pkg edge into store file %d = %+v, want one from internal/cli/run.go", dst, in)
		}
	}
	// The package's _test.go file is NOT part of the imported surface, so no
	// external importer gets a dependency edge into it (else its impact/hotspot
	// centrality would be falsely inflated to the package's whole in-degree).
	if in, _ := s.IncomingEdges("file", stTest, "pkg"); len(in) != 0 {
		t.Fatalf("pkg edge into a _test.go file = %+v, want none", in)
	}
	// Blast radius of any store file now reaches the importer.
	dep, _ := s.TransitiveDependents(st2)
	if len(dep) != 1 || dep[0].File != "internal/cli/run.go" {
		t.Fatalf("blast of queries.go = %+v, want internal/cli/run.go", dep)
	}
	// The stdlib import resolves to nothing and is not flagged as a pkg edge.
	dang, _ := s.UnresolvedEdges("includes")
	var fmtUnresolved bool
	for _, e := range dang {
		if e.Raw == "fmt" {
			fmtUnresolved = true
		}
	}
	if !fmtUnresolved {
		t.Fatalf("stdlib import fmt should remain unresolved, got %+v", dang)
	}
	// The in-module import is marked resolved (not dangling), so it is distinct
	// from stdlib and callees shows it as an internal dependency.
	for _, e := range dang {
		if e.Raw == "example.com/proj/internal/store" {
			t.Fatalf("in-module import should be resolved, still unresolved: %+v", e)
		}
	}
	// Resolving the import must not double-count callers: the store file has
	// exactly one incoming dependency edge from the importer (the pkg fan-out),
	// not also a resolved includes edge to the same file.
	if in, _ := s.IncomingEdges("file", st2, "includes", "pkg"); len(in) != 1 {
		t.Fatalf("store file incoming dep edges = %d, want 1 (pkg only, no double-count): %+v", len(in), in)
	}
}

func TestResolveGoPackagesNoModule(t *testing.T) {
	// With no go_module set, the Go pass is a no-op and creates no pkg edges.
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.UpsertFile(store.File{RelPath: "a.go", Lang: "go", Hash: "h", Size: 1, MTime: 1})
	if err := s.ReplaceFileGraph(id, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "x/y/z", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}
	dep, _ := s.TransitiveDependents(id)
	if len(dep) != 0 {
		t.Fatalf("no module: expected no pkg edges, got %+v", dep)
	}
}
