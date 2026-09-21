package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const compatMachineKey = "prowl-test-machine-key"

// postCompat sends a request to a compat inference surface with the machine
// key, which is the credential an external agent presents.
func postCompat(t *testing.T, s *Server, path, body string) (*http.Response, string) {
	t.Helper()
	return do(t, s, http.MethodPost, path, body, map[string]string{
		"Authorization": "Bearer " + compatMachineKey,
		"Content-Type":  "application/json",
	})
}

// newStreamingUpstream serves an OpenAI-shaped SSE stream, so a test can prove
// a compat surface re-frames real chunks rather than a single buffered answer.
func newStreamingUpstream(t *testing.T, pieces ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, piece := range pieces {
			frame, _ := json.Marshal(map[string]any{
				"id":     "chatcmpl-1",
				"object": "chat.completion.chunk",
				"model":  "m",
				"choices": []map[string]any{{
					"index": 0, "delta": map[string]any{"content": piece},
				}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		final, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "m",
			"choices": []map[string]any{{
				"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3},
		})
		_, _ = w.Write([]byte("data: " + string(final) + "\n\ndata: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnthropicMessageRoundTrips is the surface Claude Code actually calls. It
// must answer in Anthropic's shape, not OpenAI's, or the client cannot read it.
func TestAnthropicMessageRoundTrips(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "hello from anthropic surface")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var out struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)

	require.Equal(t, "message", out.Type)
	require.Equal(t, "assistant", out.Role)
	require.True(t, strings.HasPrefix(out.ID, "msg_"),
		"an Anthropic client groups turns by the msg_ prefix, got %q", out.ID)
	require.Equal(t, "claude-sonnet-4-5", out.Model,
		"the model echoed back must be the one the caller asked for")
	require.Nil(t, out.StopSequence, "stop_sequence must be null, not absent or empty")
	require.Equal(t, "end_turn", out.StopReason)
	require.Len(t, out.Content, 1)
	require.Equal(t, "text", out.Content[0].Type)
	require.Equal(t, "hello from anthropic surface", out.Content[0].Text)
}

// TestAnthropicStopReasonMapping pins the field agent loops branch on. A
// truncated answer reported as end_turn makes a client treat a cut-off reply as
// complete, which is a silent wrong answer rather than a visible failure.
func TestAnthropicStopReasonMapping(t *testing.T) {
	t.Parallel()

	require.Equal(t, "tool_use", anthropicStopReason("tool_calls", false))
	require.Equal(t, "tool_use", anthropicStopReason("stop", true),
		"a message carrying tool calls is tool_use whatever the finish reason says")
	require.Equal(t, "max_tokens", anthropicStopReason("length", false))
	require.Equal(t, "end_turn", anthropicStopReason("stop", false))
	require.Equal(t, "end_turn", anthropicStopReason("", false))
	require.Equal(t, "end_turn", anthropicStopReason("content_filter", false))
}

// TestAnthropicStreamEmitsAnthropicEvents is the property a stream parser
// depends on: the EVENT NAMES and their order. An OpenAI chunk written into
// this stream is unparseable, not merely different.
func TestAnthropicStreamEmitsAnthropicEvents(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newStreamingUpstream(t, "Hel", "lo")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	// The sequence Claude Code expects, in order.
	for _, event := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		require.Contains(t, body, event, "missing %s in:\n%s", event, body)
	}
	require.Less(t, strings.Index(body, "event: message_start"),
		strings.Index(body, "event: content_block_delta"),
		"message_start must precede any content")
	require.Less(t, strings.Index(body, "event: message_delta"),
		strings.Index(body, "event: message_stop"),
		"message_delta carries the stop reason and must precede message_stop")

	require.Contains(t, body, `"text_delta"`)
	require.Contains(t, body, "Hel")
	require.Contains(t, body, "lo")
	require.NotContains(t, body, "data: [DONE]",
		"the OpenAI terminator would make an Anthropic parser wait forever")
}

// TestAnthropicCountTokens covers the endpoint Claude Code calls to size a
// context window before sending a request.
func TestAnthropicCountTokens(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})

	for _, path := range []string{"/v1/messages/count_tokens", "/v1/messages/count"} {
		resp, body := postCompat(t, s, path,
			`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"count these words please"}]}`)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s body was %s", path, body)

		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &out))
		require.Positive(t, out.InputTokens, "a non-empty prompt must count above zero")
	}
}

