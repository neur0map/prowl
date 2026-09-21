package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
	openaioauth "github.com/neur0map/prowl/internal/gateway/logins/openai"
)

// codexProvider adapts the gateway's normalized Chat Completions request to the
// Responses stream spoken by a ChatGPT subscription. The subscription backend
// requires streaming even when the caller requested a buffered response, so the
// buffered verb consumes the same stream and assembles one ordinary completion.
type codexProvider struct {
	client *http.Client
}

func newCodexProvider(client *http.Client) Provider { return &codexProvider{client: client} }

func (p *codexProvider) Platform() string { return "openai" }
func (p *codexProvider) Name() string     { return "ChatGPT (Codex)" }
func (p *codexProvider) BaseURL() string  { return openaioauth.CodexBaseURL }
func (p *codexProvider) Keyless() bool    { return false }

func (p *codexProvider) oauthClient(apiKey string) *http.Client {
	base := p.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy := *p.client
	copy.Transport = &openaioauth.Transport{
		Base: base,
		Token: &oauth.Token{
			AccessToken: apiKey,
			AccountID:   openaioauth.AccountID(apiKey),
		},
		Originator: "prowl",
	}
	return &copy
}

func (p *codexProvider) ValidateKey(ctx context.Context, apiKey string) KeyValidationResult {
	models, err := openaioauth.FetchModels(ctx, &oauth.Token{
		AccessToken: apiKey,
		AccountID:   openaioauth.AccountID(apiKey),
	})
	if err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "http 401") || strings.Contains(message, "http 403") {
			return Invalid(err.Error())
		}
		return Inconclusive(err.Error())
	}
	if len(models) == 0 {
		return Inconclusive("ChatGPT returned an empty model catalogue")
	}
	return Valid()
}

func (p *codexProvider) ChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (*ChatResponse, error) {
	stream, err := p.StreamChatCompletion(ctx, apiKey, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var (
		id       string
		model    = request.Model
		text     strings.Builder
		finish   string
		toolByIx = map[int]codexToolCall{}
		usage    *Usage
	)
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		// The terminal chunk carries the run's exact usage in Raw; assembling a
		// buffered answer must not drop it or the request bills by estimate.
		if u := codexChunkUsage(chunk.Raw); u != nil {
			usage = u
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			var delta struct {
				Content   string          `json:"content"`
				ToolCalls []codexToolCall `json:"tool_calls"`
			}
			if json.Unmarshal(choice.Delta, &delta) != nil {
				continue
			}
			text.WriteString(delta.Content)
			for _, call := range delta.ToolCalls {
				current := toolByIx[call.Index]
				current.Index = call.Index
				if call.ID != "" {
					current.ID = call.ID
				}
				if call.Type != "" {
					current.Type = call.Type
				}
				if call.Function.Name != "" {
					current.Function.Name = call.Function.Name
				}
				current.Function.Arguments += call.Function.Arguments
				toolByIx[call.Index] = current
			}
		}
	}
	if id == "" {
		id = "chatcmpl-codex"
	}
	content, _ := json.Marshal(text.String())
	message := RespMessage{Role: "assistant", Content: content}
	if len(toolByIx) > 0 {
		calls := orderedCodexToolCalls(toolByIx)
		message.ToolCalls, _ = json.Marshal(calls)
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	return &ChatResponse{
		ID: id, Object: "chat.completion", Model: model,
		Choices:   []Choice{{Index: 0, Message: message, FinishReason: finish}},
		Usage:     usage,
		RoutedVia: &RoutedVia{Platform: p.Platform(), Model: model},
	}, nil
}

func orderedCodexToolCalls(byIndex map[int]codexToolCall) []codexToolCall {
	indices := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	calls := make([]codexToolCall, 0, len(indices))
	for _, index := range indices {
		calls = append(calls, byIndex[index])
	}
	return calls
}

// codexChunkUsage reads the exact token counts a terminal chunk carries in its
// Raw (streamUsageFrame's shape), so the buffered verb can bill reported usage.
// A present usage object is exact even when a count is legitimately zero.
func codexChunkUsage(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var frame struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Usage == nil {
		return nil
	}
	return &Usage{
		PromptTokens:     frame.Usage.InputTokens,
		CompletionTokens: frame.Usage.OutputTokens,
		TotalTokens:      frame.Usage.InputTokens + frame.Usage.OutputTokens,
	}
}

func (p *codexProvider) StreamChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (ChatStream, error) {
	body, err := codexRequest(request)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, openaioauth.CodexBaseURL+"/responses", bytes.NewReader(raw))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Only bound the wait for headers. A healthy answer may stream for minutes.
	timer := time.AfterFunc(90*time.Second, cancel)
	resp, err := p.oauthClient(apiKey).Do(req)
	timer.Stop()
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponse))
		_ = resp.Body.Close()
		cancel()
		return nil, httpErrorFrom(resp.StatusCode, resp.Header, data)
	}
	return newCodexStream(resp, cancel, request.Model), nil
}

