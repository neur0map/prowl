// Package provider is the gateway's outbound adapter layer: one Provider per
// upstream platform, each translating the gateway's normalised chat request
// into a platform's wire format and back, plus a credential probe. It is a Go
// port of FreeLLMAPI's server/src/providers (the BaseProvider contract and its
// 46 registrations); constants that would be hard to re-derive are cited to
// that source as file:line under /tmp/flsrc.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// KeyValidationStatus is the tri-state verdict of a credential probe. The gap
// between Invalid and Inconclusive is load-bearing: a transport error, DNS
// failure, TLS reset or timeout must NEVER be read as a bad key, or a flaky
// network would auto-disable a working credential (base.ts:242, health.ts
// transport branch). Only a provider that actually answered 401/403 confirms a
// bad credential.
type KeyValidationStatus int

const (
	// KeyInconclusive means the probe could not reach a verdict: the provider
	// was unreachable or answered in a way that says nothing about the key.
	KeyInconclusive KeyValidationStatus = iota
	// KeyValid means the provider accepted the credential.
	KeyValid
	// KeyInvalid means the provider confirmed the credential is bad (401/403).
	KeyInvalid
)

// KeyValidationResult is what every adapter's ValidateKey returns. Reason
// carries the upstream detail for an Invalid verdict so the keys UI can show
// why, and a human-readable note for an Inconclusive one.
type KeyValidationResult struct {
	Status KeyValidationStatus
	Reason string
}

// Valid, Invalid and Inconclusive are the three constructors adapters use so a
// caller can never forget to set the status.
func Valid() KeyValidationResult { return KeyValidationResult{Status: KeyValid} }

func Invalid(reason string) KeyValidationResult {
	return KeyValidationResult{Status: KeyInvalid, Reason: reason}
}

func Inconclusive(reason string) KeyValidationResult {
	return KeyValidationResult{Status: KeyInconclusive, Reason: reason}
}

func (r KeyValidationResult) IsValid() bool        { return r.Status == KeyValid }
func (r KeyValidationResult) IsInvalid() bool      { return r.Status == KeyInvalid }
func (r KeyValidationResult) IsInconclusive() bool { return r.Status == KeyInconclusive }

// Provider is the three-verb contract every adapter implements (base.ts:
// 217-242): a chat completion, a streaming chat completion, and a credential
// probe. quotaContext in the reference is dropped here - quota observation is
// Slice A's concern and is wired by the integration pass, not by the adapter.
type Provider interface {
	Platform() string
	Name() string
	BaseURL() string
	// Keyless reports that the provider sends no Authorization header and the
	// key store holds only a sentinel row (base.ts:224).
	Keyless() bool

	ChatCompletion(ctx context.Context, apiKey string, req *ChatRequest) (*ChatResponse, error)
	StreamChatCompletion(ctx context.Context, apiKey string, req *ChatRequest) (ChatStream, error)
	ValidateKey(ctx context.Context, apiKey string) KeyValidationResult
}

// ChatRequest is the gateway's normalised chat-completion request. Messages and
// Params are passed through verbatim (the caller already built OpenAI-shaped
// JSON) so a provider forwards whatever fields the client sent; per-provider
// transforms mutate a copy of the rendered body, never this struct.
type ChatRequest struct {
	Model    string
	Messages []map[string]any
	Stream   bool
	// Params carries every other wire field: temperature, max_tokens, top_p,
	// stop, tools, tool_choice, parallel_tool_calls, response_format, ….
	Params map[string]any
}

// ChatResponse is the parsed non-streaming completion. Raw keeps the untouched
// upstream bytes for callers that need fields this struct does not model.
type ChatResponse struct {
	ID        string          `json:"id"`
	Object    string          `json:"object,omitempty"`
	Model     string          `json:"model"`
	Choices   []Choice        `json:"choices"`
	Usage     *Usage          `json:"usage,omitempty"`
	RoutedVia *RoutedVia      `json:"_routed_via,omitempty"`
	Raw       json.RawMessage `json:"-"`

	// Native marks a response whose Raw is the provider's own non-OpenAI wire
	// format (or empty behind a normalised body). The OpenAI surface must
	// marshal the normalised struct for these rather than relay Raw, which
	// would leak an envelope an OpenAI client cannot parse.
	Native bool `json:"-"`

	// Headers carries the upstream response headers. The router reads its
	// rate-limit families from here: remaining quota is only ever published
	// in headers, so discarding them left every provider reporting "no
	// published quota" however much traffic had gone through it.
	Headers http.Header `json:"-"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int         `json:"index"`
	Message      RespMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// RespMessage is an assistant message. Content is raw because a provider may
// return a string or an array of content parts; Reasoning and ReasoningAlt
// capture the two field names providers use for hidden reasoning (Ollama emits
// `reasoning`, most emit `reasoning_content`).
type RespMessage struct {
	Role         string          `json:"role,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	Reasoning    json.RawMessage `json:"reasoning_content,omitempty"`
	ReasoningAlt json.RawMessage `json:"reasoning,omitempty"`
	ToolCalls    json.RawMessage `json:"tool_calls,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// RoutedVia is stamped on every non-stream response so downstream code can see
// which platform and model actually served it (openai-compat.ts:319).
type RoutedVia struct {
	Platform string `json:"platform"`
	Model    string `json:"model"`
}

// ChatChunk is one streamed SSE frame.
type ChatChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object,omitempty"`
	Model   string          `json:"model"`
	Choices []ChunkChoice   `json:"choices"`
	Raw     json.RawMessage `json:"-"`

	// Native marks a chunk re-normalised from a non-OpenAI stream, whose Raw
	// is not OpenAI-shaped (and is usually empty). The OpenAI surface marshals
	// the normalised chunk for these instead of relaying Raw.
	Native bool `json:"-"`

	// Keepalive marks a liveness-only frame (an Anthropic `ping`) that carries
	// no client-visible content. It proves the upstream is still alive during a
	// long thinking or tool-argument phase, so the relay can reset its idle
	// guard on it without committing or writing anything.
	Keepalive bool `json:"-"`
}

