package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/cli"
	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/tui"
	versionpkg "github.com/neur0map/prowl/internal/version"
)

var version = "v0.16.4" // stamped; also fed to internal/version

// commit is the full 40-hex source commit, stamped in release builds
// (-ldflags "-X main.commit=<sha>"). It is empty for a local or downloaded
// build, which reads honestly as an unknown-origin install.
var commit = ""

// managedBy is stamped at build time for packaged binaries
// (-ldflags "-X main.managedBy=pacman"). When set, `prowl update` defers to
// the package manager instead of self-updating. It is empty for a self-built
// or downloaded binary.
var managedBy = ""

func main() {
	root := newRootCommand(version, commit, managedBy)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newRootCommand assembles the root command and stamps the build identity the
// gateway's update surface and the version command read. version and commit are
// injected so the composition is testable without rebuilding the binary.
func newRootCommand(version, commit, managedBy string) *cobra.Command {
	root := &cobra.Command{
		Use:   "prowl",
		Short: "Unified code intelligence and AI gateway",
		Long: `Prowl is the local control plane for code intelligence and model routing.

Run it without a subcommand to open the unified terminal interface: project
index status, gateway setup, connected accounts, model controls, routing
explanations, activity, and every agent-facing tool in one place.

Agent quickstart (TOON output is token-lean; add --json for machine parsing):
  prowl init                                      index and configure the current project
  prowl overview                                  map the repo (languages, subsystems, entrypoints)
  prowl find <name>                               locate a symbol, then 'def <id>' reads just it
  prowl search <text>                             semantic + full-text content search
  prowl peek <file:start-end>                     read a bounded, cited line range of any hit
  prowl impact <path>                             what breaks if you change a file
  prowl capabilities search "<intent>"            find the right command by intent
  prowl review plan                               capture and partition the current change

Every command accepts --format {toon,json,human,markdown} and --json.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, _ := cmd.Flags().GetInt("port")
			return tui.Run(cmd.Context(), port, version)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	// --version reports the semantic version and, for a release build, the
	// source commit it was stamped with. A local build has no commit, so the
	// suffix is dropped and only the version prints.
	versionTemplate := "prowl version {{.Version}}\n"
	if commit != "" {
		versionTemplate = "prowl version {{.Version}} (commit " + commit + ")\n"
	}
	root.SetVersionTemplate(versionTemplate)
	// The gateway's update-check surface reads the build identity from
	// internal/version. Stamp both fields once, before any command is
	// registered, so the surface reports the version and commit the binary
	// actually is. An empty commit reads as an unknown-origin install.
	versionpkg.Version = version
	versionpkg.Commit = commit
	root.Flags().Int("port", gateway.DefaultPort, "gateway port for the unified console")
	cli.Register(root, version, managedBy)
	return root
}
