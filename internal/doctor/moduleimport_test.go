package doctor

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/store"
)

// TestDanglingSkipsModuleImports verifies that unresolved imports in code
// languages (Go stdlib/external packages) are not reported as broken project
// references, while a genuinely broken relative include in a config file still
// is. Without this, indexing a Go/TS/Rust project floods doctor with false
// positives and drops the score to 0.
func TestDanglingSkipsModuleImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	goFile, err := s.UpsertFile(store.File{RelPath: "main.go", Lang: "go", Hash: "h1", Size: 1, MTime: 1})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.UpsertFile(store.File{RelPath: "app.conf", Lang: "generic", Hash: "h2", Size: 1, MTime: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Go imports that look path-shaped (contain "/") but are external packages.
	must(t, s.ReplaceFileGraph(goFile, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "encoding/json", Line: 1},
		{Kind: "includes", Raw: "github.com/spf13/cobra", Line: 2},
	}, nil))
	// A real broken relative include in a config file.
	must(t, s.ReplaceFileGraph(cfg, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "missing/theme.conf", Line: 1},
	}, nil))

	if err := graph.Resolve(s); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(s, config.Rules{}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	var goDangling, cfgDangling int
	for _, f := range rep.Findings {
		if f.Check != "dangling_reference" {
			continue
		}
		switch f.File {
		case "main.go":
			goDangling++
		case "app.conf":
			cfgDangling++
		}
	}
	if goDangling != 0 {
		t.Errorf("Go module imports flagged as dangling (%d); should be skipped", goDangling)
	}
	if cfgDangling != 1 {
		t.Errorf("config broken include should still be flagged once, got %d", cfgDangling)
	}
}
