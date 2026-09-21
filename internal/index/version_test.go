package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// A binary upgrade (changed index version) must force a full re-parse, so extractor
// and resolver fixes take effect instead of incremental hashing skipping unchanged
// files and serving stale data.
func TestVersionChangeForcesReindex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.lua"), []byte("local x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}
	// Same version, unchanged file: skipped.
	if sum, _ := Index(s, dir, nil); sum.Parsed != 0 || sum.Skipped != 1 {
		t.Fatalf("same version: parsed=%d skipped=%d, want 0/1", sum.Parsed, sum.Skipped)
	}
	// Simulate an upgrade: stored version no longer matches the binary's.
	if err := s.SetMeta("index_version", "older-build"); err != nil {
		t.Fatal(err)
	}
	if sum, _ := Index(s, dir, nil); sum.Parsed != 1 || sum.Skipped != 0 {
		t.Fatalf("version change should force reparse: parsed=%d skipped=%d, want 1/0", sum.Parsed, sum.Skipped)
	}
}
