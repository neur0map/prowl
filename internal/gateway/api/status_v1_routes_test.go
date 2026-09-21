package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// insertEnabledKey seeds one enabled credential row and returns its id, which
// is what the cooldown key suffix must match for a bench to be attributed to a
// platform.
func insertEnabledKey(t *testing.T, s *Server, platform, status string) int64 {
	t.Helper()
	res, err := s.engine.DB().Exec(
		`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		 VALUES(?, 'x','x','x', ?, 1, ?)`, platform, status, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func getV1Providers(t *testing.T, s *Server, headers map[string]string) (map[string]v1Provider, map[string]int) {
	t.Helper()
	resp, body := do(t, s, http.MethodGet, "/v1/providers", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	var out struct {
		Providers []v1Provider   `json:"providers"`
		Counts    map[string]int `json:"counts"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	byPlatform := map[string]v1Provider{}
	for _, p := range out.Providers {
		byPlatform[p.Platform] = p
	}
	return byPlatform, out.Counts
}

func getV1QuotaForecast(t *testing.T, s *Server, headers map[string]string) map[string]quotaForecastEntry {
	t.Helper()
	resp, body := do(t, s, http.MethodGet, "/v1/quota-forecast", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	var out struct {
		Pools []quotaForecastEntry `json:"pools"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %q", body)
	byPool := map[string]quotaForecastEntry{}
	for _, p := range out.Pools {
		byPool[p.Pool] = p
	}
	return byPool
}

// TestV1ProvidersStatusPrecedence pins the order the switch resolves a
// platform's status into: a live key masks a dead one, an unknown key is
// reported honestly rather than as broken, a dead-only platform is invalid,
// and a platform the operator disabled entirely is not reported at all.
func TestV1ProvidersStatusPrecedence(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	// A healthy key alongside an invalid one: the platform is usable.
	insertEnabledKey(t, s, "healthy-wins", "healthy")
	insertEnabledKey(t, s, "healthy-wins", "invalid")

	// Only an unknown key: not yet probed, not broken.
	insertEnabledKey(t, s, "only-unknown", "unknown")

	// Only a rejected key, whose stored rejection carries a credential that
	// must not reach the caller verbatim.
	badID := insertEnabledKey(t, s, "only-invalid", "invalid")
	_, err := s.engine.DB().Exec(
		`UPDATE api_keys SET last_health_error = ?, last_checked_at = ? WHERE id = ?`,
		"rejected: sk-leakyleakyleaky012345", time.Now().Unix(), badID)
	require.NoError(t, err)

	// A platform the operator turned off entirely.
	_, err = s.engine.DB().Exec(
		`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		 VALUES('disabled-only','x','x','x','healthy',0,?)`, time.Now().Unix())
	require.NoError(t, err)

	byPlatform, counts := getV1Providers(t, s, headers)

	require.Equal(t, "healthy", byPlatform["healthy-wins"].Status,
		"a healthy key must mask an invalid one on the same platform")
	require.Equal(t, "unknown", byPlatform["only-unknown"].Status)

	invalid := byPlatform["only-invalid"]
	require.Equal(t, "invalid", invalid.Status)
	require.NotEmpty(t, invalid.LastError, "an invalid provider must carry its reason")
	require.NotContains(t, invalid.LastError, "sk-leakyleakyleaky012345",
		"a credential in a stored health error must not reach the status surface")
	require.Contains(t, invalid.LastError, "[redacted-key]")

	_, listed := byPlatform["disabled-only"]
	require.False(t, listed, "a platform with no enabled key must not be reported")

	require.Equal(t, 1, counts["healthy"])
	require.Equal(t, 1, counts["unknown"])
	require.Equal(t, 1, counts["invalid"])
	require.Zero(t, counts["rate_limited"])
}

// TestV1ProvidersCooldownOverridesHealthAndPicksEarliestResume: benching every
// one of a platform's healthy keys turns it "rate_limited" (both keys here are
// benched, so no route remains), and the reported resume time is the earliest
// one - the moment a route frees up, not the last.
func TestV1ProvidersCooldownOverridesHealthAndPicksEarliestResume(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	early := insertEnabledKey(t, s, "cooling", "healthy")
	late := insertEnabledKey(t, s, "cooling", "healthy")

	// Bench the two keys for different durations. The provider's resume time
	// must track the earlier one.
	base := time.Now()
	s.engine.Cooldowns().Bench(cooldownKey("cooling", "model-b", late), 3*time.Hour, gateway.SourceAuthoritative)
	s.engine.Cooldowns().Bench(cooldownKey("cooling", "model-a", early), 1*time.Hour, gateway.SourceAuthoritative)

	byPlatform, counts := getV1Providers(t, s, headers)
	p := byPlatform["cooling"]

	require.Equal(t, "rate_limited", p.Status, "every healthy route benched leaves no route, so the platform is rate_limited")
	require.Equal(t, 1, counts["rate_limited"])
	require.Zero(t, counts["healthy"], "a benched platform is not counted healthy")

	require.NotEmpty(t, p.ResumeAt, "a rate-limited provider must report when it frees up")
	resume, err := time.Parse(time.RFC3339, p.ResumeAt)
	require.NoError(t, err, "resume_at must be an RFC3339 instant")
	require.True(t, resume.After(base.Add(30*time.Minute)) && resume.Before(base.Add(2*time.Hour)),
		"resume must track the earliest bench (~1h), not the latest (~3h): got %s", p.ResumeAt)
}

// TestV1ProvidersHealthyRouteSurvivesSingleBench: a bench is per (platform,
// model, key), so one benched credential must not flip a whole platform to
// rate_limited while another healthy credential can still serve. The platform
// stays healthy and reports no resume time.
func TestV1ProvidersHealthyRouteSurvivesSingleBench(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	benchedKey := insertEnabledKey(t, s, "mixed", "healthy")
	insertEnabledKey(t, s, "mixed", "healthy") // a second, un-benched healthy route

	s.engine.Cooldowns().Bench(cooldownKey("mixed", "model-a", benchedKey), time.Hour, gateway.SourceAuthoritative)

	byPlatform, counts := getV1Providers(t, s, headers)
	p := byPlatform["mixed"]

	require.Equal(t, "healthy", p.Status,
		"a single benched key must not mask a remaining healthy route")
	require.Empty(t, p.ResumeAt, "a healthy platform reports no resume time")
	require.Equal(t, 1, counts["healthy"])
	require.Zero(t, counts["rate_limited"])
}

// TestV1ProvidersResumeTracksUnavailableHealthySet: when a platform is
// rate_limited because its only healthy route is benched, resume_at is when
// THAT route frees up. An invalid key's cooldown lifting sooner restores no
// route, so it must not pull resume_at earlier.
func TestV1ProvidersResumeTracksUnavailableHealthySet(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	healthyKey := insertEnabledKey(t, s, "cooled", "healthy")
	invalidKey := insertEnabledKey(t, s, "cooled", "invalid")

	base := time.Now()
	// The invalid key frees sooner, but its return brings back no route.
	s.engine.Cooldowns().Bench(cooldownKey("cooled", "model-a", invalidKey), 10*time.Minute, gateway.SourceAuthoritative)
	s.engine.Cooldowns().Bench(cooldownKey("cooled", "model-b", healthyKey), 2*time.Hour, gateway.SourceAuthoritative)

	byPlatform, _ := getV1Providers(t, s, headers)
	p := byPlatform["cooled"]

	require.Equal(t, "rate_limited", p.Status,
		"the only healthy route is benched, so the platform is rate_limited")
	require.NotEmpty(t, p.ResumeAt)
	resume, err := time.Parse(time.RFC3339, p.ResumeAt)
	require.NoError(t, err)
	require.True(t, resume.After(base.Add(time.Hour)),
		"resume must track the benched healthy route (~2h), not the invalid key (~10m): got %s", p.ResumeAt)
}

// cooldownKey builds the (platform, model, keyID) key the bench engine parses
// its key id off of.
func cooldownKey(platform, model string, keyID int64) string {
	return platform + "/" + model + "/" + strconv.FormatInt(keyID, 10)
}

// TestV1ProvidersClampsAndTakesTightestHeadroom: an over-reported remaining is
// clamped to 100% rather than shown as >100, and when a platform reports
// several windows the tightest (smallest) is the one that binds.
func TestV1ProvidersClampsAndTakesTightestHeadroom(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	insertEnabledKey(t, s, "clamp", "healthy")
	insertEnabledKey(t, s, "tightest", "healthy")

	now := time.Now().Unix()
	// A provider that reports more remaining than its own limit.
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, observed_at)
		VALUES('clamp/1','clamp',1000,1500,?)`, now)
	// Two windows on one platform: 90% and 20%. The tightest binds.
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, observed_at)
		VALUES('tightest/1','tightest',1000,900,?)`, now)
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, observed_at)
		VALUES('tightest/2','tightest',1000,200,?)`, now)

	byPlatform, _ := getV1Providers(t, s, headers)

	clamp := byPlatform["clamp"]
	require.NotNil(t, clamp.RequestsRemainingPct, "a measured headroom must be reported")
	require.Equal(t, 100, *clamp.RequestsRemainingPct, "an over-reported remaining is clamped to 100")

	tightest := byPlatform["tightest"]
	require.NotNil(t, tightest.RequestsRemainingPct)
	require.Equal(t, 20, *tightest.RequestsRemainingPct, "the smallest window must bind")
}

// TestV1QuotaForecastLowBalanceThresholds walks the two independent low-balance
// rules and their exact boundaries: the absolute floor that applies only to
// windows large enough for "20 left" to be alarming, and the proportional
// threshold below it.
func TestV1QuotaForecastLowBalanceThresholds(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)
	now := time.Now().Unix()

	cases := []struct {
		pool   string
		limit  int64
		rem    int64
		lowBal bool
		why    string
	}{
		{"p-abs-floor", 200, 20, true, "at the absolute floor on a large-enough window"},
		{"p-abs-above", 200, 21, false, "one above the floor, and 21/200 is over 10%"},
		{"p-below-window", 199, 20, false, "20 left must not alarm on a window below the minimum"},
		{"p-pct-low", 1000, 99, true, "9.9% is below the proportional threshold"},
		{"p-pct-boundary", 1000, 100, false, "exactly 10% is not below the threshold"},
	}
	for _, c := range cases {
		mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, observed_at)
			VALUES(?,?,?,?,?)`, c.pool, "plat", c.limit, c.rem, now)
	}

	byPool := getV1QuotaForecast(t, s, headers)
	for _, c := range cases {
		entry, ok := byPool[c.pool]
		require.True(t, ok, "pool %s must be forecast", c.pool)
		require.Equal(t, c.lowBal, entry.LowBalance, "%s: %s", c.pool, c.why)
	}
}

