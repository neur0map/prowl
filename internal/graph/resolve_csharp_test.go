package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveCSharpNamespaces(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "csharp", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	user := mk("App/Models/User.cs")
	store_ := mk("App/Services/Store.cs")
	prog := mk("App/Program.cs")

	// User.cs declares namespace App.Models.
	if err := s.ReplaceFileGraph(user, nil, []store.Resource{
		{Kind: "namespace", Name: "App.Models", Line: 1},
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Store.cs declares App.Services and uses App.Models plus the System framework.
	if err := s.ReplaceFileGraph(store_, nil, []store.Resource{
		{Kind: "namespace", Name: "App.Services", Line: 2},
	}, []store.RawEdge{
		{Kind: "includes", Raw: "App.Models", Line: 1},
		{Kind: "includes", Raw: "System", Line: 0},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Program.cs uses App.Services.
	if err := s.ReplaceFileGraph(prog, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "App.Services", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// Store.cs depends on User.cs (via using App.Models): one synthetic pkg edge.
	in, _ := s.IncomingEdges("file", user, "pkg")
	if len(in) != 1 || in[0].File != "App/Services/Store.cs" {
		t.Fatalf("User.cs callers = %+v, want one from Store.cs", in)
	}
	// Program.cs depends on Store.cs (via using App.Services).
	inStore, _ := s.IncomingEdges("file", store_, "pkg")
	if len(inStore) != 1 || inStore[0].File != "App/Program.cs" {
		t.Fatalf("Store.cs callers = %+v, want one from Program.cs", inStore)
	}
	// Program declares namespace App, which nothing imports: no pkg edge to it.
	// The System framework using matches no declared namespace, so it produces
	// no pkg edge either (nsFiles["System"] is empty).
	if inProg, _ := s.IncomingEdges("file", prog, "pkg"); len(inProg) != 0 {
		t.Fatalf("Program.cs callers = %+v, want none", inProg)
	}
	// Transitive blast radius of User.cs reaches Program.cs via Store.cs.
	dep, _ := s.TransitiveDependents(user)
	var hasProg bool
	for _, d := range dep {
		if d.File == "App/Program.cs" {
			hasProg = true
		}
	}
	if !hasProg {
		t.Fatalf("blast of User.cs = %+v, want App/Program.cs (transitive)", dep)
	}
}
