package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	mcpserver "github.com/neur0map/prowl/internal/mcp"
)

// reindexer formats the shared Project refresh operation for MCP and restart.
func reindexer(project *application.Project) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		result, err := project.Refresh(ctx)
		if err != nil {
			return "", err
		}
		sum := result.Summary
		msg := fmt.Sprintf("indexed=%d parsed=%d skipped=%d deleted=%d", sum.Indexed, sum.Parsed, sum.Skipped, sum.Deleted)
		if result.EmbeddingError != nil {
			msg += fmt.Sprintf(" embed_error=%q", result.EmbeddingError.Error())
		} else if project.Inferencer != nil {
			msg += fmt.Sprintf(" embedded=%d", result.Embedded)
		}
		return msg, nil
	}
}

func openServeProject(ctx context.Context) (*application.Project, error) {
	return application.OpenProject(ctx, ".", application.Options{EnableAI: true, InferencerProvider: application.DefaultInferencer, VectorProgress: semanticBuildReporter(os.Stderr)})
}

// newServeCmd is hidden: agents launch it via the injected .mcp.json.
func newServeCmd(version string) *cobra.Command {
	var surfaceName string
	command := &cobra.Command{
		Use:    "serve",
		Short:  "Run the MCP server over stdio (launched by coding agents)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			surface, err := mcpserver.ParseSurface(surfaceName)
			if err != nil {
				return err
			}
			project, err := openServeProject(cmd.Context())
			if err != nil {
				return err
			}
			defer project.Close()
			reindex := reindexer(project)
			doctorFn := func(ctx context.Context) (doctor.Report, error) {
				release, err := project.ReadGuard(ctx)
				if err != nil {
					return doctor.Report{}, err
				}
				defer release()
				if err := project.Store.RequirePublishedGeneration(); err != nil {
					return doctor.Report{}, err
				}
				rules, err := config.LoadRules(project.Workspace.Path)
				if err != nil {
					return doctor.Report{}, err
				}
				return doctor.Run(project.Store, rules, doctor.Options{Root: project.Workspace.Root})
			}
			fresh := newFreshness(cmd.Context(), project.Workspace.Root, reindex)
			fresh.start()
			defer fresh.stop()
			srv := mcpserver.NewServerWithOptions(project.Query, project.Store, version, reindex, doctorFn, nil, mcpserver.ServerOptions{
				Surface: surface, Context: project.Context, Knowledge: project.Knowledge, Capabilities: project.Capabilities, Root: project.Workspace.Root,
				BeforeCall: fresh.onCall,
			})
			// A clean client disconnect surfaces as EOF / "closing"; treat it as success.
			if err := mcpserver.Serve(cmd.Context(), srv); err != nil &&
				!errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) &&
				!strings.Contains(err.Error(), "closing") {
				return err
			}
			return nil
		},
	}
	command.Flags().StringVar(&surfaceName, "mcp-surface", string(mcpserver.SurfaceLegacy), "MCP tool surface: legacy, core, or all")
	return command
}
