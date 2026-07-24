package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/config"
	"github.com/prowl-agent/prowl-agent/internal/store"
)

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunInit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)

	sum, err := RunInit(InitOptions{Root: root, AI: false})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Indexed != 11 {
		t.Fatalf("indexed = %d, want 11", sum.Indexed)
	}

	// Workspace + index exist.
	if _, err := os.Stat(filepath.Join(root, ".prowl", "index.db")); err != nil {
		t.Fatalf("index.db missing: %v", err)
	}
	// .mcp.json registers prowl-agent.
	mcpData, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if err != nil || !strings.Contains(string(mcpData), "prowl-agent") {
		t.Fatalf(".mcp.json = %q err=%v", mcpData, err)
	}
	// AGENTS.md has the instruction block.
	agents, _ := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if !strings.Contains(string(agents), "<!-- prowl-agent -->") {
		t.Fatalf("AGENTS.md missing marker: %q", agents)
	}
	// .gitignore ignores .prowl/.
	gi, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if !strings.Contains(string(gi), ".prowl/") {
		t.Fatalf(".gitignore = %q", gi)
	}
	// Store actually populated.
	s, err := store.Open(filepath.Join(root, ".prowl", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c, _ := s.Counts(); c.Files != 11 {
		t.Fatalf("indexed files = %d, want 11", c.Files)
	}

	// init is idempotent.
	if _, err := RunInit(InitOptions{Root: root, AI: false}); err != nil {
		t.Fatalf("re-init: %v", err)
	}
}

func TestInjectMergePreservesServers(t *testing.T) {
	root := t.TempDir()
	existing := `{"mcpServers":{"other":{"command":"other","args":["x"]}}}`
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Inject(root); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, ".mcp.json"))
	got := string(data)
	if !strings.Contains(got, "prowl-agent") || !strings.Contains(got, "other") {
		t.Fatalf("merge lost a server: %s", got)
	}
	// Cursor and VS Code configs are written too.
	cur, _ := os.ReadFile(filepath.Join(root, ".cursor", "mcp.json"))
	if !strings.Contains(string(cur), "prowl-agent") {
		t.Fatalf(".cursor/mcp.json missing prowl-agent: %s", cur)
	}
	vsc, _ := os.ReadFile(filepath.Join(root, ".vscode", "mcp.json"))
	if !strings.Contains(string(vsc), "\"servers\"") || !strings.Contains(string(vsc), "prowl-agent") {
		t.Fatalf(".vscode/mcp.json wrong shape: %s", vsc)
	}
	// Oh My Pi, Factory droid, and OpenCode configs are written too.
	for _, p := range []string{
		filepath.Join(root, ".omp", "mcp.json"),
		filepath.Join(root, ".factory", "mcp.json"),
		filepath.Join(root, "opencode.json"),
	} {
		if d, _ := os.ReadFile(p); !strings.Contains(string(d), "prowl-agent") {
			t.Fatalf("%s missing prowl-agent: %s", p, d)
		}
	}
	// OpenCode uses its own shape: an `mcp` map with a local command array.
	oc, _ := os.ReadFile(filepath.Join(root, "opencode.json"))
	if !strings.Contains(string(oc), "\"mcp\"") || !strings.Contains(string(oc), "\"local\"") {
		t.Fatalf("opencode.json wrong shape: %s", oc)
	}
}

func TestRunInitPreservesAI(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)

	// First init explicitly enables AI.
	if _, err := RunInit(InitOptions{Root: root, AI: true, AISet: true, Tier: "fast"}); err != nil {
		t.Fatal(err)
	}
	// Re-init without an AI decision must NOT reset it to false (the bug).
	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, ".prowl"))
	if !cfg.AI.Enabled {
		t.Fatal("re-init disabled AI; it must stay enabled")
	}
}

func TestRunInitInheritsGlobalDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.SaveGlobal(config.GlobalConfig{AIEnabled: true, Tier: "fast"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)
	// New project, no explicit AI: inherits the global default (on).
	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, ".prowl"))
	if !cfg.AI.Enabled {
		t.Fatal("new project should inherit global ai_enabled=true")
	}
}

func TestRunInitExplicitNoAIOverridesGlobal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_ = config.SaveGlobal(config.GlobalConfig{AIEnabled: true, Tier: "fast"})
	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)
	if _, err := RunInit(InitOptions{Root: root, AI: false, AISet: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(filepath.Join(root, ".prowl"))
	if cfg.AI.Enabled {
		t.Fatal("explicit --no-ai must win over the global default")
	}
}
