package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// readInferenceBody reads an inference request body under the plane's size cap.
// Both compat surfaces share it so the cap cannot differ between them.
func readInferenceBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBody))
	if err != nil {
		WriteErrorCode(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			"request_too_large", "request body is too large")
		return nil, err
	}
	return body, nil
}

// setRetryAfter stamps the header a backing-off client reads, rounded up so a
// sub-second wait never reads as "retry immediately".
func setRetryAfter(w http.ResponseWriter, retryAt time.Time) {
	seconds := int(math.Ceil(time.Until(retryAt).Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// The Anthropic-compatible surface.
//
// Claude Code and other Anthropic-shaped clients point at this instead of
// /v1/chat/completions, and they are unforgiving about the wire format: a wrong
// stop_reason breaks an agent loop that branches on it, and a wrong SSE event
// name breaks its stream parser outright. Nothing here decides routing - the
// request is translated into the normalised chat request and the engine's
// failover, quota and cooldown path runs unchanged.
//
// This is a port of the reference's routes/anthropic.ts. The features Prowl
// does not implement (request compression, unified model groups, inline image
// downscaling, model-mapped Claude families) are deliberately absent rather
// than stubbed: the model is passed through as the caller sent it, minus the
// synthetic `claude/` discovery prefix.

func (s *Server) registerCompatRoutes() {
	s.mux.HandleFunc("POST /v1/messages", s.RequireMachineKey(s.handleAnthropicMessages))
	s.mux.HandleFunc("POST /v1/messages/count_tokens", s.RequireMachineKey(s.handleAnthropicCountTokens))
	// Claude Code has shipped both names; the reference serves the count_tokens
	// spelling and this accepts the shorter one so a client that guessed wrong
	// still gets a count rather than a 404.
	s.mux.HandleFunc("POST /v1/messages/count", s.RequireMachineKey(s.handleAnthropicCountTokens))
	s.registerResponseCompatRoutes()
}

// ── inbound wire types ──────────────────────────────────────────────────────

// anthropicRequest is the /v1/messages body. Content is kept raw because a
// block may be a plain string or an array of typed blocks, and the conversion
// has to see which.
type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	MaxTokens     *int               `json:"max_tokens"`
	StopSequences []string           `json:"stop_sequences"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	Stream        bool               `json:"stream"`
	Thinking      json.RawMessage    `json:"thinking"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicContentBlock covers every block type this surface understands. The
// unused fields are the price of one struct instead of a discriminated union,
// which keeps the conversion linear.
type anthropicContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text"`

	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// ── request conversion ──────────────────────────────────────────────────────

// convertAnthropicRequest turns the Anthropic body into the normalised chat
// request the engine already routes. It reports content this surface refuses
// rather than silently dropping it, so a caller learns their request was not
// understood instead of getting an answer to a different question.
func convertAnthropicRequest(body *anthropicRequest) (*chatRequestBody, []string, error) {
	out := &chatRequestBody{Params: map[string]any{}}
	out.Model = strings.TrimPrefix(body.Model, "claude/")
	if out.Model == "" {
		out.Model = "auto"
	}
	out.Stream = body.Stream

	var refused []string

	if len(body.System) > 0 {
		if text := anthropicTextOf(body.System); text != "" {
			out.Messages = append(out.Messages, map[string]any{
				"role": "system", "content": text,
			})
		}
	}

	for _, message := range body.Messages {
		converted, rejects := convertAnthropicMessage(message)
		refused = append(refused, rejects...)
		out.Messages = append(out.Messages, converted...)
	}

	// Anthropic requires max_tokens and OpenAI does not, so it is forwarded
	// only when the caller actually set it. Inventing a default here would
	// change what the provider generates for a request that asked for no cap.
	if body.MaxTokens != nil && *body.MaxTokens > 0 {
		out.Params["max_tokens"] = *body.MaxTokens
	}
	if body.Temperature != nil {
		out.Params["temperature"] = *body.Temperature
	}
	if body.TopP != nil {
		out.Params["top_p"] = *body.TopP
	}
	if body.TopK != nil {
		out.Params["top_k"] = *body.TopK
	}
	if len(body.StopSequences) > 0 {
		out.Params["stop"] = body.StopSequences
	}
	if tools := convertAnthropicTools(body.Tools); len(tools) > 0 {
		out.Params["tools"] = tools
		if choice := convertAnthropicToolChoice(body.ToolChoice); choice != nil {
			out.Params["tool_choice"] = choice
		}
	}
	if effort := effortFromAnthropicThinking(body.Thinking); effort != "" {
		out.Params["reasoning_effort"] = effort
	}

	// A refusal outranks an empty result. A request whose only content was a
	// document block produces no messages, and reporting that as "messages is
	// required" would send the caller looking for the wrong mistake.
	if len(refused) > 0 {
		return out, refused, nil
	}
	if len(out.Messages) == 0 {
		return nil, refused, fmt.Errorf("messages is required")
	}
	return out, refused, nil
}

// convertAnthropicMessage renders one inbound message as one or more OpenAI
// messages. A single Anthropic message can legitimately become several: a user
// turn carrying both text and tool results has to split, because the OpenAI
// shape delivers tool results as their own role:"tool" messages.
func convertAnthropicMessage(message anthropicMessage) ([]map[string]any, []string) {
	// A plain string is the common case and needs no block parsing.
	var asString string
	if json.Unmarshal(message.Content, &asString) == nil {
		return []map[string]any{{"role": message.Role, "content": asString}}, nil
	}

	var blocks []anthropicContentBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return []map[string]any{{"role": message.Role, "content": ""}}, nil
	}

	var (
		out      []map[string]any
		refused  []string
		text     strings.Builder
		parts    []map[string]any
		tools    []map[string]any
		results  []map[string]any
		sawImage bool
	)

	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if block.Source == nil {
				continue
			}
			// Anthropic carries the bytes inline as base64; the OpenAI shape
			// wants a data URL, so the media type is folded back in.
			url := "data:" + block.Source.MediaType + ";base64," + block.Source.Data
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
			sawImage = true
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			tools = append(tools, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": arguments,
				},
			})
		case "tool_result":
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": block.ToolUseID,
				"content":      anthropicTextOf(block.Content),
			})
		case "thinking", "redacted_thinking":
			// Reasoning replay is dropped on the way in: our providers do not
			// verify a signature, so forwarding it would be noise at best.
		case "document":
			// No provider here accepts an Anthropic document block, and
			// dropping one would answer a question about a file the model never
			// saw. The caller is told instead.
			refused = append(refused, "document")
		}
	}

	// Tool results lead: the model must see what a tool returned before it
	// reasons about the next step.
	out = append(out, results...)

	if len(tools) > 0 {
		entry := map[string]any{"role": "assistant", "tool_calls": tools}
		if text.Len() > 0 {
			entry["content"] = text.String()
		}
		out = append(out, entry)
		return out, refused
	}

	if sawImage {
		out = append(out, map[string]any{"role": message.Role, "content": parts})
		return out, refused
	}
	if text.Len() > 0 || len(blocks) == 0 {
		out = append(out, map[string]any{"role": message.Role, "content": text.String()})
	}
	return out, refused
}

