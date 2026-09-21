package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// fakeUpstream stands in for a provider. It counts calls so a test can prove
// failover actually moved rather than merely returning the right status.
type fakeUpstream struct {
	*httptest.Server
	calls atomic.Int64
}

// newFakeUpstream serves an OpenAI-shaped completion, or the given status.
func newFakeUpstream(t *testing.T, status int, content string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"upstream says %d"}}`, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"cmpl-1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`, content)
	}))
	t.Cleanup(f.Close)
	return f
}

// seedRoute registers a custom-endpoint key and model so the chain has a
// candidate pointing at a fake upstream. Custom endpoints are the only way to
// aim a real provider adapter at a test server.
func seedRoute(t *testing.T, s *Server, label, baseURL, modelID string, position int) int64 {
	t.Helper()
	ctx := context.Background()

	keyID, err := s.engine.Vault().Add("custom", "test-key-"+label, gateway.AddOptions{
		Label:   label,
		BaseURL: baseURL,
	})
	require.NoError(t, err)

	res, err := s.engine.DB().ExecContext(ctx,
		`INSERT INTO models (platform, model_id, display_name, key_id, endpoint_scope,
		                     size_label, intelligence_rank, enabled, supports_tools)
		 VALUES ('custom', ?, ?, ?, ?, 'Medium', 5, 1, 1)`,
		modelID, modelID, keyID, baseURL)
	require.NoError(t, err)
	modelDBID, err := res.LastInsertId()
	require.NoError(t, err)

	_, err = s.engine.DB().ExecContext(ctx,
		`INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, ?, 1)`,
		modelDBID, position)
	require.NoError(t, err)
	return modelDBID
}

// usePriorityOrder pins candidate order to the configured positions.
func usePriorityOrder(t *testing.T, s *Server) {
	t.Helper()
	_, err := s.engine.DB().Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES ('routing_strategy', 'priority', 0)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	require.NoError(t, err)
}

func postChat(t *testing.T, s *Server, key, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

const chatBody = `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`

// TestRoutesThroughTheEngineToAProvider is the end-to-end proof that the
// ported engine is actually wired: one request, resolved through the chain,
// admitted by the ledger, dispatched to a provider, relayed back.
func TestRoutesThroughTheEngineToAProvider(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "hi there")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey, chatBody)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), upstream.calls.Load(), "the provider must have been called exactly once")
	require.Contains(t, body, "hi there")
	require.NotEmpty(t, resp.Header.Get("X-Request-ID"))
	require.Contains(t, resp.Header.Get("X-Routed-Via"), "custom",
		"the caller must be told which provider served them")
	require.Equal(t, "general", resp.Header.Get("X-Prowl-Route-Class"))
	require.Equal(t, "simple", resp.Header.Get("X-Prowl-Route-Effort"))
	require.Contains(t, resp.Header.Get("X-Prowl-Route-Reason"), "strategy")
}

