package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"github.com/prowl-agent/prowl-agent/internal/setup"
)

func TestEnsureAgentsBlockRefreshesInPlace(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	// A user's own notes, with a stale prowl block sandwiched between them.
	stale := setup.AgentsMarker + "\nold guidance: use prowl-agent serve only\n" + setup.AgentsEndMarker
	original := "# My project\n\nSome house rules.\n\n" + stale + "\n\n## Other section\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setup.EnsureAgentsBlock(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)

	// Stale block content is replaced with the current guidance.
	if strings.Contains(s, "old guidance: use prowl-agent serve only") {
		t.Errorf("stale block content not replaced:\n%s", s)
	}
	if !strings.Contains(s, "Use the local Prowl index before broad file reads") {
		t.Errorf("current guidance missing:\n%s", s)
	}
	// The user's surrounding content is preserved.
	if !strings.Contains(s, "Some house rules.") || !strings.Contains(s, "## Other section") {
		t.Errorf("user content not preserved:\n%s", s)
	}
	// Exactly one block (no duplication).
	if n := strings.Count(s, setup.AgentsMarker); n != 1 {
		t.Errorf("expected exactly one block marker, got %d:\n%s", n, s)
	}
	if n := strings.Count(s, setup.AgentsEndMarker); n != 1 {
		t.Errorf("expected exactly one end marker, got %d:\n%s", n, s)
	}
}

func TestEnsureAgentsBlockAppendsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(path, []byte("# Existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setup.EnsureAgentsBlock(path); err != nil {
		t.Fatal(err)
	}
	s, _ := os.ReadFile(path)
	if !strings.Contains(string(s), "# Existing") || !strings.Contains(string(s), setup.AgentsMarker) {
		t.Errorf("expected existing content plus appended block:\n%s", s)
	}
}

func TestEnsureAgentsBlockMissingEndMarkerKeepsUserText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	// A malformed block: the opening marker survives but the closing one was
	// removed, with the user's own text below it. The refresh must not delete to
	// end of file.
	original := "# My project\n" + setup.AgentsMarker + "\nstale prowl body\n\n## Important user rules\nKeep these.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setup.EnsureAgentsBlock(path); err != nil {
		t.Fatal(err)
	}
	s, _ := os.ReadFile(path)
	got := string(s)
	// The user's text below the orphan marker is preserved, not wiped to EOF.
	if !strings.Contains(got, "## Important user rules") || !strings.Contains(got, "Keep these.") {
		t.Errorf("user text deleted on missing end marker:\n%s", got)
	}
	if !strings.Contains(got, "# My project") {
		t.Errorf("user heading before marker lost:\n%s", got)
	}
	// A well-formed block now exists (both markers, exactly once each).
	if n := strings.Count(got, setup.AgentsMarker); n != 1 {
		t.Errorf("expected one opening marker, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, setup.AgentsEndMarker); n != 1 {
		t.Errorf("expected one closing marker, got %d:\n%s", n, got)
	}
}

func TestEnsureAgentsBlockNeverOverwritesUserFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	// A user's own AGENTS.md with no prowl markers at all.
	original := "# House rules\n\n1. Run tests before pushing.\n2. No force-push to main.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setup.EnsureAgentsBlock(path); err != nil {
		t.Fatal(err)
	}
	s, _ := os.ReadFile(path)
	got := string(s)
	// Every byte of the user's file is still present (appended to, never replaced).
	if !strings.HasPrefix(got, original) {
		t.Errorf("user file content was modified, not just appended to:\n%s", got)
	}
	if !strings.Contains(got, setup.AgentsMarker) {
		t.Errorf("prowl block was not appended:\n%s", got)
	}
}
