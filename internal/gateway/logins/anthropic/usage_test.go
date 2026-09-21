package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// pointUsageAt redirects FetchUsage at a stub for the duration of a test.
func pointUsageAt(t *testing.T, u string) {
	t.Helper()
	prev := usageEndpoint
	usageEndpoint = u
	t.Cleanup(func() { usageEndpoint = prev })
}

func TestFetchUsageAppliesOAuthContractAndParsesWindows(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 12.5, "resets_at": "2026-05-03T06:50Z"},
			"seven_day": {"utilization": 70, "resets_at": "2026-05-04T00:00Z"},
			"seven_day_opus": null
		}`))
	}))
	t.Cleanup(server.Close)
	pointUsageAt(t, server.URL)

	usage, err := FetchUsage(context.Background(), "sk-ant-oat-usage")
	require.NoError(t, err)

	// The Claude OAuth wire contract must be present so Anthropic answers at all.
	require.Equal(t, "Bearer sk-ant-oat-usage", got.Get("Authorization"))
	require.Contains(t, got.Get("anthropic-beta"), oauthBeta)
	require.Contains(t, got.Get("User-Agent"), "claude-cli/")
	require.Empty(t, got.Get("X-Api-Key"))

	require.NotNil(t, usage.Session5h())
	require.Equal(t, 12.5, usage.Session5h().Utilization)
	require.Equal(t, "2026-05-03T06:50Z", usage.Session5h().ResetsAt)
	require.NotNil(t, usage.Weekly())
	require.Equal(t, float64(70), usage.Weekly().Utilization)
	require.Empty(t, usage.ScopedWeekly())
}

func TestFetchUsageReadsLiveLimitsSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The live schema: legacy per-model buckets are null and the windows
		// arrive in limits[]. The 100%/is_active:true Fable row and the 77%/
		// is_active:false Sonnet scoped row must BOTH be reported - is_active
		// marks only the currently-binding limit, not a bucket's existence.
		_, _ = w.Write([]byte(`{
			"five_hour": null,
			"seven_day": null,
			"seven_day_opus": null,
			"seven_day_sonnet": null,
			"limits": [
				{"kind": "session", "percent": 5, "resets_at": "2026-05-03T06:50Z", "is_active": true},
				{"kind": "weekly_all", "percent": 22, "resets_at": "2026-05-04T00:00Z", "is_active": false},
				{"kind": "weekly_scoped", "percent": 100, "resets_at": "2026-05-04T00:00Z", "is_active": true, "scope": {"model": {"display_name": "Fable"}}},
				{"kind": "weekly_scoped", "percent": 77, "resets_at": "2026-05-04T00:00Z", "is_active": false, "scope": {"model": {"display_name": "Sonnet"}}}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	pointUsageAt(t, server.URL)

	usage, err := FetchUsage(context.Background(), "sk-ant-oat-usage")
	require.NoError(t, err)

	require.NotNil(t, usage.Session5h(), "5h session falls back to the limits[] session row")
	require.Equal(t, float64(5), usage.Session5h().Utilization)
	require.NotNil(t, usage.Weekly(), "7d all falls back to the limits[] weekly_all row")
	require.Equal(t, float64(22), usage.Weekly().Utilization)

	scoped := usage.ScopedWeekly()
	require.Len(t, scoped, 2, "every scoped weekly row is reported regardless of is_active")
	byLabel := map[string]float64{}
	for _, s := range scoped {
		byLabel[s.Label] = s.Utilization
	}
	require.Equal(t, float64(100), byLabel["Fable"])
	require.Equal(t, float64(77), byLabel["Sonnet"], "is_active:false must not hide a scoped row")
}

func TestFetchUsageErrorEnvelopeIsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The endpoint answers 200 with this shape when the token is stale.
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth authentication is currently not supported."}}`))
	}))
	t.Cleanup(server.Close)
	pointUsageAt(t, server.URL)

	_, err := FetchUsage(context.Background(), "sk-ant-oat-usage")
	require.Error(t, err)
	require.Contains(t, err.Error(), "OAuth authentication is currently not supported")
}

func TestFetchUsageRejectsMissingToken(t *testing.T) {
	_, err := FetchUsage(context.Background(), "  ")
	require.Error(t, err)
}

func TestFetchUsageDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":99}}`))
	}))
	t.Cleanup(leak.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", leak.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	pointUsageAt(t, server.URL)

	_, err := FetchUsage(context.Background(), "sk-ant-oat-usage")
	require.Error(t, err, "a 302 must surface as an error, not a followed redirect")
	require.False(t, leaked, "the bearer must never reach a redirect target")
}

// TestClaudeAuthorizationURLRequestsAuthorizationCode is the stale-binary guard
// Main asked for: the generated Claude authorization URL must carry
// response_type=code (and Claude's code=true), read from the real Start path so
// no URL logic is duplicated here.
func TestClaudeAuthorizationURLRequestsAuthorizationCode(t *testing.T) {
	flow, err := Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })

	parsed, err := url.Parse(flow.URL)
	require.NoError(t, err)
	require.Equal(t, "code", parsed.Query().Get("response_type"))
	require.Equal(t, "true", parsed.Query().Get("code"))
}
