package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/daemon"
	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/mcp"
	"github.com/neur0map/prowl/internal/pipeline"
	"github.com/neur0map/prowl/internal/store"
)

var rootCmd = &cobra.Command{
	Use:   "prowl",
	Short: "Context compiler for AI coding agents",
}

var indexCmd = &cobra.Command{
	Use:   "index [path]",
	Short: "Index a project and write .prowl/context/",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return pipeline.Index(args[0])
	},
}

var statusCmd = &cobra.Command{
	Use:   "status [path]",
	Short: "Show index stats for a project",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		absDir, _ := filepath.Abs(dir)
		dbPath := filepath.Join(absDir, ".prowl", "prowl.db")
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			fmt.Println("No index found. Run 'prowl index' first.")
			return nil
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		files, symbols, edges, _ := st.Stats()
		fmt.Printf("Files:   %d\nSymbols: %d\nEdges:   %d\n", files, symbols, edges)
		return nil
	},
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the file watcher daemon",
}

var daemonStartCmd = &cobra.Command{
	Use:   "start [path]",
	Short: "Start watching for file changes (foreground, Ctrl+C to stop)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		d, err := daemon.New(dir, 1*time.Second)
		if err != nil {
			return err
		}
		fmt.Println("Prowl daemon started. Watching for changes... (Ctrl+C to stop)")

		// Graceful shutdown on SIGINT/SIGTERM
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sig
			fmt.Println("\nStopping daemon...")
			d.Stop()
		}()

		d.Run()
		return nil
	},
}

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Semantic search across the codebase",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := args[0]
		limit, _ := cmd.Flags().GetInt("limit")

		dir := "."
		absDir, _ := filepath.Abs(dir)
		dbPath := filepath.Join(absDir, ".prowl", "prowl.db")

		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			fmt.Println("No index found. Run 'prowl index' first.")
			return nil
		}

		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close()

		// Load embedder
		homeDir, _ := os.UserHomeDir()
		modelDir := filepath.Join(homeDir, ".prowl", "models")
		embedder, err := embed.New(modelDir)
		if err != nil {
			return fmt.Errorf("load model: %w", err)
		}
		defer embedder.Close()

		// Encode query
		vecs, err := embedder.Encode([]string{query})
		if err != nil {
			return fmt.Errorf("encode query: %w", err)
		}

		// Search
		results, err := st.SearchSimilar(vecs[0], limit)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}

		if len(results) == 0 {
			fmt.Println("No results found.")
			return nil
		}

		for i, r := range results {
			fmt.Printf("\n%d. %s (score: %.4f)\n", i+1, r.FilePath, r.Score)
			if r.Signatures != "" {
				// Show first 3 lines of signatures
				lines := strings.SplitN(r.Signatures, "\n", 4)
				for _, line := range lines[:min(len(lines), 3)] {
					fmt.Printf("   %s\n", line)
				}
				if len(lines) > 3 {
					fmt.Printf("   ...\n")
				}
			}
		}
		fmt.Println()
		return nil
	},
}

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start MCP server for AI agent integration",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		absDir, _ := filepath.Abs(dir)
		dbPath := filepath.Join(absDir, ".prowl", "prowl.db")

		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			return fmt.Errorf("no index found at %s - run 'prowl index' first", absDir)
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

		server := mcp.New(st, embedder, "")
		return server.Run()
	},
}

func init() {
	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(statusCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	rootCmd.AddCommand(daemonCmd)
	searchCmd.Flags().IntP("limit", "n", 5, "max results")
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(mcpCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
