package graph

import "testing"

func TestAddFileAndSymbols(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "src/auth.ts", Hash: "abc123"})
	g.AddSymbol(Symbol{
		Name: "login", Kind: "function", FilePath: "src/auth.ts",
		StartLine: 10, EndLine: 20, IsExported: true,
		Signature: "func login(user: string): Promise<void>",
	})

	files := g.Files()
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	syms := g.SymbolsForFile("src/auth.ts")
	if len(syms) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(syms))
	}
	if syms[0].Name != "login" {
		t.Errorf("expected symbol name 'login', got %q", syms[0].Name)
	}
}

func TestAddEdgeAndUpstream(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "src/auth.ts", Hash: "a"})
	g.AddFile(FileRecord{Path: "src/login.ts", Hash: "b"})
	g.AddEdge(Edge{SourcePath: "src/login.ts", TargetPath: "src/auth.ts", Type: "IMPORTS"})

	imports := g.ImportsOf("src/login.ts")
	if len(imports) != 1 || imports[0] != "src/auth.ts" {
		t.Errorf("expected imports [src/auth.ts], got %v", imports)
	}

	upstream := g.UpstreamOf("src/auth.ts")
	if len(upstream) != 1 || upstream[0] != "src/login.ts" {
		t.Errorf("expected upstream [src/login.ts], got %v", upstream)
	}
}

func TestReplaceFile(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "src/auth.ts", Hash: "a"})
	g.AddSymbol(Symbol{Name: "old", Kind: "function", FilePath: "src/auth.ts"})
	g.AddFile(FileRecord{Path: "src/login.ts", Hash: "b"})
	g.AddEdge(Edge{SourcePath: "src/auth.ts", TargetPath: "src/login.ts", Type: "IMPORTS"})

	// Replace clears old symbols and edges
	g.ReplaceFile("src/auth.ts", FileRecord{Path: "src/auth.ts", Hash: "c"})

	syms := g.SymbolsForFile("src/auth.ts")
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols after replace, got %d", len(syms))
	}
	imports := g.ImportsOf("src/auth.ts")
	if len(imports) != 0 {
		t.Errorf("expected 0 imports after replace, got %d", len(imports))
	}
}

func TestEdgesOfType(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "1"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "2"})
	g.AddFile(FileRecord{Path: "c.ts", Hash: "3"})

	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "IMPORTS"})
	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "c.ts", Type: "CALLS", Confidence: 0.9})
	g.AddEdge(Edge{SourcePath: "b.ts", TargetPath: "c.ts", Type: "IMPORTS"})

	imports := g.EdgesOfType("IMPORTS")
	if len(imports) != 2 {
		t.Errorf("expected 2 IMPORTS edges, got %d", len(imports))
	}

	calls := g.EdgesOfType("CALLS")
	if len(calls) != 1 {
		t.Errorf("expected 1 CALLS edge, got %d", len(calls))
	}
	if calls[0].Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", calls[0].Confidence)
	}

	extends := g.EdgesOfType("EXTENDS")
	if len(extends) != 0 {
		t.Errorf("expected 0 EXTENDS edges, got %d", len(extends))
	}
}

func TestAllSymbols(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "1"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "2"})

	g.AddSymbol(Symbol{Name: "foo", Kind: "function", FilePath: "a.ts"})
	g.AddSymbol(Symbol{Name: "bar", Kind: "class", FilePath: "a.ts"})
	g.AddSymbol(Symbol{Name: "baz", Kind: "function", FilePath: "b.ts"})

	all := g.AllSymbols()
	if len(all) != 3 {
		t.Fatalf("expected 3 symbols, got %d", len(all))
	}

	names := make(map[string]bool)
	for _, s := range all {
		names[s.Name] = true
	}
	for _, want := range []string{"foo", "bar", "baz"} {
		if !names[want] {
			t.Errorf("expected symbol %q in AllSymbols()", want)
		}
	}
}

