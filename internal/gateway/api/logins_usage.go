package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/logins/anthropic"
	"github.com/neur0map/prowl/internal/gateway/logins/hyper"
	"github.com/neur0map/prowl/internal/gateway/logins/openai"
)

// The Accounts screen shows how much of each connected subscription is left.
// This surface reads the provider's own informational report - never a stored
// or invented number - and reports one entry per signed-in account so a single
// provider's outage degrades that row instead of failing the whole screen.

// usageWindow is one normalized rate-limit bucket: Utilization is a 0-100
// percentage and ResetsAt the wall clock the bucket refills, when the provider
// publishes one.
type usageWindow struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Utilization   float64 `json:"utilization"`
	ResetsAt      string  `json:"resetsAt,omitempty"`
	WindowSeconds int64   `json:"windowSeconds"`
	TokensUsed    int64   `json:"tokensUsed"`
}

// accountUsage is one connected account's allowance. A rate-windowed account
// (Claude) fills Windows; a running-credit account (Hyper) fills Balance/Unit
// and explains its mixed grant/purchased-credit semantics in Note. Error
// carries a per-account failure so the screen never fails whole; it never
// carries a secret.
type accountUsage struct {
	Provider    string        `json:"provider"`
	Name        string        `json:"name"`
	Windows     []usageWindow `json:"windows,omitempty"`
	Balance     *float64      `json:"balance,omitempty"`
	Unit        string        `json:"unit,omitempty"`
	Cadence     string        `json:"cadence,omitempty"`
	Note        string        `json:"note,omitempty"`
	NeedsSignIn bool          `json:"needsSignIn,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// usageProviders is the ordered set of accounts that publish an allowance the
// gateway can read. Other logins (ChatGPT, Copilot) expose no such surface, so
// they are simply absent rather than shown with a fabricated allowance.
var usageProviders = []string{"anthropic", "openai", "hyper"}

// hyperCredits is the parsed Hyper credit balance.
type hyperCredits struct {
	Balance float64
}

// Seams for deterministic tests. Production points them at the live providers;
// a handler test overrides them to exercise aggregation and degradation without
// a network.
var (
	fetchAnthropicUsage = anthropic.FetchUsage
	fetchOpenAIUsage    = openai.FetchUsage
	fetchHyperCredits   = fetchHyperCreditsLive
)

func (s *Server) handleLoginsUsage(w http.ResponseWriter, r *http.Request) {
	src := s.engine.CredentialSource()
	if src == nil {
		// A gateway running without the harness has no logins to report on.
		WriteJSON(w, http.StatusOK, map[string]any{"accounts": []any{}})
		return
	}
	ctx := r.Context()

	signedIn := map[string]gateway.LinkableProvider{}
	for _, p := range src.Linkable(ctx) {
		signedIn[strings.ToLower(strings.TrimSpace(p.ID))] = p
	}

	present := make([]string, 0, len(usageProviders))
	for _, id := range usageProviders {
		if _, ok := signedIn[id]; ok {
			present = append(present, id)
		}
	}
	// Fetch each account's allowance concurrently: a slow or timed-out provider
	// must not starve the other within the TUI's 30s budget (Anthropic alone can
	// take 20s). Each goroutine writes its own slot, so ordering is preserved
	// without a lock.
	accounts := make([]accountUsage, len(present))
	var wg sync.WaitGroup
	for i, id := range present {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			p := signedIn[id]
			switch id {
			case "anthropic":
				accounts[i] = anthropicUsage(ctx, src, p)
			case "openai":
				accounts[i] = openaiUsage(ctx, src, p)
			case "hyper":
				accounts[i] = hyperUsage(ctx, src, p)
			}
		}(i, id)
	}
	wg.Wait()
	fillWindowTokens(ctx, s.engine.DB(), accounts)
	WriteJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func anthropicUsage(ctx context.Context, src gateway.CredentialSource, p gateway.LinkableProvider) accountUsage {
	out := accountUsage{Provider: "anthropic", Name: providerDisplayName(p, "Claude Pro / Max")}
	token, _, err := src.Credential(ctx, "anthropic")
	if err != nil {
		out.NeedsSignIn = true
		out.Error = err.Error()
		return out
	}
	usage, err := fetchAnthropicUsage(ctx, token)
	if err != nil {
		out.NeedsSignIn = authenticationFailure(err)
		out.Error = redactSecret(err.Error(), token)
		return out
	}
	if w := usage.Session5h(); w != nil {
		out.Windows = append(out.Windows, usageWindow{
			Key: "five_hour", Label: "5h session", Utilization: w.Utilization, ResetsAt: w.ResetsAt, WindowSeconds: 18000,
		})
	}
	if w := usage.Weekly(); w != nil {
		out.Windows = append(out.Windows, usageWindow{
			Key: "seven_day", Label: "7d all models", Utilization: w.Utilization, ResetsAt: w.ResetsAt, WindowSeconds: 604800,
		})
	}
	for _, s := range usage.ScopedWeekly() {
		out.Windows = append(out.Windows, usageWindow{
			Key: "weekly_scoped", Label: "7d " + s.Label, Utilization: s.Utilization, ResetsAt: s.ResetsAt, WindowSeconds: 604800,
		})
	}
	return out
}

func openaiUsage(ctx context.Context, src gateway.CredentialSource, p gateway.LinkableProvider) accountUsage {
	out := accountUsage{Provider: "openai", Name: providerDisplayName(p, "ChatGPT (Codex)")}
	token, _, err := src.Credential(ctx, "openai")
	if err != nil {
		out.NeedsSignIn = true
		out.Error = err.Error()
		return out
	}
	usage, err := fetchOpenAIUsage(ctx, token)
	if err != nil {
		out.NeedsSignIn = authenticationFailure(err)
		out.Error = redactSecret(err.Error(), token)
		return out
	}
	out.Windows = codexWindows(usage)
	return out
}

// codexWindows maps the Codex rate-limit report onto the normalized windows the
// Accounts screen renders. Each window's Key follows Contract 1 (five_hour for a
// window <=6h, else seven_day). When the backend reports two windows that both
// resolve to the same Key it is publishing the 5h and weekly buckets, so the
// primary is pinned to the 5h window and the secondary to the weekly one rather
// than showing two identical keys.
func codexWindows(usage openai.Usage) []usageWindow {
	entries := make([]usageWindow, 0, 2)
	if usage.Primary != nil {
		entries = append(entries, codexWindow(usage.Primary))
	}
	if usage.Secondary != nil {
		entries = append(entries, codexWindow(usage.Secondary))
	}
	if len(entries) == 2 && entries[0].Key == entries[1].Key {
		entries[0].Key, entries[0].Label, entries[0].WindowSeconds = "five_hour", "5h session", 18000
		entries[1].Key, entries[1].Label, entries[1].WindowSeconds = "seven_day", "weekly", 604800
	}
	return entries
}

func codexWindow(w *openai.Window) usageWindow {
	key, label, windowSeconds := "seven_day", "7d all models", int64(604800)
	if w.LimitWindowSeconds <= 21600 {
		key, label, windowSeconds = "five_hour", "5h session", 18000
	}
	out := usageWindow{Key: key, Label: label, Utilization: w.UsedPercent, WindowSeconds: windowSeconds}
	if !w.ResetAt.IsZero() {
		out.ResetsAt = w.ResetAt.Format(time.RFC3339)
	}
	return out
}

// fillWindowTokens annotates every rate-limit window with the tokens this
// gateway actually routed to that account's platform inside the window, read
// from the requests ledger. It is best-effort: a query error leaves the count
// at 0 so the screen degrades to "not measured" rather than failing whole.
func fillWindowTokens(ctx context.Context, db *sql.DB, accounts []accountUsage) {
	if db == nil {
		return
	}
	now := time.Now().Unix()
	for ai := range accounts {
		platform := accounts[ai].Provider
		for wi := range accounts[ai].Windows {
			w := &accounts[ai].Windows[wi]
			if w.WindowSeconds <= 0 {
				continue
			}
			var tokens int64
			if err := db.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(input_tokens + output_tokens), 0) FROM requests WHERE platform = ? AND created_at >= ?`,
				platform, now-w.WindowSeconds).Scan(&tokens); err != nil {
				continue
			}
			w.TokensUsed = tokens
		}
	}
}

