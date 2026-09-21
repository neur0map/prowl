package gateway

// The failover driver, ported from FreeLLMAPI's server/src/lib/fallback-loop.ts
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md). It walks
// the routing chain across up to 20 attempts inside a wall-clock budget,
// classifies each upstream failure (errclass.go), applies per-key/-model/
// -platform skip state, benches through the cooldown engine, demotes the model
// only when it is genuinely at fault, and renders an honest terminal status
// when the chain is exhausted.
//
// This is a self-contained engine with injected dependencies (Chain, Cooldowns,
// Scorer, Dispatcher, and two optional hooks) so the lead can wire the real
// router/engine in the integration pass and this slice is testable with fakes.
// Nothing here writes to an http.ResponseWriter: the loop returns a *Result and
// the caller renders it, including the X-Fallback-* header data.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Driver-owned constants (the rest are reused from cooldown.go) ────────────

const (
	// fallbackMaxRetries caps failover hops (fallback-loop.ts:59).
	fallbackMaxRetries = 20

	// defaultFallbackTimeBudget bounds the failover chain; attempts 0 and 1
	// always run, attempts >= 2 are gated by it (fallback-loop.ts:129).
	//
	// It is measured against CHURN, not total wall clock: the single most
	// expensive attempt is excused. A chat-sized request fails fast, so the
	// gate behaves exactly as the reference's does. An agent turn streaming
	// tens of thousands of tokens can spend this much inside one honest
	// attempt, and charging that to the budget left the chain structurally
	// unable to fail over - a live run died after two hops with hundreds of
	// candidates still untried.
	defaultFallbackTimeBudget = 45 * time.Second

	// revalidationDedupe stops concurrent 401s stampeding one key's health
	// re-check (fallback-loop.ts:365).
	revalidationDedupe = 30 * time.Second

	// Attempt-trail / detail-header caps (fallback-loop.ts:469-470,489,492 and
	// summarizeAttemptError's 200-char cap).
	trailMaxShown          = 10
	trailHeaderMaxLength   = 1024
	detailHeaderMaxLength  = 2048
	detailSummaryMaxLength = 120
	summaryMaxLength       = 200
)

// ── Public value types ───────────────────────────────────────────────────────

// Outcome is how a dispatched attempt finished. The zero value means the
// attempt FAILED (described by DispatchResult.Status/Body/Err); the two named
// values are the only successes, so a dispatcher that forgets to set one is
// caught as a contract violation rather than silently swallowing the request.
type Outcome int

const (
	// OutcomeDone: the attempt succeeded and the full response was produced.
	OutcomeDone Outcome = 1
	// OutcomeCommitted: a stream already flushed real bytes to the client, then
	// handled its own mid-stream error; no failover is possible, so stop.
	OutcomeCommitted Outcome = 2
)

// RouteKey identifies one (platform, model, key) triple. It is the element of
// SkipState.Keys and the natural map key for per-key bookkeeping, so nothing in
// this slice depends on Slice A's QuotaKey string format.
type RouteKey struct {
	Platform string
	ModelID  string
	KeyID    int64
}

func routeKeyOf(r Route) RouteKey { return RouteKey{r.Platform, r.ModelID, r.KeyID} }

// Route is one selected candidate the driver will attempt. The chain (Slice B)
// builds it, already quota-admitted; the driver only reads it.
type Route struct {
	Platform  string
	ModelID   string
	ModelDBID int64
	KeyID     int64
	// KeyLabel is the operator-assigned label, shown in diagnostics; never the
	// credential and never the key id.
	KeyLabel string
	// BaseURL is the key's endpoint, used to recognise a local endpoint whose
	// cooldown never escalates.
	BaseURL string
	// EndpointScope is the model's endpoint identity: empty for a shipped
	// catalogue model, the custom relay's base URL, or a login's
	// link:<platform> scope. It keeps two candidates that share a
	// (platform, model_id) - a catalogue row and a linked-login row, say -
	// scored and rate-limited apart.
	EndpointScope string
	// The published rate-limit windows, nil when the provider publishes no cap.
	// They tell the cooldown/quota engines a bench or admission is measured
	// rather than guessed.
	RPMLimit *int64
	RPDLimit *int64
	TPMLimit *int64
	TPDLimit *int64
	// TokenMultiplier bills an account-wide token cap at this model's own rate
	// when it differs from the OpenAI usage total (NavyAI). Zero or below is
	// one-to-one; the quota ledger reads it per lease.
	TokenMultiplier float64
	// SupportsReasoning is the chosen model's advertised reasoning capability.
	// The dispatcher drops reasoning-only request params (reasoning_effort) for
	// a model that lacks it, so a client that always sends an effort level does
	// not make a non-reasoning model's upstream 400 ("does not support the
	// effort parameter").
	SupportsReasoning bool
	// Release frees the in-flight lease taken when the route was selected. The
	// loop calls it once the attempt finishes, however it finished. Optional.
	Release func()
}

// RouteError is what Chain.Route returns when the pool is exhausted before any
// upstream is tried. Diagnostics are the per-candidate reasons the router
// accumulated; the zero-attempt exhaustion body buckets them.
type RouteError struct {
	Status      int
	Message     string
	Diagnostics []string
}

func (e *RouteError) Error() string { return e.Message }

// SkipState is the request-scoped exclusion set threaded through the loop and
// mutated as failures accrue. Nothing here outlives the response: a provider
// that blipped once is a fresh candidate on the very next request.
type SkipState struct {
	Keys      map[RouteKey]struct{}
	Models    map[int64]struct{}
	Platforms map[string]struct{}
}

// NewSkipState returns an empty request-scoped skip set.
func NewSkipState() *SkipState {
	return &SkipState{
		Keys:      map[RouteKey]struct{}{},
		Models:    map[int64]struct{}{},
		Platforms: map[string]struct{}{},
	}
}

// DispatchResult is the outcome of one upstream attempt. On success Outcome is
// OutcomeDone/OutcomeCommitted; on failure Outcome is 0 and Status/Body/Err
// describe how it failed - Status 0 means no response (a transport fault or a
// synthetic dead-turn error carried in Err). RetryAfter is the provider's
// Retry-After header, the one signal that outranks our own cooldown ladder.
type DispatchResult struct {
	Outcome    Outcome
	Status     int
	Body       string
	Err        error
	RetryAfter time.Duration
}

func (r DispatchResult) succeeded() bool {
	return r.Outcome == OutcomeDone || r.Outcome == OutcomeCommitted
}

