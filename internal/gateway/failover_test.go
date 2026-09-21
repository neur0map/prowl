package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

// fovChain returns its candidate routes in order, honouring the request's skip
// state exactly as the real router will, and answers the model/key questions.
type fovChain struct {
	routes   []Route
	routable map[int64][]int64
	hasOther bool
	lastSkip *SkipState
}

func (c *fovChain) Route(_ int, skip *SkipState) (Route, error) {
	c.lastSkip = skip
	for _, r := range c.routes {
		if _, ok := skip.Keys[routeKeyOf(r)]; ok {
			continue
		}
		if _, ok := skip.Models[r.ModelDBID]; ok {
			continue
		}
		if _, ok := skip.Platforms[r.Platform]; ok {
			continue
		}
		return r, nil
	}
	return Route{}, &RouteError{Status: 429, Message: "no candidate", Diagnostics: []string{"cooldown"}}
}

func (c *fovChain) RoutableKeys(modelDBID int64) []int64 { return c.routable[modelDBID] }

func (c *fovChain) HasOtherUsableKey(int64, int64, map[RouteKey]struct{}) bool { return c.hasOther }

// fovDispatcher replays a fixed sequence of results; the last one repeats.
type fovDispatcher struct {
	results []DispatchResult
	calls   int
}

func (d *fovDispatcher) Dispatch(context.Context, Route, int) DispatchResult {
	i := d.calls
	if i >= len(d.results) {
		i = len(d.results) - 1
	}
	d.calls++
	return d.results[i]
}

type fovBenchCall struct {
	platform, model string
	keyID           int64
	dur             time.Duration
	source          CooldownSource
}

type fovDecideCall struct {
	platform, model string
	keyID           int64
	req             CooldownRequest
}

type fovSucceedCall struct {
	platform, model string
	keyID           int64
	since           time.Time
}

type fovCooldowns struct {
	mu       sync.Mutex
	benches  []fovBenchCall
	decides  []fovDecideCall
	active   map[RouteKey]Cooldown
	soonest  time.Time
	succeeds []fovSucceedCall
}

func fovNewCooldowns() *fovCooldowns { return &fovCooldowns{active: map[RouteKey]Cooldown{}} }

func (c *fovCooldowns) Decide(p, m string, k int64, req CooldownRequest) Cooldown {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decides = append(c.decides, fovDecideCall{p, m, k, req})
	return Cooldown{}
}

func (c *fovCooldowns) Bench(p, m string, k int64, d time.Duration, s CooldownSource) Cooldown {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.benches = append(c.benches, fovBenchCall{p, m, k, d, s})
	return Cooldown{}
}

func (c *fovCooldowns) Active(p, m string, k int64) (Cooldown, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.active[RouteKey{p, m, k}]
	return cd, ok
}

func (c *fovCooldowns) SoonestExpiry() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.soonest
}

func (c *fovCooldowns) Succeeded(p, m string, k int64, since time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.succeeds = append(c.succeeds, fovSucceedCall{p, m, k, since})
}

func (c *fovCooldowns) benchCount(d time.Duration, src CooldownSource) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, b := range c.benches {
		if b.dur == d && b.source == src {
			n++
		}
	}
	return n
}

type fovScorer struct {
	mu            sync.Mutex
	rateLimitHits []int64
	modelFailures []int64
	successes     []int64
}

func (s *fovScorer) RecordRateLimitHit(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimitHits = append(s.rateLimitHits, id)
}

func (s *fovScorer) RecordModelFailure(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelFailures = append(s.modelFailures, id)
}

func (s *fovScorer) RecordSuccess(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successes = append(s.successes, id)
}

type fovLearner struct {
	mu    sync.Mutex
	calls int
}

func (l *fovLearner) LearnLimit(int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
}

type fovRevalidator struct {
	mu    sync.Mutex
	calls int
}

func (r *fovRevalidator) Revalidate(string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
}

// fovClock returns base for the first jumpAfter calls, then base+jump. Used to
// place the loop past its wall-clock budget deterministically.
type fovClock struct {
	base      time.Time
	calls     int
	jumpAfter int
	jump      time.Duration
}

