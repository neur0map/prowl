package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
)

// newRestartCmd rebuilds the index from scratch and stops any running servers so
// the agent/editor relaunches the current binary. Use it after upgrading or if a
// project's data looks stale.
func newRestartCmd(string) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Rebuild the index from scratch and restart running MCP/LSP servers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := application.OpenProject(cmd.Context(), ".", application.Options{
				EnableAI: true, InferencerProvider: maybeInferencer,
			})
			if err != nil {
				return err
			}
			defer project.Close()
			ws, s := project.Workspace, project.Store
			out := cmd.OutOrStdout()

			// Rebuild first: restart is then the sole writer while any live server
			// just reads via WAL. Stopping servers first would make the agent respawn
			// one that re-indexes concurrently and contends on the database.
			fmt.Fprintf(out, "Rebuilding index for %s ...\n", ws.Root)
			if err := s.SetMeta("index_version", ""); err != nil { // force a full reparse
				return err
			}
			// Refresh through the shared project graph. Embedding remains best-effort,
			// so an Ollama or model issue cannot block the server stop below.
			msg, err := reindexer(project)(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "Rebuilt:", msg)

			if n := stopWorkspaceServers(ws.Root); n > 0 {
				fmt.Fprintf(out, "Stopped %d running server(s); your agent/editor relaunches them on next use.\n", n)
			}
			return nil
		},
	}
}

// matchProwlServer reports whether a process (args from /proc cmdline, cwd) is a
// prowl serve/lsp worth stopping. scope=="" matches regardless of cwd;
// otherwise only processes whose cwd is at or under scope match.
func matchProwlServer(args []string, cwd, scope string) bool {
	if len(args) < 2 || filepath.Base(args[0]) != "prowl" {
		return false
	}
	if args[1] != "serve" && args[1] != "lsp" {
		return false
	}
	if scope == "" {
		return true
	}
	return cwd == scope || strings.HasPrefix(cwd, scope+"/")
}

// stopWorkspaceServers stops the serve/lsp processes rooted at root.
func stopWorkspaceServers(root string) int {
	return stopServers(findProwlServers(root))
}
