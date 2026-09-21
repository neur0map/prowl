package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/store"
)

// qClock is a settable clock shared by the quota tests. It is safe for
// concurrent use so the -race admission test can read it from many goroutines.
type qClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *qClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *qClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func newQuotaTestLedger(t *testing.T) (*Ledger, *qClock) {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	l := NewLedger(st.DB())
	// Mid-minute, mid-day, so a 40s advance crosses a minute boundary without
	// touching midnight.
	fc := &qClock{now: time.Date(2026, 1, 15, 12, 0, 30, 0, time.UTC)}
	l.clock = fc.Now
	return l, fc
}

func quotaSettle(t *testing.T, l *Ledger, a Admission, tokens int64) {
	t.Helper()
	ls, ok := l.Acquire(a)
	if !ok {
		t.Fatalf("acquire rejected unexpectedly")
	}
	ls.Settle(tokens)
}

func TestQuotaKeyAndPoolKey(t *testing.T) {
	if got := QuotaKey("groq", "llama-3.3-70b", 7); got != "groq/llama-3.3-70b/7" {
		t.Errorf("QuotaKey = %q", got)
	}
	if got := PoolKey("groq", 7); got != "groq/7" {
		t.Errorf("PoolKey = %q", got)
	}
	// A model id containing a slash must not collide a quota key with a pool
	// key: the quota key keeps its three segments, the pool key two.
	if QuotaKey("openrouter", "a/b", 3) == PoolKey("openrouter", 3) {
		t.Errorf("quota key collided with pool key")
	}
}

// A minute window rolls over on its boundary while the day window it shares an
// instant with does not: the same advance that frees an RPM ceiling must still
// leave an RPD ceiling spent.
func TestMinuteRollsOverButDayDoesNot(t *testing.T) {
	l, fc := newQuotaTestLedger(t)
	base := Admission{Platform: "p", ModelID: "m", KeyID: 1}

	quotaSettle(t, l, base, 0) // one request at 12:00:30

	perMinute := base
	perMinute.Limits = WindowLimits{RPM: 1}
	if l.Admit(perMinute) {
		t.Fatalf("RPM=1 should reject a second request inside the same minute")
	}

	perDay := base
	perDay.Limits = WindowLimits{RPD: 1}
	if l.Admit(perDay) {
		t.Fatalf("RPD=1 should reject a second request inside the same day")
	}

	fc.Advance(40 * time.Second) // 12:01:10 - new minute, same UTC day

	if !l.Admit(perMinute) {
		t.Errorf("RPM=1 should admit again once the minute rolled over")
	}
	if l.Admit(perDay) {
		t.Errorf("RPD=1 must still reject: the day window did not roll over")
	}
}

// Under concurrent load the atomic check-and-reserve must not let more requests
// through than the limit, even though every caller reads the same zero
// persisted count. Run with -race.
func TestConcurrentAcquireDoesNotOverAdmit(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPM: 5}}

	const goroutines = 50
	var admitted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			if _, ok := l.Acquire(a); ok {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != 5 {
		t.Errorf("admitted %d requests, want exactly 5 (the RPM ceiling)", admitted)
	}
}

// Releasing a lease that never settled must free the reservation and record no
// usage, so the freed slot is admissible again and nothing was counted.
func TestReleaseUnsettledLeaseFreesReservation(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPM: 1}}

	lease, ok := l.Acquire(a)
	if !ok {
		t.Fatalf("first acquire rejected")
	}
	if l.Admit(a) {
		t.Fatalf("the in-flight lease should occupy the single RPM slot")
	}

	lease.Release()

	if !l.Admit(a) {
		t.Errorf("releasing the unsettled lease should free the slot")
	}
	if reqs, _ := l.bucket(QuotaKey("p", "m", 1), minuteWindow, minuteWindowStart(l.clock())); reqs != 0 {
		t.Errorf("release must not record usage, got %d persisted requests", reqs)
	}
}

// Headroom falls monotonically from 1 toward the floor as a window fills, and
// bottoms out exactly at the floor when the window is spent.
func TestHeadroomFallsMonotonicallyToFloor(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	const rpm = 10
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPM: rpm}}
	keys := []int64{1}

	if h := l.Headroom("p", "m", keys, a.Limits); h != 1 {
		t.Fatalf("empty window headroom = %v, want 1", h)
	}

	// used fraction rises 0.1..1.0; the ramp holds at 1 until 0.2 remaining,
	// then descends to the floor, so the sequence must never rise.
	headrooms := make([]float64, 0, rpm)
	for filled := 1; filled <= rpm; filled++ {
		quotaSettle(t, l, a, 0)
		l.invalidateSnapshot()
		headrooms = append(headrooms, l.Headroom("p", "m", keys, a.Limits))
	}
	for i := 1; i < len(headrooms); i++ {
		if headrooms[i] > headrooms[i-1]+1e-9 {
			t.Fatalf("headroom rose from %v to %v at fill %d", headrooms[i-1], headrooms[i], i+1)
		}
	}
	if last := headrooms[len(headrooms)-1]; last != headroomFloor {
		t.Errorf("headroom at a full window = %v, want floor %v", last, headroomFloor)
	}
	// A near-full window (9/10) must sit strictly between floor and 1 - the
	// ramp moves gradually, it does not step from 1 to the floor.
	if mid := headrooms[8]; mid <= headroomFloor || mid >= 1 {
		t.Errorf("headroom near-full = %v, want strictly between floor and 1", mid)
	}
}

