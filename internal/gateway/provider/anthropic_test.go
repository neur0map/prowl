package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type captureTransport func(*http.Request) (*http.Response, error)

func (fn captureTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestAnthropicProviderUsesMessagesWireAndConvertsResponse(t *testing.T) {
	var captured map[string]any
	client := &http.Client{Transport: captureTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https://api.anthropic.com/v1/messages", req.URL.String())
		require.Equal(t, "sk-ant-api-test", req.Header.Get("X-Api-Key"))
		require.Empty(t, req.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(req.Body).Decode(&captured))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",
				"content":[{"type":"text","text":"done"},{"type":"tool_use","id":"call-1","name":"read_file","input":{"path":"main.go"}}],
				"stop_reason":"tool_use","usage":{"input_tokens":10,"cache_read_input_tokens":3,"output_tokens":4}
			}`)),
		}, nil
	})}
	provider := &anthropicProvider{client: client}
	response, err := provider.ChatCompletion(context.Background(), "sk-ant-api-test", &ChatRequest{
		Model: "anthropic/claude-sonnet-5",
		Messages: []map[string]any{
			{"role": "system", "content": "Be exact."},
			{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Inspect this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}},
			}},
		},
		Params: map[string]any{
			"reasoning_effort": "high",
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{
				"name": "read_file", "description": "Read a file", "parameters": map[string]any{"type": "object"},
			}}},
		},
	})
	require.NoError(t, err)

	require.Equal(t, "claude-sonnet-5", captured["model"])
	require.Equal(t, false, captured["stream"])
	thinkingBlock, hasThinking := captured["thinking"].(map[string]any)
	require.True(t, hasThinking, "reasoning_effort must enable Claude extended thinking")
	require.Equal(t, "enabled", thinkingBlock["type"])
	require.NotContains(t, captured, "output_config", "reasoning must map to thinking, not output_config.effort")
	require.Len(t, captured["system"], 1)
	messages := captured["messages"].([]any)
	user := messages[0].(map[string]any)
	require.Len(t, user["content"], 2)
	tools := captured["tools"].([]any)
	require.Equal(t, "read_file", tools[0].(map[string]any)["name"])

	require.Equal(t, "claude-sonnet-5", response.Model)
	require.Equal(t, "tool_calls", response.Choices[0].FinishReason)
	require.JSONEq(t, `"done"`, string(response.Choices[0].Message.Content))
	require.Contains(t, string(response.Choices[0].Message.ToolCalls), `"name":"read_file"`)
	require.Equal(t, 13, response.Usage.PromptTokens)
	require.Equal(t, 17, response.Usage.TotalTokens)
}

// TestAnthropicReasoningBecomesThinking proves a client's reasoning_effort is
// turned into Claude extended thinking (not output_config.effort, which Haiku
// rejects), with a budget scaled to the effort and capped below max_tokens, and
// that enabling thinking drops the conflicting sampling params.
func TestAnthropicReasoningBecomesThinking(t *testing.T) {
	budgets := []struct {
		effort    string
		maxTokens int
		want      int
		on        bool
	}{
		{"low", 16000, 1024, true},
		{"medium", 16000, 2048, true},
		{"high", 16000, 4096, true},
		{"xhigh", 16000, 8192, true},
		{"high", 3000, 1500, true}, // small window: reserve halves to 1500
		{"high", 1500, 0, false},   // no room to think meaningfully
		{"bogus", 16000, 0, false},
		{"", 16000, 0, false},
	}
	for _, c := range budgets {
		got, on := anthropicThinkingBudget(c.effort, c.maxTokens)
		if on != c.on || got != c.want {
			t.Errorf("anthropicThinkingBudget(%q,%d) = (%d,%v); want (%d,%v)", c.effort, c.maxTokens, got, on, c.want, c.on)
		}
	}

	// End to end: reasoning_effort enables thinking and drops temperature.
	var captured map[string]any
	client := &http.Client{Transport: captureTransport(func(req *http.Request) (*http.Response, error) {
		require.NoError(t, json.NewDecoder(req.Body).Decode(&captured))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"m","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})}
	p := &anthropicProvider{client: client}
	_, err := p.ChatCompletion(context.Background(), "sk-ant-api-test", &ChatRequest{
		Model:    "anthropic/claude-opus-4-8",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Params:   map[string]any{"reasoning_effort": "high", "max_completion_tokens": float64(16000), "temperature": 0.7},
	})
	require.NoError(t, err)
	thinking, ok := captured["thinking"].(map[string]any)
	require.True(t, ok, "reasoning_effort must enable extended thinking")
	require.Equal(t, "enabled", thinking["type"])
	require.Equal(t, float64(4096), thinking["budget_tokens"])
	require.NotContains(t, captured, "temperature", "temperature must be dropped when thinking is on")
	require.NotContains(t, captured, "output_config")
}

// TestAnthropicDropsDeprecatedSamplingParams proves temperature/top_p/top_k are
// never forwarded to Claude even when the request carries no reasoning, since
// newer Claude models (opus-4-8, sonnet-5, 5.x) 400 on them as deprecated.
func TestAnthropicDropsDeprecatedSamplingParams(t *testing.T) {
	var captured map[string]any
	client := &http.Client{Transport: captureTransport(func(req *http.Request) (*http.Response, error) {
		require.NoError(t, json.NewDecoder(req.Body).Decode(&captured))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"m","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})}
	p := &anthropicProvider{client: client}
	_, err := p.ChatCompletion(context.Background(), "sk-ant-api-test", &ChatRequest{
		Model:    "anthropic/claude-opus-4-8",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
		Params:   map[string]any{"temperature": 0.7, "top_p": 0.9, "top_k": 40},
	})
	require.NoError(t, err)
	require.NotContains(t, captured, "temperature")
	require.NotContains(t, captured, "top_p")
	require.NotContains(t, captured, "top_k")
	require.NotContains(t, captured, "thinking", "no reasoning_effort means no thinking block")
}

func TestAnthropicStreamMapsTextToolArgumentsAndFinish(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_2","model":"claude-opus-5"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-2","name":"read_file"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"main.go\"}"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n\n")
	stream := newAnthropicStream(&http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, func() {}, "fallback")

	var chunks []*ChatChunk
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 4)
	require.Equal(t, "msg_2", chunks[0].ID)
	require.Equal(t, "claude-opus-5", chunks[0].Model)
	require.Contains(t, string(chunks[0].Choices[0].Delta), `"content":"hello"`)
	require.Contains(t, string(chunks[1].Choices[0].Delta), `"name":"read_file"`)
	require.Contains(t, string(chunks[2].Choices[0].Delta), `"arguments":"{\"path\":\"main.go\"}"`)
	require.NotNil(t, chunks[3].Choices[0].FinishReason)
	require.Equal(t, "tool_calls", *chunks[3].Choices[0].FinishReason)
}

func TestRegistryUsesNativeAnthropicAdapter(t *testing.T) {
	registered, ok := NewRegistry().Get("anthropic")
	require.True(t, ok)
	_, ok = registered.(*anthropicProvider)
	require.True(t, ok, "Anthropic must not be sent to an OpenAI chat-completions endpoint")
}

func anthropicStreamFrom(body string) *anthropicStream {
	return newAnthropicStream(&http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, func() {}, "fallback")
}

func drainAnthropic(t *testing.T, s *anthropicStream) ([]*ChatChunk, error) {
	t.Helper()
	var chunks []*ChatChunk
	for {
		chunk, err := s.Recv()
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

// TestAnthropicStreamRejectsTruncationBeforeStop proves a stream cut before its
// message_stop terminal event is surfaced as an unexpected EOF (a transport
// cut) rather than a clean end.
func TestAnthropicStreamRejectsTruncationBeforeStop(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":9}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		"",
	}, "\n\n")
	_, err := drainAnthropic(t, anthropicStreamFrom(body))
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// TestAnthropicStreamCapturesStartAndDeltaUsage proves the prompt count from
// message_start is joined with the output count from message_delta and carried
// on the terminal chunk's Raw, cache tokens folded into the prompt total and a
// real zero output count preserved.
func TestAnthropicStreamCapturesStartAndDeltaUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n\n")
	chunks, err := drainAnthropic(t, anthropicStreamFrom(body))
	require.ErrorIs(t, err, io.EOF)
	require.NotEmpty(t, chunks)

	final := chunks[len(chunks)-1]
	require.NotNil(t, final.Choices[0].FinishReason, "the usage-bearing frame is the finish frame")
	require.True(t, final.Native)
	in, out := usageFromChunkRaw(t, final.Raw)
	require.Equal(t, 17, in, "prompt total must fold cache_read and cache_creation tokens")
	require.Equal(t, 0, out, "a legitimately zero output count must survive as exact")
}
