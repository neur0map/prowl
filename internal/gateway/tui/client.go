// Package tui is Prowl's unified terminal console: project indexes, providers,
// credentials, subscription accounts, models, routing, activity, toolkit and
// harness setup. There is no web UI to migrate to.
//
// The app is a client of the gateway's own HTTP API over loopback. That is a
// deliberate single code path: whether the process owns the engine (started
// here, ephemeral) or attaches to a running daemon, the screens issue the same
// bearer-authenticated requests. A TUI that talked to the engine directly in
// one mode and over HTTP in the other would drift, and the drift would only
// show up when someone attached to a daemon.
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client speaks to a gateway surface at BaseURL with one bearer credential
// (the machine-local token or the unified key; both authenticate everything).
type Client struct {
	BaseURL string
	// Home is the user's home directory, so the Setup tab can read the
	// injection ledger (a file, not an API route).
	Home  string
	Token string
	HTTP  *http.Client
}

// NewClient builds a client for a listening gateway.
func NewClient(port int, token string) *Client {
	home, _ := os.UserHomeDir()
	return &Client{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Home:    home,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// unifiedKey reveals the gateway's machine credential (Setup injects it into
// harnesses; the Overview tab masks it).
func (c *Client) unifiedKey(ctx context.Context) (string, error) {
	var out APIKeyResponse
	if err := c.Get(ctx, "/api/settings/api-key", &out); err != nil {
		return "", err
	}
	return out.APIKey, nil
}

// Get performs a GET and decodes JSON into out (out may be nil).
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.call(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST with a JSON body (raw pass-through when body is a
// string of already-encoded JSON).
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.call(ctx, http.MethodPost, path, body, out)
}

// Put performs a PUT.
func (c *Client) Put(ctx context.Context, path string, body any, out any) error {
	return c.call(ctx, http.MethodPut, path, body, out)
}

// Patch performs a PATCH.
func (c *Client) Patch(ctx context.Context, path string, body any, out any) error {
	return c.call(ctx, http.MethodPatch, path, body, out)
}

// Delete performs a DELETE.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.call(ctx, http.MethodDelete, path, nil, out)
}

func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(blob)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	blob, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &APIError{Status: resp.StatusCode, Body: decodeErrorMessage(blob)}
	}
	if out != nil && len(bytes.TrimSpace(blob)) > 0 {
		if err := json.Unmarshal(blob, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// APIError carries a server-side refusal in the shape the TUI shows: the
// gateway's own sentence, not a status line.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("gateway returned HTTP %d", e.Status)
	}
	return e.Body
}

func decodeErrorMessage(blob []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(blob, &parsed) == nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return parsed.Error.Message
	}
	msg := strings.TrimSpace(string(blob))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// ── wire types (only the fields the screens read) ───────────────────────────

type DirectoryCounts struct {
	Total      int `json:"total"`
	Configured int `json:"configured"`
	Routable   int `json:"routable"`
	Ready      int `json:"ready"`
	Free       int `json:"free"`
	Credits    int `json:"credits"`
	Paid       int `json:"paid"`
	OAuth      int `json:"oauth"`
	Local      int `json:"local"`
	FreeModels int `json:"freeModels"`
}

type DirectoryProvider struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Class      string   `json:"class"`
	Friction   string   `json:"friction"`
	APIKeyURL  string   `json:"apiKeyUrl"`
	DocsURL    string   `json:"docsUrl"`
	FreeModels int      `json:"freeModels"`
	MaxContext int      `json:"maxContext"`
	Modalities []string `json:"modalities"`
	Routable   bool     `json:"routable"`
	Probe      string   `json:"probe"`
	Note       string   `json:"note"`
	// Enrichment the directory handler computes per row.
	KeyCount        int    `json:"keyCount"`
	EnabledKeyCount int    `json:"enabledKeyCount"`
	HealthyKeyCount int    `json:"healthyKeyCount"`
	ErrorKeyCount   int    `json:"errorKeyCount"`
	Ready           bool   `json:"ready"`
	ModelCount      int    `json:"modelCount"`
	Adapter         bool   `json:"adapter"`
	Platform        string `json:"platform"`
	Keyless         bool   `json:"keyless"`
	Configured      bool   `json:"configured"`
}

