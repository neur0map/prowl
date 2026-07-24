package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	if len(plan.Actions) != 2 {
		t.Fatalf("actions = %#v, want two", plan.Actions)
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
	for _, path := range []string{filepath.Join(root, ".cursor", "mcp.json"), filepath.Join(root, "AGENTS.md")} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "prowl-agent") {
			t.Fatalf("selected integration %s not written correctly: %q %v", path, data, err)
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

func TestInitDryRunJSONDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var out bytes.Buffer
	cmd := newInitCmd()
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
	for _, path := range []string{".prowl", ".cursor", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote %s: %v", path, err)
		}
	}
}

func TestInitNoInputWritesOnlySelectedIntegration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	var out bytes.Buffer
	cmd := newInitCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--no-input", "--no-ai", "--json", "--integrations", "cursor"})
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
