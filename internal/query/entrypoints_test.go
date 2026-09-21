package query

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

// overviewOf indexes a literal file tree and returns its overview, so an
// entrypoint assertion runs against real parsed dependency edges rather than a
// hand-built edge table that could drift from what the indexer produces.
func overviewOf(t *testing.T, files map[string]string) Overview {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	database, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := index.Index(database, root, nil); err != nil {
		t.Fatal(err)
	}
	overview, err := New(database).OverviewContext(context.Background(), DefaultOverviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	return overview
}

// The entrypoint list is the first orientation an agent gets from a repo, so it
// has to name files that actually start the program. "Has outgoing edges and no
// incoming ones" alone reported manifests and test harnesses instead: a
// package.json carries an edge per dependency, nothing imports a test, and a
// unit test importing the real entry point hid that entry point entirely.
func TestOverviewEntrypointsNameCodeThatStartsTheProgram(t *testing.T) {
	overview := overviewOf(t, map[string]string{
		"app.py":            "import lib\n\ndef main():\n    return lib.helper()\n",
		"lib.py":            "def helper():\n    return 1\n",
		"tests/test_app.py": "import app\n\ndef test_main():\n    assert app.main() == 1\n",
		"manifest.json":     `{"main": "app.py", "include": ["lib.py"]}`,
	})
	if got := overview.Entrypoints; len(got) != 1 || got[0] != "app.py" {
		t.Fatalf("entrypoints = %v, want [app.py]", got)
	}
	if overview.EntrypointCount != 1 {
		t.Errorf("entrypoint_count = %d, want 1 (the count must match what qualifies)", overview.EntrypointCount)
	}
}

// A data file is never an entry point even when it is the only file carrying
// dependency edges; reporting one is worse than reporting none.
func TestOverviewEntrypointsExcludeDataLanguages(t *testing.T) {
	overview := overviewOf(t, map[string]string{
		"package.json": `{"dependencies": {"left-pad": "1.0.0"}, "main": "lib.js"}`,
		"lib.js":       "export const helper = () => 1;\n",
	})
	for _, entry := range overview.Entrypoints {
		if entry == "package.json" {
			t.Fatalf("manifest reported as an entrypoint: %v", overview.Entrypoints)
		}
	}
}

// Config dialects stay eligible: in a dotfiles or theming repo the file that
// sources every other config really is that project's entry point.
func TestExecutableLangKeepsConfigDialectsEligible(t *testing.T) {
	for _, lang := range []string{"hyprlang", "ini", "css", "go", "python", "bash", ""} {
		if !executableLang(lang) {
			t.Errorf("executableLang(%q) = false, want eligible", lang)
		}
	}
	for _, lang := range []string{"json", "yaml", "toml", "markdown"} {
		if executableLang(lang) {
			t.Errorf("executableLang(%q) = true, want excluded", lang)
		}
	}
}

// Survivors are ordered by how much each pulls in, so the entry that reaches
// most of the codebase is read first.
func TestOverviewEntrypointsRankByOutgoingBreadth(t *testing.T) {
	overview := overviewOf(t, map[string]string{
		"small.py": "import one\n",
		"big.py":   "import one\nimport two\nimport three\n",
		"one.py":   "x = 1\n",
		"two.py":   "y = 2\n",
		"three.py": "z = 3\n",
	})
	if len(overview.Entrypoints) < 2 {
		t.Fatalf("entrypoints = %v, want both roots", overview.Entrypoints)
	}
	if overview.Entrypoints[0] != "big.py" {
		t.Errorf("entrypoints = %v, want big.py first (it pulls in more)", overview.Entrypoints)
	}
}