func (c *fovClock) now() time.Time {
	c.calls++
	if c.calls > c.jumpAfter {
		return c.base.Add(c.jump)
	}
	return c.base
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func fovNewFailover(cfg FailoverConfig) (*Failover, *fovCooldowns, *fovScorer, *fovLearner, *fovRevalidator) {
	cd := fovNewCooldowns()
	sc := &fovScorer{}
	ln := &fovLearner{}
	rv := &fovRevalidator{}
	f := NewFailover(FailoverDeps{Cooldowns: cd, Scorer: sc, Learner: ln, Revalidator: rv}, cfg)
	return f, cd, sc, ln, rv
}

func fovRoute(platform, model string, modelDBID, keyID int64) Route {
	return Route{Platform: platform, ModelID: model, ModelDBID: modelDBID, KeyID: keyID}
}

func fovDone() DispatchResult { return DispatchResult{Outcome: OutcomeDone} }
func fovFail(status int, body string) DispatchResult {
	return DispatchResult{Status: status, Body: body}
}

func fovRun(f *Failover, chain Chain, disp Dispatcher) *Result {
	return f.Run(context.Background(), DispatchRequest{Chain: chain, Dispatcher: disp})
}

// ── Loop behaviour ───────────────────────────────────────────────────────────

func TestRetryableAdvancesFatalStops(t *testing.T) {
	t.Run("retryable failure advances then succeeds", func(t *testing.T) {
		f, _, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, routable: map[int64][]int64{1: {1, 2}}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovDone()}}
		res := fovRun(f, chain, disp)
		if res.Status != StatusSucceeded {
			t.Fatalf("want succeeded, got %v (%+v)", res.Status, res.Exhaustion)
		}
		if disp.calls != 2 {
			t.Fatalf("retryable failure should advance to a second attempt, got %d dispatches", disp.calls)
		}
		if res.FailedAttempts != 1 {
			t.Fatalf("want 1 failed attempt recorded, got %d", res.FailedAttempts)
		}
	})

	t.Run("unmatched 400 is fatal and stops immediately", func(t *testing.T) {
		f, _, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(400, "invalid parameter foo")}}
		res := fovRun(f, chain, disp)
		if res.Status != StatusFatal {
			t.Fatalf("want fatal, got %v", res.Status)
		}
		if disp.calls != 1 {
			t.Fatalf("a fatal error must stop after one attempt, got %d dispatches", disp.calls)
		}
	})
}

func TestBudgetGate(t *testing.T) {
	f, _, _, _, _ := fovNewFailover(FailoverConfig{TimeBudget: 10 * time.Millisecond})
	clk := &fovClock{base: time.Unix(1000, 0), jumpAfter: 1, jump: 20 * time.Millisecond}
	f.now = clk.now
	chain := &fovChain{
		routes:   []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2), fovRoute("p", "m", 1, 3), fovRoute("p", "m", 1, 4)},
		routable: map[int64][]int64{1: {1, 2, 3, 4}}, hasOther: true,
	}
	disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit")}}
	res := fovRun(f, chain, disp)
	if disp.calls != 2 {
		t.Fatalf("budget must fovRun attempts 0 and 1 but stop attempt 2, got %d dispatches", disp.calls)
	}
	if res.Status != StatusExhausted || !res.TimedOut {
		t.Fatalf("want timed-out exhaustion, got status=%v timedOut=%v", res.Status, res.TimedOut)
	}
}

func TestSkipScopes(t *testing.T) {
	t.Run("model-not-found rules out the whole model", func(t *testing.T) {
		f, _, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m2", 2, 2)}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(404, "model not found"), fovDone()}}
		fovRun(f, chain, disp)
		if _, ok := chain.lastSkip.Models[1]; !ok {
			t.Fatal("model 1 should be skipped after a 404")
		}
		if _, ok := chain.lastSkip.Keys[RouteKey{"p", "m", 1}]; !ok {
			t.Fatal("the failed key should also be skipped")
		}
		if len(chain.lastSkip.Platforms) != 0 {
			t.Fatalf("a 404 must not skip the platform, got %v", chain.lastSkip.Platforms)
		}
	})

	t.Run("provider-level failure rules out the whole platform", func(t *testing.T) {
		f, _, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p2", "m2", 2, 2)}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(500, "internal server error"), fovDone()}}
		fovRun(f, chain, disp)
		if _, ok := chain.lastSkip.Platforms["p"]; !ok {
			t.Fatal("platform p should be skipped after a 5xx")
		}
		if len(chain.lastSkip.Models) != 0 {
			t.Fatalf("a 5xx must not skip a model, got %v", chain.lastSkip.Models)
		}
	})

	t.Run("transient 429 skips only the key", func(t *testing.T) {
		f, _, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, routable: map[int64][]int64{1: {1, 2}}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovDone()}}
		fovRun(f, chain, disp)
		if _, ok := chain.lastSkip.Keys[RouteKey{"p", "m", 1}]; !ok {
			t.Fatal("the failed key should be skipped")
		}
		if len(chain.lastSkip.Models) != 0 || len(chain.lastSkip.Platforms) != 0 {
			t.Fatalf("a transient 429 must skip neither model nor platform, got models=%v platforms=%v",
				chain.lastSkip.Models, chain.lastSkip.Platforms)
		}
	})
}

