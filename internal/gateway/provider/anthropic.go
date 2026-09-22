package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	anthropicoauth "github.com/neur0map/prowl/internal/gateway/logins/anthropic"
)

const anthropicBaseURL = "https://api.anthropic.com/v1"

type anthropicProvider struct{ client *http.Client }

func newAnthropicProvider(client *http.Client) Provider { return &anthropicProvider{client: client} }
func (p *anthropicProvider) Platform() string           { return "anthropic" }
func (p *anthropicProvider) Name() string               { return "Anthropic" }
func (p *anthropicProvider) BaseURL() string            { return anthropicBaseURL }
func (p *anthropicProvider) Keyless() bool              { return false }

func (p *anthropicProvider) ChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (*ChatResponse, error) {
	body, err := anthropicRequest(request, false)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, err := p.do(requestCtx, apiKey, raw, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponse+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maxProviderResponse {
		return nil, fmt.Errorf("Anthropic response exceeds %d bytes", maxProviderResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpErrorFrom(resp.StatusCode, resp.Header, responseBody)
	}
	return parseAnthropicResponse(responseBody, resp.Header)
}

func (p *anthropicProvider) StreamChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (ChatStream, error) {
	body, err := anthropicRequest(request, true)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(90*time.Second, cancel)
	resp, err := p.do(requestCtx, apiKey, raw, true)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponse))
		_ = resp.Body.Close()
		cancel()
		return nil, httpErrorFrom(resp.StatusCode, resp.Header, failure)
	}
	return newAnthropicStream(resp, cancel, request.Model), nil
}

