package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// seedScorerKey inserts a bare api_keys row so a request may reference it: the
// requests table enforces the key_id foreign key, so a synthetic id would be
// rejected.
func seedScorerKey(t *testing.T, s *Server, label string) int64 {
	t.Helper()
	res, err := s.engine.DB().Exec(
		`INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		 VALUES ('custom', ?, 'x', 'y', 'z', 'healthy', 1, ?)`,
		label, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestScorerIsolatesModelEvidenceByEndpointScope proves the per-model fallback
// keeps two custom relays that serve the same model id at different endpoints
// from inheriting each other's history. Relay A only ever succeeds and relay B
// only ever fails; if the fallback join ignored endpoint_scope, relay A's
// aggregate would carry relay B's failures (and vice versa), and a healthy
// relay would be demoted for a sibling's outages.
func TestScorerIsolatesModelEvidenceByEndpointScope(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	ctx := context.Background()
	db := s.engine.DB()

	const scopeA, scopeB = "http://relay-a", "http://relay-b"
	keyA := seedScorerKey(t, s, "relay-a")
	keyB := seedScorerKey(t, s, "relay-b")

	insertModel := func(scope string) int64 {
		res, err := db.ExecContext(ctx,
			`INSERT INTO models (platform, model_id, display_name, endpoint_scope)
			 VALUES ('custom', 'shared', 'Shared', ?)`, scope)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}
	idA := insertModel(scopeA)
	idB := insertModel(scopeB)

	for range 4 {
		_, err := s.RecordRequest(ctx, RequestLog{
			Platform: "custom", ModelID: "shared", EndpointScope: scopeA, KeyID: &keyA,
			Outcome: "success", StatusCode: 200, OutputTokens: 20, LatencyMs: 100,
		})
		require.NoError(t, err)
	}
	for range 3 {
		_, err := s.RecordRequest(ctx, RequestLog{
			Platform: "custom", ModelID: "shared", EndpointScope: scopeB, KeyID: &keyB,
			Outcome: "error", StatusCode: 502, LatencyMs: 100,
		})
		require.NoError(t, err)
	}

	_, byModel, err := aggregateTrail(ctx, s)
	require.NoError(t, err)

	a, okA := byModel[idA]
	require.True(t, okA, "relay A must have its own evidence")
	require.InDelta(t, 4.0, a.successes, 1e-9)
	require.Zero(t, a.failures, "relay A must not inherit relay B's failures")

	b, okB := byModel[idB]
	require.True(t, okB, "relay B must have its own evidence")
	require.InDelta(t, 3.0, b.failures, 1e-9)
	require.Zero(t, b.successes, "relay B must not inherit relay A's successes")
}

// TestScorerSpeedEvidenceIsDeterministicAndSampleWeighted proves same-day
// observations settle into one throughput and one TTFB figure that (a) does not
// depend on the order the rows were written, and (b) weights each observation
// by sample count rather than letting each bucket vote once or letting whichever
// row SQLite happened to return for the group decide the whole answer.
//
// One model is served by a busy key (three modest calls) and a quiet key (one
// fast call). Pooled throughput is sum(tokens)*1000/sum(latency) and pooled
// TTFB is sum(ttfb)/count, so the busy key's three samples dominate; a
// per-bucket average would instead read the two keys as equal.
func TestScorerSpeedEvidenceIsDeterministicAndSampleWeighted(t *testing.T) {
	t.Parallel()

	type obs struct {
		key          string
		outputTokens int64
		latencyMs    int64
		ttfb         int64
	}
	observations := []obs{
		{"busy", 100, 100, 50},
		{"busy", 100, 100, 50},
		{"busy", 100, 100, 50},
		{"quiet", 200, 100, 200},
	}

	build := func(order []int) routeStats {
		s := testServer(t, Options{})
		ctx := context.Background()
		db := s.engine.DB()

		keys := map[string]*int64{
			"busy":  new(seedScorerKey(t, s, "busy")),
			"quiet": new(seedScorerKey(t, s, "quiet")),
		}
		res, err := db.ExecContext(ctx,
			`INSERT INTO models (platform, model_id, display_name, endpoint_scope)
			 VALUES ('custom', 'm1', 'M1', 'http://relay')`)
		require.NoError(t, err)
		modelID, err := res.LastInsertId()
		require.NoError(t, err)

		for _, i := range order {
			o := observations[i]
			_, err := s.RecordRequest(ctx, RequestLog{
				Platform: "custom", ModelID: "m1", EndpointScope: "http://relay",
				KeyID:   keys[o.key],
				Outcome: "success", StatusCode: 200,
				OutputTokens: o.outputTokens, LatencyMs: o.latencyMs, TTFBMs: new(o.ttfb),
			})
			require.NoError(t, err)
		}

		_, byModel, err := aggregateTrail(ctx, s)
		require.NoError(t, err)
		st, ok := byModel[modelID]
		require.True(t, ok, "the model must carry its pooled evidence")
		return st
	}

	ordered := build([]int{0, 1, 2, 3})
	shuffled := build([]int{3, 1, 0, 2})

	// sum(tokens)=500, sum(latency)=400 -> 1250 tok/s. A per-bucket average
	// would read (1000+2000)/2 = 1500, so 1250 proves sample-count weighting.
	require.InDelta(t, 1250.0, ordered.tokPerSec, 1e-9)
	// sum(ttfb)=350 over 4 samples -> 87.5ms; a per-bucket average would be
	// (50+200)/2 = 125.
	require.True(t, ordered.hasTTFB)
	require.InDelta(t, 87.5, ordered.ttfbMs, 1e-9)

	// Insertion order changes nothing.
	require.InDelta(t, ordered.tokPerSec, shuffled.tokPerSec, 1e-12)
	require.InDelta(t, ordered.ttfbMs, shuffled.ttfbMs, 1e-12)
	require.Equal(t, ordered.hasTTFB, shuffled.hasTTFB)
}

// TestRateLimitFactorDispatchMatchesInspector proves the router scores a
// throttled model with the exact factor the penalty inspector reports, for
// every penalty depth. The two used to diverge: dispatch damped by
// max(0.5, 1-penalty/20) while the inspector damped by 1-(penalty/10)*0.6, so
// a maxed-out model was scored at 0.5 but shown as 0.4. Both now read one
// shared curve, so no penalty can make them disagree.
func TestRateLimitFactorDispatchMatchesInspector(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	penalties := s.engine.Penalties()
	scorer := &axisScorer{server: s}

	// Different models accrue different demotion depths, up to the cap.
	for _, id := range []int64{1, 2, 5, 40} {
		for range int(id) {
			penalties.RecordRateLimitHit(id)
		}
		penalty := penalties.Penalty(id)
		require.Positive(t, penalty, "the model must carry a demotion")

		dispatch := scorer.rateLimitFactor(&gateway.ChainEntry{ModelDBID: id})
		require.Equal(t, rateLimitFactorFor(penalty), dispatch,
			"router scoring and the inspector must share one rate-limit factor (penalty %v)", penalty)
		// The demotion must dampen the score without ever removing the model.
		require.Less(t, dispatch, 1.0)
		require.GreaterOrEqual(t, dispatch, 0.4)
	}

	// An unpenalised model is neutral in both paths.
	require.Equal(t, 1.0, scorer.rateLimitFactor(&gateway.ChainEntry{ModelDBID: 99}))
	require.Equal(t, 1.0, rateLimitFactorFor(penalties.Penalty(99)))
}