func TestModelPenaltyGate(t *testing.T) {
	t.Run("no penalty while a sibling key can still serve", func(t *testing.T) {
		f, _, sc, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, routable: map[int64][]int64{1: {1, 2}}, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovDone()}}
		fovRun(f, chain, disp)
		if n := len(sc.rateLimitHits) + len(sc.modelFailures); n != 0 {
			t.Fatalf("a usable sibling key means the model is not at fault: want no penalty, got %d", n)
		}
	})

	t.Run("heavy penalty on a 429 when no sibling is usable", func(t *testing.T) {
		f, _, sc, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, routable: map[int64][]int64{1: {1, 2}}, hasOther: false}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovDone()}}
		fovRun(f, chain, disp)
		if len(sc.rateLimitHits) != 1 || sc.rateLimitHits[0] != 1 {
			t.Fatalf("a rate-limit signal earns the heavy demotion, got %v", sc.rateLimitHits)
		}
		if len(sc.modelFailures) != 0 {
			t.Fatalf("a 429 is heavy, never light, got modelFailures=%v", sc.modelFailures)
		}
	})

	t.Run("light penalty on a 5xx when no sibling is usable", func(t *testing.T) {
		f, _, sc, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p2", "m2", 2, 2)}, hasOther: false}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(500, "internal server error"), fovDone()}}
		fovRun(f, chain, disp)
		if len(sc.modelFailures) != 1 || sc.modelFailures[0] != 1 {
			t.Fatalf("an ordinary upstream failure earns the light demotion, got %v", sc.modelFailures)
		}
		if len(sc.rateLimitHits) != 0 {
			t.Fatalf("a 5xx is not a rate-limit signal, got rateLimitHits=%v", sc.rateLimitHits)
		}
	})
}

func TestModelBench(t *testing.T) {
	threeKeys := []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2), fovRoute("p", "m", 1, 3)}
	routable := map[int64][]int64{1: {1, 2, 3}}

	t.Run("no bench after two failures in the window", func(t *testing.T) {
		f, cd, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: threeKeys, routable: routable, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovFail(429, "rate limit"), fovDone()}}
		fovRun(f, chain, disp)
		if n := cd.benchCount(modelFailureCooldown, SourceHeuristic); n != 0 {
			t.Fatalf("the model bench must not fire on the second failure, got %d benches", n)
		}
	})

	t.Run("bench spans every routable key on the third failure", func(t *testing.T) {
		f, cd, _, _, _ := fovNewFailover(FailoverConfig{})
		chain := &fovChain{routes: threeKeys, routable: routable, hasOther: true}
		disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovFail(429, "rate limit"), fovFail(429, "rate limit"), fovDone()}}
		fovRun(f, chain, disp)
		if n := cd.benchCount(modelFailureCooldown, SourceHeuristic); n != 3 {
			t.Fatalf("the third failure must bench the model on all 3 routable keys, got %d benches", n)
		}
	})
}