// Attempt is one failed hop, oldest first, carrying both the trail label and
// the opt-in detail (timings + a redacted provider summary).
type Attempt struct {
	Platform   string
	ModelID    string
	KeyOrdinal int
	Class      AttemptClass

	StartOffset time.Duration
	Duration    time.Duration
	Summary     string
}

// ExhaustionKind is the coarse class a surface may remap to its own wire
// vocabulary (fallback-loop.ts:588-604).
type ExhaustionKind string

const (
	ExhaustAuth            ExhaustionKind = "auth"
	ExhaustBadRequest      ExhaustionKind = "bad_request"
	ExhaustRateLimit       ExhaustionKind = "rate_limit"
	ExhaustUnavailable     ExhaustionKind = "unavailable"
	ExhaustContextTooLarge ExhaustionKind = "context_too_large"
	ExhaustModelNotFound   ExhaustionKind = "model_not_found"
	ExhaustUpstream        ExhaustionKind = "upstream"
)

// Exhaustion is the terminal error body the driver renders when the chain
// fails, never a 500 (that status is reserved for our own bugs).
type Exhaustion struct {
	Status  int
	Type    string
	Code    string
	Kind    ExhaustionKind
	Message string
	// RetryAt is the soonest moment a benched candidate recovers, for a 429's
	// Retry-After. Zero when there is no known reset.
	RetryAt time.Time
}

// RetryAfterSeconds renders the Retry-After header value (ceil seconds) for a
// 429 exhaustion carrying a concrete reset; ok is false when there is none.
func (e *Exhaustion) RetryAfterSeconds(now time.Time) (int, bool) {
	if e.RetryAt.IsZero() {
		return 0, false
	}
	s := int(math.Ceil(e.RetryAt.Sub(now).Seconds()))
	if s < 0 {
		s = 0
	}
	return s, true
}

// ResultStatus is why the loop stopped.
type ResultStatus int

const (
	// StatusSucceeded: an attempt produced the response (Done or Committed).
	StatusSucceeded ResultStatus = iota
	// StatusFatal: a non-retryable error; render the provider's own error.
	StatusFatal
	// StatusExhausted: the attempt cap, the time budget, or the breaker stopped
	// the loop; render Exhaustion.
	StatusExhausted
	// StatusRoutingExhausted: Chain.Route gave up; render Exhaustion.
	StatusRoutingExhausted
	// StatusClientGone: the client disconnected; there is nothing to render.
	StatusClientGone
	// StatusCommittedError: a stream flushed real bytes and then broke. The
	// bytes cannot be retracted so the loop stops without failover, but the
	// endpoint did NOT serve a good response - no success-side accounting runs
	// and the caller records it as the failure it is.
	StatusCommittedError
)

// Result is the loop's return value - everything the caller needs to render the
// response and stamp the X-Fallback-* headers, as data.
type Result struct {
	Status  ResultStatus
	Outcome Outcome // valid when StatusSucceeded

	// Route is the served route (StatusSucceeded) or the fatal route
	// (StatusFatal); nil otherwise.
	Route *Route

	// Attempts is the failed-hop trail, oldest first; FailedAttempts is its
	// length (what X-Fallback-Attempts reports).
	Attempts       []Attempt
	FailedAttempts int

	// Exhaustion is set for StatusExhausted / StatusRoutingExhausted.
	Exhaustion *Exhaustion

	// Err is the fatal error (StatusFatal) or the last upstream error.
	Err error

	// TimedOut is true when the wall-clock budget stopped the loop.
	TimedOut bool
}

// TrailHeaders renders the failover diagnostics headers as data. X-Fallback-
// Attempts and X-Fallback-Trail are always present when hops failed;
// X-Fallback-Detail is added only when the caller opts in (it reads the
// operator setting). Values are sanitised so a hostile model id can neither
// inject header lines nor make the writer reject the response.
func (r *Result) TrailHeaders(includeDetail bool) map[string]string {
	h := map[string]string{}
	if r.FailedAttempts > 0 {
		h["X-Fallback-Attempts"] = strconv.Itoa(r.FailedAttempts)
	}
	if len(r.Attempts) > 0 {
		h["X-Fallback-Trail"] = safeHeaderValue(FormatTrail(r.Attempts), trailHeaderMaxLength)
		if includeDetail {
			h["X-Fallback-Detail"] = safeHeaderValue(FormatDetail(r.Attempts), detailHeaderMaxLength)
		}
	}
	return h
}

// ── Injected dependencies (narrow interfaces this slice declares) ────────────

// Chain selects a route per attempt and answers the model/key questions the
// failure bookkeeping needs. Satisfied by Slice B's router in the integration
// pass; the skip state it reads is mutated by the loop between attempts.
// customPlatform names the per-key endpoint family. Its keys each carry their
// own base URL, so platform-wide verdicts do not apply to it.
const customPlatform = "custom"

type Chain interface {
	// Route selects the next (platform, model, key) honouring skip, or returns
	// a *RouteError when the pool is exhausted before any upstream is tried.
	Route(attempt int, skip *SkipState) (Route, error)

	// RoutableKeys lists every key that can route to a model, ignoring the
	// transient gates HasOtherUsableKey applies - the full set the model-level
	// bench must span.
	RoutableKeys(modelDBID int64) []int64

	// HasOtherUsableKey reports whether a sibling key can still serve the model
	// right now, excluding the just-failed key and every key in skipKeys. Gates
	// the model penalty: a usable sibling means the model is not at fault.
	HasOtherUsableKey(modelDBID, excludingKeyID int64, skipKeys map[RouteKey]struct{}) bool
}

// Cooldowns is the benching surface, keyed by the (platform, model, key) triple
// so this slice never touches the engine's string-key format. Satisfied by a
// thin adapter over *CooldownEngine (which is QuotaKey-string-keyed) in the
// integration pass.
type Cooldowns interface {
	// Decide prices a transient/rate-limit failure and records the bench,
	// owning the escalation ladder and the null-limit heuristic.
	Decide(platform, model string, keyID int64, req CooldownRequest) Cooldown
	// Bench records a fixed-duration bench with explicit provenance, for the
	// states that are not rate limits (auth, credit, tier, daily reset).
	Bench(platform, model string, keyID int64, d time.Duration, source CooldownSource) Cooldown
	// Active returns the live bench for a triple, if any.
	Active(platform, model string, keyID int64) (Cooldown, bool)
	// SoonestExpiry is the earliest moment any active bench lifts, for a 429
	// exhaustion's Retry-After; zero when nothing is benched.
	SoonestExpiry() time.Time
	// Succeeded is the served-request reset for one route: it returns the
	// escalation ladder to the bottom and lifts our own heuristic bench, so a
	// recovered endpoint is not punished for history. sinceKnownAt is when the
	// successful attempt began; an authoritative (provider-stated) bench
	// recorded AFTER that instant is newer information a concurrent failure
	// just learned, so it survives - a success that predates it must not
	// withdraw it.
	Succeeded(platform, model string, keyID int64, sinceKnownAt time.Time)
}

