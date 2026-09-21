package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neur0map/prowl/internal/gateway"
)

// registerStatusRoutes mounts the status-and-tier surface every page reaches
// for but that describes a hosted or optional capability: the premium licence
// panel, the free-tier budget overview, and the response-cache and prompt-
// compression readouts. Each route is session gated, matching the reference's
// requireAuth mount of the premium, free-tier, cache and compression routers
// (server/src/app.ts:255-263).
//
// Premium is a paid, hosted tier of the upstream product. Prowl is a local
// install with no such service, so the premium family answers with a well
// formed "not subscribed" status and refuses the mutations clearly, rather than
// 404ing (which would break the Premium page) or fabricating a subscription.
// The cache and compression readouts describe features Prowl does not implement
// yet, so they report disabled and zeroed with the right shape instead of a
// fake success or a 500.
func (s *Server) registerStatusRoutes() {

	s.mux.HandleFunc("GET /api/free-tier", s.RequireKey(s.handleFreeTier))

	s.mux.HandleFunc("GET /api/cache/stats", s.RequireKey(handleCacheStats))

}

// ── Premium ──────────────────────────────────────────────────────────────────

// licenseStatus documents the cached-licence shape the client's LicenseStatus
// hook expects (use-premium.ts:4-12, catalog-sync.ts:75-83). Prowl never has a
// licence, so the premium payload always carries a null licence; the type is
// kept for the contract it records.
type licenseStatus struct {
	Valid             bool    `json:"valid"`
	Plan              *string `json:"plan"`
	Status            *string `json:"status"`
	ExpiresAt         *string `json:"expiresAt"`
	CancelAtPeriodEnd bool    `json:"cancelAtPeriodEnd,omitempty"`
	Reason            string  `json:"reason,omitempty"`
	CheckedAtMs       int64   `json:"checkedAtMs"`
}

// catalogSyncState is the catalog-feed panel's view (use-premium.ts:14-20,
// catalog-sync.ts:785-791). Every field but baseUrl stays null on a bundled,
// never-synced install, which the page renders as "monthly snapshot / bundled /
// last checked never".
type catalogSyncState struct {
	BaseURL        string  `json:"baseUrl"`
	AppliedVersion *string `json:"appliedVersion"`
	AppliedTier    *string `json:"appliedTier"`
	LastSyncMs     *int64  `json:"lastSyncMs"`
	LastError      *string `json:"lastError"`
}

// premiumStatus is the whole Premium page payload (use-premium.ts:22-28,
// premium.ts:21-31). The unsubscribed values here let every field resolve so
// the page renders its free-tier path: no key, no licence, and a bundled
// catalog.
type premiumStatus struct {
	HasKey    bool             `json:"hasKey"`
	MaskedKey *string          `json:"maskedKey"`
	License   *licenseStatus   `json:"license"`
	Catalog   catalogSyncState `json:"catalog"`
	SiteURL   string           `json:"siteUrl"`
}

// ── Free tier ────────────────────────────────────────────────────────────────

// freeTierPoolQuota is a pool's live headroom, with the metric named so a
// request counter is never read as a token budget. Limit and remaining are
// nullable: a provider that publishes no number on an axis must render as "-",
// not as zero (use-premium/pool-legend.ts:25-31).
type freeTierPoolQuota struct {
	Limit     *int64  `json:"limit"`
	Remaining *int64  `json:"remaining"`
	ResetAt   *string `json:"resetAt"`
	Metric    string  `json:"metric"`
	KeyCount  int     `json:"keyCount"`
}

// freeTierPool is one provider allowance pool and the chain models that draw
// from it. The legend joins its model rows to a pool by memberModelIds, so that
// field is the model ids ordered as the chain returns them (free-tier.ts:32-47,
// pool-legend.ts:15-32).
type freeTierPool struct {
	PoolKey            string             `json:"poolKey"`
	Platform           string             `json:"platform"`
	MemberModelIDs     []string           `json:"memberModelIds"`
	ModelCount         int                `json:"modelCount"`
	DisabledModelCount int                `json:"disabledModelCount"`
	KeyCount           int                `json:"keyCount"`
	DocumentedBudget   float64            `json:"documentedBudget"`
	BestLabel          string             `json:"bestLabel"`
	Kind               string             `json:"kind"`
	Quota              *freeTierPoolQuota `json:"quota"`
}

type freeTierSummary struct {
	PoolCount               int     `json:"poolCount"`
	DocumentedMonthlyTokens float64 `json:"documentedMonthlyTokens"`
	CreditsBasedPools       int     `json:"creditsBasedPools"`
	UnpublishedPools        int     `json:"unpublishedPools"`
}

type freeTierResponse struct {
	GeneratedAt string          `json:"generatedAt"`
	Summary     freeTierSummary `json:"summary"`
	Pools       []freeTierPool  `json:"pools"`
}

// poolAgg accumulates one pool while the model rows are scanned. It mirrors the
// reference PoolAgg (free-tier.ts:32-47); documentedBudget here is the
// per-account value, scaled by usable keys only when the response is built.
type poolAgg struct {
	poolKey            string
	platform           string
	memberModelIDs     []string
	modelCount         int
	disabledModelCount int
	keyCount           int
	documentedBudget   float64
	bestLabel          string
	kind               string
}

