package api

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// The axis scorer turns the request trail into the four inputs the ordering
// needs. It is the half of the bandit that reads: scoring.go owns the maths,
// this owns the evidence.
const (
	// statsWindow and statsHalfLife weight recent behaviour over old: a
	// provider that failed last week must not outweigh one healthy since.
	// (router.ts:703-704.)
	statsWindow   = 7 * 24 * time.Hour
	statsHalfLife = 2 * 24 * time.Hour

	// statsCacheTTL bounds how often the trail is re-read. Scoring runs on
	// every request, and re-aggregating a week of rows each time would put a
	// SQL scan on the interactive path (CACHE_TTL_MS, router.ts:705).
	statsCacheTTL = 60 * time.Second
)

// routeStats is the decay-weighted evidence for one (platform, model, key).
type routeStats struct {
	successes float64
	failures  float64
	tokPerSec float64
	ttfbMs    float64
	hasTTFB   bool
	samples   float64
}

// Total is the decay-weighted sample count behind these figures. A dashboard
// needs it to say whether a score rests on evidence or on the prior, which is
// the difference between "this provider is unreliable" and "we have not tried
// it yet".
func (s routeStats) Total() float64 { return s.successes + s.failures }

// statsCache holds the aggregated trail between refreshes.
type statsCache struct {
	mu        sync.RWMutex
	byRoute   map[gateway.RouteKey]routeStats
	byModel   map[int64]routeStats
	refreshed time.Time
}

// newAxisScorer returns a scorer over the current trail. The snapshot is
// shared by every candidate in one request, so a model cannot be ranked
// against a different vintage of evidence than its rivals.
func (s *Server) newAxisScorer(ctx context.Context) gateway.AxisScorer {
	s.stats.refreshIfStale(ctx, s)
	return &axisScorer{
		server: s,
		rng:    rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x2545f491)),
	}
}

type axisScorer struct {
	server *Server
	rng    *rand.Rand
}

// Axes supplies a candidate's four inputs.
//
// sampled selects a Thompson draw for live routing versus the posterior mean
// for display. The draw's variance IS the exploration, which is why an
// untried model still earns traffic without any separate schedule; the mean
// is used on a dashboard because a number that moves when nothing changed is
// unreadable.
func (a *axisScorer) Axes(e *gateway.ChainEntry, sampled bool) gateway.Axes {
	stats := a.server.stats.forEntry(e)

	posterior := gateway.ReliabilityPosterior(stats.successes, stats.failures)
	reliability := posterior.Expected()
	if sampled {
		reliability = posterior.Sample(a.rng)
	}

	ttfb := -1.0
	if stats.hasTTFB {
		ttfb = stats.ttfbMs
	}

	return gateway.Axes{
		Reliability: reliability,
		Speed:       gateway.SpeedScore(stats.tokPerSec, ttfb),
		Headroom:    a.headroom(e),
		RateLimit:   a.rateLimitFactor(e),
	}
}

// headroom is the monthly-budget guardrail: a candidate short of budget is
// demoted gradually rather than dropped, because a throttled provider still
// beats no provider.
func (a *axisScorer) headroom(e *gateway.ChainEntry) float64 {
	if e.KeyID == nil {
		return 1
	}
	limits := windowLimitsOf(e)
	return a.server.engine.Ledger().Headroom(e.Platform, e.ModelID, []int64{*e.KeyID}, limits)
}

// windowLimitsOf converts the chain's nullable limits into the ledger's plain
// ones. A nil limit means the provider publishes none, which the ledger reads
// as zero: unknown, not zero-allowance.
func windowLimitsOf(e *gateway.ChainEntry) gateway.WindowLimits {
	deref := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	return gateway.WindowLimits{
		RPM: deref(e.RPMLimit), TPM: deref(e.TPMLimit),
		RPD: deref(e.RPDLimit), TPD: deref(e.TPDLimit),
	}
}

// rateLimitFactor is the live penalty a recent rate limit imposes. It
// multiplies rather than reorders, so a model that is genuinely better stays
// ahead until it is actually being throttled. The damping curve is the one
// shared with the penalty inspector (rateLimitFactorFor): the factor the
// router scores with and the factor the dashboard reports are the same
// function of the same penalty, so they can never disagree for any value.
func (a *axisScorer) rateLimitFactor(e *gateway.ChainEntry) float64 {
	return rateLimitFactorFor(a.server.engine.Penalties().Penalty(e.ModelDBID))
}

