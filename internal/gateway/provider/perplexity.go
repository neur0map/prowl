package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// perplexityProvider adapts the gateway's normalised Chat Completions request
// to Perplexity's Agent API - the multi-provider, web-grounded Responses-style
// endpoint at POST /v1/agent (docs.perplexity.ai/docs/agent-api/quickstart).
// This is the current API: it supersedes the deprecated Sonar-only
// /chat/completions surface, returns per-request token usage AND an exact
// monetary cost in usage.cost.total_cost, and interleaves search_results /
// citations into the output array. The whole upstream body is retained in
// ChatResponse.Raw so nothing this struct does not model - cost, citations,
// tool budgets - is lost.
type perplexityProvider struct {
	client  *http.Client
	baseURL string
}

// perplexityDefaultBaseURL is the official Agent API host root. Endpoints hang
// off /v1 (/v1/agent for completions, /v1/models for the credential probe).
const perplexityDefaultBaseURL = "https://api.perplexity.ai"

// perplexityChatTimeout is generous: a deep-research preset (medium/high/xhigh)
// runs many rounds of search and reasoning before it answers, so a chat-length
// bound would kill a healthy run. Very long jobs are what the API's background
// mode is for; this adapter is synchronous and buffered.
const perplexityChatTimeout = 300 * time.Second

// perplexityValidateTimeout bounds the GET /v1/models credential probe.
const perplexityValidateTimeout = 30 * time.Second

// perplexityResearchPresets is the small, explicit set of Agent API presets the
// adapter recognises. A request whose Model (or Params["preset"]) is one of
// these is sent in the `preset` slot rather than the `model` slot, so a caller
// can route "give me a deep-research run" by name and let Perplexity manage the
// underlying model, search config and tool budget. The current tier names and
// their pre-rename aliases are both accepted (presets doc: fast-search→fast,
// pro-search→low, deep-research→medium, advanced-deep-research→high,
// ultra→xhigh). Anything not listed is treated as a concrete model id.
var perplexityResearchPresets = map[string]bool{
	"fast":          true,
	"low":           true,
	"medium":        true,
	"high":          true,
	"xhigh":         true,
	"wide-research": true,
	// legacy names the API still resolves.
	"fast-search":            true,
	"pro-search":             true,
	"deep-research":          true,
	"advanced-deep-research": true,
	"ultra":                  true,
}

// perplexityForwardParams is the allowlist of request fields that are valid on
// the Agent API's Responses-style body and safe for a synchronous buffered
// call. Forwarding is curated rather than blanket because the Agent API rejects
// Chat-Completions-only fields (n, stop, logit_bias, response_format) with a
// 400: a research request must not be sabotaged by a stray param the client
// sent for a different provider. Chat→Responses shape differences - the
// max_tokens and reasoning_effort renames, and the custom-tool / tool_choice
// nesting - are translated in perplexityRequest instead of dropped or forwarded.
var perplexityForwardParams = map[string]bool{
	"temperature":          true,
	"top_p":                true,
	"top_logprobs":         true,
	"frequency_penalty":    true,
	"presence_penalty":     true,
	"parallel_tool_calls":  true,
	"metadata":             true,
	"max_steps":            true,
	"max_tool_calls":       true,
	"max_output_tokens":    true,
	"reasoning":            true,
	"text":                 true,
	"service_tier":         true,
	"truncation":           true,
	"previous_response_id": true,
	"prompt_cache_key":     true,
	"user":                 true,
}

// normalizePerplexityBaseURL keeps a custom relay base safe: a blank value can
// never yield a broken URL - it falls back to the official host - and a trailing
// slash is trimmed so path joining stays well-formed. The base is the host root
// (no /v1 suffix); the adapter appends the versioned path itself.
func normalizePerplexityBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return perplexityDefaultBaseURL
	}
	return strings.TrimRight(base, "/")
}

func newPerplexityProvider(client *http.Client, baseURL string) Provider {
	return &perplexityProvider{client: client, baseURL: normalizePerplexityBaseURL(baseURL)}
}

func (p *perplexityProvider) Platform() string { return "perplexity" }
func (p *perplexityProvider) Name() string     { return "Perplexity" }
func (p *perplexityProvider) BaseURL() string  { return p.baseURL }
func (p *perplexityProvider) Keyless() bool    { return false }