// TestAnthropicFailureUsesAnthropicErrorVocabulary checks both halves of the
// error contract: the envelope a client can parse, and the type that must
// never appear on an inference failure.
func TestAnthropicFailureUsesAnthropicErrorVocabulary(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	broken := newFakeUpstream(t, http.StatusInternalServerError, "")
	seedRoute(t, s, "broken", broken.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)
	require.Equal(t, "error", out.Type, "Anthropic errors are {type:error, error:{…}}")
	require.NotEmpty(t, out.Error.Type)
	require.NotEqual(t, "authentication_error", out.Error.Type,
		"an application's failed inference call must never end the operator's dashboard session")
}

// TestAnthropicToolUseSurvivesTranslation proves a tool call crosses both
// directions intact: Anthropic tool definitions in, an Anthropic tool_use
// block out with its arguments parsed into an object rather than left a string.
func TestAnthropicToolUseSurvivesTranslation(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})

	// An upstream that answers with an OpenAI tool call.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_1","type":"function","function":{
			"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
			"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`))
	}))
	t.Cleanup(upstream.Close)
	seedRoute(t, s, "tools", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,`+
			`"tools":[{"name":"get_weather","description":"weather",`+
			`"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],`+
			`"tool_choice":{"type":"any"},`+
			`"messages":[{"role":"user","content":"weather in Paris?"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var out struct {
		Content []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)
	require.Equal(t, "tool_use", out.StopReason)
	require.Len(t, out.Content, 1)
	require.Equal(t, "tool_use", out.Content[0].Type)
	require.Equal(t, "get_weather", out.Content[0].Name)
	require.Equal(t, "call_1", out.Content[0].ID)
	require.Equal(t, "Paris", out.Content[0].Input["city"],
		"tool_use.input is an object in Anthropic's shape, not the JSON string OpenAI sends")
}

// TestAnthropicToolChoiceAnyBecomesRequired guards a mistranslation that would
// silently change behaviour: Anthropic's "any" means the model MUST call a
// tool, which OpenAI spells "required". Mapping it to "auto" would let a client
// that demanded a tool call receive a plain answer.
func TestAnthropicToolChoiceAnyBecomesRequired(t *testing.T) {
	t.Parallel()

	require.Equal(t, "required", convertAnthropicToolChoice(json.RawMessage(`{"type":"any"}`)))
	require.Equal(t, "auto", convertAnthropicToolChoice(json.RawMessage(`{"type":"auto"}`)))
	require.Equal(t, "none", convertAnthropicToolChoice(json.RawMessage(`{"type":"none"}`)))
	require.Equal(t,
		map[string]any{"type": "function", "function": map[string]any{"name": "pick"}},
		convertAnthropicToolChoice(json.RawMessage(`{"type":"tool","name":"pick"}`)))
}

// TestAnthropicSystemAndImagesConvert covers the two inbound shapes that differ
// most from OpenAI: a top-level system field (a string or an array of blocks)
// and an inline base64 image.
func TestAnthropicSystemAndImagesConvert(t *testing.T) {
	t.Parallel()

	req := &anthropicRequest{
		Model:  "claude-sonnet-4-5",
		System: json.RawMessage(`[{"type":"text","text":"be terse"}]`),
		Messages: []anthropicMessage{{
			Role: "user",
			Content: json.RawMessage(`[{"type":"text","text":"what is this?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`),
		}},
	}
	converted, refused, err := convertAnthropicRequest(req)
	require.NoError(t, err)
	require.Empty(t, refused)

	require.Equal(t, "system", converted.Messages[0]["role"])
	require.Equal(t, "be terse", converted.Messages[0]["content"],
		"an array-shaped system field must flatten to text, not be dropped")

	parts, ok := converted.Messages[1]["content"].([]map[string]any)
	require.True(t, ok, "a message carrying an image must keep its parts array")
	require.Equal(t, "text", parts[0]["type"])
	require.Equal(t, "image_url", parts[1]["type"])
	image := parts[1]["image_url"].(map[string]any)
	require.Equal(t, "data:image/png;base64,AAAA", image["url"],
		"Anthropic sends the bytes inline; the OpenAI shape needs a data URL")
}

// TestAnthropicRefusesDocumentBlocks: no provider here accepts a document
// block, and dropping one would answer a question about a file the model never
// saw. The caller is told instead.
func TestAnthropicRefusesDocumentBlocks(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user",
			"content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "document")
	require.NotContains(t, body, "authentication_error")
}

// TestThinkingBudgetMapsToEffort pins the mapping, including the case that must
// send no knob at all: an adaptive request means "you decide", so forwarding a
// disabled reasoning path would override what the caller asked for.
func TestThinkingBudgetMapsToEffort(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", effortFromAnthropicThinking(nil))
	require.Equal(t, "none", effortFromAnthropicThinking(json.RawMessage(`{"type":"disabled"}`)))
	require.Equal(t, "low", effortFromAnthropicThinking(json.RawMessage(`{"budget_tokens":1024}`)))
	require.Equal(t, "medium", effortFromAnthropicThinking(json.RawMessage(`{"budget_tokens":8192}`)))
	require.Equal(t, "high", effortFromAnthropicThinking(json.RawMessage(`{"budget_tokens":32768}`)))
	require.Equal(t, "", effortFromAnthropicThinking(json.RawMessage(`{"type":"adaptive"}`)),
		"model-managed thinking without a budget must forward no knob")
}

// TestLegacyCompletionRoundTrips covers the prompt-in, text-out surface editor
// autocomplete clients still use.
func TestLegacyCompletionRoundTrips(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "  return x + y")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/completions",
		`{"model":"auto","prompt":"def add(x, y):","suffix":"\n","max_tokens":32}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Choices []struct {
			Text         string  `json:"text"`
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)
	require.Equal(t, "text_completion", out.Object,
		"a legacy client branches on this object name")
	require.True(t, strings.HasPrefix(out.ID, "cmpl-"), "got %q", out.ID)
	require.Len(t, out.Choices, 1)
	require.Equal(t, "  return x + y", out.Choices[0].Text)
}

// TestResponsesRoundTrips covers the Responses API shape, which differs from
// chat completions in both directions.
func TestResponsesRoundTrips(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newFakeUpstream(t, http.StatusOK, "the responses answer")
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/responses",
		`{"model":"auto","instructions":"be brief","input":"say something"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var out struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens int `json:"input_tokens"`
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out), "body was %s", body)
	require.Equal(t, "response", out.Object)
	require.Equal(t, "completed", out.Status)
	require.True(t, strings.HasPrefix(out.ID, "resp_"), "got %q", out.ID)
	require.Equal(t, "the responses answer", out.OutputText)
	require.Len(t, out.Output, 1)
	require.Equal(t, "message", out.Output[0].Type)
	require.Equal(t, "output_text", out.Output[0].Content[0].Type)
	require.Positive(t, out.Usage.TotalTokens)
}

