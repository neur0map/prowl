package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangedCLIE2E builds the binary, makes a git repo from the fixture, edits a
// file, and checks that `changed` maps the edit to its blast radius and keeps
// unindexed files out of the default output.
func TestChangedCLIE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process e2e in -short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "prowl")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, "./cmd/prowl")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)

	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("add", "-A")
	git("commit", "-qm", "init")

	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}

	// Edit an indexed file that other files include.
	f := filepath.Join(root, "hypr", "colors.conf")
	data, _ := os.ReadFile(f)
	if err := os.WriteFile(f, append(data, []byte("\n$new = rgb(010203)\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	out := run("changed")
	// The edited file maps to its dependent via blast radius.
	if !strings.Contains(out, "hypr/colors.conf") || !strings.Contains(out, "hypr/hyprland.conf") {
		t.Fatalf("changed should show colors.conf -> hyprland.conf impact:\n%s", out)
	}
	// Default output excludes unindexed files (prowl's own init configs).
	if strings.Contains(out, ".cursor/mcp.json") {
		t.Fatalf("default changed should omit unindexed files:\n%s", out)
	}
	// --all includes them.
	if all := run("changed", "--all"); !strings.Contains(all, ".cursor/mcp.json") {
		t.Fatalf("changed --all should include unindexed files:\n%s", all)
	}
}
