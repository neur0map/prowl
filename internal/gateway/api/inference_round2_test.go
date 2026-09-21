package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// TestRoutePublishesModelWindowsAndScope proves the chain adapter carries every
// published rate window and the model's endpoint scope onto the route the
// dispatcher reads, so admission and accounting see the same limits the catalog
// declared. Before the fix only the daily windows reached the route; the minute
// caps and the scope were dropped between the chain row and the route.
func TestRoutePublishesModelWindowsAndScope(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	up := newFakeUpstream(t, http.StatusOK, "hi")
	modelDBID := seedRoute(t, s, "ep", up.URL, "m", 1)
	usePriorityOrder(t, s)

	_, err := s.engine.DB().Exec(
		`UPDATE models SET rpm_limit = 17, rpd_limit = 170, tpm_limit = 1700,
		 tpd_limit = 17000, endpoint_scope = 'scope-x' WHERE id = ?`, modelDBID)
	require.NoError(t, err)

	chain, err := s.buildChain(context.Background(), &chatRequestBody{
		Model:    "m",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	})
	require.NoError(t, err)

	route, err := chain.Route(0, gateway.NewSkipState())
	require.NoError(t, err)

	require.NotNil(t, route.RPMLimit)
	require.EqualValues(t, 17, *route.RPMLimit, "the minute request cap must reach the route")
	require.NotNil(t, route.TPMLimit)
	require.EqualValues(t, 1700, *route.TPMLimit, "the minute token cap must reach the route")
	require.NotNil(t, route.RPDLimit)
	require.EqualValues(t, 170, *route.RPDLimit)
	require.NotNil(t, route.TPDLimit)
	require.EqualValues(t, 17000, *route.TPDLimit)
	require.Equal(t, "scope-x", route.EndpointScope, "the model's endpoint scope must ride on the route")
	require.EqualValues(t, 1, route.TokenMultiplier, "a platform with no account cap bills one-to-one")
}

