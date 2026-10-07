package inject

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type activationCase struct {
	id       string
	path     func(string) string
	original string
	active   []string
	prior    []string
	editFrom string
	editTo   string
}

func activationCases() []activationCase {
	return []activationCase{
		{
			id: "claude", path: func(home string) string { return filepath.Join(home, ".claude", "settings.json") },
			original: `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com","ANTHROPIC_MODEL":"claude-old"},"theme":"dark"}` + "\n",
			active:   []string{`"ANTHROPIC_MODEL": "auto"`}, prior: []string{`claude-old`},
			editFrom: `"ANTHROPIC_MODEL": "auto"`, editTo: `"ANTHROPIC_MODEL": "claude-user"`,
		},
		{
			id: "codex", path: func(home string) string { return filepath.Join(home, ".codex", "config.toml") },
			original: "model_provider = \"openai\"\nmodel = \"gpt-old\"\n\n[tui]\nnotifications = true\n",
			active:   []string{`model_provider = "prowl"`, `model = "auto"`}, prior: []string{`model_provider = "openai"`, `model = "gpt-old"`},
			editFrom: `model = "auto"`, editTo: `model = "gpt-user"`,
		},
		{
			id: "opencode", path: func(home string) string { return filepath.Join(home, ".config", "opencode", "opencode.json") },
			original: `{"model":"openai/gpt-old","theme":"dark"}` + "\n",
			active:   []string{`"model": "prowl/auto"`}, prior: []string{`openai/gpt-old`},
			editFrom: `"model": "prowl/auto"`, editTo: `"model": "openai/gpt-user"`,
		},
		{
			id: "hermes", path: hermesConfigPath,
			original: "model:\n  provider: openrouter\n  default: old-model\nui:\n  theme: dark\n",
			active:   []string{"provider: prowl", "default: auto"}, prior: []string{"provider: openrouter", "default: old-model"},
			editFrom: "  default: auto", editTo: "  default: user-model",
		},
		{
			id: "omp", path: func(home string) string { return filepath.Join(home, ".omp", "agent", "config.yml") },
			original: "modelRoles:\n  default: openai/gpt-old\n  smol: openai/gpt-mini\ntheme:\n  dark: titanium\n",
			active:   []string{"default: prowl/auto"}, prior: []string{"default: openai/gpt-old"},
			editFrom: "  default: prowl/auto", editTo: "  default: openai/gpt-user",
		},
	}
}

func TestActivateSetsAndRemoveRestoresHarnessDefault(t *testing.T) {
	for _, tc := range activationCases() {
		t.Run(tc.id, func(t *testing.T) {
			home := testHome(t)
			path := tc.path(home)
			writeActivationFixture(t, path, tc.original)
			opts := optsFor(home, tc.id)
			opts.Activate = true
			if _, err := Apply(opts, tc.id); err != nil {
				t.Fatal(err)
			}
			active, note := Active(home, tc.id)
			if !active {
				t.Fatalf("expected active harness, note=%q config=%q", note, read(t, path))
			}
			for _, want := range tc.active {
				if !strings.Contains(read(t, path), want) {
					t.Fatalf("active config missing %q: %s", want, read(t, path))
				}
			}
			if _, err := Remove(home, tc.id); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.prior {
				if !strings.Contains(read(t, path), want) {
					t.Fatalf("restored config missing %q: %s", want, read(t, path))
				}
			}
		})
	}
}

func TestRemoveKeepsHarnessDefaultEditedAfterActivation(t *testing.T) {
	for _, tc := range activationCases() {
		t.Run(tc.id, func(t *testing.T) {
			home := testHome(t)
			path := tc.path(home)
			writeActivationFixture(t, path, tc.original)
			opts := optsFor(home, tc.id)
			opts.Activate = true
			if _, err := Apply(opts, tc.id); err != nil {
				t.Fatal(err)
			}
			configured := read(t, path)
			changed := strings.Replace(configured, tc.editFrom, tc.editTo, 1)
			if changed == configured {
				t.Fatalf("fixture did not find active value %q in %s", tc.editFrom, configured)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Remove(home, tc.id); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(read(t, path), tc.editTo) {
				t.Fatalf("user-selected model was reverted: %s", read(t, path))
			}
		})
	}
}

func TestApplyWithoutActivateKeepsExistingDefaultSelection(t *testing.T) {
	for _, tc := range activationCases() {
		if tc.id == "claude" {
			continue
		}
		t.Run(tc.id, func(t *testing.T) {
			home := testHome(t)
			path := tc.path(home)
			writeActivationFixture(t, path, tc.original)
			if _, err := Apply(optsFor(home, tc.id), tc.id); err != nil {
				t.Fatal(err)
			}
			for _, prior := range tc.prior {
				if !strings.Contains(read(t, path), prior) {
					t.Fatalf("non-activating apply changed the existing default %q: %s", prior, read(t, path))
				}
			}
		})
	}
}

func TestClaudeInjectionRemainsActiveWithoutActivate(t *testing.T) {
	home := testHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	writeActivationFixture(t, path, `{"theme":"dark"}`+"\n")
	if _, err := Apply(optsFor(home, "claude"), "claude"); err != nil {
		t.Fatal(err)
	}
	active, _ := Active(home, "claude")
	if !active {
		t.Fatal("Claude injection has always selected the routed model")
	}
}

func writeActivationFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