func (p *anthropicProvider) ValidateKey(ctx context.Context, apiKey string) KeyValidationResult {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, anthropicBaseURL+"/models", nil)
	if err != nil {
		return Inconclusive(err.Error())
	}
	anthropicoauth.ApplyAuthHeaders(req.Header, apiKey)
	resp, err := anthropicoauth.OAuthClient(p.client, apiKey).Do(req)
	if err != nil {
		return Inconclusive(err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return classifyValidation(p.Name(), resp.StatusCode, body)
}

func (p *anthropicProvider) do(ctx context.Context, apiKey string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicBaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	anthropicoauth.ApplyAuthHeaders(req.Header, apiKey)
	return anthropicoauth.OAuthClient(p.client, apiKey).Do(req)
}

func anthropicRequest(request *ChatRequest, stream bool) (map[string]any, error) {
	if request == nil {
		return nil, fmt.Errorf("Anthropic request is nil")
	}
	body := map[string]any{
		"model":      strings.TrimPrefix(request.Model, "anthropic/"),
		"max_tokens": 8192,
		"stream":     stream,
	}
	var system []any
	messages := make([]map[string]any, 0, len(request.Messages))
	appendMessage := func(role string, content []any) {
		if len(content) == 0 {
			return
		}
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			messages[n-1]["content"] = append(messages[n-1]["content"].([]any), content...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			system = append(system, anthropicTextContent(message["content"])...)
		case "tool":
			id, _ := message["tool_call_id"].(string)
			appendMessage("user", []any{map[string]any{
				"type": "tool_result", "tool_use_id": id,
				"content": anthropicToolResultContent(message["content"]),
			}})
		case "assistant":
			content, err := anthropicMessageContent(message["content"])
			if err != nil {
				return nil, err
			}
			calls, err := anthropicToolUseContent(message["tool_calls"])
			if err != nil {
				return nil, err
			}
			appendMessage("assistant", append(content, calls...))
		default:
			content, err := anthropicMessageContent(message["content"])
			if err != nil {
				return nil, err
			}
			appendMessage("user", content)
		}
	}
	if len(system) > 0 {
		body["system"] = system
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("Anthropic request has no user or assistant messages")
	}
	body["messages"] = messages

	// Sampling params (temperature/top_p/top_k) are deliberately NOT forwarded:
	// newer Claude models (opus-4.7+/sonnet-5/5.x) deprecate them and answer a
	// request that carries one with a 400 ("temperature is deprecated for this
	// model"), and extended thinking rejects a non-default temperature outright.
	// Claude routes on its own defaults, so forwarding a harness's sampling knob
	// only breaks the call - dropping it keeps the request working, which is what
	// the harness actually needs. metadata/thinking/service_tier still pass.
	for _, key := range []string{"metadata", "thinking", "service_tier"} {
		if value, ok := request.Params[key]; ok {
			body[key] = value
		}
	}
	if value, ok := request.Params["max_tokens"]; ok {
		body["max_tokens"] = value
	} else if value, ok := request.Params["max_completion_tokens"]; ok {
		body["max_tokens"] = value
	}
	if stop, ok := request.Params["stop"]; ok {
		switch value := stop.(type) {
		case string:
			body["stop_sequences"] = []string{value}
		default:
			body["stop_sequences"] = value
		}
	}
	// Reasoning: a client's OpenAI-style reasoning_effort becomes Claude
	// extended thinking. Thinking is what actually raises answer quality and,
	// unlike output_config.effort (which Haiku rejects with a 400), every modern
	// Claude model accepts it - so routing through the gateway is as capable as
	// calling Claude directly instead of collapsing to the non-thinking model.
	// The budget is derived from the effort and capped below max_tokens so the
	// answer keeps room. A client that sent a native thinking block wins.
	if _, native := body["thinking"]; !native {
		if effort, ok := request.Params["reasoning_effort"].(string); ok {
			if budget, on := anthropicThinkingBudget(effort, anthropicMaxTokens(body)); on {
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			}
		}
	}
	if tools, ok := request.Params["tools"]; ok {
		converted, err := anthropicTools(toAnySlice(tools))
		if err != nil {
			return nil, err
		}
		if len(converted) > 0 {
			body["tools"] = converted
		}
	}
	if choice, ok := request.Params["tool_choice"]; ok {
		converted, keepTools := anthropicToolChoice(choice)
		if !keepTools {
			delete(body, "tools")
		} else if converted != nil {
			body["tool_choice"] = converted
			// Anthropic rejects extended thinking when tool_choice forces tool
			// use ("Thinking may not be enabled when tool_choice forces tool
			// use."): a forced-tool turn with reasoning_effort would 400 the
			// whole request. "auto" is fine with thinking; only "any" (from
			// "required") and a named tool force it, so drop thinking there and
			// let the tool call proceed rather than failing the turn.
			if m, isMap := converted.(map[string]any); isMap {
				if t, _ := m["type"].(string); t == "any" || t == "tool" {
					delete(body, "thinking")
				}
			}
		}
	}
	return body, nil
}

func anthropicTextContent(value any) []any {
	text := contentText(value)
	if text == "" {
		return nil
	}
	return []any{map[string]any{"type": "text", "text": text}}
}

func anthropicMessageContent(value any) ([]any, error) {
	switch content := value.(type) {
	case nil:
		return nil, nil
	case string:
		if content == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": content}}, nil
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(content, &decoded); err != nil {
			return nil, err
		}
		return anthropicMessageContent(decoded)
	case []any:
		out := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typeName, _ := part["type"].(string)
			switch typeName {
			case "text", "input_text", "output_text":
				if text, _ := part["text"].(string); text != "" {
					out = append(out, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				url := ""
				switch image := part["image_url"].(type) {
				case string:
					url = image
				case map[string]any:
					url, _ = image["url"].(string)
				}
				if image := anthropicImage(url); image != nil {
					out = append(out, image)
				}
			case "image":
				out = append(out, part)
			}
		}
		return out, nil
	default:
		text := contentText(value)
		if text == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
}

func anthropicImage(rawURL string) map[string]any {
	if rawURL == "" {
		return nil
	}
	if strings.HasPrefix(rawURL, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(rawURL, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": strings.TrimSuffix(header, ";base64"), "data": data,
		}}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": rawURL}}
}

func anthropicToolResultContent(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	parts, err := anthropicMessageContent(value)
	if err == nil && len(parts) > 0 {
		return parts
	}
	return contentText(value)
}

func anthropicToolUseContent(value any) ([]any, error) {
	calls := toAnySlice(value)
	out := make([]any, 0, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		input := any(map[string]any{})
		switch args := function["arguments"].(type) {
		case string:
			if strings.TrimSpace(args) != "" {
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					return nil, fmt.Errorf("Anthropic tool %s has invalid JSON arguments: %w", name, err)
				}
			}
		case map[string]any:
			input = args
		}
		out = append(out, map[string]any{
			"type": "tool_use", "id": call["id"], "name": name, "input": input,
		})
	}
	return out, nil
}

// anthropicThinkingBudget maps an OpenAI-style reasoning_effort to a Claude
// extended-thinking token budget, and reports whether thinking should be
// enabled at all. The budget is capped below max_tokens so the answer still has
// room to be written (Anthropic requires max_tokens > budget_tokens); when
// max_tokens is too small to leave a useful answer, thinking is skipped rather
// than starving the response. An unrecognised effort disables thinking.
func anthropicThinkingBudget(effort string, maxTokens int) (int, bool) {
	var want int
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		want = 1024
	case "medium":
		want = 2048
	case "high":
		want = 4096
	case "xhigh", "max":
		want = 8192
	default:
		return 0, false
	}
	// A coding harness sends a fixed effort on every call, so an oversized
	// budget would make even a trivial turn think for tens of seconds and burn
	// premium allowance. These budgets keep reasoning strong while holding
	// time-to-first-token and cost in check.
	// Reserve room for the answer: at least 4096 tokens, or half the window when
	// it is small. The floor of 1024 is Anthropic's minimum thinking budget.
	reserve := 4096
	if maxTokens < reserve*2 {
		reserve = maxTokens / 2
	}
	if ceiling := maxTokens - reserve; want > ceiling {
		want = ceiling
	}
	if want < 1024 {
		return 0, false
	}
	return want, true
}

// anthropicMaxTokens reads the max_tokens already resolved into the request
// body (the client's max_tokens/max_completion_tokens, or the 8192 default),
// tolerating the numeric types a JSON decode and the defaults produce.
func anthropicMaxTokens(body map[string]any) int {
	switch v := body["max_tokens"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 8192
	}
}

func anthropicTools(tools []any) ([]any, error) {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" {
			continue
		}
		entry := map[string]any{"name": name, "input_schema": function["parameters"]}
		if description, _ := function["description"].(string); description != "" {
			entry["description"] = description
		}
		if entry["input_schema"] == nil {
			entry["input_schema"] = map[string]any{"type": "object"}
		}
		out = append(out, entry)
	}
	return out, nil
}