// TestMinuteRequestCapReachesAdmission proves the route's RPM cap is actually
// consulted by the ledger on the request path: a candidate whose minute window
// is already full is refused before dispatch, so the upstream is never called.
// A dropped RPM would admit the request and reach the provider.
func TestMinuteRequestCapReachesAdmission(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	up := newFakeUpstream(t, http.StatusOK, "hi")
	modelDBID := seedRoute(t, s, "ep", up.URL, "m", 1)
	usePriorityOrder(t, s)

	_, err := s.engine.DB().Exec(`UPDATE models SET rpm_limit = 1 WHERE id = ?`, modelDBID)
	require.NoError(t, err)

	var keyID int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT key_id FROM models WHERE id = ?`, modelDBID).Scan(&keyID))

	// One still-in-flight lease fills the model's per-minute request window, so
	// the request's own admission must find the minute cap already reached.
	_, ok := s.engine.Ledger().Acquire(gateway.Admission{
		Platform: "custom", ModelID: "m", KeyID: keyID, EstimatedTokens: 1,
	})
	require.True(t, ok, "priming the minute window must itself be admissible")

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	require.GreaterOrEqual(t, resp.StatusCode, 400, "a full minute window must refuse the request, body %s", body)
	require.Zero(t, up.calls.Load(), "a request refused by the minute cap must never reach the provider")
}

// TestNavyTokenMultiplierReachesAdmission proves the route's Navy token
// multiplier is applied by admission: a candidate whose provider-visible drain
// (estimate x multiplier) overruns the shared account pool is refused before
// the provider is contacted. If the multiplier were dropped, the same estimate
// at 1x would fit and the request would be admitted.
func TestNavyTokenMultiplierReachesAdmission(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})

	keyID, err := s.engine.Vault().Add("navy", "sk-navy-test", gateway.AddOptions{Label: "navy"})
	require.NoError(t, err)

	// Navy meters one 150K-token/day pool per key. Prime it to 100K with a
	// still-in-flight lease so only the candidate's own multiplier decides
	// admission.
	_, ok := s.engine.Ledger().Acquire(gateway.Admission{
		Platform: "navy", ModelID: "primer", KeyID: keyID, EstimatedTokens: 100_000, TokenMultiplier: 1,
	})
	require.True(t, ok, "priming the pool to 100K must be admissible under the 150K cap")

	// ~20K estimated tokens: 100K + 20K x 1 = 120K fits, 100K + 20K x 3 = 160K
	// overruns. The route carries the 3x multiplier a "3x" budget label derives.
	req := &chatRequestBody{
		Model:    "navy/m",
		Messages: []map[string]any{{"role": "user", "content": strings.Repeat("a", 80_000)}},
	}
	relay := &chatRelay{server: s, writer: httptest.NewRecorder(), request: req,
		requestID: "r1", shaper: openAIShaper{}, requestCtx: context.Background()}

	route := gateway.Route{Platform: "navy", ModelID: "m", KeyID: keyID, TokenMultiplier: 3}
	res := relay.Dispatch(context.Background(), route, 0)

	require.Equal(t, http.StatusTooManyRequests, res.Status,
		"the 3x multiplier must push the pool over its cap and refuse admission before dispatch")

	// The multiplier is the cause: the identical estimate at 1x admits.
	require.True(t, s.engine.Ledger().Admit(gateway.Admission{
		Platform: "navy", ModelID: "m", KeyID: keyID, EstimatedTokens: 20_000, TokenMultiplier: 1,
	}), "the same estimate at 1x must fit, isolating the multiplier as the cause")
}

// TestRouteScopePreservesLinkedEndpointIdentity proves accounting records a
// route's own endpoint scope, not the base URL of the key that served it. A
// linked (subscription) model carries a non-URL scope like "linked:custom"
// while its key points at an ordinary endpoint; deriving the scope from the
// base URL would collapse the linked model's spend onto that endpoint.
func TestRouteScopePreservesLinkedEndpointIdentity(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	up := newFakeUpstream(t, http.StatusOK, "served")
	modelDBID := seedRoute(t, s, "ep", up.URL, "m", 1)
	usePriorityOrder(t, s)

	const linkedScope = "linked:custom"
	_, err := s.engine.DB().Exec(
		`UPDATE models SET endpoint_scope = ? WHERE id = ?`, linkedScope, modelDBID)
	require.NoError(t, err)

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	var scope string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT endpoint_scope FROM requests WHERE outcome = 'success' ORDER BY id DESC LIMIT 1`).Scan(&scope))
	require.Equal(t, linkedScope, scope, "the request must be accounted under the model's own scope")
	require.NotEqual(t, normalizeBaseURL(up.URL), scope,
		"the linked identity must not collapse onto the serving key's base URL")
}