func TestCommunitySetAndGet(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "1"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "2"})

	g.SetCommunity("a.ts", CommunityInfo{ID: 0, Name: "core", Label: "Core Module"})
	g.SetCommunity("b.ts", CommunityInfo{ID: 1, Name: "utils", Label: "Utilities"})

	c, ok := g.CommunityOf("a.ts")
	if !ok {
		t.Fatal("expected community for a.ts")
	}
	if c.ID != 0 || c.Name != "core" {
		t.Errorf("unexpected community for a.ts: %+v", c)
	}

	_, ok = g.CommunityOf("nonexistent.ts")
	if ok {
		t.Error("expected no community for nonexistent file")
	}
}

func TestAllCommunities(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "1"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "2"})
	g.AddFile(FileRecord{Path: "c.ts", Hash: "3"})

	c0 := CommunityInfo{ID: 0, Name: "core", Label: "Core"}
	c1 := CommunityInfo{ID: 1, Name: "utils", Label: "Utils"}

	g.SetCommunity("a.ts", c0)
	g.SetCommunity("b.ts", c0) // same community as a.ts
	g.SetCommunity("c.ts", c1)

	all := g.AllCommunities()
	if len(all) != 2 {
		t.Fatalf("expected 2 unique communities, got %d", len(all))
	}
	if all[0].Name != "core" {
		t.Errorf("expected community 0 name 'core', got %q", all[0].Name)
	}
	if all[1].Name != "utils" {
		t.Errorf("expected community 1 name 'utils', got %q", all[1].Name)
	}
}

func TestEdgesFromFile(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "aaa"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "bbb"})
	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "IMPORTS"})
	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "CALLS", Confidence: 0.9})

	imports := g.EdgesFromFile("a.ts", "IMPORTS")
	if len(imports) != 1 {
		t.Fatalf("expected 1 IMPORTS edge, got %d", len(imports))
	}
	calls := g.EdgesFromFile("a.ts", "CALLS")
	if len(calls) != 1 || calls[0].Confidence != 0.9 {
		t.Fatal("expected 1 CALLS edge with confidence 0.9")
	}
	// Empty type returns all edges from file
	all := g.EdgesFromFile("a.ts", "")
	if len(all) != 2 {
		t.Fatalf("expected 2 edges with empty type filter, got %d", len(all))
	}
}

func TestRemoveFile(t *testing.T) {
	g := New()
	g.AddFile(FileRecord{Path: "a.ts", Hash: "aaa"})
	g.AddFile(FileRecord{Path: "b.ts", Hash: "bbb"})
	g.AddSymbol(Symbol{FilePath: "a.ts", Name: "foo", Kind: "function"})
	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "IMPORTS"})
	g.AddEdge(Edge{SourcePath: "a.ts", TargetPath: "b.ts", Type: "CALLS", Confidence: 0.9})
	g.AddEdge(Edge{SourcePath: "b.ts", TargetPath: "a.ts", Type: "CALLS", Confidence: 0.5})
	g.SetCommunity("a.ts", CommunityInfo{ID: 1, Name: "test"})

	g.RemoveFile("a.ts")

	_, ok := g.File("a.ts")
	if ok {
		t.Fatal("file a.ts should be removed")
	}
	if len(g.SymbolsForFile("a.ts")) != 0 {
		t.Fatal("symbols for a.ts should be empty")
	}
	if len(g.EdgesFromFile("a.ts", "IMPORTS")) != 0 {
		t.Fatal("outgoing IMPORTS from a.ts should be gone")
	}
	if len(g.EdgesFromFile("a.ts", "CALLS")) != 0 {
		t.Fatal("outgoing CALLS from a.ts should be gone")
	}
	for _, e := range g.EdgesFromFile("b.ts", "CALLS") {
		if e.TargetPath == "a.ts" {
			t.Fatal("incoming CALLS edge from b.ts to a.ts should be removed")
		}
	}
	_, hasCommunity := g.CommunityOf("a.ts")
	if hasCommunity {
		t.Fatal("community for a.ts should be cleared")
	}
	for _, u := range g.UpstreamOf("b.ts") {
		if u == "a.ts" {
			t.Fatal("a.ts should not appear in upstream of b.ts")
		}
	}
}
