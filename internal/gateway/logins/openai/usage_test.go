package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// codexUsageBody is the shape verified live against the Codex backend: a weekly
// primary window at 95% and a null secondary.
const codexUsageBody = `{"plan_type":"prolite","rate_limit":{"allowed":true,"limit_reached":false,` +
	`"primary_window":{"used_percent":95,"limit_window_seconds":604800,"reset_after_seconds":459185,"reset_at":1790467355},` +
	`"secondary_window":null},"additional_rate_limits":[]}`

func TestFetchUsageParsesCodexWindows(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(codexUsageBody))
	}))
	t.Cleanup(server.Close)

	prev := usageBaseURL
	usageBaseURL = server.URL
	t.Cleanup(func() { usageBaseURL = prev })

	usage, err := FetchUsage(context.Background(), "codex-access-token")
	require.NoError(t, err)
	require.Equal(t, "prolite", usage.PlanType)
	require.Nil(t, usage.Secondary, "a null secondary_window stays nil, never a zero-value window")
	require.NotNil(t, usage.Primary)
	require.Equal(t, float64(95), usage.Primary.UsedPercent)
	require.Equal(t, int64(604800), usage.Primary.LimitWindowSeconds)
	require.Equal(t, time.Unix(1790467355, 0), usage.Primary.ResetAt)

	// The Codex backend only answers a request carrying its CLI's identity.
	require.Equal(t, "Bearer codex-access-token", got.Get("Authorization"))
	require.Equal(t, "prowl", got.Get("originator"))
	require.Equal(t, "codex_cli_rs/"+clientVersion, got.Get("User-Agent"))
}

func TestFetchUsageAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"token expired"}`))
	}))
	t.Cleanup(server.Close)

	prev := usageBaseURL
	usageBaseURL = server.URL
	t.Cleanup(func() { usageBaseURL = prev })

	_, err := FetchUsage(context.Background(), "stale-token")
	require.Error(t, err, "a 401 must surface as an error the Accounts screen can read as a re-link prompt")
	require.Contains(t, err.Error(), "401")
}