func anthropicToolChoice(value any) (any, bool) {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "none":
			return nil, false
		case "required":
			return map[string]any{"type": "any"}, true
		case "auto":
			return map[string]any{"type": "auto"}, true
		}
	case map[string]any:
		if function, ok := choice["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}, true
			}
		}
	}
	return nil, true
}

func toAnySlice(value any) []any {
	switch list := value.(type) {
	case []any:
		return list
	case []map[string]any:
		out := make([]any, len(list))
		for i := range list {
			out[i] = list[i]
		}
		return out
	case json.RawMessage:
		var out []any
		_ = json.Unmarshal(list, &out)
		return out
	default:
		return nil
	}
}

func contentText(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(content, &decoded) == nil {
			return contentText(decoded)
		}
	case []any:
		var parts []string
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

type anthropicResponse struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

func parseAnthropicResponse(raw []byte, headers http.Header) (*ChatResponse, error) {
	var upstream anthropicResponse
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return nil, fmt.Errorf("Anthropic: parse response: %w", err)
	}
	var text, reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	for _, block := range upstream.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "tool_use":
			arguments := block.Input
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": block.ID, "type": "function",
				"function": map[string]any{"name": block.Name, "arguments": string(arguments)},
			})
		}
	}
	content, _ := json.Marshal(text.String())
	message := RespMessage{Role: "assistant", Content: content}
	if reasoning.Len() > 0 {
		message.Reasoning, _ = json.Marshal(reasoning.String())
	}
	if len(toolCalls) > 0 {
		message.ToolCalls, _ = json.Marshal(toolCalls)
	}
	promptTokens := upstream.Usage.InputTokens + upstream.Usage.CacheCreationInputTokens + upstream.Usage.CacheReadInputTokens
	return &ChatResponse{
		ID: upstream.ID, Object: "chat.completion", Model: upstream.Model,
		Choices:   []Choice{{Index: 0, Message: message, FinishReason: anthropicFinish(upstream.StopReason)}},
		Usage:     &Usage{PromptTokens: promptTokens, CompletionTokens: upstream.Usage.OutputTokens, TotalTokens: promptTokens + upstream.Usage.OutputTokens},
		RoutedVia: &RoutedVia{Platform: "anthropic", Model: upstream.Model}, Raw: raw, Headers: headers,
		// Raw is the native Anthropic message envelope, not an OpenAI
		// completion; the OpenAI surface must marshal the normalised struct.
		Native: true,
	}, nil
}

