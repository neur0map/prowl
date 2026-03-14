package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/daemon"
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

func init() {
	rootCmd.AddCommand(indexCmd)
	rootCmd.AddCommand(statusCmd)
	daemonCmd.AddCommand(daemonStartCmd)
	rootCmd.AddCommand(daemonCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
