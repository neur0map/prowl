package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