// ChunkChoice is one delta in a streamed frame.
type ChunkChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

// streamUsageFrame renders the minimal usage-only JSON a re-normalised (Native)
// stream stamps into a chunk's Raw so the gateway can read exact token counts
// off the wire. The gateway pulls streamed usage out of Raw regardless of the
// shape the shaper renders (inference.go usageFromFrame), and a native
// adapter's Raw is otherwise empty, so its provider-reported counts would be
// lost and the request billed by estimate. The counts are emitted under the
// Responses-style input/output names; a present usage object is exact even when
// a count is legitimately zero, so a real zero is preserved rather than dropped.
func streamUsageFrame(inputTokens, outputTokens int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
	return raw
}

// HeaderCarrier is the optional interface a stream implements to expose its
// response headers. Streaming is the common path for an interactive client, so
// a quota reading that only worked for buffered calls would almost never fire.
type HeaderCarrier interface {
	ResponseHeaders() http.Header
}

// ChatStream is the streaming return: Recv yields frames until a clean end
// (io.EOF) or an error. Close releases the underlying response body and MUST be
// called.
type ChatStream interface {
	Recv() (*ChatChunk, error)
	Close() error
}

// HTTPError is a non-2xx upstream response the router can act on. RetryAfter is
// the provider's stated back-off, the header winning over any body hint
// (base.ts:143-151); zero means none was given.
type HTTPError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
	Body       []byte

	// Headers is the failing response's headers. A 429 is the single most
	// informative moment for quota - it usually states the limit and when it
	// resets - so they must survive the error path too.
	Headers http.Header
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("upstream HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("upstream HTTP %d", e.Status)
}

// maxRetryAfter caps a stated back-off; a provider that says "retry in a week"
// should not park an endpoint for a week (MAX_RETRY_AFTER_MS, base.ts:27).
const maxRetryAfter = 24 * time.Hour

// parseRetryAfter reads a Retry-After header as delta-seconds or an HTTP-date,
// clamped to maxRetryAfter (base.ts:33-45). Zero means unparseable/absent.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	if when, err := http.ParseTime(value); err == nil {
		d := time.Until(when)
		if d < 0 {
			return 0
		}
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	return 0
}

// validationErrBody models the shapes validationResult walks for a reason
// (base.ts:258-265).
type validationErrBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
	Title   string `json:"title"`
}

// classifyValidation mirrors BaseProvider.validationResult (base.ts:244-273):
// only 401/403 confirm a bad credential; every other answered status - a 404,
// a 429, even a 5xx - is treated as live, deliberately, because a validate
// probe is not the place to bench a key for quota or an upstream blip. The one
// exception is a redirect: a 3xx never reached the authenticated endpoint, so
// it confirms neither a working nor a rejected credential and reading it as
// valid would green-light a key the probe never actually exercised. The
// upstream reason is preserved verbatim so the keys page can show why a key is
// bad.
func classifyValidation(name string, status int, body []byte) KeyValidationResult {
	if status == 401 || status == 403 {
		detail := extractErrorDetail(body)
		if detail == "" {
			detail = http.StatusText(status)
		}
		msg := fmt.Sprintf("%s key validation failed (HTTP %d)", name, status)
		if detail != "" {
			msg += ": " + detail
		}
		return Invalid(msg)
	}
	if status >= 300 && status < 400 {
		return Inconclusive(fmt.Sprintf(
			"%s key validation inconclusive: HTTP %d redirect never reached the authenticated endpoint",
			name, status))
	}
	return Valid()
}

// extractErrorDetail returns the first non-empty of error.message,
// errors[0].message, message, detail, title (base.ts:258-265).
func extractErrorDetail(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var b validationErrBody
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	for _, cand := range []string{
		b.Error.Message,
		firstErrorsMessage(b.Errors),
		b.Message,
		b.Detail,
		b.Title,
	} {
		if strings.TrimSpace(cand) != "" {
			return cand
		}
	}
	return ""
}

func firstErrorsMessage(errs []struct {
	Message string `json:"message"`
}) string {
	if len(errs) == 0 {
		return ""
	}
	return errs[0].Message
}