func TestSmartRoutingUsesPromptDomainAndExplainsItsChoice(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	general := newFakeUpstream(t, http.StatusOK, "general")
	coder := newFakeUpstream(t, http.StatusOK, "coder")
	generalID := seedRoute(t, s, "general", general.URL, "general-top", 1)
	coderID := seedRoute(t, s, "coder", coder.URL, "gpt-codex", 2)
	_, err := s.engine.DB().Exec(`
		INSERT INTO settings(key, value, updated_at) VALUES('routing_strategy', 'smartest', 0)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	require.NoError(t, err)
	now := time.Now().Unix()
	_, err = s.engine.DB().Exec(`
		INSERT INTO model_benchmarks
		  (model_db_id, source, source_model_id, source_model_name,
		   intelligence, coding, agentic, refreshed_at)
		VALUES
		  (?, 'Artificial Analysis', 'general', 'General', .99, .20, .20, ?),
		  (?, 'Artificial Analysis', 'coder', 'Coder', .70, .95, .85, ?)`,
		generalID, now, coderID, now)
	require.NoError(t, err)

	body := `{"model":"auto","messages":[{"role":"user","content":"Debug this Go repository stack trace, find the root cause, and implement the function."}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`
	resp, responseBody := postChat(t, s, machineKey, body)

	require.Equal(t, http.StatusOK, resp.StatusCode, responseBody)
	require.Equal(t, int64(0), general.calls.Load(), "the generic benchmark leader should lose a coding request")
	require.Equal(t, int64(1), coder.calls.Load())
	require.Equal(t, "coding", resp.Header.Get("X-Prowl-Route-Class"))
	require.Contains(t, resp.Header.Get("X-Prowl-Route-Reason"), "Artificial Analysis")
	require.Contains(t, resp.Header.Get("X-Prowl-Route-Reason"), "capability")
	require.Contains(t, resp.Header.Get("X-Prowl-Route-Reason"), "headroom")
	require.Contains(t, resp.Header.Get("X-Prowl-Route-Reason"), "quota")

	_, trailBody := do(t, s, http.MethodGet, "/api/usage/requests?limit=1", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	var trail struct {
		Requests []struct {
			Class  string `json:"class"`
			Effort string `json:"effort"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal([]byte(trailBody), &trail))
	require.Len(t, trail.Requests, 1)
	require.Equal(t, resp.Header.Get("X-Prowl-Route-Class"), trail.Requests[0].Class)
	require.Equal(t, resp.Header.Get("X-Prowl-Route-Effort"), trail.Requests[0].Effort)
}

// TestFailoverMovesToTheNextProvider proves the loop is driving real routes:
// the first upstream rejects, the second answers, and the caller never sees
// the failure.
func TestFailoverMovesToTheNextProvider(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	// A rate limit is scoped to the (model, key) that hit it, so the sibling
	// candidate stays eligible. A 5xx would instead skip the whole platform,
	// which TestProviderLevelFailureSkipsThePlatform covers.
	broken := newFakeUpstream(t, http.StatusTooManyRequests, "")
	working := newFakeUpstream(t, http.StatusOK, "second provider answered")
	seedRoute(t, s, "broken", broken.URL, "broken-model", 1)
	seedRoute(t, s, "working", working.URL, "working-model", 2)

	// Both candidates are unmeasured, so a score-based strategy would order
	// them arbitrarily and the test would assert an order it never
	// established. Priority makes position decide, which is what this test is
	// actually about: what happens AFTER the first choice fails.
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, machineKey, chatBody)

	require.Equal(t, http.StatusOK, resp.StatusCode, "a failing provider must not reach the caller: %s", body)
	require.Contains(t, body, "second provider answered")
	require.GreaterOrEqual(t, broken.calls.Load(), int64(1), "the first provider must have been tried")
	require.Equal(t, int64(1), working.calls.Load(), "the second must have served it")
	require.Equal(t, "1", resp.Header.Get("X-Fallback-Attempts"),
		"the caller must be told a hop was needed")
	require.NotEmpty(t, resp.Header.Get("X-Fallback-Trail"))
}

// TestProviderLevelFailureSkipsThePlatform documents a deliberate and
// initially surprising behaviour: a 5xx is evidence about the PROVIDER, not
// about one model or key, so the rest of that platform is abandoned for this
// request. Retrying a sibling model on a provider that is down would just
// spend the wall-clock budget discovering the same outage.
func TestCustomEndpointsFailOverIndependently(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	down := newFakeUpstream(t, http.StatusInternalServerError, "")
	sibling := newFakeUpstream(t, http.StatusOK, "the sibling answered")
	seedRoute(t, s, "down", down.URL, "down-model", 1)
	seedRoute(t, s, "sibling", sibling.URL, "sibling-model", 2)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, machineKey, chatBody)

	// A 5xx rules out a hosted platform, because every key of one talks to the
	// same endpoint. Custom endpoints are the exception this asserts: each key
	// is a DIFFERENT operator-supplied server, so one being down says nothing
	// about the others, and sweeping them made a self-hosted pool fail over
	// exactly once and then give up.
	//
	// The hosted-platform behaviour is covered where it can be exercised
	// honestly: errclass_test.go pins a 5xx as SkipPlatform, and chain_test.go
	// pins Eligible honouring SkipPlatforms.
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), down.calls.Load())
	require.Equal(t, int64(1), sibling.calls.Load(),
		"a second custom endpoint is a different server and must still be tried")
	require.Contains(t, body, "the sibling answered")
}