// TestResponsesRefusesContinuation is the honesty case. This gateway stores no
// response state, so a silently ignored previous_response_id would answer
// without the conversation the client believes it referenced - a wrong answer
// rather than an error.
func TestResponsesRefusesContinuation(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	resp, body := postCompat(t, s, "/v1/responses",
		`{"model":"auto","input":"continue","previous_response_id":"resp_abc"}`)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "previous_response_id")
	require.Contains(t, body, "unsupported_parameter")
	require.NotContains(t, body, "authentication_error")
}

// TestResponsesInputShapes covers all three input forms the API allows, since
// a client may send any of them and a missed shape reads as an empty prompt.
func TestResponsesInputShapes(t *testing.T) {
	t.Parallel()

	bare, err := responsesInputMessages(json.RawMessage(`"just text"`))
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"role": "user", "content": "just text"}}, bare)

	messages, err := responsesInputMessages(json.RawMessage(
		`[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]`))
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "user", messages[0]["role"])
	require.Equal(t, "hello", messages[0]["content"],
		"text-only content collapses to a string, which is what providers expect")

	parts, err := responsesInputMessages(json.RawMessage(
		`[{"type":"input_text","text":"loose part"}]`))
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "user", parts[0]["role"])
	require.Equal(t, "loose part", parts[0]["content"])
}

