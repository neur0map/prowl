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