// TestV1QuotaForecastClampsHeadroomAndUsed: an over-reported remaining yields a
// clamped 100% and a non-negative used, and a provider-stated reset is carried
// as an ISO instant with a positive countdown.
func TestV1QuotaForecastClampsHeadroomAndUsed(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)
	now := time.Now()
	reset := now.Add(time.Hour).Unix()

	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, resets_at, observed_at)
		VALUES('p-over','plat',1000,1500,?,?)`, reset, now.Unix())

	byPool := getV1QuotaForecast(t, s, headers)
	over := byPool["p-over"]

	require.NotNil(t, over.RemainingPct)
	require.Equal(t, 100, *over.RemainingPct, "over-limit remaining clamps to 100%")
	require.NotNil(t, over.Used)
	require.Equal(t, int64(0), *over.Used, "used must never go negative")
	require.NotNil(t, over.Remaining)
	require.Equal(t, int64(1500), *over.Remaining, "the raw remaining is reported as observed")

	require.NotNil(t, over.ResetAt, "a stated reset must be carried")
	require.NotNil(t, over.SecondsUntilReset, "a future reset must count down")
	require.Positive(t, *over.SecondsUntilReset)
}

// TestV1QuotaForecastExcludesTokenOnlyPools keeps the forecast to windows it
// can predict: only request counters are forecast, so a pool that published a
// token window but no request window is absent rather than shown with a null
// forecast.
func TestV1QuotaForecastExcludesTokenOnlyPools(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)
	now := time.Now().Unix()

	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_limit, tokens_remaining, observed_at)
		VALUES('tokens-only','plat',8000,4000,?)`, now)
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, observed_at)
		VALUES('has-requests','plat',1000,500,?)`, now)

	byPool := getV1QuotaForecast(t, s, headers)
	_, hasTokenOnly := byPool["tokens-only"]
	require.False(t, hasTokenOnly, "a token-only window must not be forecast")
	_, hasRequests := byPool["has-requests"]
	require.True(t, hasRequests, "a request window must be forecast")
}

// TestV1StatusRoutesRequireACredential closes both machine-readable status
// endpoints to unauthenticated callers, and - like the rest of this port -
// never with authentication_error, which the dashboard would read as a
// session-end.
func TestV1StatusRoutesRequireACredential(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	for _, path := range []string{"/v1/providers", "/v1/quota-forecast"} {
		resp, body := do(t, s, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s must be gated", path)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
			"%s refusal must not read as a session-end", path)
		require.NotContains(t, body, `"providers"`, "%s must not serve state unauthenticated", path)
		require.NotContains(t, body, `"pools"`, "%s must not serve state unauthenticated", path)
	}
}

func mustExec(t *testing.T, s *Server, query string, args ...any) {
	t.Helper()
	_, err := s.engine.DB().Exec(query, args...)
	require.NoError(t, err)
}

// insertCatalogModel seeds one enabled, platform-wide catalogue model (no
// key_id), which the router expands into a route per usable key of the platform.
func insertCatalogModel(t *testing.T, s *Server, platform, modelID string) {
	t.Helper()
	_, err := s.engine.DB().Exec(
		`INSERT INTO models(platform, model_id, display_name) VALUES(?,?,?)`,
		platform, modelID, modelID)
	require.NoError(t, err)
}

// insertBoundModel seeds one enabled model bound to a specific key - a custom
// relay model or an enrolled login's model - which routes only through that key.
func insertBoundModel(t *testing.T, s *Server, platform, modelID string, keyID int64, scope string) {
	t.Helper()
	_, err := s.engine.DB().Exec(
		`INSERT INTO models(platform, model_id, display_name, key_id, endpoint_scope) VALUES(?,?,?,?,?)`,
		platform, modelID, modelID, keyID, scope)
	require.NoError(t, err)
}

// insertLinkedKey seeds an enabled linked-login credential the way AddLinked
// does - a `link:<provider>` reference rather than ciphertext. Catalogue models
// must never expand onto it (only its own key-bound login rows route through
// it), so the suffix here is what the exclusion keys off.
func insertLinkedKey(t *testing.T, s *Server, provider, status string) int64 {
	t.Helper()
	res, err := s.engine.DB().Exec(
		`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, created_at)
		 VALUES(?, ?, '', '', ?, 1, ?)`, provider, "link:"+provider, status, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestV1ProvidersSingleKeyKeepsHealthyWhileASiblingModelRoutes: availability is
// over actual (model, key, endpoint) routes, so benching one model of a key that
// serves two must not hide the sibling model the same key still routes.
func TestV1ProvidersSingleKeyKeepsHealthyWhileASiblingModelRoutes(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	key := insertEnabledKey(t, s, "twomodel", "healthy")
	insertCatalogModel(t, s, "twomodel", "model-a")
	insertCatalogModel(t, s, "twomodel", "model-b")

	// Only model-a is benched on the single key; model-b is still a live route.
	s.engine.Cooldowns().Bench(cooldownKey("twomodel", "model-a", key), time.Hour, gateway.SourceAuthoritative)

	byPlatform, counts := getV1Providers(t, s, headers)
	p := byPlatform["twomodel"]

	require.Equal(t, "healthy", p.Status,
		"one model's cooldown must not hide a sibling the same key still serves")
	require.Empty(t, p.ResumeAt, "a platform with a live route reports no resume time")
	require.Equal(t, 1, counts["healthy"])
	require.Zero(t, counts["rate_limited"])
}

// TestV1ProvidersRateLimitedWhenEveryRouteBenched: when EVERY actual route of
// the platform's only key is benched, the platform is rate_limited and resume_at
// tracks the earliest route to free up.
func TestV1ProvidersRateLimitedWhenEveryRouteBenched(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	key := insertEnabledKey(t, s, "spent", "healthy")
	insertCatalogModel(t, s, "spent", "model-a")
	insertCatalogModel(t, s, "spent", "model-b")

	base := time.Now()
	s.engine.Cooldowns().Bench(cooldownKey("spent", "model-b", key), 3*time.Hour, gateway.SourceAuthoritative)
	s.engine.Cooldowns().Bench(cooldownKey("spent", "model-a", key), time.Hour, gateway.SourceAuthoritative)

	byPlatform, counts := getV1Providers(t, s, headers)
	p := byPlatform["spent"]

	require.Equal(t, "rate_limited", p.Status,
		"every route the key serves is benched, so the platform is rate_limited")
	require.Equal(t, 1, counts["rate_limited"])
	require.Zero(t, counts["healthy"])

	require.NotEmpty(t, p.ResumeAt)
	resume, err := time.Parse(time.RFC3339, p.ResumeAt)
	require.NoError(t, err)
	require.True(t, resume.After(base.Add(30*time.Minute)) && resume.Before(base.Add(2*time.Hour)),
		"resume tracks the earliest benched route (~1h), not the latest (~3h): got %s", p.ResumeAt)
}

// TestV1ProvidersRoutesOverCustomAndLinkedBoundModels: a route can be bound to a
// single key - a custom relay's model or an enrolled login's - by key_id rather
// than being platform-wide. Those bound routes must be counted the same way, so
// a benched custom model never hides a sibling relay model. A linked-login
// credential is authorised ONLY for its discovered, key-bound models: a shared
// catalogue model of its platform must not expand onto it (mirroring the
// router's expandChainKeys link: exclusion), so benching the login's sole bound
// route reads as rate_limited even with an un-benched catalogue model present.
func TestV1ProvidersRoutesOverCustomAndLinkedBoundModels(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	// A custom relay key carries its models by key_id at one endpoint. Benching
	// one of its two bound models leaves the other routable.
	customKey := insertEnabledKey(t, s, "custom", "healthy")
	insertBoundModel(t, s, "custom", "relay-a", customKey, "http://relay")
	insertBoundModel(t, s, "custom", "relay-b", customKey, "http://relay")
	s.engine.Cooldowns().Bench(cooldownKey("custom", "relay-a", customKey), time.Hour, gateway.SourceAuthoritative)

	// A linked login (a real link:<provider> credential) with one discovered,
	// key-bound model. A shared catalogue model of the same platform is seeded
	// and left un-benched: it must NOT be treated as a route for the link key,
	// so benching the sole bound route still leaves the platform rate_limited.
	linkKey := insertLinkedKey(t, s, "anthropic", "healthy")
	insertBoundModel(t, s, "anthropic", "claude-linked", linkKey, "linked:anthropic")
	insertCatalogModel(t, s, "anthropic", "claude-catalogue")
	base := time.Now()
	s.engine.Cooldowns().Bench(cooldownKey("anthropic", "claude-linked", linkKey), 90*time.Minute, gateway.SourceAuthoritative)

	byPlatform, _ := getV1Providers(t, s, headers)

	custom := byPlatform["custom"]
	require.Equal(t, "healthy", custom.Status,
		"a benched custom-relay model must not hide a sibling relay model on the same key")
	require.Empty(t, custom.ResumeAt)

	link := byPlatform["anthropic"]
	require.Equal(t, "rate_limited", link.Status,
		"a catalogue model must not expand onto a link: credential, so the login's only bound route being benched is rate_limited")
	require.NotEmpty(t, link.ResumeAt)
	resume, err := time.Parse(time.RFC3339, link.ResumeAt)
	require.NoError(t, err)
	require.True(t, resume.After(base.Add(time.Hour)),
		"resume tracks the benched linked route (~90m): got %s", link.ResumeAt)
}

// TestV1QuotaExpiredWindowReadsAsFullNotStaleZero: an observation whose reset
// has already passed has refilled, so both surfaces report it full - consistent
// with KeyHeadroom - rather than carrying the stale exhausted count forward.
func TestV1QuotaExpiredWindowReadsAsFullNotStaleZero(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	insertEnabledKey(t, s, "reset", "healthy")
	past := time.Now().Add(-time.Hour).Unix()
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, resets_at, observed_at)
		VALUES('reset/1','reset',1000,0,?,?)`, past, past)

	byPlatform, _ := getV1Providers(t, s, headers)
	p := byPlatform["reset"]
	require.NotNil(t, p.RequestsRemainingPct, "a window past its reset is measured, not omitted")
	require.Equal(t, 100, *p.RequestsRemainingPct,
		"a window past its reset reads as full, not the stale exhausted count")

	byPool := getV1QuotaForecast(t, s, headers)
	entry := byPool["reset/1"]
	require.NotNil(t, entry.Remaining)
	require.Equal(t, int64(1000), *entry.Remaining, "the refilled window reports its full budget")
	require.NotNil(t, entry.RemainingPct)
	require.Equal(t, 100, *entry.RemainingPct)
	require.False(t, entry.LowBalance, "a refilled window is not low balance")
	require.Nil(t, entry.SecondsUntilReset, "a passed reset has no countdown")
}