// refreshIfStale re-aggregates the trail when the cached snapshot has aged
// out. A failed refresh keeps the previous snapshot: stale evidence routes
// better than no evidence.
func (c *statsCache) refreshIfStale(ctx context.Context, s *Server) {
	c.mu.RLock()
	fresh := time.Since(c.refreshed) < statsCacheTTL && c.byRoute != nil
	c.mu.RUnlock()
	if fresh {
		return
	}

	byRoute, byModel, err := aggregateTrail(ctx, s)
	if err != nil {
		slog.Debug("Could not refresh routing stats", "error", err)
		return
	}

	c.mu.Lock()
	c.byRoute, c.byModel, c.refreshed = byRoute, byModel, time.Now()
	c.mu.Unlock()
}

// forEntry returns the evidence for a candidate, preferring its own
// (model, key) history and falling back to the model's aggregate so a brand
// new key on a known-good model is not treated as a total unknown.
func (c *statsCache) forEntry(e *gateway.ChainEntry) routeStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if e.KeyID != nil {
		key := gateway.RouteKey{Platform: e.Platform, ModelID: e.ModelID, KeyID: *e.KeyID}
		if stats, ok := c.byRoute[key]; ok {
			return stats
		}
	}
	if stats, ok := c.byModel[e.ModelDBID]; ok {
		return stats
	}
	return routeStats{}
}

// routeAccum is the running evidence for one trail grouping while
// aggregateTrail folds the day buckets together. Reliability keeps the
// decay-and-sample weight the router has always used; the speed measurements
// are held as weighted numerator/denominator pairs so combining buckets is a
// plain commutative sum, never an order-dependent overwrite of whichever row
// SQLite happened to surface last.
type routeAccum struct {
	successes float64
	failures  float64
	samples   float64
	// tokNum/tokDen are decay-weighted output tokens over decay-weighted
	// latency milliseconds; their ratio, scaled to seconds, is throughput.
	tokNum float64
	tokDen float64
	// ttfbNum/ttfbDen are the decay-weighted sum of TTFB samples over their
	// decay-weighted count; their ratio is the mean first-token latency.
	ttfbNum float64
	ttfbDen float64
}

func (a *routeAccum) add(b routeAccum) {
	a.successes += b.successes
	a.failures += b.failures
	a.samples += b.samples
	a.tokNum += b.tokNum
	a.tokDen += b.tokDen
	a.ttfbNum += b.ttfbNum
	a.ttfbDen += b.ttfbDen
}

func (a routeAccum) finalize() routeStats {
	s := routeStats{successes: a.successes, failures: a.failures, samples: a.samples}
	if a.tokDen > 0 {
		s.tokPerSec = a.tokNum * 1000 / a.tokDen
	}
	if a.ttfbDen > 0 {
		s.ttfbMs = a.ttfbNum / a.ttfbDen
		s.hasTTFB = true
	}
	return s
}

