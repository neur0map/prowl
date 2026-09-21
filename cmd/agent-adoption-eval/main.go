package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/agenteval"
)

func main() {
	var clients string
	var cfg agenteval.Config
	flag.StringVar(&clients, "client", "claude,omp", "comma-separated clients: claude,omp")
	flag.StringVar(&cfg.Model, "model", "", "model override passed to each client")
	flag.IntVar(&cfg.Repetitions, "repetitions", 1, "fresh process repetitions per prompt and condition")
	flag.StringVar(&cfg.Set, "set", "tuning", "prompt set: tuning or held_out")
	flag.StringVar(&cfg.Fixture, "fixture", "", "fixture repository override (default: manifest fixture)")
	flag.StringVar(&cfg.OutputDir, "output", "", "required local relative artifact directory")
	flag.StringVar(&cfg.ManifestPath, "manifest", "testdata/agent-adoption/prompts.json", "prompt manifest path")
	flag.StringVar(&cfg.ProwlBinary, "prowl", "prowl", "Prowl binary")
	flag.StringVar(&cfg.ClaudeBinary, "claude", "claude", "Claude binary")
	flag.StringVar(&cfg.OMPBinary, "omp", "omp", "OMP binary")
	flag.DurationVar(&cfg.Timeout, "timeout", 3*time.Minute, "per-process timeout")
	flag.Parse()

	if cfg.OutputDir == "" {
		fmt.Fprintln(os.Stderr, "error: --output is required")
		os.Exit(2)
	}
	for _, client := range strings.Split(clients, ",") {
		client = strings.TrimSpace(client)
		if client != "" {
			cfg.Clients = append(cfg.Clients, client)
		}
	}
	manifest, err := agenteval.LoadManifest(cfg.ManifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	report, err := agenteval.Run(context.Background(), cfg, manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Print(agenteval.RenderTable(report))
	if !report.Gates.Passed {
		os.Exit(1)
	}
}
