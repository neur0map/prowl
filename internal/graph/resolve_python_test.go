package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolvePythonAbsoluteImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "python", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	services := mk("myapp/services.py")
	api := mk("myapp/api.py")
	mk("main.py")

	// api imports myapp.services (in-project) and os (stdlib).
	if err := s.ReplaceFileGraph(api, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "myapp.services", Line: 1},
		{Kind: "includes", Raw: "os", Line: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// main imports myapp.api.
	mainID, _ := s.FileID("main.py")
	if err := s.ReplaceFileGraph(mainID, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "myapp.api", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// myapp.services resolved to myapp/services.py.
	if in, _ := s.IncomingEdges("file", services, "includes"); len(in) != 1 || in[0].File != "myapp/api.py" {
		t.Fatalf("services incoming = %+v, want one from myapp/api.py", in)
	}
	// Transitive blast radius reaches main.py via api.py.
	dep, _ := s.TransitiveDependents(services)
	var hasMain bool
	for _, d := range dep {
		if d.File == "main.py" {
			hasMain = true
		}
	}
	if !hasMain {
		t.Fatalf("blast of services.py = %+v, want main.py (transitive)", dep)
	}
	// stdlib os stays unresolved.
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "os" {
		t.Fatalf("unresolved = %+v, want only os", dang)
	}
}

func TestResolvePythonRelativeImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "python", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	services := mk("myapp/services.py")
	api := mk("myapp/api.py")
	deep := mk("myapp/sub/deep.py")

	// api: from .services import serve (same package)
	if err := s.ReplaceFileGraph(api, nil, nil, []store.RawEdge{{Kind: "includes", Raw: ".services", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	// deep: from ..services import serve (parent package)
	if err := s.ReplaceFileGraph(deep, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "..services", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}
	in, _ := s.IncomingEdges("file", services, "includes")
	if len(in) != 2 {
		t.Fatalf("services incoming = %d (%+v), want 2 (from .services and ..services)", len(in), in)
	}
	files := map[string]bool{in[0].File: true, in[1].File: true}
	if !files["myapp/api.py"] || !files["myapp/sub/deep.py"] {
		t.Fatalf("relative imports resolved to %+v, want api.py and sub/deep.py", files)
	}
}