// anthropicTextOf reads text out of a value that may be a string, an array of
// text blocks, or an array of tool-result content blocks.
func anthropicTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, block := range blocks {
		if block.Type == "text" || block.Type == "" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

// convertAnthropicTools maps Anthropic tool definitions onto the OpenAI shape
// the providers speak. input_schema is passed through verbatim: it is already
// JSON Schema, which is what OpenAI wants.
func convertAnthropicTools(tools []anthropicTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := any(map[string]any{"type": "object"})
		if len(tool.InputSchema) > 0 {
			var parsed any
			if json.Unmarshal(tool.InputSchema, &parsed) == nil {
				schema = parsed
			}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  schema,
			},
		})
	}
	return out
}

// convertAnthropicToolChoice maps Anthropic's {type: auto|any|tool} onto the
// OpenAI vocabulary. "any" means the model must call some tool, which OpenAI
// spells "required" - mistranslating it as "auto" would let a client that
// demanded a tool call get a plain answer.
func convertAnthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if choice.Name == "" {
			return nil
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": choice.Name},
		}
	default:
		return nil
	}
}

// effortFromAnthropicThinking maps Anthropic's thinking budget onto the
// reasoning knob the providers accept (anthropic.ts:118-131). An empty string
// means "send no knob", which is different from "send none": a caller who
// asked for adaptive thinking should get the provider's default rather than a
// disabled reasoning path.
func effortFromAnthropicThinking(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var thinking struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens"`
	}
	if json.Unmarshal(raw, &thinking) != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "disabled", "off", "none":
		return "none"
	}
	if thinking.BudgetTokens == nil {
		if thinking.Type == "adaptive" || thinking.Type == "auto" {
			return ""
		}
		return "medium"
	}
	switch budget := *thinking.BudgetTokens; {
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// ── handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)

	body, err := readInferenceBody(w, r)
	if err != nil {
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request: body is not valid JSON")
		return
	}

	converted, refused, err := convertAnthropicRequest(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request: "+err.Error())
		return
	}
	if len(refused) > 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"Unsupported content block: "+strings.Join(refused, ", "))
		return
	}

	shaper := anthropicShaper{
		model:       requestedModelLabel(req.Model),
		inputTokens: int(estimateTokens(converted)),
	}
	s.runInference(w, r, converted, shaper, requestID)
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readInferenceBody(w, r)
	if err != nil {
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request")
		return
	}
	converted, refused, err := convertAnthropicRequest(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid request")
		return
	}
	if len(refused) > 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"Unsupported content block: "+strings.Join(refused, ", "))
		return
	}
	// A heuristic, and the reference says so too: no tokenizer runs here. The
	// number is deliberately reported under the name the client expects rather
	// than dressed up as an exact count.
	WriteJSON(w, http.StatusOK, map[string]int{"input_tokens": int(estimateTokens(converted))})
}

