package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/docs"
)

func newDocsCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "docs",
		Short: "Crawl and search external documentation",
		Long: "Ingest documentation sites (or local Markdown trees) into a shared, " +
			"indexed corpus and search it with the same bounded, cited retrieval used for code.",
	}
	command.AddCommand(newDocsAddCmd(), newDocsListCmd(), newDocsRemoveCmd(), newDocsSearchCmd())
	return command
}

func newDocsAddCmd() *cobra.Command {
	var name string
	var depth, maxPages int
	var rate float64
	var local, noLLMS bool
	command := &cobra.Command{
		Use:   "add <url-or-path>",
		Short: "Crawl a docs site or ingest a local Markdown tree",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := docs.Home()
			if err != nil {
				return err
			}
			target := args[0]
			isURL := strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
			out := command.OutOrStdout()
			if isURL && !local {
				u, err := url.Parse(target)
				if err != nil {
					return fmt.Errorf("invalid URL: %w", err)
				}
				cfg := docs.CrawlConfig{
					MaxDepth: depth, MaxPages: maxPages, Rate: rate, SamePathPrefix: true, NoLLMS: noLLMS,
					Progress: func(pages, quarantined int, _ string) {
						if pages%10 == 0 && pages > 0 {
							fmt.Fprintf(command.ErrOrStderr(), "\rcrawled %d pages (%d quarantined)...", pages, quarantined)
						}
					},
				}
				res, err := docs.AddRemote(command.Context(), home, name, u, cfg)
				if err != nil {
					return err
				}
				fmt.Fprintf(command.ErrOrStderr(), "\r")
				fmt.Fprintf(out, "Added docs source %q: %d pages indexed", res.Source.Name, res.Source.Pages)
				if res.Source.Quarantined > 0 {
					fmt.Fprintf(out, " (%d quarantined)", res.Source.Quarantined)
				}
				fmt.Fprintln(out)
				return nil
			}
			res, err := docs.AddLocal(command.Context(), home, name, target)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Ingested docs source %q: %d files indexed", res.Source.Name, res.Source.Pages)
			if res.Source.Quarantined > 0 {
				fmt.Fprintf(out, " (%d quarantined)", res.Source.Quarantined)
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	command.Flags().StringVar(&name, "name", "", "source name (default: host or directory name)")
	command.Flags().IntVar(&depth, "depth", 3, "maximum crawl depth (remote only)")
	command.Flags().IntVar(&maxPages, "max-pages", 200, "maximum pages to crawl (remote only)")
	command.Flags().Float64Var(&rate, "rate", 5, "requests per second (remote only)")
	command.Flags().BoolVar(&local, "local", false, "treat the argument as a local directory")
	command.Flags().BoolVar(&noLLMS, "no-llms", false, "skip the llms.txt / llms-full.txt fast path (remote only)")
	return command
}

func newDocsListCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List ingested documentation sources",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := docs.Home()
			if err != nil {
				return err
			}
			m, err := docs.LoadManifest(home)
			if err != nil {
				return err
			}
			out := command.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(m.Sources)
			}
			if len(m.Sources) == 0 {
				fmt.Fprintln(out, "No documentation sources. Add one with `prowl docs add <url>`.")
				return nil
			}
			for _, s := range m.Sources {
				origin := s.URL
				if origin == "" {
					origin = s.Path
				}
				fmt.Fprintf(out, "%-24s %d pages  %s\n", s.Name, s.Pages, origin)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newDocsRemoveCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an ingested documentation source",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := docs.Home()
			if err != nil {
				return err
			}
			src, err := docs.Remove(command.Context(), home, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "Removed docs source %q (%d pages)\n", src.Name, src.Pages)
			return nil
		},
	}
	return command
}

func newDocsSearchCmd() *cobra.Command {
	var budgetTokens int
	var asJSON bool
	command := &cobra.Command{
		Use:   "search <question>",
		Short: "Retrieve bounded context from ingested documentation",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			home, err := docs.Home()
			if err != nil {
				return err
			}
			packet, err := docs.Search(home, args[0], budgetTokens)
			if err != nil {
				return err
			}
			return writeContextPacket(command, packet, asJSON)
		},
	}
	command.Flags().IntVar(&budgetTokens, "budget-tokens", 1800, "estimated token budget")
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}
