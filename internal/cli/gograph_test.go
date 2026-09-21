package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

// TestGoGraphPipeline verifies the full Go path: init detects the module from
// go.mod, indexing resolves an in-module import to the imported package's files,
// and impact/callers report the dependency across packages.
func TestGoGraphPipeline(t *testing.T) {
	root := t.TempDir()
	writeFile := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("go.mod", "module example.com/proj\n\ngo 1.22\n")
	writeFile("store/store.go", "package store\n\ntype Store struct{}\n\nfunc Open() *Store { return &Store{} }\n")
	writeFile("cli/run.go", "package cli\n\nimport \"example.com/proj/store\"\n\nfunc Run() { _ = store.Open() }\n")

	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}

	ws, err := workspace.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ws.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q := query.New(s)

	contains := func(deps []store.Dep, file string) bool {
		for _, d := range deps {
			if d.File == file {
				return true
			}
		}
		return false
	}

	impacted, err := q.BlastRadius("store/store.go")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(impacted, "cli/run.go") {
		t.Fatalf("impact of store/store.go = %+v, want cli/run.go (imports package store)", impacted)
	}

	callers, err := q.FindCallers("store/store.go")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range callers {
		if c.File == "cli/run.go" && c.Kind == "pkg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("callers of store/store.go = %+v, want a pkg edge from cli/run.go", callers)
	}
}
