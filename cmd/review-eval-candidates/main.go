package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/prowl-agent/prowl-agent/internal/revieweval"
)

func main() {
	config := revieweval.CandidateDiscoveryConfig{}
	flag.StringVar(&config.SourcesPath, "sources", "testdata/review-eval/sources.json", "frozen source manifest")
	flag.StringVar(&config.RejectionsPath, "rejections", "testdata/review-eval/candidate_rejections.json", "evidenced immutable candidate rejections")
	flag.StringVar(&config.CachePath, "cache", ".review-eval-cache/sources", "verified source payload cache")
	flag.StringVar(&config.RepositoryCachePath, "repo-cache", ".review-eval-cache/repos", "pinned Git repository cache")
	flag.StringVar(&config.CandidatePoolPath, "candidate-pool", "testdata/review-eval/candidate_pool.json", "generated mechanically qualified candidate pool")
	flag.StringVar(&config.TuningPath, "tuning", "testdata/review-eval/tuning.json", "generated tuning audit queue")
	flag.StringVar(&config.HeldOutPath, "held-out", "testdata/review-eval/held_out.json", "generated held-out audit queue")
	flag.StringVar(&config.AuditPacketsPath, "audit-packets", "testdata/review-eval/audit_packets.json", "generated factual audit packets")
	flag.StringVar(&config.PartitionSeedSHA256, "partition-seed", "5b5ce22aecb96031533dbd873bc7be87ac766f392eac46236f42f6fd001accd8", "frozen partition seed SHA-256")
	flag.BoolVar(&config.Offline, "offline", false, "reuse verified source and Git caches without network access")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := revieweval.DiscoverCandidates(ctx, config)
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
