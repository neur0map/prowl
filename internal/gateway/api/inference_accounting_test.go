package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReportedZeroUsageIsRecordedExact proves a provider that reports usage of
// zero prompt and zero completion tokens -- a real, if unusual, answer -- is
// recorded as exact, never unavailable. Before the explicit usage-known bit a
// reported zero/zero read identically to a provider that reported nothing, so
// the accounting surface could not tell "used nothing" from "we don't know".
func TestReportedZeroUsageIsRecordedExact(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"c","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	}))
	t.Cleanup(upstream.Close)
	seedRoute(t, s, "zero", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	var quality string
	var estimated bool
	var inTok, outTok int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT usage_quality, estimated, input_tokens, output_tokens
		   FROM requests ORDER BY id DESC LIMIT 1`).Scan(&quality, &estimated, &inTok, &outTok))
	require.Equal(t, "exact", quality, "a provider-reported zero usage is exact, not unavailable")
	require.False(t, estimated, "a reported count is never an estimate")
	require.Zero(t, inTok)
	require.Zero(t, outTok)
}

// TestUsageQualityHonoursProviderUsageKnownBit pins the three-state marker at
// the record boundary: a provider-reported count (UsageKnown) is exact even at
// zero, a call that captured no usage is unavailable, and a derived count stays
// estimated.
func TestUsageQualityHonoursProviderUsageKnownBit(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})
	ctx := t.Context()

	cases := []struct {
		name string
		log  RequestLog
		want string
	}{
		{"reported zero is exact",
			RequestLog{Platform: "p", ModelID: "zero", Outcome: "success", StatusCode: 200, UsageKnown: true},
			"exact"},
		{"missing usage is unavailable",
			RequestLog{Platform: "p", ModelID: "missing", Outcome: "success", StatusCode: 200},
			"unavailable"},
		{"derived count stays estimated",
			RequestLog{Platform: "p", ModelID: "est", Outcome: "success", StatusCode: 200, Estimated: true, OutputTokens: 4},
			"estimated"},
	}
	for _, tc := range cases {
		_, err := s.RecordRequest(ctx, tc.log)
		require.NoError(t, err, tc.name)
		var got string
		require.NoError(t, s.engine.DB().QueryRow(
			`SELECT usage_quality FROM requests WHERE model_id = ? ORDER BY id DESC LIMIT 1`,
			tc.log.ModelID).Scan(&got))
		require.Equal(t, tc.want, got, tc.name)
	}
}

// TestCustomEndpointsPersistDistinctScopeOnSuccess proves a served request
// records the endpoint it was served through, so two custom relays never
// collapse into one identity on the success path.
func TestCustomEndpointsPersistDistinctScopeOnSuccess(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	upA := newFakeUpstream(t, http.StatusOK, "from A")
	upB := newFakeUpstream(t, http.StatusOK, "from B")
	seedRoute(t, s, "epA", upA.URL, "chat-a", 1)
	seedRoute(t, s, "epB", upB.URL, "chat-b", 2)
	usePriorityOrder(t, s)

	respA, bodyA := postChat(t, s, machineKey,
		`{"model":"chat-a","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, respA.StatusCode, bodyA)
	respB, bodyB := postChat(t, s, machineKey,
		`{"model":"chat-b","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, respB.StatusCode, bodyB)

	scopeA := successScope(t, s, "chat-a")
	scopeB := successScope(t, s, "chat-b")
	require.Equal(t, normalizeBaseURL(upA.URL), scopeA, "the success row must record its own endpoint")
	require.Equal(t, normalizeBaseURL(upB.URL), scopeB)
	require.NotEqual(t, scopeA, scopeB, "two endpoints must persist distinct scopes")
}

// TestCustomEndpointsPersistDistinctScopeOnFailure covers the failure path with
// the hardest case: two endpoints serving the SAME model id, where only the
// endpoint scope tells the two failed requests apart. A failure row that lost
// the scope would attribute one relay's outage to the other.
func TestCustomEndpointsPersistDistinctScopeOnFailure(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	upC := newFakeUpstream(t, http.StatusInternalServerError, "")
	upD := newFakeUpstream(t, http.StatusInternalServerError, "")
	seedRoute(t, s, "epC", upC.URL, "m", 1)
	seedRoute(t, s, "epD", upD.URL, "m", 2)
	usePriorityOrder(t, s)

	scopeC, scopeD := normalizeBaseURL(upC.URL), normalizeBaseURL(upD.URL)

	// Endpoint C alone (D disabled), so the failing hop is unambiguously C.
	setModelEnabled(t, s, "m", scopeD, false)
	respC, _ := postChat(t, s, machineKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.GreaterOrEqual(t, respC.StatusCode, 400)
	require.Equal(t, scopeC, latestErrorScope(t, s), "the failure row must carry the endpoint that failed")

	// Endpoint D alone.
	setModelEnabled(t, s, "m", scopeC, false)
	setModelEnabled(t, s, "m", scopeD, true)
	respD, _ := postChat(t, s, machineKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.GreaterOrEqual(t, respD.StatusCode, 400)
	require.Equal(t, scopeD, latestErrorScope(t, s))

	require.NotEqual(t, scopeC, scopeD, "the two failure rows must carry distinct scopes")
}

func successScope(t *testing.T, s *Server, modelID string) string {
	t.Helper()
	var scope string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT endpoint_scope FROM requests WHERE model_id = ? AND outcome = 'success'
		 ORDER BY id DESC LIMIT 1`, modelID).Scan(&scope))
	return scope
}

func latestErrorScope(t *testing.T, s *Server) string {
	t.Helper()
	var scope string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT endpoint_scope FROM requests WHERE outcome = 'error' ORDER BY id DESC LIMIT 1`).Scan(&scope))
	return scope
}

func setModelEnabled(t *testing.T, s *Server, modelID, scope string, enabled bool) {
	t.Helper()
	_, err := s.engine.DB().Exec(
		`UPDATE models SET enabled = ? WHERE model_id = ? AND endpoint_scope = ?`,
		boolInt(enabled), modelID, scope)
	require.NoError(t, err)
}
