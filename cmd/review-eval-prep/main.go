package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/neur0map/prowl/internal/revieweval"
)

func main() {
	config := revieweval.PrepareConfig{}
	flag.StringVar(&config.SourcesPath, "sources", "testdata/review-eval/sources.json", "frozen source manifest")
	flag.StringVar(&config.CandidatePoolPath, "candidate-pool", "testdata/review-eval/candidate_pool.json", "canonical multi-source candidate pool")
	flag.StringVar(&config.RejectionsPath, "rejections", "testdata/review-eval/candidate_rejections.json", "evidenced immutable candidate rejections")
	flag.StringVar(&config.AuditPacketsPath, "audit-packets", "testdata/review-eval/audit_packets.json", "canonical candidate audit packets")
	flag.StringVar(&config.TuningPath, "tuning", "testdata/review-eval/tuning.json", "frozen tuning corpus")
	flag.StringVar(&config.HeldOutPath, "held-out", "testdata/review-eval/held_out.json", "frozen held-out corpus")
	flag.StringVar(&config.SmallPath, "small", "testdata/review-eval/small_non_regression.json", "frozen SWR small-change corpus")
	flag.StringVar(&config.ScoringPath, "scoring", "testdata/review-eval/scoring.json", "frozen scoring protocol")
	flag.StringVar(&config.CachePath, "cache", ".review-eval-cache/sources", "local source cache directory")
	flag.StringVar(&config.RepositoryCachePath, "repo-cache", ".review-eval-cache/repos", "local pinned Git repository cache directory")
	flag.BoolVar(&config.Offline, "offline", false, "reuse verified cache without network access")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.TODO(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := revieweval.Prepare(ctx, config)
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "error:", marshalErr)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