// TestEveryProviderFailingIsNotA500 pins the exhaustion taxonomy at the edge:
// the gateway must name what went wrong upstream rather than claim its own
// internal error.
func TestEveryProviderFailingIsNotA500(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	first := newFakeUpstream(t, http.StatusInternalServerError, "")
	second := newFakeUpstream(t, http.StatusBadGateway, "")
	seedRoute(t, s, "first", first.URL, "first-model", 1)
	seedRoute(t, s, "second", second.URL, "second-model", 2)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, machineKey, chatBody)

	require.NotEqual(t, http.StatusInternalServerError, resp.StatusCode,
		"an upstream failure is never the gateway's own 500: %s", body)
	require.GreaterOrEqual(t, resp.StatusCode, 400)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
		"an upstream failure must not sign the operator out")
}

// TestUnconfiguredGatewayExplainsItself is the first-run experience: with no
// keys, the answer must say what to do rather than fail opaquely.
func TestUnconfiguredGatewayExplainsItself(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	resp, body := postChat(t, s, machineKey, chatBody)

	require.GreaterOrEqual(t, resp.StatusCode, 400)

	// The contract is actionable guidance, not one particular word: the reply
	// must name the surface that fixes it.
	lower := strings.ToLower(body)
	require.True(t,
		strings.Contains(lower, "models page") || strings.Contains(lower, "keys page"),
		"an unconfigured gateway must point the user at where to fix it: %s", body)
}

// TestRequestTrailRecordsTheEvidence matters because the trail IS the
// reliability and speed evidence the bandit reads back. A served request that
// leaves no row would make the router permanently blind.
func TestRequestTrailRecordsTheEvidence(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "recorded")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var (
		platform, outcome string
		inTok, outTok     int
		latency           int64
	)
	err := s.engine.DB().QueryRow(
		`SELECT platform, outcome, input_tokens, output_tokens, latency_ms FROM requests ORDER BY id DESC LIMIT 1`,
	).Scan(&platform, &outcome, &inTok, &outTok, &latency)
	require.NoError(t, err, "a served request must leave evidence for the router to learn from")
	require.Equal(t, "custom", platform)
	require.Equal(t, "success", outcome)
	require.Equal(t, 11, inTok, "the reported prompt tokens are recorded as input")
	require.Equal(t, 7, outTok, "the reported completion tokens are recorded as output, not the summed total")
	require.GreaterOrEqual(t, latency, int64(0))
}

// TestModelsListsRoutingAliasesFirst keeps the aliases discoverable: they are
// the ids a client should normally use, because they are what lets the gateway
// choose at all.
func TestModelsListsRoutingAliasesFirst(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "x")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer "+machineKey)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	require.Equal(t, "list", parsed.Object)
	require.NotEmpty(t, parsed.Data)
	require.Equal(t, "auto", parsed.Data[0].ID, "the routing alias must lead the list")
}

// TestServedRequestFeedsTheRollupAndLifetimeCounters is why the relay writes
// through RecordRequest rather than a plain INSERT. Analytics reads the hourly
// rollup and the lifetime counters precisely so its charts survive the raw
// trail being pruned; a writer that only inserted the trail row would leave
// the dashboard empty after the first retention pass.
func TestServedRequestFeedsTheRollupAndLifetimeCounters(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "counted")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rollupRequests, rollupIn, rollupOut int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT SUM(requests), SUM(input_tokens), SUM(output_tokens) FROM request_hourly`).Scan(&rollupRequests, &rollupIn, &rollupOut))
	require.Equal(t, int64(1), rollupRequests, "the hourly rollup must see the request")
	require.Equal(t, int64(11), rollupIn, "the rollup carries the prompt tokens as input")
	require.Equal(t, int64(7), rollupOut, "the rollup carries the completion tokens as output")

	var lifetime string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT value FROM settings WHERE key = 'total_requests'`).Scan(&lifetime))
	require.Equal(t, "1", lifetime,
		"the lifetime counter must advance, since it is what survives pruning")
}