type DirectoryResponse struct {
	Providers []DirectoryProvider `json:"providers"`
	Counts    DirectoryCounts     `json:"counts"`
	Sources   map[string]string   `json:"sources"`
}

type KeyView struct {
	ID              int64    `json:"id"`
	Platform        string   `json:"platform"`
	Label           string   `json:"label"`
	MaskedKey       string   `json:"maskedKey"`
	BaseURL         *string  `json:"baseUrl"`
	Status          string   `json:"status"`
	Enabled         bool     `json:"enabled"`
	Keyless         bool     `json:"keyless"`
	CreatedAt       string   `json:"createdAt"`
	LastCheckedAt   *string  `json:"lastCheckedAt"`
	LastHealthError *string  `json:"lastHealthError"`
	ModelScope      []string `json:"modelScope"`
}

// QuotaSignal is the provider's own account of a window it reported.
type QuotaSignal struct {
	Platform   string  `json:"platform"`
	KeyID      int64   `json:"keyId"`
	Metric     string  `json:"metric"`
	Limit      *int64  `json:"limit"`
	Remaining  *int64  `json:"remaining"`
	ResetAt    *string `json:"resetAt"`
	Source     string  `json:"source"`
	ObservedAt string  `json:"observedAt"`
}

// HealthResponse is /api/health: per-platform key tallies plus the quota
// signals the providers themselves reported.
type HealthResponse struct {
	Platforms   []HealthPlatform `json:"platforms"`
	QuotaStates []QuotaSignal    `json:"quotaStates"`
	Degradation any              `json:"degradation"`
}

type HealthPlatform struct {
	Platform    string `json:"platform"`
	HasProvider bool   `json:"hasProvider"`
	TotalKeys   int    `json:"totalKeys"`
	HealthyKeys int    `json:"healthyKeys"`
	ErrorKeys   int    `json:"errorKeys"`
	EnabledKeys int    `json:"enabledKeys"`
}

// ProvidersEnvelope is /api/keys/providers.
type ProvidersEnvelope struct {
	Providers []KeyProvider  `json:"providers"`
	Summary   map[string]int `json:"summary"`
}

// APIKeyResponse is /api/settings/api-key (the explicit reveal).
type APIKeyResponse struct {
	APIKey string `json:"apiKey"`
}

type RoutingState struct {
	Strategy             string          `json:"strategy"`
	ExploreEnabled       bool            `json:"exploreEnabled"`
	KeySelectionStrategy string          `json:"keySelectionStrategy"`
	Presets              []any           `json:"presets"`
	CooldownCeilingMs    *int64          `json:"cooldownCeilingMs"`
	Benchmark            BenchmarkStatus `json:"benchmark"`
}

type BenchmarkStatus struct {
	Source       string     `json:"source"`
	Configured   bool       `json:"configured"`
	Status       string     `json:"status"`
	LastSuccess  *time.Time `json:"lastSuccess"`
	Matched      int        `json:"matched"`
	Available    int        `json:"available"`
	Error        string     `json:"error"`
	Attribution  string     `json:"attribution"`
	IndexVersion float64    `json:"indexVersion"`
}

type Profile struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ModelCount int    `json:"modelCount"`
	CreatedAt  string `json:"created_at"`
}

type ChainPreset struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Group        string   `json:"group"`
	Requirements []string `json:"requirements"`
	Models       int      `json:"models"`
}

type ModelRow struct {
	ID               int64  `json:"id"`
	Platform         string `json:"platform"`
	ModelID          string `json:"modelId"`
	DisplayName      string `json:"displayName"`
	IntelligenceRank int    `json:"intelligenceRank"`
	SpeedRank        int    `json:"speedRank"`
	SizeLabel        string `json:"sizeLabel"`
	ContextWindow    *int64 `json:"contextWindow"`
	Enabled          bool   `json:"enabled"`
	FallbackEnabled  bool   `json:"fallbackEnabled"`
	Source           string `json:"source"`
	MonthlyBudget    string `json:"monthlyTokenBudget"`
	KeyCount         int    `json:"keyCount"`
	Access           string `json:"access"`
	Keyless          bool   `json:"keyless"`
	Available        bool   `json:"available"`
}

