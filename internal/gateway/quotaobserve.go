package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Observed quota is what a provider itself tells us about remaining budget,
// read from the rate-limit headers on a live response. It is the counterpart
// to the locally-counted rate_limit_usage ledger in quota.go: the ledger knows
// what we have spent, the observation knows what the provider says is left, and
// least-remaining key selection steers toward the roomier credential
// (server/src/services/provider-quota.ts). A provider that exposes no such
// headers must leave no trace rather than have a zero invented for it.

type headerMetric int

const (
	metricRequests headerMetric = iota
	metricTokens
	// metricCredits is a monetary/credit budget with no column in the
	// simplified schema; it is preserved in the raw observation log but does
	// not populate the requests/tokens state numbers.
	metricCredits
)

type headerSpec struct {
	metric                  headerMetric
	limit, remaining, reset string
}

// headerSpecs are the provider-specific rate-limit header families the
// reference observes (provider-quota.ts:177-218 HEADER_SPECS). Only live-seen
// spellings are listed; a header a provider does not send is simply absent and
// contributes nothing.
var headerSpecs = map[string][]headerSpec{
	"router9": {
		{metricCredits, "x-credits-limit", "x-credits-remaining", "x-credits-reset"},
	},
	"septor": {
		{metricRequests, "x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset"},
	},
	"electronhub": {
		{metricRequests, "x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset"},
	},
	"groq": {
		{metricRequests, "x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests"},
		{metricTokens, "x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens"},
	},
	"cerebras": {
		{metricRequests, "x-ratelimit-limit-requests-day", "x-ratelimit-remaining-requests-day", "x-ratelimit-reset-requests-day"},
		{metricTokens, "x-ratelimit-limit-tokens-minute", "x-ratelimit-remaining-tokens-minute", "x-ratelimit-reset-tokens-minute"},
	},
	"openrouter": {
		{metricRequests, "x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests"},
		{metricTokens, "x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens"},
	},
	"radeon": {
		{metricRequests, "x-ratelimit-limit-user-rpm", "x-ratelimit-remaining-user-rpm", "x-ratelimit-reset"},
		{metricCredits, "x-ratelimit-limit-user-daily-usd", "x-ratelimit-remaining-user-daily-usd", "x-ratelimit-reset-user-daily-usd"},
	},
	"modelscope": {
		{metricRequests, "modelscope-ratelimit-requests-limit", "modelscope-ratelimit-requests-remaining", "modelscope-ratelimit-requests-reset"},
	},
}

// MetricObservation is one axis of what a provider reported.
type MetricObservation struct {
	Limit        int64
	Remaining    int64
	HasLimit     bool
	HasRemaining bool
}

// QuotaObservation is what a single response told us about a credential pool.
// A nil return from ObserveResponse means the response carried no quota signal
// at all.
type QuotaObservation struct {
	Platform   string
	PoolKey    string
	ObservedAt int64
	Requests   MetricObservation
	Tokens     MetricObservation
	ResetsAt   int64  // 0 when unknown
	Raw        string // the headers_json appended to the observation log
}