// TestFailedHopsArePersistedWithoutLeakingCredentials covers the attempt
// trail. A provider's rejection commonly echoes the key back, and that
// message lands in a stored row the dashboard renders.
func TestFailedHopsArePersistedWithoutLeakingCredentials(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	rejecting := newFakeUpstream(t, http.StatusTooManyRequests, "")
	working := newFakeUpstream(t, http.StatusOK, "served")
	seedRoute(t, s, "rejecting", rejecting.URL, "rejecting-model", 1)
	seedRoute(t, s, "working", working.URL, "working-model", 2)
	usePriorityOrder(t, s)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	rows, err := s.engine.DB().Query(
		`SELECT attempt, platform, model_id, error_kind, COALESCE(error_message,'')
		   FROM request_attempts ORDER BY attempt`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var (
			attempt           int
			platform, modelID string
			kind, message     string
		)
		require.NoError(t, rows.Scan(&attempt, &platform, &modelID, &kind, &message))
		seen++
		require.NotEmpty(t, kind, "a persisted hop must say how it failed")
		require.NotContains(t, message, "test-key-",
			"a stored hop must never carry the credential the provider echoed back")
	}
	require.Equal(t, 1, seen, "the failed hop must be persisted for the trail view")
}

// lastRequestTokens reads the input/output token columns of the most recent
// requests row, which is what analytics, cost and budget maths all read.
func lastRequestTokens(t *testing.T, s *Server) (int, int) {
	t.Helper()
	var in, out int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT input_tokens, output_tokens FROM requests ORDER BY id DESC LIMIT 1`).Scan(&in, &out))
	return in, out
}

// TestChatRecordsPromptAndCompletionSeparately proves the relay no longer
// collapses usage into the output column: a 90-prompt / 16-completion answer
// must record input 90 and output 16, not 0 and the 106 sum.
func TestChatRecordsPromptAndCompletionSeparately(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":90,"completion_tokens":16,"total_tokens":106}}`))
	}))
	t.Cleanup(upstream.Close)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	in, out := lastRequestTokens(t, s)
	require.Equal(t, 90, in)
	require.Equal(t, 16, out)
}

// TestStreamedChatRecordsUsageSplit proves the streamed path reads the split
// from the late usage frame, not a sum. The streaming upstream's final frame
// reports prompt 5 / completion 3.
func TestStreamedChatRecordsUsageSplit(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newStreamingUpstream(t, "hel", "lo")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey,
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	in, out := lastRequestTokens(t, s)
	require.Equal(t, 5, in)
	require.Equal(t, 3, out)
}