// requestedModelLabel echoes the model the caller named, for the response's
// model field. An Anthropic client shows it back to the user, so reporting our
// internal routing decision there would be a confusing lie; the real answer is
// in X-Routed-Via.
func requestedModelLabel(model string) string {
	if model == "" {
		return "auto"
	}
	return model
}

// ── response rendering ──────────────────────────────────────────────────────

// anthropicStopReason maps an OpenAI finish_reason onto Anthropic's vocabulary
// (anthropic.ts:384-387). This is load-bearing: an agent branches on it, so a
// truncated answer reported as end_turn makes it treat a cut-off reply as
// complete. stop_sequence is never produced because the reference never
// produces it either - a matched stop sequence arrives as a plain "stop".
func anthropicStopReason(finishReason string, hadToolCalls bool) string {
	if hadToolCalls || finishReason == "tool_calls" || finishReason == "function_call" {
		return "tool_use"
	}
	if finishReason == "length" {
		return "max_tokens"
	}
	return "end_turn"
}

type anthropicShaper struct {
	// model is the name the caller asked for, echoed back in the response.
	model string
	// inputTokens is the pre-flight estimate, used only where the provider
	// reports no usage of its own.
	inputTokens int
}

func (a anthropicShaper) buffered(resp *provider.ChatResponse) ([]byte, error) {
	var message provider.RespMessage
	finish := ""
	if len(resp.Choices) > 0 {
		message = resp.Choices[0].Message
		finish = resp.Choices[0].FinishReason
	}
	blocks, hadTools := anthropicBlocks(message)

	usage := map[string]int{"input_tokens": a.inputTokens, "output_tokens": 0}
	if resp.Usage != nil {
		usage = map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		}
	}

	return json.Marshal(map[string]any{
		"id":            anthropicMessageID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         a.model,
		"content":       blocks,
		"stop_reason":   anthropicStopReason(finish, hadTools),
		"stop_sequence": nil,
		"usage":         usage,
	})
}

func (a anthropicShaper) newStream() streamShaper { return &anthropicStream{shaper: a} }

func (a anthropicShaper) writeError(w http.ResponseWriter, status int, kind gateway.ExhaustionKind, _ string, message string, retryAt time.Time) {
	writeAnthropicExhaustion(w, status, kind, message, retryAt)
}

// anthropicBlocks renders an assistant message as content blocks, in the order
// Anthropic expects: reasoning first, then text, then tool calls.
func anthropicBlocks(message provider.RespMessage) ([]map[string]any, bool) {
	blocks := make([]map[string]any, 0, 3)

	reasoning := jsonText(firstNonEmpty(message.Reasoning, message.ReasoningAlt))
	if reasoning != "" {
		blocks = append(blocks, map[string]any{
			"type": "thinking", "thinking": reasoning, "signature": "",
		})
	}

	if text := jsonText(message.Content); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}

	tools := parseToolCalls(message.ToolCalls)
	for _, call := range tools {
		block := map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Input,
		}
		blocks = append(blocks, block)
	}
	// A message with nothing renderable still needs a block, or the client
	// shows an assistant turn with no content at all.
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks, len(tools) > 0
}

