package gateway

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cooldowns are the benching engine ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md).
//
// A single fixed cooldown is wrong in both directions: too short and a
// rate-limited endpoint is hammered every request, too long and a blip costs
// a provider for an hour. So a repeat offender escalates through a ladder,
// while what the provider itself told us always wins over our guess.
var cooldownDurations = []time.Duration{
	2 * time.Minute,
	10 * time.Minute,
	time.Hour,
	24 * time.Hour,
}

const (
	// transientCooldown is the default bench for a one-off rate limit or a
	// provider-side error, short enough that a blip costs almost nothing.
	transientCooldown = 90 * time.Second

	// authFailureCooldown covers a rejected key. Long enough to stop retrying
	// a dead credential, short enough that pasting a new one recovers fast.
	authFailureCooldown = 5 * time.Minute

	// paymentRequiredCooldown and modelForbiddenCooldown cover states that do
	// not fix themselves within a session: no credit, or no access to a tier.
	paymentRequiredCooldown = 24 * time.Hour
	modelForbiddenCooldown  = 24 * time.Hour

	// unknownLimitMaxCooldown caps a purely guessed bench. When a provider
	// publishes no daily limit we are inferring exhaustion from 429s alone,
	// and a wrong guess must not cost hours.
	unknownLimitMaxCooldown = 10 * time.Minute

	// localEndpointCooldown applies to loopback and private-network
	// endpoints. A local model that just restarted is back in seconds, and
	// benching it for minutes would look like the gateway is broken.
	localEndpointCooldown = 5 * time.Second

	// nullLimitHitThreshold and nullLimitHitWindow gate the guess above: one
	// 429 is noise, two inside an hour is a pattern.
	nullLimitHitThreshold = 2
	nullLimitHitWindow    = time.Hour

	// ladderWindow is how long prior strikes count toward escalation.
	ladderWindow = 24 * time.Hour

	// modelFailureThreshold benches a model across every key once it fails
	// this many times in modelFailureWindow: the model is the common factor,
	// not the credential.
	modelFailureThreshold = 3
	modelFailureWindow    = 15 * time.Minute
	modelFailureCooldown  = 10 * time.Minute

	// emptyCompletionStreakLimit lets the first two empty answers pass
	// without benching, since a reasoning model can legitimately return
	// nothing once. The third is treated as a real failure.
	emptyCompletionStreakLimit = 3
)

// CooldownSource records who decided a bench, which decides what may later
// shorten it. Only our own guesses are probe-eligible or subject to the
// operator ceiling; a provider-stated wait is never second-guessed.
type CooldownSource string

const (
	// SourceHeuristic is our own inference.
	SourceHeuristic CooldownSource = "heuristic"
	// SourceAuthoritative is the provider's own Retry-After or reset time.
	SourceAuthoritative CooldownSource = "authoritative"
	// SourceCredit is an out-of-credit state.
	SourceCredit CooldownSource = "credit"
	// SourceTier is a plan or tier restriction.
	SourceTier CooldownSource = "tier"
)

// Cooldown is one active bench.
type Cooldown struct {
	Until  time.Time
	Source CooldownSource

	// Step is the ladder position that produced this bench, so the next
	// strike escalates instead of restarting at the bottom.
	Step int
}

// CooldownRequest describes a failure the engine must price.
type CooldownRequest struct {
	// RetryAfter is what the provider asked for; zero when it said nothing.
	RetryAfter time.Duration

	// DailyExhausted means a published daily counter is spent, so the bench
	// should run to the reset rather than to a guess.
	DailyExhausted bool

	// DailyReset is when that counter rolls over. Zero means unknown.
	DailyReset time.Time

	// HasPublishedDailyLimit distinguishes a measured exhaustion from an
	// inferred one; an inferred bench is capped.
	HasPublishedDailyLimit bool

	// QuotaSignal is true when the provider actually returned a rate-limit
	// status, as opposed to us merely suspecting one.
	QuotaSignal bool

	// RecentHits is how many quota signals this endpoint produced inside
	// nullLimitHitWindow.
	RecentHits int

	// BaseURL is used to recognise a local endpoint.
	BaseURL string

	// Ceiling is the operator's maximum for our own guesses. Zero means
	// unlimited. It never shortens a provider-stated wait.
	Ceiling time.Duration
}