// TestChatWithoutUsageRecordsAnExplicitEstimate protects accounting from a
// provider that omits usage: the request remains visible with non-zero,
// clearly marked approximate counts instead of silently reading as free.
func TestChatWithoutUsageRecordsAnExplicitEstimate(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	in, out := lastRequestTokens(t, s)
	require.Positive(t, in)
	require.Positive(t, out)
	var estimated bool
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT estimated FROM requests ORDER BY id DESC LIMIT 1`).Scan(&estimated))
	require.True(t, estimated, "derived token counts must never be presented as provider-reported")
}

func TestActivitySummaryGroupsTokensByProviderAndModel(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	_, err := s.RecordRequest(t.Context(), RequestLog{
		Platform: "openai", ModelID: "gpt-codex", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 90, OutputTokens: 16, LatencyMs: 700,
	})
	require.NoError(t, err)
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "openai", ModelID: "gpt-codex", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 40, OutputTokens: 9, Estimated: true, LatencyMs: 300,
	})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/usage/summary?window=24h", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got struct {
		Summary struct {
			Requests          int64 `json:"requests"`
			InputTokens       int64 `json:"inputTokens"`
			OutputTokens      int64 `json:"outputTokens"`
			EstimatedRequests int64 `json:"estimatedRequests"`
		} `json:"summary"`
		Models []struct {
			Platform          string `json:"platform"`
			Model             string `json:"model"`
			Requests          int64  `json:"requests"`
			InputTokens       int64  `json:"inputTokens"`
			OutputTokens      int64  `json:"outputTokens"`
			EstimatedRequests int64  `json:"estimatedRequests"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Equal(t, int64(2), got.Summary.Requests)
	require.Equal(t, int64(130), got.Summary.InputTokens)
	require.Equal(t, int64(25), got.Summary.OutputTokens)
	require.Equal(t, int64(1), got.Summary.EstimatedRequests)
	require.Len(t, got.Models, 1)
	require.Equal(t, "openai", got.Models[0].Platform)
	require.Equal(t, "gpt-codex", got.Models[0].Model)
	require.Equal(t, int64(2), got.Models[0].Requests)
	require.Equal(t, int64(130), got.Models[0].InputTokens)
	require.Equal(t, int64(25), got.Models[0].OutputTokens)
	require.Equal(t, int64(1), got.Models[0].EstimatedRequests)
}

// TestFailedAttemptLeavesAWarnLogWithoutSecrets proves a failed hop is no
// longer silent: it lands one warn line in the Logs store naming the provider,
// model and classified event, with the credential the provider echoed scrubbed.
func TestFailedAttemptLeavesAWarnLogWithoutSecrets(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		// A careless provider echoes the credential back; the stored line must not.
		_, _ = fmt.Fprintf(w, `{"error":{"message":"rate limit for %s"}}`, r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)

	var count int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM server_logs WHERE level = 'warn'`).Scan(&count))
	require.Equal(t, 1, count, "one failed hop leaves exactly one warn line")

	var provider, model, event, message string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COALESCE(provider,''), COALESCE(model,''), COALESCE(event,''), message
		   FROM server_logs WHERE level = 'warn' ORDER BY id DESC LIMIT 1`).
		Scan(&provider, &model, &event, &message))
	require.Equal(t, "custom", provider)
	require.Equal(t, "test-model", model)
	require.Equal(t, "rate_limited", event)
	require.NotContains(t, message, "test-key-only", "the credential must be redacted from the log line")
}

// TestExhaustedRunLeavesAnErrorLog proves a run that fails every candidate
// leaves an error line an operator can find, not just an empty Logs page.
func TestExhaustedRunLeavesAnErrorLog(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusInternalServerError, "")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)

	var count int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM server_logs WHERE level = 'error' AND event = 'exhausted'`).Scan(&count))
	require.Equal(t, 1, count, "an exhausted run must be recorded")
}

// TestSuccessfulRequestLeavesNoLogs keeps the buffer signal, not traffic: a
// served request writes nothing, so warn/error rows always mean real trouble.
func TestSuccessfulRequestLeavesNoLogs(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "ok")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postChat(t, s, machineKey, chatBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var count int
	require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM server_logs`).Scan(&count))
	require.Zero(t, count, "a successful request must leave the log buffer empty")
}

