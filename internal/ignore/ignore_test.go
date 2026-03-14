package ignore

import (
	"os"
	"testing"
)

func TestHardcodedIgnores(t *testing.T) {
	tests := []struct {
		path   string
		expect bool
	}{
		{"node_modules/foo/bar.js", true},
		{".git/HEAD", true},
		{"dist/bundle.js", true},
		{".prowl/prowl.db", true},
		{"vendor/lib/util.go", true},
		{"__pycache__/mod.cpython.pyc", true},
		{".next/static/chunks/main.js", true},
		{"src/auth.ts", false},
		{"pkg/handler/auth.go", false},
		{"lib/utils.py", false},
		// binary extensions
		{"assets/logo.png", true},
		{"build/app.exe", true},
		{"data/model.wasm", true},
		// lock files
		{"package-lock.json", true},
		{"go.sum", true},
		// config files
		{".editorconfig", true},
		{".prettierrc", true},
		// minified/generated
		{"dist/app.min.js", true},
		{"types/generated.d.ts", true},
	}

	ig := New("")
	for _, tt := range tests {
		got := ig.ShouldIgnore(tt.path)
		if got != tt.expect {
			t.Errorf("ShouldIgnore(%q) = %v, want %v", tt.path, got, tt.expect)
		}
	}
}

func TestGitignoreIntegration(t *testing.T) {
	// Create a temp dir with a .gitignore
	dir := t.TempDir()
	gi := dir + "/.gitignore"
	os.WriteFile(gi, []byte("*.log\nsecrets/\n"), 0644)

	ig := New(dir)
	if !ig.ShouldIgnore("app.log") {
		t.Error("expected .gitignore pattern *.log to match")
	}
	if !ig.ShouldIgnore("secrets/keys.txt") {
		t.Error("expected .gitignore pattern secrets/ to match")
	}
	if ig.ShouldIgnore("src/main.go") {
		t.Error("src/main.go should not be ignored")
	}
}