// handleFreeTier reports the pool-deduped free-tier budget overview the
// token-usage bar groups its legend by (#905, free-tier.ts:106-205). It uses
// the same model set, key scaling and pool key as GET /api/fallback/token-usage
// so the two never disagree about the same pool.
func (s *Server) handleFreeTier(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := s.engine.DB()

	// Only platforms with an enabled key have a free tier to speak of - the
	// same filter the stacked bar uses (free-tier.ts:109-114).
	platformSet := map[string]bool{}
	if rows, err := db.QueryContext(ctx, `SELECT DISTINCT platform FROM api_keys WHERE enabled = 1`); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				platformSet[p] = true
			}
		}
		rows.Close()
	}

	// Usable keys per platform (enabled + healthy/unknown): a documented budget
	// is per account, so two usable accounts are twice the pool
	// (free-tier.ts:115-120).
	keyCounts := keyCountsByPlatform(db, true)

	// Chain membership mirrors /token-usage: the active profile's chain when one
	// is active, every enabled model otherwise. A chain-disabled row still
	// draws from the same provider allowance, so it is marked, never dropped
	// (free-tier.ts:122-138).
	activeID, active := routingActiveProfileID(db)
	var query string
	var args []any
	if active {
		// Prowl's profile_models has no per-row enabled flag (unlike the
		// reference), so a profile chain's models are all live - the same 1 AS
		// enabled the token-usage read uses (routing_routes.go:1223).
		query = `SELECT m.platform, m.model_id, m.monthly_token_budget, 1 AS chain_enabled
			FROM profile_models pm JOIN models m ON m.id = pm.model_db_id
			WHERE pm.profile_id = ? AND m.enabled = 1 AND m.available = 1
			ORDER BY pm.position ASC`
		args = append(args, activeID)
	} else {
		query = `SELECT m.platform, m.model_id, m.monthly_token_budget, COALESCE(fc.enabled, 1) AS chain_enabled
			FROM models m LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
			WHERE m.enabled = 1 AND m.available = 1`
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read free-tier budgets")
		return
	}
	defer rows.Close()

	pools := map[string]*poolAgg{}
	var order []string
	for rows.Next() {
		var platform, modelID, budgetLabel string
		var chainEnabled int
		if err := rows.Scan(&platform, &modelID, &budgetLabel, &chainEnabled); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not read free-tier budgets")
			return
		}
		if !platformSet[platform] {
			continue
		}
		poolKey := gateway.InferQuotaPool(platform, modelID)
		budget := parseBudgetTokens(budgetLabel)
		p := pools[poolKey]
		if p == nil {
			p = &poolAgg{
				poolKey:        poolKey,
				platform:       platform,
				memberModelIDs: []string{},
				keyCount:       keyCounts[platform],
				bestLabel:      budgetLabel,
				kind:           "unpublished",
			}
			pools[poolKey] = p
			order = append(order, poolKey)
		}
		p.memberModelIDs = append(p.memberModelIDs, modelID)
		p.modelCount++
		if chainEnabled == 0 {
			p.disabledModelCount++
		}
		// One budget per pool: keep the largest documented value.
		if budget > p.documentedBudget {
			p.documentedBudget = budget
			p.bestLabel = budgetLabel
		}
		if p.kind != "documented" {
			if budget > 0 {
				p.kind = "documented"
			} else if strings.Contains(strings.ToLower(budgetLabel), "credit") {
				p.kind = "credits"
			}
		}
	}

	// Prowl observes provider quota per credential (PoolKey = platform/keyId),
	// not per tier the way the reference does, and each observation carries the
	// whole account allowance. So group the observations by platform and show a
	// platform's live headroom against each pool that platform feeds; summing
	// the documented budgets (below) is unaffected.
	quotaByPlatform := map[string][]gateway.QuotaState{}
	if states, err := s.engine.Ledger().QuotaStates(ctx); err == nil {
		for _, q := range states {
			quotaByPlatform[q.Platform] = append(quotaByPlatform[q.Platform], q)
		}
	}

	poolList := make([]freeTierPool, 0, len(order))
	for _, key := range order {
		p := pools[key]
		poolList = append(poolList, freeTierPool{
			PoolKey:            p.poolKey,
			Platform:           p.platform,
			MemberModelIDs:     p.memberModelIDs,
			ModelCount:         p.modelCount,
			DisabledModelCount: p.disabledModelCount,
			KeyCount:           p.keyCount,
			// Pooled capacity is one documented allowance per usable account
			// (free-tier.ts:184-189). A pool can survive into this list on an
			// enabled-but-unusable credential (invalid/error), leaving zero
			// usable keys: there is no account to draw the allowance from, so
			// the pooled budget is zero, not a fabricated single account's
			// worth.
			DocumentedBudget: p.documentedBudget * float64(p.keyCount),
			BestLabel:        p.bestLabel,
			Kind:             p.kind,
			Quota:            summarizePoolQuota(quotaByPlatform[p.platform]),
		})
	}
	sort.SliceStable(poolList, func(i, j int) bool {
		if poolList[i].DocumentedBudget != poolList[j].DocumentedBudget {
			return poolList[i].DocumentedBudget > poolList[j].DocumentedBudget
		}
		return poolList[i].PoolKey < poolList[j].PoolKey
	})

	summary := freeTierSummary{PoolCount: len(poolList)}
	for _, p := range poolList {
		switch p.Kind {
		case "documented":
			summary.DocumentedMonthlyTokens += p.DocumentedBudget
		case "credits":
			summary.CreditsBasedPools++
		case "unpublished":
			summary.UnpublishedPools++
		}
	}

	WriteJSON(w, http.StatusOK, freeTierResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Summary:     summary,
		Pools:       poolList,
	})
}