// TestUsageQualityDistinguishesReportedEstimatedAndMissing pins the honesty
// contract the accounting surface promises: provider-reported usage is exact,
// usage the gateway had to derive is estimated, and a call that produced no
// usage at all is unavailable -- never a silent exact-zero.
func TestUsageQualityDistinguishesReportedEstimatedAndMissing(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	_, err := s.RecordRequest(t.Context(), RequestLog{
		Platform: "openai", ModelID: "m", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 30, OutputTokens: 10, LatencyMs: 5,
	})
	require.NoError(t, err)
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "openai", ModelID: "m", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 12, OutputTokens: 4, Estimated: true, LatencyMs: 5,
	})
	require.NoError(t, err)
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "openai", ModelID: "m", Outcome: "error",
		StatusCode: http.StatusBadGateway, LatencyMs: 5,
	})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/usage/summary?window=24h", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var summary struct {
		Summary struct {
			ExactRequests       int64 `json:"exactRequests"`
			EstimatedRequests   int64 `json:"estimatedRequests"`
			UnavailableRequests int64 `json:"unavailableRequests"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	require.Equal(t, int64(1), summary.Summary.ExactRequests, "provider-reported usage is exact")
	require.Equal(t, int64(1), summary.Summary.EstimatedRequests, "derived usage is estimated")
	require.Equal(t, int64(1), summary.Summary.UnavailableRequests, "missing usage is unavailable")

	respR, bodyR := do(t, s, http.MethodGet, "/api/usage/requests?limit=10", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, respR.StatusCode, bodyR)
	var rows struct {
		Requests []struct {
			UsageQuality string `json:"usageQuality"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodyR), &rows))
	require.Len(t, rows.Requests, 3)
	quals := map[string]int{}
	for _, r := range rows.Requests {
		quals[r.UsageQuality]++
	}
	require.Equal(t, 1, quals["exact"])
	require.Equal(t, 1, quals["estimated"])
	require.Equal(t, 1, quals["unavailable"])
}

// TestKnownPricePersistsCostAndUnknownPriceRendersUnknown proves cost accounting
// is honest three ways: a catalog price is applied to the recorded tokens at
// record time, a provider-reported total overrides that estimate, and a model
// with no published price records an unknown cost that renders as JSON null --
// never a silent $0 that would read as free.
func TestKnownPricePersistsCostAndUnknownPriceRendersUnknown(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	_, err := s.engine.DB().Exec(`
		INSERT INTO models (platform, model_id, display_name, paid_input_per_m, paid_output_per_m, source)
		VALUES ('anthropic', 'priced', 'Priced', 3.0, 15.0, 'catalog')`)
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(`
		INSERT INTO models (platform, model_id, display_name, source)
		VALUES ('groq', 'free', 'Free', 'catalog')`)
	require.NoError(t, err)

	// Priced model: 1M in + 1M out at $3/M and $15/M => $18.
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "anthropic", ModelID: "priced", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 1_000_000, OutputTokens: 1_000_000, LatencyMs: 10,
	})
	require.NoError(t, err)
	// Unpriced model: tokens present but no published rate => unknown cost.
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "groq", ModelID: "free", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 500, OutputTokens: 500, LatencyMs: 10,
	})
	require.NoError(t, err)
	// Provider-reported total wins even without a catalog rate.
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "groq", ModelID: "free", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 100, OutputTokens: 100, CostUSD: 0.42, LatencyMs: 10,
	})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/usage/requests?limit=10", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got struct {
		Requests []struct {
			Platform    string   `json:"platform"`
			Model       string   `json:"model"`
			InputTokens int64    `json:"inputTokens"`
			CostUSD     *float64 `json:"costUsd"`
			CostKnown   bool     `json:"costKnown"`
		} `json:"requests"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Requests, 3)

	var pricedCost, reportedCost, freeCost *float64
	var pricedKnown, reportedKnown, freeKnown bool
	classified := 0
	for _, r := range got.Requests {
		switch {
		case r.Platform == "anthropic":
			pricedCost, pricedKnown = r.CostUSD, r.CostKnown
		case r.InputTokens == 100:
			reportedCost, reportedKnown = r.CostUSD, r.CostKnown
		default:
			freeCost, freeKnown = r.CostUSD, r.CostKnown
		}
		classified++
	}
	require.Equal(t, 3, classified)

	require.True(t, pricedKnown, "a published price yields a known cost")
	require.NotNil(t, pricedCost)
	require.InDelta(t, 18.0, *pricedCost, 1e-9)

	require.True(t, reportedKnown, "a provider-reported total is authoritative")
	require.NotNil(t, reportedCost)
	require.InDelta(t, 0.42, *reportedCost, 1e-9)

	require.False(t, freeKnown, "an unpublished price is unknown, not free")
	require.Nil(t, freeCost, "an unknown cost must render as JSON null")

	// The summary rolls the same honesty up: only known costs sum, and the
	// known/unknown split is counted rather than hidden.
	respS, bodyS := do(t, s, http.MethodGet, "/api/usage/summary?window=24h", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, respS.StatusCode, bodyS)
	var sum struct {
		Summary struct {
			CostUSD             float64 `json:"costUsd"`
			CostKnownRequests   int64   `json:"costKnownRequests"`
			UnknownCostRequests int64   `json:"unknownCostRequests"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodyS), &sum))
	require.Equal(t, int64(2), sum.Summary.CostKnownRequests)
	require.Equal(t, int64(1), sum.Summary.UnknownCostRequests)
	require.InDelta(t, 18.42, sum.Summary.CostUSD, 1e-9, "the unknown-cost request contributes nothing")
}