func hyperUsage(ctx context.Context, src gateway.CredentialSource, p gateway.LinkableProvider) accountUsage {
	out := accountUsage{
		Provider: "hyper", Name: providerDisplayName(p, "Charm Hyper"),
		Unit: "Hypercredits",
		// GET /v1/credits publishes only one mixed running balance. Every
		// account gets a 100-Hypercredit monthly grant, while purchased bundles
		// do not expire; presenting that grant as this account's total or
		// deriving a reset from the balance would therefore lie.
		Note: "Current balance from Hyper. Published free grant: 100 Hypercredits/month. Purchased bundles can raise this balance and do not expire. Hyper publishes no 24h usage/limit or account-specific monthly total/reset.",
	}
	token, _, err := src.Credential(ctx, "hyper")
	if err != nil {
		out.NeedsSignIn = true
		out.Error = err.Error()
		return out
	}
	credits, err := fetchHyperCredits(ctx, token)
	if err != nil {
		out.NeedsSignIn = authenticationFailure(err)
		out.Error = redactSecret(err.Error(), token)
		return out
	}
	balance := credits.Balance
	out.Balance = &balance
	return out
}

func authenticationFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"401", "403", "unauthor", "forbidden", "expired", "revoked", "sign in again"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func providerDisplayName(p gateway.LinkableProvider, fallback string) string {
	if name := strings.TrimSpace(p.Name); name != "" {
		return name
	}
	return fallback
}

