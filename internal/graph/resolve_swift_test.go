package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// Swift has no file-level local imports; a file's dependencies are the project
// types it references. A `uses TypeName` edge must resolve to the file declaring
// that struct/class/enum/protocol, so impact/callers/clusters work across the
// app, while framework types (SwiftUI's View/Text) resolve to nothing and drop.
func TestResolveSwiftTypeUsage(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "swift", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mgr := mk("deskmon/Services/ServerManager.swift")
	model := mk("deskmon/Models/ServerStats.swift")
	view := mk("deskmon/Views/DashboardView.swift")

	if err := s.ReplaceFileGraph(mgr, []store.Symbol{{Name: "ServerManager", Kind: "class", StartLine: 1, EndLine: 5}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(model, []store.Symbol{{Name: "ServerStats", Kind: "struct", StartLine: 1, EndLine: 3}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(view, []store.Symbol{{Name: "DashboardView", Kind: "struct", StartLine: 1, EndLine: 10}}, nil, []store.RawEdge{
		{Kind: "uses", Raw: "ServerManager", Line: 2},
		{Kind: "uses", Raw: "ServerStats", Line: 3},
		{Kind: "uses", Raw: "View", Line: 1},
		{Kind: "uses", Raw: "Text", Line: 5},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id  int64
		rel string
	}{{mgr, "ServerManager.swift"}, {model, "ServerStats.swift"}} {
		in, _ := s.IncomingEdges("file", tc.id, "uses")
		if len(in) != 1 || in[0].File != "deskmon/Views/DashboardView.swift" {
			t.Fatalf("%s incoming = %+v, want one from DashboardView.swift", tc.rel, in)
		}
	}
	if dang, _ := s.UnresolvedEdges("uses"); len(dang) != 0 {
		t.Fatalf("unresolved uses = %+v, want none (framework types dropped)", dang)
	}
}