// Scorer carries the model-level reliability penalty the bandit reads. The
// magnitudes (+3/+1), the lazy 2-minute decay, and the priority-position
// ordering all live behind these three calls in Slice B; the loop only decides
// which to call.
type Scorer interface {
	// RecordRateLimitHit is the heavy demotion (PENALTY_PER_429 = 3) for any
	// rate-limit signal.
	RecordRateLimitHit(modelDBID int64)
	// RecordModelFailure is the light demotion (PENALTY_PER_FAIL = 1) for an
	// ordinary upstream failure.
	RecordModelFailure(modelDBID int64)
	// RecordSuccess clears the model's penalty after a served request.
	RecordSuccess(modelDBID int64)
}

// Dispatcher runs one attempt against the chosen route and reports how it
// finished. It never throws for control flow: a failed HTTP exchange is a
// DispatchResult with Status/Body set, a transport/timeout/dead-turn failure
// sets Err.
type Dispatcher interface {
	Dispatch(ctx context.Context, route Route, attempt int) DispatchResult
}

// LimitLearner tightens a model's stored rate-limit ceiling from an error body
// (e.g. a Groq 413 "TPM: Limit 30000"). Optional; nil disables limit-learning.
type LimitLearner interface {
	LearnLimit(modelDBID int64, err error)
}

// Revalidator kicks an immediate key health re-check after a 401 so a
// confirmed-bad key drops out of routing in seconds. Optional.
type Revalidator interface {
	Revalidate(platform string, keyID int64)
}

// FailoverDeps are the process-lived dependencies shared across requests.
type FailoverDeps struct {
	Cooldowns   Cooldowns
	Scorer      Scorer
	Learner     LimitLearner // optional
	Revalidator Revalidator  // optional
}

// FailoverConfig tunes the loop. MaxRetries <= 0 uses fallbackMaxRetries.
// TimeBudget < 0 uses defaultFallbackTimeBudget, == 0 disables the budget, > 0
// sets it. BreakerLimit <= 0 disables the circuit breaker. CooldownCeiling caps
// our own guessed benches (never a provider-stated wait); 0 = unlimited.
type FailoverConfig struct {
	MaxRetries      int
	TimeBudget      time.Duration
	BreakerLimit    int
	CooldownCeiling time.Duration
}

// DispatchRequest is the per-request wiring: the chain and dispatcher bound to
// this request's body, plus an optional client-disconnect probe. It pairs with
// DispatchResult - the request the loop drives, the result each attempt yields.
type DispatchRequest struct {
	Chain      Chain
	Dispatcher Dispatcher
	// ClientGone reports whether the client has hung up; checked before each
	// retry so a chain nobody is waiting for stops burning quota. Optional.
	ClientGone func() bool
}

// Failover is the process-lived driver. It holds the config, the shared
// dependencies, and the in-memory failure state the reference keeps at module
// scope (model-failure windows, empty-completion streaks, revalidation dedupe,
// null-limit hit counters), all guarded by mu since requests run concurrently.
type Failover struct {
	deps FailoverDeps
	cfg  FailoverConfig
	now  func() time.Time

	mu            sync.Mutex
	modelFailures map[int64][]time.Time // model_db_id -> failure timestamps
	emptyStreaks  map[RouteKey]int      // consecutive empty completions
	revalidated   map[int64]time.Time   // key_id -> last revalidation
	nullLimitHits map[RouteKey][]time.Time

	// cooldownCeiling is the operator's live maximum for our own guessed
	// benches, initialised from cfg and overridable at runtime via
	// SetCooldownCeiling so a routing-config change takes effect without
	// rebuilding the driver. 0 = unlimited; it never shortens a provider-
	// stated wait.
	cooldownCeiling time.Duration
}

// NewFailover builds a driver. The clock is wall-clock; tests set f.now.
func NewFailover(deps FailoverDeps, cfg FailoverConfig) *Failover {
	return &Failover{
		deps:            deps,
		cfg:             cfg,
		now:             time.Now,
		modelFailures:   map[int64][]time.Time{},
		emptyStreaks:    map[RouteKey]int{},
		revalidated:     map[int64]time.Time{},
		nullLimitHits:   map[RouteKey][]time.Time{},
		cooldownCeiling: cfg.CooldownCeiling,
	}
}

// SetCooldownCeiling updates the operator's live maximum for our own guessed
// benches, so a routing-config change takes effect on the very next failure
// without rebuilding the driver. 0 = unlimited; it never shortens a provider-
// stated wait. The routing-settings layer pushes the configured
// routing_cooldown_ceiling_ms here through the Engine passthrough.
func (f *Failover) SetCooldownCeiling(d time.Duration) {
	f.mu.Lock()
	f.cooldownCeiling = d
	f.mu.Unlock()
}

// currentCooldownCeiling reads the live ceiling under the lock, so a pricing
// decision always sees the operator's latest value.
func (f *Failover) currentCooldownCeiling() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cooldownCeiling
}

// ── The attempt loop ─────────────────────────────────────────────────────────