type toolCall struct {
	ID    string
	Name  string
	Input any
	// Args accumulates the streamed argument fragments, which are not valid
	// JSON until the final one lands. The buffered path leaves it empty and
	// carries the parsed object in Input instead.
	Args string
}

// parseToolCalls reads the OpenAI tool_calls array into a shape the Anthropic
// renderer can emit. Arguments arrive as a JSON string and are handed on as the
// parsed object, because Anthropic's tool_use.input is an object, not a string.
func parseToolCalls(raw json.RawMessage) []toolCall {
	if len(raw) == 0 {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return nil
	}
	out := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		input := any(map[string]any{})
		if call.Function.Arguments != "" {
			var parsed any
			if json.Unmarshal([]byte(call.Function.Arguments), &parsed) == nil {
				input = parsed
			}
		}
		out = append(out, toolCall{ID: call.ID, Name: call.Function.Name, Input: input})
	}
	return out
}

// jsonText reads text from a value that may be a bare string or an array of
// typed content parts, since providers disagree on which they send.
func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range parts {
		sb.WriteString(part.Text)
	}
	return sb.String()
}

func firstNonEmpty(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

// anthropicMessageID gives the response an Anthropic-shaped id. Providers mint
// their own ids in their own formats, and a client that groups turns by the
// `msg_` prefix would not recognise one.
func anthropicMessageID(upstream string) string {
	if strings.HasPrefix(upstream, "msg_") {
		return upstream
	}
	return "msg_" + newRequestID()
}

// ── streaming ───────────────────────────────────────────────────────────────

// anthropicStream renders the SSE sequence Claude Code expects:
//
//	message_start → (content_block_start → content_block_delta* →
//	content_block_stop)* → message_delta → message_stop
//
// The event NAMES matter more than the payloads: a client's parser switches on
// them, so an OpenAI-shaped chunk written into this stream is not a degraded
// answer, it is an unparseable one.
type anthropicStream struct {
	shaper anthropicShaper

	started    bool
	textOpen   bool
	textIndex  int
	nextIndex  int
	reasonBuf  string
	outputChar int
	finish     string
	tools      []toolCall
	// toolIndexes tracks which accumulator slot each streamed tool call
	// belongs to, since a provider may interleave their argument fragments.
	toolSlots map[int]int
}

func (a *anthropicStream) frame(chunk *provider.ChatChunk) []byte {
	var delta struct {
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
		Reasoning        string `json:"reasoning"`
		ToolCalls        []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}

	// The parsed delta is the primary source; a provider that sends an
	// unmodelled shape falls back to the raw frame so reasoning written under
	// a different key is still not lost.
	if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta) > 0 {
		_ = json.Unmarshal(chunk.Choices[0].Delta, &delta)
	}
	if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
		a.finish = *chunk.Choices[0].FinishReason
	}

	var out []byte

	reasoning := delta.ReasoningContent
	if reasoning == "" {
		reasoning = delta.Reasoning
	}
	if reasoning != "" {
		a.reasonBuf += reasoning
	}

	for _, call := range delta.ToolCalls {
		slot, ok := a.toolSlots[call.Index]
		if !ok {
			a.tools = append(a.tools, toolCall{Input: map[string]any{}})
			slot = len(a.tools) - 1
			if a.toolSlots == nil {
				a.toolSlots = map[int]int{}
			}
			a.toolSlots[call.Index] = slot
		}
		if call.ID != "" {
			a.tools[slot].ID = call.ID
		}
		if call.Function.Name != "" {
			a.tools[slot].Name = call.Function.Name
		}
		if call.Function.Arguments != "" {
			a.tools[slot].Args += call.Function.Arguments
		}
	}

	if delta.Content != "" {
		out = append(out, a.ensureStart()...)
		out = append(out, a.flushThinking()...)
		out = append(out, a.openText()...)
		out = append(out, sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": delta.Content},
		})...)
		a.outputChar += len(delta.Content)
	}

	return out
}

