package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFreeTierGroupsPoolsWithScaledBudgetAndQuota: two models on one platform
// collapse into a single provider pool, the documented budget scales by usable
// keys, and the pool carries the provider's observed token headroom.
func TestFreeTierGroupsPoolsWithScaledBudgetAndQuota(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)
	db := s.engine.DB()
	now := time.Now().Unix()

	_, err := db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('groq','x','x','x','healthy',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.1-8b','Llama 3.1 8B','~120M',1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.3-70b','Llama 3.3 70B','~30M',1)`)
	require.NoError(t, err)
	reset := now + 3600
	// Pool key is the credential pool (platform/keyId), the way Prowl records
	// observed quota.
	_, err = db.Exec(`INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_limit, tokens_remaining, resets_at, observed_at)
		VALUES('groq/1','groq',1000000,250000,?,?)`, reset, now)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/free-tier", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out freeTierResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	require.Len(t, out.Pools, 1, "both models share one groq pool")

	pool := out.Pools[0]
	require.Equal(t, "groq::account", pool.PoolKey)
	require.Equal(t, "groq", pool.Platform)
	require.Equal(t, 2, pool.ModelCount)
	require.ElementsMatch(t, []string{"llama-3.1-8b", "llama-3.3-70b"}, pool.MemberModelIDs)
	require.Equal(t, "documented", pool.Kind)
	require.Equal(t, "~120M", pool.BestLabel, "the largest documented label wins")
	require.Equal(t, 1, pool.KeyCount)
	require.Equal(t, float64(120_000_000), pool.DocumentedBudget, "budget is the pool max scaled by one usable key")

	require.NotNil(t, pool.Quota)
	require.Equal(t, "tokens", pool.Quota.Metric, "a token axis is preferred over a request counter")
	require.NotNil(t, pool.Quota.Limit)
	require.Equal(t, int64(1_000_000), *pool.Quota.Limit)
	require.NotNil(t, pool.Quota.Remaining)
	require.Equal(t, int64(250_000), *pool.Quota.Remaining)
	require.NotNil(t, pool.Quota.ResetAt)
	require.Equal(t, 1, pool.Quota.KeyCount)

	require.Equal(t, 1, out.Summary.PoolCount)
	require.Equal(t, float64(120_000_000), out.Summary.DocumentedMonthlyTokens)
	require.Zero(t, out.Summary.CreditsBasedPools)
	require.Zero(t, out.Summary.UnpublishedPools)
}

// TestFreeTierQuotaNullsAndAbsenceAreHonest: an axis a provider does not publish
// stays null instead of reading as zero, and a pool with no observation at all
// carries a null quota rather than a zeroed object.
func TestFreeTierQuotaNullsAndAbsenceAreHonest(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)
	db := s.engine.DB()
	now := time.Now().Unix()

	// groq: a remaining reading with no published limit.
	_, err := db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('groq','x','x','x','healthy',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.1-8b','Llama 3.1 8B','~120M',1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_limit, tokens_remaining, resets_at, observed_at)
		VALUES('groq/1','groq',NULL,500000,NULL,?)`, now)
	require.NoError(t, err)

	// cerebras: a usable key and a model, but no observation yet.
	_, err = db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('cerebras','x','x','x','healthy',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('cerebras','llama-3.3-70b','Llama 3.3 70B','~5M',1)`)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/free-tier", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out freeTierResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)

	byKey := map[string]freeTierPool{}
	for _, p := range out.Pools {
		byKey[p.PoolKey] = p
	}

	groq, ok := byKey["groq::account"]
	require.True(t, ok)
	require.NotNil(t, groq.Quota)
	require.Nil(t, groq.Quota.Limit, "an unpublished limit must stay null, never zero")
	require.NotNil(t, groq.Quota.Remaining)
	require.Equal(t, int64(500000), *groq.Quota.Remaining)
	require.Nil(t, groq.Quota.ResetAt, "no reset published means null")

	cerebras, ok := byKey["cerebras::shared"]
	require.True(t, ok)
	require.Nil(t, cerebras.Quota, "no observation means a null quota, not a zeroed one")
}

// TestFreeTierZeroUsableKeysDoesNotInventBudget: a platform whose only enabled
// credential is unusable (invalid) still has a free tier to list, but with zero
// usable accounts there is no allowance to draw from, so the documented budget
// is zero rather than one fabricated account's worth.
func TestFreeTierZeroUsableKeysDoesNotInventBudget(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)
	db := s.engine.DB()
	now := time.Now().Unix()

	_, err := db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('groq','x','x','x','invalid',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.1-8b','Llama 3.1 8B','~120M',1)`)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/free-tier", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out freeTierResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	require.Len(t, out.Pools, 1, "an enabled credential still lists the pool")

	pool := out.Pools[0]
	require.Equal(t, "groq::account", pool.PoolKey)
	require.Equal(t, 0, pool.KeyCount, "an invalid key is not a usable account")
	require.Zero(t, pool.DocumentedBudget, "zero usable keys must not invent a single account's budget")
	require.Zero(t, out.Summary.DocumentedMonthlyTokens, "the summary must not sum a fabricated budget")
}

