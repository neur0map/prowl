package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

func TestWriteContext(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/auth.ts", Hash: "a"})
	g.AddSymbol(graph.Symbol{
		Name: "handleLogin", Kind: "function", FilePath: "src/auth.ts",
		StartLine: 12, EndLine: 20, IsExported: true,
		Signature: "export function handleLogin(req: Request, res: Response): Promise<void>",
	})
	g.AddSymbol(graph.Symbol{
		Name: "AuthService", Kind: "class", FilePath: "src/auth.ts",
		StartLine: 25, EndLine: 40, IsExported: true,
		Signature: "export class AuthService",
	})

	g.AddFile(graph.FileRecord{Path: "src/login.ts", Hash: "b"})
	g.AddEdge(graph.Edge{SourcePath: "src/login.ts", TargetPath: "src/auth.ts", Type: "IMPORTS"})
	g.AddEdge(graph.Edge{SourcePath: "src/auth.ts", TargetPath: "src/db.ts", Type: "IMPORTS"})

	err := WriteContext(dir, g)
	if err != nil {
		t.Fatal(err)
	}

	// Check .exports
	exports := readFile(t, filepath.Join(dir, "src", "auth.ts", ".exports"))
	if !strings.Contains(exports, "func handleLogin (line 12)") {
		t.Errorf(".exports missing handleLogin, got:\n%s", exports)
	}
	if !strings.Contains(exports, "class AuthService (line 25)") {
		t.Errorf(".exports missing AuthService, got:\n%s", exports)
	}

	// Check .signatures
	sigs := readFile(t, filepath.Join(dir, "src", "auth.ts", ".signatures"))
	if !strings.Contains(sigs, "export function handleLogin") {
		t.Errorf(".signatures missing handleLogin sig, got:\n%s", sigs)
	}

	// Check .imports
	imports := readFile(t, filepath.Join(dir, "src", "auth.ts", ".imports"))
	if !strings.Contains(imports, "src/db.ts") {
		t.Errorf(".imports missing src/db.ts, got:\n%s", imports)
	}

	// Check .upstream (reverse imports)
	upstream := readFile(t, filepath.Join(dir, "src", "auth.ts", ".upstream"))
	if !strings.Contains(upstream, "src/login.ts") {
		t.Errorf(".upstream missing src/login.ts, got:\n%s", upstream)
	}

	// Check _meta/index.txt
	index := readFile(t, filepath.Join(dir, "_meta", "index.txt"))
	if !strings.Contains(index, "src/auth.ts") {
		t.Errorf("_meta/index.txt missing src/auth.ts")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}