type UsageSummary struct {
	Window              string  `json:"window"`
	Requests            int64   `json:"requests"`
	Successes           int64   `json:"successes"`
	Failures            int64   `json:"failures"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	ExactRequests       int64   `json:"exactRequests"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
	UnknownCostRequests int64   `json:"unknownCostRequests"`
	AvgLatencyMs        int64   `json:"avgLatencyMs"`
	FailoverRate        float64 `json:"failoverRate"`
}

type UsagePlatform struct {
	Platform string `json:"platform"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
	Tokens   int64  `json:"tokens"`
	AvgMs    int64  `json:"avgMs"`
}

type UsageModel struct {
	Platform            string  `json:"platform"`
	Model               string  `json:"model"`
	Requests            int64   `json:"requests"`
	Errors              int64   `json:"errors"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
	AvgMs               int64   `json:"avgMs"`
}

type UsageResponse struct {
	Summary   UsageSummary       `json:"summary"`
	Platforms []UsagePlatform    `json:"platforms"`
	Models    []UsageModel       `json:"models"`
	Series    []UsageSeriesPoint `json:"series"`
}

// UsageSeriesPoint is one time bucket of the summary chart (hourly for 24h,
// daily for 7d/30d), in chronological order. CostUSD carries only known cost.
type UsageSeriesPoint struct {
	Start               string  `json:"start"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
}

type UsageRequestRow struct {
	ID           int64   `json:"id"`
	CreatedAt    string  `json:"createdAt"`
	Platform     string  `json:"platform"`
	Model        string  `json:"model"`
	Outcome      string  `json:"outcome"`
	Status       int     `json:"status"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	Estimated    bool    `json:"estimated"`
	UsageQuality string  `json:"usageQuality"`
	CostUSD      float64 `json:"costUsd"`
	CostKnown    bool    `json:"costKnown"`
	LatencyMs    int64   `json:"latencyMs"`
	Attempts     int     `json:"attempts"`
	RoutedFrom   string  `json:"routedFrom"`
	Class        string  `json:"class"`
	Effort       string  `json:"effort"`
	Error        string  `json:"error"`
	ErrorKind    string  `json:"errorKind"`
}

type LogsResponse struct {
	Logs  []ServerLog `json:"logs"`
	MaxID int64       `json:"maxId"`
}

type ServerLog struct {
	ID       int64  `json:"id"`
	Level    string `json:"level"`
	TS       int64  `json:"ts"`
	Source   string `json:"source"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Event    string `json:"event"`
	Message  string `json:"message"`
}

type LoginRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Detail   string `json:"detail"`
	Enrolled bool   `json:"enrolled"`
	Offered  int    `json:"offered"`
	Models   int    `json:"models"`
	KeyID    int64  `json:"keyId"`
}

// KeyProvider is one adapter row from /api/keys/providers.
type KeyProvider struct {
	Platform        string `json:"platform"`
	Name            string `json:"name"`
	Keyless         bool   `json:"keyless"`
	Configured      bool   `json:"configured"`
	KeyCount        int    `json:"keyCount"`
	EnabledKeyCount int    `json:"enabledKeyCount"`
	ModelCount      int    `json:"modelCount"`
}

type SignInPlatform struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	RoutesTo    string `json:"routes_to"`
	AccountHint string `json:"account_hint"`
	SignedIn    bool   `json:"signed_in"`
	Broken      bool   `json:"broken"`
	Account     string `json:"account"`
}

type SignInSession struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	URL       string `json:"url"`
	UserCode  string `json:"user_code"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Error     string `json:"error"`
	Account   string `json:"account"`
	ExpiresAt string `json:"expires_at"`
}

type StatusResponse struct {
	Version       string `json:"version"`
	Port          int    `json:"port"`
	KeyCount      int    `json:"keys"`
	EnabledKeys   int    `json:"enabledKeys"`
	ModelCount    int    `json:"models"`
	ProviderCount int    `json:"providers"`
	Strategy      string `json:"strategy"`
	LatestRequest string `json:"latestRequest"`
	RequestsToday int64  `json:"requestsToday"`
	TokensToday   int64  `json:"tokensToday"`
	UnifiedKey    string `json:"unifiedKey"`
	LocalToken    string `json:"localToken"`
	BaseURL       string `json:"baseUrl"`
}