func TestEmptyCompletionStreak(t *testing.T) {
	f, cd, sc, ln, _ := fovNewFailover(FailoverConfig{})
	// A single fovRoute so the streak accrues on the same (model, key) across the
	// three requests; within one request the key is skipped after its failure.
	runEmpty := func() {
		chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1)}, routable: map[int64][]int64{1: {1}}, hasOther: false}
		disp := &fovDispatcher{results: []DispatchResult{{Err: &UpstreamError{SkipBench: true, Message: "empty completion"}}}}
		fovRun(f, chain, disp)
	}

	runEmpty()
	runEmpty()
	if n := len(cd.benches) + len(cd.decides); n != 0 {
		t.Fatalf("the first two empty completions are exempt: want no cooldown recorded, got %d", n)
	}
	if len(sc.rateLimitHits)+len(sc.modelFailures) != 0 {
		t.Fatal("an exempt empty completion must incur no model penalty")
	}
	if ln.calls != 0 {
		t.Fatal("an exempt empty completion must not learn limits")
	}

	runEmpty()
	if len(cd.decides) != 1 {
		t.Fatalf("the third empty completion lifts the exemption: want one cooldown recorded, got %d", len(cd.decides))
	}
	if len(sc.modelFailures) != 1 {
		t.Fatalf("the third empty completion must incur one light penalty, got %v", sc.modelFailures)
	}
	if ln.calls != 1 {
		t.Fatalf("the third empty completion must learn limits once, got %d", ln.calls)
	}
}

// ── Exhaustion taxonomy ──────────────────────────────────────────────────────

func TestExhaustionTaxonomy(t *testing.T) {
	f, cd, _, _, _ := fovNewFailover(FailoverConfig{})
	at := func(c AttemptClass) Attempt { return Attempt{Platform: "p", ModelID: "m", KeyOrdinal: 1, Class: c} }
	ue := func(status int, msg string) error { return &UpstreamError{Status: status, Message: msg} }

	assert := func(t *testing.T, ex *Exhaustion, status int, kind ExhaustionKind, code string) {
		t.Helper()
		if ex.Status == 500 {
			t.Fatal("an exhaustion body must never be 500")
		}
		if ex.Status != status || ex.Kind != kind || ex.Code != code {
			t.Fatalf("got %d/%s/%s, want %d/%s/%s", ex.Status, ex.Kind, ex.Code, status, kind, code)
		}
	}

	t.Run("all auth -> 502", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(401, "unauthorized"), []Attempt{at(AttemptAuth), at(AttemptAuth)}, exhaustionCtx{}, 20)
		assert(t, ex, 502, ExhaustAuth, "provider_authentication_failed")
	})
	t.Run("all context too large -> 413", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(400, "maximum context length"), []Attempt{at(AttemptContextTooLarge), at(AttemptContextTooLarge)}, exhaustionCtx{}, 20)
		assert(t, ex, 413, ExhaustContextTooLarge, "context_length_exceeded")
	})
	t.Run("all model not found -> 404", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(404, "not found"), []Attempt{at(AttemptModelNotFound), at(AttemptModelNotFound)}, exhaustionCtx{}, 20)
		assert(t, ex, 404, ExhaustModelNotFound, "model_not_found")
	})
	t.Run("degraded last error -> 503", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(400, "DEGRADED function cannot be invoked"), []Attempt{at(AttemptUpstreamError)}, exhaustionCtx{}, 20)
		assert(t, ex, 503, ExhaustUnavailable, "provider_degraded")
	})
	t.Run("provider bad request last error -> 400", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(400, "API error 400: bad shape"), []Attempt{at(AttemptProviderBadRequest)}, exhaustionCtx{}, 20)
		assert(t, ex, 400, ExhaustBadRequest, "provider_rejected_request")
	})
	t.Run("breaker trip -> 503", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(500, "boom"), []Attempt{at(AttemptUpstreamError), at(AttemptUpstreamError), at(AttemptUpstreamError)}, exhaustionCtx{breakerFails: 3}, 20)
		assert(t, ex, 503, ExhaustUnavailable, "upstream_unhealthy")
	})
	t.Run("all time-bound -> 429 with retry time", func(t *testing.T) {
		cd.soonest = f.now().Add(90 * time.Second)
		ex := f.exhaustedRetryError(ue(429, "rate limit"), []Attempt{at(AttemptRateLimited), at(AttemptForbidden)}, exhaustionCtx{}, 20)
		assert(t, ex, 429, ExhaustRateLimit, "rate_limit_exceeded")
		if ex.RetryAt.IsZero() {
			t.Fatal("a 429 exhaustion must carry the soonest cooldown reset")
		}
		if s, ok := ex.RetryAfterSeconds(f.now()); !ok || s <= 0 {
			t.Fatalf("Retry-After seconds must be positive, got %d ok=%v", s, ok)
		}
	})
	t.Run("mixed upstream failures -> 502", func(t *testing.T) {
		ex := f.exhaustedRetryError(ue(500, "boom"), []Attempt{at(AttemptRateLimited), at(AttemptUpstreamError)}, exhaustionCtx{}, 20)
		assert(t, ex, 502, ExhaustUpstream, "upstream_failed")
	})
}