// CooldownEngine tracks benches per (provider, model, key) with escalation.
type CooldownEngine struct {
	mu     sync.Mutex
	active map[string]Cooldown
	steps  map[string]ladderState
	// recorded is when each active bench was written; in-memory only, so a
	// restart forgets it - correct, because there is no concurrent in-flight
	// request to protect across a restart. It lets a served-request reset keep
	// a newer authoritative bench a parallel failure recorded after the
	// successful attempt began.
	recorded map[string]time.Time
	now      func() time.Time

	// sink persists benches so a restart does not forget them; nil keeps the
	// engine purely in memory, which is what the tests use.
	sink CooldownSink
}

type ladderState struct {
	step    int
	lastHit time.Time
}

// NewCooldownEngine returns an engine using wall-clock time.
func NewCooldownEngine() *CooldownEngine {
	return &CooldownEngine{
		active:   map[string]Cooldown{},
		steps:    map[string]ladderState{},
		recorded: map[string]time.Time{},
		now:      time.Now,
	}
}

// Decide prices a failure and records the resulting bench under key.
//
// Precedence is deliberate: a provider-stated wait outranks our ladder, a
// spent daily counter outranks a transient guess, and a local endpoint
// outranks everything because it recovers in seconds.
func (c *CooldownEngine) Decide(key string, req CooldownRequest) Cooldown {
	var flush func()
	// Registered before the unlock below, so it runs after it.
	defer func() {
		if flush != nil {
			flush()
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()

	if isLocalEndpoint(req.BaseURL) {
		// Never escalates and ignores Retry-After: the endpoint is ours.
		cd := Cooldown{Until: now.Add(localEndpointCooldown), Source: SourceHeuristic}
		c.active[key] = cd
		c.recorded[key] = now
		flush = c.saveLater(key, cd, 0, now)
		return cd
	}

	state := c.steps[key]
	if now.Sub(state.lastHit) > ladderWindow {
		state.step = 0
	}

	base := transientCooldown
	source := SourceHeuristic
	stepUsed := state.step

	if req.DailyExhausted || c.nullLimitHeuristic(req) {
		base = cooldownDurations[min(state.step, len(cooldownDurations)-1)]
		stepUsed = state.step
		state.step = min(state.step+1, len(cooldownDurations)-1)
		if !req.HasPublishedDailyLimit {
			// A guess, so cap it: we may be wrong about exhaustion entirely.
			base = min(base, unknownLimitMaxCooldown)
		}
	}

	if req.Ceiling > 0 {
		// The operator's ceiling binds our guesses only.
		base = min(base, req.Ceiling)
	}

	until := now.Add(base)

	// The provider's own answer is a floor, not a suggestion, and it is not
	// subject to the ceiling above.
	if req.RetryAfter > 0 && req.RetryAfter > base {
		until = now.Add(min(req.RetryAfter, 24*time.Hour))
		source = SourceAuthoritative
	}
	if req.DailyExhausted && !req.DailyReset.IsZero() && req.DailyReset.After(until) {
		until = req.DailyReset
		source = SourceAuthoritative
	}

	state.lastHit = now
	c.steps[key] = state

	cd := Cooldown{Until: until, Source: source, Step: stepUsed}
	// Never shorten a bench that is already longer.
	if existing, ok := c.active[key]; ok && existing.Until.After(cd.Until) {
		return existing
	}
	c.active[key] = cd
	c.recorded[key] = now
	flush = c.saveLater(key, cd, state.step, now)
	return cd
}

// nullLimitHeuristic reports whether repeated real 429s justify treating an
// endpoint as exhausted even though it publishes no daily limit.
func (c *CooldownEngine) nullLimitHeuristic(req CooldownRequest) bool {
	return req.QuotaSignal && req.RecentHits >= nullLimitHitThreshold
}

// Bench records a fixed-duration cooldown with an explicit provenance, for the
// states that are not rate limits: a rejected key, no credit, no tier access.
func (c *CooldownEngine) Bench(key string, d time.Duration, source CooldownSource) Cooldown {
	var flush func()
	// Registered before the unlock below, so it runs after it.
	defer func() {
		if flush != nil {
			flush()
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	cd := Cooldown{Until: c.now().Add(d), Source: source}
	if existing, ok := c.active[key]; ok && existing.Until.After(cd.Until) {
		return existing
	}
	c.active[key] = cd
	c.recorded[key] = c.now()
	flush = c.saveLater(key, cd, c.steps[key].step, c.now())
	return cd
}

// Active returns the live bench for key, if any.
func (c *CooldownEngine) Active(key string) (Cooldown, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.active[key]
	if !ok || !cd.Until.After(c.now()) {
		return Cooldown{}, false
	}
	return cd, true
}

// BenchedByKey counts the active benches per key id and reports when each
// key's earliest one lifts.
//
// Benches are per (platform, model, key), so a key is rarely benched as a
// whole: it is benched for the models that refused it. A dashboard showing
// "unhealthy" cannot tell that apart from a broken credential, which is the
// difference between "wait" and "fix this".
func (c *CooldownEngine) BenchedByKey() map[int64]KeyBench {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()

	out := map[int64]KeyBench{}
	for key, cd := range c.active {
		if !cd.Until.After(now) {
			continue
		}
		idx := strings.LastIndex(key, "/")
		if idx < 0 {
			continue
		}
		keyID, err := strconv.ParseInt(key[idx+1:], 10, 64)
		if err != nil {
			continue
		}
		entry := out[keyID]
		entry.Models++
		if entry.Until.IsZero() || cd.Until.Before(entry.Until) {
			entry.Until = cd.Until
		}
		out[keyID] = entry
	}
	return out
}

// KeyBench summarises one key's active benches.
type KeyBench struct {
	Models int       // how many of the key's models are held back
	Until  time.Time // when the earliest of them lifts
}

// SoonestExpiry is the earliest moment any active bench lifts, or the zero
// time when nothing is benched.
//
// It answers the only question a caller can usefully be told when every
// candidate is cooling down: come back then. That becomes the Retry-After on
// an all-time-bound exhaustion, which is the difference between a client
// backing off correctly and one hammering a gateway that has nothing to give.
func (c *CooldownEngine) SoonestExpiry() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var soonest time.Time
	for _, cd := range c.active {
		if !cd.Until.After(now) {
			continue
		}
		if soonest.IsZero() || cd.Until.Before(soonest) {
			soonest = cd.Until
		}
	}
	return soonest
}

// Clear lifts every heuristic bench for a key, which is what a successful
// probe means: our guess was wrong, or the limit has reset. Provider-stated
// waits survive, because the provider has not withdrawn them.
func (c *CooldownEngine) Clear(key string) {
	var flush func()
	// Registered before the unlock below, so it runs after it.
	defer func() {
		if flush != nil {
			flush()
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.active[key]; ok && cd.Source == SourceHeuristic {
		delete(c.active, key)
		delete(c.recorded, key)
		flush = c.dropLater(key)
	}
}

// Succeeded is the served-request reset for a route: a real answer means the
// endpoint recovered, so the escalation ladder returns to the bottom and our
// own guessed bench lifts. sinceKnownAt is when the successful attempt began;
// an authoritative (provider-stated) bench recorded AFTER that instant is
// newer information a concurrent failure just learned, so it survives - a
// success that predates it must not withdraw a wait the provider still owns.
// A heuristic bench is always lifted: a served response disproves our guess
// regardless of timing.
func (c *CooldownEngine) Succeeded(key string, sinceKnownAt time.Time) {
	var flush func()
	// Registered before the unlock below, so it runs after it.
	defer func() {
		if flush != nil {
			flush()
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.steps, key)
	cd, ok := c.active[key]
	if !ok {
		return
	}
	if cd.Source != SourceHeuristic && c.recorded[key].After(sinceKnownAt) {
		return
	}
	delete(c.active, key)
	delete(c.recorded, key)
	flush = c.dropLater(key)
}

// saveLater and dropLater capture a sink write to be run AFTER the lock is
// released.
//
// The sink must never be called while the lock is held. The store runs SQLite
// with a single connection, so a write that waits for it under this lock lets
// a request that already owns the connection -- and then needs this lock --
// park the whole gateway. That is not hypothetical: it happened, with every
// goroutine stuck on the connection pool.
func (c *CooldownEngine) saveLater(key string, cd Cooldown, step int, lastHit time.Time) func() {
	sink := c.sink
	if sink == nil {
		return nil
	}
	return func() { sink.SaveCooldown(key, cd, step, lastHit) }
}

func (c *CooldownEngine) dropLater(key string) func() {
	sink := c.sink
	if sink == nil {
		return nil
	}
	return func() { sink.DropCooldown(key) }
}

// isLocalEndpoint reports whether a base URL points at this machine or a
// private network.
func isLocalEndpoint(raw string) bool {
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
