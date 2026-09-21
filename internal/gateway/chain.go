package gateway

// Chain resolution and ordering, ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md).
//
// A request names a chain, not a model: the fallback chain is the ordered list
// of candidates the router walks. Which chain a request gets is resolved from
// the persisted routing configuration (ResolveChain), then ordered for this one
// request by the contextual bandit or by the operator's manual priority
// (OrderChain), then filtered against the request's own requirements
// (Eligible). Key selection is a separate concern (keyselect.go): the model
// ranking here must never depend on which credential ends up serving it.

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RoutingStrategy is the ported bandit strategy (scoring.ts:35). It is distinct
// from the pre-port Strategy enum in router.go, which the integration pass
// retires; the two coexist until then.
type RoutingStrategy string

const (
	// RoutingPriority follows the operator's manual chain order plus the
	// 429/failure penalty. It has no weight vector and never reaches the
	// bandit.
	RoutingPriority RoutingStrategy = "priority"
	// RoutingBalanced is the default (scoring.ts:60).
	RoutingBalanced RoutingStrategy = "balanced"
	RoutingSmartest RoutingStrategy = "smartest"
	RoutingFastest  RoutingStrategy = "fastest"
	RoutingReliable RoutingStrategy = "reliable"
	// RoutingCustom scores with the operator's own normalised weight vector.
	RoutingCustom RoutingStrategy = "custom"
	// RoutingCheapest orders by the catalogue's published input/output price,
	// cheapest known route first. It backs the auto:cheap / auto:cheapest /
	// auto:price / auto:budget aliases: there IS a real cost axis to sort on,
	// so those aliases must not silently collapse into Balanced. It is not an
	// operator-persistable strategy (ParseRoutingStrategy rejects it) - it is
	// only ever reached as an alias's OrderBy.
	RoutingCheapest RoutingStrategy = "cheapest"
)

// DefaultRoutingStrategy matches the upstream default (scoring.ts:60,
// router.ts:443-448).
const DefaultRoutingStrategy = RoutingBalanced

// ParseRoutingStrategy validates a persisted strategy string, falling back to
// the default for anything unrecognised (router.ts:443-448).
func ParseRoutingStrategy(raw string) RoutingStrategy {
	switch s := RoutingStrategy(strings.TrimSpace(raw)); s {
	case RoutingPriority, RoutingBalanced, RoutingSmartest, RoutingFastest,
		RoutingReliable, RoutingCustom:
		return s
	default:
		return DefaultRoutingStrategy
	}
}

// weightsFor maps a bandit strategy to its weight vector. Priority is handled
// by OrderChain before this is reached (it has no vector); everything that is
// not a recognised preset falls back to Balanced, and Custom uses the caller's
// vector. The preset values themselves live in scoring.go, verified against
// scoring.ts:46-53 - this is only the lookup, so there is one table of weights
// in the package, not two.
func weightsFor(s RoutingStrategy, custom Weights) Weights {
	switch s {
	case RoutingSmartest:
		return WeightsSmartest
	case RoutingFastest:
		return WeightsFastest
	case RoutingReliable:
		return WeightsReliable
	case RoutingCustom:
		return custom
	default:
		return WeightsBalanced
	}
}

// WeightsFor resolves a strategy to its weight vector - a preset for a named
// bandit strategy, the operator's saved vector for custom. Exposed so the
// integration layer can compute the base weights before a peak-hour adjustment.
func WeightsFor(strategy RoutingStrategy, custom Weights) Weights {
	return weightsFor(strategy, custom)
}

// ── Peak-hour dynamic weighting ─────────────────────────────────────────────
//
// During an operator-declared peak window free relays are congested, so a
// model's raw throughput is a weaker signal than its reliability: part of the
// speed weight moves onto reliability (peakAdjustedWeights, scoring.ts #760).
// It is OFF by default and every parameter is operator-set - routing that
// silently changes with the wall clock is impossible to reason about when
// someone reports "it picked a different model this evening".

// peakSpeedToReliability is the fraction of the speed weight moved onto
// reliability inside the peak window (PEAK_SPEED_TO_RELIABILITY, scoring.ts).
const peakSpeedToReliability = 0.6

// PeakHoursConfig is the persisted peak-window setting the router reads per
// request. The hour is interpreted in an explicit IANA timezone, never the
// host clock, so "peak" means the same thing on a UTC container as on the
// operator's own machine.
type PeakHoursConfig struct {
	Enabled   bool
	StartHour int
	EndHour   int
	Timezone  string
}

// isPeakExemptStrategy reports whether a strategy opts out of the peak
// adjustment: fastest and reliable are the two ends of the speed/reliability
// axis an operator picks deliberately, so shifting their weight would make one
// a noisy copy of the other (PEAK_EXEMPT_STRATEGIES, scoring.ts). Smartest
// routes through the capability model rather than this vector and priority has
// no vector, so neither reaches the adjustment.
func isPeakExemptStrategy(s RoutingStrategy) bool {
	return s == RoutingFastest || s == RoutingReliable
}

// IsPeakHours reports whether now falls inside the configured window. An empty
// window (StartHour == EndHour) is nothing, not everything; a window whose end
// is before its start spans midnight (isPeakHours, scoring.ts).
func IsPeakHours(config PeakHoursConfig, now time.Time) bool {
	if !config.Enabled {
		return false
	}
	if !validPeakHour(config.StartHour) || !validPeakHour(config.EndHour) {
		return false
	}
	if config.StartHour == config.EndHour {
		return false
	}
	h := hourInZone(now, config.Timezone)
	if config.StartHour < config.EndHour {
		return h >= config.StartHour && h < config.EndHour
	}
	return h >= config.StartHour || h < config.EndHour
}

func validPeakHour(h int) bool { return h >= 0 && h <= 23 }

// hourInZone is the 0-23 hour at now in the given IANA timezone, falling back to
// UTC for an unknown zone rather than failing - routing must never break because
// a settings row went stale (hourInTimezone, scoring.ts).
func hourInZone(now time.Time, timezone string) int {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	return now.In(loc).Hour()
}

