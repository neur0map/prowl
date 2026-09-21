package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/neur0map/prowl/internal/revieweval"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("review-eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var cfg revieweval.RunConfig
	var clients, phase string
	flags.StringVar(&phase, "phase", "collect", "phase: collect trials, then score retained collection")
	flags.StringVar(&clients, "client", "", "optional comma-separated clients in exact frozen order")
	flags.StringVar(&clients, "clients", "", "optional comma-separated clients in exact frozen order")
	flags.StringVar(&cfg.Model, "model", "", "frozen model identifier")
	flags.IntVar(&cfg.Repetitions, "repetitions", 0, "optional repetitions override; must match frozen config")
	flags.StringVar(&cfg.Set, "set", "tuning", "evaluation set: tuning or held_out")
	flags.StringVar(&cfg.ManifestPath, "manifest", "testdata/review-eval/manifest.json", "evaluation manifest")
	flags.StringVar(&cfg.ScoringConfigPath, "scoring-config", "testdata/review-eval/scoring.json", "frozen scoring configuration")
	flags.StringVar(&cfg.ScoringConfigPath, "config", "testdata/review-eval/scoring.json", "frozen scoring configuration")
	flags.StringVar(&cfg.CollectionPath, "collection", "", "score phase retained review.eval-collection.v1 file")
	flags.StringVar(&cfg.AdjudicationPath, "adjudication", "", "score phase frozen blind-adjudication matrix")
	flags.StringVar(&cfg.OutputDir, "output", "", "required local relative artifact directory")
	flags.StringVar(&cfg.PreparedRoot, "prepared-root", ".", "root containing offline prepared repositories")
	flags.StringVar(&cfg.ReviewSkill, "review-skill", "skills/prowl-pr-review", "review skill installed only for treatment")
	flags.StringVar(&cfg.ProwlBinary, "prowl", "prowl", "built Prowl binary")
	flags.StringVar(&cfg.ProwlBinary, "prowl-binary", "prowl", "built Prowl binary")
	flags.StringVar(&cfg.ClaudeBinary, "claude", "claude", "Claude binary")
	flags.StringVar(&cfg.ClaudeBinary, "claude-binary", "claude", "Claude binary")
	flags.StringVar(&cfg.OMPBinary, "omp", "omp", "OMP binary")
	flags.StringVar(&cfg.OMPBinary, "omp-binary", "omp", "OMP binary")
	flags.DurationVar(&cfg.Timeout, "timeout", 0, "wall-time cap override; must match frozen config")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if cfg.OutputDir == "" {
		fmt.Fprintln(stderr, "error: --output is required")
		return 2
	}
	if phase != "collect" && phase != "score" {
		fmt.Fprintln(stderr, "error: --phase must be collect or score")
		return 2
	}
	if phase == "score" && (cfg.CollectionPath == "" || cfg.AdjudicationPath == "") {
		fmt.Fprintln(stderr, "error: score phase requires --collection and --adjudication")
		return 2
	}
	cfg.Production = cfg.Set == "held_out"
	if cfg.Production && cfg.Repetitions != 0 && cfg.Repetitions != 3 {
		fmt.Fprintln(stderr, "error: production held_out repetitions must equal 3")
		return 2
	}
	for _, client := range strings.Split(clients, ",") {
		if client = strings.TrimSpace(client); client != "" {
			cfg.Clients = append(cfg.Clients, client)
		}
	}
	manifest, err := revieweval.LoadManifest(cfg.ManifestPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	scoring, err := revieweval.LoadScoringConfig(cfg.ScoringConfigPath, manifest, cfg.Production)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cfg.Model != "" && cfg.Model != scoring.Model {
		fmt.Fprintln(stderr, "error: --model differs from frozen scoring model")
		return 2
	}
	if cfg.Timeout > 0 && cfg.Timeout != scoring.Budget.Timeout {
		fmt.Fprintln(stderr, "error: --timeout differs from frozen budget")
		return 2
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if phase == "collect" {
		collection, err := revieweval.Collect(context.Background(), cfg, manifest, scoring)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		summary := struct {
			Schema          string `json:"schema"`
			Trials          int    `json:"trials"`
			CandidateDigest string `json:"candidate_digest"`
		}{collection.Schema, len(collection.Trials), collection.CandidateDigest}
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	collection, err := revieweval.LoadCollection(cfg.CollectionPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	adjudication, err := revieweval.LoadAdjudication(cfg.AdjudicationPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	report, err := revieweval.ScoreCollection(cfg, manifest, scoring, collection, adjudication)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	summary := struct {
		Control   revieweval.Metric        `json:"control"`
		Treatment revieweval.Metric        `json:"treatment"`
		Gates     revieweval.ShippingGates `json:"gates"`
	}{report.Metrics.Conditions[revieweval.ConditionControl], report.Metrics.Conditions[revieweval.ConditionTreatment], report.Gates}
	if err := encoder.Encode(summary); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if cfg.Production && !report.Gates.Passed {
		return 1
	}
	return 0
}