func parseHeaderNumber(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

// parseResetToUnix normalises a reset header into Unix seconds. Providers send
// one of three shapes, disambiguated by magnitude (provider-quota.ts:99-105):
// epoch milliseconds, epoch seconds, or seconds-from-now.
func parseResetToUnix(raw string, now time.Time) (int64, bool) {
	n, ok := parseHeaderNumber(raw)
	if !ok {
		return 0, false
	}
	switch {
	case n > 1_000_000_000_000:
		return int64(n / 1000), true
	case n > 1_000_000_000:
		return int64(n), true
	default:
		return now.Unix() + int64(n), true
	}
}

// parseRetryAfterSeconds reads a Retry-After header, which is either
// delta-seconds or an HTTP-date.
func parseRetryAfterSeconds(raw string, now time.Time) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := int64(t.Sub(now).Seconds())
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// ObserveResponse parses a live response's rate-limit headers into the pool's
// quota state and appends the raw observation to the append-only log. It
// returns nil when the response carried no quota signal - a provider without
// these headers degrades to no observation, never to a fabricated zero. A 429
// or 402 is itself a signal that the pool is spent, so it records remaining=0
// even when no headers accompany it.
func (l *Ledger) ObserveResponse(platform, modelID string, keyID int64, status int, headers http.Header) (*QuotaObservation, error) {
	now := l.clock()
	obs := &QuotaObservation{
		Platform:   platform,
		PoolKey:    PoolKey(platform, keyID),
		ObservedAt: now.Unix(),
	}
	raw := map[string]string{}
	observed := false

	get := func(name string) string {
		if headers == nil {
			return ""
		}
		return headers.Get(name)
	}

	var resetRequests, resetTokens, resetOther int64
	var haveResetRequests, haveResetTokens, haveResetOther bool

	for _, spec := range headerSpecs[platform] {
		limRaw, remRaw, resRaw := get(spec.limit), get(spec.remaining), get(spec.reset)
		lim, hasLim := parseHeaderNumber(limRaw)
		rem, hasRem := parseHeaderNumber(remRaw)
		reset, hasReset := parseResetToUnix(resRaw, now)
		if !hasLim && !hasRem && !hasReset {
			continue // this family is absent; contribute nothing
		}
		observed = true
		if hasLim {
			raw[spec.limit] = limRaw
		}
		if hasRem {
			raw[spec.remaining] = remRaw
		}
		if hasReset {
			raw[spec.reset] = resRaw
		}
		switch spec.metric {
		case metricRequests:
			applyMetric(&obs.Requests, lim, hasLim, rem, hasRem)
			if hasReset {
				resetRequests, haveResetRequests = reset, true
			}
		case metricTokens:
			applyMetric(&obs.Tokens, lim, hasLim, rem, hasRem)
			if hasReset {
				resetTokens, haveResetTokens = reset, true
			}
		case metricCredits:
			if hasReset {
				resetOther, haveResetOther = reset, true
			}
		}
	}

	if raRaw := get("retry-after"); raRaw != "" {
		if secs, ok := parseRetryAfterSeconds(raRaw, now); ok {
			observed = true
			raw["retry-after"] = raRaw
			// A wait means the request budget is spent until it elapses.
			obs.Requests.Remaining, obs.Requests.HasRemaining = 0, true
			if !haveResetRequests {
				resetRequests, haveResetRequests = now.Unix()+secs, true
			}
		}
	}

	if status == 429 || status == 402 {
		observed = true
		obs.Requests.Remaining, obs.Requests.HasRemaining = 0, true
	}

	if !observed {
		return nil, nil
	}

	// A pool metered on both requests and tokens can publish two different
	// reset deadlines, but the schema carries one. A window whose reset has
	// passed reads as a full budget again (KeyHeadroom), so collapsing to the
	// EARLIER reset would treat the later window as replenished while it is
	// still spent - over-admitting it. Retain the later reset instead: both
	// axes stay held until the last one has genuinely rolled over.
	for _, r := range []struct {
		has bool
		at  int64
	}{
		{haveResetRequests, resetRequests},
		{haveResetTokens, resetTokens},
		{haveResetOther, resetOther},
	} {
		if r.has && r.at > obs.ResetsAt {
			obs.ResetsAt = r.at
		}
	}

	payload, err := json.Marshal(struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	}{status, raw})
	if err != nil {
		return nil, err
	}
	obs.Raw = string(payload)

	l.persistObservation(obs)
	return obs, nil
}

func applyMetric(m *MetricObservation, limit float64, hasLimit bool, remaining float64, hasRemaining bool) {
	if hasLimit {
		m.Limit, m.HasLimit = int64(limit), true
	}
	if hasRemaining {
		m.Remaining, m.HasRemaining = int64(remaining), true
	}
}

