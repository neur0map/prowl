package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// usageBaseURL is the Codex subscription's rate-limit report. It is a var only
// so a test can point it at an httptest server; production always dials the
// ChatGPT backend the Codex CLI reads.
var usageBaseURL = "https://chatgpt.com/backend-api/wham/usage"

// usageHTTPClient reads the informational usage report as a plain GET. The
// bearer must never chase a redirect onto another host, and the 20s ceiling
// keeps the Accounts screen inside the TUI's fetch budget.
var usageHTTPClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Window is one Codex rate-limit bucket as the subscription backend reports it:
// UsedPercent is 0-100, LimitWindowSeconds sizes the window, ResetAfterSeconds
// is the countdown, and ResetAt the wall clock it refills (zero when omitted).
type Window struct {
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
	ResetAt            time.Time
}

// Usage is a signed-in ChatGPT subscription's allowance: a primary window every
// plan reports and an optional secondary the backend may leave null.
type Usage struct {
	PlanType  string
	Primary   *Window
	Secondary *Window
}

// FetchUsage reads the signed-in ChatGPT subscription's rate-limit report from
// the Codex backend. It mirrors the Codex CLI's request identity (originator,
// User-Agent, account id) so the backend answers, and returns a plain error -
// never the bearer. A 401/403 surfaces with its status code text so the caller
// can recognise an authentication failure and prompt a re-link.
func FetchUsage(ctx context.Context, token string) (Usage, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Usage{}, fmt.Errorf("ChatGPT usage lookup requires an OAuth access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageBaseURL, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "prowl")
	req.Header.Set("User-Agent", "codex_cli_rs/"+clientVersion)
	if id := AccountID(token); id != "" {
		req.Header.Set("ChatGPT-Account-Id", id)
	}
	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("ChatGPT usage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Usage{}, fmt.Errorf("read ChatGPT usage: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status code text lets the Accounts surface recognise a 401/403 as
		// an authentication failure and offer a re-link, not a raw error row.
		return Usage{}, fmt.Errorf("ChatGPT usage endpoint returned HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *usageWindowPayload `json:"primary_window"`
			Secondary *usageWindowPayload `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Usage{}, fmt.Errorf("parse ChatGPT usage: %w", err)
	}
	return Usage{
		PlanType:  envelope.PlanType,
		Primary:   envelope.RateLimit.Primary.window(),
		Secondary: envelope.RateLimit.Secondary.window(),
	}, nil
}

// usageWindowPayload is the wire shape of one rate-limit window.
type usageWindowPayload struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// window normalizes a wire window, folding a null payload to a nil Window and a
// zero reset_at to a zero time rather than the Unix epoch.
func (p *usageWindowPayload) window() *Window {
	if p == nil {
		return nil
	}
	w := &Window{
		UsedPercent:        p.UsedPercent,
		LimitWindowSeconds: p.LimitWindowSeconds,
		ResetAfterSeconds:  p.ResetAfterSeconds,
	}
	if p.ResetAt > 0 {
		w.ResetAt = time.Unix(p.ResetAt, 0)
	}
	return w
}