// PeakAdjustedWeights returns the weight vector to score with, plus whether the
// adjustment actually fired (the dashboard labels its weight summary from that
// flag). It moves peakSpeedToReliability of the speed weight onto reliability
// inside the window for a non-exempt strategy, and returns the base untouched
// otherwise (peakAdjustedWeights, scoring.ts).
func PeakAdjustedWeights(base Weights, strategy RoutingStrategy, config PeakHoursConfig, now time.Time) (Weights, bool) {
	if isPeakExemptStrategy(strategy) {
		return base, false
	}
	if !IsPeakHours(config, now) {
		return base, false
	}
	shift := base.Speed * peakSpeedToReliability
	if shift <= 0 {
		return base, false
	}
	return Weights{
		Reliability:  base.Reliability + shift,
		Speed:        base.Speed - shift,
		Intelligence: base.Intelligence,
	}, true
}

// ChainEntry is one candidate model, carrying everything the bandit needs to
// score and gate it without a second query. It joins a chain row (position,
// enable flag) with the model's catalog fields (router.ts ChainRow, :149-182).
type ChainEntry struct {
	ModelDBID int64
	// Priority is the manual chain position: lower goes first, and it breaks
	// ties between (near-)equal bandit scores so the operator's order still
	// matters.
	Priority int
	Enabled  bool

	Platform      string
	ModelID       string
	DisplayName   string
	EndpointScope string
	// KeyID binds a custom relay model to the api_keys row carrying its
	// endpoint; nil for built-in platforms (router.ts:166-168).
	KeyID *int64

	// Tier is the cross-provider capability class derived from SizeLabel; it
	// dominates the intelligence axis. IntelRank refines the order within a
	// tier but can never overturn it (scoring.go IntelligenceComposite).
	Tier      Tier
	SizeLabel string
	IntelRank int

	SupportsVision    bool
	SupportsTools     bool
	SupportsReasoning bool
	// ContextWindow is the raw advertised window; nil means unknown, which is
	// never filtered (fitsContextWindowStrict, router.ts:1691-1693).
	ContextWindow *int64

	// The four rate-limit windows, nil when the provider publishes no cap.
	RPMLimit *int64
	RPDLimit *int64
	TPMLimit *int64
	TPDLimit *int64
	// MonthlyBudget is the human budget label ("~120M"); the headroom
	// guardrail parses it. Empty means "no budget info".
	MonthlyBudget string

	// PaidInputPerM and PaidOutputPerM are the catalogue's USD prices per
	// million tokens. Nil is unknown, not free.
	PaidInputPerM  *float64
	PaidOutputPerM *float64

	// MatchTier is an OUTER sort key, ahead of score, for both branches. Zero
	// is a normal candidate; a higher number is a fallback that may only serve
	// once every lower tier is exhausted, however good its live numbers - a
	// model reached through a group's auto-derived slug rather than the id the
	// client wrote, where answering on score alone would be a silent
	// substitution (router.ts:173-181).
	MatchTier int

	// WeightOverride multiplies the final bandit score (nil = no override,
	// treated as 1). 0 demotes a model out of bandit selection while leaving a
	// manual priority chain able to route to it (model-weight-overrides.ts).
	WeightOverride *float64
}

// Axes are the live per-candidate inputs the bandit needs but that live outside
// chain resolution: the decay-weighted reliability posterior (sampled or
// expected), the measured speed score, the combined quota-headroom guardrail
// and the rate-limit penalty damping. Chain ordering owns only the intelligence
// axis - it needs the per-request min/max of the composite - and the convex
// mix.
type Axes struct {
	Reliability float64
	Speed       float64
	Headroom    float64
	RateLimit   float64
}

// AxisScorer supplies those inputs. The integration pass satisfies it from the
// stats cache and the quota ledger; the rate-limit factor it returns is the
// multiplicative guardrail derived from the same PenaltyStore this slice owns.
// This slice only consumes AxisScorer, so the model bandit stays independent of
// every subsystem it scores against.
type AxisScorer interface {
	// Axes returns a candidate's four external axis inputs. sampled selects a
	// Thompson draw over the reliability posterior (live routing) versus its
	// expected value (a stable dashboard display).
	Axes(e *ChainEntry, sampled bool) Axes
}

// OrderChain orders a chain for one request (orderChain, router.ts:1071-1136).
//
// MatchTier is the outer sort key in both branches: a slug-resolved fallback
// can never outrank a directly requested model, whatever its live numbers.
//
//   - Priority strategy: sort by manual position, dense-rank the survivors
//     1..N, add the penalty to that rank, then sort by tier, effective rank,
//     raw priority, original index. Dense ranking is what makes one penalty
//     position mean one position regardless of how the priorities are spaced
//     (router.ts:1099-1110).
//   - Bandit strategies: score each entry and sort by tier, score descending,
//     then raw priority so the chain still breaks ties.
//
// sampled drives the bandit branch only: true for live routing (per-call
// randomness IS the exploration), false for a stable display. custom is used
// only when strategy is RoutingCustom. The penalty enters the PRIORITY branch as
// a position shift, distinct from the multiplicative guardrails the bandit
// branch reads through Axes; a nil scorer or nil penalties is tolerated (both
// read as neutral) so ordering never panics on a partial wiring.
func OrderChain(chain []ChainEntry, strategy RoutingStrategy, custom Weights, sampled bool, scorer AxisScorer, penalties *PenaltyStore) []ChainEntry {
	return orderChainWith(chain, strategy, weightsFor(strategy, custom), sampled, scorer, penalties)
}

// OrderChainWeighted orders a chain under an already-resolved weight vector,
// bypassing the strategy→preset lookup so the caller can score under
// peak-adjusted (or otherwise externally computed) weights. The strategy still
// selects the branch - priority, cheapest, or the bandit - and breaks ties;
// only the bandit branch reads the weights.
func OrderChainWeighted(chain []ChainEntry, strategy RoutingStrategy, weights Weights, sampled bool, scorer AxisScorer, penalties *PenaltyStore) []ChainEntry {
	return orderChainWith(chain, strategy, weights, sampled, scorer, penalties)
}

