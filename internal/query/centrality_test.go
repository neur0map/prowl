package query

import (
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestFileCentralityRanksHub(t *testing.T) {
	paths := []string{"hub.go", "a.go", "b.go", "c.go", "leaf.go"}
	edges := []store.FileEdge{
		{SrcFile: "a.go", DstFile: "hub.go"},
		{SrcFile: "b.go", DstFile: "hub.go"},
		{SrcFile: "c.go", DstFile: "hub.go"},
		{SrcFile: "a.go", DstFile: "leaf.go"},
	}
	score, degree := fileCentrality(paths, edges)
	for _, p := range []string{"a.go", "b.go", "c.go", "leaf.go"} {
		if score["hub.go"] <= score[p] {
			t.Fatalf("hub score %.5f not highest (vs %s %.5f)", score["hub.go"], p, score[p])
		}
	}
	if degree["hub.go"] != 3 {
		t.Fatalf("hub in-degree=%d want 3", degree["hub.go"])
	}
	if degree["leaf.go"] != 1 {
		t.Fatalf("leaf in-degree=%d want 1", degree["leaf.go"])
	}
	// Self-loops and edges to unknown nodes are ignored, not counted.
	score2, degree2 := fileCentrality([]string{"x.go"}, []store.FileEdge{
		{SrcFile: "x.go", DstFile: "x.go"},
		{SrcFile: "x.go", DstFile: "ghost.go"},
	})
	if score2["x.go"] <= 0 {
		t.Fatalf("single-node score should be positive, got %v", score2["x.go"])
	}
	if degree2["x.go"] != 0 {
		t.Fatalf("self/ghost edges must not add in-degree, got %d", degree2["x.go"])
	}
}

func TestCentralFilesExcludesVendored(t *testing.T) {
	files := []store.File{
		{RelPath: "app/core.go"},
		{RelPath: "vendor/lib/x.go"},
		{RelPath: "app/util.go"},
	}
	edges := []store.FileEdge{
		{SrcFile: "app/util.go", DstFile: "app/core.go"},
		{SrcFile: "vendor/lib/x.go", DstFile: "app/core.go"},
	}
	central := centralFiles(files, edges, 10)
	for _, r := range central {
		if r.File == "vendor/lib/x.go" {
			t.Fatalf("vendored file should be excluded: %+v", central)
		}
	}
	if len(central) == 0 || central[0].File != "app/core.go" {
		t.Fatalf("central[0]=%+v want app/core.go", central)
	}
}
