package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/gateway/provider"
	"github.com/stretchr/testify/require"
)

// TestOpenAIShaperMarshalsNativeBufferedResponse proves the native /v1/chat
// surface does not relay a native provider's own envelope. An Anthropic-style
// response carries its native bytes in Raw, but an OpenAI client must receive a
// normalised completion it can parse rather than the native message shape.
func TestOpenAIShaperMarshalsNativeBufferedResponse(t *testing.T) {
	t.Parallel()

	nativeRaw := []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`)
	resp := &provider.ChatResponse{
		ID:     "msg_1",
		Object: "chat.completion",
		Model:  "claude",
		Choices: []provider.Choice{{
			Index:        0,
			Message:      provider.RespMessage{Role: "assistant", Content: json.RawMessage(`"hi"`)},
			FinishReason: "stop",
		}},
		Raw:    nativeRaw,
		Native: true,
	}

	out, err := openAIShaper{}.buffered(resp)
	require.NoError(t, err)
	require.NotContains(t, string(out), `"type":"message"`,
		"the native Anthropic envelope must not leak onto the OpenAI route")
	require.Contains(t, string(out), `"choices"`,
		"a native response is rendered as a normalised OpenAI completion")
	require.Contains(t, string(out), `"hi"`)
}

// TestOpenAIShaperRelaysCompatBufferedResponse proves an explicitly
// OpenAI-compatible adapter still relays its raw bytes unchanged, so fields the
// gateway does not model survive the round trip.
func TestOpenAIShaperRelaysCompatBufferedResponse(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"id":"x","object":"chat.completion","choices":[],"prowl_extra":42}`)
	resp := &provider.ChatResponse{Raw: raw} // Native defaults false.

	out, err := openAIShaper{}.buffered(resp)
	require.NoError(t, err)
	require.Equal(t, raw, out, "a compat adapter's raw bytes are relayed verbatim")
	require.Contains(t, string(out), `"prowl_extra":42`,
		"an unmodelled field survives because the raw payload is relayed")
}

// TestOpenAIStreamMarshalsNativeChunk proves a native stream's normalised chunk
// (Raw empty) is emitted rather than dropped. Dropping it had left the whole
// stream reaching an OpenAI client as nothing but the terminator.
func TestOpenAIStreamMarshalsNativeChunk(t *testing.T) {
	t.Parallel()

	chunk := &provider.ChatChunk{
		ID: "c1", Object: "chat.completion.chunk", Model: "claude",
		Choices: []provider.ChunkChoice{{Index: 0, Delta: json.RawMessage(`{"content":"Hi"}`)}},
		Native:  true,
	}

	frame := openAIStream{}.frame(chunk)
	require.NotEmpty(t, frame, "a Raw-empty native chunk must still be emitted")
	require.True(t, strings.HasPrefix(string(frame), "data: "),
		"the normalised chunk is framed as an SSE data line")
	require.Contains(t, string(frame), `"content":"Hi"`)
	require.Contains(t, string(frame), `"choices"`)
}

// TestOpenAIStreamRelaysCompatChunk proves a compat chunk's raw SSE payload is
// relayed verbatim, preserving fields the gateway does not model.
func TestOpenAIStreamRelaysCompatChunk(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"id":"c1","choices":[{"delta":{"content":"x"}}],"vendor":1}`)
	frame := openAIStream{}.frame(&provider.ChatChunk{Raw: raw})
	require.Equal(t, "data: "+string(raw)+"\n\n", string(frame))
}

// TestOpenAIStreamSkipsEmptyNativeChunk proves a native chunk with neither Raw
// nor choices yields nothing, so a bookkeeping frame is not written as an empty
// SSE line.
func TestOpenAIStreamSkipsEmptyNativeChunk(t *testing.T) {
	t.Parallel()

	require.Nil(t, openAIStream{}.frame(&provider.ChatChunk{Native: true}))
}
