package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexRequestConvertsChatMessagesAndTools(t *testing.T) {
	request := &ChatRequest{
		Model: "gpt-5-codex",
		Messages: []map[string]any{
			{"role": "system", "content": "Be precise."},
			{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Inspect this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}},
			}},
			{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call-1", "function": map[string]any{"name": "read_file", "arguments": `{"path":"main.go"}`},
			}}},
			{"role": "tool", "tool_call_id": "call-1", "content": "package main"},
		},
		Params: map[string]any{
			"reasoning_effort": "high",
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "read_file", "description": "Read a file",
					"parameters": map[string]any{"type": "object"},
				},
			}},
		},
	}

	body, err := codexRequest(request)
	require.NoError(t, err)
	require.Equal(t, true, body["stream"])
	require.Equal(t, map[string]any{"effort": "high"}, body["reasoning"])

	input := body["input"].([]any)
	require.Len(t, input, 4)
	user := input[1].(map[string]any)
	parts := user["content"].([]any)
	require.Equal(t, "input_text", parts[0].(map[string]any)["type"])
	require.Equal(t, "input_image", parts[1].(map[string]any)["type"])
	require.Equal(t, "function_call", input[2].(map[string]any)["type"])
	require.Equal(t, "function_call_output", input[3].(map[string]any)["type"])

	tools := body["tools"].([]any)
	flat := tools[0].(map[string]any)
	require.Equal(t, "read_file", flat["name"])
	require.NotContains(t, flat, "function")
}

func TestCodexStreamMapsTextToolsAndCompletion(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-1","model":"gpt-5-codex"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call-2","name":"read_file"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"main.go\"}"}`,
		`data: {"type":"response.completed","response":{"id":"resp-1","model":"gpt-5-codex"}}`,
		"",
	}, "\n\n")
	stream := newCodexStream(&http.Response{
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
	require.Len(t, chunks, 5)
	require.Equal(t, "resp-1", chunks[0].ID)
	require.Equal(t, "gpt-5-codex", chunks[0].Model)

	var textDelta struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(chunks[0].Choices[0].Delta, &textDelta))
	require.Equal(t, "hello", textDelta.Content)
	finish := chunks[len(chunks)-1].Choices[0].FinishReason
	require.NotNil(t, finish)
	require.Equal(t, "tool_calls", *finish)
}

func TestOrderedCodexToolCallsKeepsSparseOutputIndexes(t *testing.T) {
	first := codexToolCall{Index: 3, ID: "third"}
	second := codexToolCall{Index: 1, ID: "first"}
	ordered := orderedCodexToolCalls(map[int]codexToolCall{3: first, 1: second})
	require.Equal(t, []string{"first", "third"}, []string{ordered[0].ID, ordered[1].ID})
}

func TestCodexStreamReportsResponseFailure(t *testing.T) {
	stream := newCodexStream(&http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.failed","response":{"error":{"message":"subscription unavailable"}}}` + "\n\n")),
	}, func() {}, "gpt")
	_, err := stream.Recv()
	require.EqualError(t, err, "subscription unavailable")
}

// drainCodex collects every chunk until the stream ends, returning the
// terminating error (io.EOF on a clean end).
func drainCodex(t *testing.T, s *codexStream) ([]*ChatChunk, error) {
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

func codexStreamFrom(body string) *codexStream {
	return newCodexStream(&http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, func() {}, "fallback")
}

// TestCodexStreamRejectsTruncationBeforeCompleted proves a stream cut before its
// response.completed terminal event is surfaced as an unexpected EOF (a
// transport cut) rather than a clean end that would pass a partial answer off as
// success.
func TestCodexStreamRejectsTruncationBeforeCompleted(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp-1","model":"gpt-5-codex"}}`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
	}, "\n\n")
	_, err := drainCodex(t, codexStreamFrom(body))
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// TestCodexStreamCapturesCompletedUsage proves the run's exact token usage from
// response.completed is carried on the terminal chunk's Raw, where the gateway
// reads streamed usage - a real zero output count included.
func TestCodexStreamCapturesCompletedUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`data: {"type":"response.completed","response":{"id":"resp-1","model":"gpt-5-codex","usage":{"input_tokens":42,"output_tokens":0,"total_tokens":42}}}`,
		"",
	}, "\n\n")
	chunks, err := drainCodex(t, codexStreamFrom(body))
	require.ErrorIs(t, err, io.EOF)
	require.NotEmpty(t, chunks)

	final := chunks[len(chunks)-1]
	require.True(t, final.Native, "a re-normalised chunk must be Native so its usage Raw is not relayed to the client")
	require.NotNil(t, final.Choices[0].FinishReason)
	in, out := usageFromChunkRaw(t, final.Raw)
	require.Equal(t, 42, in)
	require.Equal(t, 0, out, "a legitimately zero output count must survive as exact, not be dropped")
}

// TestCodexStreamOmitsUsageWhenUnreported proves the terminal chunk carries no
// synthetic usage when the completed event reported none, so the gateway falls
// back to estimation instead of billing a fabricated zero.
func TestCodexStreamOmitsUsageWhenUnreported(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		`data: {"type":"response.completed","response":{"id":"resp-1","model":"gpt-5-codex"}}`,
		"",
	}, "\n\n")
	chunks, err := drainCodex(t, codexStreamFrom(body))
	require.ErrorIs(t, err, io.EOF)
	require.Empty(t, chunks[len(chunks)-1].Raw, "no reported usage must leave Raw empty")
}

// usageFromChunkRaw parses the input/output token counts a native stream stamps
// into a chunk's Raw, the same shape the gateway's usageFromFrame reads.
func usageFromChunkRaw(t *testing.T, raw json.RawMessage) (int, int) {
	t.Helper()
	require.NotEmpty(t, raw)
	var frame struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(raw, &frame))
	require.NotNil(t, frame.Usage, "Raw must carry a usage object")
	return frame.Usage.InputTokens, frame.Usage.OutputTokens
}

// TestCodexChunkUsageReadsExactCounts proves the buffered verb's usage reader
// round-trips the exact counts stamped by streamUsageFrame, a real zero
// preserved, and reports nothing when no usage object is present.
func TestCodexChunkUsageReadsExactCounts(t *testing.T) {
	require.Nil(t, codexChunkUsage(nil))
	require.Nil(t, codexChunkUsage(json.RawMessage(`{"choices":[]}`)))

	u := codexChunkUsage(streamUsageFrame(42, 0))
	require.NotNil(t, u)
	require.Equal(t, 42, u.PromptTokens)
	require.Equal(t, 0, u.CompletionTokens, "a legitimately zero output count must survive as exact")
	require.Equal(t, 42, u.TotalTokens)
}