func orderChainWith(chain []ChainEntry, strategy RoutingStrategy, weights Weights, sampled bool, scorer AxisScorer, penalties *PenaltyStore) []ChainEntry {
	out := make([]ChainEntry, len(chain))
	copy(out, chain)
	if len(out) == 0 {
		return out
	}

	if strategy == RoutingPriority {
		return orderByPriority(out, penalties)
	}

	if strategy == RoutingCheapest {
		return orderByPrice(out)
	}

	// Intelligence is normalised across THIS request's candidate set, so the
	// axis stays comparable whatever the chain happens to contain
	// (router.ts:1124-1126). Computed here, before scoring, because the min/max
	// span every entry.
	composites := make([]float64, len(out))
	min, max := math.Inf(1), math.Inf(-1)
	for i := range out {
		c := IntelligenceComposite(out[i].Tier, out[i].IntelRank)
		composites[i] = c
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}

	scores := make([]float64, len(out))
	for i := range out {
		ax := Axes{Headroom: 1, RateLimit: 1, Speed: speedPrior}
		if scorer != nil {
			ax = scorer.Axes(&out[i], sampled)
		}
		intel := IntelligenceScore(composites[i], min, max)
		s := Combine(weights, ax.Reliability, ax.Speed, intel, ax.Headroom, ax.RateLimit).Effective
		// The per-model override multiplies the FINAL score, after the
		// guardrails, so a demoted model still composes with every strategy
		// (model-weight-overrides.ts). Absent = 1.
		if out[i].WeightOverride != nil {
			s *= *out[i].WeightOverride
		}
		scores[i] = s
	}

	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	// Stable so equal-score entries keep their incoming order, matching the
	// V8 stable sort the reference relies on.
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if out[ia].MatchTier != out[ib].MatchTier {
			return out[ia].MatchTier < out[ib].MatchTier
		}
		if scores[ia] != scores[ib] {
			return scores[ia] > scores[ib]
		}
		return out[ia].Priority < out[ib].Priority
	})
	res := make([]ChainEntry, len(out))
	for i, id := range idx {
		res[i] = out[id]
	}
	return res
}

func orderByPriority(out []ChainEntry, penalties *PenaltyStore) []ChainEntry {
	type item struct {
		e   ChainEntry
		idx int
		eff float64
	}
	items := make([]item, len(out))
	for i := range out {
		items[i] = item{e: out[i], idx: i}
	}
	// Rank first: sort by manual priority (original index breaks equal
	// priorities), then dense-rank 1..N, THEN add the penalty to that rank.
	// Penalising the raw priority instead would be silently inert whenever the
	// priorities are spaced wider than MAX_PENALTY (router.ts:1099-1104).
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].e.Priority != items[b].e.Priority {
			return items[a].e.Priority < items[b].e.Priority
		}
		return items[a].idx < items[b].idx
	})
	for rank := range items {
		items[rank].eff = float64(rank+1) + penaltyOf(penalties, items[rank].e.ModelDBID)
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].e.MatchTier != items[b].e.MatchTier {
			return items[a].e.MatchTier < items[b].e.MatchTier
		}
		if items[a].eff != items[b].eff {
			return items[a].eff < items[b].eff
		}
		if items[a].e.Priority != items[b].e.Priority {
			return items[a].e.Priority < items[b].e.Priority
		}
		return items[a].idx < items[b].idx
	})
	res := make([]ChainEntry, len(items))
	for i := range items {
		res[i] = items[i].e
	}
	return res
}

// orderByPrice orders a chain by the catalogue's published price, cheapest
// known route first (the auto:cheap family). It reuses the same blended
// input/output figure the smart router scores economy on, so there is one
// notion of "price" in the package, not a parallel one.
//
// A model whose price the catalogue does not publish sorts AFTER every priced
// one, whatever their figures: an unknown price is not evidence of being cheap,
// so it must never outrank a genuinely cheaper known route. Among equally
// priced (or equally unknown) candidates the manual chain priority stays the
// tie-breaker, and MatchTier remains the outer key so a directly requested
// model still leads its failovers.
func orderByPrice(out []ChainEntry) []ChainEntry {
	prices := make([]float64, len(out))
	known := make([]bool, len(out))
	for i := range out {
		prices[i], known[i] = smartBlendedPrice(out[i])
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if out[ia].MatchTier != out[ib].MatchTier {
			return out[ia].MatchTier < out[ib].MatchTier
		}
		if known[ia] != known[ib] {
			return known[ia] // a known price precedes an unknown one
		}
		if known[ia] && prices[ia] != prices[ib] {
			return prices[ia] < prices[ib]
		}
		return out[ia].Priority < out[ib].Priority
	})
	res := make([]ChainEntry, len(out))
	for i, id := range idx {
		res[i] = out[id]
	}
	return res
}

func penaltyOf(penalties *PenaltyStore, modelDBID int64) float64 {
	if penalties == nil {
		return 0
	}
	return penalties.Penalty(modelDBID)
}

// ── Model penalty ledger ─────────────────────────────────────────────────────
//
// A model that just rate-limited or failed is demoted for a while so failover
// stops re-picking it, then recovers on its own. The state is in-memory by
// design (reset on restart) and lives here, with ordering, because that is the
// only thing it affects: the priority strategy reads it as a position shift, and
// the bandit's multiplicative rate-limit guardrail is derived from the same
// figure by the integration facade. The failover loop only decides heavy vs
// light and records the outcome; it holds no penalty state of its own.

const (
	// penaltyPer429 is the demotion for a rate-limit/payment signal - the
	// strongest short-term health cue (PENALTY_PER_429, router.ts:267).
	penaltyPer429 = 3.0
	// penaltyPerFail is the demotion for an ordinary upstream failure
	// (5xx/timeout/empty stream) (PENALTY_PER_FAIL, router.ts:268).
	penaltyPerFail = 1.0
	// maxPenalty caps the demotion so a model does not sink forever
	// (MAX_PENALTY, router.ts:269).
	maxPenalty = 10.0
	// penaltyDecayAmount is removed per penaltyDecayInterval (DECAY_AMOUNT,
	// router.ts:271).
	penaltyDecayAmount = 1.0
	// penaltyDecayInterval is the recovery step (DECAY_INTERVAL_MS,
	// router.ts:270).
	penaltyDecayInterval = 2 * time.Minute
)