// TestCompatSurfacesRequireACredential keeps the new routes behind the same
// gate as the native one: an unauthenticated agent must be refused, and the
// refusal must not be the type that ends a dashboard session.
func TestCompatSurfacesRequireACredential(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	for _, path := range []string{
		"/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/v1/completions",
	} {
		resp, body := do(t, s, http.MethodPost, path, `{"model":"auto"}`,
			map[string]string{"Content-Type": "application/json"})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s must be gated", path)
		require.NotContains(t, body, "authentication_error",
			"%s: an application's bad key must not sign the operator out", path)
	}
}

// newDeltaStreamingUpstream streams the given OpenAI chat deltas as SSE frames,
// then a finish frame carrying the reason, then [DONE]. It lets a test drive
// tool-call and reasoning streams the content-only helper cannot express.
func newDeltaStreamingUpstream(t *testing.T, finishReason string, deltas ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, d := range deltas {
			frame, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "m",
				"choices": []map[string]any{{"index": 0, "delta": d}},
			})
			_, _ = w.Write([]byte("data: " + string(frame) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		final, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "m",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 3},
		})
		_, _ = w.Write([]byte("data: " + string(final) + "\n\ndata: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnthropicStreamToolArgumentsAreComplete proves the streamed tool call
// carries its accumulated arguments, not the empty object the accumulator used
// to emit - a Claude Code loop reads tool_use.input, so empty args silently
// call the tool with nothing.
func TestAnthropicStreamToolArgumentsAreComplete(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newDeltaStreamingUpstream(t, "tool_calls",
		map[string]any{"tool_calls": []map[string]any{{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": ""}}}},
		map[string]any{"tool_calls": []map[string]any{{"index": 0,
			"function": map[string]any{"arguments": `{"city":`}}}},
		map[string]any{"tool_calls": []map[string]any{{"index": 0,
			"function": map[string]any{"arguments": `"Paris"}`}}}},
	)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,`+
			`"messages":[{"role":"user","content":"weather?"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	require.Contains(t, body, `"type":"tool_use"`)
	require.Contains(t, body, `"name":"get_weather"`)
	require.Contains(t, body, `"id":"call_1"`)
	require.Contains(t, body, `"type":"input_json_delta"`)
	require.Contains(t, body, `{\"city\":\"Paris\"}`,
		"the accumulated fragments must reach the client as the completed input")
	require.NotContains(t, body, `"partial_json":"{}"`,
		"empty arguments would call the tool with nothing")
	require.Contains(t, body, `"stop_reason":"tool_use"`)
}

// TestAnthropicStreamReasoningOnlyStartsMessage proves a completion that emits
// only reasoning still opens with message_start and flushes the thinking block
// in valid order - a stream that reached message_delta without message_start is
// unparseable to a client's SSE reader.
func TestAnthropicStreamReasoningOnlyStartsMessage(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newDeltaStreamingUpstream(t, "stop",
		map[string]any{"reasoning_content": "let me think"},
		map[string]any{"reasoning_content": " about it"},
	)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/messages",
		`{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	require.Contains(t, body, "event: message_start")
	require.Contains(t, body, `"type":"thinking"`)
	require.Contains(t, body, `"type":"thinking_delta"`)
	require.Contains(t, body, "let me think about it")
	require.Contains(t, body, "event: message_stop")
	require.Less(t, strings.Index(body, "event: message_start"),
		strings.Index(body, `"thinking_delta"`),
		"message_start must precede the thinking block")
	require.Less(t, strings.Index(body, `"thinking_delta"`),
		strings.Index(body, "event: message_delta"),
		"thinking must precede the terminal message_delta")
}

// TestResponsesInlineToolContinuation proves a replayed tool turn survives:
// a Responses function_call becomes an assistant tool_calls message and a
// function_call_output becomes the matching role:"tool" result, or the model
// would answer as if the tool had never run.
func TestResponsesInlineToolContinuation(t *testing.T) {
	t.Parallel()

	msgs, err := responsesInputMessages(json.RawMessage(`[
		{"role":"user","content":"weather in Paris?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}
	]`))
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	require.Equal(t, "user", msgs[0]["role"])

	require.Equal(t, "assistant", msgs[1]["role"])
	calls, ok := msgs[1]["tool_calls"].([]map[string]any)
	require.True(t, ok, "a function_call becomes an assistant tool_calls turn")
	require.Len(t, calls, 1)
	require.Equal(t, "call_1", calls[0]["id"])
	fn, ok := calls[0]["function"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "get_weather", fn["name"])
	require.Equal(t, `{"city":"Paris"}`, fn["arguments"])

	require.Equal(t, "tool", msgs[2]["role"])
	require.Equal(t, "call_1", msgs[2]["tool_call_id"])
	require.Equal(t, "sunny", msgs[2]["content"])
}

// TestResponsesFlatToolsBecomeChatSchema proves the Responses API's flat tool
// and tool_choice shapes are rewritten to the nested chat schema a provider
// accepts; a flat function tool passed through unchanged is silently ignored.
func TestResponsesFlatToolsBecomeChatSchema(t *testing.T) {
	t.Parallel()

	tools := responsesToolsToChat(json.RawMessage(`[
		{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}},
		{"type":"web_search"}
	]`))
	require.Len(t, tools, 1, "a built-in Responses tool with no chat equivalent is dropped")
	require.Equal(t, "function", tools[0]["type"])
	fn, ok := tools[0]["function"].(map[string]any)
	require.True(t, ok, "the flat function is nested under `function`")
	require.Equal(t, "get_weather", fn["name"])
	require.Equal(t, "w", fn["description"])
	require.NotNil(t, fn["parameters"])

	require.Equal(t,
		map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		responsesToolChoiceToChat(json.RawMessage(`{"type":"function","name":"get_weather"}`)))
	require.Equal(t, "required", responsesToolChoiceToChat(json.RawMessage(`"required"`)))
}

// TestResponsesStreamEmitsFunctionCall proves the Responses stream renders a
// streamed tool call as its own output item with argument deltas and a
// completed item, which a Responses client switches on by event name.
func TestResponsesStreamEmitsFunctionCall(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	upstream := newDeltaStreamingUpstream(t, "tool_calls",
		map[string]any{"tool_calls": []map[string]any{{"index": 0, "id": "call_9", "type": "function",
			"function": map[string]any{"name": "lookup", "arguments": ""}}}},
		map[string]any{"tool_calls": []map[string]any{{"index": 0,
			"function": map[string]any{"arguments": `{"q":`}}}},
		map[string]any{"tool_calls": []map[string]any{{"index": 0,
			"function": map[string]any{"arguments": `"go"}`}}}},
	)
	seedRoute(t, s, "only", upstream.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/responses",
		`{"model":"auto","stream":true,"input":"look it up"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	for _, event := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		require.Contains(t, body, event, "missing %s in:\n%s", event, body)
	}
	require.Contains(t, body, `"type":"function_call"`)
	require.Contains(t, body, `"name":"lookup"`)
	require.Contains(t, body, `"call_id":"call_9"`)
	require.Contains(t, body, `"arguments":"{\"q\":\"go\"}"`,
		"the completed arguments must reach the client's function_call item")
}
