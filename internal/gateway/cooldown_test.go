package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testEngine(t *testing.T) (*CooldownEngine, *time.Time) {
	t.Helper()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	e := NewCooldownEngine()
	e.now = func() time.Time { return now }
	return e, &now
}

// TestRepeatOffenderEscalates is why the ladder exists: a single fixed bench
// is wrong in both directions, so an endpoint that keeps failing must cost
// progressively more while a one-off stays cheap.
func TestRepeatOffenderEscalates(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	req := CooldownRequest{DailyExhausted: true, HasPublishedDailyLimit: true}

	var got []time.Duration
	for range 5 {
		cd := e.Decide("groq/llama/k1", req)
		got = append(got, cd.Until.Sub(*now))
		e.Succeeded("groq/llama/k1", *now) // clear the bench, keep escalating
		e.steps["groq/llama/k1"] = ladderState{step: len(got), lastHit: *now}
	}

	require.Equal(t, []time.Duration{
		2 * time.Minute, 10 * time.Minute, time.Hour, 24 * time.Hour, 24 * time.Hour,
	}, got, "the ladder must climb and then hold at its top step")
}

// TestTransientFailureStaysCheap keeps a blip from costing a provider: with no
// exhaustion signal, the bench is the short transient one, not a ladder step.
func TestTransientFailureStaysCheap(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	cd := e.Decide("groq/llama/k1", CooldownRequest{})
	require.Equal(t, transientCooldown, cd.Until.Sub(*now))
	require.Equal(t, SourceHeuristic, cd.Source)
}

// TestProviderStatedWaitWins is the precedence that matters most: when a
// provider tells us how long to wait, our guess must not override it.
func TestProviderStatedWaitWins(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)

	// Longer than our transient guess: the provider's number is used and the
	// bench is marked authoritative so nothing later shortens it.
	cd := e.Decide("p/m/k", CooldownRequest{RetryAfter: 10 * time.Minute, QuotaSignal: true})
	require.Equal(t, 10*time.Minute, cd.Until.Sub(*now))
	require.Equal(t, SourceAuthoritative, cd.Source)

	// A Retry-After shorter than our guess is a floor, not a cap: we keep the
	// longer bench rather than hammering the endpoint sooner.
	e2, now2 := testEngine(t)
	cd2 := e2.Decide("p/m/k", CooldownRequest{RetryAfter: 5 * time.Second})
	require.Equal(t, transientCooldown, cd2.Until.Sub(*now2))
	require.Equal(t, SourceHeuristic, cd2.Source)
}

// TestOperatorCeilingBindsGuessesOnly keeps the ceiling from undercutting a
// provider: it may shorten what we inferred, never what we were told.
func TestOperatorCeilingBindsGuessesOnly(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	e.steps["p/m/k"] = ladderState{step: 3, lastHit: *now} // ladder says 24h

	capped := e.Decide("p/m/k", CooldownRequest{
		DailyExhausted: true, HasPublishedDailyLimit: true, Ceiling: 5 * time.Minute,
	})
	require.Equal(t, 5*time.Minute, capped.Until.Sub(*now), "our own guess must respect the ceiling")

	e2, now2 := testEngine(t)
	stated := e2.Decide("p/m/k", CooldownRequest{
		RetryAfter: time.Hour, Ceiling: 5 * time.Minute,
	})
	require.Equal(t, time.Hour, stated.Until.Sub(*now2),
		"the ceiling must not shorten a provider-stated wait")
	require.Equal(t, SourceAuthoritative, stated.Source)
}

// TestGuessedExhaustionIsCapped covers the case where a provider publishes no
// daily limit: we are inferring exhaustion from 429s alone, and a wrong guess
// must not bench an endpoint for hours.
func TestGuessedExhaustionIsCapped(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	e.steps["p/m/k"] = ladderState{step: 3, lastHit: *now} // ladder step is 24h

	cd := e.Decide("p/m/k", CooldownRequest{
		QuotaSignal: true, RecentHits: nullLimitHitThreshold, HasPublishedDailyLimit: false,
	})
	require.Equal(t, unknownLimitMaxCooldown, cd.Until.Sub(*now))
}

// TestOneRateLimitIsNotAPattern gates the inference above: a single 429 is
// noise, so it must not be treated as exhaustion.
func TestOneRateLimitIsNotAPattern(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	e.steps["p/m/k"] = ladderState{step: 2, lastHit: *now}

	cd := e.Decide("p/m/k", CooldownRequest{QuotaSignal: true, RecentHits: 1})
	require.Equal(t, transientCooldown, cd.Until.Sub(*now),
		"a lone rate limit must stay transient rather than climbing the ladder")
}

// TestLocalEndpointRecoversInSeconds keeps a restarted local model from
// looking broken: benching it for minutes would read as a gateway fault.
func TestLocalEndpointRecoversInSeconds(t *testing.T) {
	t.Parallel()

	for _, base := range []string{
		"http://127.0.0.1:11434/v1", "http://localhost:8080/v1",
		"http://192.168.1.50:1234/v1", "http://[::1]:11434/v1",
	} {
		e, now := testEngine(t)
		cd := e.Decide("local/m/k", CooldownRequest{
			BaseURL: base, RetryAfter: time.Hour, DailyExhausted: true,
		})
		require.Equal(t, localEndpointCooldown, cd.Until.Sub(*now),
			"%s must be treated as local and ignore a stated wait", base)
	}

	// A public endpoint must not be mistaken for a local one.
	e, now := testEngine(t)
	cd := e.Decide("p/m/k", CooldownRequest{BaseURL: "https://api.groq.com/openai/v1"})
	require.Equal(t, transientCooldown, cd.Until.Sub(*now))
}