// ChatCompletion issues one buffered POST /v1/agent and normalises the response.
func (p *perplexityProvider) ChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (*ChatResponse, error) {
	raw, err := json.Marshal(perplexityRequest(request, false))
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, perplexityChatTimeout)
	defer cancel()
	resp, err := p.do(requestCtx, apiKey, "/v1/agent", raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProviderResponse {
		return nil, fmt.Errorf("Perplexity response exceeds %d bytes", maxProviderResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpErrorFrom(resp.StatusCode, resp.Header, body)
	}
	return parsePerplexityResponse(body, resp.Header, request.Model)
}

// StreamChatCompletion honours the stream contract without pretending the
// buffered /v1/agent call is an incremental SSE stream: it runs the completion
// and replays it as a single terminal frame. Faking token-by-token streaming
// over a request that only answers once would misrepresent the upstream.
func (p *perplexityProvider) StreamChatCompletion(ctx context.Context, apiKey string, request *ChatRequest) (ChatStream, error) {
	resp, err := p.ChatCompletion(ctx, apiKey, request)
	if err != nil {
		return nil, err
	}
	return newBufferedStream(resp), nil
}

// ValidateKey probes GET /v1/models, which requires authentication: a 200
// confirms the key, only 401/403 confirm it is bad, and any transport failure
// is inconclusive so a network blip never disables a working credential.
func (p *perplexityProvider) ValidateKey(ctx context.Context, apiKey string) KeyValidationResult {
	requestCtx, cancel := context.WithTimeout(ctx, perplexityValidateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, p.baseURL+"/v1/models", nil)
	if err != nil {
		return Inconclusive(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return Inconclusive(err.Error())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return classifyValidation(p.Name(), resp.StatusCode, body)
}

func (p *perplexityProvider) do(ctx context.Context, apiKey, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return p.client.Do(req)
}

// perplexityRequest translates the normalised Chat request into the Agent API's
// Responses-style body. System/developer turns are hoisted into the top-level
// `instructions` field (where the Responses API carries the standing prompt);
// every other turn becomes an `input` item. A preset name lands in the `preset`
// slot and a concrete id in the `model` slot - both may be present, which the
// API reads as "run this preset but override its model" (presets doc).
func perplexityRequest(request *ChatRequest, stream bool) map[string]any {
	input := make([]any, 0, len(request.Messages))
	var instructions strings.Builder
	appendInstruction := func(text string) {
		if text == "" {
			return
		}
		if instructions.Len() > 0 {
			instructions.WriteString("\n\n")
		}
		instructions.WriteString(text)
	}
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			appendInstruction(textContent(message["content"]))
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

	body := map[string]any{"input": input}
	if model := strings.ToLower(strings.TrimSpace(request.Model)); model != "" {
		if perplexityResearchPresets[model] {
			body["preset"] = model
		} else {
			body["model"] = strings.TrimSpace(request.Model)
		}
	}

	for key, value := range request.Params {
		switch key {
		case "max_tokens", "max_completion_tokens":
			if _, exists := body["max_output_tokens"]; !exists {
				body["max_output_tokens"] = value
			}
		case "reasoning_effort":
			if effort, ok := value.(string); ok && effort != "" {
				if _, exists := body["reasoning"]; !exists {
					body["reasoning"] = map[string]any{"effort": effort}
				}
			}
		case "preset":
			if preset, ok := value.(string); ok {
				preset = strings.ToLower(strings.TrimSpace(preset))
				if perplexityResearchPresets[preset] {
					body["preset"] = preset
				}
			}
		case "instructions":
			if text, ok := value.(string); ok {
				appendInstruction(text)
			}
		case "tools":
			body["tools"] = perplexityTools(value)
		case "tool_choice":
			body["tool_choice"] = perplexityToolChoice(value)
		default:
			if perplexityForwardParams[key] {
				body[key] = value
			}
		}
	}
	if instructions.Len() > 0 {
		body["instructions"] = instructions.String()
	}

	if stream {
		body["stream"] = true
	}
	return body
}

// perplexityTools maps the normalised Chat Completions `tools` array onto the
// Agent API's Responses tool shape. A Chat custom tool nests its identity under
// `function` ({type:"function", function:{name,description,parameters}}); the
// Responses shape hoists name/description/parameters to the tool object's top
// level ({type:"function", name, description, parameters}). A tool that carries
// no nested `function` object is already Responses-shaped or is a non-function
// built-in Agent tool (web_search and friends) and is forwarded untouched.
func perplexityTools(value any) any {
	tools, ok := value.([]any)
	if !ok {
		return value
	}
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
			if v, exists := function[key]; exists {
				flat[key] = v
			}
		}
		converted = append(converted, flat)
	}
	return converted
}

// perplexityToolChoice mirrors perplexityTools for a forced tool selection. The
// Chat shape names the function under `function` ({type:"function",
// function:{name}}); the Responses shape hoists the name to the top level
// ({type:"function", name}). String choices ("auto"/"none"/"required") and
// already-flat or built-in choices pass through unchanged.
func perplexityToolChoice(value any) any {
	choice, ok := value.(map[string]any)
	if !ok {
		return value
	}
	function, ok := choice["function"].(map[string]any)
	if !ok {
		return value
	}
	return map[string]any{"type": "function", "name": function["name"]}
}

type perplexityContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type perplexityOutputItem struct {
	Type      string                  `json:"type"`
	Role      string                  `json:"role"`
	Content   []perplexityContentPart `json:"content"`
	Queries   []string                `json:"queries"`
	Results   []json.RawMessage       `json:"results"`
	CallID    string                  `json:"call_id"`
	Name      string                  `json:"name"`
	Arguments string                  `json:"arguments"`
}

type perplexityUsageCost struct {
	Currency   string  `json:"currency"`
	InputCost  float64 `json:"input_cost"`
	OutputCost float64 `json:"output_cost"`
	TotalCost  float64 `json:"total_cost"`
}

type perplexityUsage struct {
	InputTokens  int                  `json:"input_tokens"`
	OutputTokens int                  `json:"output_tokens"`
	TotalTokens  int                  `json:"total_tokens"`
	Cost         *perplexityUsageCost `json:"cost"`
}

type perplexityResponse struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Object string `json:"object"`
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
	Output []perplexityOutputItem `json:"output"`
	Usage  *perplexityUsage       `json:"usage"`
}