// redactSecret removes a bearer from an error string before it reaches the
// client, defending against any future upstream error that echoes the token.
func redactSecret(msg, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "«redacted»")
}

// hyperCreditsBaseURL mirrors hyper.BaseURL() (honouring HYPER_URL). It is a
// var so a test can point the live probe at a stub server.
var hyperCreditsBaseURL = hyper.BaseURL()

var hyperUsageClient = &http.Client{
	Timeout: 15 * time.Second,
	// A Hyper bearer must never chase a redirect onto another host.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// fetchHyperCreditsLive reads the authenticated Hyper credit balance. GET
// /v1/credits is informational and returns {"balance": <n>}; the bearer is
// never echoed into an error.
func fetchHyperCreditsLive(ctx context.Context, token string) (hyperCredits, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return hyperCredits{}, fmt.Errorf("hyper credits lookup needs an access token")
	}
	endpoint := strings.TrimRight(hyperCreditsBaseURL, "/") + "/v1/credits"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return hyperCredits{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "prowl")
	resp, err := hyperUsageClient.Do(req)
	if err != nil {
		return hyperCredits{}, fmt.Errorf("hyper credits request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return hyperCredits{}, fmt.Errorf("read hyper credits: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The proxy answers with {"error":{"message","type"}}; surface the
		// message it chose, not the request that carried the bearer.
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && strings.TrimSpace(env.Error.Message) != "" {
			return hyperCredits{}, fmt.Errorf("hyper credits: %s", env.Error.Message)
		}
		return hyperCredits{}, fmt.Errorf("hyper credits endpoint returned %d", resp.StatusCode)
	}
	var parsed struct {
		Balance float64 `json:"balance"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return hyperCredits{}, fmt.Errorf("parse hyper credits: %w", err)
	}
	return hyperCredits{Balance: parsed.Balance}, nil
}