// PenaltyStore is the per-model 429/failure demotion ledger. It is safe for
// concurrent use from request goroutines; the lock never wraps I/O. Its
// RecordRateLimitHit/RecordModelFailure/RecordSuccess methods satisfy the
// failover loop's scorer seam.
type PenaltyStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[int64]*penaltyEntry
}

type penaltyEntry struct {
	count   int
	penalty float64
	// lastHit anchors the lazy decay: the stored penalty is what was recorded,
	// and the current value is that minus one unit per interval since. Reads
	// never write it back, so a model's penalty does not appear to move while
	// nothing happens (router.ts:317-319).
	lastHit time.Time
}

// NewPenaltyStore returns an empty ledger using the wall clock.
func NewPenaltyStore() *PenaltyStore {
	return &PenaltyStore{now: time.Now, m: map[int64]*penaltyEntry{}}
}

// RecordRateLimitHit records a 429/402 as the heavy demotion (recordRateLimitHit,
// router.ts:298-300).
func (p *PenaltyStore) RecordRateLimitHit(modelDBID int64) {
	p.record(modelDBID, penaltyPer429)
}

// RecordModelFailure records an ordinary upstream failure as the light demotion
// (recordModelFailure, router.ts:280-292).
func (p *PenaltyStore) RecordModelFailure(modelDBID int64) {
	p.record(modelDBID, penaltyPerFail)
}

func (p *PenaltyStore) record(modelDBID int64, weight float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	e, ok := p.m[modelDBID]
	if !ok {
		p.m[modelDBID] = &penaltyEntry{count: 1, penalty: weight, lastHit: now}
		return
	}
	// Catch the decay up to now, then add the fresh weight and cap. Decay is
	// applied on the hit, not by a ticker, so an idle model is indistinguishable
	// from one that was polled (router.ts:283-288).
	e.penalty = math.Max(0, e.penalty-decaySteps(now.Sub(e.lastHit))*penaltyDecayAmount)
	e.count++
	e.lastHit = now
	e.penalty = math.Min(e.penalty+weight, maxPenalty)
}

// RecordSuccess is a served request: it removes ONE unit of penalty and drops a
// model that reaches zero (recordSuccess, router.ts:305-313). This is a single
// step of recovery, not a full clear - a model that has 429'd repeatedly still
// carries the rest of its demotion after one good response.
func (p *PenaltyStore) RecordSuccess(modelDBID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[modelDBID]
	if !ok {
		return
	}
	e.penalty = math.Max(0, e.penalty-1)
	if e.penalty == 0 {
		delete(p.m, modelDBID)
	}
}

// Penalty is the model's current demotion in priority positions (getPenalty,
// router.ts:321-333). It returns the decayed value but writes nothing back
// except dropping a model that has decayed to zero, so the decay clock advances
// only on a recorded hit - a busy gateway polling the score does not decay
// penalties faster than an idle one. A model that has decayed to zero is
// dropped.
func (p *PenaltyStore) Penalty(modelDBID int64) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.penaltyLocked(modelDBID)
}

func (p *PenaltyStore) penaltyLocked(modelDBID int64) float64 {
	e, ok := p.m[modelDBID]
	if !ok {
		return 0
	}
	decayed := math.Max(0, e.penalty-decaySteps(p.now().Sub(e.lastHit))*penaltyDecayAmount)
	if decayed == 0 {
		delete(p.m, modelDBID)
		return 0
	}
	return decayed
}

// PenaltyStat is one model's demotion, for the dashboard penalty inspector.
type PenaltyStat struct {
	ModelDBID int64
	Count     int
	Penalty   float64
}

// Snapshot lists every model currently carrying a penalty, decay applied,
// heaviest first (getAllPenalties, router.ts:338-347).
func (p *PenaltyStore) Snapshot() []PenaltyStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	type row struct {
		id    int64
		count int
	}
	rows := make([]row, 0, len(p.m))
	for id, e := range p.m {
		rows = append(rows, row{id, e.count})
	}
	out := make([]PenaltyStat, 0, len(rows))
	for _, r := range rows {
		if pen := p.penaltyLocked(r.id); pen > 0 {
			out = append(out, PenaltyStat{ModelDBID: r.id, Count: r.count, Penalty: pen})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Penalty > out[b].Penalty })
	return out
}

// Clear forgets every model's penalty and reports how many were carrying one
// (clearAllPenalties, router.ts:355-359).
func (p *PenaltyStore) Clear() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]int64, 0, len(p.m))
	for id := range p.m {
		ids = append(ids, id)
	}
	n := 0
	for _, id := range ids {
		if p.penaltyLocked(id) > 0 {
			n++
		}
	}
	p.m = map[int64]*penaltyEntry{}
	return n
}

// decaySteps is the whole number of decay intervals in an elapsed duration
// (floor), matching the reference's integer division (router.ts:326).
func decaySteps(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(elapsed / penaltyDecayInterval)
}

// RequestGate is the set of request-level requirements a candidate must satisfy
// before key selection. Key-level gates (scope, cooldown, quota) belong to
// keyselect.go and the quota ledger.
type RequestGate struct {
	// EstimatedTokens is the input estimate plus a capped output reserve.
	EstimatedTokens int64
	RequireVision   bool
	RequireTools    bool
	// SkipModels / SkipPlatforms are ruled out for the rest of this request by
	// earlier attempts; never persisted (router.ts:2084-2091).
	SkipModels    map[int64]bool
	SkipPlatforms map[string]bool
}

