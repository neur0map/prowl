package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/query"
)

// newHistoryCmd reports the commits that touched a symbol -- the "why is this
// code the way it is" lookup that, in practice, is answered with raw `git log
// -L` by hand. It resolves the symbol's CURRENT line range from the index and
// asks git for the commits over that range, newest first. Like def, span, and
// peek it needs the workspace root (to run git there), so it is not built from
// the generic helper. A --limit caps how many commits a symbol returns so a
// long-lived one does not flood the context.
func newHistoryCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "history <name-or-id>",
		Short: "Commits that touched a symbol: why the code is the way it is",
		Long: "Commits that touched a symbol, newest first: short SHA, author, relative date, and subject.\n\n" +
			"The range traced is the symbol's CURRENT location in the index, not where it lived in the past, so\n" +
			"the history is exact about the lines the symbol occupies now. Because `git log -L` cannot combine with\n" +
			"`--follow`, a symbol whose file was renamed has its history truncated at the rename -- commits from\n" +
			"before the file's current path are not reached. When a name matches several symbols, every exact match\n" +
			"is traced and each commit is tagged with its file so the choice is visible.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			format, err := resolveFormat(cmd, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			q, ws, s, closer, err := openQuerier(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closer()
			commits, err := q.History(cmd.Context(), ws.Root, a[0], limit)
			if err != nil {
				return err
			}
			if len(commits) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "hint: no commit history for %q; the symbol may be unknown ('prowl find %s'), its file untracked, this not a git repository, or the history walk timed out on a very large repository\n", a[0], a[0])
			} else if files := distinctFiles(commits); len(files) > 1 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %q matches %d symbols (%v); history for each is shown, tagged by file\n", a[0], len(files), files)
			}
			_ = s.RecordAnswer(commits)
			str, err := formatValue(commits, format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
			return err
		},
	}
	c.Flags().IntVar(&limit, "limit", 0, fmt.Sprintf("cap commits per symbol to N (0 = default %d)", query.DefaultHistoryLimit))
	return c
}

// distinctFiles lists, in first-seen order, the files a history result spans, so
// an ambiguous name's separate symbols can be named in a single hint.
func distinctFiles(commits []query.SymbolCommit) []string {
	seen := make(map[string]bool, len(commits))
	var files []string
	for _, c := range commits {
		if !seen[c.File] {
			seen[c.File] = true
			files = append(files, c.File)
		}
	}
	return files
}