// A provider-stated limit fills an unknown ceiling and then only ever tightens
// it: a higher later figure is ignored, a lower one wins.
func TestLearnLimitOverridesGuessThenOnlyTightens(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	if _, err := l.db.Exec(
		`INSERT INTO models (id, platform, model_id, display_name)
		 VALUES (1, 'groq', 'm', 'M')`,
	); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	learned, ok := l.LearnLimitFromError(1, "Rate limit reached: requests per minute (RPM): Limit 20")
	if !ok || learned.Kind != "rpm" || learned.Limit != 20 {
		t.Fatalf("first learn = %+v ok=%v, want rpm/20", learned, ok)
	}
	if got := quotaModelLimit(t, l, "rpm_limit"); got != 20 {
		t.Fatalf("rpm_limit = %d after learning a NULL, want 20", got)
	}

	if _, ok := l.LearnLimitFromError(1, "requests per minute (RPM): Limit 50"); ok {
		t.Errorf("a higher limit must not overwrite a lower learned one")
	}
	if got := quotaModelLimit(t, l, "rpm_limit"); got != 20 {
		t.Errorf("rpm_limit = %d, want 20 (unchanged by the higher figure)", got)
	}

	if _, ok := l.LearnLimitFromError(1, "requests per minute (RPM): Limit 8"); !ok {
		t.Errorf("a lower limit should tighten the ceiling")
	}
	if got := quotaModelLimit(t, l, "rpm_limit"); got != 8 {
		t.Errorf("rpm_limit = %d, want 8 after tightening", got)
	}
}

func quotaModelLimit(t *testing.T, l *Ledger, column string) int64 {
	t.Helper()
	var v int64
	if err := l.db.QueryRow("SELECT " + column + " FROM models WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return v
}

// Persisted counters survive a restart: reopening the same database must not
// hand back quota the provider has already counted.
func TestCountersSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 1, 15, 12, 0, 30, 0, time.UTC)
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1}

	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	l := NewLedger(st.DB())
	l.clock = func() time.Time { return at }
	for range 3 {
		quotaSettle(t, l, a, 100)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	l2 := NewLedger(st2.DB())
	l2.clock = func() time.Time { return at }

	reqs, tokens := l2.bucket(QuotaKey("p", "m", 1), minuteWindow, minuteWindowStart(at))
	if reqs != 3 || tokens != 300 {
		t.Errorf("after restart minute bucket = (%d req, %d tok), want (3, 300)", reqs, tokens)
	}
	// And the gate must still see the spent budget.
	blocked := a
	blocked.Limits = WindowLimits{RPD: 3}
	if l2.Admit(blocked) {
		t.Errorf("RPD=3 should stay exhausted across the restart")
	}
}

// A provider-wide daily request cap is one allowance shared by every model on
// the account+key: two different models draw down the same bucket, and a
// different key has its own.
func TestProviderPoolCapSharedAcrossModels(t *testing.T) {
	t.Setenv("PROVIDER_DAILY_REQUEST_CAP_TESTP", "2")
	l, _ := newQuotaTestLedger(t)

	modelA := Admission{Platform: "testp", ModelID: "a", KeyID: 1}
	modelB := Admission{Platform: "testp", ModelID: "b", KeyID: 1}

	quotaSettle(t, l, modelA, 0) // pool now 1/2
	if !l.Admit(modelB) {
		t.Fatalf("a different model on the same key should still fit the pool at 1/2")
	}
	quotaSettle(t, l, modelB, 0) // pool now 2/2, spent by two different models
	if l.Admit(modelA) {
		t.Errorf("the shared pool is spent, so model a must be rejected too")
	}

	otherKey := Admission{Platform: "testp", ModelID: "a", KeyID: 2}
	if !l.Admit(otherKey) {
		t.Errorf("a different key has its own pool and must not be blocked")
	}
}