// Eligible filters an ordered chain to the candidates that can serve this
// request, preserving order (the serving-loop gates, router.ts:2078-2140):
// disabled rows, models/platforms ruled out this request, vision/tools
// capability, the strict context-window fit, and a per-minute token ceiling a
// single request could never fit. An unknown context window or absent tpm_limit
// never excludes a model.
func Eligible(chain []ChainEntry, g RequestGate) []ChainEntry {
	out := make([]ChainEntry, 0, len(chain))
	for _, e := range chain {
		if !e.Enabled {
			continue
		}
		if g.SkipModels[e.ModelDBID] {
			continue
		}
		if g.SkipPlatforms[e.Platform] {
			continue
		}
		if g.RequireVision && !e.SupportsVision {
			continue
		}
		if g.RequireTools && !e.SupportsTools {
			continue
		}
		if e.ContextWindow != nil && g.EstimatedTokens > *e.ContextWindow {
			continue
		}
		if e.TPMLimit != nil && g.EstimatedTokens > *e.TPMLimit {
			continue
		}
		out = append(out, e)
	}
	return out
}

// tierFor maps a catalog size label to the capability tier (TIER_VALUE,
// scoring.ts:379-383). An unrecognised label is the floor of the axis, not an
// exclusion: intelligence is one weighted term, so an untiered model with
// strong reliability and speed still wins routes.
func tierFor(sizeLabel string) Tier {
	switch sizeLabel {
	case "Frontier":
		return TierFrontier
	case "Large":
		return TierLarge
	case "Medium":
		return TierMedium
	case "Small":
		return TierSmall
	default:
		return TierUnknown
	}
}

// ChainError is a client-facing refusal to resolve a chain: an unknown or empty
// profile, or a global sort with nothing enabled. Status is the HTTP status the
// reference attaches (400 for all of these), so the integration layer can map
// it without re-deriving the reason.
type ChainError struct {
	Status  int
	Message string
}

func (e *ChainError) Error() string { return e.Message }

// ResolvedChain is what a request's model string resolves to: the candidate
// membership, a stable key for sticky-session namespacing, and the strategy the
// caller should order by.
type ResolvedChain struct {
	Chain []ChainEntry
	// StrategyKey namespaces sticky sessions so a conversation pinned under one
	// routing intent ("auto", "auto:smart", "auto:<profile>") does not leak its
	// preferred model into another (proxy.ts getSessionKey).
	StrategyKey string
	// OrderBy is the strategy OrderChain should use. For a global-sort alias it
	// is the strategy the alias names; for `auto` and a named profile it is the
	// operator's configured strategy, passed through unchanged. The reference
	// orders a global-sort chain inside getChainByGlobalSort and then re-orders
	// it in routeRequest with the configured strategy, discarding the alias's
	// ordering; returning OrderBy lets the caller order exactly once, by what
	// the alias actually means.
	OrderBy RoutingStrategy
}

// globalSortAliases collapse the accepted spellings of each axis onto one token
// (GLOBAL_SORT_ALIASES, router.ts:1160-1166).
var globalSortAliases = map[string]string{
	"smart": "smart", "smartest": "smart", "intelligence": "smart",
	"fast": "fast", "fastest": "fast", "speed": "fast",
	"cheap": "cheap", "cheapest": "cheap", "price": "cheap", "budget": "cheap",
	"reliable": "reliable", "reliability": "reliable",
	"balanced": "balanced",
}

// GlobalSortAliases returns the canonical axis tokens a client may call as
// auto:<axis>, so the models list can advertise them instead of leaving them
// to be discovered by reading the source.
func GlobalSortAliases() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, axis := range globalSortAliases {
		if _, ok := seen[axis]; ok {
			continue
		}
		seen[axis] = struct{}{}
		out = append(out, axis)
	}
	sort.Strings(out)
	return out
}

// aliasStrategy maps a global-sort axis onto the strategy it orders by
// (getChainByGlobalSort strategyMap, router.ts:1244-1251). `cheap` (and its
// cheapest/price/budget spellings) orders by the catalogue's published price,
// not Balanced: the reference's catalog was free-tier only, but this one
// carries real per-token prices, so there is a genuine cost axis to sort on.
func aliasStrategy(axis string) RoutingStrategy {
	switch axis {
	case "smart":
		return RoutingSmartest
	case "fast":
		return RoutingFastest
	case "reliable":
		return RoutingReliable
	case "cheap":
		return RoutingCheapest
	default: // "balanced"
		return RoutingBalanced
	}
}

// StrategyKeyFor derives the sticky-session namespace for a request's model
// string, matching the StrategyKey ResolveChain returns (proxy.ts getSessionKey).
// It is pure - no database - so a success handler can recover the same namespace
// the request routed under without re-resolving the chain.
func StrategyKeyFor(modelString string) string {
	lower := strings.ToLower(strings.TrimSpace(modelString))
	if !strings.HasPrefix(lower, "auto:") {
		return "auto"
	}
	suffix := strings.TrimSpace(lower[len("auto:"):])
	if suffix == "" {
		return "auto"
	}
	if axis, ok := globalSortAliases[suffix]; ok {
		return "auto:" + axis
	}
	return "auto:" + suffix
}

