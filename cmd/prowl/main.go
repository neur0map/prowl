package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/mcp"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/tui"
	"github.com/neur0map/prowl/internal/updater"
)

// Version is set at build time via -ldflags.
// Falls back to the module version from go install.
var Version = "dev"

func init() {
	if Version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			Version = info.Main.Version
		}
	}
}

var rootCmd = &cobra.Command{
	Use:   "prowl [path]",
	Short: "Context compiler for AI coding agents",
	Long:  "prowl indexes your codebase and serves structural context to AI agents via MCP.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		absDir, _ := filepath.Abs(dir)
		prowlDir := filepath.Join(absDir, ".prowl")

		if _, err := os.Stat(filepath.Join(prowlDir, "prowl.db")); os.IsNotExist(err) {
			// No index exists — run the setup wizard
			return tui.RunWithWizard(dir)
		}

		// Index exists — open dashboard
		return tui.RunDashboard(dir, Version)
	},
}

var mcpCmd = &cobra.Command{
	Use:    "mcp [path]",
	Short:  "Start MCP server for AI agent integration",
	Hidden: true,
	Args:   cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		absDir, _ := filepath.Abs(dir)
		prowlDir := filepath.Join(absDir, ".prowl")
		dbPath := filepath.Join(prowlDir, "prowl.db")
		contextDir := filepath.Join(prowlDir, "context")

		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return fmt.Errorf("no index found at %s - run 'prowl' first", absDir)
		}

		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close()

		homeDir, _ := os.UserHomeDir()
		modelDir := filepath.Join(homeDir, ".prowl", "models")
		embedder, err := embed.New(modelDir)
		if err != nil {
			return fmt.Errorf("load embedding model: %w", err)
		}
		defer embedder.Close()

		server := mcp.New(st, embedder, contextDir, Version)
		defer server.Close()
		return server.Run()
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the prowl version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("prowl %s\n", Version)
	},
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update prowl to the latest release",
	RunE: func(cmd *cobra.Command, args []string) error {
		return updater.Update(Version)
	},
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(updateCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
