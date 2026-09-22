package gateway

import (
	"context"
	"database/sql"
	"time"
)

// Seeded multi-axis capability scores, normalised to 0..1 from real, cited
// Sept-2026 benchmarks (see docs/BENCHMARKS.md; every value traces to a public
// leaderboard). This is the offline catalogue prior: it lets the router match a
// task to a model's genuine strength - vision, writing, planning, coding,
// tool-use, math - even without an ARTIFICIAL_ANALYSIS_API_KEY. When that key is
// set a live AA refresh overwrites the composite axes AA publishes
// (intelligence/coding/agentic); axes AA does not publish (vision Pro-only,
// writing, planning, and math which AA retired) stay sourced from here.
//
// Normalisation per axis (clamped to 0..1): intelligence = AA Intelligence Index
// /100; coding = SWE-bench Verified %/100 (a Pro-only score is placed on its
// relative standing, not divided naively); agentic = AA Coding-Agent Index /100
// or the model's agentic standing; math = AIME-2026 %/100; vision = MMMU-Pro
// %/100; writing = EQ-Bench Elo mapped from ~1450..2100; planning = GDPval-AA /
// long-horizon agentic standing. A 0/omitted axis means "no public
// measurement", which the scorer reads as "fall back to overall intelligence",
// never as incompetence.
type seededBenchmark struct {
	intelligence, coding, agentic, math, vision, writing, planning float64
}

// seededBenchmarks is keyed by a readable model id; SeedBenchmarks matches it to
// catalogue rows through modelSignature, so a provider prefix or date suffix
// (zai-org/GLM-5.2, claude-haiku-4-5-20251001) resolves to the same row.
var seededBenchmarks = map[string]seededBenchmark{
	// Anthropic (Claude Max subscription pool).
	"claude-opus-4-8":  {intelligence: 0.53, coding: 0.886, agentic: 0.80, math: 0.652, vision: 0.788, writing: 0.90, planning: 0.82},
	"claude-sonnet-5":  {intelligence: 0.52, coding: 0.852, agentic: 0.804, writing: 0.88, planning: 0.83},
	"claude-haiku-4-5": {intelligence: 0.40, coding: 0.733, agentic: 0.55},
	"claude-fable-5":   {intelligence: 0.53, coding: 0.95, agentic: 0.80, writing: 0.95, planning: 0.82},
	// OpenAI (ChatGPT/Codex subscription pool).
	"gpt-5.6-sol":   {intelligence: 0.589, coding: 0.646, agentic: 0.90, math: 0.89, vision: 0.799, writing: 0.85, planning: 0.86},
	"gpt-5.6-terra": {intelligence: 0.55, agentic: 0.87, math: 0.87, planning: 0.80},
	"gpt-5.6-luna":  {intelligence: 0.50, agentic: 0.85, math: 0.92, planning: 0.75},
	"gpt-5.3-codex": {intelligence: 0.52, coding: 0.85, agentic: 0.90, vision: 0.785, planning: 0.85},
	// DeepSeek (Hyper subscription + catalogue).
	"deepseek-v4.1-flash": {intelligence: 0.39, coding: 0.742, agentic: 0.90, math: 0.909, planning: 0.80},
	"deepseek-v4-pro":     {intelligence: 0.36, coding: 0.70, agentic: 0.72},
	// Open-weight / other frontier catalogue.
	"GLM-5.2":           {intelligence: 0.50, coding: 0.744, agentic: 0.81, math: 0.992, writing: 0.72},
	"Qwen3.5-397B-A17B": {intelligence: 0.45, coding: 0.764, agentic: 0.70, math: 0.913},
	"Kimi-K2.5":         {intelligence: 0.47, coding: 0.768, agentic: 0.90, math: 0.961},
	"grok-4.1":          {intelligence: 0.44, coding: 0.60, agentic: 0.62},
	"gemini-3-pro":      {intelligence: 0.55, coding: 0.762, agentic: 0.78, math: 0.95, vision: 0.81, planning: 0.80},
	"gemini-3-flash":    {intelligence: 0.48, coding: 0.78, agentic: 0.72, vision: 0.812},
	"nemotron-3-ultra":  {intelligence: 0.48, coding: 0.493, agentic: 0.598},
}

// SeedBenchmarks writes the cited offline scores for any catalogue model that
// has no live-refreshed benchmark row yet, so a fresh install routes by real
// multi-axis capability out of the box. It never clobbers an
// Artificial-Analysis-sourced row (the ON CONFLICT update is gated to
// source='catalogue-seed'), so a live refresh stays authoritative; an improved
// seed lands on a prior seed row on the next boot.
func SeedBenchmarks(ctx context.Context, db *sql.DB) (int, error) {
	// Index the seed by signature so both sides go through the same normaliser.
	bySig := make(map[string]seededBenchmark, len(seededBenchmarks))
	for id, bench := range seededBenchmarks {
		bySig[modelSignature(id)] = bench
	}

	rows, err := db.QueryContext(ctx, `SELECT id, model_id, display_name FROM models`)
	if err != nil {
		return 0, err
	}
	type target struct {
		id    int64
		bench seededBenchmark
	}
	var targets []target
	for rows.Next() {
		var id int64
		var modelID, name string
		if err := rows.Scan(&id, &modelID, &name); err != nil {
			_ = rows.Close()
			return 0, err
		}
		bench, ok := bySig[modelSignature(modelID)]
		if !ok && name != "" {
			bench, ok = bySig[modelSignature(name)]
		}
		if ok {
			targets = append(targets, target{id: id, bench: bench})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	now := time.Now().Unix()
	seeded := 0
	for _, t := range targets {
		res, err := db.ExecContext(ctx, `
			INSERT INTO model_benchmarks
				(model_db_id, source, source_model_id, source_model_name, index_version,
				 intelligence, coding, agentic, math, multilingual, vision, writing, planning, refreshed_at)
			VALUES (?, 'catalogue-seed', '', '', NULL, ?, ?, ?, ?, NULL, ?, ?, ?, ?)
			ON CONFLICT(model_db_id) DO UPDATE SET
				intelligence = excluded.intelligence,
				coding       = excluded.coding,
				agentic      = excluded.agentic,
				math         = excluded.math,
				vision       = excluded.vision,
				writing      = excluded.writing,
				planning     = excluded.planning,
				refreshed_at = excluded.refreshed_at
			 WHERE model_benchmarks.source = 'catalogue-seed'`,
			t.id, nzBenchmark(t.bench.intelligence), nzBenchmark(t.bench.coding), nzBenchmark(t.bench.agentic),
			nzBenchmark(t.bench.math), nzBenchmark(t.bench.vision), nzBenchmark(t.bench.writing), nzBenchmark(t.bench.planning), now)
		if err != nil {
			return seeded, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			seeded++
		}
	}
	return seeded, nil
}

// nzBenchmark stores a real measurement and leaves an unmeasured axis NULL, so
// the scorer's COALESCE(...,0) + valueOr fallback treats absence as "use the
// overall prior", never as a zero score.
func nzBenchmark(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}
