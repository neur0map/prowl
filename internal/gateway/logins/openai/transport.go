package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

// Transport adapts Fantasy Responses requests to the subscription backend.
type Transport struct {
	Base       http.RoundTripper
	Token      *oauth.Token
	Originator string
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.Token == nil {
		return base.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del("chatgpt-account-id")
	host := strings.ToLower(r.URL.Hostname())
	if r.URL.Scheme != "https" || (host != "chatgpt.com" && host != "api.openai.com" && host != "auth.openai.com") {
		return base.RoundTrip(r)
	}
	r.Header.Set("Authorization", "Bearer "+t.Token.AccessToken)
	if host == "chatgpt.com" {
		originator := t.Originator
		if originator == "" {
			originator = "prowl"
		}
		r.Header.Set("version", clientVersion)
		if sessionID := r.Header.Get("x-session-id"); sessionID != "" {
			r.Header.Set("session_id", sessionID)
			r.Header.Set("conversation_id", sessionID)
		}
		r.Header.Set("originator", originator)
		r.Header.Set("OpenAI-Beta", "responses=experimental")
		if t.Token.AccountID != "" {
			r.Header.Set("chatgpt-account-id", t.Token.AccountID)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") {
			if r.Body == nil {
				return nil, fmt.Errorf("codex request has no body")
			}
			data, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read Codex request: %w", err)
			}
			data, err = codexRequestBody(data)
			if err != nil {
				return nil, err
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			r.ContentLength = int64(len(data))
			r.Header.Del("Content-Length")
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		}
	}
	return base.RoundTrip(r)
}

func codexRequestBody(data []byte) ([]byte, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil || body == nil {
		return nil, fmt.Errorf("codex request must be a JSON object")
	}
	var streaming bool
	_ = json.Unmarshal(body["stream"], &streaming)
	if !streaming {
		return nil, fmt.Errorf("ChatGPT subscription requests require streaming")
	}
	body["store"] = json.RawMessage("false")
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "top_k", "min_p", "presence_penalty", "frequency_penalty", "repetition_penalty", "stop"} {
		delete(body, key)
	}
	var instructions string
	if value, exists := body["instructions"]; exists && string(value) != "null" {
		if json.Unmarshal(value, &instructions) != nil {
			return nil, fmt.Errorf("codex instructions must be text")
		}
	}
	var input []json.RawMessage
	if json.Unmarshal(body["input"], &input) == nil {
		leading := 0
		for _, raw := range input {
			var message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw, &message) != nil || (message.Role != "system" && message.Role != "developer") {
				break
			}
			var text string
			if json.Unmarshal(message.Content, &text) != nil {
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(message.Content, &parts) != nil {
					return nil, fmt.Errorf("invalid Codex system message")
				}
				for _, part := range parts {
					if part.Type != "input_text" && part.Type != "text" {
						return nil, fmt.Errorf("unsupported Codex system content")
					}
					if text != "" {
						text += "\n\n"
					}
					text += part.Text
				}
			}
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += text
			leading++
		}
		if leading > 0 {
			body["input"], _ = json.Marshal(input[leading:])
		}
	}
	body["instructions"], _ = json.Marshal(instructions)
	return json.Marshal(body)
}