// ResolveChain resolves a request's model string to its candidate chain
// (resolveRoutingChain, router.ts:1280-1323), in the reference's priority order:
//
//  1. empty / "auto" / any non-"auto:" string → the active chain (the active
//     profile if one is set, else the global fallback_config);
//  2. "auto:<axis>" for a known sort alias → the whole enabled catalog, to be
//     ordered by the alias's strategy;
//  3. "auto:<name>" → the profile of that name.
//
// configured is the operator's persisted strategy, used as OrderBy for cases
// that do not name their own. An unknown profile, an empty named/aliased chain,
// or an active profile with nothing enabled is a ChainError, never a panic. A
// legacy install with no active profile and an empty fallback returns an empty
// chain (no error), matching the reference so the ordinary "all models
// exhausted" path still applies (activeChainOrThrow, router.ts:1264-1278).
func ResolveChain(db *sql.DB, modelString string, configured RoutingStrategy) (*ResolvedChain, error) {
	lower := strings.ToLower(strings.TrimSpace(modelString))
	strategyKey := StrategyKeyFor(modelString)

	if !strings.HasPrefix(lower, "auto:") {
		// "", "auto", or a concrete model id, all served over the active chain.
		// When the client named a concrete model the gateway advertises, honour
		// it ahead of the strategy: preferConcreteModel marks it MatchTier 0 and
		// demotes the rest into failover tiers, so OrderChain places it first
		// whatever strategy it later scores by while the rest stay available for
		// failover (routeRequest's preferred-model handling, router.ts:2006-2013).
		chain, err := activeChainOrThrow(db)
		if err != nil {
			return nil, err
		}
		preferConcreteModel(chain, lower)
		return &ResolvedChain{Chain: chain, StrategyKey: strategyKey, OrderBy: configured}, nil
	}

	suffix := strings.TrimSpace(lower[len("auto:"):])
	if suffix == "" {
		chain, err := activeChainOrThrow(db)
		if err != nil {
			return nil, err
		}
		return &ResolvedChain{Chain: chain, StrategyKey: strategyKey, OrderBy: configured}, nil
	}

	if axis, ok := globalSortAliases[suffix]; ok {
		chain, err := globalSortChain(db)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			return nil, &ChainError{Status: 400, Message: fmt.Sprintf("no enabled models available for global sort %q", suffix)}
		}
		return &ResolvedChain{Chain: chain, StrategyKey: strategyKey, OrderBy: aliasStrategy(axis)}, nil
	}

	chain, found, err := profileChainByName(db, suffix)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &ChainError{Status: 400, Message: fmt.Sprintf("profile %q not found; use 'auto' for the active chain, or call /v1/models for the options", suffix)}
	}
	if !anyEnabled(chain) {
		// One voice for "this list cannot route": the explainer distinguishes
		// an empty list from one whose platforms have no key.
		return nil, &ChainError{Status: 400, Message: ExplainUnroutableChain(db, "auto:"+suffix)}
	}
	return &ResolvedChain{Chain: chain, StrategyKey: strategyKey, OrderBy: configured}, nil
}

