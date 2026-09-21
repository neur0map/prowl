package gateway

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func testRNG() *rand.Rand {
	return rand.New(rand.NewPCG(1, 2))
}

// TestUntriedCandidateStillGetsTraffic is why routing samples the posterior
// instead of using its mean: a model with no history must sometimes win, or it
// can never earn the data that would rank it, and the chain ossifies around
// whatever happened to be tried first.
func TestUntriedCandidateStillGetsTraffic(t *testing.T) {
	t.Parallel()

	rng := testRNG()
	untried := ReliabilityPosterior(0, 0)
	proven := ReliabilityPosterior(40, 2)

	wins := 0
	const draws = 2000
	for range draws {
		if untried.Sample(rng) > proven.Sample(rng) {
			wins++
		}
	}

	require.Greater(t, wins, 0, "an untried candidate must be explored")
	require.Less(t, wins, draws/2, "but it must not beat a proven one most of the time")
}

// TestEvidenceNarrowsExploration is the other half of the bandit: once a
// candidate has a lot of consistent history, the draws must concentrate, so
// exploration cost falls away by itself with no schedule to tune.
func TestEvidenceNarrowsExploration(t *testing.T) {
	t.Parallel()

	rng := testRNG()
	spread := func(p Posterior) float64 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for range 2000 {
			v := p.Sample(rng)
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		return hi - lo
	}

	thin := spread(ReliabilityPosterior(2, 1))
	thick := spread(ReliabilityPosterior(400, 100))
	require.Less(t, thick, thin, "more evidence must mean less exploration")
}

// TestPosteriorMeanIsStableForDisplay keeps the dashboard readable: the number
// a human reads must not move when nothing changed.
func TestPosteriorMeanIsStableForDisplay(t *testing.T) {
	t.Parallel()

	p := ReliabilityPosterior(9, 1)
	require.InDelta(t, 10.0/12.0, p.Expected(), 1e-9)
	require.Equal(t, p.Expected(), p.Expected())

	// A candidate with no history sits at even odds, not at zero: no evidence
	// is not evidence of failure.
	require.InDelta(t, 0.5, ReliabilityPosterior(0, 0).Expected(), 1e-9)
}

// TestSpeedFallsBackToAnOptimisticPrior keeps an unmeasured endpoint in
// contention; a pessimistic default would mean the fastest provider is never
// discovered.
func TestSpeedFallsBackToAnOptimisticPrior(t *testing.T) {
	t.Parallel()

	require.InDelta(t, speedPrior, SpeedScore(0, -1), 1e-9)
	require.Greater(t, SpeedScore(0, -1), 0.5, "the prior must be optimistic")

	// Either signal alone is usable on its own.
	require.InDelta(t, ThroughputScore(120), SpeedScore(120, -1), 1e-9)
	require.InDelta(t, TTFBScore(400), SpeedScore(0, 400), 1e-9)

	// With both, the blend sits between them and leans on throughput.
	blended := SpeedScore(120, 4000)
	require.Greater(t, blended, TTFBScore(4000))
	require.Less(t, blended, ThroughputScore(120))
}

// TestTTFBIsClampedAtBothEnds pins the boundaries so a pathological latency
// cannot produce a negative or >1 axis and skew the convex mix.
func TestTTFBIsClampedAtBothEnds(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1.0, TTFBScore(0))
	require.Equal(t, 1.0, TTFBScore(ttfbBestMs))
	require.Equal(t, 0.0, TTFBScore(ttfbWorstMs))
	require.Equal(t, 0.0, TTFBScore(60000))
	require.InDelta(t, 0.5, TTFBScore((ttfbBestMs+ttfbWorstMs)/2), 1e-9)
}

// TestTierDominatesRank is the intelligence axis's defining property: rank
// refines an order inside a tier but can never lift a small model above a
// frontier one, however well it is ranked.
func TestTierDominatesRank(t *testing.T) {
	t.Parallel()

	bestSmall := IntelligenceComposite(TierSmall, 1)
	worstMedium := IntelligenceComposite(TierMedium, 500)
	require.Greater(t, worstMedium, bestSmall,
		"the worst-ranked model of a tier must still outrank the best of the tier below")

	// Within a tier, a better rank scores higher.
	require.Greater(t, IntelligenceComposite(TierLarge, 1), IntelligenceComposite(TierLarge, 20))
}

// TestIntelligenceNormalisesToTheCandidateSet keeps the axis comparable
// whatever the chain contains, including a set with no spread.
func TestIntelligenceNormalisesToTheCandidateSet(t *testing.T) {
	t.Parallel()

	lo := IntelligenceComposite(TierSmall, 10)
	hi := IntelligenceComposite(TierFrontier, 1)

	require.InDelta(t, 0.0, IntelligenceScore(lo, lo, hi), 1e-9)
	require.InDelta(t, 1.0, IntelligenceScore(hi, lo, hi), 1e-9)
	require.Equal(t, 1.0, IntelligenceScore(hi, hi, hi), "a set with no spread must not divide by zero")
}

