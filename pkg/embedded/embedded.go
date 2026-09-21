// Package embedded exposes Prowl's read-only and scriptable CLI command tree as
// an in-process entry point so a host program can run it natively - no
// subprocess, no external binary - while preserving identical command behavior
// and output.
//
// Two contract narrowings apply versus the standalone `prowl` binary, both
// because the running executable is the host program, not Prowl:
//
//   - The interactive unified TUI is not exposed. Standalone `prowl` with no
//     subcommand (and `prowl gateway` with none) opens a console that manages a
//     gateway daemon by spawning this executable - unsafe when it is the host.
//     Embedded, a bare Execute prints the command help, and the `gateway`
//     console is refused; its non-spawning subcommands (providers, inject,
//     status, down) stay available.
//   - Commands that would replace the running executable (`update`) or spawn it
//     as a gateway daemon (`gateway up`/`restart`/`serve`) are disabled with a
//     clear error via cli.Embedded().
//
// Prowl's commands locate the repository from a start directory. Execute threads
// the caller's workdir through the command context (application.WithRoot) so a
// query resolves against that directory without ever mutating the process
// working directory. Concurrent host goroutines keep their own cwd, and
// independent Execute calls never contend: each builds its own command tree and
// its own context.
package embedded

import (
	"context"
	"io"
	"sync"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/cli"
	"github.com/neur0map/prowl/internal/version"
)

// Version is the Prowl version reported by the embedded command tree. The host
// may override it at startup (before the first Execute) to match the bundled
// build.
var Version = "embedded"

// versionOnce publishes Version to the shared version surface exactly once, so
// concurrent Execute calls never race on the package variable.
var versionOnce sync.Once

// Execute runs a Prowl command in-process against workdir and writes the
// command's output to stdout/stderr. args is the argument vector without the
// program name (e.g. []string{"find", "NewGui", "--format", "toon"}). An empty
// workdir runs against the current process directory; empty args print the
// command help.
//
// Execute never mutates the process working directory, so a concurrent host
// goroutine's cwd-relative work is unaffected while it runs; the workdir is
// threaded through the command context instead.
func Execute(workdir string, args []string, stdout, stderr io.Writer) error {
	// Report the embedded build identity through the shared version surface the
	// gateway's status/version API reads, mirroring cmd/prowl before it registers
	// commands. Idempotent and single-writer, so it adds no cross-goroutine race.
	versionOnce.Do(func() { version.Version = Version })

	root := &cobra.Command{
		Use:           "prowl",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	// Embedded mode: disable the commands that would own or replace this
	// executable (self-update, gateway daemon lifecycle) - it is the host
	// program, not a standalone Prowl binary. managedBy stays empty; the embedded
	// marker, not a package stamp, is what gates those commands here.
	cli.Register(root, Version, "", cli.Embedded())
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.ExecuteContext(application.WithRoot(context.Background(), workdir))
}
