package resolve

import (
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

// helper: build a graph with files, symbols, and IMPORTS edges.
func setupGraph() *graph.Graph {
	g := graph.New()

	// Files
	g.AddFile(graph.FileRecord{Path: "src/app.ts", Hash: "a1"})
	g.AddFile(graph.FileRecord{Path: "src/auth.ts", Hash: "a2"})
	g.AddFile(graph.FileRecord{Path: "src/db.ts", Hash: "a3"})
	g.AddFile(graph.FileRecord{Path: "src/utils.ts", Hash: "a4"})

	// Symbols in auth.ts
	g.AddSymbol(graph.Symbol{
		Name: "login", Kind: "function", FilePath: "src/auth.ts",
		IsExported: true,
	})
	g.AddSymbol(graph.Symbol{
		Name: "logout", Kind: "function", FilePath: "src/auth.ts",
		IsExported: true,
	})

	// Symbols in db.ts
	g.AddSymbol(graph.Symbol{
		Name: "query", Kind: "function", FilePath: "src/db.ts",
		IsExported: true,
	})

	// Symbols in utils.ts
	g.AddSymbol(graph.Symbol{
		Name: "format", Kind: "function", FilePath: "src/utils.ts",
		IsExported: true,
	})

	// Symbols in app.ts (self-file symbols)
	g.AddSymbol(graph.Symbol{
		Name: "handleRequest", Kind: "function", FilePath: "src/app.ts",
		IsExported: true,
	})

	// IMPORTS edges: app.ts imports auth.ts
	g.AddEdge(graph.Edge{
		SourcePath: "src/app.ts", TargetPath: "src/auth.ts", Type: "IMPORTS",
	})

	return g
}

func TestImportedCallResolvesWithHighConfidence(t *testing.T) {
	g := setupGraph()

	callsByFile := map[string][]graph.CallRef{
		"src/app.ts": {
			{CalleeName: "login", Line: 15},
		},
	}

	edges := ResolveCalls(g, callsByFile, nil)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SourcePath != "src/app.ts" {
		t.Errorf("expected source src/app.ts, got %s", e.SourcePath)
	}
	if e.TargetPath != "src/auth.ts" {
		t.Errorf("expected target src/auth.ts, got %s", e.TargetPath)
	}
	if e.Type != "CALLS" {
		t.Errorf("expected type CALLS, got %s", e.Type)
	}
	if e.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", e.Confidence)
	}
}

func TestSameFileCallSkipped(t *testing.T) {
	g := setupGraph()

	callsByFile := map[string][]graph.CallRef{
		"src/app.ts": {
			{CalleeName: "handleRequest", Line: 30},
		},
	}

	edges := ResolveCalls(g, callsByFile, nil)

	if len(edges) != 0 {
		t.Fatalf("expected 0 edges for same-file call, got %d", len(edges))
	}
}

func TestGlobalUniqueCallResolvesWithMediumConfidence(t *testing.T) {
	g := setupGraph()

	// app.ts does NOT import utils.ts, but calls "format" which is only in utils.ts.
	callsByFile := map[string][]graph.CallRef{
		"src/app.ts": {
			{CalleeName: "format", Line: 20},
		},
	}

	edges := ResolveCalls(g, callsByFile, nil)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.TargetPath != "src/utils.ts" {
		t.Errorf("expected target src/utils.ts, got %s", e.TargetPath)
	}
	if e.Confidence != 0.5 {
		t.Errorf("expected confidence 0.5, got %f", e.Confidence)
	}
}

func TestHeritageExtendsResolves(t *testing.T) {
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/base.ts", Hash: "b1"})
	g.AddFile(graph.FileRecord{Path: "src/child.ts", Hash: "b2"})

	g.AddSymbol(graph.Symbol{
		Name: "BaseService", Kind: "class", FilePath: "src/base.ts",
		IsExported: true,
	})
	g.AddSymbol(graph.Symbol{
		Name: "ChildService", Kind: "class", FilePath: "src/child.ts",
		IsExported: true,
	})

	heritageByFile := map[string][]graph.HeritageRef{
		"src/child.ts": {
			{ChildName: "ChildService", ParentName: "BaseService", Type: "extends"},
		},
	}

	edges := ResolveCalls(g, nil, heritageByFile)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.SourcePath != "src/child.ts" {
		t.Errorf("expected source src/child.ts, got %s", e.SourcePath)
	}
	if e.TargetPath != "src/base.ts" {
		t.Errorf("expected target src/base.ts, got %s", e.TargetPath)
	}
	if e.Type != "EXTENDS" {
		t.Errorf("expected type EXTENDS, got %s", e.Type)
	}
	if e.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", e.Confidence)
	}
}