func anthropicFinish(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

type anthropicStream struct {
	response *http.Response
	cancel   context.CancelFunc
	scanner  *bufio.Scanner
	id       string
	model    string
	closed   bool
	roleSent bool
	// inputTokens is the prompt count Anthropic reports on message_start; the
	// output count arrives only on message_delta, so the two halves are joined
	// there. stopped records that the protocol terminal event (message_stop)
	// was seen, so a stream that ends without it is caught as a truncation.
	inputTokens int
	stopped     bool
}

func newAnthropicStream(response *http.Response, cancel context.CancelFunc, fallbackModel string) *anthropicStream {
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &anthropicStream{response: response, cancel: cancel, scanner: scanner, model: fallbackModel}
}

func (s *anthropicStream) ResponseHeaders() http.Header { return s.response.Header }

func (s *anthropicStream) Recv() (*ChatChunk, error) {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				ID       string `json:"id"`
				Name     string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			// message_delta carries the run's cumulative output count in a
			// top-level usage block; a pointer keeps a real zero distinct from
			// an absent block.
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, fmt.Errorf("Anthropic stream: %w", err)
		}
		switch event.Type {
		case "message_start":
			s.id, s.model = event.Message.ID, event.Message.Model
			if u := event.Message.Usage; u != nil {
				// Prompt tokens, including cache traffic, are reported once at
				// the top of the stream and never repeated.
				s.inputTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
			}
			continue
		case "content_block_start":
			switch event.ContentBlock.Type {
			case "text":
				if event.ContentBlock.Text == "" {
					continue
				}
				return s.chunk(map[string]any{"role": "assistant", "content": event.ContentBlock.Text}, nil), nil
			case "thinking":
				if event.ContentBlock.Thinking == "" {
					continue
				}
				return s.chunk(map[string]any{"role": "assistant", "reasoning_content": event.ContentBlock.Thinking}, nil), nil
			case "tool_use":
				return s.chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"index": event.Index, "id": event.ContentBlock.ID, "type": "function",
					"function": map[string]any{"name": event.ContentBlock.Name, "arguments": ""},
				}}}, nil), nil
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				return s.chunk(map[string]any{"content": event.Delta.Text}, nil), nil
			case "thinking_delta":
				return s.chunk(map[string]any{"reasoning_content": event.Delta.Thinking}, nil), nil
			case "input_json_delta":
				return s.chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": event.Index, "function": map[string]any{"arguments": event.Delta.PartialJSON},
				}}}, nil), nil
			}
		case "message_delta":
			finish := anthropicFinish(event.Delta.StopReason)
			done := s.chunk(map[string]any{}, &finish)
			// Join the prompt count seen at message_start with the output count
			// reported here, carrying the exact usage in Raw so the gateway bills
			// reported tokens (a real zero included) rather than an estimate.
			if event.Usage != nil {
				done.Raw = streamUsageFrame(s.inputTokens, event.Usage.OutputTokens)
			}
			return done, nil
		case "error":
			if event.Error.Message == "" {
				event.Error.Message = "Anthropic stream failed"
			}
			return nil, fmt.Errorf("%s", event.Error.Message)
		case "message_stop":
			s.stopped = true
			return nil, io.EOF
		case "ping":
			// Anthropic sends periodic `ping` keepalives during a long thinking
			// or tool-argument phase, when no content frame is on the wire for
			// tens of seconds. Surfacing it as a liveness frame lets the relay's
			// post-commit idle guard see the upstream is alive and working, so a
			// healthy stream is not killed as stalled. It carries no content, so
			// the relay never commits or writes on it.
			return &ChatChunk{Keepalive: true}, nil
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	// The stream ended without the message_stop terminal event, so the
	// connection was cut mid-message. Wrapping io.ErrUnexpectedEOF classifies
	// it as a transport cut for the failover loop instead of passing a
	// truncated answer off as a clean end.
	if !s.stopped {
		return nil, fmt.Errorf("Anthropic stream ended before message_stop: %w", io.ErrUnexpectedEOF)
	}
	return nil, io.EOF
}

func (s *anthropicStream) chunk(delta map[string]any, finish *string) *ChatChunk {
	if !s.roleSent {
		if _, present := delta["role"]; !present {
			delta["role"] = "assistant"
		}
		s.roleSent = true
	}
	raw, _ := json.Marshal(delta)
	// The chunk is re-normalised from Anthropic's native SSE, so its Delta -
	// not Raw - is the OpenAI-shaped payload; mark it native so the OpenAI
	// surface marshals the normalised chunk instead of dropping the empty Raw.
	return &ChatChunk{ID: s.id, Object: "chat.completion.chunk", Model: s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: raw, FinishReason: finish}}, Native: true}
}

func (s *anthropicStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	return s.response.Body.Close()
}
