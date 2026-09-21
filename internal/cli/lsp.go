package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/index"
	lspserver "github.com/neur0map/prowl/internal/lsp"
)

func openLSPProject(ctx context.Context) (*application.Project, error) {
	return application.OpenProject(ctx, ".", application.Options{
		EnableAI: true, InferencerProvider: maybeInferencer, VectorProgress: semanticBuildReporter(os.Stderr),
	})
}

func runLSP(ctx context.Context, srv *lspserver.Server, stdin *os.File, stdout io.Writer) error {
	input, err := lspserver.NewCancellableInput(ctx, stdin)
	if err != nil {
		return fmt.Errorf("prepare cancellable LSP input: %w", err)
	}
	defer input.Close()
	return srv.Run(ctx, input, stdout)
}

// newLSPCmd is hidden: editors launch it via the config that init writes. It
// serves the same per-project index as `serve`, but over LSP for a human editor.
func newLSPCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:    "lsp",
		Short:  "Run the language server over stdio (launched by your editor)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprintln(os.Stderr, "prowl lsp is a language server; your editor launches it over stdin/stdout.")
				fmt.Fprintln(os.Stderr, "You do not run it by hand. See .prowl/editor/SETUP.md for editor setup.")
				return nil
			}
			project, err := openLSPProject(cmd.Context())
			if err != nil {
				return err
			}
			defer project.Close()
			ws, s := project.Workspace, project.Store
			rules, err := config.LoadRules(ws.Path)
			if err != nil {
				return err
			}

			reindex := func() error {
				_, err := project.Refresh(cmd.Context())
				return err
			}

			srv := lspserver.New(ws.Root, version, s, rules, reindex).WithReadGuard(project.ReadGuard)
			// External edits (agent, git, formatter): reindex and refresh squiggles.
			watchCtx, cancelWatch := context.WithCancel(cmd.Context())
			var watchGroup sync.WaitGroup
			watchGroup.Add(1)
			defer func() {
				cancelWatch()
				watchGroup.Wait()
			}()
			go func() {
				defer watchGroup.Done()
				watchErr := index.Watch(watchCtx, ws.Root, 750*time.Millisecond, func() {
					refreshID := srv.BeginIndexRefresh()
					_, err := project.Refresh(watchCtx)
					if !srv.CompleteIndexRefresh(refreshID, err) || err != nil {
						return
					}
					srv.RepublishOpen()
				})
				if watchErr != nil && !errors.Is(watchErr, context.Canceled) {
					srv.SetIndexError(watchErr)
				}
			}()
			return runLSP(cmd.Context(), srv, os.Stdin, os.Stdout)
		},
	}
}
