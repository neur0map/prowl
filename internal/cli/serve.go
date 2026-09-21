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
	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/embed"
	mcpserver "github.com/neur0map/prowl/internal/mcp"
)

// loadEmbedder loads the in-process, binary-bundled static embedder. It is a
// package variable so tests can stub the embedding backend.
var loadEmbedder = func(context.Context) (assist.Embedder, error) {
	m, err := embed.Load()
	if err != nil {
		return nil, err
	}
	return m, nil
}

// maybeInferencer resolves the semantic-assist backend. Embeddings always come
// from the binary-bundled static code embedder (internal/embed): it is in-process,
// needs no daemon, and measures ~650 chunks/s against ~47 chunks/s for a remote
// Ollama embed model, which is the difference between a semantic index that
// builds in two minutes and one that takes half an hour on a large repo. It is
// also code-tuned, where the Ollama presets are general-purpose text embedders.
//
// Just as importantly, stored vectors are keyed by their producing model, so
// choosing the embedder by "is Ollama up right now" made the whole vector index
// silently invalidate and rebuild whenever the daemon or a model came and went.
// One embedder, always, keeps the vector space stable.
//
// Generation and rerank (the --smart half) genuinely need an LLM: Ollama when
// reachable, else a coding-agent CLI, else nothing (vector+FTS still works).
func maybeInferencer(ctx context.Context, cfg config.Config) assist.Inferencer {
	if !cfg.AI.Enabled {
		return nil
	}
	var helper assist.Inferencer
	if cfg.AI.Provider != "agent" && cfg.AI.AssistModel != "" {
		oll := assist.NewOllama(cfg.AI.OllamaURL, cfg.AI.AssistModel)
		if oll.Available(ctx) && oll.HasModel(ctx, cfg.AI.AssistModel) {
			helper = oll
		}
	}
	if helper == nil {
		cmd := cfg.AI.AgentCommand
		if cmd == "" {
			cmd = detectAgentCLI()
		}
		if cmd != "" {
			if a := assist.NewAgentCLI(cmd); a.Available(ctx) {
				helper = a
			}
		}
	}
	m, err := loadEmbedder(ctx)
	if err == nil {
		return assist.Composite{Emb: m, Assist: helper}
	}
	if helper != nil {
		fmt.Fprintf(os.Stderr, "prowl: built-in embedder unavailable (%v); using rerank without embeddings\n", err)
		return helper
	}
	fmt.Fprintf(os.Stderr, "prowl: built-in embedder unavailable (%v); structural search only\n", err)
	return nil
}

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
	return application.OpenProject(ctx, ".", application.Options{EnableAI: true, InferencerProvider: maybeInferencer, VectorProgress: semanticBuildReporter(os.Stderr)})
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
