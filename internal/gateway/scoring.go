package gateway

import (
	"math"
	"math/rand/v2"
)

// Scoring is the contextual-bandit model ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md).
//
// A candidate is not ranked by a single measurement. Three normalised axes are
// combined convexly and then damped by multiplicative guardrails:
//
//	base      = (w_rel·reliability + w_speed·speed + w_intel·intelligence) / wSum
//	effective = base × headroomFactor × rateLimitFactor
//
// The guardrails multiply rather than reorder: a model that is genuinely
// better stays ahead of a worse one until it is actually short of quota, which
// is why quota pressure demotes gradually instead of flipping the order.
const (
	// priorSuccess and priorFailure give a never-tried model a uniform
	// Beta(1,1) posterior, so it samples across the whole [0,1] range and
	// earns traffic instead of being frozen out by having no history.
	priorSuccess = 1.0
	priorFailure = 1.0

	// speedScaleTokS is the throughput at which the score reaches 1-1/e.
	speedScaleTokS = 60.0

	// ttfbBestMs scores a full mark; ttfbWorstMs scores zero.
	ttfbBestMs  = 300.0
	ttfbWorstMs = 5000.0

	throughputWeight = 0.6
	ttfbWeight       = 0.4

	// speedPrior is deliberately optimistic: an unmeasured model must look
	// worth trying, or the fastest endpoint could never be discovered.
	speedPrior = 0.6

	// rankScale compresses the intelligence rank so tier dominates it. The
	// worst in-tier rank still scores below the next tier up.
	rankScale = 31.0

	// headroomRampStart is the fraction of monthly budget below which a
	// candidate starts being demoted; headroomFloor is how far it can fall.
	headroomRampStart = 0.2
	headroomFloor     = 0.1
)

// Weights are the convex mix of the three axes.
type Weights struct {
	Reliability  float64
	Speed        float64
	Intelligence float64
}

// Named strategy presets, matching the upstream values exactly
// (scoring.ts:46-53). `fastest` and `reliable` are deliberately the two ends
// of the speed/reliability axis rather than extremes: their own comment notes
// that pushing `fastest` harder would turn it into a noisy copy of
// `reliable`, which is a preset the user could have chosen instead.
var (
	WeightsBalanced = Weights{Reliability: 0.5, Speed: 0.25, Intelligence: 0.25}
	WeightsSmartest = Weights{Reliability: 0.35, Speed: 0.1, Intelligence: 0.55}
	WeightsFastest  = Weights{Reliability: 0.35, Speed: 0.55, Intelligence: 0.1}
	WeightsReliable = Weights{Reliability: 0.7, Speed: 0.15, Intelligence: 0.15}
)

// Posterior is a candidate's decay-weighted Beta posterior over success.
type Posterior struct {
	Alpha float64
	Beta  float64
}

// ReliabilityPosterior folds observed pseudo-counts into the prior. Counts are
// decay-weighted rather than raw, so a provider that failed last week does not
// outweigh one that has been healthy since.
func ReliabilityPosterior(successes, failures float64) Posterior {
	return Posterior{
		Alpha: math.Max(0, successes) + priorSuccess,
		Beta:  math.Max(0, failures) + priorFailure,
	}
}

// Expected is the posterior mean. This is what a dashboard shows, because a
// number that moves when nothing changed is unreadable.
func (p Posterior) Expected() float64 {
	total := p.Alpha + p.Beta
	if total <= 0 {
		return 0
	}
	return p.Alpha / total
}

// Sample draws from the posterior. Routing uses this rather than the mean:
// the draw's variance IS the exploration, so a candidate with little history
// occasionally wins and earns the data that would settle its rank, without any
// separate epsilon-greedy schedule.
func (p Posterior) Sample(rng *rand.Rand) float64 {
	return sampleBeta(p.Alpha, p.Beta, rng)
}

// ThroughputScore maps tokens per second onto [0,1).
func ThroughputScore(tokensPerSecond float64) float64 {
	if tokensPerSecond <= 0 {
		return 0
	}
	return 1 - math.Exp(-tokensPerSecond/speedScaleTokS)
}

// TTFBScore maps time-to-first-byte onto [0,1], clamped at both ends.
func TTFBScore(ms float64) float64 {
	switch {
	case ms <= ttfbBestMs:
		return 1
	case ms >= ttfbWorstMs:
		return 0
	default:
		return 1 - (ms-ttfbBestMs)/(ttfbWorstMs-ttfbBestMs)
	}
}