// The per-model day window slides: a request stays counted until it is a full
// 24h old, so a bucket exactly 24h old still blocks (the case midnight
// alignment got wrong) while one 26h old no longer does.
func TestModelDayWindowIsTrailing24h(t *testing.T) {
	l, fc := newQuotaTestLedger(t) // 2026-01-15 12:00:30Z
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPD: 1}}

	quotaSettle(t, l, a, 0)

	fc.Advance(24 * time.Hour)
	if l.Admit(a) {
		t.Errorf("a request exactly 24h old must still count toward the day limit")
	}

	fc.Advance(2 * time.Hour) // 26h after the request
	if !l.Admit(a) {
		t.Errorf("a request 26h old must have aged out of the trailing window")
	}
}

// At one instant just after midnight, the provider pool cap has reset (it is
// keyed on the wall-clock day) while the per-model window has not (it slides):
// the same request that no longer counts against the account still counts
// against the model.
func TestProviderPoolResetsAtMidnightModelDoesNot(t *testing.T) {
	t.Setenv("PROVIDER_DAILY_REQUEST_CAP_POOLP", "1")
	l, fc := newQuotaTestLedger(t)            // 2026-01-15 12:00:30Z
	fc.Advance(11*time.Hour + 30*time.Minute) // 2026-01-15 23:30:30Z

	spend := Admission{Platform: "poolp", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPD: 1}}
	quotaSettle(t, l, spend, 0) // pool 1/1 (15th), model hour bucket at 23:00

	fc.Advance(time.Hour) // 2026-01-16 00:30:30Z - past midnight

	poolOnly := Admission{Platform: "poolp", ModelID: "m", KeyID: 1}
	if !l.Admit(poolOnly) {
		t.Errorf("the provider pool cap should reset at UTC midnight")
	}
	modelGate := Admission{Platform: "poolp", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPD: 1}}
	if l.Admit(modelGate) {
		t.Errorf("the per-model window slides, so it must still be spent after midnight")
	}
}

// A live request can legitimately run for minutes - Perplexity's deep-research
// bound is 300s - so an in-flight lease must outlast every supported provider
// timeout. At three minutes, past the reference's old 2-minute backstop but
// well inside a real attempt, the reservation must still hold its slot; only
// after the true backstop does a genuinely leaked lease get reclaimed.
func TestLiveLeaseOutlastsLongestProviderTimeout(t *testing.T) {
	l, fc := newQuotaTestLedger(t)
	a := Admission{Platform: "p", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPM: 1}}

	lease, ok := l.Acquire(a)
	if !ok {
		t.Fatalf("first acquire rejected")
	}
	_ = lease // deliberately never settled: it models a still-in-flight attempt

	fc.Advance(3 * time.Minute)
	if l.Admit(a) {
		t.Fatalf("a lease from a still-in-flight attempt was pruned after 3m; it must outlast every provider timeout (Perplexity 300s)")
	}

	fc.Advance(4 * time.Minute) // 7m total, past leaseMaxAge
	if !l.Admit(a) {
		t.Fatalf("the backstop must reclaim a lease that outlived every attempt")
	}
}

// NavyAI meters one shared token pool across every model on a key, but each
// model bills at its own multiplier. The pool total must sum each in-flight
// lease at ITS OWN multiplier, and the candidate's multiplier must touch only
// the new estimate - never be retro-applied to the already-billed pool.
func TestPoolTokenCapBillsEachLeaseAtItsOwnMultiplier(t *testing.T) {
	l, _ := newQuotaTestLedger(t) // navy's default token cap is 150K/day
	acquire := func(model string, mult float64, tokens int64) {
		if _, ok := l.Acquire(Admission{
			Platform: "navy", ModelID: model, KeyID: 1,
			EstimatedTokens: tokens, TokenMultiplier: mult,
		}); !ok {
			t.Fatalf("acquire %s rejected", model)
		}
	}
	acquire("m3x", 3.0, 20000) // bills 60K
	acquire("m1x", 1.0, 20000) // bills 20K
	// The pool already draws 80K of the 150K cap. The old code summed the pool
	// RAW (40K) and applied the candidate's multiplier to all of it.

	// True drain of an 80K-at-1x candidate is 60K+20K+80K = 160K > 150K, so it
	// must be refused. The old code read the pool as 40K and admitted at 120K.
	over := Admission{Platform: "navy", ModelID: "mnew", KeyID: 1, EstimatedTokens: 80000, TokenMultiplier: 1.0}
	if l.Admit(over) {
		t.Fatalf("candidate drives the pool to 160K > 150K; each lease must bill at its own multiplier")
	}

	// A candidate that fills the cap exactly (80K + 70K = 150K, rejected only
	// strictly above) is still admissible.
	fits := Admission{Platform: "navy", ModelID: "mnew", KeyID: 1, EstimatedTokens: 70000, TokenMultiplier: 1.0}
	if !l.Admit(fits) {
		t.Fatalf("a candidate that fills the cap exactly must still admit")
	}

	// The candidate's own multiplier applies ONLY to its new estimate. A 3x
	// candidate estimating 20K bills 60K atop the pool's 80K = 140K, which
	// fits. The old code applied the 3x to the whole 40K raw pool (120K) and
	// wrongly refused it.
	candidate3x := Admission{Platform: "navy", ModelID: "mnew", KeyID: 1, EstimatedTokens: 20000, TokenMultiplier: 3.0}
	if !l.Admit(candidate3x) {
		t.Fatalf("the candidate multiplier must apply only to its new estimate, not the billed pool")
	}
}

