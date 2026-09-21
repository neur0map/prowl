package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestSubsystemOf(t *testing.T) {
	cases := map[string]string{
		"internal/cli/graph.go": "internal/cli",
		"a/b/c.go":              "a/b",
		"top.go":                "(root)",
	}
	for in, want := range cases {
		if got := subsystemOf(in); got != want {
			t.Errorf("subsystemOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildGraphDataAndRender(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	up := func(p string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: p, Lang: "go", Hash: p, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, c := up("a.go"), up("b.go"), up("c.go")
	// a -> b, a -> a (self, must be dropped), b -> c.
	if err := s.ReplaceFileGraph(a, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "b.go", Line: 1}, {Kind: "includes", Raw: "a.go", Line: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(b, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "c.go", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	target := map[string]int64{"a.go": a, "b.go": b, "c.go": c}
	for _, src := range []int64{a, b} {
		es, _ := s.EdgesFromFile(src, "includes")
		for _, e := range es {
			if dst, ok := target[e.Raw]; ok {
				if err := s.SetEdgeResolved(e.ID, "file", dst); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	data, err := buildGraphData(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (%+v)", len(data.Nodes), data.Nodes)
	}
	// a->b and b->c; the a->a self edge is dropped.
	if len(data.Links) != 2 {
		t.Fatalf("links = %d, want 2 (self-loop excluded): %+v", len(data.Links), data.Links)
	}
	html, err := renderGraphHTML("myrepo", data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "/*PROWL_DATA*/") || strings.Contains(html, "/*PROWL_TITLE*/") {
		t.Error("template placeholders not replaced")
	}
	for _, want := range []string{"myrepo", "\"a.go\"", "const DATA", "<canvas"} {
		if !strings.Contains(html, want) {
			t.Errorf("html missing %q", want)
		}
	}
}

func TestBuildGraphDataIncludesIsolatedFiles(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	up := func(p string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: p, Lang: "go", Hash: p, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b := up("a.go"), up("b.go")
	up("lonely.go") // no edges: a standalone file that must still appear
	if err := s.ReplaceFileGraph(a, nil, nil, []store.RawEdge{{Kind: "includes", Raw: "b.go", Line: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	es, _ := s.EdgesFromFile(a, "includes")
	for _, e := range es {
		if e.Raw == "b.go" {
			if err := s.SetEdgeResolved(e.ID, "file", b); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := buildGraphData(s)
	if err != nil {
		t.Fatal(err)
	}
	// Every indexed file is a node -- including the one with no dependencies --
	// so the header count matches the repo, and Total reflects all files.
	if len(data.Nodes) != 3 || data.Total != 3 {
		t.Fatalf("nodes=%d total=%d, want 3/3 (isolated file must appear): %+v", len(data.Nodes), data.Total, data.Nodes)
	}
	var foundLonely bool
	for _, n := range data.Nodes {
		if n.Path == "lonely.go" {
			foundLonely = true
			if n.Deg != 0 {
				t.Errorf("lonely.go degree = %d, want 0", n.Deg)
			}
		}
	}
	if !foundLonely {
		t.Error("standalone file lonely.go missing from graph")
	}
}
