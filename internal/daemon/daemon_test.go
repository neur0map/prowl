package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/pipeline"
	"github.com/neur0map/prowl/internal/store"
)

func TestDaemonDetectsFileChange(t *testing.T) {
	// Create a temp project
	projDir := t.TempDir()
	os.MkdirAll(filepath.Join(projDir, "src"), 0755)
	os.WriteFile(filepath.Join(projDir, "src", "auth.ts"), []byte(`
export function login(): void {}
`), 0644)

	// Run initial index
	err := runIndex(projDir)
	if err != nil {
		t.Fatal(err)
	}

	// Start daemon
	d, err := New(projDir, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	go d.Run()
	defer d.Stop()

	// Wait for watcher to start
	time.Sleep(300 * time.Millisecond)

	// Modify the file — add a new export
	os.WriteFile(filepath.Join(projDir, "src", "auth.ts"), []byte(`
export function login(): void {}
export function logout(): void {}
`), 0644)

	// Wait for debounce + processing
	time.Sleep(2 * time.Second)

	// Check that .exports was updated
	exports, err := os.ReadFile(filepath.Join(projDir, ".prowl", "context", "src", "auth.ts", ".exports"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exports), "logout") {
		t.Errorf("expected .exports to contain 'logout' after update, got:\n%s", exports)
	}
}

func TestDaemonLoadsCallsEdgesOnStartup(t *testing.T) {
	dir := t.TempDir()
	prowlDir := filepath.Join(dir, ".prowl")
	os.MkdirAll(prowlDir, 0o755)

	// Pre-populate the store with a CALLS edge
	st, _ := store.Open(filepath.Join(prowlDir, "prowl.db"))
	fid, _ := st.UpsertFile("a.ts", "aaa")
	bid, _ := st.UpsertFile("b.ts", "bbb")
	st.InsertSymbols(fid, []graph.Symbol{{FilePath: "a.ts", Name: "foo", Kind: "function", StartLine: 1, EndLine: 5}})
	st.UpsertEdge(fid, bid, "IMPORTS")
	st.UpsertEdgeWithConfidence(fid, bid, "CALLS", 0.9)
	st.Close()

	d, err := New(dir, 1*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Stop()

	d.loadGraphFromStore()

	// Check CALLS edges are loaded
	callEdges := d.memGraph.EdgesFromFile("a.ts", "CALLS")
	if len(callEdges) != 1 {
		t.Fatalf("expected 1 CALLS edge, got %d", len(callEdges))
	}
	if callEdges[0].Confidence != 0.9 {
		t.Fatalf("expected confidence 0.9, got %f", callEdges[0].Confidence)
	}
}

func TestDaemonHandlesFileDeletion(t *testing.T) {
	dir := t.TempDir()

	// Create a source file
	srcDir := filepath.Join(dir, "src")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "a.ts"), []byte("export function foo() {}"), 0o644)

	// Index first
	pipeline.Index(dir)

	d, err := New(dir, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	d.loadGraphFromStore()

	// Verify file exists in graph
	_, ok := d.memGraph.File("src/a.ts")
	if !ok {
		t.Fatal("file should exist in graph after index")
	}

	// Simulate deletion
	d.deleteFile("src/a.ts")

	// File should be gone from graph
	_, ok = d.memGraph.File("src/a.ts")
	if ok {
		t.Fatal("file should be removed from graph after deletion")
	}

	// Context directory should be removed
	contextPath := filepath.Join(dir, ".prowl", "context", "src", "a.ts")
	if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
		t.Fatal("context directory should be removed")
	}

	d.Stop()
}
