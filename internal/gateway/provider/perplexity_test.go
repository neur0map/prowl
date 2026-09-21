package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPerplexityAgentRequestTranslatesCustomToolsAndChoice captures the actual
// POST /v1/agent body and asserts that Chat-Completions custom tools and a
// Chat-style function tool_choice are hoisted into the Agent API's Responses
// shape, while an already-native (flat) custom tool and a non-function built-in
// Agent tool travel through untouched.
func TestPerplexityAgentRequestTranslatesCustomToolsAndChoice(t *testing.T) {
	var captured []byte
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","model":"sonar-pro","status":"completed",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	prov := newPerplexityProvider(server.Client(), server.URL)
	request := &ChatRequest{
		Model:    "sonar-pro",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Params: map[string]any{
			"tools": []any{
				map[string]any{ // Chat Completions custom tool: identity nested under function.
					"type": "function",
					"function": map[string]any{
						"name":        "get_weather",
						"description": "Look up weather",
						"parameters":  map[string]any{"type": "object"},
						"strict":      true,
					},
				},
				map[string]any{ // Already Responses-shaped custom tool: name at top level.
					"type":        "function",
					"name":        "already_flat",
					"description": "Native flat tool",
					"parameters":  map[string]any{"type": "object"},
				},
				map[string]any{ // Non-function built-in Agent tool.
					"type": "web_search",
				},
			},
			"tool_choice": map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_weather"},
			},
		},
	}

	_, err := prov.ChatCompletion(context.Background(), "test-key", request)
	require.NoError(t, err)
	require.Equal(t, "/v1/agent", capturedPath)
	require.NotEmpty(t, captured)

	var body map[string]any
	require.NoError(t, json.Unmarshal(captured, &body))

	tools, ok := body["tools"].([]any)
	require.True(t, ok, "tools must be present in the request")
	require.Len(t, tools, 3)

	// Chat custom tool is hoisted to the Responses top level, nesting removed.
	weather := tools[0].(map[string]any)
	require.Equal(t, "function", weather["type"])
	require.Equal(t, "get_weather", weather["name"])
	require.Equal(t, "Look up weather", weather["description"])
	require.Equal(t, map[string]any{"type": "object"}, weather["parameters"])
	require.Equal(t, true, weather["strict"])
	require.NotContains(t, weather, "function")

	// Already Responses-shaped custom tool passes through unchanged.
	flat := tools[1].(map[string]any)
	require.Equal(t, "function", flat["type"])
	require.Equal(t, "already_flat", flat["name"])
	require.NotContains(t, flat, "function")

	// Non-function built-in Agent tool passes through unchanged.
	builtin := tools[2].(map[string]any)
	require.Equal(t, "web_search", builtin["type"])
	require.NotContains(t, builtin, "name")

	// Chat function tool_choice is hoisted to the Responses shape.
	choice := body["tool_choice"].(map[string]any)
	require.Equal(t, "function", choice["type"])
	require.Equal(t, "get_weather", choice["name"])
	require.NotContains(t, choice, "function")
}

// TestPerplexityToolChoicePassThrough covers the non-translated tool_choice
// forms: the string sentinels ("auto"/"none"/"required") and any choice that
// carries no nested function object are forwarded verbatim.
func TestPerplexityToolChoicePassThrough(t *testing.T) {
	body := perplexityRequest(&ChatRequest{
		Params: map[string]any{"tool_choice": "required"},
	}, false)
	require.Equal(t, "required", body["tool_choice"])

	native := map[string]any{"type": "web_search"}
	body = perplexityRequest(&ChatRequest{
		Params: map[string]any{"tool_choice": native},
	}, false)
	require.Equal(t, native, body["tool_choice"])
}

// TestPerplexityResponseIsNative proves a normalised Agent API response is
// marked Native: its Raw is Perplexity's own envelope (an output array, status
// and usage.cost), not an OpenAI completion, so the OpenAI surface must marshal
// the normalised struct rather than relay Raw to a client that cannot parse it.
// The successful response headers are retained for quota observation.
func TestPerplexityResponseIsNative(t *testing.T) {
	raw := []byte(`{"id":"resp_1","model":"perplexity/sonar","object":"response","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`)
	resp, err := parsePerplexityResponse(raw, http.Header{"X-Ratelimit-Remaining": []string{"7"}}, "perplexity/sonar")
	require.NoError(t, err)
	require.True(t, resp.Native, "the Agent API envelope in Raw is not OpenAI-shaped")
	require.JSONEq(t, `"hi"`, string(resp.Choices[0].Message.Content))
	require.Equal(t, "chat.completion", resp.Object)
	require.Equal(t, "7", resp.Headers.Get("X-Ratelimit-Remaining"),
		"successful response headers must be retained for quota observation")
}
