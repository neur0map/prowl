package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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
		fmt.Printf("Indexing %s...\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(indexCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