func TestPartialPublishedPriceDoesNotUnderstateCost(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})
	_, err := s.engine.DB().Exec(`
		INSERT INTO models (platform, model_id, display_name, paid_input_per_m, source)
		VALUES ('partial', 'input-only', 'Input only', 4.0, 'catalog')`)
	require.NoError(t, err)

	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "partial", ModelID: "input-only", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 1_000_000, OutputTokens: 1,
	})
	require.NoError(t, err)
	_, err = s.RecordRequest(t.Context(), RequestLog{
		Platform: "partial", ModelID: "input-only", Outcome: "success",
		StatusCode: http.StatusOK, InputTokens: 1_000_000,
	})
	require.NoError(t, err)

	rows, err := s.engine.DB().Query(`
		SELECT cost_usd, cost_known FROM requests
		 WHERE platform = 'partial' ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	require.True(t, rows.Next())
	var cost float64
	var known int
	require.NoError(t, rows.Scan(&cost, &known))
	require.Zero(t, known, "a missing output rate makes the total cost unknown when output tokens were used")
	require.Zero(t, cost)

	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&cost, &known))
	require.Equal(t, 1, known, "a missing rate for an unused token dimension does not hide a known cost")
	require.InDelta(t, 4.0, cost, 1e-9)
	require.False(t, rows.Next())
}

// TestUsageSeriesIsChronologicalAndWindowBounded proves the summary chart is
// ordered oldest-first and bounded to the requested window: a request outside
// the window leaves no bucket, and the in-window buckets ascend in time.
func TestUsageSeriesIsChronologicalAndWindowBounded(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	now := time.Now().UTC()
	for _, at := range []time.Time{
		now.Add(-40 * 24 * time.Hour), // outside the 24h window
		now.Add(-2 * time.Hour),
		now.Add(-1 * time.Hour),
	} {
		_, err := s.RecordRequest(t.Context(), RequestLog{
			CreatedAt: at, Platform: "openai", ModelID: "m", Outcome: "success",
			StatusCode: http.StatusOK, InputTokens: 5, OutputTokens: 5, LatencyMs: 5,
		})
		require.NoError(t, err)
	}

	resp, body := do(t, s, http.MethodGet, "/api/usage/summary?window=24h", "",
		map[string]string{"Authorization": "Bearer " + machineKey})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var got struct {
		Series []struct {
			Start    string `json:"start"`
			Requests int64  `json:"requests"`
		} `json:"series"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))

	require.Len(t, got.Series, 2, "only in-window buckets are returned")
	windowStart := now.Add(-24 * time.Hour)
	var total int64
	var prev time.Time
	for i, p := range got.Series {
		total += p.Requests
		start, perr := time.Parse(time.RFC3339, p.Start)
		require.NoError(t, perr)
		require.False(t, start.Before(windowStart), "a bucket must fall within the requested window")
		if i > 0 {
			require.True(t, start.After(prev), "series must be chronologically ordered")
		}
		prev = start
	}
	require.Equal(t, int64(2), total, "the out-of-window request is excluded from the series")
}
