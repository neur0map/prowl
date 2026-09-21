package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/setup"
)

func TestDetectIntegrationsOnlyReportsPresentClients(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".cursor", ".vscode", ".omp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := DetectIntegrations(root)
	for _, want := range []string{IntegrationCursor, IntegrationVSCode, IntegrationOMP} {
		if !slices.Contains(got, want) {
			t.Errorf("detected integrations %v missing %q", got, want)
		}
	}
	for _, unwanted := range []string{IntegrationFactory, IntegrationOpenCode, IntegrationHelix} {
		if slices.Contains(got, unwanted) {
			t.Errorf("detected absent integration %q in %v", unwanted, got)
		}
	}
}

func TestBuildSetupPlanDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildSetupPlan(root, []string{IntegrationCursor, IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	// The plan covers the two selected clients (AGENTS.md and the Cursor MCP
	// config) plus the Cursor skill files; the exact count is not pinned so
	// adding or removing a skill does not break this no-write invariant test.
	if len(plan.Actions) < 2 {
		t.Fatalf("actions = %#v, want at least the two selected clients", plan.Actions)
	}
	var haveAgents, haveCursorMCP bool
	for _, a := range plan.Actions {
		if a.Path == "AGENTS.md" {
			haveAgents = true
		}
		if a.Path == ".cursor/mcp.json" {
			haveCursorMCP = true
		}
	}
	if !haveAgents || !haveCursorMCP {
		t.Fatalf("actions = %#v, want AGENTS.md and .cursor/mcp.json", plan.Actions)
	}
	if _, err := os.Stat(filepath.Join(root, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("planning wrote .cursor: %v", err)
	}
}

func TestApplyIntegrationsWritesOnlySelectedClients(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildSetupPlan(root, []string{IntegrationCursor, IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySetupPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		path, needle string
	}{
		{filepath.Join(root, ".cursor", "mcp.json"), `"prowl"`},
		{filepath.Join(root, "AGENTS.md"), "prowl overview"},
	} {
		data, err := os.ReadFile(check.path)
		if err != nil || !strings.Contains(string(data), check.needle) {
			t.Fatalf("selected integration %s not written correctly: %q %v", check.path, data, err)
		}
	}
	for _, path := range []string{filepath.Join(root, ".mcp.json"), filepath.Join(root, ".vscode", "mcp.json"), filepath.Join(root, "opencode.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unselected integration was written: %s (%v)", path, err)
		}
	}
}

func TestRemoveIntegrationsPreservesUnownedConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"mcpServers":{"other":{"command":"other"},"prowl-agent":{"command":"prowl-agent","args":["serve"]}}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIntegrations(root, []string{IntegrationCursor}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"prowl-agent"`) || !strings.Contains(string(data), `"other"`) {
		t.Fatalf("ownership-safe removal failed: %s", data)
	}
}

func TestApplySetupPlanRollsBackWhenExistingConfigIsInvalid(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	invalid := []byte("{ definitely not json")
	if err := os.WriteFile(path, invalid, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildSetupPlan(root, []string{IntegrationAgents, IntegrationCursor})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySetupPlan(plan); err == nil {
		t.Fatal("invalid existing client config should fail")
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("earlier action was not rolled back: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(invalid) {
		t.Fatalf("invalid user file was modified: %q %v", got, err)
	}
}

func TestApplySetupPlanRejectsStaleConfiguration(t *testing.T) {
	root := t.TempDir()
	plan, err := BuildSetupPlan(root, []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".prowl", "config.toml"), []byte("changed = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplySetupPlan(plan); err == nil {
		t.Fatal("stale setup plan succeeded")
	}
	if _, statErr := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(statErr) {
		t.Fatalf("stale setup plan wrote integration: %v", statErr)
	}
}

func TestParseIntegrationSelection(t *testing.T) {
	got, err := ParseIntegrationSelection("cursor, agents, cursor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{IntegrationAgents, IntegrationCursor}) {
		t.Fatalf("selection = %v", got)
	}
	if _, err := ParseIntegrationSelection("cursor,warp", nil); err == nil {
		t.Fatal("unknown integration should fail")
	}
}

func TestParseIntegrationSelectionAutoBaseline(t *testing.T) {
	// `auto` always installs the client-agnostic baseline -- AGENTS.md guidance
	// and the `.mcp.json` MCP registration -- even when nothing is detected, so a
	// bare init never leaves an indexed repo with no signal that Prowl exists.
	got, err := ParseIntegrationSelection("auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{IntegrationAgents, IntegrationGeneric}) {
		t.Fatalf("auto baseline = %v, want [%s %s]", got, IntegrationAgents, IntegrationGeneric)
	}
	// Detected clients merge in without dropping the baseline.
	got, err = ParseIntegrationSelection("auto", []string{IntegrationCursor})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{IntegrationAgents, IntegrationCursor, IntegrationGeneric}) {
		t.Fatalf("auto with detected cursor = %v", got)
	}
}

func TestInitDryRunJSONDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	cmd := newInitCmd("v0.0.0-test")
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dry-run", "--json", "--integrations", "cursor"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON report %q: %v", out.String(), err)
	}
	if report["dry_run"] != true {
		t.Fatalf("dry_run report = %#v", report)
	}
	plan, ok := report["plan"].(map[string]any)
	if !ok || plan["root"] != root {
		t.Fatalf("plan root = %#v, want %q", report["plan"], root)
	}
	if _, found := plan["Root"]; found {
		t.Fatalf("plan exposed incompatible Root key: %#v", plan)
	}
	for _, path := range []string{".prowl", ".cursor", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s: %v", path, err)
		}
	}
}

func TestInitNoInputWritesOnlySelectedIntegration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no ollama/claude/codex/omp discoverable: structural-only, hermetic
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var out bytes.Buffer
	cmd := newInitCmd("v0.0.0-test")
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--no-input", "--json", "--integrations", "cursor"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".cursor", "mcp.json")); err != nil {
		t.Fatalf("selected Cursor integration missing: %v", err)
	}
	for _, path := range []string{".mcp.json", "AGENTS.md", "opencode.json"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("unselected integration was written: %s (%v)", path, err)
		}
	}
}

func TestRemoveIntegrationsDeletesProwlOnlyAgentsFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	plan, err := BuildSetupPlan(root, []string{IntegrationAgents})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySetupPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := RemoveIntegrations(root, []string{IntegrationAgents}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Prowl-only AGENTS.md remains after removal: %v", err)
	}
}

// TestInitWiresUserOnlyHarnessSkills proves the supported init contract for the
// user-level-only harnesses (Pi/Hermes/OpenClaw/Prowl Legacy): a selection that
// includes one actually installs its skills through the user-skill transaction
// -- init no longer counts it "configured" while writing nothing -- a dry-run
// preview touches nothing, re-apply is idempotent, and --remove-integrations
// removes them symmetrically. A project-only selection stays a pure no-op.
func TestInitWiresUserOnlyHarnessSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	const version = "v1.2.3"
	selection := []string{IntegrationAgents, IntegrationGeneric, "pi"}

	// A project-only selection is a pure no-op: no user-only harness, no plan.
	if up, err := planInitUserSkills(version, []string{IntegrationAgents, IntegrationGeneric, IntegrationCursor}, false); err != nil || up != nil {
		t.Fatalf("project-only selection produced a user plan: %+v (err %v)", up, err)
	}

	// Dry-run preview names the harness and its install actions but writes nothing.
	preview, err := planInitUserSkills(version, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview == nil || len(preview.Clients) != 1 || preview.Clients[0] != "pi" {
		t.Fatalf("preview clients = %+v, want [pi]", preview)
	}
	if len(writeActions(preview.Plan)) == 0 {
		t.Fatal("preview had no user-skill install actions")
	}
	if _, err := os.Stat(filepath.Join(home, ".pi")); !os.IsNotExist(err) {
		t.Fatal("planning wrote user assets before apply")
	}

	// Apply installs the harness's skills under its user root.
	applied, err := applyInitUserSkills(version, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil || len(writeActions(applied.Plan)) == 0 {
		t.Fatalf("apply installed nothing: %+v", applied)
	}
	for _, action := range writeActions(applied.Plan) {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(action.Destination))); err != nil {
			t.Fatalf("installed action %s missing on disk: %v", action.Destination, err)
		}
	}

	// Re-apply is idempotent: the fresh plan has no more writes.
	again, err := applyInitUserSkills(version, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil && planHasWrites(again.Plan) {
		t.Fatalf("second apply still had writes: %+v", writeActions(again.Plan))
	}

	// --remove-integrations symmetry: removal uninstalls the harness skills, so a
	// fresh install plan sees every asset as a missing install again.
	if _, err := applyInitUserSkills(version, selection, true); err != nil {
		t.Fatal(err)
	}
	reinstall, err := planInitUserSkills(version, selection, false)
	if err != nil {
		t.Fatal(err)
	}
	if reinstall == nil || len(writeActions(reinstall.Plan)) == 0 {
		t.Fatal("removal left nothing to reinstall; user assets were not removed")
	}
	for _, action := range reinstall.Plan.Actions {
		if action.Kind != "install" {
			t.Fatalf("after removal, %s is %q, want a fresh install", action.Destination, action.Kind)
		}
	}
}

// TestInitPickerNamesKeepsDetectedUserOnlyHarness: the interactive picker must
// offer (and thereby preserve) a detected user-only harness. huh rebuilds the
// bound slice from rendered options only, so a selected "pi" absent from the
// options would be silently dropped and its user-level skills never installed.
// Every selected name must appear in the offered names; a user-only harness
// that is NOT selected must not be advertised on a bare init.
func TestInitPickerNamesKeepsDetectedUserOnlyHarness(t *testing.T) {
	selected := []string{IntegrationAgents, IntegrationGeneric, setup.IntegrationPi}
	names := initPickerNames(selected)

	for _, want := range selected {
		if !slices.Contains(names, want) {
			t.Errorf("offered names %v dropped selected %q", names, want)
		}
	}
	for _, want := range []string{IntegrationCursor, IntegrationVSCode, IntegrationOMP} {
		if !slices.Contains(names, want) {
			t.Errorf("offered names %v missing project-level %q", names, want)
		}
	}
	if slices.Contains(names, setup.IntegrationHermes) {
		t.Errorf("offered names %v advertised an undetected user-only harness", names)
	}
}
