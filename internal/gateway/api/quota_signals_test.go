package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQuotaSignalsMatchTheViewsContract is the crash this shape fixes. The
// dashboard's Quota signals tab renders one row per metric and calls string
// methods on `observedAt`; it was handed the stored pool row, with both
// metrics in one object and Unix integers for the instants, so the tab threw
// "e.includes is not a function" and the error boundary took the whole page.
func TestQuotaSignalsMatchTheViewsContract(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)

	// A pool that reported both a request and a token window, which is what
	// Groq's headers actually carry.
	reset := time.Now().Add(time.Hour).Unix()
	_, err := s.engine.DB().Exec(`
		INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit,
			requests_remaining, tokens_limit, tokens_remaining, resets_at, observed_at)
		VALUES('groq/4', 'groq', 1000, 12, 8000, 900, ?, ?)`,
		reset, time.Now().Unix())
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/health", "", authed(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var payload struct {
		QuotaStates []struct {
			Platform     string  `json:"platform"`
			KeyID        int64   `json:"keyId"`
			QuotaPoolKey string  `json:"quotaPoolKey"`
			Metric       string  `json:"metric"`
			Limit        *int64  `json:"limit"`
			Remaining    *int64  `json:"remaining"`
			ResetAt      *string `json:"resetAt"`
			Source       string  `json:"source"`
			Confidence   float64 `json:"confidence"`
			ObservedAt   string  `json:"observedAt"`
			UpdatedAt    string  `json:"updatedAt"`
		} `json:"quotaStates"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))

	byMetric := map[string]int{}
	for _, row := range payload.QuotaStates {
		byMetric[row.Metric]++
		// The client calls string methods on these; a number here is the bug.
		require.NotEmpty(t, row.ObservedAt, "observedAt must be a date string")
		require.NotEmpty(t, row.UpdatedAt, "updatedAt must be a date string")
		require.Contains(t, row.ObservedAt, "-", "observedAt must parse as a date")
		require.Equal(t, "header", row.Source, "a header reading must say so")
		require.InDelta(t, 1.0, row.Confidence, 0.001)
		require.Equal(t, "groq", row.Platform)
		require.Equal(t, int64(4), row.KeyID, "the key id must come off the pool key")
		require.Equal(t, "groq/4", row.QuotaPoolKey)
		require.NotNil(t, row.ResetAt, "a provider-stated reset must be carried")
	}
	require.Equal(t, 1, byMetric["requests"], "the request window must be its own row")
	require.Equal(t, 1, byMetric["tokens"], "the token window must be its own row")
}

// TestQuotaSignalsOmitMetricsNoProviderReported keeps the view from showing a
// window that was never observed.
func TestQuotaSignalsOmitMetricsNoProviderReported(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)

	_, err := s.engine.DB().Exec(`
		INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit,
			requests_remaining, observed_at)
		VALUES('kilo/7', 'kilo', NULL, 0, ?)`, time.Now().Unix())
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/health", "", authed(tok))
	var payload struct {
		QuotaStates []struct {
			Metric  string  `json:"metric"`
			ResetAt *string `json:"resetAt"`
		} `json:"quotaStates"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Len(t, payload.QuotaStates, 1, "only the observed metric may appear")
	require.Equal(t, "requests", payload.QuotaStates[0].Metric)
	require.Nil(t, payload.QuotaStates[0].ResetAt, "an unobserved reset must stay absent")
}

// TestQuotaSignalsAttributeKeyIDAcrossWindowSuffix: a subscription window pool
// key is "platform/keyID::window". The signal must parse the key id past the
// "::window" suffix and name the credential, rather than dropping it to an
// unnamed zero, and label the share as credits rather than a request count.
func TestQuotaSignalsAttributeKeyIDAcrossWindowSuffix(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)

	res, err := s.engine.DB().Exec(
		`INSERT INTO api_keys(platform, encrypted_key, iv, auth_tag, status, enabled, label, created_at)
		 VALUES('anthropic','x','x','x','healthy',1,'Claude Max seat',?)`, time.Now().Unix())
	require.NoError(t, err)
	keyID, err := res.LastInsertId()
	require.NoError(t, err)

	reset := time.Now().Add(time.Hour).Unix()
	poolKey := fmt.Sprintf("anthropic/%d::5h", keyID)
	_, err = s.engine.DB().Exec(`
		INSERT INTO provider_quota_state(quota_pool_key, platform, requests_limit,
			requests_remaining, resets_at, observed_at)
		VALUES(?, 'anthropic', 100, 78, ?, ?)`, poolKey, reset, time.Now().Unix())
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/health", "", authed(tok))
	var payload struct {
		QuotaStates []struct {
			KeyID    int64   `json:"keyId"`
			KeyLabel *string `json:"keyLabel"`
			Metric   string  `json:"metric"`
		} `json:"quotaStates"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Len(t, payload.QuotaStates, 1)
	row := payload.QuotaStates[0]
	require.Equal(t, keyID, row.KeyID, "the key id must survive the ::window suffix")
	require.NotNil(t, row.KeyLabel, "the parsed id must name the credential")
	require.Equal(t, "Claude Max seat", *row.KeyLabel)
	require.Equal(t, "credits", row.Metric, "a subscription window is a credit share, not a request count")
}

// TestQuotaSignalsLabelHyperBodyBalance: Hyper publishes its remaining balance
// in the response body, stored on the tokens axis for want of a column. The
// signal must report it as a body-sourced credit balance, not a token window
// read from a rate-limit header.
func TestQuotaSignalsLabelHyperBodyBalance(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)

	_, err := s.engine.DB().Exec(`
		INSERT INTO provider_quota_state(quota_pool_key, platform, tokens_remaining, observed_at)
		VALUES('hyper/9', 'hyper', 56, ?)`, time.Now().Unix())
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/health", "", authed(tok))
	var payload struct {
		QuotaStates []struct {
			Platform  string `json:"platform"`
			Metric    string `json:"metric"`
			Source    string `json:"source"`
			Remaining *int64 `json:"remaining"`
			Limit     *int64 `json:"limit"`
		} `json:"quotaStates"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Len(t, payload.QuotaStates, 1)
	row := payload.QuotaStates[0]
	require.Equal(t, "hyper", row.Platform)
	require.Equal(t, "hypercredits", row.Metric, "a Hyper balance is a credit balance, not tokens")
	require.Equal(t, "body", row.Source, "the balance came from the response body, not a header")
	require.NotNil(t, row.Remaining)
	require.Equal(t, int64(56), *row.Remaining)
	require.Nil(t, row.Limit, "a running balance has no ceiling to render")
}