// Run walks the routing chain until an attempt succeeds, a fatal error stops
// it, the pool is exhausted, or a guardrail (budget/breaker/client-gone) trips.
// It creates a fresh request-scoped skip state; the process-lived failure state
// on f persists across requests.
func (f *Failover) Run(ctx context.Context, req DispatchRequest) *Result {
	maxRetries := f.cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = fallbackMaxRetries
	}
	budget := f.cfg.TimeBudget
	if budget < 0 {
		budget = defaultFallbackTimeBudget
	}

	startedAt := f.now()
	skip := NewSkipState()
	var attempts []Attempt
	var lastError error // nil interface or a non-nil *UpstreamError
	breaker := 0
	// longestAttempt excuses the single most expensive attempt from the time
	// budget, so real provider work does not read as thrashing.
	var longestAttempt time.Duration

	// Per-request key ordinals: the trail shows key1, key2… keyed by
	// (platform, key) so two models on the same key share an ordinal, exactly
	// as the reference does (fallback-loop.ts:1043-1051).
	keyOrdinals := map[struct {
		platform string
		keyID    int64
	}]int{}
	keyOrdinal := func(r Route) int {
		k := struct {
			platform string
			keyID    int64
		}{r.Platform, r.KeyID}
		if ord, ok := keyOrdinals[k]; ok {
			return ord
		}
		ord := len(keyOrdinals) + 1
		keyOrdinals[k] = ord
		return ord
	}

	clientGone := func() bool { return req.ClientGone != nil && req.ClientGone() }

	for attempt := range maxRetries {
		// Client disconnect: nobody is waiting, so stop without rendering.
		if attempt > 0 && clientGone() {
			return &Result{Status: StatusClientGone, Attempts: attempts, FailedAttempts: len(attempts)}
		}

		// Time budget: refuse to START attempts >= 2 once the churn budget is
		// spent. Attempt 0 always runs, and so does the first retry - when
		// attempt 0 alone consumed the budget, refusing attempt 1 would make
		// failover structurally impossible for exactly the slow-failing models
		// that need it (#751). A slow attempt is never aborted mid-flight
		// here; it just becomes the last one.
		if attempt > 1 && budget > 0 && f.now().Sub(startedAt)-longestAttempt >= budget {
			return f.exhaustedResult(lastError, attempts, exhaustionCtx{timedOut: true, budget: budget}, maxRetries, true)
		}

		route, rerr := req.Chain.Route(attempt, skip)
		if rerr != nil {
			var ex *Exhaustion
			if lastError != nil {
				ex = f.exhaustedRetryError(lastError, attempts, exhaustionCtx{}, maxRetries)
			} else {
				ex = f.routingExhaustionBody(rerr)
			}
			err := lastError
			if err == nil {
				err = rerr
			}
			return &Result{
				Status:         StatusRoutingExhausted,
				Attempts:       attempts,
				FailedAttempts: len(attempts),
				Exhaustion:     ex,
				Err:            err,
			}
		}

		attemptStart := f.now()
		res := f.dispatch(ctx, req.Dispatcher, route, attempt)
		if spent := f.now().Sub(attemptStart); spent > longestAttempt {
			longestAttempt = spent
		}

		if res.succeeded() {
			// A committed stream that broke mid-flight (OutcomeCommitted with a
			// carried error) flushed bytes we cannot retract, so there is no
			// failover left - but the endpoint did NOT serve a good response.
			// Running recordSuccess here would reset its cooldown ladder, lift
			// its bench and clear its model penalty, reporting a broken endpoint
			// as healthy and wiping a concurrent authoritative bench. That is
			// the exact accounting that made a broken stream invisible, so it is
			// booked as the failure it is instead.
			if res.Outcome == OutcomeCommitted && res.Err != nil {
				return &Result{
					Status:         StatusCommittedError,
					Outcome:        res.Outcome,
					Route:          new(route),
					Attempts:       attempts,
					FailedAttempts: len(attempts),
					Err:            normalizeError(res.Status, res.Body, res.Err),
				}
			}
			f.recordSuccess(route, attemptStart)
			return &Result{
				Status:         StatusSucceeded,
				Outcome:        res.Outcome,
				Route:          new(route),
				Attempts:       attempts,
				FailedAttempts: len(attempts),
			}
		}

		ue := normalizeError(res.Status, res.Body, res.Err)

		// The two gateway-owned aborts are not provider health: no bookkeeping.
		if IsClientAbortError(ue) {
			return &Result{Status: StatusClientGone, Attempts: attempts, FailedAttempts: len(attempts)}
		}
		if IsHedgeAbortError(ue) {
			return f.exhaustedResult(lastError, attempts, exhaustionCtx{timedOut: true, budget: budget}, maxRetries, true)
		}

		cls := classifyNormalized(ue)
		ord := keyOrdinal(route)

		if cls.KeyAuth {
			// 401: KEY-fatal, not request-fatal. Rotate past the bad key and
			// revalidate it; no penalty, no limit-learning.
			f.recordAuthFailure(route, skip)
			attempts = append(attempts, f.newAttempt(route, ord, AttemptAuth, attemptStart, startedAt, ue))
			lastError = ue
			if f.recordBreaker(&breaker) {
				return f.breakerResult(lastError, attempts, breaker, maxRetries, clientGone)
			}
			continue
		}

		if cls.Retryable {
			exempt := f.recordRetryableFailure(route, ue, cls, res.RetryAfter, req.Chain, skip)
			attempts = append(attempts, f.newAttempt(route, ord, cls.Attempt, attemptStart, startedAt, ue))
			lastError = ue
			// A still-exempt skipBench failure (format ignored, hidden-reasoning
			// truncation under the streak limit) is model behaviour, not provider
			// health - it must not count toward the "pool looks unhealthy"
			// breaker either.
			if !exempt && f.recordBreaker(&breaker) {
				return f.breakerResult(lastError, attempts, breaker, maxRetries, clientGone)
			}
			continue
		}

		// Fatal: render the provider's own error (no trail entry for this hop).
		return &Result{
			Status:         StatusFatal,
			Route:          new(route),
			Attempts:       attempts,
			FailedAttempts: len(attempts),
			Err:            ue,
		}
	}

	return f.exhaustedResult(lastError, attempts, exhaustionCtx{}, maxRetries, false)
}

// dispatch runs one attempt and frees its in-flight lease on every exit, so no
// path can leak the key's concurrency budget.
func (f *Failover) dispatch(ctx context.Context, d Dispatcher, route Route, attempt int) DispatchResult {
	if route.Release != nil {
		defer route.Release()
	}
	return d.Dispatch(ctx, route, attempt)
}

func (f *Failover) exhaustedResult(lastError error, attempts []Attempt, ctx exhaustionCtx, maxRetries int, timedOut bool) *Result {
	return &Result{
		Status:         StatusExhausted,
		Attempts:       attempts,
		FailedAttempts: len(attempts),
		Exhaustion:     f.exhaustedRetryError(lastError, attempts, ctx, maxRetries),
		Err:            lastError,
		TimedOut:       timedOut,
	}
}

func (f *Failover) breakerResult(lastError error, attempts []Attempt, breaker, maxRetries int, clientGone func() bool) *Result {
	// Tripped after the client already hung up: stop, but there is no socket to
	// render an exhaustion body to.
	if clientGone() {
		return &Result{Status: StatusClientGone, Attempts: attempts, FailedAttempts: len(attempts)}
	}
	return &Result{
		Status:         StatusExhausted,
		Attempts:       attempts,
		FailedAttempts: len(attempts),
		Exhaustion:     f.exhaustedRetryError(lastError, attempts, exhaustionCtx{breakerFails: breaker}, maxRetries),
		Err:            lastError,
	}
}

