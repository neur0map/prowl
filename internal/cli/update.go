package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/selfupdate"
)

func newUpdateCmd(_ string, managedBy string, embedded bool) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update Prowl to the latest build and restart running servers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// Embedded in a host program, the running executable is the host, not
			// Prowl: a self-update would download over and replace a binary the
			// host owns. Refuse before touching os.Executable and defer to the host.
			if embedded {
				fmt.Fprintln(out, "prowl update is unavailable while running embedded in a host program; the host manages the Prowl build")
				return nil
			}
			// A packaged or system-installed binary must not self-update: defer to
			// the package manager instead of downloading over a file the user does
			// not own.
			if msg, managed := selfupdate.Managed(managedBy); managed {
				fmt.Fprintln(out, msg)
				return nil
			}
			uiLog.Info("checking for the latest build")
			msg, err := selfupdate.Apply()
			if err != nil {
				return err
			}
			fmt.Fprintln(out, msg)
			// Recycle every running serve/lsp so the agent/editor relaunches the
			// new binary; update replaces the one binary they all share.
			if n := stopServers(findProwlServers("")); n > 0 {
				uiLog.Infof("recycled %d running server(s); your agent or editor relaunches the new binary on next use", n)
			}
			return nil
		},
	}
}