// parsePerplexityResponse folds the Agent API's output array into one OpenAI
// completion. Assistant message text (all output_text parts, in order) becomes
// the choice content; any client-facing custom-function calls become tool_calls
// so the standard tool loop still works; the untouched body - search_results,
// citations, usage.cost - is kept in Raw for callers this struct does not model.
func parsePerplexityResponse(raw []byte, headers http.Header, fallbackModel string) (*ChatResponse, error) {
	var upstream perplexityResponse
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return nil, fmt.Errorf("Perplexity: parse response: %w", err)
	}
	if upstream.Error != nil && upstream.Error.Message != "" {
		return nil, fmt.Errorf("Perplexity: %s", upstream.Error.Message)
	}
	switch upstream.Status {
	case "failed", "cancelled", "canceled", "cancelling":
		return nil, fmt.Errorf("Perplexity: run %s", upstream.Status)
	case "queued", "in_progress":
		// A background run answers later via a poll this synchronous adapter
		// does not perform; surfacing it as text would be a lie.
		return nil, fmt.Errorf("Perplexity: run did not complete synchronously (status %q)", upstream.Status)
	}

	var text strings.Builder
	toolCalls := make([]map[string]any, 0)
	for _, item := range upstream.Output {
		switch item.Type {
		case "message":
			if item.Role != "" && item.Role != "assistant" {
				continue
			}
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
		case "function_call":
			arguments := item.Arguments
			if arguments == "" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": item.CallID, "type": "function",
				"function": map[string]any{"name": item.Name, "arguments": arguments},
			})
		}
	}

	model := upstream.Model
	if model == "" {
		model = fallbackModel
	}
	content, _ := json.Marshal(text.String())
	message := RespMessage{Role: "assistant", Content: content}
	finish := "stop"
	if upstream.Status == "incomplete" {
		finish = "length"
	}
	if len(toolCalls) > 0 {
		message.ToolCalls, _ = json.Marshal(toolCalls)
		if finish == "stop" {
			finish = "tool_calls"
		}
	}

	resp := &ChatResponse{
		ID: upstream.ID, Object: "chat.completion", Model: model,
		Choices:   []Choice{{Index: 0, Message: message, FinishReason: finish}},
		RoutedVia: &RoutedVia{Platform: "perplexity", Model: model},
		Raw:       raw, Headers: headers,
		// Raw is Perplexity's Agent API envelope (an output array, status and
		// usage.cost), not an OpenAI completion, so the OpenAI surface must
		// marshal this normalised struct rather than relay Raw - which an
		// OpenAI client could not parse. Raw is still retained for the router's
		// cost/citation extraction.
		Native: true,
	}
	if upstream.Usage != nil {
		total := upstream.Usage.TotalTokens
		if total == 0 {
			total = upstream.Usage.InputTokens + upstream.Usage.OutputTokens
		}
		resp.Usage = &Usage{
			PromptTokens:     upstream.Usage.InputTokens,
			CompletionTokens: upstream.Usage.OutputTokens,
			TotalTokens:      total,
		}
	}
	return resp, nil
}

var _ Provider = (*perplexityProvider)(nil)