// metric preference names the axis worth reporting for a pool: a token budget
// reads better than a request counter, which a 429 tends to pin at zero
// (free-tier.ts:57-62). Prowl records only the tokens and requests axes.
func summarizePoolQuota(states []gateway.QuotaState) *freeTierPoolQuota {
	if len(states) == 0 {
		return nil
	}
	metric := "requests"
	for _, q := range states {
		if q.TokensLimit != nil || q.TokensRemaining != nil {
			metric = "tokens"
			break
		}
	}
	// A pool is one allowance per account, so limit/remaining sum across the
	// platform's credentials rather than latest-wins. A nil axis on a credential
	// contributes nothing and must not read as zero (free-tier.ts:89-102).
	//
	// An observation whose reset has already passed is stale: the window has
	// refilled, so - consistent with KeyHeadroom and the provider-status
	// surfaces - its live headroom is a full budget, not the exhausted count
	// last seen. With a known limit the remaining refills to that limit; with no
	// published limit the refilled amount is unknown, so the credential's stale
	// remaining is omitted rather than carried forward as a spent pool.
	nowSec := time.Now().Unix()
	var limit, remaining *int64
	var resetAt *int64
	for _, q := range states {
		l, rem := q.RequestsLimit, q.RequestsRemaining
		if metric == "tokens" {
			l, rem = q.TokensLimit, q.TokensRemaining
		}
		expired := quotaWindowExpired(q.ResetsAt, nowSec)
		if l != nil {
			v := deref(limit) + *l
			limit = &v
		}
		switch {
		case expired && l != nil:
			v := deref(remaining) + *l
			remaining = &v
		case expired:
			// Refilled but unbounded: omit the stale remaining entirely.
		case rem != nil:
			v := deref(remaining) + *rem
			remaining = &v
		}
		// Only a future reset is worth showing: a past reset has already fired,
		// so surfacing it would present a stale moment as the pool's next reset.
		if !expired && q.ResetsAt != nil && (resetAt == nil || *q.ResetsAt < *resetAt) {
			resetAt = q.ResetsAt
		}
	}
	return &freeTierPoolQuota{
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   unixToISO(resetAt),
		Metric:    metric,
		KeyCount:  len(states),
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// unixToISO renders an observed reset moment (Unix seconds) as the ISO string
// the client parses, or nil when the provider published no reset.
func unixToISO(sec *int64) *string {
	if sec == nil {
		return nil
	}
	s := time.Unix(*sec, 0).UTC().Format(time.RFC3339)
	return &s
}

// ── Response cache ───────────────────────────────────────────────────────────

// cacheStats is GET /api/cache/stats (cache.ts:18-28, cache.ts:719-743). Prowl
// has no response cache, so the readout is honestly disabled and zeroed rather
// than absent: the Analytics page keys its "cache" card on enabled and, seeing
// false, never renders the card or reads the counters.
type cacheStats struct {
	Enabled                bool    `json:"enabled"`
	TTLSeconds             int     `json:"ttlSeconds"`
	MaxEntries             int     `json:"maxEntries"`
	MaxTemperature         float64 `json:"maxTemperature"`
	Entries                int     `json:"entries"`
	TotalHits              int     `json:"totalHits"`
	EstimatedRequestsSaved int     `json:"estimatedRequestsSaved"`
	SavedPromptTokens      int     `json:"savedPromptTokens"`
	SavedCompletionTokens  int     `json:"savedCompletionTokens"`
	LookupHits             int     `json:"lookupHits"`
	LookupMisses           int     `json:"lookupMisses"`
	HitRate                float64 `json:"hitRate"`
	SavedTokens            int     `json:"savedTokens"`
}

func handleCacheStats(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, cacheStats{})
}

// ── Prompt compression ───────────────────────────────────────────────────────

// decodeJSONString reports whether raw is a JSON string and returns its value.
func decodeJSONString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// contentChars counts a message's content characters the way an uncompressed
// size would: the decoded length of string content, or the raw byte length of
// structured content. It only feeds the informational char/token counters.
func contentChars(msg json.RawMessage) int {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil || len(m.Content) == 0 {
		return 0
	}
	if s, ok := decodeJSONString(m.Content); ok {
		return utf8.RuneCountInString(s)
	}
	return len(m.Content)
}
