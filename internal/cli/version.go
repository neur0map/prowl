package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/selfupdate"
)

// newVersionCmd prints the running version and, when the daily cached check has
// run, whether a newer build is available.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and whether an update is available",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "prowl %s\n", version)
			if r := selfupdate.Check(version); r.Available {
				fmt.Fprintln(out, "update available; run 'prowl update'")
			}
			return nil
		},
	}
}
