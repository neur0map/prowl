package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/setup"
	"github.com/neur0map/prowl/internal/store"
)

func sampleOverview() query.Overview {
	return query.Overview{
		Counts: store.Counts{
			Files: 3, Symbols: 10, Edges: 5, Resolved: 3, External: 2, Unresolved: 0,
			Langs: map[string]int{"go": 3, "markdown": 1},
		},
		Clusters:        []query.ClusterSummary{{Label: "internal/core", Lang: "go", Files: 5}},
		Entrypoints:     []string{"cmd/app/main.go", "internal/core/core_test.go", "internal/util/leaf.go"},
		EntrypointCount: 3,
		Hotspots:        []store.FanRow{{File: "internal/core/store.go", In: 9}},
		Docs:            []string{"README.md", "docs/ARCHITECTURE.md"},
	}
}

func TestProjectMapBlock(t *testing.T) {
	out := projectMapBlock(sampleOverview())
	for _, want := range []string{
		agentsMapMarker, agentsMapEndMarker,
		"3 files, 10 symbols", "external deps 2", "unresolved 0",
		"go:3", "internal/core(5,go)", "cmd/app/main.go", "README.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("map missing %q:\n%s", want, out)
		}
	}
	// Test files are not useful entrypoints and must be filtered out.
	if strings.Contains(out, "core_test.go") {
		t.Errorf("test file leaked into entrypoints:\n%s", out)
	}
}

func TestRefreshAgentsMapInsertsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	guidance := "# My repo\n\n" + setup.AgentsMarker + "\nProwl guidance here.\n" + setup.AgentsEndMarker + "\n"
	if err := os.WriteFile(path, []byte(guidance), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refreshAgentsMap(dir, sampleOverview()); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if strings.Count(string(first), agentsMapMarker) != 1 {
		t.Fatalf("expected exactly one map block:\n%s", first)
	}
	if !strings.Contains(string(first), "Prowl guidance here.") {
		t.Error("refresh clobbered the guidance block")
	}
	// A second refresh must replace, not duplicate.
	if err := refreshAgentsMap(dir, sampleOverview()); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if strings.Count(string(second), agentsMapMarker) != 1 {
		t.Fatalf("map block duplicated on re-run:\n%s", second)
	}
}

func TestRefreshAgentsMapNoOps(t *testing.T) {
	// No AGENTS.md at all: no error, no file created.
	dir := t.TempDir()
	if err := refreshAgentsMap(dir, sampleOverview()); err != nil {
		t.Fatalf("err on missing AGENTS.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Error("refresh created AGENTS.md where none existed")
	}
	// AGENTS.md without a Prowl block: left untouched (repo did not opt in).
	dir2 := t.TempDir()
	p2 := filepath.Join(dir2, "AGENTS.md")
	os.WriteFile(p2, []byte("# hand-written, no prowl\n"), 0o644)
	if err := refreshAgentsMap(dir2, sampleOverview()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p2)
	if strings.Contains(string(got), agentsMapMarker) {
		t.Errorf("map written into a non-Prowl AGENTS.md:\n%s", got)
	}
}
