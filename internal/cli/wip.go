package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/wip"
)

// newWipCmd reports uncommitted work: the files touched since the last commit,
// the unfinished-work markers left inside them (TODO, FIXME, and friends), and
// the blast radius of each indexed file. It is the "where did I leave off?"
// command, so an agent can resume a session without re-reading the whole tree.
func newWipCmd() *cobra.Command {
	var markers string
	c := &cobra.Command{
		Use:   "wip",
		Short: "Investigate uncommitted work: touched files, unfinished markers, and their blast radius",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := resolveFormat(cmd, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			q, ws, s, closer, err := openQuerier(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closer()

			indexed := map[string]bool{}
			if sizes, err := s.FileSizes(); err == nil {
				for path := range sizes {
					indexed[path] = true
				}
			}
			opts := wip.Options{}
			if trimmed := strings.TrimSpace(markers); trimmed != "" {
				opts.Markers = splitCommaList(trimmed)
			}
			report, err := wip.Investigate(cmd.Context(), ws.Root, indexed, q, opts)
			if err != nil {
				return err
			}
			_ = s.RecordAnswer(report)
			str, err := formatValue(report, format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
			return err
		},
	}
	c.Flags().StringVar(&markers, "markers", "", "comma-separated markers to scan for (default TODO,FIXME,HACK,XXX,BUG,WIP,OPTIMIZE)")
	return c
}

func splitCommaList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