func codexRequest(request *ChatRequest) (map[string]any, error) {
	input := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": callID,
				"output": textContent(message["content"]),
			})
		case "assistant":
			if content := textContent(message["content"]); content != "" {
				input = append(input, map[string]any{"role": role, "content": content})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					input = append(input, map[string]any{
						"type": "function_call", "call_id": call["id"],
						"name": function["name"], "arguments": function["arguments"],
					})
				}
			}
		default:
			input = append(input, map[string]any{"role": role, "content": responsesContent(role, message["content"])})
		}
	}
	body := map[string]any{"model": request.Model, "input": input, "stream": true}
	for _, key := range []string{"tool_choice", "parallel_tool_calls", "metadata"} {
		if value, ok := request.Params[key]; ok {
			body[key] = value
		}
	}
	if effort, ok := request.Params["reasoning_effort"].(string); ok && effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if tools, ok := request.Params["tools"].([]any); ok {
		converted := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			if function == nil {
				converted = append(converted, rawTool)
				continue
			}
			flat := map[string]any{"type": "function", "name": function["name"]}
			for _, key := range []string{"description", "parameters", "strict"} {
				if value, exists := function[key]; exists {
					flat[key] = value
				}
			}
			converted = append(converted, flat)
		}
		body["tools"] = converted
	}
	return body, nil
}

func textContent(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var text strings.Builder
		for _, rawPart := range content {
			part, _ := rawPart.(map[string]any)
			if value, _ := part["text"].(string); value != "" {
				text.WriteString(value)
			}
		}
		return text.String()
	default:
		return ""
	}
}

func responsesContent(role string, value any) any {
	parts, ok := value.([]any)
	if !ok {
		return value
	}
	converted := make([]any, 0, len(parts))
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		typ, _ := part["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			converted = append(converted, map[string]any{"type": partType, "text": part["text"]})
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			converted = append(converted, map[string]any{"type": "input_image", "image_url": image["url"]})
		default:
			converted = append(converted, rawPart)
		}
	}
	return converted
}

type codexToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type codexStream struct {
	response *http.Response
	cancel   context.CancelFunc
	scanner  *bufio.Scanner
	id       string
	model    string
	toolSeen bool
	finished bool
}

func newCodexStream(response *http.Response, cancel context.CancelFunc, model string) *codexStream {
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	return &codexStream{response: response, cancel: cancel, scanner: scanner, model: model}
}

func (s *codexStream) ResponseHeaders() http.Header { return s.response.Header }

func (s *codexStream) Recv() (*ChatChunk, error) {
	if s.finished {
		return nil, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type        string `json:"type"`
			Delta       string `json:"delta"`
			OutputIndex int    `json:"output_index"`
			Message     string `json:"message"`
			Item        struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"item"`
			Response struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				// response.completed carries the run's exact token accounting;
				// a pointer distinguishes "no usage reported" from a real zero.
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if event.Response.ID != "" {
			s.id = event.Response.ID
		}
		if event.Response.Model != "" {
			s.model = event.Response.Model
		}
		switch event.Type {
		case "response.output_text.delta":
			delta, _ := json.Marshal(map[string]any{"content": event.Delta})
			return s.chunk(delta, nil), nil
		case "response.output_item.added":
			if event.Item.Type != "function_call" {
				continue
			}
			s.toolSeen = true
			call := codexToolCall{Index: event.OutputIndex, ID: event.Item.CallID, Type: "function"}
			call.Function.Name = event.Item.Name
			delta, _ := json.Marshal(map[string]any{"tool_calls": []codexToolCall{call}})
			return s.chunk(delta, nil), nil
		case "response.function_call_arguments.delta":
			s.toolSeen = true
			call := codexToolCall{Index: event.OutputIndex, Type: "function"}
			call.Function.Arguments = event.Delta
			delta, _ := json.Marshal(map[string]any{"tool_calls": []codexToolCall{call}})
			return s.chunk(delta, nil), nil
		case "response.completed":
			s.finished = true
			reason := "stop"
			if s.toolSeen {
				reason = "tool_calls"
			}
			done := s.chunk(json.RawMessage(`{}`), &reason)
			// The subscription backend reports the run's exact token usage on
			// the terminal event; carry it in Raw so the gateway bills the real
			// counts (including a legitimate zero) instead of an estimate.
			if u := event.Response.Usage; u != nil {
				done.Raw = streamUsageFrame(u.InputTokens, u.OutputTokens)
			}
			return done, nil
		case "response.failed", "error":
			message := event.Message
			if event.Error != nil && event.Error.Message != "" {
				message = event.Error.Message
			}
			if event.Response.Error != nil && event.Response.Error.Message != "" {
				message = event.Response.Error.Message
			}
			if message == "" {
				message = "Codex response failed"
			}
			return nil, fmt.Errorf("%s", message)
		}
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	// The stream ran dry without a response.completed event: the connection
	// was cut mid-run. Surfacing io.EOF here would let the buffered verb return
	// a truncated answer as a success; wrapping io.ErrUnexpectedEOF classifies
	// it as a transport cut so the failover loop retries elsewhere.
	if !s.finished {
		return nil, fmt.Errorf("Codex stream ended before response.completed: %w", io.ErrUnexpectedEOF)
	}
	return nil, io.EOF
}

func (s *codexStream) chunk(delta json.RawMessage, finish *string) *ChatChunk {
	return &ChatChunk{
		ID: s.id, Object: "chat.completion.chunk", Model: s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		// The delta is re-normalised from the Responses stream into the OpenAI
		// chunk shape, so it - not Raw - is the payload the OpenAI surface must
		// render. Marking it Native keeps the shaper on the normalised delta so
		// the usage carried in Raw on the terminal chunk is read for billing
		// without being relayed to the client as a stray frame.
		Native: true,
	}
}

func (s *codexStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.response.Body.Close()
}

var _ Provider = (*codexProvider)(nil)
var _ HeaderCarrier = (*codexStream)(nil)
