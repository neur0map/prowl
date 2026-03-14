package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexEndToEnd(t *testing.T) {
	// Create a temp project with TS files
	projDir := t.TempDir()

	os.MkdirAll(filepath.Join(projDir, "src"), 0755)
	os.WriteFile(filepath.Join(projDir, "src", "auth.ts"), []byte(`
export function handleLogin(req: Request): void {}
export class AuthService {}
`), 0644)

	os.WriteFile(filepath.Join(projDir, "src", "login.ts"), []byte(`
import { handleLogin } from './auth';
export function showLoginPage(): void { handleLogin(req); }
`), 0644)

	// Also create a node_modules file that should be ignored
	os.MkdirAll(filepath.Join(projDir, "node_modules", "foo"), 0755)
	os.WriteFile(filepath.Join(projDir, "node_modules", "foo", "index.js"), []byte("module.exports = {}"), 0644)

	err := Index(projDir)
	if err != nil {
		t.Fatal(err)
	}

	contextDir := filepath.Join(projDir, ".prowl", "context")

	// auth.ts should have exports
	exports, err := os.ReadFile(filepath.Join(contextDir, "src", "auth.ts", ".exports"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exports), "handleLogin") {
		t.Errorf("missing handleLogin in .exports:\n%s", exports)
	}

	// login.ts imports auth.ts
	imports, err := os.ReadFile(filepath.Join(contextDir, "src", "login.ts", ".imports"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(imports), "src/auth.ts") {
		t.Errorf("missing src/auth.ts in .imports:\n%s", imports)
	}

	// auth.ts should have login.ts as upstream
	upstream, err := os.ReadFile(filepath.Join(contextDir, "src", "auth.ts", ".upstream"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upstream), "src/login.ts") {
		t.Errorf("missing src/login.ts in .upstream:\n%s", upstream)
	}

	// node_modules should NOT be indexed
	_, err = os.Stat(filepath.Join(contextDir, "node_modules"))
	if err == nil {
		t.Error("node_modules should not be in context output")
	}

	// SQLite DB should exist
	dbPath := filepath.Join(projDir, ".prowl", "prowl.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Error("prowl.db should exist")
	}
}