// recordBreaker records one failed attempt against the request's breaker and
// reports whether it tripped. No-op returning false when the breaker is
// disabled (guardrails.ts newBreaker/recordBreakerFailure; BreakerLimit <= 0).
// Within one request every recorded failure is consecutive by construction (a
// success ends the loop), so this is "max upstream failures per request".
func (f *Failover) recordBreaker(consecutive *int) bool {
	if f.cfg.BreakerLimit <= 0 {
		return false
	}
	*consecutive++
	return *consecutive >= f.cfg.BreakerLimit
}

func (f *Failover) newAttempt(route Route, ord int, class AttemptClass, attemptStart, startedAt time.Time, ue *UpstreamError) Attempt {
	return Attempt{
		Platform:    route.Platform,
		ModelID:     route.ModelID,
		KeyOrdinal:  ord,
		Class:       class,
		StartOffset: attemptStart.Sub(startedAt),
		Duration:    f.now().Sub(attemptStart),
		Summary:     summarizeError(ue),
	}
}

// ── Per-failure bookkeeping ──────────────────────────────────────────────────

// recordRetryableFailure applies the full per-key failure bookkeeping after a
// retryable failure and reports whether the skipBench exemption held, so the
// caller keeps the breaker in lockstep with the bench decision
// (fallback-loop.ts:304-351).
func (f *Failover) recordRetryableFailure(route Route, ue *UpstreamError, cls ErrorClass, retryAfter time.Duration, chain Chain, skip *SkipState) bool {
	// Rule out the whole model on a model-level failure; a sibling key would
	// fail it identically (404/403/context/skipModelForRequest).
	if cls.SkipModel {
		skip.Models[route.ModelDBID] = struct{}{}
	}
	// The just-failed key is always skipped; adding it here preserves the
	// "count budget across keys" semantics for the penalty gate below.
	skip.Keys[routeKeyOf(route)] = struct{}{}
	// A provider-level failure (5xx/timeout/transport/degraded) rules out the
	// whole platform for the request, because every key of a hosted provider
	// talks to the same endpoint and would fail identically.
	//
	// Custom endpoints are the exception and must not be swept: each key there
	// is a DIFFERENT operator-supplied server, so one of them being down says
	// nothing about the others. Sweeping them made a self-hosted pool fail
	// over exactly once and then give up.
	if cls.SkipPlatform && route.Platform != customPlatform {
		skip.Platforms[route.Platform] = struct{}{}
	}

	// A candidate with no wire adapter earned no upstream verdict: the platform
	// skip above is the ONLY bookkeeping it warrants. Nothing was asked of the
	// provider or the model, so no cooldown, no model-failure window, no
	// penalty, and no limit-learning - recording health signal would slander a
	// healthy endpoint for our own configuration gap.
	if ue.Undispatchable {
		return false
	}

	// Reasoning-truncation / dead-turn exemption: still fail over, but record no
	// health signal while the streak holds.
	if f.consumeSkipBenchExemption(route, ue) {
		return true
	}

	f.applyCooldown(route, cls, retryAfter)

	// Model-level failure benching: a model failing across keys sinks out of
	// routing until upstream heals.
	f.noteModelFailure(route, f.now(), chain)

	// Model-level penalty ONLY when no sibling key can still serve the model
	// (#454): a usable sibling means the model is not at fault. skip.Keys
	// already holds the just-failed key, so it is excluded too.
	if !chain.HasOtherUsableKey(route.ModelDBID, route.KeyID, skip.Keys) {
		switch cls.Penalty {
		case penaltyHeavy:
			f.deps.Scorer.RecordRateLimitHit(route.ModelDBID)
		case penaltyLight:
			f.deps.Scorer.RecordModelFailure(route.ModelDBID)
		}
	}

	if f.deps.Learner != nil {
		f.deps.Learner.LearnLimit(route.ModelDBID, ue)
	}
	return false
}

// recordAuthFailure benches the model+key for the health-cycle window and kicks
// an immediate revalidation. No model penalty and no limit-learning - a bad key
// says nothing about the model (fallback-loop.ts:385-389).
func (f *Failover) recordAuthFailure(route Route, skip *SkipState) {
	skip.Keys[routeKeyOf(route)] = struct{}{}
	f.deps.Cooldowns.Bench(route.Platform, route.ModelID, route.KeyID, authFailureCooldown, SourceHeuristic)
	f.triggerKeyRevalidation(route.Platform, route.KeyID)
}

// recordSuccess is the success-side accounting the driver owns: reset this
// route's cooldown escalation and lift its heuristic bench, then clear the
// model's penalty and the two per-key streaks a served request disproves.
// Quota counters, key health, and token metering are recorded by the
// dispatcher / integration, which know the token counts.
func (f *Failover) recordSuccess(route Route, sinceKnownAt time.Time) {
	f.deps.Scorer.RecordSuccess(route.ModelDBID)
	// The served request means this endpoint recovered, so the next failure
	// starts at the bottom of the ladder and our own guessed bench lifts.
	// sinceKnownAt (the attempt's start) keeps a newer concurrent authoritative
	// bench - a fresh provider-stated wait a parallel failure just learned -
	// from being wiped by a success that predates it.
	f.deps.Cooldowns.Succeeded(route.Platform, route.ModelID, route.KeyID, sinceKnownAt)
	f.mu.Lock()
	delete(f.emptyStreaks, routeKeyOf(route))
	delete(f.modelFailures, route.ModelDBID)
	f.mu.Unlock()
}

// applyCooldown records the bench for a retryable failure, choosing the pricing
// path from the classification (cooldownDecisionForError, fallback-loop.ts:
// 205-222). Payment/tier honour the operator ceiling (#952); a message-detected
// daily exhaustion benches authoritatively to the provider's own reset;
// everything else defers to the escalation ladder.
func (f *Failover) applyCooldown(route Route, cls ErrorClass, retryAfter time.Duration) {
	ceiling := f.currentCooldownCeiling()
	switch cls.Cooldown {
	case cooldownPayment:
		f.deps.Cooldowns.Bench(route.Platform, route.ModelID, route.KeyID,
			capByCeiling(paymentRequiredCooldown, ceiling), SourceCredit)
	case cooldownForbidden:
		f.deps.Cooldowns.Bench(route.Platform, route.ModelID, route.KeyID,
			capByCeiling(modelForbiddenCooldown, ceiling), SourceTier)
	case cooldownDaily:
		// The provider's Retry-After wins over the UTC-midnight convention:
		// rolling daily windows reset before midnight and the provider knows
		// best. Either way the expiry is a fact, so the bench is authoritative.
		d := retryAfter
		if d <= 0 {
			d = f.untilNextUTCMidnight()
		}
		f.deps.Cooldowns.Bench(route.Platform, route.ModelID, route.KeyID, d, SourceAuthoritative)
	default: // cooldownTransient
		f.deps.Cooldowns.Decide(route.Platform, route.ModelID, route.KeyID, CooldownRequest{
			RetryAfter:             retryAfter,
			QuotaSignal:            cls.QuotaSignal,
			RecentHits:             f.recordNullLimitHit(route, cls.QuotaSignal),
			HasPublishedDailyLimit: route.RPDLimit != nil || route.TPDLimit != nil,
			BaseURL:                route.BaseURL,
			Ceiling:                ceiling,
		})
	}
}