// done closes every open block and reports the terminal reason. Tool calls are
// emitted here rather than as they stream because their arguments arrive in
// fragments that are not valid JSON until the last one lands.
func (a *anthropicStream) done() []byte {
	var out []byte

	// A reasoning-only or tool-only completion can reach done() without a
	// content frame ever having started the message, since only text starts
	// it. Start it and flush any buffered thinking so the event order stays
	// valid: message_start must precede every block and message_delta.
	out = append(out, a.ensureStart()...)
	out = append(out, a.flushThinking()...)
	out = append(out, a.closeText()...)

	for _, call := range a.tools {
		index := a.nextIndex
		a.nextIndex++
		out = append(out, sseEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
		})...)
		// The accumulated fragments are already the argument JSON string that
		// input_json_delta.partial_json carries; a call with no arguments is
		// an empty object, not the empty string a client cannot parse.
		arguments := call.Args
		if arguments == "" {
			arguments = "{}"
		}
		out = append(out, sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
		})...)
		out = append(out, sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})...)
	}

	stopReason := anthropicStopReason(a.finish, len(a.tools) > 0)
	out = append(out, sseEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		// Estimated from characters emitted: the provider's real usage arrives
		// in a frame this surface does not see, and reporting zero would be a
		// wrong number rather than an approximate one.
		"usage": map[string]any{"output_tokens": a.outputChar / 4},
	})...)
	out = append(out, sseEvent("message_stop", map[string]any{"type": "message_stop"})...)
	return out
}

func (a *anthropicStream) streamError(message string) []byte {
	out := a.ensureStart()
	out = append(out, sseEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})...)
	return out
}

// ensureStart emits message_start exactly once, on the first meaningful frame.
// Until then the attempt can still fail over invisibly, which is why nothing
// is written for an empty opening chunk.
func (a *anthropicStream) ensureStart() []byte {
	if a.started {
		return nil
	}
	a.started = true
	return sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": anthropicMessageID(""), "type": "message", "role": "assistant",
			"model": a.shaper.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": a.shaper.inputTokens, "output_tokens": 0},
		},
	})
}

func (a *anthropicStream) flushThinking() []byte {
	if a.reasonBuf == "" {
		return nil
	}
	index := a.nextIndex
	a.nextIndex++
	out := sseEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
	out = append(out, sseEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": a.reasonBuf},
	})...)
	out = append(out, sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})...)
	a.outputChar += len(a.reasonBuf)
	a.reasonBuf = ""
	return out
}

func (a *anthropicStream) openText() []byte {
	if a.textOpen {
		return nil
	}
	a.textOpen = true
	a.textIndex = a.nextIndex
	a.nextIndex++
	return sseEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         a.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (a *anthropicStream) closeText() []byte {
	if !a.textOpen {
		return nil
	}
	a.textOpen = false
	return sseEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": a.textIndex,
	})
}

// sseEvent renders one Anthropic SSE frame. The event name is part of the
// frame, unlike OpenAI's bare `data:` lines.
func sseEvent(event string, payload any) []byte {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	out := make([]byte, 0, len(event)+len(data)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, data...)
	out = append(out, '\n', '\n')
	return out
}

// ── error envelopes ─────────────────────────────────────────────────────────

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	WriteJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType, "message": message},
	})
}

// anthropicErrorType remaps the shared exhaustion vocabulary onto Anthropic's
// (anthropic.ts:161-167). The two vocabularies genuinely differ: "auth" and
// "upstream" are not Anthropic error types at all, and a client branching on
// them would not recognise the failure.
func anthropicErrorType(kind gateway.ExhaustionKind, status int) string {
	switch kind {
	case gateway.ExhaustAuth, gateway.ExhaustUpstream:
		return "api_error"
	case gateway.ExhaustUnavailable:
		return "overloaded_error"
	case gateway.ExhaustContextTooLarge:
		return "request_too_large"
	case gateway.ExhaustModelNotFound:
		return "not_found_error"
	case gateway.ExhaustRateLimit:
		return "rate_limit_error"
	case gateway.ExhaustBadRequest:
		return "invalid_request_error"
	}
	return string(exhaustionType(status))
}

func writeAnthropicExhaustion(w http.ResponseWriter, status int, kind gateway.ExhaustionKind, message string, retryAt time.Time) {
	if !retryAt.IsZero() {
		setRetryAfter(w, retryAt)
	}
	writeAnthropicError(w, status, anthropicErrorType(kind, status), message)
}