// TestQuotaPressureDampensGraduallyAndNeverToZero is the guardrail contract:
// a candidate short of budget is demoted, not removed, because a throttled
// provider is still better than no provider.
func TestQuotaPressureDampensGraduallyAndNeverToZero(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1.0, HeadroomFactor(0, 1000), "an untouched budget must not demote")
	require.Equal(t, 1.0, HeadroomFactor(500, 1000), "half spent is still above the ramp")
	require.Equal(t, 1.0, HeadroomFactor(100, 0), "an unknown budget is not evidence of exhaustion")

	nearlySpent := HeadroomFactor(950, 1000)
	require.Less(t, nearlySpent, 1.0, "inside the ramp a candidate must be demoted")
	require.GreaterOrEqual(t, nearlySpent, headroomFloor)

	exhausted := HeadroomFactor(1000, 1000)
	require.InDelta(t, headroomFloor, exhausted, 1e-9, "an exhausted budget floors but does not zero")
	require.Greater(t, exhausted, 0.0)

	// Monotonic: more spend never raises the factor.
	prev := 1.1
	for used := 0.0; used <= 1000; used += 25 {
		f := HeadroomFactor(used, 1000)
		require.LessOrEqual(t, f, prev+1e-12, "headroom must fall monotonically with spend")
		prev = f
	}
}

// TestGuardrailsDoNotReorderHealthyCandidates is the reason the guardrails are
// multiplicative. While both candidates have budget, the better model stays
// ahead; only real quota pressure changes the order.
func TestGuardrailsDoNotReorderHealthyCandidates(t *testing.T) {
	t.Parallel()

	better := Combine(WeightsBalanced, 0.9, 0.8, 0.9, HeadroomFactor(100, 1000), 1)
	worse := Combine(WeightsBalanced, 0.6, 0.5, 0.4, HeadroomFactor(0, 1000), 1)
	require.Greater(t, better.Effective, worse.Effective,
		"a clearly better candidate must not be demoted below a worse one by an untouched guardrail")

	// Once the better one is out of budget, the worse one takes over.
	starved := Combine(WeightsBalanced, 0.9, 0.8, 0.9, HeadroomFactor(1000, 1000), 1)
	require.Less(t, starved.Effective, worse.Effective,
		"an exhausted candidate must yield to a healthy one")
}

// TestCustomWeightsAreRenormalised means an operator's weights do not have to
// sum to one for the base score to stay comparable between candidates.
func TestCustomWeightsAreRenormalised(t *testing.T) {
	t.Parallel()

	normal := Combine(Weights{Reliability: 0.5, Speed: 0.25, Intelligence: 0.25}, 0.8, 0.4, 0.6, 1, 1)
	scaled := Combine(Weights{Reliability: 5, Speed: 2.5, Intelligence: 2.5}, 0.8, 0.4, 0.6, 1, 1)
	require.InDelta(t, normal.Base, scaled.Base, 1e-12)

	// All-zero weights must not divide by zero.
	require.Equal(t, 0.0, Combine(Weights{}, 0.9, 0.9, 0.9, 1, 1).Base)
}

// TestStrategyPresetsRankTheirOwnAxisHighest checks the presets actually mean
// what their names say.
func TestStrategyPresetsRankTheirOwnAxisHighest(t *testing.T) {
	t.Parallel()

	for name, w := range map[string]Weights{
		"balanced": WeightsBalanced, "smartest": WeightsSmartest,
		"fastest": WeightsFastest, "reliable": WeightsReliable,
	} {
		require.InDelta(t, 1.0, w.Reliability+w.Speed+w.Intelligence, 1e-9, "%s must be convex", name)
	}

	require.Greater(t, WeightsSmartest.Intelligence, WeightsBalanced.Intelligence)
	require.Greater(t, WeightsFastest.Speed, WeightsBalanced.Speed)
	require.Greater(t, WeightsReliable.Reliability, WeightsBalanced.Reliability)

	// The same candidates must reorder under different strategies, or the
	// presets are decoration.
	fastButDim := func(w Weights) float64 { return Combine(w, 0.8, 0.95, 0.2, 1, 1).Effective }
	slowButSmart := func(w Weights) float64 { return Combine(w, 0.8, 0.2, 0.95, 1, 1).Effective }
	require.Greater(t, fastButDim(WeightsFastest), slowButSmart(WeightsFastest))
	require.Greater(t, slowButSmart(WeightsSmartest), fastButDim(WeightsSmartest))
}