// TestSuccessResetsTheLadder means a recovered endpoint is not punished for
// history: the next failure starts at the bottom step again.
func TestSuccessResetsTheLadder(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	req := CooldownRequest{DailyExhausted: true, HasPublishedDailyLimit: true}

	e.Decide("p/m/k", req)
	e.Decide("p/m/k", req)
	e.Succeeded("p/m/k", *now)

	cd := e.Decide("p/m/k", req)
	require.Equal(t, cooldownDurations[0], cd.Until.Sub(*now), "a served request must reset escalation")
}

// TestActiveExpires keeps a lapsed bench from blocking a candidate forever.
func TestActiveExpires(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	e.Decide("p/m/k", CooldownRequest{})

	_, ok := e.Active("p/m/k")
	require.True(t, ok, "the bench must hold while it is live")

	*now = now.Add(transientCooldown + time.Second)
	_, ok = e.Active("p/m/k")
	require.False(t, ok, "an expired bench must stop blocking the candidate")
}

// TestProbeClearsOnlyOurOwnGuesses is the rule that makes cooldown probing
// safe: a successful probe may withdraw our inference, but a provider has not
// withdrawn its own stated wait.
func TestProbeClearsOnlyOurOwnGuesses(t *testing.T) {
	t.Parallel()

	e, _ := testEngine(t)

	e.Decide("guess/m/k", CooldownRequest{})
	e.Clear("guess/m/k")
	_, ok := e.Active("guess/m/k")
	require.False(t, ok, "a heuristic bench must be clearable by a probe")

	e.Bench("credit/m/k", paymentRequiredCooldown, SourceCredit)
	e.Clear("credit/m/k")
	cd, ok := e.Active("credit/m/k")
	require.True(t, ok, "an out-of-credit bench must survive a probe")
	require.Equal(t, SourceCredit, cd.Source)
}

// TestBenchNeverShortensAnExistingLongerOne stops a later, milder failure from
// releasing an endpoint early.
func TestBenchNeverShortensAnExistingLongerOne(t *testing.T) {
	t.Parallel()

	e, now := testEngine(t)
	e.Bench("p/m/k", 24*time.Hour, SourceCredit)

	cd := e.Decide("p/m/k", CooldownRequest{})
	require.Equal(t, 24*time.Hour, cd.Until.Sub(*now))
	require.Equal(t, SourceCredit, cd.Source)
}

// TestSucceededKeepsNewerAuthoritativeBench is the concurrency guarantee: a
// served request resets the ladder and lifts our own guess, but must not
// withdraw a provider-stated wait a concurrent failure recorded AFTER the
// successful attempt began - that bench is newer information than the success.
func TestSucceededKeepsNewerAuthoritativeBench(t *testing.T) {
	t.Parallel()
	e, now := testEngine(t)

	attemptStart := *now
	e.steps["p/m/k"] = ladderState{step: 3, lastHit: attemptStart}
	// A concurrent failure records an authoritative wait AFTER the attempt began.
	*now = now.Add(time.Second)
	e.Bench("p/m/k", time.Hour, SourceAuthoritative)

	e.Succeeded("p/m/k", attemptStart)

	cd, ok := e.Active("p/m/k")
	require.True(t, ok, "a newer provider-stated wait must survive a success that predates it")
	require.Equal(t, SourceAuthoritative, cd.Source)
	require.Equal(t, ladderState{}, e.steps["p/m/k"], "the escalation ladder must still reset to the bottom")
}

// TestSucceededClearsOlderAuthoritativeBench is the flip side: a success that
// post-dates the bench supersedes it, provider-stated or not - the endpoint
// just answered, so a stale wait must not keep blocking it.
func TestSucceededClearsOlderAuthoritativeBench(t *testing.T) {
	t.Parallel()
	e, now := testEngine(t)

	e.Bench("p/m/k", time.Hour, SourceAuthoritative)
	*now = now.Add(time.Second)
	e.Succeeded("p/m/k", *now)

	_, ok := e.Active("p/m/k")
	require.False(t, ok, "a success that post-dates the bench lifts it, provider-stated or not")
}

// TestSucceededAlwaysLiftsHeuristicGuess proves the freshness protection is for
// provider-stated waits only: a real answer disproves our own guess whenever it
// was recorded, so even a newer heuristic bench is lifted.
func TestSucceededAlwaysLiftsHeuristicGuess(t *testing.T) {
	t.Parallel()
	e, now := testEngine(t)

	attemptStart := *now
	*now = now.Add(time.Second)
	e.Decide("p/m/k", CooldownRequest{}) // transient => SourceHeuristic
	e.Succeeded("p/m/k", attemptStart)

	_, ok := e.Active("p/m/k")
	require.False(t, ok, "a success must always lift our own guess")
}