// preferConcreteModel honours an explicitly requested model ahead of the active
// chain's strategy. When the request names a concrete model the active chain
// actually carries, that model's candidates keep MatchTier 0 (directly
// requested) and every other candidate is demoted one failover tier. Because
// MatchTier is OrderChain's outer sort key, the requested model then leads
// whatever bandit or price strategy scores the chain, while the demoted rest
// stay in the chain for failover - routing over a different model only once the
// requested one is exhausted, never as a silent substitution.
//
// requested is the already-lowercased, trimmed model string. "", "auto" and any
// string that does not name a member of the chain leave every tier untouched:
// there is nothing concrete to prefer, so the chain routes normally.
func preferConcreteModel(chain []ChainEntry, requested string) {
	if requested == "" || requested == "auto" {
		return
	}
	matched := false
	for i := range chain {
		if requestedModelMatches(chain[i], requested) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	for i := range chain {
		if requestedModelMatches(chain[i], requested) {
			continue // the directly requested model stays at MatchTier 0
		}
		chain[i].MatchTier++
	}
}

// requestedModelMatches reports whether a candidate is the model a client named,
// accepting both the bare model id and the platform-qualified "platform/model"
// spelling the models list advertises (inference.go models handler). requested
// is compared case-insensitively.
func requestedModelMatches(e ChainEntry, requested string) bool {
	return strings.EqualFold(e.ModelID, requested) ||
		strings.EqualFold(e.Platform+"/"+e.ModelID, requested)
}

// activeChainOrThrow returns the active chain, or a ChainError when an active
// profile has nothing enabled - the reference refuses rather than silently
// routing over the whole catalog (router.ts:1264-1278). A legacy install with
// no active profile keeps the ordinary exhaustion path, so an empty chain there
// is returned as-is.
func activeChainOrThrow(db *sql.DB) ([]ChainEntry, error) {
	profileID, active, err := activeProfileID(db)
	if err != nil {
		return nil, err
	}
	if active {
		chain, err := profileChain(db, profileID)
		if err != nil {
			return nil, err
		}
		if !anyEnabled(chain) {
			name, _ := profileName(db, profileID)
			label := ""
			if name != "" {
				label = fmt.Sprintf(" %q", name)
			}
			return nil, &ChainError{Status: 400, Message: fmt.Sprintf(
				"the active fallback chain%s has no enabled models; enable models for it, switch the active chain, or name another with \"auto:<chain>\"", label)}
		}
		return chain, nil
	}
	return fallbackChain(db)
}

// activeProfileID reads the active profile id, verifying the profile still
// exists (getActiveProfileId, profile-models.ts:3-10). A missing or unparseable
// setting, or a dangling id, means "no active profile".
func activeProfileID(db *sql.DB) (int64, bool, error) {
	var value string
	err := db.QueryRow("SELECT value FROM settings WHERE key = 'active_profile_id'").Scan(&value)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if perr != nil {
		return 0, false, nil
	}
	var exists int64
	err = db.QueryRow("SELECT id FROM profiles WHERE id = ?", id).Scan(&exists)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func profileName(db *sql.DB, id int64) (string, error) {
	var name string
	err := db.QueryRow("SELECT name FROM profiles WHERE id = ?", id).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// The catalog columns every chain query selects, in scan order. profile_models
// carries no per-row enable flag in this schema (unlike the reference), so a
// profile row's enabled state is the model's own - the JOIN already requires
// m.enabled = 1, so those rows are always enabled. fallback_config does carry
// its own enable flag.
const chainModelColumns = `m.platform, m.model_id, m.display_name, m.intelligence_rank,
	m.size_label, m.monthly_token_budget,
	m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit,
	m.supports_vision, m.supports_tools, m.supports_reasoning,
	m.context_window, m.key_id, m.endpoint_scope,
	m.paid_input_per_m, m.paid_output_per_m`

// expandChainKeys turns each catalogue model into one candidate per usable key
// of its platform.
//
// Catalogue rows carry no key_id - a shipped model is not bound to anyone's
// credential - but a route needs one, so without this every catalogue model is
// skipped and the router reports that every candidate was skipped even with
// healthy keys configured. Custom-endpoint rows already name their key and
// pass through untouched.
//
// A key's model_scope_json is honoured here: a scoped key must not become a
// candidate for a model it was not granted, or the operator's restriction
// would be silently ignored.
//
// Linked-login credentials (encrypted_key `link:<provider>`) are excluded from
// this generic expansion: a subscription/OAuth login is authorised only for the
// models it discovered, which are seeded as their own key-bound, login-scoped
// rows and pass through above with KeyID already set. Letting a link: key soak
// up every catalogue model of its platform would route arbitrary models through
// a credential that was never granted them.
func expandChainKeys(db *sql.DB, chain []ChainEntry) ([]ChainEntry, error) {
	type usableKey struct {
		id    int64
		scope []string
	}

	rows, err := db.Query(`
		SELECT id, platform, COALESCE(model_scope_json, '')
		  FROM api_keys
		 WHERE enabled = 1 AND status <> 'error'
		   AND encrypted_key NOT LIKE 'link:%'
		 ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	byPlatform := map[string][]usableKey{}
	for rows.Next() {
		var (
			id       int64
			platform string
			scopeRaw string
		)
		if err := rows.Scan(&id, &platform, &scopeRaw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key := usableKey{id: id}
		if scopeRaw != "" {
			_ = json.Unmarshal([]byte(scopeRaw), &key.scope)
		}
		byPlatform[platform] = append(byPlatform[platform], key)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	out := make([]ChainEntry, 0, len(chain))
	for _, entry := range chain {
		if entry.KeyID != nil {
			out = append(out, entry)
			continue
		}
		for _, key := range byPlatform[entry.Platform] {
			if !keyServesModel(key.scope, entry.ModelID) {
				continue
			}
			candidate := entry
			id := key.id
			candidate.KeyID = &id
			out = append(out, candidate)
		}
		// A model whose platform has no usable key is dropped rather than
		// carried as an unroutable candidate: the caller's diagnostics should
		// say "no key for this provider", not "every candidate was skipped".
	}
	return out, nil
}

// keyServesModel reports whether a key's allow-list covers a model. An empty
// scope means unscoped, which serves every model of the platform.
func keyServesModel(scope []string, modelID string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, allowed := range scope {
		if allowed == modelID {
			return true
		}
	}
	return false
}

func profileChain(db *sql.DB, profileID int64) ([]ChainEntry, error) {
	rows, err := db.Query(`
		SELECT pm.model_db_id, pm.position AS priority, 1 AS enabled, `+chainModelColumns+`
		FROM profile_models pm
		JOIN models m ON m.id = pm.model_db_id AND m.enabled = 1 AND m.available = 1
		WHERE pm.profile_id = ?
		ORDER BY pm.position ASC`, profileID)
	if err != nil {
		return nil, err
	}
	return scanChain(db, rows)
}

func profileChainByName(db *sql.DB, name string) ([]ChainEntry, bool, error) {
	var id int64
	err := db.QueryRow("SELECT id FROM profiles WHERE LOWER(name) = ?", strings.ToLower(name)).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	chain, err := profileChain(db, id)
	if err != nil {
		return nil, false, err
	}
	return chain, true, nil
}

func fallbackChain(db *sql.DB) ([]ChainEntry, error) {
	rows, err := db.Query(`
		SELECT fc.model_db_id, fc.position AS priority, fc.enabled, ` + chainModelColumns + `
		FROM fallback_config fc
		JOIN models m ON m.id = fc.model_db_id AND m.enabled = 1 AND m.available = 1
		ORDER BY fc.position ASC`)
	if err != nil {
		return nil, err
	}
	return scanChain(db, rows)
}

// globalSortChain is the whole enabled catalog: a global sort ignores the
// chain's ORDER but not its enable flags - a model switched off in the
// fallback_config stays off, and a fresh catalog row with no chain row yet
// defaults in (getChainByGlobalSort, router.ts:1223-1242). This schema has no
// per-profile enable flag, so only fallback_config gates membership regardless
// of whether a profile is active.
func globalSortChain(db *sql.DB) ([]ChainEntry, error) {
	rows, err := db.Query(`
		SELECT m.id AS model_db_id, 0 AS priority, 1 AS enabled, ` + chainModelColumns + `
		FROM models m
		LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
		WHERE m.enabled = 1 AND m.available = 1 AND COALESCE(fc.enabled, 1) = 1`)
	if err != nil {
		return nil, err
	}
	return scanChain(db, rows)
}

func scanChain(db *sql.DB, rows *sql.Rows) ([]ChainEntry, error) {
	defer rows.Close()
	var chain []ChainEntry
	for rows.Next() {
		var (
			e        ChainEntry
			enabled  int
			rpm, rpd sql.NullInt64
			tpm, tpd sql.NullInt64
			ctx, key sql.NullInt64
			vision   int
			tools    int
			reason   int
			priceIn  sql.NullFloat64
			priceOut sql.NullFloat64
		)
		if err := rows.Scan(
			&e.ModelDBID, &e.Priority, &enabled,
			&e.Platform, &e.ModelID, &e.DisplayName, &e.IntelRank,
			&e.SizeLabel, &e.MonthlyBudget,
			&rpm, &rpd, &tpm, &tpd,
			&vision, &tools, &reason,
			&ctx, &key, &e.EndpointScope, &priceIn, &priceOut,
		); err != nil {
			return nil, err
		}
		e.Enabled = enabled != 0
		e.SupportsVision = vision != 0
		e.SupportsTools = tools != 0
		e.SupportsReasoning = reason != 0
		e.Tier = tierFor(e.SizeLabel)
		e.RPMLimit = nullInt(rpm)
		e.RPDLimit = nullInt(rpd)
		e.TPMLimit = nullInt(tpm)
		e.TPDLimit = nullInt(tpd)
		e.ContextWindow = nullInt(ctx)
		e.KeyID = nullInt(key)
		e.PaidInputPerM = nullFloat(priceIn)
		e.PaidOutputPerM = nullFloat(priceOut)
		chain = append(chain, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Expansion happens before weighting so the overrides apply to the
	// candidates that will actually be routed.
	expanded, err := expandChainKeys(db, chain)
	if err != nil {
		return nil, err
	}
	return applyWeightOverrides(db, expanded)
}

// applyWeightOverrides attaches each model's persisted routing weight override
// (model_overrides table; model-weight-overrides.ts). Only finite values in
// [0,2] are applied; anything else is dropped, leaving the model at its natural
// score (an implicit 1). Read once for the whole chain.
func applyWeightOverrides(db *sql.DB, chain []ChainEntry) ([]ChainEntry, error) {
	if len(chain) == 0 {
		return chain, nil
	}
	rows, err := db.Query("SELECT model_id, weight FROM model_overrides")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overrides := map[string]float64{}
	for rows.Next() {
		var id string
		var w float64
		if err := rows.Scan(&id, &w); err != nil {
			return nil, err
		}
		if !math.IsInf(w, 0) && !math.IsNaN(w) && w >= 0 && w <= 2 {
			overrides[id] = w
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range chain {
		if w, ok := overrides[chain[i].ModelID]; ok {
			v := w
			chain[i].WeightOverride = &v
		}
	}
	return chain, nil
}

func anyEnabled(chain []ChainEntry) bool {
	for i := range chain {
		if chain[i].Enabled {
			return true
		}
	}
	return false
}

func nullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

// ── Sticky sessions ─────────────────────────────────────────────────────────
//
// A conversation keeps the model it started on. Without this, a rate limit or a
// failover mid-conversation would move the next turn to a different model, which
// has no idea it is continuing someone else's work - a visible quality cliff.

// StickyTTL is how long a conversation keeps its model after its last turn
// (STICKY_TTL_MS, proxy.ts:167).
const StickyTTL = 30 * time.Minute

// stickySoftCap / stickyHardCap bound the store: past the soft cap a Set prunes
// expired entries, and past the hard cap it evicts the oldest, so a flood of
// one-shot conversations cannot grow it without bound (proxy.ts:289-304).
const (
	stickySoftCap = 500
	stickyHardCap = 1000
)

// SessionKey derives the sticky-session key for a conversation (getSessionKey,
// proxy.ts:253-264). A client-supplied session id header takes precedence;
// otherwise the key is the SHA-1 of the first user message. The strategy key is
// folded in so the same conversation routed under different intents ("auto" vs
// "auto:smart") does not share a pin. Returns "" when there is nothing to key
// on, which the caller reads as "no sticky session". SHA-1 is a cache key here,
// not a security primitive, matching the reference.
func SessionKey(firstUserMessage, sessionIDHeader, strategyKey string) string {
	if sessionIDHeader != "" {
		if strategyKey != "" {
			return "hdr:" + sessionIDHeader + "::" + strategyKey
		}
		return "hdr:" + sessionIDHeader
	}
	if firstUserMessage == "" {
		return ""
	}
	payload := firstUserMessage
	if strategyKey != "" {
		payload = firstUserMessage + "::" + strategyKey
	}
	sum := sha1.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// StickyStore maps a conversation key to its pinned model. It is safe for
// concurrent use from request goroutines; the lock never wraps I/O.
type StickyStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]stickyEntry
}

type stickyEntry struct {
	modelDBID int64
	lastUsed  time.Time
}

// NewStickyStore returns an empty store with the default TTL and clock.
func NewStickyStore() *StickyStore {
	return &StickyStore{ttl: StickyTTL, now: time.Now, entries: map[string]stickyEntry{}}
}

// Get returns the model pinned to a session when the key is non-empty and the
// pin is still within its TTL (getStickyModel, proxy.ts:266-281); an expired pin
// is dropped. The caller applies the "conversation already has an assistant
// turn" precondition - a first turn has nothing to stick to.
func (s *StickyStore) Get(key string) (int64, bool) {
	if key == "" {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return 0, false
	}
	if s.now().Sub(e.lastUsed) > s.ttl {
		delete(s.entries, key)
		return 0, false
	}
	return e.modelDBID, true
}

// Set pins a model to a session and refreshes its timestamp (setStickyModel,
// proxy.ts:283-306). It prunes expired entries past the soft cap and evicts the
// oldest past the hard cap.
func (s *StickyStore) Set(key string, modelDBID int64) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.entries[key] = stickyEntry{modelDBID: modelDBID, lastUsed: now}
	if len(s.entries) <= stickySoftCap {
		return
	}
	for k, v := range s.entries {
		if now.Sub(v.lastUsed) > s.ttl {
			delete(s.entries, k)
		}
	}
	if len(s.entries) <= stickyHardCap {
		return
	}
	type aged struct {
		key      string
		lastUsed time.Time
	}
	all := make([]aged, 0, len(s.entries))
	for k, v := range s.entries {
		all = append(all, aged{k, v.lastUsed})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].lastUsed.Before(all[b].lastUsed) })
	for i := range len(s.entries) - stickyHardCap {
		delete(s.entries, all[i].key)
	}
}

// ResolveStickyPreference filters a sticky pin down to something still routable:
// the pinned model must still be present and enabled in the chain
// (resolveStickyPreference, router.ts:2272-2278). Otherwise the pin is dropped
// and the request routes normally.
func ResolveStickyPreference(stickyModelDBID int64, chain []ChainEntry) (int64, bool) {
	for i := range chain {
		if chain[i].ModelDBID == stickyModelDBID && chain[i].Enabled {
			return stickyModelDBID, true
		}
	}
	return 0, false
}

// PreferFront moves a preferred model to the head of an already-ordered chain so
// a sticky or pinned conversation does not switch models mid-stream (the
// in-chain case of routeRequest's preferred-model handling, router.ts:2006-2013).
// The rest keep their order. A model not in the chain, or already at the front,
// leaves the chain unchanged - injecting a non-member is the caller's explicit
// pin path, which needs the database.
func PreferFront(chain []ChainEntry, modelDBID int64) []ChainEntry {
	idx := -1
	for i := range chain {
		if chain[i].ModelDBID == modelDBID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return chain
	}
	out := make([]ChainEntry, 0, len(chain))
	out = append(out, chain[idx])
	out = append(out, chain[:idx]...)
	out = append(out, chain[idx+1:]...)
	return out
}