// TestDistinctSessionHeadersKeyDistinctStickySessions proves a client-supplied
// x-session-id is threaded through to the sticky key on the native dialect, so
// two conversations with the same first message but different session ids pin
// independently. Before the fix the header was dropped and both keyed on the
// message hash, cross-contaminating their pins.
func TestDistinctSessionHeadersKeyDistinctStickySessions(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	up := newFakeUpstream(t, http.StatusOK, "hi")
	modelDBID := seedRoute(t, s, "ep", up.URL, "m", 1)
	usePriorityOrder(t, s)

	post := func(sessionID string) {
		resp, body := do(t, s, http.MethodPost, "/v1/chat/completions",
			`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"Authorization": "Bearer " + compatMachineKey, "x-session-id": sessionID})
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
	}

	post("sess-A")
	post("sess-B")

	keyA := gateway.SessionKey("hi", "sess-A", gateway.StrategyKeyFor("auto"))
	keyB := gateway.SessionKey("hi", "sess-B", gateway.StrategyKeyFor("auto"))
	require.NotEqual(t, keyA, keyB, "distinct session ids must produce distinct sticky keys")

	gotA, okA := s.engine.Sticky().Get(keyA)
	require.True(t, okA, "session A's header must key its own sticky pin")
	require.Equal(t, modelDBID, gotA)
	gotB, okB := s.engine.Sticky().Get(keyB)
	require.True(t, okB, "session B's header must key its own sticky pin")
	require.Equal(t, modelDBID, gotB)

	// The message-hash fallback must be unused: the header, not the prompt,
	// keyed the session.
	_, okFallback := s.engine.Sticky().Get(gateway.SessionKey("hi", "", gateway.StrategyKeyFor("auto")))
	require.False(t, okFallback, "a supplied session id must supersede the first-message hash")
}

// newEnvelopeUpstream serves one raw JSON completion envelope verbatim, so a
// test can drive a no-choices or malformed-tool-call response the content-only
// helper cannot express.
func newEnvelopeUpstream(t *testing.T, envelope string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, envelope)
	}))
	t.Cleanup(f.Close)
	return f
}

// TestEmptyBufferedEnvelopeFailsOver proves a buffered success envelope with no
// choices is rejected before commit and the loop moves to the next candidate,
// rather than serving the client an empty answer as if it were the truth.
func TestEmptyBufferedEnvelopeFailsOver(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	empty := newEnvelopeUpstream(t,
		`{"id":"x","object":"chat.completion","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	good := newFakeUpstream(t, http.StatusOK, "recovered")
	seedRoute(t, s, "empty", empty.URL, "m", 1)
	seedRoute(t, s, "good", good.URL, "m2", 2)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, int64(1), empty.calls.Load(), "the empty candidate must have been tried")
	require.Contains(t, body, "recovered", "the answer must come from the healthy candidate")
}

// TestEmptyBufferedEnvelopeOnLastCandidateIsReported proves a lone no-choices
// envelope is an error the caller is told about, never a served empty success.
func TestEmptyBufferedEnvelopeOnLastCandidateIsReported(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	empty := newEnvelopeUpstream(t,
		`{"id":"x","object":"chat.completion","model":"m","choices":[]}`)
	seedRoute(t, s, "empty", empty.URL, "m", 1)

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.GreaterOrEqual(t, resp.StatusCode, 400,
		"a run whose only candidate returned no choices must not report success, body %s", body)
}

// TestBufferedToolArgumentsMalformed unit-covers the guard: present-but-invalid
// JSON arguments are rejected, while empty arguments (a valid no-argument call),
// valid arguments, and an unmodelled tool_calls shape are not.
func TestBufferedToolArgumentsMalformed(t *testing.T) {
	t.Parallel()

	malformed := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		ToolCalls: json.RawMessage(`[{"id":"1","type":"function","function":{"name":"f","arguments":"{bad"}}]`),
	}}}}
	require.True(t, bufferedToolArgumentsMalformed(malformed), "unparseable arguments must be rejected")

	valid := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		ToolCalls: json.RawMessage(`[{"id":"1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]`),
	}}}}
	require.False(t, bufferedToolArgumentsMalformed(valid), "valid JSON arguments must pass")

	empty := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		ToolCalls: json.RawMessage(`[{"id":"1","type":"function","function":{"name":"f","arguments":""}}]`),
	}}}}
	require.False(t, bufferedToolArgumentsMalformed(empty), "an empty-argument call is valid")

	none := &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		Content: json.RawMessage(`"hi"`),
	}}}}
	require.False(t, bufferedToolArgumentsMalformed(none), "a plain text answer has nothing to reject")
}

// TestMalformedToolArgumentsFailOver proves a buffered tool call whose
// arguments are present but not valid JSON is rejected before commit and the
// loop reroutes, rather than committing a call an agent would run with garbage.
func TestMalformedToolArgumentsFailOver(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	bad := newEnvelopeUpstream(t,
		`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{not json"}}]},"finish_reason":"tool_calls"}]}`)
	good := newFakeUpstream(t, http.StatusOK, "recovered")
	seedRoute(t, s, "bad", bad.URL, "m", 1)
	seedRoute(t, s, "good", good.URL, "m2", 2)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, compatMachineKey,
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, int64(1), bad.calls.Load(), "the malformed-tool candidate must have been tried")
	require.Contains(t, body, "recovered", "the answer must come from the healthy candidate")
	require.NotContains(t, body, "not json", "the malformed tool call must never reach the client")
}

