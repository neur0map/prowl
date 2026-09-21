// Package setup: machine-level harness detection for init.
package setup

import (
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// IntegrationPi is a user-level-only client. Pi discovers skills exactly
	// one directory below ~/.pi/agent/skills, so portable Prowl skills install
	// directly under that root rather than in a nested group.
	IntegrationPi = "pi"
	// IntegrationOpenClaw is a user-level-only client. OpenClaw reads user-level
	// skills, so it has no project-level action; its assets install under
	// ~/.openclaw/skills/prowl.
	IntegrationOpenClaw = "openclaw"
)

// DetectInstalledHarnesses reports harness integrations whose coding-agent tool
// is present on this machine -- by its user config directory or a launcher on
// PATH -- even when the current project has no config directory for it yet.
//
// init folds these into the `auto` selection so a freshly indexed repo picks up
// the native integration for the agents the user actually runs: the harness's
// own skills (its "when to reach for prowl" routing signal) and, for omp, a
// native MCP server entry. The client-agnostic AGENTS.md guidance and root
// .mcp.json are always written regardless; this adds the harness-native layer on
// top so every agent -- not only MCP-aware ones -- knows prowl out of the box.
//
// Scope is deliberately limited to omp, pi, claude, hermes, openclaw, and the
// retired prowl-legacy harness (the harnesses with a native skill system prowl
// targets); other clients stay
// project-directory detected so a bare init does not scatter unused tool
// configs into every repo.
func DetectInstalledHarnesses() []string {
	home, _ := os.UserHomeDir()
	var out []string
	if harnessPresent(home, ".omp/agent", "omp") {
		out = append(out, IntegrationOMP)
	}
	if harnessPresent(home, ".pi/agent", "pi") {
		out = append(out, IntegrationPi)
	}
	if harnessPresent(home, ".claude", "claude") {
		out = append(out, IntegrationClaude)
	}
	if harnessPresent(home, ".hermes", "hermes") {
		out = append(out, IntegrationHermes)
	}
	if harnessPresent(home, ".openclaw", "openclaw") {
		out = append(out, IntegrationOpenClaw)
	}
	// The retired Prowl harness is the `prowl-legacy` binary (its `dirs` reports
	// ~/.config/prowl). Detect that binary or its config root -- never the current
	// `prowl` product binary, which is not a skill-reading harness.
	if harnessPresent(home, ".config/prowl", "prowl-legacy") {
		out = append(out, IntegrationProwl)
	}
	return out
}

// harnessPresent reports whether a harness is installed: its user config
// directory exists, or its launcher is on PATH.
func harnessPresent(home, userSubdir, bin string) bool {
	if home != "" {
		if info, err := os.Stat(filepath.Join(home, filepath.FromSlash(userSubdir))); err == nil && info.IsDir() {
			return true
		}
	}
	_, err := exec.LookPath(bin)
	return err == nil
}
