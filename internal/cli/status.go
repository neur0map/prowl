package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/selfupdate"
	"github.com/neur0map/prowl/internal/workspace"
)

func newStatusCmd(version string) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show index status, token savings, and update availability",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			ws, err := workspace.Resolve(".")
			if err == workspace.ErrNotFound {
				entries, _ := workspace.List()
				if asJSON {
					return json.NewEncoder(out).Encode(entries)
				}
				if len(entries) == 0 {
					fmt.Fprintln(out, "No Prowl projects yet. Run 'prowl init'.")
					return nil
				}
				fmt.Fprintln(out, "Prowl projects:")
				for _, e := range entries {
					fmt.Fprintf(out, "  %s  (ai=%v)\n", e.Root, e.AI)
				}
				if _, combined := aggregateSavings(); combined.Queries > 0 {
					fmt.Fprintf(out, "\nCombined savings: ~%s tokens across %d answers (estimated)\n",
						humanTokens(combined.SavedTokens), combined.Queries)
				}
				fmt.Fprintf(out, "Measure it yourself: %s\n", tokensDocURL)
				return nil
			}
			if err != nil {
				return err
			}
			project, err := application.OpenProject(cmd.Context(), ws.Root, application.Options{})
			if err != nil {
				return err
			}
			defer project.Close()
			st, err := project.Query.Status()
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(out).Encode(st)
			}
			upd := selfupdate.Check(version)
			perProject, combined := aggregateSavings()
			for _, w := range unindexedLanguageWarnings(ws.Root) {
				fmt.Fprintf(out, "Warning: %s\n", w)
			}
			if f, ok := out.(*os.File); ok && isTTY(f) {
				fmt.Fprintln(out, renderStatusCard(version, ws.Root, filepath.Base(ws.Root), st, upd, perProject, combined))
				return nil
			}
			printPlainStatus(out, ws.Root, st, upd, combined)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return c
}

func printPlainStatus(out io.Writer, root string, st query.Status, upd selfupdate.Result, combined query.Savings) {
	c := st.Counts
	fmt.Fprintf(out, "Project:   %s\n", root)
	fmt.Fprintf(out, "Files:     %d\n", c.Files)
	fmt.Fprintf(out, "Symbols:   %d\n", c.Symbols)
	fmt.Fprintf(out, "Edges:     %d (resolved %d, external %d, unresolved %d)\n", c.Edges, c.Resolved, c.External, c.Unresolved)
	fmt.Fprintf(out, "Resources: %d\n", c.Resources)
	langs := make([]string, 0, len(c.Langs))
	for l := range c.Langs {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	fmt.Fprint(out, "Languages: ")
	for _, l := range langs {
		fmt.Fprintf(out, "%s=%d ", l, c.Langs[l])
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "AI-assist: %v\n", st.AIEnabled)
	if sem := st.Semantic; st.AIEnabled && sem.Chunks > 0 {
		if sem.Complete {
			fmt.Fprintf(out, "Semantic:  fully built (%d chunks embedded)\n", sem.Embedded)
		} else {
			fmt.Fprintf(out, "Semantic:  building, %d of %d chunks embedded, %d to go (lexical search covers the rest; 'prowl init' continues it)\n",
				sem.Embedded, sem.Chunks, sem.Remaining)
		}
	}
	if st.Savings.Queries > 0 {
		fmt.Fprintf(out, "Saved:     ~%s tokens across %d answers (estimated)\n",
			humanTokens(st.Savings.SavedTokens), st.Savings.Queries)
	}
	if combined.Queries > st.Savings.Queries {
		fmt.Fprintf(out, "Combined:  ~%s tokens across all your projects (estimated)\n",
			humanTokens(combined.SavedTokens))
	}
	if upd.Available {
		fmt.Fprintln(out, "Update:    available. Run 'prowl update'.")
	} else if upd.Checked {
		fmt.Fprintln(out, "Update:    up to date.")
	}
	fmt.Fprintf(out, "Verify:    %s\n", tokensDocURL)
}