// aggregateTrail reads the request trail and applies the decay weights.
//
// The decay is applied in Go rather than SQL: expressing an exponential
// half-life in SQLite would need either a UDF or a pile of CASE arms, and the
// day-bucket aggregation keeps the row count small enough that the arithmetic
// is free. Within a bucket the token, latency, and TTFB figures are settled by
// SQL aggregates rather than a bare grouped column, so the evidence a route
// earns does not depend on which row SQLite returned for the group; the Go
// fold then combines buckets by weight, which is order-independent by
// construction.
func aggregateTrail(ctx context.Context, s *Server) (
	map[gateway.RouteKey]routeStats, map[int64]routeStats, error,
) {
	cutoff := time.Now().Add(-statsWindow).Unix()
	rows, err := s.engine.DB().QueryContext(ctx,
		`SELECT platform, model_id, endpoint_scope, COALESCE(key_id, 0), outcome,
		        SUM(CASE WHEN output_tokens > 0 AND latency_ms > 0
		                 THEN output_tokens ELSE 0 END) AS tok_sum,
		        SUM(CASE WHEN output_tokens > 0 AND latency_ms > 0
		                 THEN latency_ms ELSE 0 END) AS lat_sum,
		        COALESCE(SUM(ttfb_ms), 0) AS ttfb_sum,
		        COUNT(ttfb_ms) AS ttfb_n,
		        CAST((? - created_at) / 86400 AS INTEGER) AS age_days,
		        count(*) AS n
		   FROM requests
		  WHERE created_at >= ? AND outcome <> 'canceled'
		  GROUP BY platform, model_id, endpoint_scope, key_id, outcome, age_days`,
		time.Now().Unix(), cutoff)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	// scopeKey attributes a bucket to a (platform, model, endpoint) identity.
	// endpoint_scope is what stops two custom relays serving the same model id
	// from pooling each other's history: they are distinct rows in models, so
	// the per-model fallback must not merge one relay's failures into the
	// other's evidence.
	type scopeKey struct {
		platform string
		modelID  string
		scope    string
	}

	byRouteAcc := map[gateway.RouteKey]routeAccum{}
	byScopeAcc := map[scopeKey]routeAccum{}
	for rows.Next() {
		var (
			platform, modelID, scope, outcome string
			keyID                             int64
			tokSum, latSum, ttfbSum, ttfbN    int64
			ageDays, n                        int64
		)
		if err := rows.Scan(&platform, &modelID, &scope, &keyID, &outcome,
			&tokSum, &latSum, &ttfbSum, &ttfbN, &ageDays, &n); err != nil {
			return nil, nil, err
		}

		decay := decayWeight(ageDays)
		bucket := routeAccum{samples: decay * float64(n)}
		switch outcome {
		case "success":
			bucket.successes = decay * float64(n)
			// Weighted sums, not per-bucket rates: a bucket with more samples
			// pulls the pooled throughput and TTFB toward its own figures.
			bucket.tokNum = decay * float64(tokSum)
			bucket.tokDen = decay * float64(latSum)
			bucket.ttfbNum = decay * float64(ttfbSum)
			bucket.ttfbDen = decay * float64(ttfbN)
		default:
			// A non-success counts against reliability. Its wall-clock is left
			// out of speed: a failed hop's latency measures the failure, not
			// the model's throughput.
			bucket.failures = decay * float64(n)
		}

		rk := gateway.RouteKey{Platform: platform, ModelID: modelID, KeyID: keyID}
		route := byRouteAcc[rk]
		route.add(bucket)
		byRouteAcc[rk] = route

		sk := scopeKey{platform: platform, modelID: modelID, scope: scope}
		scoped := byScopeAcc[sk]
		scoped.add(bucket)
		byScopeAcc[sk] = scoped
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	byRoute := make(map[gateway.RouteKey]routeStats, len(byRouteAcc))
	for rk, acc := range byRouteAcc {
		byRoute[rk] = acc.finalize()
	}

	// The per-model aggregate is the fallback for a key with no history: it
	// pools the evidence recorded under the model's own endpoint scope so a
	// brand new key on a known-good model is not treated as a total unknown,
	// while a same-id relay at a different endpoint stays isolated.
	byModel := map[int64]routeStats{}
	modelRows, err := s.engine.DB().QueryContext(ctx,
		`SELECT id, platform, model_id, endpoint_scope FROM models`)
	if err != nil {
		return byRoute, byModel, nil
	}
	defer func() { _ = modelRows.Close() }()

	for modelRows.Next() {
		var (
			modelDBID       int64
			platform, model string
			scope           string
		)
		if err := modelRows.Scan(&modelDBID, &platform, &model, &scope); err != nil {
			continue
		}
		if acc, ok := byScopeAcc[scopeKey{platform: platform, modelID: model, scope: scope}]; ok {
			byModel[modelDBID] = acc.finalize()
		}
	}

	return byRoute, byModel, nil
}

// decayWeight halves a sample's influence every statsHalfLife.
func decayWeight(ageDays int64) float64 {
	return math.Pow(0.5, float64(ageDays)/(statsHalfLife.Hours()/24))
}

// keyBaseURL reads a key's endpoint, which a relay needs and which also tells
// the cooldown engine whether this is a local endpoint that recovers in
// seconds.
func (s *Server) keyBaseURL(keyID int64) string {
	var baseURL *string
	if err := s.engine.DB().QueryRow(
		`SELECT base_url FROM api_keys WHERE id = ?`, keyID).Scan(&baseURL); err != nil {
		return ""
	}
	if baseURL == nil {
		return ""
	}
	return *baseURL
}
