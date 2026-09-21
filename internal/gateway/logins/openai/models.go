package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

// Model is one ChatGPT-catalogue entry, shaped to feed the gateway's
// LinkedModel seeding rather than the harness's picker.
type Model struct {
	ID                     string
	Name                   string
	ContextWindow          int64
	DefaultMaxTokens       int64
	CanReason              bool
	ReasoningLevels        []string
	DefaultReasoningEffort string
	SupportsImages         bool
}

// This pin matches OMP's Codex feature-version negotiation, not Prowl's version.
const clientVersion = "0.153.0"

var modelsEndpoint = CodexBaseURL + "/models"

// FetchModels returns only the models offered to the signed-in subscription. A
// valid catalog whose visible set filters to zero returns an empty slice and no
// error, so authoritative reconciliation can retire a subscription that stopped
// offering models; only transport, status, and malformed-response failures are
// errors.
func FetchModels(ctx context.Context, token *oauth.Token) ([]Model, error) {
	if token == nil || token.AccessToken == "" {
		return nil, fmt.Errorf("ChatGPT model discovery requires OAuth")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsEndpoint+"?client_version="+clientVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "prowl")
	req.Header.Set("version", clientVersion)
	if token.AccountID != "" {
		req.Header.Set("chatgpt-account-id", token.AccountID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch ChatGPT catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ChatGPT model catalog returned HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Models json.RawMessage `json:"models"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 4<<20 || json.Unmarshal(body, &envelope) != nil {
		return nil, fmt.Errorf("invalid ChatGPT model catalog")
	}
	// The `models` array is the REQUIRED catalogue. An absent or wrong-typed
	// field is an unexpected shape (an error payload, a schema change), never an
	// authoritative empty catalogue, so it stays an error rather than retiring
	// the subscription's rows. A present, valid array - even `[]` - is
	// authoritative and reconciliation may retire against it.
	if !isJSONArray(envelope.Models) {
		return nil, fmt.Errorf("invalid ChatGPT model catalog")
	}
	var entries []struct {
		Slug             string `json:"slug"`
		Name             string `json:"display_name"`
		Visibility       string `json:"visibility"`
		ContextWindow    int64  `json:"context_window"`
		MaxOutputTokens  int64  `json:"max_output_tokens"`
		DefaultReasoning string `json:"default_reasoning_level"`
		Levels           []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	}
	if json.Unmarshal(envelope.Models, &entries) != nil {
		return nil, fmt.Errorf("invalid ChatGPT model catalog")
	}
	models := make([]Model, 0, len(entries))
	for _, model := range entries {
		if model.Slug == "" || model.Visibility != "list" {
			continue
		}
		levels := make([]string, 0, len(model.Levels))
		for _, level := range model.Levels {
			if level.Effort != "" {
				levels = append(levels, level.Effort)
			}
		}
		name := model.Name
		if name == "" {
			name = model.Slug
		}
		models = append(models, Model{ID: model.Slug, Name: name, ContextWindow: model.ContextWindow, DefaultMaxTokens: model.MaxOutputTokens, CanReason: len(levels) > 0, ReasoningLevels: levels, DefaultReasoningEffort: model.DefaultReasoning, SupportsImages: true})
	}
	return models, nil
}

// isJSONArray reports whether raw is a present JSON array. An absent field
// (nil), JSON null, or any non-array value all fail, so a required catalogue
// array the response omits or mistypes is never read as an authoritative empty
// set that would retire the subscription's models.
func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}
