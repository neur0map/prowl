package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/setup"
)

func integrationDoctorEnv(t *testing.T) (home, state string) {
	t.Helper()
	home = t.TempDir()
	state = filepath.Join(t.TempDir(), "missing-state")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".omp", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("PATH", "")
	t.Chdir(t.TempDir())
	return home, state
}

func TestDoctorIntegrationsRunsOutsideProjectAndRendersJSON(t *testing.T) {
	_, state := integrationDoctorEnv(t)
	cmd := newDoctorCmd("v9.9.9")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--integrations", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor integrations: %v\n%s", err, out.String())
	}
	var report setup.UserIntegrationHealth
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if report.PackageVersion != "v9.9.9" || len(report.Clients) != 3 {
		t.Fatalf("report: %+v", report)
	}
	if report.Clients[0].Client != setup.IntegrationClaude || report.Clients[1].Client != setup.IntegrationOMP ||
		report.Clients[2].Client != setup.IntegrationProwl {
		t.Fatalf("client order: %+v", report.Clients)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("doctor created state directory: %v", err)
	}
}

func TestDoctorIntegrationsHonorsInheritedRootFormat(t *testing.T) {
	integrationDoctorEnv(t)
	cases := [][]string{{"--format=json"}, {"--format", "json"}, {"--json"}}
	for _, flags := range cases {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			root := &cobra.Command{Use: "prowl"}
			Register(root, "v9.9.9", "")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append(flags, "doctor", "--integrations"))
			if err := root.Execute(); err != nil {
				t.Fatalf("root doctor: %v\n%s", err, out.String())
			}
			var report setup.UserIntegrationHealth
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("inherited %v did not produce JSON: %v\n%s", flags, err, out.String())
			}
		})
	}
}

func TestDoctorIntegrationsHumanNamesStatuses(t *testing.T) {
	integrationDoctorEnv(t)
	cmd := newDoctorCmd("v9.9.9")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--integrations", "--format", "human"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(out.String())
	for _, want := range []string{"v9.9.9", "cli", "missing", "claude", "omp"} {
		if !strings.Contains(text, want) {
			t.Errorf("human report missing %q:\n%s", want, out.String())
		}
	}
	current := renderDoctorIntegrations(setup.UserIntegrationHealth{
		PackageVersion: "v9.9.9",
		CLI:            setup.UserCLIHealth{Status: setup.UserCLIStatusCurrent, Version: "v9.9.9"},
		Clients: []setup.UserClientHealth{
			{Client: setup.IntegrationClaude, Status: setup.UserIntegrationCurrent, Activation: setup.UserActivationRestartRequired},
			{Client: setup.IntegrationOMP, Status: setup.UserIntegrationCurrent, Activation: setup.UserActivationReloadRequired},
		},
		Assets: []setup.UserAssetHealth{
			{Client: setup.IntegrationClaude, AssetID: "claude:commands/search.md", Destination: ".claude/skills/prowl/commands/search.md", State: setup.UserAssetMissing},
			{Client: setup.IntegrationOMP, AssetID: "omp:extensions/prowl-routing.ts", Destination: ".omp/agent/extensions/prowl-routing.ts", State: setup.UserAssetConflict},
		},
	})
	for _, want := range []string{
		"claude: current", "restart required", "omp: current", "reload required",
		"missing .claude/skills/prowl/commands/search.md",
		"conflict .omp/agent/extensions/prowl-routing.ts",
	} {
		if !strings.Contains(strings.ToLower(current), want) {
			t.Errorf("current human report missing %q:\n%s", want, current)
		}
	}
}

func TestDoctorIntegrationsRejectsRepositoryOnlyOptions(t *testing.T) {
	integrationDoctorEnv(t)
	cases := [][]string{
		{"--integrations", "--profile", "general"},
		{"--integrations", "--baseline", "old.json"},
		{"--integrations", "--fail-on", "none"},
		{"--integrations", "--include-excluded"},
		{"--integrations", "--format", "sarif"},
		{"--integrations", "--format", "shields"},
	}
	for _, args := range cases {
		cmd := newDoctorCmd("v9.9.9")
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--integrations") {
			t.Errorf("args %v error = %v", args, err)
		}
	}
}
