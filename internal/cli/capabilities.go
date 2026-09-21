package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/capability"
	"github.com/neur0map/prowl/internal/embed"
)

func newCapabilitiesCmd() *cobra.Command {
	command := &cobra.Command{Use: "capabilities", Short: "Discover Prowl workflows and the commands that run them"}
	command.AddCommand(newCapabilitiesSearchCmd(nil), newCapabilitiesGetCmd(nil))
	return command
}

func newCapabilitiesSearchCmd(catalog *capability.Catalog) *cobra.Command {
	command := &cobra.Command{
		Use:   "search [intent]",
		Short: "Find the right workflow (and its Prowl commands) by intent",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := resolveCapabilityCatalog(catalog)
			if err != nil {
				return err
			}
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			// Rank with the bundled embedder when the agent describes an intent;
			// fall back to lexical matching (and for the bare listing).
			var matches []capability.Summary
			if query != "" {
				matches = semanticCapabilities(command.Context(), resolved, query, 10)
			}
			if matches == nil {
				matches = resolved.Search(query, 10)
			}
			format, err := resolveFormat(command, command.OutOrStdout())
			if err != nil {
				return err
			}
			if format == formatJSON {
				return writeJSON(command.OutOrStdout(), matches)
			}
			if len(matches) == 0 {
				_, err = fmt.Fprintln(command.OutOrStdout(), "No matching capabilities.")
				return err
			}
			return writeCapabilitySummaries(command.OutOrStdout(), matches)
		},
	}
	return command
}

func newCapabilitiesGetCmd(catalog *capability.Catalog) *cobra.Command {
	command := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a workflow's CLI commands, plus its MCP tools, resources, and privacy",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			resolved, err := resolveCapabilityCatalog(catalog)
			if err != nil {
				return err
			}
			manifest, ok := resolved.Get(args[0])
			if !ok {
				fmt.Fprintf(command.ErrOrStderr(), "hint: unknown capability %q; run 'prowl capabilities search \"<intent>\"' to list workflows\n", args[0])
				return fmt.Errorf("capability not found: %s", args[0])
			}
			format, err := resolveFormat(command, command.OutOrStdout())
			if err != nil {
				return err
			}
			if format == formatJSON {
				return writeJSON(command.OutOrStdout(), manifest)
			}
			w := command.OutOrStdout()
			if _, err := fmt.Fprintf(w, "%s - %s\n%s\n\nCommands:\n", manifest.Name, manifest.Title, manifest.Description); err != nil {
				return err
			}
			for _, recipe := range manifest.Commands {
				if _, err := fmt.Fprintf(w, "  $ %s\n", recipe); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(w, "\nMCP tools: %v\nResources: %v\nOutputs: %v\nPrivacy: %s\nRead only: %t\nVersion: %s\n",
				manifest.Tools, manifest.Resources, manifest.Outputs, manifest.Privacy, manifest.ReadOnly, manifest.Version)
			return err
		},
	}
	return command
}

// writeCapabilitySummaries renders discovery results CLI-first: each workflow's
// runnable prowl commands sit right under its description, so the agent's
// next action is a command it can paste, not an MCP tool name.
func writeCapabilitySummaries(w io.Writer, summaries []capability.Summary) error {
	for _, summary := range summaries {
		if _, err := fmt.Fprintf(w, "%s - %s\n  %s\n", summary.Name, summary.Title, summary.Description); err != nil {
			return err
		}
		for _, recipe := range summary.Commands {
			if _, err := fmt.Fprintf(w, "  $ %s\n", recipe); err != nil {
				return err
			}
		}
	}
	return nil
}

// semanticCapabilities ranks the catalog against a natural-language intent using
// the bundled in-process embedder, so an agent can ask "what breaks if I change
// a file" and land on the inspect-structure workflow without knowing command
// names. This is the bundled model applied where it is cheapest and most useful:
// a tiny fixed catalog, embedded offline with no repo-vector cold start. Any
// failure returns nil, so the caller falls back to lexical matching.
func semanticCapabilities(ctx context.Context, catalog *capability.Catalog, query string, limit int) []capability.Summary {
	model, err := embed.Load()
	if err != nil {
		return nil
	}
	all := catalog.All()
	if len(all) == 0 {
		return nil
	}
	texts := make([]string, 0, len(all)+1)
	texts = append(texts, query)
	for _, manifest := range all {
		texts = append(texts, manifest.Title+". "+manifest.Description+" "+strings.Join(manifest.Triggers, ", "))
	}
	vectors, err := model.Embed(ctx, texts)
	if err != nil || len(vectors) != len(all)+1 {
		return nil
	}
	queryVec := vectors[0]
	type scored struct {
		manifest capability.Manifest
		score    float32
	}
	ranked := make([]scored, len(all))
	for i, manifest := range all {
		ranked[i] = scored{manifest, dot(queryVec, vectors[i+1])}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]capability.Summary, len(ranked))
	for i, entry := range ranked {
		out[i] = entry.manifest.Summary()
	}
	return out
}

// dot is the similarity between two embeddings. The bundled model L2-normalizes
// its output, so the dot product is the cosine similarity.
func dot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float32
	for i := 0; i < n; i++ {
		sum += a[i] * b[i]
	}
	return sum
}

func resolveCapabilityCatalog(catalog *capability.Catalog) (*capability.Catalog, error) {
	if catalog != nil {
		return catalog, nil
	}
	return capability.BuiltinCatalog()
}

func writeJSON(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(encoded))
	return err
}
