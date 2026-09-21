package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
)

// The machine-readable status pair from the reference (status.ts:112-239):
// GET /v1/providers and GET /v1/quota-forecast.
//
// Both sit behind the unified key like the rest of /v1 and carry no key
// material. They exist so a caller in front of this gateway can decide "route
// here or skip" and "is this pool nearly spent" WITHOUT spending a request to
// find out - the dashboard's own views are session-authenticated and shaped
// for a browser, so they cannot serve that purpose.

// Low-balance thresholds, ported from quota-forecast.ts:17-23. The absolute
// floor only applies to windows large enough for "20 left" to be alarming:
// below that a small tier would warn from its first request onwards.
const (
	lowBalanceThreshold       = 0.1
	lowBalanceAbsolute        = 20
	lowBalanceAbsoluteMinimum = 200
)

func (s *Server) registerStatusV1Routes() {
	s.mux.HandleFunc("GET /v1/providers", s.RequireMachineKey(s.handleV1Providers))
	s.mux.HandleFunc("GET /v1/quota-forecast", s.RequireMachineKey(s.handleV1QuotaForecast))
}

type v1Provider struct {
	Platform string `json:"platform"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Keys     int    `json:"keys"`

	ResumeAt             string `json:"resume_at,omitempty"`
	LastError            string `json:"last_error,omitempty"`
	RequestsRemainingPct *int   `json:"requests_remaining_pct,omitempty"`
}

func (s *Server) handleV1Providers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.engine.DB().QueryContext(r.Context(),
		`SELECT platform, id, status FROM api_keys WHERE enabled = 1`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	defer func() { _ = rows.Close() }()

	// Per-key rather than a GROUP BY count: whether a platform is rate_limited
	// turns on which of its healthy credentials are benched, which an aggregate
	// count cannot answer. healthyKeys keeps the ids so they can be intersected
	// with the bench set below.
	type platformRow struct {
		enabled, healthy, unknown, invalid, errored int
		healthyKeys                                 []int64
	}
	byPlatform := map[string]platformRow{}
	for rows.Next() {
		var (
			platform string
			id       int64
			status   string
		)
		if err := rows.Scan(&platform, &id, &status); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
			return
		}
		row := byPlatform[platform]
		row.enabled++
		switch status {
		case "healthy":
			row.healthy++
			row.healthyKeys = append(row.healthyKeys, id)
		case "unknown":
			row.unknown++
		case "invalid":
			row.invalid++
		case "error":
			row.errored++
		}
		byPlatform[platform] = row
	}
	if err := rows.Err(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}

	// Benches are per (platform, model, key): a key here is cooling for at
	// least one of its models but may still route others. Availability is
	// therefore decided over the actual routes a key serves, not the key as a
	// whole - a cooldown on model A must never hide model B on the same key.
	// benchedByKey answers "does this key have any active bench" for the fast
	// path; routable is the (model, key, endpoint) set each key can serve, so a
	// benched key is only unavailable once EVERY route it serves is benched.
	cds := s.engine.Cooldowns()
	benchedByKey := cds.BenchedByKey()
	routable := s.routableModelsByKey(r.Context())

	lastError := map[string]string{}
	if errRows, err := s.engine.DB().QueryContext(r.Context(), `
		SELECT platform, last_health_error
		  FROM api_keys
		 WHERE enabled = 1 AND last_health_error IS NOT NULL
		 ORDER BY last_checked_at DESC`); err == nil {
		defer func() { _ = errRows.Close() }()
		for errRows.Next() {
			var platform, msg string
			if err := errRows.Scan(&platform, &msg); err != nil {
				break
			}
			if _, seen := lastError[platform]; !seen {
				lastError[platform] = msg
			}
		}
	}

	// Tightest observed request headroom per platform: with several keys on
	// one account pool, the number that decides "can I keep calling" is the
	// smallest. An observation whose reset has already passed is stale: the
	// window has refilled, so it reads as full (KeyHeadroom applies the same
	// rule) rather than as the exhausted count last seen.
	headroom := map[string]int{}
	states, _ := s.engine.Ledger().QuotaStates(r.Context())
	nowSec := time.Now().Unix()
	for _, st := range states {
		// Unknown is not zero: a limit with no observed remaining tells us
		// nothing about headroom, so it is skipped rather than shown as spent
		// (KeyHeadroom drops the same metric).
		if st.RequestsLimit == nil || *st.RequestsLimit <= 0 || st.RequestsRemaining == nil {
			continue
		}
		var pct int
		if quotaWindowExpired(st.ResetsAt, nowSec) {
			pct = 100
		} else {
			pct = min(max(int(math.Round(float64(*st.RequestsRemaining)/float64(*st.RequestsLimit)*100)), 0), 100)
		}
		if prev, seen := headroom[st.Platform]; !seen || pct < prev {
			headroom[st.Platform] = pct
		}
	}

	counts := map[string]int{"healthy": 0, "rate_limited": 0, "invalid": 0, "unknown": 0}
	providers := make([]v1Provider, 0, len(byPlatform))
	for platform, row := range byPlatform {
		if row.enabled == 0 {
			continue
		}

		// A platform is rate_limited only when EVERY healthy route is benched.
		// A route is one (model, key, endpoint) the router would attempt, so a
		// key stays available while any model it serves is not benched - one
		// model's cooldown must not mask a sibling the same key still serves.
		// resume_at then describes that unavailable set: the earliest benched
		// route to free up - the moment a route returns, not the earliest bench
		// of any key (an invalid key's cooldown lifting gives nothing back).
		var resume time.Time
		healthyAvailable := 0
		for _, keyID := range row.healthyKeys {
			kb, hasBench := benchedByKey[keyID]
			if !hasBench {
				// Nothing benched: every route this key serves is live.
				healthyAvailable++
				continue
			}
			available, frees := keyRouteAvailability(cds, platform, keyID, routable[keyID], kb)
			if available {
				healthyAvailable++
				continue
			}
			if resume.IsZero() || frees.Before(resume) {
				resume = frees
			}
		}
		cooling := row.healthy > 0 && healthyAvailable == 0

		var status string
		switch {
		case healthyAvailable > 0:
			status = "healthy"
		case cooling:
			status = "rate_limited"
		case row.unknown > 0:
			status = "unknown"
		case row.invalid > 0 || row.errored > 0:
			status = "invalid"
		default:
			status = "unknown"
		}
		counts[status]++

		entry := v1Provider{
			Platform: platform,
			Name:     catalogName(platform),
			Status:   status,
			Keys:     row.enabled,
		}
		if status == "rate_limited" {
			entry.ResumeAt = resume.UTC().Format(time.RFC3339)
		}
		if status == "invalid" {
			entry.LastError = gateway.RedactWith(lastError[platform])
		}
		if pct, ok := headroom[platform]; ok {
			entry.RequestsRemainingPct = &pct
		}
		providers = append(providers, entry)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Platform < providers[j].Platform })

	WriteJSON(w, http.StatusOK, map[string]any{"providers": providers, "counts": counts})
}

type quotaForecastEntry struct {
	Platform          string  `json:"platform"`
	Pool              string  `json:"pool"`
	Used              *int64  `json:"used"`
	Remaining         *int64  `json:"remaining"`
	Limit             *int64  `json:"limit"`
	RemainingPct      *int    `json:"remaining_pct"`
	ResetAt           *string `json:"reset_at"`
	LowBalance        bool    `json:"low_balance"`
	SecondsUntilReset *int64  `json:"seconds_until_reset"`
}

func (s *Server) handleV1QuotaForecast(w http.ResponseWriter, r *http.Request) {
	states, err := s.engine.Ledger().QuotaStates(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}

	now := time.Now().UTC()
	nowSec := now.Unix()
	// Deduped to the TIGHTEST row per pool: keys sharing an account report the
	// same window, and the least headroom is the one that binds.
	byPool := map[string]quotaForecastEntry{}
	for _, st := range states {
		// Only request windows are predictable from quota headers; token
		// pools reset too differently across providers to forecast honestly.
		if st.RequestsLimit == nil || *st.RequestsLimit <= 0 {
			continue
		}
		limit := *st.RequestsLimit
		entry := quotaForecastEntry{
			Platform: st.Platform,
			Pool:     st.PoolKey,
			Limit:    &limit,
		}
		if entry.Pool == "" {
			entry.Pool = st.Platform + "::default"
		}
		if st.RequestsRemaining != nil {
			remaining := *st.RequestsRemaining
			if quotaWindowExpired(st.ResetsAt, nowSec) {
				// The reset has passed: the pool has refilled, so report it
				// full rather than carrying the stale exhausted count forward
				// as a low balance the provider has already replenished.
				remaining = limit
			}
			used := max(limit-remaining, 0)
			pct := min(max(int(math.Round(float64(remaining)/float64(limit)*100)), 0), 100)
			entry.Remaining = &remaining
			entry.Used = &used
			entry.RemainingPct = &pct
			entry.LowBalance = (limit >= lowBalanceAbsoluteMinimum && remaining <= lowBalanceAbsolute) ||
				float64(remaining)/float64(limit) < lowBalanceThreshold
		}
		if st.ResetsAt != nil {
			reset := time.Unix(*st.ResetsAt, 0).UTC()
			iso := reset.Format(time.RFC3339)
			entry.ResetAt = &iso
			if secs := int64(reset.Sub(now).Seconds()); secs > 0 {
				entry.SecondsUntilReset = &secs
			}
		}

		prev, seen := byPool[entry.Pool]
		if !seen || tighter(entry, prev) {
			byPool[entry.Pool] = entry
		}
	}

	pools := make([]quotaForecastEntry, 0, len(byPool))
	for _, entry := range byPool {
		pools = append(pools, entry)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Pool < pools[j].Pool })

	WriteJSON(w, http.StatusOK, map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"low_balance_threshold": map[string]any{
			"pct":                lowBalanceThreshold,
			"absolute":           lowBalanceAbsolute,
			"absolute_min_limit": lowBalanceAbsoluteMinimum,
		},
		"pools": pools,
	})
}

// tighter reports whether a has less headroom than b. A known remaining always
// beats an unknown one: an unmeasured pool must not mask a nearly spent pool.
func tighter(a, b quotaForecastEntry) bool {
	if a.Remaining == nil {
		return false
	}
	if b.Remaining == nil {
		return true
	}
	return *a.Remaining < *b.Remaining
}

// keyRouteAvailability decides whether a benched healthy key still has a live
// route, and - when it does not - when its earliest route frees up. models is
// the (model, key, endpoint) set the key can serve; kb summarises its active
// benches.
//
// A key with a known route set stays available while any of those routes is not
// benched: a cooldown on one model never hides a sibling. When the key serves
// no catalogue or bound model we can enumerate - an unseeded or synthetic pool
// - its benches are the only routes known, so a single active bench leaves it
// with nothing left to serve.
func keyRouteAvailability(cds *gateway.CooldownEngine, platform string, keyID int64, models []string, kb gateway.KeyBench) (available bool, frees time.Time) {
	if len(models) == 0 {
		return false, kb.Until
	}
	var earliest time.Time
	for _, m := range models {
		cd, benched := cds.Active(gateway.QuotaKey(platform, m, keyID))
		if !benched {
			return true, time.Time{}
		}
		if earliest.IsZero() || cd.Until.Before(earliest) {
			earliest = cd.Until
		}
	}
	return false, earliest
}

// routableModelsByKey lists, per enabled api_keys.id, the model ids that key can
// actually serve as a route: the shared catalogue models of its platform it is
// scoped to, plus any model bound to it directly - a custom relay's models or an
// enrolled login's. This mirrors the router's chain expansion (expandChainKeys),
// so /v1/providers measures availability over the same (model, key, endpoint)
// routes the router would attempt, and a cooldown on one of a key's models never
// passes for the key having nothing left to serve.
func (s *Server) routableModelsByKey(ctx context.Context) map[int64][]string {
	out := map[int64][]string{}
	db := s.engine.DB()

	// Bound models: a custom relay or a login-enrolled model carries its own
	// key_id and endpoint, so it routes only through that one credential.
	if rows, err := db.QueryContext(ctx,
		`SELECT key_id, model_id FROM models WHERE key_id IS NOT NULL AND enabled = 1 AND available = 1`); err == nil {
		for rows.Next() {
			var keyID int64
			var modelID string
			if err := rows.Scan(&keyID, &modelID); err != nil {
				break
			}
			out[keyID] = append(out[keyID], modelID)
		}
		_ = rows.Close()
	}

	// A scoped key serves only its allow-list, so a catalogue model outside that
	// list is not one of its routes.
	scopes := map[int64][]string{}
	if rows, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(model_scope_json, '') FROM api_keys WHERE enabled = 1`); err == nil {
		for rows.Next() {
			var id int64
			var raw string
			if err := rows.Scan(&id, &raw); err != nil {
				break
			}
			if scope := gateway.ParseModelScope(raw); len(scope) > 0 {
				scopes[id] = scope
			}
		}
		_ = rows.Close()
	}

	// Shared catalogue models carry no key, so the router turns each into one
	// candidate per usable key of its platform. Reproduce that expansion -
	// including its linked-login exclusion (expandChainKeys): a link:<provider>
	// credential is authorised only for the models it discovered, which are
	// seeded as key-bound rows and already counted above, so it must never soak
	// up the platform's whole catalogue. Bound login rows stay routable.
	if rows, err := db.QueryContext(ctx,
		`SELECT k.id, m.model_id
		   FROM api_keys k
		   JOIN models m ON m.platform = k.platform AND m.key_id IS NULL AND m.enabled = 1 AND m.available = 1
		  WHERE k.enabled = 1 AND k.encrypted_key NOT LIKE 'link:%'`); err == nil {
		for rows.Next() {
			var keyID int64
			var modelID string
			if err := rows.Scan(&keyID, &modelID); err != nil {
				break
			}
			if !gateway.ScopeAllows(scopes[keyID], modelID) {
				continue
			}
			out[keyID] = append(out[keyID], modelID)
		}
		_ = rows.Close()
	}
	return out
}

// quotaWindowExpired reports whether a provider quota observation's reset has
// already passed, meaning its remaining count is stale and the window has
// refilled. Consistent with KeyHeadroom, such a window reads as a full budget.
func quotaWindowExpired(resetsAt *int64, nowSec int64) bool {
	return resetsAt != nil && *resetsAt > 0 && *resetsAt <= nowSec
}

// catalogName is the provider's published name, falling back to the platform
// slug when the directory does not list it.
func catalogName(platform string) string {
	entries, err := catalog.Directory()
	if err != nil {
		return platform
	}
	for i := range entries {
		if resolved, ok := gateway.PlatformForCatalogID(entries[i].ID); ok && resolved == platform {
			return entries[i].Name
		}
		if entries[i].ID == platform {
			return entries[i].Name
		}
	}
	return platform
}
