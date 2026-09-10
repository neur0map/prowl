// Package cli wires Prowl Agent's user-facing commands (init, status, doctor,
// update, restart, version), the hidden agent-launched serve command, the
// editor-launched lsp command, and the read-only query commands. Prowl is an
// agent-first context engine: the CLI and MCP surfaces are the whole product.
package cli

import "github.com/spf13/cobra"

// Register adds all subcommands to the root command. managedBy is the build-time
// package-manager stamp (empty for a self-built binary) that gates self-update.
func Register(root *cobra.Command, version string, managedBy string) {
	// One predictable output surface: --format / --json are persistent on the
	// root, so every subcommand accepts them in any position. Operator commands
	// that predate this keep their own local --json (cobra lets the local flag
	// shadow the inherited one); the agent-hot query and discovery commands read
	// these through resolveFormat.
	root.PersistentFlags().String("format", "", "output format: human, toon, json, or markdown (default: human on a terminal, toon when piped)")
	root.PersistentFlags().Bool("json", false, "output JSON (shorthand for --format json)")
	root.AddCommand(newInitCmd(), newStatusCmd(version), newDoctorCmd(version), newKnowledgeCmd(), newContextCmd(), newCapabilitiesCmd(), newReviewCmd(), newServeCmd(version), newLSPCmd(version), newUpdateCmd(version, managedBy), newRestartCmd(version), newVersionCmd(version), newSkillsCmd(version), newSearchAdvisoryCmd())
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
