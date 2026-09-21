package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	contextpacket "github.com/neur0map/prowl/internal/context"
)

// defaultBenchQuestions are generic "how/where does X work" questions that most
// codebases can answer, so `bench` is meaningful out of the box; override with
// --questions or positional args for a repo-specific run.
var defaultBenchQuestions = []string{
	"how is the project structured",
	"where is the program entrypoint",
	"how is configuration loaded",
	"how are errors handled",
	"how is data stored or indexed",
	"how does search work",
}

type benchRow struct {
	Question     string `json:"question"`
	PacketTokens int    `json:"packet_tokens"`
	CitedFiles   int    `json:"cited_files"`
	CitedTokens  int    `json:"cited_file_tokens"`
}

type benchReport struct {
	Repo         string     `json:"repo"`
	Files        int        `json:"files"`
	RepoTokens   int        `json:"repo_tokens"`
	Rows         []benchRow `json:"questions"`
	PacketTotal  int        `json:"packet_total"`
	CitedTotal   int        `json:"cited_file_total"`
	VsCitedRatio float64    `json:"vs_cited_files_ratio"`
	VsRepoRatio  float64    `json:"vs_whole_repo_ratio"`
}

// newBenchCmd measures Prowl's token efficiency the honest, reproducible way and
// with zero external services: for each question, the cost of Prowl's cited
// packet versus reading the files it cites whole, and versus loading the whole
// repo (the "put everything in context" baseline that vector-DB tools bill for).
func newBenchCmd() *cobra.Command {
	var asJSON bool
	var questionsFile string
	var budget int
	c := &cobra.Command{
		Use:   "bench [question...]",
		Short: "Measure token efficiency: cited packets vs reading cited files vs the whole repo (no API keys, no cloud)",
		RunE: func(cmd *cobra.Command, args []string) error {
			questions := defaultBenchQuestions
			if questionsFile != "" {
				data, err := os.ReadFile(questionsFile)
				if err != nil {
					return err
				}
				questions = nil
				for _, ln := range strings.Split(string(data), "\n") {
					if s := strings.TrimSpace(ln); s != "" {
						questions = append(questions, s)
					}
				}
			}
			if len(args) > 0 {
				questions = args
			}
			svc, closer, err := openContextService(cmd.Context())
			if err != nil {
				return err
			}
			defer closer()

			est := contextpacket.ByteQuarterEstimator{}
			rep := benchReport{Repo: filepath.Base(svc.Root)}
			files, err := svc.Store.AllFiles()
			if err != nil {
				return err
			}
			rep.Files = len(files)
			for _, f := range files {
				rep.RepoTokens += (int(f.Size) + 3) / 4
			}
			for _, q := range questions {
				pkt, err := svc.Search(contextpacket.Request{Question: q, Mode: contextpacket.ModeCompact, BudgetTokens: budget})
				if err != nil {
					continue
				}
				cited := map[string]bool{}
				for _, it := range pkt.Items {
					for _, ci := range it.Citations {
						if ci.Path != "" {
							cited[ci.Path] = true
						}
					}
				}
				citedTokens := 0
				for p := range cited {
					if data, err := os.ReadFile(filepath.Join(svc.Root, p)); err == nil {
						citedTokens += est.Tokens(string(data))
					}
				}
				rep.Rows = append(rep.Rows, benchRow{Question: q, PacketTokens: pkt.Budget.EstimatedTokens, CitedFiles: len(cited), CitedTokens: citedTokens})
				rep.PacketTotal += pkt.Budget.EstimatedTokens
				rep.CitedTotal += citedTokens
			}
			if rep.PacketTotal > 0 {
				rep.VsCitedRatio = float64(rep.CitedTotal) / float64(rep.PacketTotal)
				rep.VsRepoRatio = float64(rep.RepoTokens) / float64(rep.PacketTotal/max(1, len(rep.Rows)))
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			fmt.Fprintf(out, "Repo: %s - %d files, ~%s tokens total\n\n", rep.Repo, rep.Files, humanInt(rep.RepoTokens))
			fmt.Fprintf(out, "%-40s %10s %12s %10s\n", "question", "prowl", "read-files", "reduction")
			for _, r := range rep.Rows {
				red := 0.0
				if r.CitedTokens > 0 {
					red = 100 * (1 - float64(r.PacketTokens)/float64(r.CitedTokens))
				}
				fmt.Fprintf(out, "%-40s %10s %12s %9.0f%%\n", clipStr(r.Question, 40), humanInt(r.PacketTokens), humanInt(r.CitedTokens), red)
			}
			fmt.Fprintf(out, "%-40s %10s %12s\n", strings.Repeat("-", 20), "", "")
			totalRed := 0.0
			if rep.CitedTotal > 0 {
				totalRed = 100 * (1 - float64(rep.PacketTotal)/float64(rep.CitedTotal))
			}
			fmt.Fprintf(out, "%-40s %10s %12s %9.0f%%\n", fmt.Sprintf("totals (%d questions)", len(rep.Rows)), humanInt(rep.PacketTotal), humanInt(rep.CitedTotal), totalRed)
			fmt.Fprintln(out)
			if rep.VsCitedRatio > 0 {
				fmt.Fprintf(out, "Reading the cited files costs %.1fx more than Prowl's cited packets.\n", rep.VsCitedRatio)
			}
			if rep.VsRepoRatio > 0 {
				fmt.Fprintf(out, "Loading the whole repo for one question costs ~%.0fx more than one Prowl answer.\n", rep.VsRepoRatio)
			}
			fmt.Fprintln(out, "No API keys, no cloud, no embeddings required (deterministic full-text; local vectors when AI is enabled).")
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	c.Flags().StringVar(&questionsFile, "questions", "", "file with one question per line")
	c.Flags().IntVar(&budget, "budget-tokens", 1800, "token budget per packet")
	return c
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