// TestTypedTurnIntegrityErrorsCarryExemptions proves the empty-completion and
// invalid-tool-argument errors are typed so the classifier reads their turn
// integrity markers: an empty completion is a SkipBench exemption candidate, and
// malformed tool arguments rule out the model for the request. A plain error
// would carry neither and be booked as ordinary provider health.
func TestTypedTurnIntegrityErrorsCarryExemptions(t *testing.T) {
	t.Parallel()

	empty := gateway.ClassifyError(0, "", errEmptyCompletion)
	require.True(t, empty.SkipBench, "an empty completion must be a turn-integrity exemption candidate")
	require.Equal(t, gateway.AttemptEmptyCompletion, empty.Attempt)

	tools := gateway.ClassifyError(0, "", errInvalidToolArguments)
	require.True(t, tools.SkipModelForRequest, "malformed tool arguments must rule out the model for the request")
	require.True(t, tools.SkipModel, "and widen the skip scope to the whole model")
	require.Equal(t, gateway.AttemptInvalidToolArguments, tools.Attempt)
}

// fakeStreamProvider streams a fixed set of frames, so a test can drive the
// relay's stream path without an HTTP upstream - the only way to reach a fixed
// platform (Hyper) whose base URL cannot be pointed at a test server.
type fakeStreamProvider struct {
	platform string
	frames   []*provider.ChatChunk
}

func (f *fakeStreamProvider) Platform() string { return f.platform }
func (f *fakeStreamProvider) Name() string     { return f.platform }
func (f *fakeStreamProvider) BaseURL() string  { return "" }
func (f *fakeStreamProvider) Keyless() bool    { return true }

func (f *fakeStreamProvider) ChatCompletion(context.Context, string, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, fmt.Errorf("buffered path unused")
}

func (f *fakeStreamProvider) StreamChatCompletion(context.Context, string, *provider.ChatRequest) (provider.ChatStream, error) {
	return &fakeChatStream{frames: f.frames}, nil
}

func (f *fakeStreamProvider) ValidateKey(context.Context, string) provider.KeyValidationResult {
	return provider.Inconclusive("test provider")
}

type fakeChatStream struct {
	frames []*provider.ChatChunk
	i      int
}