// SpeedScore blends throughput with latency. Either signal alone is used on
// its own; with neither, the optimistic prior applies.
//
// ttfbMs is negative when no first-byte sample exists.
func SpeedScore(tokensPerSecond, ttfbMs float64) float64 {
	hasThroughput := tokensPerSecond > 0
	hasTTFB := ttfbMs >= 0
	switch {
	case hasThroughput && hasTTFB:
		return throughputWeight*ThroughputScore(tokensPerSecond) + ttfbWeight*TTFBScore(ttfbMs)
	case hasThroughput:
		return ThroughputScore(tokensPerSecond)
	case hasTTFB:
		return TTFBScore(ttfbMs)
	default:
		return speedPrior
	}
}

// Tier is a model's coarse capability class. It dominates the intelligence
// axis: a frontier model outranks the best-ranked small model regardless of
// their ranks.
type Tier int

const (
	TierUnknown Tier = iota
	TierSmall
	TierMedium
	TierLarge
	TierFrontier
)

// IntelligenceComposite is the pre-normalisation intelligence figure. Tier
// contributes a thousand per step while rank is square-root compressed, so
// rank refines an ordering it can never overturn.
func IntelligenceComposite(tier Tier, rank int) float64 {
	return float64(tier)*1000 - math.Sqrt(math.Max(1, float64(rank)))*rankScale
}

// IntelligenceScore normalises a composite against the range present in this
// request's candidate set, so the axis stays comparable whatever the chain
// happens to contain. A set with no spread scores 1 throughout.
func IntelligenceScore(composite, min, max float64) float64 {
	if max <= min {
		return 1
	}
	return (composite - min) / (max - min)
}

// HeadroomFactor demotes a candidate as its monthly budget runs down. It ramps
// rather than cuts off: falling to the floor still leaves the candidate usable,
// because a throttled provider beats no provider.
func HeadroomFactor(usedTokens, budgetTokens float64) float64 {
	if budgetTokens <= 0 {
		// An unknown budget is not evidence of exhaustion.
		return 1
	}
	remaining := 1 - usedTokens/budgetTokens
	return headroomRamp(remaining)
}

func headroomRamp(remaining float64) float64 {
	if remaining >= headroomRampStart {
		return 1
	}
	if remaining <= 0 {
		return headroomFloor
	}
	return headroomFloor + (1-headroomFloor)*(remaining/headroomRampStart)
}

// Score combines the axes and applies the guardrails.
type Score struct {
	Reliability  float64
	Speed        float64
	Intelligence float64
	Headroom     float64
	RateLimit    float64

	// Base is the convex mix before guardrails; Effective is after. Both are
	// reported because a dashboard needs to show why a good model was demoted.
	Base      float64
	Effective float64
}

// Combine mixes the axes under the given weights and applies the guardrails.
// Weights are renormalised so a custom set that does not sum to one still
// yields a comparable base.
func Combine(w Weights, reliability, speed, intelligence, headroom, rateLimit float64) Score {
	sum := w.Reliability + w.Speed + w.Intelligence
	base := 0.0
	if sum > 0 {
		base = (w.Reliability*reliability + w.Speed*speed + w.Intelligence*intelligence) / sum
	}
	return Score{
		Reliability:  reliability,
		Speed:        speed,
		Intelligence: intelligence,
		Headroom:     headroom,
		RateLimit:    rateLimit,
		Base:         base,
		Effective:    base * headroom * rateLimit,
	}
}

// sampleBeta draws from Beta(a,b) as the ratio of two Gamma draws.
func sampleBeta(a, b float64, rng *rand.Rand) float64 {
	x := sampleGamma(a, rng)
	y := sampleGamma(b, rng)
	if x+y <= 0 {
		return 0.5
	}
	return x / (x + y)
}

// sampleGamma draws from Gamma(shape,1) by Marsaglia and Tsang's method. The
// shape<1 case is handled by the standard boost, since a posterior parameter
// can legitimately be below one once decay has shrunk the counts.
func sampleGamma(shape float64, rng *rand.Rand) float64 {
	if shape <= 0 {
		return 0
	}
	if shape < 1 {
		return sampleGamma(shape+1, rng) * math.Pow(randFloat(rng), 1/shape)
	}
	d := shape - 1.0/3.0
	c := 1 / math.Sqrt(9*d)
	for {
		x := normFloat(rng)
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := randFloat(rng)
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

func randFloat(rng *rand.Rand) float64 {
	if rng == nil {
		return rand.Float64()
	}
	return rng.Float64()
}

func normFloat(rng *rand.Rand) float64 {
	if rng == nil {
		return rand.NormFloat64()
	}
	return rng.NormFloat64()
}