// forceSettleInAcquireWindow reproduces the settle-in-the-gap race
// deterministically. It acquires `first` as a still-in-flight lease, then runs
// Acquire for `victim` with a hook that settles the first lease in the exact
// gap between the victim's lock-free persisted snapshot and its admission.
// That interleaving is what once made the settled request invisible on BOTH
// axes: the snapshot predates its persisted write and the live-lease map no
// longer holds it, so a correct-looking admission handed back headroom the
// provider had already counted. It returns whether the victim was admitted.
func forceSettleInAcquireWindow(t *testing.T, l *Ledger, first Admission, firstTokens int64, victim Admission) bool {
	t.Helper()
	firstLease, ok := l.Acquire(first)
	if !ok {
		t.Fatalf("seed acquire rejected")
	}
	var once sync.Once
	// Settle exactly once: the victim's generation re-check forces a retry, so
	// the hook is re-entered, and a second Settle would be a no-op anyway.
	l.afterSnapshotHook = func() { once.Do(func() { firstLease.Settle(firstTokens) }) }
	defer func() { l.afterSnapshotHook = nil }()
	lease, admitted := l.Acquire(victim)
	if lease != nil {
		lease.Release()
	}
	return admitted
}

// When a lease settles between Acquire's lock-free snapshot and its admission,
// the freshly consumed quota is briefly invisible on both the persisted and the
// in-flight axis. The generation re-check must catch that and re-read, so the
// admitting caller sees the spent budget on every gate: the per-model request
// window, the per-model token window, and the shared provider pool cap.
func TestAcquireSeesQuotaSettledInSnapshotWindow(t *testing.T) {
	t.Run("request window", func(t *testing.T) {
		l, _ := newQuotaTestLedger(t)
		first := Admission{Platform: "racereq", ModelID: "m", KeyID: 1, Limits: WindowLimits{RPD: 1}}
		victim := first
		if forceSettleInAcquireWindow(t, l, first, 0, victim) {
			t.Fatalf("RPD=1 was spent by the settled request; the racing acquire must not over-admit")
		}
		if reqs, _ := l.modelDaySum(QuotaKey("racereq", "m", 1), l.clock()); reqs != 1 {
			t.Fatalf("settled request not persisted: day requests = %d, want 1", reqs)
		}
	})

	t.Run("token window", func(t *testing.T) {
		l, _ := newQuotaTestLedger(t)
		first := Admission{Platform: "racetok", ModelID: "m", KeyID: 1, Limits: WindowLimits{TPD: 100}}
		victim := Admission{Platform: "racetok", ModelID: "m", KeyID: 1, EstimatedTokens: 1, Limits: WindowLimits{TPD: 100}}
		if forceSettleInAcquireWindow(t, l, first, 100, victim) {
			t.Fatalf("TPD=100 was filled by the settled request; a 1-token estimate must push past it, not over-admit")
		}
		if _, tokens := l.modelDaySum(QuotaKey("racetok", "m", 1), l.clock()); tokens != 100 {
			t.Fatalf("settled tokens not persisted: day tokens = %d, want 100", tokens)
		}
	})

	t.Run("provider pool cap", func(t *testing.T) {
		t.Setenv("PROVIDER_DAILY_REQUEST_CAP_RACEPOOL", "1")
		l, _ := newQuotaTestLedger(t)
		// Different models on the same key share one pool, so the pool axis is
		// what rejects - not any per-model window.
		first := Admission{Platform: "racepool", ModelID: "a", KeyID: 1}
		victim := Admission{Platform: "racepool", ModelID: "b", KeyID: 1}
		if forceSettleInAcquireWindow(t, l, first, 0, victim) {
			t.Fatalf("the shared pool cap of 1 was spent by the settled request; the racing acquire must see it")
		}
		if reqs, _ := l.bucket(PoolKey("racepool", 1), dayWindow, dayWindowStart(l.clock())); reqs != 1 {
			t.Fatalf("settled pool request not persisted: pool day requests = %d, want 1", reqs)
		}
	})
}