// ── Attempt trail headers ────────────────────────────────────────────────────

func TestTrailHeaders(t *testing.T) {
	r := &Result{
		FailedAttempts: 2,
		Attempts: []Attempt{
			{Platform: "groq", ModelID: "x", KeyOrdinal: 1, Class: AttemptRateLimited, Summary: "rate; limited"},
			{Platform: "nvidia", ModelID: "y", KeyOrdinal: 2, Class: AttemptTimeout},
		},
	}
	h := r.TrailHeaders(false)
	if h["X-Fallback-Attempts"] != "2" {
		t.Fatalf("X-Fallback-Attempts = %q, want 2", h["X-Fallback-Attempts"])
	}
	if want := "groq/x key1=rate_limited; nvidia/y key2=timeout"; h["X-Fallback-Trail"] != want {
		t.Fatalf("X-Fallback-Trail = %q, want %q", h["X-Fallback-Trail"], want)
	}
	if _, ok := h["X-Fallback-Detail"]; ok {
		t.Fatal("X-Fallback-Detail is opt-in and must be absent by default")
	}

	hd := r.TrailHeaders(true)
	detail, ok := hd["X-Fallback-Detail"]
	if !ok {
		t.Fatal("X-Fallback-Detail must be present when requested")
	}
	// The record separator "; " is protected: a summary's own semicolon becomes
	// a comma so the segments stay unambiguous.
	if !strings.Contains(detail, "msg=rate, limited") {
		t.Fatalf("detail summary should neutralise the semicolon, got %q", detail)
	}
}

func TestTrailCapAndSanitisation(t *testing.T) {
	var atts []Attempt
	for i := range 12 {
		atts = append(atts, Attempt{Platform: "p", ModelID: "m", KeyOrdinal: i + 1, Class: AttemptRateLimited})
	}
	s := FormatTrail(atts)
	if !strings.HasSuffix(s, "; +2 more") {
		t.Fatalf("trail must cap at %d hops with a remainder, got %q", trailMaxShown, s)
	}
	if !strings.Contains(s, "key10=") || strings.Contains(s, "key11=") {
		t.Fatalf("trail must show exactly the first %d hops, got %q", trailMaxShown, s)
	}

	if got := safeHeaderValue("ok\r\nInjected: bad\x00\u00e9", 100); strings.ContainsAny(got, "\r\n\x00") || strings.Contains(got, "\u00e9") {
		t.Fatalf("safeHeaderValue must strip control and non-ASCII bytes, got %q", got)
	}
}

// TestConcurrentRunsRaceFree exercises the process-lived failure state (model
// windows, empty-completion streaks, revalidation dedupe, null-limit counters)
// from many request goroutines at once. It defends the "guard shared state"
// invariant: dropping a lock trips the race detector or a concurrent-map panic.
func TestConcurrentRunsRaceFree(t *testing.T) {
	f, _, _, _, _ := fovNewFailover(FailoverConfig{})
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			chain := &fovChain{
				routes:   []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2), fovRoute("p", "m", 1, 3)},
				routable: map[int64][]int64{1: {1, 2, 3}},
				hasOther: false,
			}
			// 429 (model window + null-limit + penalty), then 401 (auth bench +
			// revalidation dedupe), then success (clears the streaks).
			disp := &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit"), fovFail(401, "unauthorized"), fovDone()}}
			fovRun(f, chain, disp)
		}()
	}
	wg.Wait()
}

