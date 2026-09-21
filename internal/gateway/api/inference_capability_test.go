package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// TestCapabilityRefusalsMapToUndispatchable pins the shared error mapping: a
// typed capability refusal - the resolved adapter has no wire for the requested
// verb (chat, embeddings, or a media modality, including a fixed-format speech
// refusal) - must classify exactly like an unregistered adapter. That means
// fail over, skip the whole platform, and record NO cooldown or penalty; it is
// the gateway's routing gap, not a provider-health verdict. A plain error and a
// real HTTP verdict must NOT be swallowed by this branch.
func TestCapabilityRefusalsMapToUndispatchable(t *testing.T) {
	t.Parallel()

	// The classification an unregistered adapter earns, which every capability
	// refusal must match byte for byte: retryable, platform-skip, no health signal.
	wantClass := gateway.ClassifyError(0, "", gateway.UndispatchableError("x"))

	cases := []struct {
		name string
		err  error
	}{
		{"chat", provider.ErrChatUnsupported},
		{"embeddings", provider.ErrEmbeddingsUnsupported},
		{"modality", provider.ErrModalityUnsupported},
		// The exact shape enforceFixedSpeechFormat produces for a Cloudflare
		// MeloTTS opus request: the sentinel wrapped with a specific reason.
		{"wrapped speech format refusal",
			fmt.Errorf("%w: Cloudflare speech only produces mp3, not opus", provider.ErrModalityUnsupported)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := dispatchFromError(tc.err)

			ue := gateway.AsUpstreamError(res.Err)
			require.NotNil(t, ue, "a capability refusal must carry a typed UpstreamError")
			require.True(t, ue.Undispatchable,
				"a capability refusal must be marked undispatchable so the loop skips it without a health verdict")
			require.ErrorIs(t, res.Err, tc.err,
				"the original refusal must remain unwrappable for diagnostics")

			cls := gateway.ClassifyError(res.Status, res.Body, res.Err)
			require.True(t, cls.Retryable, "it must fail over, not halt the run as a fatal verdict")
			require.True(t, cls.SkipPlatform, "every key on the platform refuses the same verb")
			require.Equal(t, gateway.SkipScopePlatform, cls.Scope)
			require.Equal(t, wantClass, cls,
				"a capability refusal must classify identically to an unregistered adapter: no cooldown, no penalty")
		})
	}

	// A real HTTP verdict keeps its status, body, and back-off - the capability
	// branch must not intercept it.
	httpRes := dispatchFromError(&provider.HTTPError{
		Status: http.StatusTooManyRequests, Body: []byte("rate limit"), RetryAfter: 3 * time.Second,
	})
	require.Equal(t, http.StatusTooManyRequests, httpRes.Status)
	require.Equal(t, "rate limit", httpRes.Body)
	require.Equal(t, 3*time.Second, httpRes.RetryAfter)
	require.Nil(t, gateway.AsUpstreamError(httpRes.Err), "an HTTP verdict must not be re-typed as undispatchable")

	// An ordinary transport error stays a plain error, not an undispatchable one.
	plainRes := dispatchFromError(errors.New("connection reset"))
	require.Nil(t, gateway.AsUpstreamError(plainRes.Err),
		"a plain error must not be mistaken for a capability refusal")
}

// TestChatUnsupportedProviderAdvancesToHealthy proves the chat plane fails over
// past a provider that has no chat wire (Google is registered for embeddings and
// speech but refuses chat) instead of surfacing its refusal as the request's
// fatal verdict, and never books the mis-routed provider as a success.
func TestChatUnsupportedProviderAdvancesToHealthy(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	usePriorityOrder(t, s)

	// First candidate: Google - a real registered adapter that refuses chat.
	googleKeyID, err := s.engine.Vault().Add("google", "google-test-key", gateway.AddOptions{Label: "google"})
	require.NoError(t, err)
	res, err := s.engine.DB().Exec(`
		INSERT INTO models (platform, model_id, display_name, key_id, endpoint_scope,
			size_label, intelligence_rank, enabled, supports_tools)
		VALUES ('google', 'gemini-2.5-flash', 'Gemini', ?, '', 'Medium', 9, 1, 1)`, googleKeyID)
	require.NoError(t, err)
	googleModelID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(
		`INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, 1, 1)`, googleModelID)
	require.NoError(t, err)

	// Second candidate: a reachable chat provider.
	healthy := newFakeUpstream(t, http.StatusOK, "served")
	seedRoute(t, s, "healthy", healthy.URL, "healthy-model", 2)

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a chat-unsupported first candidate must not halt the run: body was %s", body)
	require.Contains(t, body, "served", "the answer must come from the reachable candidate")
	require.Equal(t, int64(1), healthy.calls.Load(), "the reachable candidate must have been called")

	var googleSuccesses int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM requests WHERE platform = 'google' AND outcome = 'success'`).Scan(&googleSuccesses))
	require.Zero(t, googleSuccesses, "a provider that refused chat must never be booked as a success")
}

// TestRejectedEmptyEnvelopeConsumesQuotaBeforeFailover proves a buffered 200
// with reported usage but no choices is rejected AND its spent quota is settled
// against the endpoint's rate windows - the provider processed the request and
// billed it, so releasing the lease would let it serve unbillable rejects for
// free. The run still fails over to a healthy candidate and never commits the
// empty body.
func TestRejectedEmptyEnvelopeConsumesQuotaBeforeFailover(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	empty := newEnvelopeUpstream(t,
		`{"id":"x","object":"chat.completion","model":"m","choices":[],`+
			`"usage":{"prompt_tokens":9,"completion_tokens":6,"total_tokens":15}}`)
	good := newFakeUpstream(t, http.StatusOK, "recovered")
	emptyModelID := seedRoute(t, s, "empty", empty.URL, "m", 1)
	seedRoute(t, s, "good", good.URL, "m2", 2)
	usePriorityOrder(t, s)

	var emptyKeyID int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT key_id FROM models WHERE id = ?`, emptyModelID).Scan(&emptyKeyID))

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, int64(1), empty.calls.Load(), "the empty candidate must have been tried")
	require.Contains(t, body, "recovered", "the served answer comes from the healthy candidate")

	// The rejected 200 consumed the provider's request slot: the empty
	// candidate's minute request window is spent, so a fresh request at RPM=1
	// is refused. A released lease would have refunded it and admitted here.
	require.False(t, s.engine.Ledger().Admit(gateway.Admission{
		Platform: "custom", ModelID: "m", KeyID: emptyKeyID, EstimatedTokens: 1,
		Limits: gateway.WindowLimits{RPM: 1},
	}), "a rejected 200 with usage must consume an RPM slot, not refund it")

	// And it consumed the reported tokens: the minute token window holds the 15
	// reported tokens, so a request needing one more overruns TPM=15.
	require.False(t, s.engine.Ledger().Admit(gateway.Admission{
		Platform: "custom", ModelID: "m", KeyID: emptyKeyID, EstimatedTokens: 1,
		Limits: gateway.WindowLimits{TPM: 15},
	}), "the reported usage must settle against the token window, not be refunded")
}

