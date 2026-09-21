package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// The OpenAI Responses surface and the legacy completions surface.
//
// Both are translations onto the same engine path as /v1/chat/completions:
// they parse their own dialect, run the normalised request through failover,
// quota and cooldown unchanged, and render the answer back in their own shape.
//
// Ported from the reference's routes/responses.ts and the legacy handler in
// routes/proxy.ts. Parameters the reference accepts and ignores - metadata,
// previous_response_id, store - are accepted and ignored here too, with one
// deliberate exception noted on the handler: continuation is refused rather
// than silently dropped.

func (s *Server) registerResponseCompatRoutes() {
	s.mux.HandleFunc("POST /v1/responses", s.RequireMachineKey(s.handleResponses))
	s.mux.HandleFunc("POST /v1/completions", s.RequireMachineKey(s.handleLegacyCompletions))
}

// ── legacy completions ──────────────────────────────────────────────────────

// legacyCompletionRequest is the prompt-in, text-out shape editor autocomplete
// clients still send.
type legacyCompletionRequest struct {
	Model       string          `json:"model"`
	Prompt      json.RawMessage `json:"prompt"`
	Suffix      *string         `json:"suffix"`
	MaxTokens   *int            `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	Stop        json.RawMessage `json:"stop"`
	Stream      bool            `json:"stream"`
}

// legacyCompletionMaxTokens is the reference's default for this surface
// (proxy.ts:1030). Autocomplete wants a short answer, and no cap at all would
// let a chat model write an essay into a ghost-text box.
const legacyCompletionMaxTokens = 128

func (s *Server) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)

	body, err := readInferenceBody(w, r)
	if err != nil {
		return
	}
	var req legacyCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid request: body is not valid JSON")
		return
	}

	prompt := legacyPromptText(req.Prompt)
	if prompt == "" {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "prompt is required")
		return
	}

	suffix := ""
	if req.Suffix != nil {
		suffix = *req.Suffix
	}

	converted := &chatRequestBody{
		Model:    modelOrAuto(req.Model),
		Stream:   req.Stream,
		Params:   map[string]any{},
		Messages: legacyPromptMessages(prompt, suffix),
	}

	maxTokens := legacyCompletionMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	converted.Params["max_tokens"] = maxTokens
	if req.Temperature != nil {
		converted.Params["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		converted.Params["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		var stop any
		if json.Unmarshal(req.Stop, &stop) == nil {
			converted.Params["stop"] = stop
		}
	}

	s.runInference(w, r, converted, legacyCompletionShaper{model: modelOrAuto(req.Model)}, requestID)
}

// legacyPromptText accepts the two prompt shapes OpenAI allows: one string, or
// an array of strings. A token-array prompt is not supported by any provider
// here, so it reads as empty and the caller is told the prompt is missing
// rather than being served a completion of nothing.
func legacyPromptText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return asString
	}
	var asList []string
	if json.Unmarshal(raw, &asList) == nil {
		return strings.Join(asList, "")
	}
	return ""
}

// legacyPromptMessages reproduces the reference's autocomplete framing
// (proxy.ts completionPromptToMessages). The system message is what stops a
// chat model answering conversationally into a ghost-text box.
func legacyPromptMessages(prompt, suffix string) []map[string]any {
	instruction := "You are a code autocomplete engine. " +
		"Complete at the cursor and return only the text to insert. " +
		"Do not include markdown fences, explanations, or repeat surrounding code."

	user := "Prefix before cursor:\n" + prompt + "\n\nCompletion to insert:"
	if suffix != "" {
		user = "Prefix before cursor:\n" + prompt +
			"\n\nSuffix after cursor:\n" + suffix + "\n\nCompletion to insert:"
	}
	return []map[string]any{
		{"role": "system", "content": instruction},
		{"role": "user", "content": user},
	}
}

// legacyCompletionShaper renders the text_completion shape. Errors reuse the
// OpenAI envelope, which is what this surface's clients parse.
type legacyCompletionShaper struct {
	openAIShaper
	model string
}

func (l legacyCompletionShaper) buffered(resp *provider.ChatResponse) ([]byte, error) {
	text := ""
	finish := any(nil)
	if len(resp.Choices) > 0 {
		text = jsonText(resp.Choices[0].Message.Content)
		if reason := resp.Choices[0].FinishReason; reason != "" {
			finish = reason
		}
	}
	payload := map[string]any{
		"id":      legacyCompletionID(resp.ID),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   l.model,
		"choices": []map[string]any{{
			"text": text, "index": 0, "logprobs": nil, "finish_reason": finish,
		}},
	}
	if resp.Usage != nil {
		payload["usage"] = map[string]int{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		}
	}
	return json.Marshal(payload)
}

func (l legacyCompletionShaper) newStream() streamShaper {
	return &legacyCompletionStream{model: l.model}
}

func legacyCompletionID(upstream string) string {
	if strings.HasPrefix(upstream, "cmpl-") {
		return upstream
	}
	if upstream != "" {
		return "cmpl-" + upstream
	}
	return "cmpl-" + newRequestID()
}

// legacyCompletionStream re-frames each chat delta as a text_completion chunk,
// which is the only shape this surface's clients parse.
type legacyCompletionStream struct {
	model string
	id    string
}

func (l *legacyCompletionStream) frame(chunk *provider.ChatChunk) []byte {
	var delta struct {
		Content string `json:"content"`
	}
	if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta) > 0 {
		_ = json.Unmarshal(chunk.Choices[0].Delta, &delta)
	}
	finish := any(nil)
	if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
		finish = *chunk.Choices[0].FinishReason
	}
	if delta.Content == "" && finish == nil {
		return nil
	}
	if l.id == "" {
		l.id = legacyCompletionID(chunk.ID)
	}
	payload, err := json.Marshal(map[string]any{
		"id":      l.id,
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   l.model,
		"choices": []map[string]any{{
			"text": delta.Content, "index": 0, "logprobs": nil, "finish_reason": finish,
		}},
	})
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), payload...), '\n', '\n')
}

func (l *legacyCompletionStream) done() []byte { return []byte("data: [DONE]\n\n") }

func (l *legacyCompletionStream) streamError(message string) []byte {
	return openAIStream{}.streamError(message)
}

// ── Responses API ───────────────────────────────────────────────────────────

type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       *string         `json:"instructions"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Stream             bool            `json:"stream"`
	PreviousResponseID *string         `json:"previous_response_id"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)

	body, err := readInferenceBody(w, r)
	if err != nil {
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid request: body is not valid JSON")
		return
	}

	// Continuation is refused rather than ignored. This gateway stores no
	// response state, so honouring the parameter is impossible - and silently
	// dropping it would answer without the conversation the client believes it
	// referenced, which is a wrong answer rather than an error.
	if req.PreviousResponseID != nil && *req.PreviousResponseID != "" {
		WriteErrorCode(w, http.StatusBadRequest, TypeInvalidRequest, "unsupported_parameter",
			"previous_response_id is not supported: this gateway stores no response state, "+
				"so send the prior turns in input instead")
		return
	}

	messages, err := responsesInputMessages(req.Input)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid request: "+err.Error())
		return
	}
	if req.Instructions != nil && *req.Instructions != "" {
		messages = append([]map[string]any{
			{"role": "system", "content": *req.Instructions},
		}, messages...)
	}
	if len(messages) == 0 {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "input is required")
		return
	}

	converted := &chatRequestBody{
		Model:    modelOrAuto(req.Model),
		Stream:   req.Stream,
		Params:   map[string]any{},
		Messages: messages,
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		converted.Params["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		converted.Params["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		converted.Params["top_p"] = *req.TopP
	}
	if tools := responsesToolsToChat(req.Tools); len(tools) > 0 {
		converted.Params["tools"] = tools
	}
	if choice := responsesToolChoiceToChat(req.ToolChoice); choice != nil {
		converted.Params["tool_choice"] = choice
	}

	shaper := responsesShaper{
		model:       modelOrAuto(req.Model),
		responseID:  "resp_" + newRequestID(),
		inputTokens: int(estimateTokens(converted)),
	}
	s.runInference(w, r, converted, shaper, requestID)
}

// responsesInputMessages accepts the three input shapes the Responses API
// allows: a bare string, an array of messages, and an array of content parts
// belonging to a single user turn.
func responsesInputMessages(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		if asString == "" {
			return nil, nil
		}
		return []map[string]any{{"role": "user", "content": asString}}, nil
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errInputShape
	}

	out := make([]map[string]any, 0, len(items))
	var loose []map[string]any
	for _, item := range items {
		switch itemType, _ := item["type"].(string); itemType {
		case "function_call", "function_call_output":
			// A replayed tool turn. Flush any pending loose user parts first so
			// ordering survives, then translate the call or its result into the
			// assistant/tool messages a chat provider understands.
			if len(loose) > 0 {
				out = append(out, map[string]any{"role": "user", "content": responsesPartList(loose)})
				loose = nil
			}
			out = append(out, responsesFunctionItem(item)...)
			continue
		}
		role, hasRole := item["role"].(string)
		if !hasRole {
			// A bare content part belongs to a user turn, which is how the
			// single-message form is written.
			loose = append(loose, item)
			continue
		}
		content := responsesContent(item["content"])
		out = append(out, map[string]any{"role": role, "content": content})
	}
	if len(loose) > 0 {
		out = append(out, map[string]any{"role": "user", "content": responsesPartList(loose)})
	}
	return out, nil
}

// responsesFunctionItem turns a Responses function_call or function_call_output
// item into the chat messages a provider understands: a function_call becomes
// an assistant turn carrying a tool_calls entry, and a function_call_output
// becomes the matching role:"tool" result. Dropping them would lose both the
// model's call and the tool's answer, so a replayed loop would reason as if the
// tool had never run.
func responsesFunctionItem(item map[string]any) []map[string]any {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID, _ = item["id"].(string)
	}
	switch itemType, _ := item["type"].(string); itemType {
	case "function_call":
		name, _ := item["name"].(string)
		return []map[string]any{{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": responsesArgumentsString(item["arguments"])},
			}},
		}}
	case "function_call_output":
		return []map[string]any{{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      responsesOutputText(item["output"]),
		}}
	}
	return nil
}

// responsesArgumentsString normalises a function call's arguments to the JSON
// string a chat tool_call carries. The Responses API sends them as a string,
// but a client may echo back the parsed object, so both are accepted.
func responsesArgumentsString(value any) string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return "{}"
		}
		return typed
	case nil:
		return "{}"
	default:
		if raw, err := json.Marshal(typed); err == nil {
			return string(raw)
		}
		return "{}"
	}
}

// responsesOutputText reads a function_call_output's result, which the API
// allows as a bare string or an array of output_text parts.
func responsesOutputText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var sb strings.Builder
		for _, entry := range typed {
			part, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				sb.WriteString(text)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// responsesToolsToChat maps the Responses API's flat function tools onto the
// nested { type, function } shape chat providers expect. A tool already in the
// nested shape passes through; a built-in Responses tool (web_search and the
// like) has no chat equivalent and is dropped rather than sent as a shape a
// provider would reject.
func responsesToolsToChat(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if _, nested := item["function"]; nested {
			out = append(out, item)
			continue
		}
		if itemType, _ := item["type"].(string); itemType != "" && itemType != "function" {
			continue
		}
		fn := map[string]any{}
		for _, field := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := item[field]; ok {
				fn[field] = value
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// responsesToolChoiceToChat maps the Responses tool_choice onto chat's. The
// string forms (auto|none|required) are shared; the forced-function form is
// flat here ({type:function,name}) and nested in chat.
func responsesToolChoiceToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		if asString == "" {
			return nil
		}
		return asString
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	if _, nested := obj["function"]; nested {
		return obj
	}
	if choiceType, _ := obj["type"].(string); choiceType == "function" {
		if name, ok := obj["name"]; ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return obj
}

var errInputShape = &inputShapeError{}

type inputShapeError struct{}

func (*inputShapeError) Error() string {
	return "input must be a string, an array of messages, or an array of content parts"
}

// responsesContent renders one message's content. Text-only content collapses
// back to a plain string because that is what the chat providers expect; an
// array survives only when a part cannot be represented as text.
func responsesContent(value any) any {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]map[string]any, 0, len(typed))
		for _, entry := range typed {
			if part, ok := entry.(map[string]any); ok {
				parts = append(parts, part)
			}
		}
		return responsesPartList(parts)
	default:
		return ""
	}
}

// responsesPartList maps Responses content parts onto chat content blocks.
// input_text, output_text and summary_text all carry plain text; a refusal is
// folded in as text so a replayed assistant turn is not silently emptied.
func responsesPartList(parts []map[string]any) any {
	var (
		text    strings.Builder
		blocks  []map[string]any
		hasFile bool
	)
	for _, part := range parts {
		partType, _ := part["type"].(string)
		switch partType {
		case "text", "input_text", "output_text", "summary_text", "":
			if value, ok := part["text"].(string); ok {
				text.WriteString(value)
				blocks = append(blocks, map[string]any{"type": "text", "text": value})
			}
		case "refusal":
			if value, ok := part["refusal"].(string); ok {
				text.WriteString(value)
				blocks = append(blocks, map[string]any{"type": "text", "text": value})
			}
		case "input_image", "image_url", "image", "computer_screenshot":
			if url := responsesPartImageURL(part); url != "" {
				blocks = append(blocks, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
				hasFile = true
			}
		}
	}
	if !hasFile {
		return text.String()
	}
	return blocks
}

// responsesPartImageURL reads the image out of the shapes the Responses API
// and the chat API each use for one.
func responsesPartImageURL(part map[string]any) string {
	if url, ok := part["image_url"].(string); ok {
		return url
	}
	if nested, ok := part["image_url"].(map[string]any); ok {
		if url, ok := nested["url"].(string); ok {
			return url
		}
	}
	if url, ok := part["url"].(string); ok {
		return url
	}
	return ""
}

func modelOrAuto(model string) string {
	if strings.TrimSpace(model) == "" {
		return "auto"
	}
	return model
}

// responsesShaper renders the Responses object shape.
type responsesShaper struct {
	openAIShaper
	model       string
	responseID  string
	inputTokens int
}

func (rs responsesShaper) buffered(resp *provider.ChatResponse) ([]byte, error) {
	text, reasoning := "", ""
	var calls []toolCall
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		text = jsonText(msg.Content)
		reasoning = jsonText(firstNonEmpty(msg.Reasoning, msg.ReasoningAlt))
		calls = parseToolCalls(msg.ToolCalls)
	}
	// normalizeChoices folds hidden reasoning into content when the model
	// produced no visible answer, so a reasoning-only turn arrives with content
	// equal to its reasoning. Undo that here: the Responses surface carries
	// reasoning in its own output item, and leaving it in content would speak
	// the model's private reasoning as the assistant's answer.
	if reasoning != "" && text == reasoning {
		text = ""
	}

	prompt, completion := rs.inputTokens, 0
	if resp.Usage != nil {
		prompt, completion = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	return json.Marshal(rs.object(text, reasoning, calls, "completed", prompt, completion))
}

// object builds the Responses envelope (responses.ts buildResponseObject).
func (rs responsesShaper) object(text, reasoning string, calls []toolCall, status string, prompt, completion int) map[string]any {
	output := make([]map[string]any, 0, 2+len(calls))
	if reasoning != "" {
		// A reasoning output item precedes the message so a client reading the
		// output array in order sees the thinking before the answer, and a
		// reasoning-only turn still carries a non-empty output.
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + newRequestID(),
			"summary": []map[string]any{{"type": "summary_text", "text": reasoning}},
		})
	}
	if text != "" {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      "msg_" + newRequestID(),
			"status":  "completed",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	for _, call := range calls {
		arguments := "{}"
		if raw, err := json.Marshal(call.Input); err == nil {
			arguments = string(raw)
		}
		output = append(output, map[string]any{
			"type": "function_call", "id": "fc_" + newRequestID(), "call_id": call.ID,
			"name": call.Name, "arguments": arguments, "status": "completed",
		})
	}
	return map[string]any{
		"id":          rs.responseID,
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      status,
		"model":       rs.model,
		"output":      output,
		"output_text": text,
		"usage": map[string]any{
			"input_tokens":          prompt,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens":         completion,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          prompt + completion,
		},
	}
}

func (rs responsesShaper) newStream() streamShaper {
	return &responsesStream{shaper: rs}
}

// responsesStream renders the Responses event sequence. The names are the
// contract: a client switches on them, so an OpenAI chat chunk written here is
// unparseable rather than merely different.
type responsesStream struct {
	shaper          responsesShaper
	started         bool // response.created/in_progress emitted
	reasoningOpen   bool // the reasoning output item was opened
	reasoning       strings.Builder
	reasoningItemID string // the reasoning item id
	reasoningIndex  int    // the reasoning item's output_index
	textStarted     bool   // the assistant message output item was opened
	text            strings.Builder
	finish          string
	itemID          string // the message item id
	textIndex       int    // the message item's output_index
	nextIndex       int    // next output_index to assign
	tools           []*responsesStreamTool
	toolSlots       map[int]int
}

// responsesStreamTool accumulates one streamed function call. Its arguments
// arrive in fragments across frames, so the final value is only known at done.
type responsesStreamTool struct {
	callID    string
	name      string
	args      strings.Builder
	itemID    string
	outputIdx int
}

func (rs *responsesStream) frame(chunk *provider.ChatChunk) []byte {
	var delta struct {
		Content      string `json:"content"`
		Reasoning    string `json:"reasoning_content"`
		ReasoningAlt string `json:"reasoning"`
		ToolCalls    []struct {
			Index    int    `json:"index"`
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta) > 0 {
		_ = json.Unmarshal(chunk.Choices[0].Delta, &delta)
	}
	if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
		rs.finish = *chunk.Choices[0].FinishReason
	}
	reasoning := delta.Reasoning
	if reasoning == "" {
		reasoning = delta.ReasoningAlt
	}

	var out []byte
	if delta.Content != "" || reasoning != "" || len(delta.ToolCalls) > 0 {
		out = append(out, rs.ensureStart()...)
	}

	if reasoning != "" {
		out = append(out, rs.openReasoning()...)
		rs.reasoning.WriteString(reasoning)
		out = append(out, sseEvent("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": rs.reasoningItemID,
			"output_index": rs.reasoningIndex, "summary_index": 0, "delta": reasoning,
		})...)
	}

	if delta.Content != "" {
		out = append(out, rs.openText()...)
		rs.text.WriteString(delta.Content)
		out = append(out, sseEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": rs.itemID,
			"output_index": rs.textIndex, "content_index": 0, "delta": delta.Content,
		})...)
	}

	for _, call := range delta.ToolCalls {
		out = append(out, rs.toolFrame(call.Index, call.ID, call.Function.Name, call.Function.Arguments)...)
	}
	return out
}

// ensureStart emits response.created and response.in_progress once, on the
// first frame carrying anything a client can use - reasoning, text or a tool
// call. done also calls it so a stream that committed on nothing renderable
// still opens its lifecycle before response.completed.
func (rs *responsesStream) ensureStart() []byte {
	if rs.started {
		return nil
	}
	rs.started = true
	var out []byte
	out = append(out, sseEvent("response.created", map[string]any{
		"type":     "response.created",
		"response": rs.shaper.object("", "", nil, "in_progress", rs.shaper.inputTokens, 0),
	})...)
	out = append(out, sseEvent("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": rs.shaper.object("", "", nil, "in_progress", rs.shaper.inputTokens, 0),
	})...)
	return out
}

// openReasoning opens the reasoning output item once, on the first reasoning
// delta, and opens its summary part so the summary-text deltas have a container
// to stream into. A turn with no hidden reasoning never opens it.
func (rs *responsesStream) openReasoning() []byte {
	if rs.reasoningOpen {
		return nil
	}
	rs.reasoningOpen = true
	rs.reasoningItemID = "rs_" + newRequestID()
	rs.reasoningIndex = rs.nextIndex
	rs.nextIndex++
	var out []byte
	out = append(out, sseEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": rs.reasoningIndex,
		"item": map[string]any{
			"type": "reasoning", "id": rs.reasoningItemID, "status": "in_progress",
			"summary": []any{},
		},
	})...)
	out = append(out, sseEvent("response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "item_id": rs.reasoningItemID,
		"output_index": rs.reasoningIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})...)
	return out
}

// openText opens the assistant message output item once, on the first text
// delta. A tool-only answer never opens it.
func (rs *responsesStream) openText() []byte {
	if rs.textStarted {
		return nil
	}
	rs.textStarted = true
	rs.itemID = "msg_" + newRequestID()
	rs.textIndex = rs.nextIndex
	rs.nextIndex++
	var out []byte
	out = append(out, sseEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": rs.textIndex,
		"item": map[string]any{
			"type": "message", "id": rs.itemID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})...)
	out = append(out, sseEvent("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": rs.itemID,
		"output_index": rs.textIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})...)
	return out
}

// toolFrame opens a function_call output item on first sight of a tool index
// and streams its argument fragments as function_call_arguments.delta events.
func (rs *responsesStream) toolFrame(index int, id, name, args string) []byte {
	slot, ok := rs.toolSlots[index]
	var out []byte
	if !ok {
		tool := &responsesStreamTool{outputIdx: rs.nextIndex, itemID: "fc_" + newRequestID(), callID: id, name: name}
		rs.nextIndex++
		rs.tools = append(rs.tools, tool)
		slot = len(rs.tools) - 1
		if rs.toolSlots == nil {
			rs.toolSlots = map[int]int{}
		}
		rs.toolSlots[index] = slot
		out = append(out, sseEvent("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": tool.outputIdx,
			"item": map[string]any{
				"type": "function_call", "id": tool.itemID, "call_id": tool.callID,
				"name": tool.name, "arguments": "", "status": "in_progress",
			},
		})...)
	}
	tool := rs.tools[slot]
	if id != "" {
		tool.callID = id
	}
	if name != "" {
		tool.name = name
	}
	if args != "" {
		tool.args.WriteString(args)
		out = append(out, sseEvent("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      tool.itemID,
			"output_index": tool.outputIdx,
			"delta":        args,
		})...)
	}
	return out
}

func (rs *responsesStream) done() []byte {
	text := rs.text.String()
	reasoning := rs.reasoning.String()
	// A stream that committed on a frame carrying only a finish reason never
	// opened its lifecycle in frame(); open it now so response.completed is
	// never the first event a client sees.
	out := rs.ensureStart()
	if rs.reasoningOpen {
		out = append(out, sseEvent("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": rs.reasoningItemID,
			"output_index": rs.reasoningIndex, "summary_index": 0, "text": reasoning,
		})...)
		out = append(out, sseEvent("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": rs.reasoningItemID,
			"output_index": rs.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": reasoning},
		})...)
		out = append(out, sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": rs.reasoningIndex,
			"item": map[string]any{
				"type": "reasoning", "id": rs.reasoningItemID, "status": "completed",
				"summary": []map[string]any{{"type": "summary_text", "text": reasoning}},
			},
		})...)
	}
	if rs.textStarted {
		out = append(out, sseEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": rs.itemID,
			"output_index": rs.textIndex, "content_index": 0, "text": text,
		})...)
		out = append(out, sseEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": rs.itemID,
			"output_index": rs.textIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		})...)
		out = append(out, sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": rs.textIndex,
			"item": map[string]any{
				"type": "message", "id": rs.itemID, "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
			},
		})...)
	}

	calls := make([]toolCall, 0, len(rs.tools))
	for _, tool := range rs.tools {
		arguments := tool.args.String()
		if arguments == "" {
			arguments = "{}"
		}
		out = append(out, sseEvent("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      tool.itemID,
			"output_index": tool.outputIdx,
			"arguments":    arguments,
		})...)
		out = append(out, sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": tool.outputIdx,
			"item": map[string]any{
				"type": "function_call", "id": tool.itemID, "call_id": tool.callID,
				"name": tool.name, "arguments": arguments, "status": "completed",
			},
		})...)
		var input any
		if json.Unmarshal([]byte(arguments), &input) != nil {
			input = map[string]any{}
		}
		calls = append(calls, toolCall{ID: tool.callID, Name: tool.name, Input: input})
	}

	// Output tokens are estimated: the provider reports usage in a frame this
	// surface never sees, and zero would be a wrong number rather than an
	// approximate one.
	out = append(out, sseEvent("response.completed", map[string]any{
		"type":     "response.completed",
		"response": rs.shaper.object(text, reasoning, calls, "completed", rs.shaper.inputTokens, len(text)/4),
	})...)
	return out
}

func (rs *responsesStream) streamError(message string) []byte {
	return sseEvent("response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": rs.shaper.responseID, "object": "response", "status": "failed",
			"error": map[string]any{"code": "upstream_error", "message": message},
		},
	})
}

// writeError keeps the OpenAI envelope: the Responses API is an OpenAI family
// surface and its SDKs parse that shape. The type is derived from the engine's
// coarse kind so a caller can still branch on what went wrong.
func (rs responsesShaper) writeError(w http.ResponseWriter, status int, kind gateway.ExhaustionKind, code, message string, retryAt time.Time) {
	_ = kind
	rs.openAIShaper.writeError(w, status, kind, code, message, retryAt)
}
