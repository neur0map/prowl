// Package cli wires Prowl's user-facing commands, the hidden agent-launched
// serve command, the editor-launched LSP, read-only repository intelligence,
// and gateway controls. The bare root command launches the unified console.
package cli

import "github.com/spf13/cobra"

// Register adds all subcommands to the root command. managedBy is the build-time
// package-manager stamp (empty for a self-built binary) that gates self-update.
// Optional Options describe how the tree is hosted: Embedded() disables the
// commands that would own or replace the running executable, because when Prowl
// runs embedded that executable is the host program, not Prowl.
func Register(root *cobra.Command, version string, managedBy string, opts ...Option) {
	var cfg registerConfig
	for _, apply := range opts {
		apply(&cfg)
	}
	// One predictable output surface: --format / --json are persistent on the
	// root, so every subcommand accepts them in any position. Operator commands
	// that predate this keep their own local --json (cobra lets the local flag
	// shadow the inherited one); the agent-hot query and discovery commands read
	// these through resolveFormat.
	root.PersistentFlags().String("format", "", "output format: human, toon, json, or markdown (default: human on a terminal, toon when piped)")
	root.PersistentFlags().Bool("json", false, "output JSON (shorthand for --format json)")
	root.AddCommand(newInitCmd(version), newStatusCmd(version), newDoctorCmd(version), newKnowledgeCmd(), newContextCmd(), newCapabilitiesCmd(), newReviewCmd(), newServeCmd(version), newLSPCmd(version), newUpdateCmd(version, managedBy, cfg.embedded), newRestartCmd(version), newVersionCmd(version), newSkillsCmd(version), newSearchAdvisoryCmd(), newGatewayCmd(version, cfg.embedded))
	// Read-only query commands: the CLI-first path. Any agent can shell out to
	// these (token-lean TOON output) with no MCP server and no `serve`.
	root.AddCommand(
		newFindCmd(), newDefCmd(), newPeekCmd(), newOutlineCmd(), newSearchCmd(), newOverviewCmd(), newClustersCmd(),
		newCallersCmd(), newCalleesCmd(), newRelationsCmd(), newImpactCmd(),
		newEntrypointsCmd(), newHotspotsCmd(), newViolationsCmd(), newTestsCmd(),
		newReferencesCmd(), newChangedCmd(), newWipCmd(), newExploreCmd(),
		newBriefCmd(), newDocsCmd(), newSketchCmd(), newGraphCmd(), newBenchCmd(),
		newSpanCmd(),
		newHistoryCmd(),
	)
}

// Option configures how Register hosts the command tree.
type Option func(*registerConfig)

type registerConfig struct {
	// embedded is set when the command tree runs inside a host program (via
	// pkg/embedded), where the running executable is the host, not Prowl.
	embedded bool
}

// Embedded marks the command tree as running embedded in a host program.
// Commands that would replace the running executable (`update`) or spawn it as a
// gateway daemon (`gateway up`/`restart`/`serve`) are disabled with a clear
// error, since that executable is the host, not a standalone Prowl binary.
func Embedded() Option {
	return func(c *registerConfig) { c.embedded = true }
}