// persistObservation upserts the pool's state (fresh numbers win, absent
// numbers keep their prior value) and appends the raw observation. Both writes
// share one transaction so a reader never sees the state and its explaining
// observation disagree. A write failure is best-effort, like the rest of the
// ledger.
func (l *Ledger) persistObservation(obs *QuotaObservation) {
	hasStateData := obs.Requests.HasLimit || obs.Requests.HasRemaining ||
		obs.Tokens.HasLimit || obs.Tokens.HasRemaining || obs.ResetsAt != 0

	tx, err := l.db.Begin()
	if err != nil {
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if hasStateData {
		if _, err := tx.Exec(
			`INSERT INTO provider_quota_state (
				quota_pool_key, platform, requests_limit, requests_remaining,
				tokens_limit, tokens_remaining, resets_at, observed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(quota_pool_key) DO UPDATE SET
				platform = excluded.platform,
				requests_limit = COALESCE(excluded.requests_limit, provider_quota_state.requests_limit),
				requests_remaining = COALESCE(excluded.requests_remaining, provider_quota_state.requests_remaining),
				tokens_limit = COALESCE(excluded.tokens_limit, provider_quota_state.tokens_limit),
				tokens_remaining = COALESCE(excluded.tokens_remaining, provider_quota_state.tokens_remaining),
				resets_at = COALESCE(excluded.resets_at, provider_quota_state.resets_at),
				observed_at = excluded.observed_at`,
			obs.PoolKey, obs.Platform,
			nullIntFrom(obs.Requests.HasLimit, obs.Requests.Limit),
			nullIntFrom(obs.Requests.HasRemaining, obs.Requests.Remaining),
			nullIntFrom(obs.Tokens.HasLimit, obs.Tokens.Limit),
			nullIntFrom(obs.Tokens.HasRemaining, obs.Tokens.Remaining),
			nullIntFrom(obs.ResetsAt != 0, obs.ResetsAt),
			obs.ObservedAt,
		); err != nil {
			return
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO provider_quota_observations (quota_pool_key, observed_at, headers_json)
		 VALUES (?, ?, ?)`,
		obs.PoolKey, obs.ObservedAt, obs.Raw,
	); err != nil {
		return
	}

	if tx.Commit() != nil {
		return
	}
	committed = true
}

func nullIntFrom(present bool, value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: present}
}

// ── QuotaHeadroom (least-remaining key selection) ────────────────────────────

// KeyHeadroom is the fraction of its observed budget each key of platform has
// left, keyed by api_keys.id, in [0,1] where 1 is untouched and 0 exhausted. A
// key with no usable observation is absent from the map - a miss means unknown,
// not zero. A key metered on both requests and tokens takes the worse of them,
// since the binding constraint is what 429s (provider-quota.ts:502-542). A
// window whose reset has already passed reads as a full budget again.
func (l *Ledger) KeyHeadroom(platform string) map[int64]float64 {
	out := make(map[int64]float64)
	rows, err := l.db.Query(
		`SELECT quota_pool_key, requests_limit, requests_remaining,
		        tokens_limit, tokens_remaining, resets_at
		   FROM provider_quota_state
		  WHERE platform = ?`,
		platform,
	)
	if err != nil {
		return out
	}
	defer rows.Close()

	nowSec := l.clock().Unix()
	prefix := platform + "/"
	for rows.Next() {
		var poolKey string
		var reqLim, reqRem, tokLim, tokRem, resets sql.NullInt64
		if err := rows.Scan(&poolKey, &reqLim, &reqRem, &tokLim, &tokRem, &resets); err != nil {
			continue
		}
		rest := strings.TrimPrefix(poolKey, prefix)
		if rest == poolKey {
			continue // not a per-key pool of this platform
		}
		keyID, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			continue
		}
		expired := resets.Valid && resets.Int64 > 0 && resets.Int64 <= nowSec

		frac := math.Inf(1)
		consider := func(lim, rem sql.NullInt64) {
			if !lim.Valid || lim.Int64 <= 0 || !rem.Valid {
				return
			}
			r := 1.0
			if !expired {
				r = clamp01(float64(rem.Int64) / float64(lim.Int64))
			}
			if r < frac {
				frac = r
			}
		}
		consider(reqLim, reqRem)
		consider(tokLim, tokRem)
		if math.IsInf(frac, 1) {
			continue
		}
		if prev, ok := out[keyID]; !ok || frac < prev {
			out[keyID] = frac
		}
	}
	return out
}

// AccountScoped reports whether the platform's quota pool for modelID is one
// shared account budget. Least-remaining key selection is skipped for these,
// because ranking keys by their own remaining quota is meaningless when they
// draw from a single account allowance (reference: inferQuotaPoolKey(...)
// ending in "::account", provider-quota.ts:112-168, router.ts:1374-1398).
func (l *Ledger) AccountScoped(platform, modelID string) bool {
	return strings.HasSuffix(inferQuotaPool(platform, modelID), "::account")
}

// InferQuotaPool names the credential pool a provider's published quota
// applies to, which is how a free-tier view groups models that share one
// account allowance. It is exported so the HTTP layer can group without
// duplicating the per-provider table below, since two copies of that table
// would drift the moment a provider changes its pooling.
func InferQuotaPool(platform, modelID string) string {
	return inferQuotaPool(platform, modelID)
}

// inferQuotaPool names the credential pool a provider's published quota applies
// to. It ports provider-quota.ts inferPoolForPlatform (provider-quota.ts:112-168)
// and is used only to classify account-scoped pools; the persisted state is
// keyed by PoolKey, which embeds the credential id.
func inferQuotaPool(platform, modelID string) string {
	m := strings.TrimSpace(modelID)
	switch platform {
	case "openrouter":
		if strings.HasSuffix(m, ":free") {
			return "openrouter::free"
		}
		return "openrouter::account"
	case "google":
		return "google::project"
	case "groq":
		return "groq::account"
	case "cerebras":
		return "cerebras::shared"
	case "sail":
		return "sail::monthly-credit"
	case "electronhub":
		if strings.HasSuffix(m, ":free") {
			return "electronhub::daily-free"
		}
		return "electronhub::weekly-credit"
	case "experiential":
		return "experiential::monthly-credit"
	case "router9":
		return "router9::monthly-credit"
	case "septor":
		return "septor::daily-free"
	case "bai":
		return "bai::promo"
	case "radeon":
		return "radeon::daily-free"
	case "sambanova":
		return "sambanova::shared"
	case "nvidia":
		return "nvidia::credit-pool"
	case "mistral":
		return "mistral::experiment-pool"
	case "github":
		return "github::account"
	case "cohere":
		return "cohere::trial-pool"
	case "cloudflare":
		return "cloudflare::account"
	case "zhipu":
		return "zhipu::account"
	case "ollama":
		return "ollama::cloud"
	case "kilo":
		return "kilo::anonymous"
	case "pollinations":
		return "pollinations::account"
	case "llm7":
		return "llm7::anonymous"
	case "aihorde":
		return "aihorde::anonymous"
	case "huggingface":
		return "huggingface::router"
	case "opencode":
		return "opencode::promo"
	case "routeway":
		return "routeway::free"
	case "bazaarlink":
		return "bazaarlink::free"
	case "ainative":
		return "ainative::account"
	case "aion":
		return "aion::free"
	case "requesty":
		return "requesty::free"
	case "navy":
		return "navy::free"
	case "nara":
		return "nara::free"
	case "sealion":
		return "sealion::free"
	case "orcarouter":
		return "orcarouter::free"
	case "unorouter":
		return "unorouter::free"
	case "xkiro":
		return "xkiro::free"
	case "anyapi":
		return "anyapi::free"
	case "modelscope":
		return "modelscope::account"
	}
	if m != "" {
		return platform + "::" + m
	}
	return platform + "::account"
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// QuotaState is what a provider last told us about its own remaining quota.
// Nil limits mean the provider publishes none on that axis, which is not the
// same as zero remaining and must not be rendered as such.
type QuotaState struct {
	PoolKey           string `json:"poolKey"`
	Platform          string `json:"platform"`
	RequestsLimit     *int64 `json:"requestsLimit"`
	RequestsRemaining *int64 `json:"requestsRemaining"`
	TokensLimit       *int64 `json:"tokensLimit"`
	TokensRemaining   *int64 `json:"tokensRemaining"`
	ResetsAt          *int64 `json:"resetsAt"`
	ObservedAt        int64  `json:"observedAt"`
}

// QuotaStates returns every observed provider quota, newest observation
// first. This is the provider's own account of itself rather than our
// inference, which is why the dashboard shows it alongside our counters: when
// the two disagree, the difference is the interesting part.
func (l *Ledger) QuotaStates(ctx context.Context) ([]QuotaState, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT quota_pool_key, platform, requests_limit, requests_remaining,
		        tokens_limit, tokens_remaining, resets_at, observed_at
		   FROM provider_quota_state
		  ORDER BY observed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("read quota states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []QuotaState
	for rows.Next() {
		var s QuotaState
		if err := rows.Scan(&s.PoolKey, &s.Platform, &s.RequestsLimit, &s.RequestsRemaining,
			&s.TokensLimit, &s.TokensRemaining, &s.ResetsAt, &s.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