// TestSpeechCapabilityRefusalAdvancesToCompatibleRoute proves the modality plane
// fails over past a fixed-format speech refusal to a later candidate that CAN
// produce the requested codec. Cloudflare MeloTTS emits only MP3, so an opus
// request is refused before any upstream call (a capability refusal); the loop
// must advance to an opus-capable route and serve it, rather than halting on the
// refusal as a fatal verdict.
func TestSpeechCapabilityRefusalAdvancesToCompatibleRoute(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})

	// A compatible later route: a custom endpoint that answers speech in the
	// opus container the caller asked for.
	const opusBody = "OggS-fake-opus-bytes"
	opusFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/opus")
		_, _ = io.WriteString(w, opusBody)
	}))
	t.Cleanup(opusFake.Close)

	// First candidate: Cloudflare MeloTTS, which produces MP3 and refuses opus.
	cfKeyID, err := s.engine.Vault().Add("cloudflare", "acct:token", gateway.AddOptions{Label: "cf"})
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(
		`INSERT INTO media_models (platform, model_id, display_name, modality, priority, enabled, key_id)
		 VALUES ('cloudflare', 'melotts', 'MeloTTS', 'audio', 1, 1, ?)`, cfKeyID)
	require.NoError(t, err)

	// Second candidate: the opus-capable custom route.
	seedMediaRoute(t, s, "opus", opusFake.URL, "audio", "tts-opus", 2)

	resp, body := postModalityJSON(t, s, "/v1/audio/speech", modalityMachineKey,
		`{"model":"auto","input":"hello there","voice":"alloy","response_format":"opus"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"the speech-format refusal must fail over to a compatible route, not halt: body was %s", body)
	require.Equal(t, "audio/opus", resp.Header.Get("Content-Type"),
		"the served codec must be the one the compatible route produced")
	require.Equal(t, opusBody, body, "the compatible route's audio must be relayed unchanged")

	platform, outcome, _, _ := latestRequestRow(t, s)
	require.Equal(t, "success", outcome, "the run must record a success")
	require.Equal(t, "custom", platform, "attributed to the route that actually served, not the one that refused")
}

// TestParamsForRouteDropsReasoningForNonReasoningModel pins the fix for a
// non-reasoning model 400-ing on a reasoning-only param: a client that always
// sends reasoning_effort must not have it forwarded to a model that cannot
// reason (Anthropic Haiku: "this model does not support the effort parameter"),
// and the shared params map must survive so a failover to a reasoning model
// still carries it.
func TestParamsForRouteDropsReasoningForNonReasoningModel(t *testing.T) {
	t.Parallel()
	params := map[string]any{"temperature": 0.2, "reasoning_effort": "xhigh"}

	got := paramsForRoute(gateway.Route{SupportsReasoning: false}, params)
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatal("reasoning_effort must be stripped for a non-reasoning route")
	}
	require.Equal(t, 0.2, got["temperature"], "non-reasoning params must be preserved")
	if _, ok := params["reasoning_effort"]; !ok {
		t.Fatal("the caller's params map must not be mutated; a reasoning failover still needs it")
	}

	got = paramsForRoute(gateway.Route{SupportsReasoning: true}, params)
	require.Equal(t, "xhigh", got["reasoning_effort"], "a reasoning route must keep reasoning_effort")
}