// TestCacheStatsZeroedNotAbsent: Prowl has no response cache, so the readout is
// present, disabled and zeroed. The Analytics page needs every field present to
// key its card on `enabled` without reading undefined counters.
func TestCacheStatsZeroedNotAbsent(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)

	resp, body := do(t, s, http.MethodGet, "/api/cache/stats", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	for _, field := range []string{
		"enabled", "entries", "totalHits", "estimatedRequestsSaved",
		"savedPromptTokens", "savedCompletionTokens", "lookupHits", "lookupMisses",
		"hitRate", "savedTokens",
	} {
		require.Contains(t, raw, field, "cache stats must be zeroed, not absent")
	}

	var stats cacheStats
	require.NoError(t, json.Unmarshal([]byte(body), &stats))
	require.False(t, stats.Enabled)
	require.Zero(t, stats.SavedTokens)
	require.Zero(t, stats.HitRate)
	require.Zero(t, stats.Entries)
}

// TestStatusRoutesRequireACredential: every status route answers a typed 401
// when there is no credential, and never with authentication_error, because
// this port has no session for that verdict to end.
func TestStatusRoutesRequireACredential(t *testing.T) {
	t.Parallel()
	s := bareServer(t)

	for _, path := range []string{
		"/api/free-tier", "/api/cache/stats",
	} {
		resp, body := do(t, s, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "path %s", path)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body), "path %s", path)
	}
}

// TestFreeTierExpiredWindowRefillsAndHidesPastReset proves the free-tier summary
// treats a stale observation the way KeyHeadroom and the provider-status
// surfaces do: a window past its reset has refilled, so with a known limit its
// remaining reads full - not the exhausted count last seen - and the past reset
// is never surfaced as the pool's next reset.
func TestFreeTierExpiredWindowRefillsAndHidesPastReset(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)
	db := s.engine.DB()
	now := time.Now().Unix()
	past := now - 3600

	_, err := db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('groq','x','x','x','healthy',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.1-8b','Llama 3.1 8B','~120M',1)`)
	require.NoError(t, err)
	// A token window fully spent (remaining 0) whose reset already passed: the
	// pool has refilled.
	_, err = db.Exec(`INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_limit, tokens_remaining, resets_at, observed_at)
		VALUES('groq/1','groq',1000000,0,?,?)`, past, past)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/free-tier", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out freeTierResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	require.Len(t, out.Pools, 1)

	q := out.Pools[0].Quota
	require.NotNil(t, q)
	require.Equal(t, "tokens", q.Metric)
	require.NotNil(t, q.Limit)
	require.Equal(t, int64(1_000_000), *q.Limit)
	require.NotNil(t, q.Remaining)
	require.Equal(t, int64(1_000_000), *q.Remaining, "a refilled window reads full, not the stale exhausted count")
	require.Nil(t, q.ResetAt, "a reset that already passed is not exposed as the pool's next reset")
}

// TestFreeTierExpiredWindowUnknownLimitOmitsRemaining proves a refilled window
// with no published limit contributes no remaining - the refilled amount is
// unknown, so the stale exhausted count must never surface.
func TestFreeTierExpiredWindowUnknownLimitOmitsRemaining(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	headers := settingsSession(t, s)
	db := s.engine.DB()
	now := time.Now().Unix()
	past := now - 3600

	_, err := db.Exec(`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES('groq','x','x','x','healthy',1,?)`, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO models(platform, model_id, display_name, monthly_token_budget, enabled)
		VALUES('groq','llama-3.1-8b','Llama 3.1 8B','~120M',1)`)
	require.NoError(t, err)
	// Tokens remaining published as spent, but NO limit, and the reset passed.
	_, err = db.Exec(`INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_remaining, resets_at, observed_at)
		VALUES('groq/1','groq',0,?,?)`, past, past)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/free-tier", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out freeTierResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	require.Len(t, out.Pools, 1)

	q := out.Pools[0].Quota
	require.NotNil(t, q)
	require.Equal(t, "tokens", q.Metric)
	require.Nil(t, q.Limit, "no limit was published")
	require.Nil(t, q.Remaining, "a refilled window with unknown limit omits the stale remaining")
	require.Nil(t, q.ResetAt)
}
