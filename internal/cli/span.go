package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSpanCmd reports a symbol's current file and line range plus a content
// digest, so an agent whose line numbers went stale after its own edits can
// cheaply re-ground -- and tell whether the body it planned to edit actually
// drifted -- without re-reading the file. When a name is ambiguous it lists
// every match so the choice is visible. It needs the workspace root to read the
// current bytes for the digest, so (like def and peek) it is not built from the
// generic helper.
func newSpanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "span <name-or-id>",
		Short: "Where a symbol currently is: file, line range, and a digest to detect drift after edits",
		Args:  cobra.ExactArgs(1),
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
			spans, err := q.Span(ws.Root, a[0])
			if err != nil {
				return err
			}
			if len(spans) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "hint: no symbol %q; 'prowl find %s' lists candidates\n", a[0], a[0])
			}
			_ = s.RecordAnswer(spans)
			str, err := formatValue(spans, format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
			return err
		},
	}
	return c
}