func (s *fakeChatStream) Recv() (*provider.ChatChunk, error) {
	if s.i >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

func (s *fakeChatStream) Close() error { return nil }

// TestStreamingBodyQuotaObservedForHyper proves the streaming path reads a
// balance a provider publishes inside its payload (Hyper's hypercredits), which
// the header observer cannot see. Before the fix only the buffered path
// observed it, so an interactive stream left the balance unrecorded.
func TestStreamingBodyQuotaObservedForHyper(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	const keyID int64 = 7

	prov := &fakeStreamProvider{platform: "hyper", frames: []*provider.ChatChunk{{
		Choices: []provider.ChunkChoice{{Delta: json.RawMessage(`{"content":"hi"}`)}},
		Raw:     json.RawMessage(`{"choices":[{"delta":{"content":"hi"}}],"usage":{"remaining":{"hypercredits":42}}}`),
	}}}

	relay := &chatRelay{server: s, writer: httptest.NewRecorder(),
		request: &chatRequestBody{Model: "hyper/m", Stream: true}, requestID: "r1",
		shaper: openAIShaper{}, requestCtx: context.Background()}

	route := gateway.Route{Platform: "hyper", ModelID: "m", KeyID: keyID}
	res := relay.stream(context.Background(), prov, "", &provider.ChatRequest{Model: "m"}, route)
	require.True(t, res.succeeded(), "a clean stream must complete")

	var remaining int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT tokens_remaining FROM provider_quota_state WHERE quota_pool_key = ?`,
		gateway.PoolKey("hyper", keyID)).Scan(&remaining),
		"the streamed body balance must have been recorded")
	require.EqualValues(t, 42, remaining, "the recorded balance must be the payload's hypercredits")
}

// TestResponsesReasoningOnlyBufferedPreservesReasoning proves the buffered
// Responses shaper renders reasoning as its own output item and never as the
// assistant's spoken text. A reasoning-only turn (folded or not) must still
// produce a non-empty output, and a reasoning-plus-answer turn must carry both.
func TestResponsesReasoningOnlyBufferedPreservesReasoning(t *testing.T) {
	t.Parallel()

	rs := responsesShaper{model: "m", responseID: "resp_1", inputTokens: 5}

	decode := func(resp *provider.ChatResponse) map[string]any {
		raw, err := rs.buffered(resp)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw, &out))
		return out
	}
	outputTypes := func(env map[string]any) []string {
		items, _ := env["output"].([]any)
		types := make([]string, 0, len(items))
		for _, it := range items {
			m, _ := it.(map[string]any)
			typ, _ := m["type"].(string)
			types = append(types, typ)
		}
		return types
	}

	// Reasoning-only, unfolded: content empty, reasoning present.
	unfolded := decode(&provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		Content: json.RawMessage(`""`), Reasoning: json.RawMessage(`"let me think"`),
	}}}})
	require.Equal(t, []string{"reasoning"}, outputTypes(unfolded), "a reasoning-only turn carries a reasoning item")
	require.Equal(t, "", unfolded["output_text"], "reasoning is never spoken as the answer text")
	items := unfolded["output"].([]any)
	summary := items[0].(map[string]any)["summary"].([]any)
	require.Equal(t, "let me think", summary[0].(map[string]any)["text"])

	// Reasoning-only, folded by normalizeChoices: content equals reasoning.
	folded := decode(&provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		Content: json.RawMessage(`"let me think"`), Reasoning: json.RawMessage(`"let me think"`),
	}}}})
	require.Equal(t, []string{"reasoning"}, outputTypes(folded),
		"a folded reasoning-only turn must not duplicate its reasoning as a message")

	// Reasoning plus a real answer: both are preserved, in order.
	both := decode(&provider.ChatResponse{Choices: []provider.Choice{{Message: provider.RespMessage{
		Content: json.RawMessage(`"the answer"`), Reasoning: json.RawMessage(`"thinking"`),
	}}}})
	require.Equal(t, []string{"reasoning", "message"}, outputTypes(both))
	require.Equal(t, "the answer", both["output_text"])
}

// TestResponsesReasoningOnlyStreamEmitsLifecycle proves a streamed reasoning-only
// Responses turn opens its lifecycle (created, in_progress) and preserves the
// reasoning as summary deltas before response.completed. Before the fix the
// stream lost the reasoning and reached response.completed with no created
// event, which a client's SSE reader cannot parse.
func TestResponsesReasoningOnlyStreamEmitsLifecycle(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	up := newDeltaStreamingUpstream(t, "stop",
		map[string]any{"reasoning_content": "let me think"},
		map[string]any{"reasoning_content": " about it"},
	)
	seedRoute(t, s, "only", up.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/responses",
		`{"model":"auto","stream":true,"input":"hi"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	require.Contains(t, body, "response.created")
	require.Contains(t, body, "response.in_progress")
	require.Contains(t, body, "response.reasoning_summary_text.delta")
	require.Contains(t, body, "let me think about it")
	require.Contains(t, body, "response.reasoning_summary_text.done")
	require.Contains(t, body, "response.completed")
	require.Less(t, strings.Index(body, "response.created"), strings.Index(body, "response.completed"),
		"the lifecycle must open before it completes")
	require.Less(t, strings.Index(body, "response.reasoning_summary_text.delta"),
		strings.Index(body, "response.completed"),
		"the reasoning must stream before the response completes")
}
