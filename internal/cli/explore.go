package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// newExploreCmd indexes a repository you do not own into an ephemeral scratch
// index, answers one question (or prints an overview), then removes the scratch
// dir. The target is never touched: no .prowl/, .gitignore, or AGENTS.md is
// written into it. Use it to review or extract from an outside repo.
func newExploreCmd() *cobra.Command {
	var question string
	var budgetTokens int
	c := &cobra.Command{
		Use:   "explore <path>",
		Short: "Index a repo you do not own into a scratch index, answer, then clean up",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			root, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if fi, err := os.Stat(root); err != nil {
				return fmt.Errorf("explore target %q: %w", args[0], err)
			} else if !fi.IsDir() {
				return fmt.Errorf("explore target %q is not a directory", args[0])
			}

			// Index into an ephemeral scratch dir, never into the target. This is
			// what keeps the target pristine: the index.db lives here, not under a
			// .prowl/ in the repo, and nothing writes .gitignore or AGENTS.md into
			// it. The dir (and its WAL sidecars) is removed on return.
			scratch, err := os.MkdirTemp("", "prowl-explore-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(scratch)

			s, err := store.Open(filepath.Join(scratch, "index.db"))
			if err != nil {
				return err
			}
			defer s.Close()

			if _, err := index.IndexWithOptionsContext(cmd.Context(), s, root, index.Options{}); err != nil {
				return err
			}

			var res any
			if question != "" {
				// Bounded, cited context packet, built directly from the scratch
				// store and the target root (no application.OpenProject, which
				// would write .prowl into the root).
				service := &contextpacket.Service{Store: s, Root: root}
				packet, err := service.Search(contextpacket.Request{
					Question:     question,
					Mode:         contextpacket.ModeCompact,
					BudgetTokens: budgetTokens,
				})
				if err != nil {
					return err
				}
				res = packet
			} else {
				overview, err := query.New(s).Overview()
				if err != nil {
					return err
				}
				res = overview
			}

			str, err := formatValue(res, format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
			return err
		},
	}
	c.Flags().StringVar(&question, "question", "", "build a bounded, cited context packet for this question")
	c.Flags().IntVar(&budgetTokens, "budget-tokens", 1800, "estimated token budget for --question")
	return c
}