// TestV1QuotaUnexpiredExhaustedWindowStaysExhausted: an exhausted window whose
// reset is still in the future is live, not stale - it must report zero
// headroom and a low balance, so a caller does not route into an empty pool.
func TestV1QuotaUnexpiredExhaustedWindowStaysExhausted(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	insertEnabledKey(t, s, "drained", "healthy")
	now := time.Now()
	mustExec(t, s, `INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit, requests_remaining, resets_at, observed_at)
		VALUES('drained/1','drained',1000,0,?,?)`, now.Add(time.Hour).Unix(), now.Unix())

	byPlatform, _ := getV1Providers(t, s, headers)
	p := byPlatform["drained"]
	require.NotNil(t, p.RequestsRemainingPct)
	require.Equal(t, 0, *p.RequestsRemainingPct, "an unexpired exhausted window reports 0% headroom")

	byPool := getV1QuotaForecast(t, s, headers)
	entry := byPool["drained/1"]
	require.NotNil(t, entry.Remaining)
	require.Equal(t, int64(0), *entry.Remaining)
	require.NotNil(t, entry.RemainingPct)
	require.Equal(t, 0, *entry.RemainingPct)
	require.True(t, entry.LowBalance, "a spent, not-yet-reset window is low balance")
	require.NotNil(t, entry.SecondsUntilReset)
	require.Positive(t, *entry.SecondsUntilReset)
}