// TestSlowHonestAttemptDoesNotForecloseFailover is the live failure this gate
// caused: an agent turn streamed for well over the time budget, died with
// "unexpected EOF", and the budget then refused every further hop even though
// hundreds of candidates were untried. Time inside one attempt is the
// provider's work, not the gateway thrashing, so it must not spend the budget.
func TestSlowHonestAttemptDoesNotForecloseFailover(t *testing.T) {
	f, _, _, _, _ := fovNewFailover(FailoverConfig{TimeBudget: 45 * time.Second})

	// The clock advances 90s while the FIRST attempt is in flight and stays
	// still afterwards, so later hops add no churn at all.
	base := time.Unix(1000, 0)
	disp := &fovDispatcher{results: []DispatchResult{
		fovFail(500, "unexpected EOF"),
		fovFail(500, "unexpected EOF"),
		fovDone(),
	}}
	f.now = func() time.Time {
		if disp.calls == 0 {
			return base
		}
		return base.Add(90 * time.Second)
	}

	chain := &fovChain{
		// Distinct platforms keep the 5xx platform sweep out of the way: the
		// subject here is the time budget, not the skip scope.
		routes: []Route{
			fovRoute("p1", "m", 1, 1), fovRoute("p2", "m2", 2, 2), fovRoute("p3", "m3", 3, 3),
		},
		routable: map[int64][]int64{1: {1}, 2: {2}, 3: {3}},
		hasOther: true,
	}
	res := fovRun(f, chain, disp)

	if disp.calls != 3 {
		t.Fatalf("a slow first attempt must not stop later hops, got %d dispatches", disp.calls)
	}
	if res.Status != StatusSucceeded {
		t.Fatalf("want the third hop to succeed, got status=%v", res.Status)
	}
}

// TestUndispatchableRecordsNoHealthSignal proves a candidate the gateway cannot
// dispatch (no wire adapter) skips its platform and advances, but mutates no
// provider health: nothing was asked of the provider or the model, so a
// cooldown, a model-failure window, a penalty, or a limit-learning attempt
// would slander a healthy endpoint for our own configuration gap.
func TestUndispatchableRecordsNoHealthSignal(t *testing.T) {
	f, cd, sc, ln, _ := fovNewFailover(FailoverConfig{})
	chain := &fovChain{
		routes:   []Route{fovRoute("p", "m", 1, 1), fovRoute("p2", "m2", 2, 2)},
		routable: map[int64][]int64{1: {1}}, hasOther: false,
	}
	disp := &fovDispatcher{results: []DispatchResult{{Err: UndispatchableError("p")}, fovDone()}}
	res := fovRun(f, chain, disp)

	if res.Status != StatusSucceeded {
		t.Fatalf("the loop must advance past an undispatchable platform to a servable one, got %v", res.Status)
	}
	if disp.calls != 2 || res.FailedAttempts != 1 {
		t.Fatalf("want 2 dispatches / 1 failed attempt, got %d / %d", disp.calls, res.FailedAttempts)
	}
	cd.mu.Lock()
	nDecide, nBench := len(cd.decides), len(cd.benches)
	cd.mu.Unlock()
	if nDecide != 0 || nBench != 0 {
		t.Fatalf("undispatchable must bench nothing, got %d decides / %d benches", nDecide, nBench)
	}
	sc.mu.Lock()
	nPenalty := len(sc.rateLimitHits) + len(sc.modelFailures)
	sc.mu.Unlock()
	if nPenalty != 0 {
		t.Fatalf("undispatchable must not penalise the model, got %d penalty calls", nPenalty)
	}
	ln.mu.Lock()
	nLearn := ln.calls
	ln.mu.Unlock()
	if nLearn != 0 {
		t.Fatalf("undispatchable must not attempt limit-learning, got %d calls", nLearn)
	}
}

// TestSuccessResetsRouteLadder proves the served-request reset is actually
// wired: recordSuccess calls the cooldown seam for the served route, carrying
// the attempt's start as the freshness bound that protects a concurrent bench.
func TestSuccessResetsRouteLadder(t *testing.T) {
	f, cd, _, _, _ := fovNewFailover(FailoverConfig{})
	base := time.Unix(2000, 0)
	f.now = func() time.Time { return base }
	chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1)}, routable: map[int64][]int64{1: {1}}, hasOther: true}
	res := fovRun(f, chain, &fovDispatcher{results: []DispatchResult{fovDone()}})
	if res.Status != StatusSucceeded {
		t.Fatalf("want succeeded, got %v", res.Status)
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.succeeds) != 1 {
		t.Fatalf("a served request must reset the route's ladder exactly once, got %d", len(cd.succeeds))
	}
	got := cd.succeeds[0]
	if got.platform != "p" || got.model != "m" || got.keyID != 1 {
		t.Fatalf("ladder reset must target the served route, got %+v", got)
	}
	if !got.since.Equal(base) {
		t.Fatalf("ladder reset must carry the attempt start as its freshness bound, got %v want %v", got.since, base)
	}
}