func TestAmbiguousCallResolvesWithLowConfidence(t *testing.T) {
	g := setupGraph()

	// Add a second "format" symbol in db.ts so it's ambiguous
	g.AddSymbol(graph.Symbol{
		Name: "format", Kind: "function", FilePath: "src/db.ts",
		IsExported: true,
	})

	// app.ts does not import utils.ts or db.ts, calls "format" which exists in both
	callsByFile := map[string][]graph.CallRef{
		"src/app.ts": {
			{CalleeName: "format", Line: 20},
		},
	}

	edges := ResolveCalls(g, callsByFile, nil)

	if len(edges) != 2 {
		t.Fatalf("expected 2 ambiguous edges, got %d", len(edges))
	}
	for _, e := range edges {
		if e.Confidence != 0.3 {
			t.Errorf("expected confidence 0.3 for ambiguous match, got %f (target: %s)", e.Confidence, e.TargetPath)
		}
		if e.Type != "CALLS" {
			t.Errorf("expected type CALLS, got %s", e.Type)
		}
	}
}

func TestHeritageImplementsResolves(t *testing.T) {
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/iface.ts", Hash: "i1"})
	g.AddFile(graph.FileRecord{Path: "src/impl.ts", Hash: "i2"})

	g.AddSymbol(graph.Symbol{
		Name: "Serializable", Kind: "interface", FilePath: "src/iface.ts",
		IsExported: true,
	})
	g.AddSymbol(graph.Symbol{
		Name: "UserModel", Kind: "class", FilePath: "src/impl.ts",
		IsExported: true,
	})

	heritageByFile := map[string][]graph.HeritageRef{
		"src/impl.ts": {
			{ChildName: "UserModel", ParentName: "Serializable", Type: "implements"},
		},
	}

	edges := ResolveCalls(g, nil, heritageByFile)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.Type != "IMPLEMENTS" {
		t.Errorf("expected type IMPLEMENTS, got %s", e.Type)
	}
	if e.TargetPath != "src/iface.ts" {
		t.Errorf("expected target src/iface.ts, got %s", e.TargetPath)
	}
	if e.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", e.Confidence)
	}
}

func TestResolveCallsForFileSingleFile(t *testing.T) {
	g := graph.New()
	g.AddFile(graph.FileRecord{Path: "a.ts", Hash: "aaa"})
	g.AddFile(graph.FileRecord{Path: "b.ts", Hash: "bbb"})
	g.AddSymbol(graph.Symbol{FilePath: "b.ts", Name: "helper", Kind: "function", IsExported: true})
	g.AddEdge(graph.Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "IMPORTS"})

	calls := []graph.CallRef{{CalleeName: "helper", Line: 10}}
	heritage := []graph.HeritageRef{}

	edges := ResolveCallsForFile(g, "a.ts", calls, heritage)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].TargetPath != "b.ts" {
		t.Fatalf("expected target b.ts, got %s", edges[0].TargetPath)
	}
	if edges[0].Type != "CALLS" {
		t.Fatalf("expected CALLS edge, got %s", edges[0].Type)
	}
	if edges[0].Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9, got %f", edges[0].Confidence)
	}
}

func TestResolveCallsForFileHeritage(t *testing.T) {
	g := graph.New()
	g.AddFile(graph.FileRecord{Path: "a.ts", Hash: "aaa"})
	g.AddFile(graph.FileRecord{Path: "b.ts", Hash: "bbb"})
	g.AddSymbol(graph.Symbol{FilePath: "b.ts", Name: "BaseClass", Kind: "class", IsExported: true})

	calls := []graph.CallRef{}
	heritage := []graph.HeritageRef{{ChildName: "MyClass", ParentName: "BaseClass", Type: "extends"}}

	edges := ResolveCallsForFile(g, "a.ts", calls, heritage)

	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].Type != "EXTENDS" {
		t.Fatalf("expected EXTENDS, got %s", edges[0].Type)
	}
}

func TestDeduplication(t *testing.T) {
	g := setupGraph()

	// app.ts calls "login" and "logout" — both in auth.ts (which is imported).
	// Should produce only one CALLS edge from app.ts -> auth.ts.
	callsByFile := map[string][]graph.CallRef{
		"src/app.ts": {
			{CalleeName: "login", Line: 15},
			{CalleeName: "logout", Line: 20},
			{CalleeName: "login", Line: 25}, // duplicate call
		},
	}

	edges := ResolveCalls(g, callsByFile, nil)

	if len(edges) != 1 {
		t.Fatalf("expected 1 deduplicated edge, got %d", len(edges))
	}
	if edges[0].TargetPath != "src/auth.ts" {
		t.Errorf("expected target src/auth.ts, got %s", edges[0].TargetPath)
	}
}
