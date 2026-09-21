package setup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDetectInstalledHarnessesRetargetsProwlLegacy proves the retired Prowl
// harness is detected by its retained ~/.config/prowl root or the `prowl-legacy`
// binary, and that the current `prowl` product binary is never mistaken for a
// skill-reading harness (which would install skills to a root nothing reads).
func TestDetectInstalledHarnessesRetargetsProwlLegacy(t *testing.T) {
	// The ~/.config/prowl root (what `prowl-legacy dirs` reports) detects it.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	if got := DetectInstalledHarnesses(); !slices.Contains(got, IntegrationProwl) {
		t.Fatalf("~/.config/prowl not detected as %q: %v", IntegrationProwl, got)
	}

	// The current `prowl` binary on PATH -- without the config root or the
	// prowl-legacy binary -- must NOT be detected; it is the product, not a
	// harness that reads user-level skills.
	home2 := t.TempDir()
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "prowl"))
	t.Setenv("HOME", home2)
	t.Setenv("PATH", bin)
	if got := DetectInstalledHarnesses(); slices.Contains(got, IntegrationProwl) {
		t.Fatalf("current prowl binary was mistaken for a skill harness: %v", got)
	}

	// The prowl-legacy binary on PATH IS detected.
	writeExecutable(t, filepath.Join(bin, "prowl-legacy"))
	if got := DetectInstalledHarnesses(); !slices.Contains(got, IntegrationProwl) {
		t.Fatalf("prowl-legacy binary not detected as %q: %v", IntegrationProwl, got)
	}
}
