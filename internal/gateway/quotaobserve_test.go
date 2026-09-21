package gateway

import (
	"database/sql"
	"math"
	"net/http"
	"testing"
)

func quotaStateRow(t *testing.T, l *Ledger, poolKey string) (reqLim, reqRem, tokLim, tokRem, resets sql.NullInt64, found bool) {
	t.Helper()
	err := l.db.QueryRow(
		`SELECT requests_limit, requests_remaining, tokens_limit, tokens_remaining, resets_at
		   FROM provider_quota_state WHERE quota_pool_key = ?`, poolKey,
	).Scan(&reqLim, &reqRem, &tokLim, &tokRem, &resets)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		t.Fatalf("read state %s: %v", poolKey, err)
	}
	found = true
	return
}

func quotaObsCount(t *testing.T, l *Ledger, poolKey string) int {
	t.Helper()
	var n int
	if err := l.db.QueryRow(
		`SELECT COUNT(*) FROM provider_quota_observations WHERE quota_pool_key = ?`, poolKey,
	).Scan(&n); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	return n
}

// Groq exposes separate request and token header families; both land in the
// pool's state and one raw observation is logged.
func TestObserveGroqHeaders(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "1000")
	h.Set("x-ratelimit-remaining-requests", "990")
	h.Set("x-ratelimit-limit-tokens", "50000")
	h.Set("x-ratelimit-remaining-tokens", "45000")

	obs, err := l.ObserveResponse("groq", "m", 7, 200, h)
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	if obs.Requests.Limit != 1000 || obs.Requests.Remaining != 990 {
		t.Errorf("requests observation = %+v", obs.Requests)
	}
	if obs.Tokens.Limit != 50000 || obs.Tokens.Remaining != 45000 {
		t.Errorf("tokens observation = %+v", obs.Tokens)
	}

	reqLim, reqRem, tokLim, tokRem, _, found := quotaStateRow(t, l, PoolKey("groq", 7))
	if !found {
		t.Fatalf("no state row written")
	}
	if reqLim.Int64 != 1000 || reqRem.Int64 != 990 || tokLim.Int64 != 50000 || tokRem.Int64 != 45000 {
		t.Errorf("state = %d/%d req, %d/%d tok", reqLim.Int64, reqRem.Int64, tokLim.Int64, tokRem.Int64)
	}
	if n := quotaObsCount(t, l, PoolKey("groq", 7)); n != 1 {
		t.Errorf("observation rows = %d, want 1", n)
	}
}

// Cerebras uses the day/minute-suffixed spelling; the request-day family maps
// to the request columns and the token-minute family to the token columns.
func TestObserveCerebrasDayMinuteSpelling(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests-day", "100")
	h.Set("x-ratelimit-remaining-requests-day", "90")
	h.Set("x-ratelimit-limit-tokens-minute", "8000")
	h.Set("x-ratelimit-remaining-tokens-minute", "7000")

	obs, err := l.ObserveResponse("cerebras", "m", 4, 200, h)
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	reqLim, reqRem, tokLim, tokRem, _, found := quotaStateRow(t, l, PoolKey("cerebras", 4))
	if !found {
		t.Fatalf("no state row written")
	}
	if reqLim.Int64 != 100 || reqRem.Int64 != 90 || tokLim.Int64 != 8000 || tokRem.Int64 != 7000 {
		t.Errorf("state = %d/%d req, %d/%d tok", reqLim.Int64, reqRem.Int64, tokLim.Int64, tokRem.Int64)
	}
}

// Septor's plain x-ratelimit-* triple, including a relative reset that becomes
// an absolute Unix timestamp.
func TestObserveSeptorRelativeReset(t *testing.T) {
	l, fc := newQuotaTestLedger(t)
	h := http.Header{}
	h.Set("x-ratelimit-limit", "60")
	h.Set("x-ratelimit-remaining", "55")
	h.Set("x-ratelimit-reset", "30") // 30 seconds from now

	obs, err := l.ObserveResponse("septor", "m", 3, 200, h)
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	if obs.Requests.Limit != 60 || obs.Requests.Remaining != 55 {
		t.Errorf("requests observation = %+v", obs.Requests)
	}
	wantReset := fc.Now().Unix() + 30
	if obs.ResetsAt != wantReset {
		t.Errorf("resets_at = %d, want %d", obs.ResetsAt, wantReset)
	}
	_, _, _, _, resets, found := quotaStateRow(t, l, PoolKey("septor", 3))
	if !found || resets.Int64 != wantReset {
		t.Errorf("state resets_at = %d (found=%v), want %d", resets.Int64, found, wantReset)
	}
}