// consumeSkipBenchExemption advances (or breaks) the empty-completion streak and
// reports whether the exemption still holds (fallback-loop.ts:261-273). The
// first two consecutive empty completions on a (model, key) are exempt; the
// third takes the normal cooldown/penalty/limit-learning path.
func (f *Failover) consumeSkipBenchExemption(route Route, ue *UpstreamError) bool {
	key := routeKeyOf(route)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ue.SkipBench {
		// A normally-penalised failure breaks the streak: the cooldown ladder is
		// already handling whatever is wrong with this model+key.
		delete(f.emptyStreaks, key)
		return false
	}
	if ue.SkipModelForRequest {
		// Format violation: model behaviour for this request, never a streak.
		return true
	}
	streak := f.emptyStreaks[key] + 1
	f.emptyStreaks[key] = streak
	return streak < emptyCompletionStreakLimit
}

// noteModelFailure records one retryable failure for a model and, once the
// sliding window holds modelFailureThreshold of them, benches the model on
// every routable key so it sinks out of routing until upstream heals
// (fallback-loop.ts:85-107). The bench spans keys because the window counts
// across keys; it never shortens a longer existing bench.
func (f *Failover) noteModelFailure(route Route, now time.Time, chain Chain) {
	f.mu.Lock()
	window := f.modelFailures[route.ModelDBID]
	kept := window[:0]
	for _, t := range window {
		if now.Sub(t) < modelFailureWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	f.modelFailures[route.ModelDBID] = kept
	tripped := len(kept) >= modelFailureThreshold
	if tripped {
		// One bench per streak: drop the window so it must re-accumulate.
		delete(f.modelFailures, route.ModelDBID)
	}
	f.mu.Unlock()
	if !tripped {
		return
	}

	// Bench outside the lock: chain and cooldown calls are I/O-ish.
	keyIDs := chain.RoutableKeys(route.ModelDBID)
	if len(keyIDs) == 0 {
		// A narrower bench beats none when the key set can't be read.
		keyIDs = []int64{route.KeyID}
	}
	benchUntil := now.Add(modelFailureCooldown)
	for _, keyID := range keyIDs {
		if cd, ok := f.deps.Cooldowns.Active(route.Platform, route.ModelID, keyID); ok && !cd.Until.Before(benchUntil) {
			// Never SHORTEN an existing bench: a provider-stated reset or an
			// escalated ladder step outlasting this window knows more, and
			// overwriting would drop its non-probeable provenance.
			continue
		}
		f.deps.Cooldowns.Bench(route.Platform, route.ModelID, keyID, modelFailureCooldown, SourceHeuristic)
	}
}

// recordNullLimitHit counts this key's recent quota signals inside
// nullLimitHitWindow, so the cooldown engine's null-limit heuristic can treat a
// no-published-limit endpoint as exhausted after enough real 429s. Returns 0
// for a non-quota failure (a timeout or 5xx must never feed the ladder, #592).
func (f *Failover) recordNullLimitHit(route Route, quotaSignal bool) int {
	if !quotaSignal {
		return 0
	}
	key := routeKeyOf(route)
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	hits := f.nullLimitHits[key]
	kept := hits[:0]
	for _, t := range hits {
		if now.Sub(t) < nullLimitHitWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	f.nullLimitHits[key] = kept
	return len(kept)
}

// triggerKeyRevalidation kicks an immediate health re-check, deduped so
// concurrent 401s on one key do not stampede the provider (fallback-loop.ts:
// 368-377). No-op when no Revalidator is wired.
func (f *Failover) triggerKeyRevalidation(platform string, keyID int64) {
	if f.deps.Revalidator == nil {
		return
	}
	now := f.now()
	f.mu.Lock()
	last := f.revalidated[keyID]
	if !last.IsZero() && now.Sub(last) < revalidationDedupe {
		f.mu.Unlock()
		return
	}
	f.revalidated[keyID] = now
	f.mu.Unlock()
	f.deps.Revalidator.Revalidate(platform, keyID)
}

// untilNextUTCMidnight is the time until the next UTC midnight - when most
// providers' daily free allocations reset - floored at one minute so a hit
// seconds before midnight still records a real bench (fallback-loop.ts:168-172).
func (f *Failover) untilNextUTCMidnight() time.Duration {
	now := f.now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	if d := next.Sub(now); d > time.Minute {
		return d
	}
	return time.Minute
}

func capByCeiling(d, ceiling time.Duration) time.Duration {
	if ceiling > 0 && ceiling < d {
		return ceiling
	}
	return d
}

// ── Exhaustion taxonomy ──────────────────────────────────────────────────────

type exhaustionCtx struct {
	timedOut     bool
	budget       time.Duration
	breakerFails int
}

// unavailableUntilKnownTime are the attempt classes meaning "this candidate is
// unavailable until a KNOWN time" (fallback-loop.ts:653-658). When EVERY
// attempt is in this family the pool recovers by itself, so the honest
// exhaustion is a 429 with a concrete retry time; anything else in the mix
// means waiting is not a promise, so the case renders a 502.
var unavailableUntilKnownTime = map[AttemptClass]struct{}{
	AttemptRateLimited:         {},
	AttemptDailyQuotaExhausted: {},
	AttemptOutOfCredits:        {},
	AttemptForbidden:           {},
}

// exhaustedRetryError renders the terminal status ladder every surface shares,
// aggregated over the per-attempt classes most-specific first (fallback-loop.ts:
// 687-810). Never 500.
func (f *Failover) exhaustedRetryError(lastError error, attempts []Attempt, ctx exhaustionCtx, maxRetries int) *Exhaustion {
	everyAttempt := func(match func(AttemptClass) bool) bool {
		if len(attempts) == 0 {
			return false
		}
		for _, a := range attempts {
			if !match(a.Class) {
				return false
			}
		}
		return true
	}
	isClass := func(c AttemptClass) func(AttemptClass) bool {
		return func(x AttemptClass) bool { return x == c }
	}
	budgetNote := ""
	if ctx.timedOut {
		budgetNote = fmt.Sprintf(" (stopped early: retry time budget %ds exceeded)", int(ctx.budget/time.Second))
	}
	trail := trailNote(attempts)
	// The same unwrap the fatal path uses: lastError carries the provider's raw
	// body, and embedding that JSON in a sentence makes the reason unreadable
	// exactly when an operator is trying to read it.
	last := sanitizeSummary(ProviderMessage(AsUpstreamError(lastError)))
	if last == "" {
		last = sanitizeSummary(errMsg(lastError))
	}

	switch {
	case everyAttempt(isClass(AttemptAuth)):
		return &Exhaustion{
			Kind: ExhaustAuth, Status: 502, Type: "provider_error", Code: "provider_authentication_failed",
			Message: fmt.Sprintf("All %d attempted provider key(s) failed authentication%s. The configured upstream API key(s) look invalid or expired; check Credentials in `prowl`.%s Last error: %s",
				len(attempts), budgetNote, trail, last),
		}
	case everyAttempt(isClass(AttemptContextTooLarge)):
		return &Exhaustion{
			Kind: ExhaustContextTooLarge, Status: 413, Type: "invalid_request_error", Code: "context_length_exceeded",
			Message: fmt.Sprintf("The request is too large for every routed candidate: all %d attempt(s) were rejected as over the model's context/size limit%s. Reduce the prompt/history size or enable a larger-context model.%s Last error: %s",
				len(attempts), budgetNote, trail, last),
		}
	case everyAttempt(isClass(AttemptModelNotFound)):
		return &Exhaustion{
			Kind: ExhaustModelNotFound, Status: 404, Type: "invalid_request_error", Code: "model_not_found",
			Message: fmt.Sprintf("Every routed provider reports the model as not found or removed upstream (%d attempt(s))%s. Pick another model or call /v1/models for the available list.%s Last error: %s",
				len(attempts), budgetNote, trail, last),
		}
	case IsProviderDegradedError(lastError):
		return &Exhaustion{
			Kind: ExhaustUnavailable, Status: 503, Type: "service_unavailable", Code: "provider_degraded",
			Message: fmt.Sprintf("The routed provider reported the model's hosted deployment as temporarily degraded%s. Retry later or route to another provider.%s Last error: %s",
				budgetNote, trail, last),
		}
	case IsProviderBadRequestError(lastError):
		return &Exhaustion{
			Kind: ExhaustBadRequest, Status: 400, Type: "invalid_request_error", Code: "provider_rejected_request",
			Message: fmt.Sprintf("All routed providers rejected the request as invalid%s.%s Last error: %s", budgetNote, trail, last),
		}
	case ctx.breakerFails > 0:
		etaNote := f.etaNote()
		plural := "s"
		if ctx.breakerFails == 1 {
			plural = ""
		}
		return &Exhaustion{
			Kind: ExhaustUnavailable, Status: 503, Type: "service_unavailable", Code: "upstream_unhealthy",
			Message: fmt.Sprintf("Failover stopped early by the circuit-breaker guardrail: %d consecutive upstream failure%s (max_consecutive_upstream_fails). The enabled pool looks unhealthy right now.%s%s Last error: %s",
				ctx.breakerFails, plural, etaNote, trail, last),
		}
	case len(attempts) == 0 || everyAttempt(func(c AttemptClass) bool { _, ok := unavailableUntilKnownTime[c]; return ok }):
		retryAt := f.deps.Cooldowns.SoonestExpiry()
		scope := "All models rate-limited"
		count := len(attempts)
		if count == 0 {
			count = maxRetries
		}
		if len(attempts) > 0 {
			plural := "s"
			if count == 1 {
				plural = ""
			}
			scope = fmt.Sprintf("All models rate-limited after %d attempt%s", count, plural)
		}
		ex := &Exhaustion{
			Kind: ExhaustRateLimit, Status: 429, Type: "rate_limit_error", Code: "rate_limit_exceeded",
			Message: fmt.Sprintf("%s%s.%s%s Last error: %s", scope, budgetNote, f.etaNote(), trail, last),
		}
		if !retryAt.IsZero() {
			ex.RetryAt = retryAt
		}
		return ex
	default:
		return &Exhaustion{
			Kind: ExhaustUpstream, Status: 502, Type: "provider_error", Code: "upstream_failed",
			Message: fmt.Sprintf("All %d routed attempt(s) failed with upstream provider errors%s. This is a provider-side failure, not a problem with your request; retry, or check provider status.%s Last error: %s",
				len(attempts), budgetNote, trail, last),
		}
	}
}

// routingDiagClass buckets a router diagnostic line (fallback-loop.ts:826-836).
type routingDiagClass int

const (
	diagConfig routingDiagClass = iota
	diagTooLarge
	diagTimeBound
	diagOther
)

var diagConfigRe = regexp.MustCompile(`no provider registered|no enabled\+healthy key|no usable key|decrypt-error|no-resolved-provider|custom-key-mismatch`)
var diagTimeBoundRe = regexp.MustCompile(`cooldown|rpm|rpd|tpm|tpd|provider-daily-cap|provider-minute-cap|provider-daily-token-cap|key-concurrency`)

func classifyRoutingDiagLine(line string) routingDiagClass {
	l := strings.ToLower(line)
	// "< estimated" first: the tpm_limit-too-small line also contains 'tpm',
	// which would otherwise misread as a transient window.
	if strings.Contains(l, "< estimated") {
		return diagTooLarge
	}
	if diagConfigRe.MatchString(l) {
		return diagConfig
	}
	if diagTimeBoundRe.MatchString(l) {
		return diagTimeBound
	}
	return diagOther
}

// routingExhaustionBody renders the terminal status when routing gave up before
// any upstream was tried, mapping the router's per-candidate diagnostics onto
// the same honest-status taxonomy (fallback-loop.ts:838-888).
func (f *Failover) routingExhaustionBody(routeErr error) *Exhaustion {
	var diag []string
	message := "No model available to route this request"
	if re, ok := routeErr.(*RouteError); ok && re != nil {
		diag = re.Diagnostics
		if re.Message != "" {
			message = re.Message
		}
	} else if routeErr != nil && routeErr.Error() != "" {
		message = routeErr.Error()
	}

	classes := make([]routingDiagClass, len(diag))
	for i, l := range diag {
		classes[i] = classifyRoutingDiagLine(l)
	}

	allConfig := true
	for _, c := range classes {
		if c != diagConfig {
			allConfig = false
			break
		}
	}
	if len(diag) == 0 || allConfig {
		msg := fmt.Sprintf("No candidate model has a configured, usable provider key. Add provider API keys in the Credentials tab of `prowl`. %s", message)
		if len(diag) == 0 {
			msg = fmt.Sprintf("No models are enabled/configured to serve this request. Add credentials and enable models in `prowl`. %s", message)
		}
		return &Exhaustion{Kind: ExhaustUnavailable, Status: 503, Type: "service_unavailable", Code: "no_providers_configured", Message: msg}
	}

	allTooLargeOrConfig := true
	hasTooLarge := false
	for _, c := range classes {
		switch c {
		case diagTooLarge:
			hasTooLarge = true
		case diagConfig:
		default:
			allTooLargeOrConfig = false
		}
	}
	if allTooLargeOrConfig && hasTooLarge {
		return &Exhaustion{
			Kind: ExhaustContextTooLarge, Status: 413, Type: "invalid_request_error", Code: "context_length_exceeded",
			Message: fmt.Sprintf("The request is too large for every available candidate's context/token window. Reduce the prompt/history size or enable a larger-context model. %s", message),
		}
	}

	hasTimeBound := false
	hasOther := false
	for _, c := range classes {
		if c == diagTimeBound {
			hasTimeBound = true
		}
		if c == diagOther {
			hasOther = true
		}
	}
	if hasTimeBound && !hasOther {
		ex := &Exhaustion{Kind: ExhaustRateLimit, Status: 429, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: message}
		if retryAt := f.deps.Cooldowns.SoonestExpiry(); !retryAt.IsZero() {
			ex.RetryAt = retryAt
		}
		return ex
	}

	// Capability filters or mixed reasons: keep the router's verdict (429) but
	// stamp a code so clients can tell it from a plain rate-limit exhaustion.
	status := 429
	if re, ok := routeErr.(*RouteError); ok && re != nil && re.Status != 0 {
		status = re.Status
	}
	return &Exhaustion{Kind: ExhaustRateLimit, Status: status, Type: "rate_limit_error", Code: "routing_exhausted", Message: message}
}

func (f *Failover) etaNote() string {
	eta := formatResetEta(f.deps.Cooldowns.SoonestExpiry(), f.now())
	if eta == "" {
		return ""
	}
	return fmt.Sprintf(" Soonest cooldown reset %s.", eta)
}

// ── Trail / header formatting ────────────────────────────────────────────────

// FormatTrail renders X-Fallback-Trail: up to trailMaxShown hops as
// "platform/model keyN=class", with a "+K more" tail (fallback-loop.ts:557-563).
func FormatTrail(attempts []Attempt) string {
	shown, extra := capAttempts(attempts)
	parts := make([]string, len(shown))
	for i, a := range shown {
		parts[i] = fmt.Sprintf("%s/%s key%d=%s", a.Platform, a.ModelID, a.KeyOrdinal, a.Class)
	}
	return joinWithExtra(parts, extra)
}

// FormatDetail renders X-Fallback-Detail: the same hops plus per-hop timings and
// the redacted provider summary (fallback-loop.ts:519-533).
func FormatDetail(attempts []Attempt) string {
	shown, extra := capAttempts(attempts)
	parts := make([]string, len(shown))
	for i, a := range shown {
		seg := fmt.Sprintf("%s/%s key%d=%s t=%d+%dms", a.Platform, a.ModelID, a.KeyOrdinal, a.Class,
			a.StartOffset.Milliseconds(), a.Duration.Milliseconds())
		if a.Summary != "" {
			s := a.Summary
			if len(s) > detailSummaryMaxLength {
				s = s[:detailSummaryMaxLength]
			}
			// "; " is the record separator, so a summary's semicolons become
			// commas to keep the segments unambiguous.
			seg += " msg=" + strings.ReplaceAll(s, ";", ",")
		}
		parts[i] = seg
	}
	return joinWithExtra(parts, extra)
}

// formatTrailMessage is the in-body trail ("keyN: class") the exhaustion message
// embeds (fallback-loop.ts:535-541).
func formatTrailMessage(attempts []Attempt) string {
	shown, extra := capAttempts(attempts)
	parts := make([]string, len(shown))
	for i, a := range shown {
		parts[i] = fmt.Sprintf("%s/%s key%d: %s", a.Platform, a.ModelID, a.KeyOrdinal, a.Class)
	}
	return joinWithExtra(parts, extra)
}

func trailNote(attempts []Attempt) string {
	if len(attempts) == 0 {
		return ""
	}
	return fmt.Sprintf(" Attempt trail: %s.", formatTrailMessage(attempts))
}

func capAttempts(attempts []Attempt) ([]Attempt, int) {
	if len(attempts) > trailMaxShown {
		return attempts[:trailMaxShown], len(attempts) - trailMaxShown
	}
	return attempts, 0
}

func joinWithExtra(parts []string, extra int) string {
	s := strings.Join(parts, "; ")
	if extra > 0 {
		s += fmt.Sprintf("; +%d more", extra)
	}
	return s
}

// summarizeError reduces a provider error to a single capped line for the
// detail header and per-hop trace. Redaction proper lives in the integration's
// error-redaction layer; here we collapse whitespace and cap length so the
// header stays well-formed.
func summarizeError(ue *UpstreamError) string {
	return sanitizeSummary(errMsg(ue))
}

func sanitizeSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > summaryMaxLength {
		s = s[:summaryMaxLength]
	}
	return s
}

// safeHeaderValue strips control and non-ASCII bytes so a hostile model id can
// neither inject header lines nor make the HTTP writer reject the response, then
// caps the length (fallback-loop.ts safeHeaderValue).
func safeHeaderValue(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// formatResetEta renders a human retry ETA from a cooldown expiry (router.ts:
// 60-71): "~Ns" under 90s, "~Nm" under 90m, else "~Nh". Empty when nothing is
// cooling down or it already lapsed.
func formatResetEta(reset, now time.Time) string {
	if reset.IsZero() {
		return ""
	}
	delta := reset.Sub(now)
	if delta <= 0 {
		return ""
	}
	secs := int(math.Round(delta.Seconds()))
	if secs < 90 {
		return fmt.Sprintf("~%ds", secs)
	}
	mins := int(math.Round(float64(secs) / 60))
	if mins < 90 {
		return fmt.Sprintf("~%dm", mins)
	}
	return fmt.Sprintf("~%dh", int(math.Round(float64(mins)/60)))
}