// TestBrokenCommittedStreamIsNotAccountedAsSuccess proves the fix for a stream
// that flushed bytes and then broke: the loop must NOT run success-side
// health/cooldown accounting (which would report a broken endpoint healthy and
// wipe a concurrent bench), must NOT fail over after the commit, and must
// surface the hop as a failure rather than a success.
func TestBrokenCommittedStreamIsNotAccountedAsSuccess(t *testing.T) {
	f, cd, sc, _, _ := fovNewFailover(FailoverConfig{})
	chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1), fovRoute("p", "m", 1, 2)}, routable: map[int64][]int64{1: {1, 2}}, hasOther: true}
	disp := &fovDispatcher{results: []DispatchResult{
		{Outcome: OutcomeCommitted, Err: errors.New("stream broke mid-flight")},
	}}
	res := fovRun(f, chain, disp)

	if res.Status != StatusCommittedError {
		t.Fatalf("a broken committed stream must be a committed-error, got %v", res.Status)
	}
	if res.Err == nil {
		t.Fatal("a broken committed stream must carry its error")
	}
	if disp.calls != 1 {
		t.Fatalf("a committed stream cannot fail over: bytes are flushed, want 1 dispatch, got %d", disp.calls)
	}
	cd.mu.Lock()
	succeeds := len(cd.succeeds)
	cd.mu.Unlock()
	if succeeds != 0 {
		t.Fatalf("a broken committed stream must not reset any cooldown ladder, got %d Succeeded calls", succeeds)
	}
	sc.mu.Lock()
	successes := len(sc.successes)
	sc.mu.Unlock()
	if successes != 0 {
		t.Fatalf("a broken committed stream must not lift the model penalty, got %d RecordSuccess calls", successes)
	}
}

// TestCleanCommittedStreamStaysASuccess is the contrast: a committed stream that
// carried no error is a genuine success and DOES run the success accounting, so
// the fix narrows to broken commits only.
func TestCleanCommittedStreamStaysASuccess(t *testing.T) {
	f, cd, sc, _, _ := fovNewFailover(FailoverConfig{})
	chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1)}, routable: map[int64][]int64{1: {1}}, hasOther: true}
	disp := &fovDispatcher{results: []DispatchResult{{Outcome: OutcomeCommitted}}}
	res := fovRun(f, chain, disp)

	if res.Status != StatusSucceeded {
		t.Fatalf("a clean committed stream is a success, got %v", res.Status)
	}
	cd.mu.Lock()
	succeeds := len(cd.succeeds)
	cd.mu.Unlock()
	sc.mu.Lock()
	successes := len(sc.successes)
	sc.mu.Unlock()
	if succeeds != 1 || successes != 1 {
		t.Fatalf("a clean committed stream must run success accounting, got %d ladder resets and %d penalty lifts", succeeds, successes)
	}
}

// TestConfiguredCeilingReachesCooldownPricing proves the operator's live
// cooldown ceiling is consumed, not ignored: the construction default binds the
// first transient pricing, and a runtime SetCooldownCeiling change binds the
// next one.
func TestConfiguredCeilingReachesCooldownPricing(t *testing.T) {
	f, cd, _, _, _ := fovNewFailover(FailoverConfig{CooldownCeiling: 5 * time.Minute})
	chain := &fovChain{routes: []Route{fovRoute("p", "m", 1, 1)}, routable: map[int64][]int64{1: {1}}, hasOther: false}

	fovRun(f, chain, &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit")}})
	f.SetCooldownCeiling(30 * time.Second)
	fovRun(f, chain, &fovDispatcher{results: []DispatchResult{fovFail(429, "rate limit")}})

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.decides) != 2 {
		t.Fatalf("want two transient pricings, got %d", len(cd.decides))
	}
	if cd.decides[0].req.Ceiling != 5*time.Minute {
		t.Fatalf("first pricing must use the construction ceiling, got %v", cd.decides[0].req.Ceiling)
	}
	if cd.decides[1].req.Ceiling != 30*time.Second {
		t.Fatalf("second pricing must use the live ceiling, got %v", cd.decides[1].req.Ceiling)
	}
}