// A provider that sends none of its rate-limit headers on a normal response
// leaves no trace: no state row, no observation, nil return. It must never be
// recorded as zero remaining.
func TestObserveAbsentHeadersNoObservation(t *testing.T) {
	l, _ := newQuotaTestLedger(t)

	obs, err := l.ObserveResponse("groq", "m", 7, 200, http.Header{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if obs != nil {
		t.Errorf("expected no observation, got %+v", obs)
	}
	if _, _, _, _, _, found := quotaStateRow(t, l, PoolKey("groq", 7)); found {
		t.Errorf("a state row was written for an absent-header response")
	}
	if n := quotaObsCount(t, l, PoolKey("groq", 7)); n != 0 {
		t.Errorf("observation rows = %d, want 0", n)
	}

	// A platform with no header spec at all behaves the same.
	if obs, _ := l.ObserveResponse("nosuchplatform", "m", 1, 200, http.Header{}); obs != nil {
		t.Errorf("unknown platform produced an observation")
	}
}

// A 429 is itself the signal that the request budget is spent, so it records
// remaining=0 even with no headers - that is a real observation, not an
// invented zero.
func TestObserve429RecordsExhaustion(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	obs, err := l.ObserveResponse("groq", "m", 7, 429, http.Header{})
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	if !obs.Requests.HasRemaining || obs.Requests.Remaining != 0 {
		t.Errorf("429 should record remaining=0, got %+v", obs.Requests)
	}
	_, reqRem, _, _, _, found := quotaStateRow(t, l, PoolKey("groq", 7))
	if !found || !reqRem.Valid || reqRem.Int64 != 0 {
		t.Errorf("state requests_remaining = %v (found=%v), want 0", reqRem, found)
	}
}

// KeyHeadroom reports each key's remaining fraction, worst metric wins, and a
// never-observed key is absent rather than zero.
func TestKeyHeadroomWorstMetric(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "1000")
	h.Set("x-ratelimit-remaining-requests", "200") // 0.20 - the binding one
	h.Set("x-ratelimit-limit-tokens", "50000")
	h.Set("x-ratelimit-remaining-tokens", "45000") // 0.90
	if _, err := l.ObserveResponse("groq", "m", 7, 200, h); err != nil {
		t.Fatalf("observe: %v", err)
	}

	m := l.KeyHeadroom("groq")
	if got := m[7]; math.Abs(got-0.2) > 1e-9 {
		t.Errorf("KeyHeadroom[7] = %v, want 0.2 (worst of the two metrics)", got)
	}
	if _, ok := m[8]; ok {
		t.Errorf("a never-observed key must be absent, not present")
	}
}

// AccountScoped mirrors the reference ::account classification, including
// OpenRouter's split between its per-model :free pool and its account pool.
func TestAccountScoped(t *testing.T) {
	l, _ := newQuotaTestLedger(t)
	cases := []struct {
		platform, model string
		want            bool
	}{
		{"github", "m", true},
		{"groq", "m", true},
		{"openrouter", "gpt:free", false},
		{"openrouter", "gpt", true},
		{"septor", "m", false},
		{"unknownxyz", "", true}, // default pool is ::account
	}
	for _, c := range cases {
		if got := l.AccountScoped(c.platform, c.model); got != c.want {
			t.Errorf("AccountScoped(%q,%q) = %v, want %v", c.platform, c.model, got, c.want)
		}
	}
}

// A pool metered on both requests and tokens can publish two different reset
// deadlines. The single schema field must retain the LATER one: a window whose
// reset has passed reads as full again, so keeping the earlier reset would let
// the still-spent later window read as replenished and over-admit. This holds
// whichever axis is later.
func TestSeparateResetDeadlinesRetainTheLater(t *testing.T) {
	l, fc := newQuotaTestLedger(t)
	now := fc.Now().Unix()

	// Token window resets later than the request window.
	ht := http.Header{}
	ht.Set("x-ratelimit-limit-requests", "1000")
	ht.Set("x-ratelimit-remaining-requests", "10")
	ht.Set("x-ratelimit-reset-requests", "30") // requests roll in 30s
	ht.Set("x-ratelimit-limit-tokens", "50000")
	ht.Set("x-ratelimit-remaining-tokens", "5")
	ht.Set("x-ratelimit-reset-tokens", "120") // tokens roll in 120s (later)

	obs, err := l.ObserveResponse("groq", "m", 7, 200, ht)
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	if want := now + 120; obs.ResetsAt != want {
		t.Fatalf("resets_at = %d, want the later token reset %d (collapsing to the earlier request reset over-admits the still-spent token window)", obs.ResetsAt, want)
	}
	if _, _, _, _, resets, found := quotaStateRow(t, l, PoolKey("groq", 7)); !found || resets.Int64 != now+120 {
		t.Fatalf("state resets_at = %d (found=%v), want %d", resets.Int64, found, now+120)
	}

	// Request window resets later than the token window: same rule, other axis.
	hr := http.Header{}
	hr.Set("x-ratelimit-limit-requests", "1000")
	hr.Set("x-ratelimit-remaining-requests", "10")
	hr.Set("x-ratelimit-reset-requests", "200") // requests roll in 200s (later)
	hr.Set("x-ratelimit-limit-tokens", "50000")
	hr.Set("x-ratelimit-remaining-tokens", "5")
	hr.Set("x-ratelimit-reset-tokens", "60") // tokens roll in 60s

	obs, err = l.ObserveResponse("groq", "m", 8, 200, hr)
	if err != nil || obs == nil {
		t.Fatalf("ObserveResponse = %v, %v", obs, err)
	}
	if want := now + 200; obs.ResetsAt != want {
		t.Fatalf("resets_at = %d, want the later request reset %d", obs.ResetsAt, want)
	}
}
