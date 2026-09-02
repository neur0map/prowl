package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/prowl-agent/prowl-agent/internal/cli"
)

var version = "v0.15.6"

// managedBy is stamped at build time for packaged binaries
// (-ldflags "-X main.managedBy=pacman"). When set, `prowl-agent update` defers
// to the package manager instead of self-updating. It is empty for a self-built
// or downloaded binary.
var managedBy = ""

func main() {
	root := &cobra.Command{
		Use:   "prowl-agent",
		Short: "Local code index that gives AI agents small, cited answers instead of whole files",
		Long: `prowl-agent is a local, CLI-first code index for AI agents: one command
returns a small, cited answer instead of a wall of grep hits or whole files.

Agent quickstart (TOON output is token-lean; add --json for machine parsing):
  prowl-agent overview                          map the repo (languages, subsystems, entrypoints)
  prowl-agent find <name>                       locate a symbol, then 'def <id>' reads just it
  prowl-agent search <text>                     semantic + full-text content search
  prowl-agent peek <file:start-end>             read a bounded, cited line range of any hit
  prowl-agent impact <path>                     what breaks if you change a file
  prowl-agent capabilities search "<intent>"    find the right command by intent

Every command accepts --format {toon,json,human,markdown} and --json.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	cli.Register(root, version, managedBy)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
