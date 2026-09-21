package api

// The models/routing/chains/profiles surface the dashboard's Models page and
// its routing controls call, ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md;
// server/src/routes/fallback.ts, profiles.ts, models.ts).
//
// Every route here is session-gated: only RequireSession may answer with
// TypeAuthentication, so a validation or not-found refusal below carries
// TypeInvalidRequest / TypeNotFound and never signs the operator out.
//
// The chain and its scores read from the ported engine - ResolveChain for the
// active chain, the axis scorer for the per-model bandit breakdown, the penalty
// store for demotions, the quota ledger for window headroom - over the
// models / fallback_config / profiles / profile_models / settings tables.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
)

// registerRoutingRoutes wires the whole family onto the shared mux. Custom
// model deletion is registered as its own literal segment so it is never
// shadowed by DELETE /api/models/{id}.
func (s *Server) registerRoutingRoutes() {
	s.mux.HandleFunc("GET /api/fallback", s.RequireKey(s.handleFallbackGet))
	s.mux.HandleFunc("PUT /api/fallback", s.RequireKey(s.handleFallbackPut))
	s.mux.HandleFunc("POST /api/fallback/sort/{preset}", s.RequireKey(s.handleFallbackSort))

	s.mux.HandleFunc("GET /api/fallback/routing", s.RequireKey(s.handleRoutingGet))
	s.mux.HandleFunc("PUT /api/fallback/routing", s.RequireKey(s.handleRoutingPut))
	s.mux.HandleFunc("GET /api/fallback/token-usage", s.RequireKey(s.handleTokenUsage))
	s.mux.HandleFunc("GET /api/fallback/rate-limit-usage", s.RequireKey(s.handleRateLimitUsage))
	s.mux.HandleFunc("GET /api/fallback/penalty-inspector", s.RequireKey(s.handlePenaltyInspectorGet))
	s.mux.HandleFunc("DELETE /api/fallback/penalty-inspector", s.RequireKey(s.handlePenaltyInspectorClear))

	s.mux.HandleFunc("GET /api/profiles", s.RequireKey(s.handleProfilesList))
	s.mux.HandleFunc("GET /api/profiles/active", s.RequireKey(s.handleProfileActiveGet))
	s.mux.HandleFunc("POST /api/profiles/active", s.RequireKey(s.handleProfileActiveSet))
	s.mux.HandleFunc("POST /api/profiles", s.RequireKey(s.handleProfileCreate))
	s.mux.HandleFunc("GET /api/profiles/{id}/models", s.RequireKey(s.handleProfileModels))
	s.mux.HandleFunc("PUT /api/profiles/{id}/reorder", s.RequireKey(s.handleProfileReorder))
	s.mux.HandleFunc("PATCH /api/profiles/{id}", s.RequireKey(s.handleProfileRename))
	s.mux.HandleFunc("DELETE /api/profiles/{id}", s.RequireKey(s.handleProfileDelete))

	s.mux.HandleFunc("GET /api/models", s.RequireKey(s.handleModelsList))
	s.mux.HandleFunc("PATCH /api/models/{id}", s.RequireKey(s.handleModelPatch))
	s.mux.HandleFunc("DELETE /api/models/custom/{id}", s.RequireKey(s.handleModelCustomDelete))
	s.mux.HandleFunc("DELETE /api/models/{id}", s.RequireKey(s.handleModelDelete))
}

// ── settings helpers ────────────────────────────────────────────────────────
//
// The routing knobs live in the shared settings table under routing_* /
// active_profile_id / key_selection_strategy keys. These helpers are
// routing-prefixed so they never collide with a sibling's settings accessor in
// the same package.

func routingGetSetting(db *sql.DB, key string) (string, bool) {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func routingDeleteSetting(db *sql.DB, key string) error {
	_, err := db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

const (
	settingRoutingStrategy      = "routing_strategy"
	settingCustomWeights        = "routing_custom_weights"
	settingExploreEnabled       = "routing_explore_enabled"
	settingPeakHoursAdjust      = "routing_peak_hours_adjust"
	settingPeakStartHour        = "routing_peak_start_hour"
	settingPeakEndHour          = "routing_peak_end_hour"
	settingPeakTimezone         = "routing_peak_timezone"
	settingKeySelectionStrategy = "key_selection_strategy"
	settingCooldownCeilingMs    = "routing_cooldown_ceiling_ms"
	settingActiveProfileID      = "active_profile_id"

	// The cooldown ceiling bounds the router's own guesses only: one minute
	// to a day (fallback.ts:73-76, ratelimit.ts MIN/MAX_COOLDOWN_CEILING_MS).
	minCooldownCeilingMs = int64(60_000)
	maxCooldownCeilingMs = int64(24 * 60 * 60 * 1000)

	// Peak defaults match scoring.ts:62-135 so the dashboard toggle renders a
	// sane window even before the operator has saved one.
	defaultPeakStartHour = 18
	defaultPeakEndHour   = 6
	defaultPeakTimezone  = "UTC"
)

// Validation errors surfaced to the client as 400s. Kept as package vars so a
// handler and its helper agree on the exact wording the dashboard shows.
var (
	errPeakHour        = errors.New("peakStartHour and peakEndHour must be integers between 0 and 23")
	errPeakTimezone    = errors.New("peakTimezone must be a valid IANA timezone name")
	errBadWeights      = errors.New("custom weights must be finite and non-negative")
	errZeroWeights     = errors.New("custom weights cannot all be zero")
	errBadStrategy     = errors.New("strategy must be one of priority, balanced, smartest, efficient, fastest, reliable, custom")
	errBadKeySelection = errors.New("keySelectionStrategy must be auto or least-remaining")
	errCooldownRange   = errors.New("cooldownCeilingMs must be between 60000 (1 minute) and 86400000 (24 hours)")
)

// ── shared shapes ───────────────────────────────────────────────────────────

type weightsOut struct {
	Reliability  float64 `json:"reliability"`
	Speed        float64 `json:"speed"`
	Intelligence float64 `json:"intelligence"`
}

func weightsToOut(w gateway.Weights) weightsOut {
	return weightsOut{Reliability: w.Reliability, Speed: w.Speed, Intelligence: w.Intelligence}
}

// routingCustomWeights reads the operator's saved custom vector, defaulting to
// the balanced preset so the dashboard's custom sliders always have a value to
// render (getCustomWeights, router.ts:538-552).
func routingCustomWeights(db *sql.DB) gateway.Weights {
	raw, ok := routingGetSetting(db, settingCustomWeights)
	if !ok {
		return gateway.WeightsBalanced
	}
	var w weightsOut
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return gateway.WeightsBalanced
	}
	return gateway.Weights{Reliability: w.Reliability, Speed: w.Speed, Intelligence: w.Intelligence}
}

// routingDisplayWeights maps a strategy to the vector a dashboard scores under.
// Priority has no vector of its own, so it is displayed under balanced weights
// - the point of the table is a meaningful ranking even with the bandit off
// (getRoutingScores, router.ts:2207-2209).
func routingDisplayWeights(strategy gateway.RoutingStrategy, custom gateway.Weights) gateway.Weights {
	switch strategy {
	case gateway.RoutingSmartest, gateway.RoutingEfficient:
		return gateway.WeightsSmartest
	case gateway.RoutingFastest:
		return gateway.WeightsFastest
	case gateway.RoutingReliable:
		return gateway.WeightsReliable
	case gateway.RoutingCustom:
		return custom
	default:
		return gateway.WeightsBalanced
	}
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// parseBudgetTokens parses a human budget label ("~120M", "~50-100M", "~500K")
// to its upper-bound token count, requiring an M/K unit so a rate label like
// "free · 40 RPM" reads as no budget rather than 40 tokens (parseBudget,
// lib/budget.ts:5-19).
var budgetLabelRE = regexp.MustCompile(`~?([0-9.]+)(?:-([0-9.]+))?([MK])`)

func parseBudgetTokens(label string) float64 {
	if label == "" {
		return 0
	}
	m := budgetLabelRE.FindStringSubmatch(label)
	if m == nil {
		return 0
	}
	raw := m[1]
	if m[2] != "" {
		raw = m[2]
	}
	high, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	unit := 1000.0
	if m[3] == "M" {
		unit = 1_000_000
	}
	return high * unit
}

// keyCountsByPlatform counts usable keys per platform. healthyOnly matches the
// router's own eligibility (enabled + healthy/unknown) for the capacity pool;
// the plain enabled count is what the Models list reports (fallback.ts:240-244,
// models.ts:237-242).
func keyCountsByPlatform(db *sql.DB, healthyOnly bool) map[string]int {
	q := `SELECT platform, COUNT(*) FROM api_keys WHERE enabled = 1`
	if healthyOnly {
		q += ` AND status IN ('healthy', 'unknown')`
	}
	q += ` GROUP BY platform`
	out := map[string]int{}
	rows, err := db.Query(q)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err == nil {
			out[p] = n
		}
	}
	return out
}

// penaltyMap indexes the live per-model demotions by model id for annotating a
// chain read (getAllPenalties, router.ts:338-347).
func (s *Server) penaltyMap() map[int64]gateway.PenaltyStat {
	out := map[int64]gateway.PenaltyStat{}
	for _, p := range s.engine.Penalties().Snapshot() {
		out[p.ModelDBID] = p
	}
	return out
}

// routingActiveProfileID reads the active profile id, verifying the profile
// still exists - a dangling id means "no active profile" (getActiveProfileId,
// profile-models.ts:3-10). This is the same key the engine's ResolveChain
// consults, so a chain read and a live route always agree on the active chain.
func routingActiveProfileID(db *sql.DB) (int64, bool) {
	raw, ok := routingGetSetting(db, settingActiveProfileID)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	var exists int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		return 0, false
	}
	return id, true
}

// profileExecer runs the activation statements. The active-profile handler
// passes the bare *sql.DB; a set created and activated in one go passes its
// *sql.Tx. Both satisfy ExecContext.
type profileExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// routingActivateProfile makes profileID the one active profile: it clears
// every active flag, sets this one's, and writes the active_profile_id setting
// the engine's ResolveChain reads. This is the single activation path, so a set
// activated from the routing screen and a set created-and-activated from a
// selection change exactly the same state and can never drift.
func routingActivateProfile(ctx context.Context, ex profileExecer, profileID int64) error {
	if _, err := ex.ExecContext(ctx, `UPDATE profiles SET active = 0`); err != nil {
		return err
	}
	if _, err := ex.ExecContext(ctx, `UPDATE profiles SET active = 1 WHERE id = ?`, profileID); err != nil {
		return err
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingActiveProfileID, strconv.FormatInt(profileID, 10), time.Now().Unix())
	return err
}

// ── fallback chain read ─────────────────────────────────────────────────────

type fallbackEntryOut struct {
	// Tier separates what costs nothing from what bills, and from what a
	// subscription already covers. The routing table mixes all three now, and
	// without this the only way to tell them apart is to know the provider.
	Tier        string   `json:"tier"`
	PaidInPerM  *float64 `json:"paidInputPerM"`
	PaidOutPerM *float64 `json:"paidOutputPerM"`

	ModelDBID                int64    `json:"modelDbId"`
	Priority                 int64    `json:"priority"`
	EffectivePriority        float64  `json:"effectivePriority"`
	Penalty                  float64  `json:"penalty"`
	RateLimitHits            int      `json:"rateLimitHits"`
	Enabled                  bool     `json:"enabled"`
	Platform                 string   `json:"platform"`
	ModelID                  string   `json:"modelId"`
	DisplayName              string   `json:"displayName"`
	IntelligenceRank         int      `json:"intelligenceRank"`
	SpeedRank                int      `json:"speedRank"`
	SizeLabel                string   `json:"sizeLabel"`
	RPMLimit                 *int64   `json:"rpmLimit"`
	RPDLimit                 *int64   `json:"rpdLimit"`
	TPMLimit                 *int64   `json:"tpmLimit"`
	TPDLimit                 *int64   `json:"tpdLimit"`
	ContextWindow            *int64   `json:"contextWindow"`
	MonthlyTokenBudget       string   `json:"monthlyTokenBudget"`
	MonthlyTokenBudgetTokens float64  `json:"monthlyTokenBudgetTokens"`
	SupportsVision           bool     `json:"supportsVision"`
	SupportsTools            bool     `json:"supportsTools"`
	Source                   string   `json:"source"`
	KeyID                    *int64   `json:"keyId"`
	KeyLabel                 *string  `json:"keyLabel"`
	EndpointScope            *string  `json:"endpointScope"`
	HasOverrides             bool     `json:"hasOverrides"`
	OverrideFields           []string `json:"overrideFields"`
	KeyCount                 int      `json:"keyCount"`
}

// chainRow is a scanned catalog row before it is ordered and numbered.
type chainRow struct {
	modelDBID     int64
	chainPos      sql.NullInt64
	catalogPos    sql.NullInt64
	inChain       bool
	fcEnabled     sql.NullInt64
	platform      string
	modelID       string
	displayName   string
	intelRank     int
	speedRank     int
	sizeLabel     string
	rpm, rpd      sql.NullInt64
	tpm, tpd      sql.NullInt64
	contextWindow sql.NullInt64
	monthlyBudget string
	vision, tools bool
	source        string
	keyID         sql.NullInt64
	keyLabel      sql.NullString
	endpointScope string
	paidInPerM    sql.NullFloat64
	paidOutPerM   sql.NullFloat64
}

const chainSelectCols = `m.platform, m.model_id, m.display_name, m.intelligence_rank,
	m.speed_rank, m.size_label, m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit,
	m.context_window, m.monthly_token_budget, m.supports_vision, m.supports_tools,
	m.source, m.key_id, m.endpoint_scope, ak.label AS key_label,
	m.paid_input_per_m, m.paid_output_per_m`

// handleFallbackGet returns the active (or ?profile=) chain, penalty-annotated.
//
// An explicit ?profile= pins the read to that chain so a client caching
// per-chain can never write "the active chain, whichever it is" into a specific
// chain's cache (fallback.ts:211-317, #1047). With a profile the read is the
// whole catalog seen through it - members keep their order and switched-on
// flag, the rest list after, switched off - so a hand-built chain renders as
// "the catalog, nothing turned on yet" rather than an empty page (#1021).
func (s *Server) handleFallbackGet(w http.ResponseWriter, r *http.Request) {
	db := s.engine.DB()

	activeID, active := routingActiveProfileID(db)
	if raw := r.URL.Query().Get("profile"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			WriteError(w, http.StatusNotFound, TypeNotFound, "no such profile: "+raw)
			return
		}
		var exists int64
		if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, id).Scan(&exists); err != nil {
			WriteError(w, http.StatusNotFound, TypeNotFound, "no such profile: "+raw)
			return
		}
		activeID, active = id, true
	}

	var (
		rows []chainRow
		err  error
	)
	if active {
		rows, err = readProfileChainRows(db, activeID)
	} else {
		rows, err = readGlobalChainRows(db)
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the fallback chain")
		return
	}

	keyCounts := keyCountsByPlatform(db, true)
	penalties := s.penaltyMap()

	out := make([]fallbackEntryOut, 0, len(rows))
	for _, row := range rows {
		var priority int64
		if row.chainPos.Valid {
			priority = row.chainPos.Int64
		} else if row.catalogPos.Valid {
			priority = row.catalogPos.Int64
		}
		enabled := row.inChain
		if !active {
			// The global chain carries its own enable flag; a profile row's
			// membership IS its enabled state (profile_models has no flag).
			enabled = row.fcEnabled.Int64 != 0
		}
		out = append(out, s.buildFallbackEntry(row, priority, enabled, keyCounts, penalties))
	}
	// Re-number a profile read so members lead, then the rest - the query
	// returns catalog order, ordering is applied here.
	if active {
		out = orderProfileEntries(out, rows)
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) buildFallbackEntry(row chainRow, priority int64, enabled bool, keyCounts map[string]int, penalties map[int64]gateway.PenaltyStat) fallbackEntryOut {
	pen := penalties[row.modelDBID]
	keyCount := keyCounts[row.platform]
	tier := "free"
	switch {
	case row.source == "login":
		tier = "subscription"
	case row.paidOutPerM.Valid || row.paidInPerM.Valid:
		tier = "paid"
	}
	source := "catalog"
	if row.source == "custom" || row.keyID.Valid {
		source = "custom"
	}
	var scope *string
	if row.endpointScope != "" {
		s := row.endpointScope
		scope = &s
	}
	var keyLabel *string
	if row.keyLabel.Valid {
		l := row.keyLabel.String
		keyLabel = &l
	}
	return fallbackEntryOut{
		ModelDBID:                row.modelDBID,
		Priority:                 priority,
		EffectivePriority:        float64(priority) + pen.Penalty,
		Penalty:                  pen.Penalty,
		RateLimitHits:            pen.Count,
		Enabled:                  enabled,
		Platform:                 row.platform,
		ModelID:                  row.modelID,
		DisplayName:              row.displayName,
		IntelligenceRank:         row.intelRank,
		SpeedRank:                row.speedRank,
		SizeLabel:                row.sizeLabel,
		RPMLimit:                 nullInt64Ptr(row.rpm),
		RPDLimit:                 nullInt64Ptr(row.rpd),
		TPMLimit:                 nullInt64Ptr(row.tpm),
		TPDLimit:                 nullInt64Ptr(row.tpd),
		ContextWindow:            nullInt64Ptr(row.contextWindow),
		MonthlyTokenBudget:       row.monthlyBudget,
		MonthlyTokenBudgetTokens: parseBudgetTokens(row.monthlyBudget) * float64(max(1, keyCount)),
		SupportsVision:           row.vision,
		SupportsTools:            row.tools,
		Source:                   source,
		KeyID:                    nullInt64Ptr(row.keyID),
		KeyLabel:                 keyLabel,
		EndpointScope:            scope,
		HasOverrides:             false,
		OverrideFields:           []string{},
		KeyCount:                 keyCount,
		Tier:                     tier,
		PaidInPerM:               nullFloatPtr(row.paidInPerM),
		PaidOutPerM:              nullFloatPtr(row.paidOutPerM),
	}
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func readGlobalChainRows(db *sql.DB) ([]chainRow, error) {
	rows, err := db.Query(`
		SELECT fc.model_db_id, fc.position, fc.enabled, ` + chainSelectCols + `
		FROM fallback_config fc
		JOIN models m ON m.id = fc.model_db_id
		LEFT JOIN api_keys ak ON ak.id = m.key_id
		WHERE m.enabled = 1 AND m.available = 1
		ORDER BY fc.position ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chainRow
	for rows.Next() {
		var cr chainRow
		if err := rows.Scan(&cr.modelDBID, &cr.catalogPos, &cr.fcEnabled,
			&cr.platform, &cr.modelID, &cr.displayName, &cr.intelRank, &cr.speedRank,
			&cr.sizeLabel, &cr.rpm, &cr.rpd, &cr.tpm, &cr.tpd, &cr.contextWindow,
			&cr.monthlyBudget, &cr.vision, &cr.tools, &cr.source, &cr.keyID,
			&cr.endpointScope, &cr.keyLabel,
			&cr.paidInPerM, &cr.paidOutPerM); err != nil {
			return nil, err
		}
		cr.chainPos = cr.catalogPos
		cr.inChain = true
		out = append(out, cr)
	}
	return out, rows.Err()
}

func readProfileChainRows(db *sql.DB, profileID int64) ([]chainRow, error) {
	rows, err := db.Query(`
		SELECT m.id, pm.position AS chain_pos, fc.position AS catalog_pos,
		       (pm.model_db_id IS NOT NULL) AS in_chain, `+chainSelectCols+`
		FROM models m
		LEFT JOIN profile_models pm ON pm.profile_id = ? AND pm.model_db_id = m.id
		LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
		LEFT JOIN api_keys ak ON ak.id = m.key_id
		WHERE m.enabled = 1 AND m.available = 1`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chainRow
	for rows.Next() {
		var cr chainRow
		if err := rows.Scan(&cr.modelDBID, &cr.chainPos, &cr.catalogPos, &cr.inChain,
			&cr.platform, &cr.modelID, &cr.displayName, &cr.intelRank, &cr.speedRank,
			&cr.sizeLabel, &cr.rpm, &cr.rpd, &cr.tpm, &cr.tpd, &cr.contextWindow,
			&cr.monthlyBudget, &cr.vision, &cr.tools, &cr.source, &cr.keyID,
			&cr.endpointScope, &cr.keyLabel,
			&cr.paidInPerM, &cr.paidOutPerM); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// orderProfileEntries puts profile members first in stored order, then the rest
// in catalog order numbered after the last member, without writing anything
// back (orderProfileRows, fallback.ts:195-208).
func orderProfileEntries(entries []fallbackEntryOut, rows []chainRow) []fallbackEntryOut {
	inChain := map[int64]bool{}
	catalogPos := map[int64]int64{}
	hasCatalog := map[int64]bool{}
	for _, r := range rows {
		inChain[r.modelDBID] = r.inChain
		if r.catalogPos.Valid {
			catalogPos[r.modelDBID] = r.catalogPos.Int64
			hasCatalog[r.modelDBID] = true
		}
	}
	var members, rest []fallbackEntryOut
	for _, e := range entries {
		if inChain[e.ModelDBID] {
			members = append(members, e)
		} else {
			rest = append(rest, e)
		}
	}
	sort.SliceStable(members, func(a, b int) bool {
		if members[a].Priority != members[b].Priority {
			return members[a].Priority < members[b].Priority
		}
		return members[a].ModelDBID < members[b].ModelDBID
	})
	sort.SliceStable(rest, func(a, b int) bool {
		pa, oka := catalogPos[rest[a].ModelDBID], hasCatalog[rest[a].ModelDBID]
		pb, okb := catalogPos[rest[b].ModelDBID], hasCatalog[rest[b].ModelDBID]
		if oka != okb {
			return oka // rows with a catalog position sort before those without
		}
		if oka && pa != pb {
			return pa < pb
		}
		return rest[a].ModelDBID < rest[b].ModelDBID
	})
	maxStored := int64(0)
	if len(members) > 0 {
		maxStored = members[len(members)-1].Priority
	}
	for i := range rest {
		rest[i].Priority = maxStored + 1 + int64(i)
		rest[i].EffectivePriority = float64(rest[i].Priority) + rest[i].Penalty
		rest[i].Enabled = false
	}
	return append(members, rest...)
}

// ── fallback chain write (reorder + enable/disable) ─────────────────────────

type chainUpdateEntry struct {
	ModelDBID int64 `json:"modelDbId"`
	Priority  int64 `json:"priority"`
	Enabled   bool  `json:"enabled"`
}

// handleFallbackPut replaces the active chain. Into an active profile it upserts
// membership (a profile_models row IS an enabled member, so a disabled entry is
// removed); otherwise it updates the global fallback_config in place. Unknown
// model ids are skipped so a stale client snapshot cannot fail the write. The
// whole write is one transaction so a partial reorder never leaves two models
// sharing a position (fallback.ts:336-376).
func (s *Server) handleFallbackPut(w http.ResponseWriter, r *http.Request) {
	var entries []chainUpdateEntry
	if !DecodeJSON(w, r, &entries) {
		return
	}
	db := s.engine.DB()
	activeID, active := routingActiveProfileID(db)

	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not open a transaction")
		return
	}
	defer func() { _ = tx.Rollback() }()

	known := knownModelIDs(tx)
	if active {
		upsert, err := tx.Prepare(`
			INSERT INTO profile_models (profile_id, model_db_id, position)
			VALUES (?, ?, ?)
			ON CONFLICT(profile_id, model_db_id) DO UPDATE SET position = excluded.position`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not prepare the write")
			return
		}
		defer upsert.Close()
		del, err := tx.Prepare(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not prepare the write")
			return
		}
		defer del.Close()
		// A profile_models row IS the model's membership, so a removal must be
		// durable: record it in profile_exclusions so the Default profile's
		// boot-time auto-include cannot resurrect it, and lift that exclusion
		// when the operator explicitly re-includes the model.
		excl, err := tx.Prepare(`INSERT OR IGNORE INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not prepare the write")
			return
		}
		defer excl.Close()
		unexcl, err := tx.Prepare(`DELETE FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not prepare the write")
			return
		}
		defer unexcl.Close()
		for _, e := range entries {
			if !known[e.ModelDBID] {
				continue
			}
			if e.Enabled {
				if _, err := upsert.Exec(activeID, e.ModelDBID, e.Priority); err != nil {
					WriteError(w, http.StatusInternalServerError, TypeServer, "could not write the chain")
					return
				}
				if _, err := unexcl.Exec(activeID, e.ModelDBID); err != nil {
					WriteError(w, http.StatusInternalServerError, TypeServer, "could not write the chain")
					return
				}
			} else {
				if _, err := del.Exec(activeID, e.ModelDBID); err != nil {
					WriteError(w, http.StatusInternalServerError, TypeServer, "could not write the chain")
					return
				}
				if _, err := excl.Exec(activeID, e.ModelDBID); err != nil {
					WriteError(w, http.StatusInternalServerError, TypeServer, "could not write the chain")
					return
				}
			}
		}
	} else {
		update, err := tx.Prepare(`UPDATE fallback_config SET position = ?, enabled = ? WHERE model_db_id = ?`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not prepare the write")
			return
		}
		defer update.Close()
		for _, e := range entries {
			enabled := 0
			if e.Enabled {
				enabled = 1
			}
			if _, err := update.Exec(e.Priority, enabled, e.ModelDBID); err != nil {
				WriteError(w, http.StatusInternalServerError, TypeServer, "could not write the chain")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not commit the chain")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

func knownModelIDs(tx *sql.Tx) map[int64]bool {
	out := map[int64]bool{}
	rows, err := tx.Query(`SELECT id FROM models`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = true
		}
	}
	return out
}

// ── sort preset ─────────────────────────────────────────────────────────────

// handleFallbackSort renumbers the current chain by a preset. Unlike the
// reference, which pulls the whole catalog into the chain (this schema has no
// per-profile enable flag, so that would enable everything), it reorders only
// the models already in the chain, positionally and atomically
// (fallback.ts:416-460).
func (s *Server) handleFallbackSort(w http.ResponseWriter, r *http.Request) {
	preset := r.PathValue("preset")
	if preset != "intelligence" && preset != "speed" && preset != "budget" {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			"Unknown preset: "+preset+". Use: intelligence, speed, budget")
		return
	}
	db := s.engine.DB()
	activeID, active := routingActiveProfileID(db)

	var memberIDs map[int64]bool
	var err error
	if active {
		memberIDs, err = profileMemberSet(db, activeID)
	} else {
		memberIDs, err = fallbackMemberSet(db)
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the chain")
		return
	}
	ordered, err := modelsInPresetOrder(db, preset)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not sort the chain")
		return
	}
	var seq []int64
	for _, id := range ordered {
		if memberIDs[id] {
			seq = append(seq, id)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not open a transaction")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if active {
		stmt, _ := tx.Prepare(`UPDATE profile_models SET position = ? WHERE profile_id = ? AND model_db_id = ?`)
		defer stmt.Close()
		for i, id := range seq {
			if _, err := stmt.Exec(i+1, activeID, id); err != nil {
				WriteError(w, http.StatusInternalServerError, TypeServer, "could not sort the chain")
				return
			}
		}
	} else {
		stmt, _ := tx.Prepare(`UPDATE fallback_config SET position = ? WHERE model_db_id = ?`)
		defer stmt.Close()
		for i, id := range seq {
			if _, err := stmt.Exec(i+1, id); err != nil {
				WriteError(w, http.StatusInternalServerError, TypeServer, "could not sort the chain")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not commit the sort")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true, "preset": preset})
}

func fallbackMemberSet(db *sql.DB) (map[int64]bool, error) {
	return idSet(db, `SELECT model_db_id FROM fallback_config`)
}

func profileMemberSet(db *sql.DB, profileID int64) (map[int64]bool, error) {
	return idSet(db, `SELECT model_db_id FROM profile_models WHERE profile_id = ?`, profileID)
}

func idSet(db *sql.DB, query string, args ...any) (map[int64]bool, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// modelsInPresetOrder returns every model id in the order a preset ranks it.
// intelligence sorts on the cross-provider size tier first, then the provider's
// own rank; budget scores a spendable-tokens figure in Go (fallback.ts:383-414).
func modelsInPresetOrder(db *sql.DB, preset string) ([]int64, error) {
	if preset == "budget" {
		return modelsByBudget(db)
	}
	order := "m.speed_rank ASC"
	if preset == "intelligence" {
		order = `CASE m.size_label WHEN 'Frontier' THEN 1 WHEN 'Large' THEN 2
			WHEN 'Medium' THEN 3 WHEN 'Small' THEN 4 ELSE 5 END ASC, m.intelligence_rank ASC`
	}
	rows, err := db.Query(`SELECT m.id FROM models m ORDER BY ` + order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func modelsByBudget(db *sql.DB) ([]int64, error) {
	rows, err := db.Query(`SELECT id, monthly_token_budget, tpd_limit FROM models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		id    int64
		score float64
	}
	var list []scored
	for rows.Next() {
		var id int64
		var label string
		var tpd sql.NullInt64
		if err := rows.Scan(&id, &label, &tpd); err != nil {
			return nil, err
		}
		list = append(list, scored{id, budgetScore(label, tpd)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].score > list[b].score })
	out := make([]int64, len(list))
	for i, s := range list {
		out[i] = s.id
	}
	return out, nil
}

// budgetScore ranks a model by spendable capacity: a daily token limit implies
// ~30x monthly, otherwise the parsed monthly label wins, and "unlimited" sorts
// first (getBudgetScore, fallback.ts:393-414).
func budgetScore(label string, tpd sql.NullInt64) float64 {
	if tpd.Valid {
		return float64(tpd.Int64) * 30
	}
	lower := strings.ToLower(label)
	if strings.Contains(lower, "unlimited") || strings.Contains(label, "∞") {
		return math.Inf(1)
	}
	return parseBudgetTokens(label)
}

// ── routing strategy (read + set) ───────────────────────────────────────────

type routingScoreOut struct {
	ModelDBID     int64   `json:"modelDbId"`
	Platform      string  `json:"platform"`
	ModelID       string  `json:"modelId"`
	DisplayName   string  `json:"displayName"`
	Enabled       bool    `json:"enabled"`
	Reliability   float64 `json:"reliability"`
	Speed         float64 `json:"speed"`
	Intelligence  float64 `json:"intelligence"`
	Headroom      float64 `json:"headroom"`
	RateLimit     float64 `json:"rateLimit"`
	Score         float64 `json:"score"`
	TotalRequests int     `json:"totalRequests"`
}

type routingDataOut struct {
	Strategy             string                         `json:"strategy"`
	Weights              *weightsOut                    `json:"weights"`
	CustomWeights        weightsOut                     `json:"customWeights"`
	ExploreEnabled       bool                           `json:"exploreEnabled"`
	PeakHoursAdjust      bool                           `json:"peakHoursAdjust"`
	PeakStartHour        int                            `json:"peakStartHour"`
	PeakEndHour          int                            `json:"peakEndHour"`
	PeakTimezone         string                         `json:"peakTimezone"`
	PeakAdjusted         bool                           `json:"peakAdjusted"`
	KeySelectionStrategy string                         `json:"keySelectionStrategy"`
	CooldownCeilingMs    *int64                         `json:"cooldownCeilingMs"`
	Benchmark            gateway.BenchmarkRefreshStatus `json:"benchmark"`
	Scores               []routingScoreOut              `json:"scores"`
}

// handleRoutingGet returns the strategy, its weights, and the per-model score
// breakdown the dashboard uses to show WHY a model ranks where it does: the
// three axes and the two guardrail multipliers separately, not just the final
// number. Reliability is the stable posterior mean (Expected), never a Thompson
// draw - a dashboard figure that jitters with no cause is a lie about the data
// (getRoutingScores, router.ts:2179-2257).
func (s *Server) handleRoutingGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := s.engine.DB()
	strategy := s.routingStrategy(ctx)
	custom := routingCustomWeights(db)

	scores := s.routingScores(ctx, db, strategy, custom)

	peakAdjusted := s.peakAdjustedNow(strategy, custom)
	var weights *weightsOut
	if strategy != gateway.RoutingPriority {
		disp := routingDisplayWeights(strategy, custom)
		if peakAdjusted {
			disp, _ = gateway.PeakAdjustedWeights(disp, strategy, s.peakHoursConfig(), time.Now())
		}
		wv := weightsToOut(disp)
		weights = &wv
	}

	WriteJSON(w, http.StatusOK, routingDataOut{
		Strategy:             string(strategy),
		Weights:              weights,
		CustomWeights:        weightsToOut(custom),
		ExploreEnabled:       routingExploreEnabled(db),
		PeakHoursAdjust:      routingBoolSetting(db, settingPeakHoursAdjust, false),
		PeakStartHour:        routingIntSetting(db, settingPeakStartHour, defaultPeakStartHour),
		PeakEndHour:          routingIntSetting(db, settingPeakEndHour, defaultPeakEndHour),
		PeakTimezone:         routingStringSetting(db, settingPeakTimezone, defaultPeakTimezone),
		PeakAdjusted:         peakAdjusted,
		KeySelectionStrategy: routingKeySelection(db),
		CooldownCeilingMs:    routingCooldownCeiling(db),
		Benchmark:            s.engine.BenchmarkStatus(ctx),
		Scores:               scores,
	})
}

// routingScores scores the resolved active chain for display. It orders by
// score descending - the same ranking OrderChain's bandit branch produces, but
// computed here so the per-axis breakdown travels with each row (router.ts:2216-2233).
func (s *Server) routingScores(ctx context.Context, db *sql.DB, strategy gateway.RoutingStrategy, custom gateway.Weights) []routingScoreOut {
	resolved, err := gateway.ResolveChain(db, "auto", strategy)
	if err != nil || resolved == nil || len(resolved.Chain) == 0 {
		// An empty or refused chain (e.g. an active profile with nothing
		// enabled) is a blank table, not a 500.
		return []routingScoreOut{}
	}
	entries := resolved.Chain

	// Intelligence is normalised against the range present in THIS chain, so
	// compute the composite min/max once (router.ts:1114-1116).
	intelMin, intelMax := math.Inf(1), math.Inf(-1)
	for i := range entries {
		c := gateway.IntelligenceComposite(entries[i].Tier, entries[i].IntelRank)
		intelMin = math.Min(intelMin, c)
		intelMax = math.Max(intelMax, c)
	}

	weights := routingDisplayWeights(strategy, custom)
	if s.peakAdjustedNow(strategy, custom) {
		weights, _ = gateway.PeakAdjustedWeights(weights, strategy, s.peakHoursConfig(), time.Now())
	}
	scorer := s.newAxisScorer(ctx)

	out := make([]routingScoreOut, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		axes := scorer.Axes(e, false)
		composite := gateway.IntelligenceComposite(e.Tier, e.IntelRank)
		intel := gateway.IntelligenceScore(composite, intelMin, intelMax)
		combined := gateway.Combine(weights, axes.Reliability, axes.Speed, intel, axes.Headroom, axes.RateLimit)
		score := combined.Effective
		if e.WeightOverride != nil {
			score *= *e.WeightOverride
		}
		st := s.stats.forEntry(e)
		out = append(out, routingScoreOut{
			ModelDBID:     e.ModelDBID,
			Platform:      e.Platform,
			ModelID:       e.ModelID,
			DisplayName:   e.DisplayName,
			Enabled:       e.Enabled,
			Reliability:   axes.Reliability,
			Speed:         axes.Speed,
			Intelligence:  intel,
			Headroom:      axes.Headroom,
			RateLimit:     axes.RateLimit,
			Score:         score,
			TotalRequests: int(math.Round(st.Total())),
		})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

type routingPutBody struct {
	Strategy             string      `json:"strategy"`
	Weights              *weightsOut `json:"weights"`
	ExploreEnabled       *bool       `json:"exploreEnabled"`
	PeakHoursAdjust      *bool       `json:"peakHoursAdjust"`
	PeakStartHour        *int        `json:"peakStartHour"`
	PeakEndHour          *int        `json:"peakEndHour"`
	PeakTimezone         *string     `json:"peakTimezone"`
	KeySelectionStrategy *string     `json:"keySelectionStrategy"`
	CooldownCeilingMs    *int64      `json:"cooldownCeilingMs"`
	clearCooldownCeiling bool
}

// handleRoutingPut switches the strategy and its knobs. An unknown strategy is
// rejected rather than silently coerced to the default, so a typo cannot leave
// the operator convinced they set one thing while the router does another
// (routingSchema, fallback.ts:49-138).
func (s *Server) handleRoutingPut(w http.ResponseWriter, r *http.Request) {
	// A null cooldownCeilingMs means "no cap" and is distinct from an omitted
	// field, so decode into a raw map first to tell the two apart.
	var raw map[string]json.RawMessage
	if !DecodeJSON(w, r, &raw) {
		return
	}
	var body routingPutBody
	for k, v := range raw {
		if err := json.Unmarshal(v, fieldPtr(&body, k)); err != nil {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "invalid value for "+k)
			return
		}
	}
	if _, ok := raw["cooldownCeilingMs"]; ok && body.CooldownCeilingMs == nil {
		body.clearCooldownCeiling = true
	}

	// Validate the WHOLE body before writing anything. A routing PUT is one
	// change the operator submits as a unit, so a bad value in a later field -
	// a mistyped key-selection strategy, an out-of-range peak hour, an
	// oversized cooldown ceiling - must leave the store exactly as it was,
	// not with the strategy and weights already committed. Validation and
	// persistence are two phases; persistence is one transaction.
	plan, err := validateRoutingPut(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}

	db := s.engine.DB()
	if err := s.persistRoutingPut(r.Context(), db, plan); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not save routing settings")
		return
	}

	// Push the cooldown ceiling to the live failover driver so the advertised
	// cap binds the next bench, not the next restart. A cleared ceiling is
	// unlimited; a ceiling left out of this PUT is not touched.
	switch {
	case plan.clearCooldown:
		s.engine.SetCooldownCeiling(0)
	case plan.cooldownCeilingMs != nil:
		s.engine.SetCooldownCeiling(time.Duration(*plan.cooldownCeilingMs) * time.Millisecond)
	}

	var weights *weightsOut
	if plan.strategy != gateway.RoutingPriority {
		wv := weightsToOut(routingDisplayWeights(plan.strategy, routingCustomWeights(db)))
		weights = &wv
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"strategy":             string(plan.strategy),
		"exploreEnabled":       routingExploreEnabled(db),
		"keySelectionStrategy": routingKeySelection(db),
		"presets":              banditPresets(),
		"weights":              weights,
		"peakAdjusted":         s.peakAdjustedNow(plan.strategy, routingCustomWeights(db)),
		"peakHoursAdjust":      routingBoolSetting(db, settingPeakHoursAdjust, false),
		"peakStartHour":        routingIntSetting(db, settingPeakStartHour, defaultPeakStartHour),
		"peakEndHour":          routingIntSetting(db, settingPeakEndHour, defaultPeakEndHour),
		"peakTimezone":         routingStringSetting(db, settingPeakTimezone, defaultPeakTimezone),
		"cooldownCeilingMs":    routingCooldownCeiling(db),
	})
}

// routingPutPlan is a validated routing PUT: every field here has already
// passed its bounds check, so persistRoutingPut can write them with no chance
// of failing partway on bad input.
type routingPutPlan struct {
	strategy          gateway.RoutingStrategy
	weights           *gateway.Weights // normalised; nil when not provided
	exploreEnabled    *bool
	keySelection      *string
	clearCooldown     bool
	cooldownCeilingMs *int64
	peakHoursAdjust   *bool
	peakStartHour     *int
	peakEndHour       *int
	peakTimezone      *string
}

// validateRoutingPut checks every field of a routing PUT and returns the plan
// to persist, or the first validation error. It writes nothing, which is what
// lets the caller guarantee an invalid body mutates no setting.
func validateRoutingPut(body routingPutBody) (routingPutPlan, error) {
	strategy, ok := validRoutingStrategy(body.Strategy)
	if !ok {
		return routingPutPlan{}, errBadStrategy
	}
	plan := routingPutPlan{
		strategy:        strategy,
		exploreEnabled:  body.ExploreEnabled,
		peakHoursAdjust: body.PeakHoursAdjust,
		peakStartHour:   body.PeakStartHour,
		peakEndHour:     body.PeakEndHour,
	}
	if body.Weights != nil {
		normalized, err := normalizeCustomWeights(*body.Weights)
		if err != nil {
			return routingPutPlan{}, err
		}
		plan.weights = &normalized
	}
	if body.KeySelectionStrategy != nil {
		if *body.KeySelectionStrategy != "auto" && *body.KeySelectionStrategy != "least-remaining" {
			return routingPutPlan{}, errBadKeySelection
		}
		plan.keySelection = body.KeySelectionStrategy
	}
	switch {
	case body.clearCooldownCeiling:
		plan.clearCooldown = true
	case body.CooldownCeilingMs != nil:
		if *body.CooldownCeilingMs < minCooldownCeilingMs || *body.CooldownCeilingMs > maxCooldownCeilingMs {
			return routingPutPlan{}, errCooldownRange
		}
		plan.cooldownCeilingMs = body.CooldownCeilingMs
	}
	for _, h := range []*int{body.PeakStartHour, body.PeakEndHour} {
		if h != nil && (*h < 0 || *h > 23) {
			return routingPutPlan{}, errPeakHour
		}
	}
	if body.PeakTimezone != nil {
		if _, err := time.LoadLocation(*body.PeakTimezone); err != nil {
			return routingPutPlan{}, errPeakTimezone
		}
		plan.peakTimezone = body.PeakTimezone
	}
	return plan, nil
}

// persistRoutingPut writes a validated plan in one transaction so a routing PUT
// is all-or-nothing: a mid-write failure rolls back every field rather than
// leaving the strategy saved without its weights.
func (s *Server) persistRoutingPut(ctx context.Context, db *sql.DB, p routingPutPlan) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	set := func(key, value string) error { return routingSetSettingTx(ctx, tx, key, value) }
	if p.weights != nil {
		enc, _ := json.Marshal(weightsToOut(*p.weights))
		if err := set(settingCustomWeights, string(enc)); err != nil {
			return err
		}
	}
	if err := set(settingRoutingStrategy, string(p.strategy)); err != nil {
		return err
	}
	if p.exploreEnabled != nil {
		if err := set(settingExploreEnabled, strconv.FormatBool(*p.exploreEnabled)); err != nil {
			return err
		}
	}
	if p.keySelection != nil {
		if err := set(settingKeySelectionStrategy, *p.keySelection); err != nil {
			return err
		}
	}
	switch {
	case p.clearCooldown:
		if err := routingDeleteSettingTx(ctx, tx, settingCooldownCeilingMs); err != nil {
			return err
		}
	case p.cooldownCeilingMs != nil:
		if err := set(settingCooldownCeilingMs, strconv.FormatInt(*p.cooldownCeilingMs, 10)); err != nil {
			return err
		}
	}
	if p.peakHoursAdjust != nil {
		if err := set(settingPeakHoursAdjust, strconv.FormatBool(*p.peakHoursAdjust)); err != nil {
			return err
		}
	}
	if p.peakStartHour != nil {
		if err := set(settingPeakStartHour, strconv.Itoa(*p.peakStartHour)); err != nil {
			return err
		}
	}
	if p.peakEndHour != nil {
		if err := set(settingPeakEndHour, strconv.Itoa(*p.peakEndHour)); err != nil {
			return err
		}
	}
	if p.peakTimezone != nil {
		if err := set(settingPeakTimezone, *p.peakTimezone); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func routingSetSettingTx(ctx context.Context, ex profileExecer, key, value string) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

func routingDeleteSettingTx(ctx context.Context, ex profileExecer, key string) error {
	_, err := ex.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

// SyncRoutingRuntimeConfig pushes persisted routing configuration that a
// background subsystem holds in memory (rather than reading per request) into
// that subsystem once, at startup. Today that is the cooldown ceiling: the
// failover driver caches it, so without this a restart would forget an operator
// ceiling until the next PUT. Only a persisted value is pushed; an unset ceiling
// is left at the construction default, so nothing regresses when the operator
// never set one. Safe to call any time after the server is built.
func (s *Server) SyncRoutingRuntimeConfig(ctx context.Context) {
	_ = ctx
	if ms := routingCooldownCeiling(s.engine.DB()); ms != nil {
		s.engine.SetCooldownCeiling(time.Duration(*ms) * time.Millisecond)
	}
}

// fieldPtr maps a JSON key to the address of its field so an unknown-field
// change decodes without a second struct definition.
func fieldPtr(b *routingPutBody, key string) any {
	switch key {
	case "strategy":
		return &b.Strategy
	case "weights":
		return &b.Weights
	case "exploreEnabled":
		return &b.ExploreEnabled
	case "peakHoursAdjust":
		return &b.PeakHoursAdjust
	case "peakStartHour":
		return &b.PeakStartHour
	case "peakEndHour":
		return &b.PeakEndHour
	case "peakTimezone":
		return &b.PeakTimezone
	case "keySelectionStrategy":
		return &b.KeySelectionStrategy
	case "cooldownCeilingMs":
		return &b.CooldownCeilingMs
	default:
		return new(json.RawMessage) // tolerate unknown fields
	}
}

func validRoutingStrategy(raw string) (gateway.RoutingStrategy, bool) {
	switch s := gateway.RoutingStrategy(raw); s {
	case gateway.RoutingPriority, gateway.RoutingBalanced, gateway.RoutingSmartest,
		gateway.RoutingFastest, gateway.RoutingReliable, gateway.RoutingCustom,
		gateway.RoutingEfficient:
		return s, true
	default:
		return "", false
	}
}

// normalizeCustomWeights rejects negative or non-finite components and an
// all-zero vector, then renormalises to sum one (setCustomWeights,
// router.ts:554-568).
func normalizeCustomWeights(w weightsOut) (gateway.Weights, error) {
	for _, v := range []float64{w.Reliability, w.Speed, w.Intelligence} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return gateway.Weights{}, errBadWeights
		}
	}
	sum := w.Reliability + w.Speed + w.Intelligence
	if sum <= 0 {
		return gateway.Weights{}, errZeroWeights
	}
	return gateway.Weights{
		Reliability:  w.Reliability / sum,
		Speed:        w.Speed / sum,
		Intelligence: w.Intelligence / sum,
	}, nil
}

func banditPresets() map[string]weightsOut {
	return map[string]weightsOut{
		"balanced": weightsToOut(gateway.WeightsBalanced),
		"smartest": weightsToOut(gateway.WeightsSmartest),
		"fastest":  weightsToOut(gateway.WeightsFastest),
		"reliable": weightsToOut(gateway.WeightsReliable),
	}
}

func routingBoolSetting(db *sql.DB, key string, def bool) bool {
	if raw, ok := routingGetSetting(db, key); ok {
		if v, err := strconv.ParseBool(raw); err == nil {
			return v
		}
	}
	return def
}

func routingIntSetting(db *sql.DB, key string, def int) int {
	if raw, ok := routingGetSetting(db, key); ok {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return v
		}
	}
	return def
}

func routingStringSetting(db *sql.DB, key, def string) string {
	if raw, ok := routingGetSetting(db, key); ok && raw != "" {
		return raw
	}
	return def
}

func routingKeySelection(db *sql.DB) string {
	v := routingStringSetting(db, settingKeySelectionStrategy, "auto")
	if v != "auto" && v != "least-remaining" {
		return "auto"
	}
	return v
}

// routingExploreEnabled reports whether live routing should Thompson-sample
// (explore) rather than take the posterior mean. It defaults to true: a bandit
// that never explores never discovers an untried model, and that exploration
// variance is the whole mechanism by which a fresh candidate earns the data
// that settles its rank. A degraded fleet suppresses exploration at the routing
// layer regardless; this is only the operator's standing preference.
func routingExploreEnabled(db *sql.DB) bool {
	return routingBoolSetting(db, settingExploreEnabled, true)
}

// peakHoursConfig reads the operator's persisted peak-window settings,
// defaulted so an untouched install still describes a sane window.
func (s *Server) peakHoursConfig() gateway.PeakHoursConfig {
	db := s.engine.DB()
	return gateway.PeakHoursConfig{
		Enabled:   routingBoolSetting(db, settingPeakHoursAdjust, false),
		StartHour: routingIntSetting(db, settingPeakStartHour, defaultPeakStartHour),
		EndHour:   routingIntSetting(db, settingPeakEndHour, defaultPeakEndHour),
		Timezone:  routingStringSetting(db, settingPeakTimezone, defaultPeakTimezone),
	}
}

// peakAdjustedNow reports whether the peak-hour adjustment is currently shifting
// this strategy's weights in LIVE routing. It fires only for the weight-vector
// bandit strategies the OrderChain path scores (balanced, custom): smartest
// routes through the capability model, fastest and reliable are exempt, and
// priority and cheapest have no vector - so reporting adjusted for any of those
// would claim a shift the router never makes.
func (s *Server) peakAdjustedNow(strategy gateway.RoutingStrategy, custom gateway.Weights) bool {
	switch strategy {
	case gateway.RoutingBalanced, gateway.RoutingCustom:
	default:
		return false
	}
	_, adjusted := gateway.PeakAdjustedWeights(
		gateway.WeightsFor(strategy, custom), strategy, s.peakHoursConfig(), time.Now())
	return adjusted
}

func routingCooldownCeiling(db *sql.DB) *int64 {
	raw, ok := routingGetSetting(db, settingCooldownCeilingMs)
	if !ok {
		return nil
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

// ── token usage / monthly budget ────────────────────────────────────────────

type tokenUsageModel struct {
	ModelDBID   int64   `json:"modelDbId"`
	DisplayName string  `json:"displayName"`
	Platform    string  `json:"platform"`
	ModelID     string  `json:"modelId"`
	Budget      float64 `json:"budget"`
	Used        int64   `json:"used"`
	Enabled     bool    `json:"enabled"`
	RPMLimit    *int64  `json:"rpmLimit"`
	RPDLimit    *int64  `json:"rpdLimit"`
	TPMLimit    *int64  `json:"tpmLimit"`
	TPDLimit    *int64  `json:"tpdLimit"`
}

// handleTokenUsage reports each model's monthly token budget against what it has
// spent this calendar month. Budget pools across usable keys, and both enabled
// and disabled models contribute to the totals - failover can spend any of them
// (fallback.ts:463-552).
func (s *Server) handleTokenUsage(w http.ResponseWriter, _ *http.Request) {
	db := s.engine.DB()

	platformsWithKeys := map[string]bool{}
	if rows, err := db.Query(`SELECT DISTINCT platform FROM api_keys WHERE enabled = 1`); err == nil {
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil {
				platformsWithKeys[p] = true
			}
		}
		rows.Close()
	}

	activeID, active := routingActiveProfileID(db)
	var query string
	var args []any
	if active {
		query = `SELECT m.id, m.platform, m.model_id, m.display_name, m.monthly_token_budget, 1 AS enabled,
			m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit, m.endpoint_scope
			FROM profile_models pm JOIN models m ON m.id = pm.model_db_id
			WHERE pm.profile_id = ? AND m.enabled = 1 AND m.available = 1
			ORDER BY pm.position ASC`
		args = append(args, activeID)
	} else {
		query = `SELECT m.id, m.platform, m.model_id, m.display_name, m.monthly_token_budget, fc.enabled,
			m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit, m.endpoint_scope
			FROM fallback_config fc JOIN models m ON m.id = fc.model_db_id
			WHERE m.enabled = 1 AND m.available = 1
			ORDER BY fc.position ASC`
	}

	usage := monthlyUsageByModel(db)
	keyCounts := keyCountsByPlatform(db, true)

	rows, err := db.Query(query, args...)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read token usage")
		return
	}
	defer rows.Close()

	models := make([]tokenUsageModel, 0)
	var totalBudget float64
	var totalUsed int64
	for rows.Next() {
		var (
			id                      int64
			platform, modelID, name string
			budgetLabel             string
			enabled                 int
			endpointScope           string
			rpm, rpd, tpm, tpd      sql.NullInt64
		)
		if err := rows.Scan(&id, &platform, &modelID, &name, &budgetLabel, &enabled,
			&rpm, &rpd, &tpm, &tpd, &endpointScope); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not read token usage")
			return
		}
		if !platformsWithKeys[platform] {
			continue
		}
		budget := parseBudgetTokens(budgetLabel) * float64(max(1, keyCounts[platform]))
		// Usage is keyed by (platform, model, endpoint_scope), never by model id
		// alone: two custom relays that share a model id at different endpoints
		// are distinct rows, so collapsing them would report each one's spend as
		// the sum of both.
		used := usage[platform+"\x00"+modelID+"\x00"+endpointScope]
		models = append(models, tokenUsageModel{
			ModelDBID:   id,
			DisplayName: name,
			Platform:    platform,
			ModelID:     modelID,
			Budget:      budget,
			Used:        used,
			Enabled:     enabled != 0,
			RPMLimit:    nullInt64Ptr(rpm),
			RPDLimit:    nullInt64Ptr(rpd),
			TPMLimit:    nullInt64Ptr(tpm),
			TPDLimit:    nullInt64Ptr(tpd),
		})
		totalBudget += budget
		totalUsed += used
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"totalBudget": totalBudget,
		"totalUsed":   totalUsed,
		"models":      models,
	})
}

// monthlyUsageByModel sums this calendar month's tokens per (platform, model,
// endpoint_scope). endpoint_scope is part of the key so a custom relay's spend
// is attributed to its own endpoint, not pooled with every same-id relay
// (the isolation aggregateTrail keeps for evidence, applied here to budget).
func monthlyUsageByModel(db *sql.DB) map[string]int64 {
	out := map[string]int64{}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	rows, err := db.Query(`
		SELECT platform, model_id, endpoint_scope, COALESCE(SUM(input_tokens + output_tokens), 0)
		FROM requests
		WHERE created_at >= ?
		GROUP BY platform, model_id, endpoint_scope`, monthStart)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var platform, modelID, scope string
		var used int64
		if err := rows.Scan(&platform, &modelID, &scope, &used); err == nil {
			out[platform+"\x00"+modelID+"\x00"+scope] = used
		}
	}
	return out
}

// ── rate-limit window usage ─────────────────────────────────────────────────

type rateLimitWindowOut struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type rateLimitUsageRow struct {
	ModelDBID int64               `json:"modelDbId"`
	Platform  string              `json:"platform"`
	ModelID   string              `json:"modelId"`
	RPM       *rateLimitWindowOut `json:"rpm"`
	RPD       *rateLimitWindowOut `json:"rpd"`
	TPM       *rateLimitWindowOut `json:"tpm"`
}

type keyWindowUse struct {
	rpm, rpd, tpm int64
}

// handleRateLimitUsage reports each model's RPM/RPD/TPM used-vs-limit for the
// dashboard's remaining-quota badge (#876). It follows the key the router would
// pick NEXT - the eligible key with the most headroom - so a model with one
// exhausted key and one idle key does not paint red (fallback.ts:598-702).
//
// A window with no published limit serialises as null, never {used:0,limit:0}:
// the client renders "-" for null and a real number for a limit, so a zero
// would misreport an unmetered axis as fully spent.
func (s *Server) handleRateLimitUsage(w http.ResponseWriter, r *http.Request) {
	db := s.engine.DB()
	now := time.Now()
	minuteStart := now.Unix() - now.Unix()%60
	dayCutoff := now.Unix() - now.Unix()%3600 - 24*3600

	usage := windowUsageByQuotaKey(db, minuteStart, dayCutoff)
	keys, _ := s.engine.Vault().List(r.Context())

	rows, err := db.Query(`
		SELECT id, platform, model_id, key_id, rpm_limit, rpd_limit, tpm_limit
		FROM models WHERE enabled = 1 AND available = 1`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read rate-limit usage")
		return
	}
	defer rows.Close()

	out := make([]rateLimitUsageRow, 0)
	for rows.Next() {
		var (
			id                int64
			platform, modelID string
			modelKeyID        sql.NullInt64
			rpmLim, rpdLim    sql.NullInt64
			tpmLim            sql.NullInt64
		)
		if err := rows.Scan(&id, &platform, &modelID, &modelKeyID, &rpmLim, &rpdLim, &tpmLim); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not read rate-limit usage")
			return
		}

		best, ok := bestKeyUsage(platform, modelID, modelKeyID, keys, usage,
			rpmLim, rpdLim, tpmLim)
		row := rateLimitUsageRow{ModelDBID: id, Platform: platform, ModelID: modelID}
		if ok {
			if rpmLim.Valid {
				row.RPM = &rateLimitWindowOut{Used: best.rpm, Limit: rpmLim.Int64}
			}
			if rpdLim.Valid {
				row.RPD = &rateLimitWindowOut{Used: best.rpd, Limit: rpdLim.Int64}
			}
			if tpmLim.Valid {
				row.TPM = &rateLimitWindowOut{Used: best.tpm, Limit: tpmLim.Int64}
			}
		}
		out = append(out, row)
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"generatedAtMs": now.UnixMilli(),
		"rows":          out,
	})
}

// bestKeyUsage picks the eligible key with the least window pressure and returns
// its usage, mirroring the router's key eligibility (enabled + healthy/unknown,
// scope allows the model, and for a custom model the key must be the model's own
// endpoint). No eligible key means the model cannot be served, so a usage number
// would be fiction - the caller reports no windows.
func bestKeyUsage(platform, modelID string, modelKeyID sql.NullInt64, keys []gateway.KeyRow,
	usage map[string]keyWindowUse, rpmLim, rpdLim, tpmLim sql.NullInt64) (keyWindowUse, bool) {
	var best keyWindowUse
	found := false
	bestPressure := math.Inf(1)
	for _, k := range keys {
		if k.Platform != platform || !k.Enabled {
			continue
		}
		if k.Status != gateway.StatusHealthy && k.Status != gateway.StatusUnknown {
			continue
		}
		if !scopeAllows(k.ModelScope, modelID) {
			continue
		}
		if platform == "custom" && modelKeyID.Valid && k.ID != modelKeyID.Int64 {
			continue
		}
		use := usage[gateway.QuotaKey(platform, modelID, k.ID)]
		pressure := windowPressure(use, rpmLim, rpdLim, tpmLim)
		if pressure < bestPressure {
			bestPressure = pressure
			best = use
			found = true
		}
	}
	return best, found
}

func scopeAllows(scope []string, modelID string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, m := range scope {
		if m == modelID {
			return true
		}
	}
	return false
}

func windowPressure(use keyWindowUse, rpmLim, rpdLim, tpmLim sql.NullInt64) float64 {
	worst := 0.0
	for _, wr := range []struct {
		used  int64
		limit sql.NullInt64
	}{{use.rpm, rpmLim}, {use.rpd, rpdLim}, {use.tpm, tpmLim}} {
		if !wr.limit.Valid || wr.limit.Int64 <= 0 {
			continue
		}
		if r := float64(wr.used) / float64(wr.limit.Int64); r > worst {
			worst = r
		}
	}
	return worst
}

// windowUsageByQuotaKey folds the persisted counters into one usage figure per
// (platform, model, key): RPM/TPM from the current minute bucket and RPD as the
// trailing-24h sum of hourly buckets, the same windows the ledger enforces
// (quota.go liveCounts/modelDaySum).
func windowUsageByQuotaKey(db *sql.DB, minuteStart, dayCutoff int64) map[string]keyWindowUse {
	out := map[string]keyWindowUse{}
	rows, err := db.Query(`
		SELECT quota_key,
		       SUM(CASE WHEN window_kind = 'minute' AND window_start = ? THEN requests ELSE 0 END),
		       SUM(CASE WHEN window_kind = 'minute' AND window_start = ? THEN tokens ELSE 0 END),
		       SUM(CASE WHEN window_kind = 'hour' AND window_start > ? THEN requests ELSE 0 END)
		FROM rate_limit_usage
		WHERE (window_kind = 'minute' AND window_start = ?) OR (window_kind = 'hour' AND window_start > ?)
		GROUP BY quota_key`,
		minuteStart, minuteStart, dayCutoff, minuteStart, dayCutoff)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var qk string
		var rpm, tpm, rpd int64
		if err := rows.Scan(&qk, &rpm, &tpm, &rpd); err == nil {
			out[qk] = keyWindowUse{rpm: rpm, rpd: rpd, tpm: tpm}
		}
	}
	return out
}

// ── penalty inspector ───────────────────────────────────────────────────────

type inspectorPenalty struct {
	Hits            int     `json:"hits"`
	Value           float64 `json:"value"`
	RateLimitFactor float64 `json:"rateLimitFactor"`
}

type inspectorCooldown struct {
	KeyID       int64   `json:"keyId"`
	KeyLabel    *string `json:"keyLabel"`
	KeyStatus   *string `json:"keyStatus"`
	ExpiresAtMs int64   `json:"expiresAtMs"`
	ExpiresInMs int64   `json:"expiresInMs"`
}

type inspectorError struct {
	ID        int64   `json:"id"`
	KeyID     *int64  `json:"keyId"`
	KeyLabel  *string `json:"keyLabel"`
	Error     string  `json:"error"`
	LatencyMs int64   `json:"latencyMs"`
	CreatedAt string  `json:"createdAt"`
}

type inspectorRow struct {
	ModelDBID        *int64              `json:"modelDbId"`
	Platform         string              `json:"platform"`
	ModelID          string              `json:"modelId"`
	DisplayName      string              `json:"displayName"`
	Enabled          bool                `json:"enabled"`
	FallbackEnabled  bool                `json:"fallbackEnabled"`
	Priority         *int64              `json:"priority"`
	Penalty          inspectorPenalty    `json:"penalty"`
	Cooldowns        []inspectorCooldown `json:"cooldowns"`
	RecentErrors     []inspectorError    `json:"recentErrors"`
	RecentErrorCount int                 `json:"recentErrorCount"`
	Reasons          []string            `json:"reasons"`
}

const (
	inspectorLookbackMinutes = 30
	maxErrorsPerModel        = 5
)

// handlePenaltyInspectorGet exposes the live router pressure: per-model score
// penalties, active cooldowns, and recent upstream errors, so an operator can
// see why a pool is not routing (getPenaltyInspector, penalty-inspector.ts:121-272).
func (s *Server) handlePenaltyInspectorGet(w http.ResponseWriter, _ *http.Request) {
	db := s.engine.DB()
	now := time.Now()
	rows := map[string]*inspectorRow{}

	for _, p := range s.engine.Penalties().Snapshot() {
		var (
			platform, modelID, displayName string
			modelEnabled                   int
			fcEnabled                      sql.NullInt64
			priority                       sql.NullInt64
		)
		err := db.QueryRow(`
			SELECT m.platform, m.model_id, m.display_name, m.enabled, fc.enabled, fc.position
			FROM models m LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
			WHERE m.id = ?`, p.ModelDBID).Scan(&platform, &modelID, &displayName,
			&modelEnabled, &fcEnabled, &priority)
		if err != nil {
			continue
		}
		id := p.ModelDBID
		row := ensureInspectorRow(rows, &id, platform, modelID, displayName, modelEnabled, fcEnabled, priority)
		row.Penalty = inspectorPenalty{
			Hits:            p.Count,
			Value:           p.Penalty,
			RateLimitFactor: rateLimitFactorFor(p.Penalty),
		}
		addReason(row, "penalty")
	}

	for _, cd := range activeCooldowns(db, now) {
		row := ensureInspectorRow(rows, cd.modelDBID, cd.platform, cd.modelID, cd.displayName,
			cd.modelEnabled, cd.fcEnabled, cd.priority)
		row.Cooldowns = append(row.Cooldowns, inspectorCooldown{
			KeyID:       cd.keyID,
			KeyLabel:    cd.keyLabel,
			KeyStatus:   cd.keyStatus,
			ExpiresAtMs: cd.expiresAtMs,
			ExpiresInMs: max(0, cd.expiresAtMs-now.UnixMilli()),
		})
		addReason(row, "cooldown")
	}

	for _, e := range recentErrorRows(db, now.Add(-inspectorLookbackMinutes*time.Minute).Unix()) {
		row := ensureInspectorRow(rows, e.modelDBID, e.platform, e.modelID, e.displayName,
			e.modelEnabled, e.fcEnabled, e.priority)
		row.RecentErrorCount++
		if len(row.RecentErrors) < maxErrorsPerModel {
			row.RecentErrors = append(row.RecentErrors, inspectorError{
				ID:        e.id,
				KeyID:     e.keyID,
				KeyLabel:  e.keyLabel,
				Error:     e.message,
				LatencyMs: e.latencyMs,
				CreatedAt: time.Unix(e.createdAt, 0).UTC().Format(time.RFC3339),
			})
		}
		addReason(row, "recent_errors")
	}

	ordered := make([]*inspectorRow, 0, len(rows))
	for _, r := range rows {
		ordered = append(ordered, r)
	}
	sort.SliceStable(ordered, func(a, b int) bool {
		if ordered[a].Penalty.Value != ordered[b].Penalty.Value {
			return ordered[a].Penalty.Value > ordered[b].Penalty.Value
		}
		if len(ordered[a].Cooldowns) != len(ordered[b].Cooldowns) {
			return len(ordered[a].Cooldowns) > len(ordered[b].Cooldowns)
		}
		if ordered[a].RecentErrorCount != ordered[b].RecentErrorCount {
			return ordered[a].RecentErrorCount > ordered[b].RecentErrorCount
		}
		return ordered[a].DisplayName < ordered[b].DisplayName
	})

	WriteJSON(w, http.StatusOK, map[string]any{
		"generatedAtMs":   now.UnixMilli(),
		"lookbackMinutes": inspectorLookbackMinutes,
		"rows":            ordered,
	})
}

func ensureInspectorRow(rows map[string]*inspectorRow, modelDBID *int64, platform, modelID, displayName string,
	modelEnabled int, fcEnabled, priority sql.NullInt64) *inspectorRow {
	key := "model:" + platform + ":" + modelID
	if modelDBID != nil {
		key = "id:" + strconv.FormatInt(*modelDBID, 10)
	}
	if existing, ok := rows[key]; ok {
		return existing
	}
	name := displayName
	if name == "" {
		name = modelID
	}
	row := &inspectorRow{
		ModelDBID:       modelDBID,
		Platform:        platform,
		ModelID:         modelID,
		DisplayName:     name,
		Enabled:         modelEnabled != 0,
		FallbackEnabled: !fcEnabled.Valid || fcEnabled.Int64 != 0,
		Priority:        nullInt64Ptr(priority),
		Penalty:         inspectorPenalty{Hits: 0, Value: 0, RateLimitFactor: 1},
		Cooldowns:       []inspectorCooldown{},
		RecentErrors:    []inspectorError{},
		Reasons:         []string{},
	}
	rows[key] = row
	return row
}

func addReason(row *inspectorRow, reason string) {
	for _, r := range row.Reasons {
		if r == reason {
			return
		}
	}
	row.Reasons = append(row.Reasons, reason)
}

// rateLimitFactorFor is the multiplicative guardrail a demotion imposes, capped
// so even a maxed-out model keeps 40% of its score (rateLimitFactor,
// scoring.ts:441-449; MAX_PENALTY=10, RATE_LIMIT_MAX_DAMP=0.6).
func rateLimitFactorFor(penalty float64) float64 {
	p := math.Max(0, math.Min(penalty, 10))
	return 1 - (p/10)*0.6
}

type cooldownRow struct {
	modelDBID    *int64
	platform     string
	modelID      string
	displayName  string
	modelEnabled int
	fcEnabled    sql.NullInt64
	priority     sql.NullInt64
	keyID        int64
	keyLabel     *string
	keyStatus    *string
	expiresAtMs  int64
}

// activeCooldowns reads the persisted benches. The quota_key encodes
// platform/model/key with the model id in the middle (model ids contain
// slashes), so it is split from both ends.
func activeCooldowns(db *sql.DB, now time.Time) []cooldownRow {
	rows, err := db.Query(`SELECT quota_key, until FROM rate_limit_cooldowns WHERE until > ? ORDER BY until ASC`,
		now.Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []cooldownRow
	for rows.Next() {
		var qk string
		var until int64
		if err := rows.Scan(&qk, &until); err != nil {
			continue
		}
		platform, modelID, keyID, ok := splitQuotaKey(qk)
		if !ok {
			continue
		}
		cr := cooldownRow{
			platform:    platform,
			modelID:     modelID,
			keyID:       keyID,
			expiresAtMs: until * 1000,
			displayName: modelID,
		}
		var (
			mID          int64
			name         string
			modelEnabled sql.NullInt64
		)
		if err := db.QueryRow(`
			SELECT m.id, m.display_name, m.enabled, fc.enabled, fc.position
			FROM models m LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
			WHERE m.platform = ? AND m.model_id = ?`, platform, modelID).Scan(
			&mID, &name, &modelEnabled, &cr.fcEnabled, &cr.priority); err == nil {
			cr.modelDBID = &mID
			cr.displayName = name
			if modelEnabled.Valid {
				cr.modelEnabled = int(modelEnabled.Int64)
			}
		}
		var label, status sql.NullString
		if err := db.QueryRow(`SELECT label, status FROM api_keys WHERE id = ?`, keyID).Scan(&label, &status); err == nil {
			if label.Valid {
				cr.keyLabel = &label.String
			}
			if status.Valid {
				cr.keyStatus = &status.String
			}
		}
		out = append(out, cr)
	}
	return out
}

func splitQuotaKey(qk string) (platform, modelID string, keyID int64, ok bool) {
	first := strings.IndexByte(qk, '/')
	last := strings.LastIndexByte(qk, '/')
	if first < 0 || last <= first {
		return "", "", 0, false
	}
	platform = qk[:first]
	modelID = qk[first+1 : last]
	id, err := strconv.ParseInt(qk[last+1:], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return platform, modelID, id, true
}

type errorRow struct {
	id           int64
	modelDBID    *int64
	platform     string
	modelID      string
	displayName  string
	modelEnabled int
	fcEnabled    sql.NullInt64
	priority     sql.NullInt64
	keyID        *int64
	keyLabel     *string
	message      string
	latencyMs    int64
	createdAt    int64
}

// recentErrorRows lists failed attempts in the lookback window. A failure is any
// terminal outcome that is neither a success nor a client cancel, matching the
// scorer's own notion of a reliability miss (scorer.go aggregateTrail).
func recentErrorRows(db *sql.DB, since int64) []errorRow {
	rows, err := db.Query(`
		SELECT r.id, r.platform, r.model_id, r.key_id,
		       COALESCE(r.error_message, r.error_kind, 'Unknown upstream error'),
		       r.latency_ms, r.created_at,
		       m.id, m.display_name, m.enabled, fc.enabled, fc.position, ak.label
		FROM requests r
		LEFT JOIN models m ON m.platform = r.platform AND m.model_id = r.model_id
		LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
		LEFT JOIN api_keys ak ON ak.id = r.key_id
		WHERE r.outcome NOT IN ('success', 'canceled') AND r.created_at >= ?
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT 250`, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []errorRow
	for rows.Next() {
		var (
			er           errorRow
			keyID        sql.NullInt64
			mID          sql.NullInt64
			name         sql.NullString
			modelEnabled sql.NullInt64
			label        sql.NullString
		)
		if err := rows.Scan(&er.id, &er.platform, &er.modelID, &keyID, &er.message,
			&er.latencyMs, &er.createdAt, &mID, &name, &modelEnabled, &er.fcEnabled,
			&er.priority, &label); err != nil {
			continue
		}
		er.keyID = nullInt64Ptr(keyID)
		if mID.Valid {
			v := mID.Int64
			er.modelDBID = &v
		}
		if name.Valid {
			er.displayName = name.String
		}
		if modelEnabled.Valid {
			er.modelEnabled = int(modelEnabled.Int64)
		}
		if label.Valid {
			er.keyLabel = &label.String
		}
		out = append(out, er)
	}
	return out
}

// handlePenaltyInspectorClear lifts all router pressure: score penalties (via
// the penalty store) and the persisted cooldown benches. It reports how many of
// each were cleared (clearRouterPressure, penalty-inspector.ts:113-119). The
// model-failure-window streak has no store exposed to this layer, so its count
// is zero.
func (s *Server) handlePenaltyInspectorClear(w http.ResponseWriter, _ *http.Request) {
	db := s.engine.DB()
	var cooldowns int64
	if res, err := db.Exec(`DELETE FROM rate_limit_cooldowns`); err == nil {
		cooldowns, _ = res.RowsAffected()
	}
	penalties := s.engine.Penalties().Clear()
	WriteJSON(w, http.StatusOK, map[string]any{
		"cooldowns":      cooldowns,
		"penalties":      penalties,
		"failureWindows": 0,
	})
}

// ── profiles ────────────────────────────────────────────────────────────────

type profileOut struct {
	ID                   int64   `json:"id"`
	Name                 string  `json:"name"`
	Strategy             string  `json:"strategy"`
	Emoji                string  `json:"emoji"`
	Color                string  `json:"color"`
	Type                 string  `json:"type"`
	IsFavorite           int     `json:"is_favorite"`
	SortOrder            int64   `json:"sort_order"`
	AutoSort             *string `json:"auto_sort"`
	LayoutConfig         *string `json:"layout_config"`
	AutoIncludeNewModels int     `json:"auto_include_new_models"`
	ModelCount           int     `json:"modelCount"`
	CreatedAt            string  `json:"created_at"`
}

// profileToOut adapts this schema's minimal profile row (id, name, strategy,
// created_at) to the shape the client renders, defaulting the cosmetic fields
// the dashboard shows but this backend does not store. strategy is the raw
// stored value ("" when the set inherits the operator's default).
func profileToOut(id int64, name string, createdAt int64, strategy string) profileOut {
	typ := "custom"
	if strings.EqualFold(name, "default") {
		typ = "default"
	}
	return profileOut{
		ID:                   id,
		Name:                 name,
		Strategy:             strategy,
		Emoji:                "",
		Color:                "#6366f1",
		Type:                 typ,
		IsFavorite:           0,
		SortOrder:            id,
		AutoSort:             nil,
		LayoutConfig:         nil,
		AutoIncludeNewModels: 1,
		CreatedAt:            sqliteDateTime(createdAt),
	}
}

func (s *Server) handleProfilesList(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.engine.DB().Query(`
		SELECT p.id, p.name, p.strategy, p.created_at,
		       (SELECT COUNT(*) FROM profile_models pm WHERE pm.profile_id = p.id) AS model_count
		FROM profiles p
		ORDER BY (CASE WHEN LOWER(p.name) = 'default' THEN 1 ELSE 0 END) DESC, p.id ASC`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not list profiles")
		return
	}
	defer rows.Close()
	out := make([]profileOut, 0)
	for rows.Next() {
		var id, createdAt int64
		var modelCount int
		var name, strategy string
		if err := rows.Scan(&id, &name, &strategy, &createdAt, &modelCount); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not list profiles")
			return
		}
		po := profileToOut(id, name, createdAt, strategy)
		po.ModelCount = modelCount
		out = append(out, po)
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleProfileActiveGet(w http.ResponseWriter, _ *http.Request) {
	id, active := routingActiveProfileID(s.engine.DB())
	if !active {
		WriteJSON(w, http.StatusOK, map[string]any{"activeProfileId": nil})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"activeProfileId": id})
}

type activeProfileBody struct {
	ProfileID *int64 `json:"profileId"`
}

// handleProfileActiveSet sets or clears the active profile. Setting a new one
// replaces the previous id, so activating B deactivates A: there is exactly one
// active profile, and it is the one the engine's ResolveChain reads
// (profiles.ts:84-105).
func (s *Server) handleProfileActiveSet(w http.ResponseWriter, r *http.Request) {
	var body activeProfileBody
	if !DecodeJSON(w, r, &body) {
		return
	}
	db := s.engine.DB()
	if body.ProfileID == nil {
		_ = routingDeleteSetting(db, settingActiveProfileID)
		_, _ = db.Exec(`UPDATE profiles SET active = 0`)
		WriteJSON(w, http.StatusOK, map[string]any{"activeProfileId": nil})
		return
	}
	var exists int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, *body.ProfileID).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Profile not found")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not activate the profile")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if err := routingActivateProfile(r.Context(), tx, *body.ProfileID); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not activate the profile")
		return
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not activate the profile")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"activeProfileId": *body.ProfileID})
}

type profileModelOut struct {
	ModelDBID        int64  `json:"model_db_id"`
	Priority         int64  `json:"priority"`
	Enabled          bool   `json:"enabled"`
	Platform         string `json:"platform"`
	ModelID          string `json:"model_id"`
	DisplayName      string `json:"display_name"`
	IntelligenceRank int    `json:"intelligence_rank"`
	SpeedRank        int    `json:"speed_rank"`
	SizeLabel        string `json:"size_label"`
	RPMLimit         *int64 `json:"rpm_limit"`
	RPDLimit         *int64 `json:"rpd_limit"`
	TPMLimit         *int64 `json:"tpm_limit"`
	TPDLimit         *int64 `json:"tpd_limit"`
	MonthlyBudget    string `json:"monthly_token_budget"`
}

func (s *Server) handleProfileModels(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	db := s.engine.DB()
	var exists int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Profile not found")
		return
	}
	rows, err := db.Query(`
		SELECT pm.model_db_id, pm.position, m.enabled, m.platform, m.model_id, m.display_name,
		       m.intelligence_rank, m.speed_rank, m.size_label,
		       m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit, m.monthly_token_budget
		FROM profile_models pm JOIN models m ON m.id = pm.model_db_id
		WHERE pm.profile_id = ?
		ORDER BY pm.position ASC`, id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read profile models")
		return
	}
	defer rows.Close()
	out := make([]profileModelOut, 0)
	for rows.Next() {
		var m profileModelOut
		var enabled int
		var rpm, rpd, tpm, tpd sql.NullInt64
		if err := rows.Scan(&m.ModelDBID, &m.Priority, &enabled, &m.Platform, &m.ModelID, &m.DisplayName,
			&m.IntelligenceRank, &m.SpeedRank, &m.SizeLabel, &rpm, &rpd, &tpm, &tpd, &m.MonthlyBudget); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not read profile models")
			return
		}
		// Every row here IS a member; report the model's true catalogue-wide
		// enabled state so a globally disabled member stays visible (and is
		// preserved across a reorder) rather than silently vanishing.
		m.Enabled = enabled != 0
		m.RPMLimit, m.RPDLimit, m.TPMLimit, m.TPDLimit = nullInt64Ptr(rpm), nullInt64Ptr(rpd), nullInt64Ptr(tpm), nullInt64Ptr(tpd)
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, out)
}

var profileNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var reservedProfileNames = map[string]bool{
	"auto": true, "smart": true, "fast": true, "cheap": true, "budget": true,
	"intelligence": true, "speed": true, "active": true, "default": true,
}

type createProfileBody struct {
	Name            string `json:"name"`
	SourceProfileID *int64 `json:"sourceProfileId"`
	Empty           bool   `json:"empty"`
	// Strategy is the set's own routing order, optional. Empty means the set
	// inherits the operator's default; any other value must name a real
	// strategy or the create is rejected, so a set can never store a value the
	// engine would silently coerce.
	Strategy string `json:"strategy"`
}

// handleProfileCreate creates a named chain. An empty request wins over a
// source copy (its whole point is to start with nothing), otherwise the source
// profile's membership - or the global fallback baseline - seeds it
// (profiles.ts:138-203).
func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	var body createProfileBody
	if !DecodeJSON(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 20 || !profileNameRE.MatchString(name) {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			"Only Latin letters, digits, hyphens (-) and underscores (_) are allowed, up to 20 characters")
		return
	}
	if reservedProfileNames[strings.ToLower(name)] {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "This name is reserved by the system")
		return
	}
	strategy := ""
	if body.Strategy != "" {
		valid, ok := validRoutingStrategy(body.Strategy)
		if !ok {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "unknown routing strategy")
			return
		}
		strategy = string(valid)
	}
	db := s.engine.DB()
	var dup int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE LOWER(name) = LOWER(?)`, name).Scan(&dup); err == nil {
		WriteError(w, http.StatusConflict, TypeInvalidRequest, "Profile with name '"+name+"' already exists")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not create the profile")
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO profiles (name, strategy, active, created_at) VALUES (?, ?, 0, ?)`, name, strategy, time.Now().Unix())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not create the profile")
		return
	}
	profileID, _ := res.LastInsertId()

	if !body.Empty {
		if body.SourceProfileID != nil {
			var srcExists int64
			if err := tx.QueryRow(`SELECT id FROM profiles WHERE id = ?`, *body.SourceProfileID).Scan(&srcExists); err == nil {
				_, err = tx.Exec(`INSERT INTO profile_models (profile_id, model_db_id, position)
					SELECT ?, model_db_id, position FROM profile_models WHERE profile_id = ?`,
					profileID, *body.SourceProfileID)
			} else {
				err = seedFromFallback(tx, profileID)
			}
		} else {
			err = seedFromFallback(tx, profileID)
		}
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not seed the profile")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not create the profile")
		return
	}

	var createdAt int64
	_ = db.QueryRow(`SELECT created_at FROM profiles WHERE id = ?`, profileID).Scan(&createdAt)
	WriteJSON(w, http.StatusCreated, profileToOut(profileID, name, createdAt, strategy))
}

// seedFromFallback copies the global chain's enabled members into a new profile
// as its starting membership (copyFromDefault, profiles.ts:196-203).
func seedFromFallback(tx *sql.Tx, profileID int64) error {
	_, err := tx.Exec(`
		INSERT INTO profile_models (profile_id, model_db_id, position)
		SELECT ?, model_db_id, position FROM fallback_config WHERE enabled = 1 ORDER BY position ASC`,
		profileID)
	return err
}

type reorderEntry struct {
	ModelDBID int64 `json:"modelDbId"`
	Priority  int64 `json:"priority"`
	Enabled   bool  `json:"enabled"`
}

// handleProfileReorder replaces a profile's membership in one transaction. It
// numbers positions densely by request order, so a reorder can never leave two
// models sharing a position, and it touches only profile_models - models.id,
// which fallback_config and profile_models both address, is never renumbered
// (profiles.ts:270-295).
func (s *Server) handleProfileReorder(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	db := s.engine.DB()
	var exists int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Profile not found")
		return
	}
	var entries []reorderEntry
	if !DecodeJSON(w, r, &entries) {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	defer func() { _ = tx.Rollback() }()
	// Capture membership before the wholesale replace so the exclusion ledger
	// can be reconciled below. Read and close before any write: within one
	// transaction a held cursor across an insert deadlocks.
	before := map[int64]bool{}
	brows, err := tx.Query(`SELECT model_db_id FROM profile_models WHERE profile_id = ?`, id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	for brows.Next() {
		var mid int64
		if err := brows.Scan(&mid); err != nil {
			_ = brows.Close()
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
			return
		}
		before[mid] = true
	}
	if err := brows.Err(); err != nil {
		_ = brows.Close()
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	_ = brows.Close()
	if _, err := tx.Exec(`DELETE FROM profile_models WHERE profile_id = ?`, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	insert, err := tx.Prepare(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, ?)`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	defer insert.Close()
	pos := 0
	now := map[int64]bool{}
	for _, e := range entries {
		if !e.Enabled {
			continue // a disabled entry is simply not a member in this schema
		}
		pos++
		if _, err := insert.Exec(id, e.ModelDBID, pos); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
			return
		}
		now[e.ModelDBID] = true
	}
	// Membership implies the model is a routing candidate: selecting a model
	// into a set makes it usable, so a globally-disabled (⊘) model is enabled
	// here in the same transaction rather than left inert. This honours the
	// reorder payload, which marks every member enabled, and keeps a bulk
	// select fast (one transaction) instead of a per-model round trip.
	if len(now) > 0 {
		enable, err := tx.Prepare(`UPDATE models SET enabled = 1 WHERE id = ?`)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
			return
		}
		defer enable.Close()
		for mid := range now {
			if _, err := enable.Exec(mid); err != nil {
				WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
				return
			}
		}
	}
	// Reconcile the exclusion ledger against the replacement membership: a model
	// dropped by the reorder is a durable removal, a model kept or added lifts
	// any prior exclusion. Only the Default profile is auto-included, but keeping
	// the ledger uniform across profiles makes the invariant hold everywhere.
	excl, err := tx.Prepare(`INSERT OR IGNORE INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	defer excl.Close()
	unexcl, err := tx.Prepare(`DELETE FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
		return
	}
	defer unexcl.Close()
	for mid := range now {
		if _, err := unexcl.Exec(id, mid); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
			return
		}
	}
	for mid := range before {
		if now[mid] {
			continue
		}
		if _, err := excl.Exec(id, mid); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not reorder the profile")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not commit the reorder")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

type profilePatchBody struct {
	// Pointers so an absent field is distinguishable from a cleared one: a
	// PATCH that names only one field must leave the other exactly as stored,
	// and "strategy":"" must be able to clear a set back to inheriting.
	Name     *string `json:"name"`
	Strategy *string `json:"strategy"`
}

// handleProfileRename applies a partial update to a set. When Name is present it
// obeys the same rules as create - the shared regex, length and reserved-name
// checks, and a refusal of a name another set already holds - except that
// renaming a set to its own current name stays a no-op that never trips the
// reserved-name guard. When Strategy is present, "" clears it back to inheriting
// the operator's default and any other value must name a real strategy. Both
// fields are written in one UPDATE, and a body with neither field returns the
// current row unchanged.
func (s *Server) handleProfileRename(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	var body profilePatchBody
	if !DecodeJSON(w, r, &body) {
		return
	}
	db := s.engine.DB()
	var current, currentStrategy string
	var createdAt int64
	if err := db.QueryRow(`SELECT name, strategy, created_at FROM profiles WHERE id = ?`, id).Scan(&current, &currentStrategy, &createdAt); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Profile not found")
		return
	}

	newName := current
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name != current {
			if name == "" || len(name) > 20 || !profileNameRE.MatchString(name) {
				WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
					"Only Latin letters, digits, hyphens (-) and underscores (_) are allowed, up to 20 characters")
				return
			}
			if reservedProfileNames[strings.ToLower(name)] {
				WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "This name is reserved by the system")
				return
			}
			var dup int64
			if err := db.QueryRow(`SELECT id FROM profiles WHERE LOWER(name) = LOWER(?) AND id != ?`, name, id).Scan(&dup); err == nil {
				WriteError(w, http.StatusConflict, TypeInvalidRequest, "Profile with name '"+name+"' already exists")
				return
			}
		}
		newName = name
	}

	newStrategy := currentStrategy
	if body.Strategy != nil {
		if *body.Strategy == "" {
			newStrategy = ""
		} else {
			valid, ok := validRoutingStrategy(*body.Strategy)
			if !ok {
				WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "unknown routing strategy")
				return
			}
			newStrategy = string(valid)
		}
	}

	if _, err := db.Exec(`UPDATE profiles SET name = ?, strategy = ? WHERE id = ?`, newName, newStrategy, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the profile")
		return
	}
	po := profileToOut(id, newName, createdAt, newStrategy)
	po.ModelCount = profileMemberCount(db, id)
	WriteJSON(w, http.StatusOK, po)
}

func profileMemberCount(db *sql.DB, profileID int64) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM profile_models WHERE profile_id = ?`, profileID).Scan(&n)
	return n
}

// handleProfileDelete removes a profile. The last one cannot be deleted, and
// deleting the active one clears the active pointer so the gateway falls back to
// the global chain rather than routing over a dangling id (profiles.ts:337-370).
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	db := s.engine.DB()
	var exists int64
	if err := db.QueryRow(`SELECT id FROM profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Profile not found")
		return
	}
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&count)
	if count <= 1 {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Cannot delete the last profile")
		return
	}

	activeID, active := routingActiveProfileID(db)
	if _, err := db.Exec(`DELETE FROM profiles WHERE id = ?`, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the profile")
		return
	}
	if active && activeID == id {
		_ = routingDeleteSetting(db, settingActiveProfileID)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ── models ──────────────────────────────────────────────────────────────────

type modelListOut struct {
	ID               int64    `json:"id"`
	Platform         string   `json:"platform"`
	ModelID          string   `json:"modelId"`
	DisplayName      string   `json:"displayName"`
	IntelligenceRank int      `json:"intelligenceRank"`
	SpeedRank        int      `json:"speedRank"`
	SizeLabel        string   `json:"sizeLabel"`
	RPMLimit         *int64   `json:"rpmLimit"`
	RPDLimit         *int64   `json:"rpdLimit"`
	TPMLimit         *int64   `json:"tpmLimit"`
	TPDLimit         *int64   `json:"tpdLimit"`
	MonthlyBudget    string   `json:"monthlyTokenBudget"`
	ContextWindow    *int64   `json:"contextWindow"`
	Enabled          bool     `json:"enabled"`
	SupportsVision   bool     `json:"supportsVision"`
	SupportsTools    bool     `json:"supportsTools"`
	Priority         *int64   `json:"priority"`
	FallbackEnabled  bool     `json:"fallbackEnabled"`
	Source           string   `json:"source"`
	KeyID            *int64   `json:"keyId"`
	KeyLabel         *string  `json:"keyLabel"`
	EndpointScope    *string  `json:"endpointScope"`
	QualifiedModelID *string  `json:"qualifiedModelId"`
	HasOverrides     bool     `json:"hasOverrides"`
	OverrideFields   []string `json:"overrideFields"`
	HasProvider      bool     `json:"hasProvider"`
	KeyCount         int      `json:"keyCount"`
	// Access separates what costs nothing (free) from what bills per token
	// (paid) from what an enrolled login already covers (subscription).
	Access    string `json:"access"`
	Keyless   bool   `json:"keyless"`
	Available bool   `json:"available"`
}

// handleModelsList returns the whole catalog with its chain position, provider
// availability, access tier and key count. The dashboard derives its rows from
// GET /api/fallback; this endpoint stays for API consumers and the TUI's model
// editor (models.ts:211-283).
//
// With a profile active, a row's chain membership is EXACTLY its profile
// membership: the global fallback_config is not consulted, so a model enabled
// in the global chain but absent from the active profile reads as out-of-chain
// rather than leaking in.
func (s *Server) handleModelsList(w http.ResponseWriter, _ *http.Request) {
	db := s.engine.DB()
	activeID, active := routingActiveProfileID(db)

	var query string
	var args []any
	if active {
		query = `SELECT m.id, m.platform, m.model_id, m.display_name, m.intelligence_rank,
			m.speed_rank, m.size_label, m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit,
			m.monthly_token_budget, m.context_window, m.enabled, m.supports_vision, m.supports_tools,
			m.source, m.key_id, m.endpoint_scope, ak.label,
			pm.position,
			CASE WHEN pm.model_db_id IS NOT NULL THEN 1 ELSE 0 END AS fallback_enabled,
			m.paid_input_per_m, m.paid_output_per_m, m.available
			FROM models m
			LEFT JOIN profile_models pm ON pm.profile_id = ? AND pm.model_db_id = m.id
			LEFT JOIN api_keys ak ON ak.id = m.key_id
			ORDER BY (CASE WHEN pm.model_db_id IS NOT NULL THEN 0 ELSE 1 END) ASC,
				COALESCE(pm.position, m.intelligence_rank) ASC`
		args = append(args, activeID)
	} else {
		query = `SELECT m.id, m.platform, m.model_id, m.display_name, m.intelligence_rank,
			m.speed_rank, m.size_label, m.rpm_limit, m.rpd_limit, m.tpm_limit, m.tpd_limit,
			m.monthly_token_budget, m.context_window, m.enabled, m.supports_vision, m.supports_tools,
			m.source, m.key_id, m.endpoint_scope, ak.label,
			fc.position, COALESCE(fc.enabled, 0) AS fallback_enabled,
			m.paid_input_per_m, m.paid_output_per_m, m.available
			FROM models m
			LEFT JOIN fallback_config fc ON fc.model_db_id = m.id
			LEFT JOIN api_keys ak ON ak.id = m.key_id
			ORDER BY COALESCE(fc.position, m.intelligence_rank) ASC`
	}

	keyCounts := keyCountsByPlatform(db, false)
	usableKeyCounts := keyCountsByPlatform(db, true)
	usableKeyIDs := usableKeyIDSet(db)
	rows, err := db.Query(query, args...)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not list models")
		return
	}
	defer rows.Close()

	out := make([]modelListOut, 0)
	for rows.Next() {
		var (
			m                  modelListOut
			rpm, rpd, tpm, tpd sql.NullInt64
			ctx, keyID, pri    sql.NullInt64
			enabled, vision    int
			tools, fbEnabled   int
			endpointScope      string
			keyLabel           sql.NullString
			rawSource          string
			paidIn, paidOut    sql.NullFloat64
			served             int
		)
		if err := rows.Scan(&m.ID, &m.Platform, &m.ModelID, &m.DisplayName, &m.IntelligenceRank,
			&m.SpeedRank, &m.SizeLabel, &rpm, &rpd, &tpm, &tpd, &m.MonthlyBudget, &ctx,
			&enabled, &vision, &tools, &rawSource, &keyID, &endpointScope, &keyLabel,
			&pri, &fbEnabled, &paidIn, &paidOut, &served); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not list models")
			return
		}
		m.RPMLimit, m.RPDLimit, m.TPMLimit, m.TPDLimit = nullInt64Ptr(rpm), nullInt64Ptr(rpd), nullInt64Ptr(tpm), nullInt64Ptr(tpd)
		m.ContextWindow = nullInt64Ptr(ctx)
		m.Enabled = enabled != 0
		m.SupportsVision = vision != 0
		m.SupportsTools = tools != 0
		m.Priority = nullInt64Ptr(pri)
		m.FallbackEnabled = fbEnabled != 0
		m.Access = "free"
		switch {
		case rawSource == "login":
			m.Access = "subscription"
		case paidIn.Valid || paidOut.Valid:
			m.Access = "paid"
		}
		m.Source = rawSource
		if m.Source != "custom" {
			m.Source = "catalog"
		}
		m.KeyID = nullInt64Ptr(keyID)
		if keyLabel.Valid {
			m.KeyLabel = &keyLabel.String
		}
		if endpointScope != "" {
			m.EndpointScope = &endpointScope
		}
		m.HasOverrides = false
		m.OverrideFields = []string{}
		m.HasProvider = s.engine.Registry().Has(m.Platform)
		if p, ok := s.engine.Registry().Get(m.Platform); ok {
			m.Keyless = p.Keyless()
		}
		m.KeyCount = keyCounts[m.Platform]
		// Availability is the router's own eligibility: an adapter plus an
		// enabled, non-errored credential row for the platform. A keyless
		// provider still needs its enabled sentinel row (key expansion drops a
		// platform with no usable row), so the adapter's Keyless flag alone must
		// not call it available - that showed models nobody had switched on as
		// routable. A custom relay is bound to one endpoint's key, so its
		// availability is that key's health, not the platform aggregate.
		// A row the provider retired (models.available = 0) is never a
		// candidate, whatever its credential looks like.
		if rawSource == "custom" && keyID.Valid {
			m.Available = served != 0 && m.HasProvider && usableKeyIDs[keyID.Int64]
		} else {
			m.Available = served != 0 && m.HasProvider && usableKeyCounts[m.Platform] > 0
		}
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, out)
}

// usableKeyIDSet is the set of api_keys ids the router treats as usable:
// enabled and not parked at error (unknown counts as usable - "not yet probed"
// is not evidence of failure). It exists so a custom relay's availability can be
// judged by ITS bound key rather than the "custom" platform aggregate, which
// would let one working relay make a sibling with a dead credential look live.
func usableKeyIDSet(db *sql.DB) map[int64]bool {
	out := map[int64]bool{}
	rows, err := db.Query(
		`SELECT id FROM api_keys WHERE enabled = 1 AND status IN ('healthy', 'unknown')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out[id] = true
		}
	}
	return out
}

type modelPatchBody struct {
	DisplayName        *string `json:"displayName"`
	IntelligenceRank   *int    `json:"intelligenceRank"`
	SpeedRank          *int    `json:"speedRank"`
	SizeLabel          *string `json:"sizeLabel"`
	RPMLimit           *int64  `json:"rpmLimit"`
	RPDLimit           *int64  `json:"rpdLimit"`
	TPMLimit           *int64  `json:"tpmLimit"`
	TPDLimit           *int64  `json:"tpdLimit"`
	MonthlyTokenBudget *string `json:"monthlyTokenBudget"`
	ContextWindow      *int64  `json:"contextWindow"`
	Enabled            *bool   `json:"enabled"`
	SupportsVision     *bool   `json:"supportsVision"`
	SupportsTools      *bool   `json:"supportsTools"`
	FallbackEnabled    *bool   `json:"fallbackEnabled"`
}

// handleModelPatch edits a model's catalog fields and its membership in the
// chain currently visible to the operator. Model enablement and chain
// membership are independent controls: disabling a model makes every saved
// membership inert without silently deleting it, so re-enabling restores the
// operator's prior sets.
func (s *Server) handleModelPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	var body modelPatchBody
	if !DecodeJSON(w, r, &body) {
		return
	}
	db := s.engine.DB()
	var exists int64
	if err := db.QueryRow(`SELECT id FROM models WHERE id = ?`, id).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Unknown model "+strconv.FormatInt(id, 10))
		return
	}

	assignments, values := modelPatchAssignments(body)
	if len(assignments) == 0 && body.FallbackEnabled == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "No model fields provided")
		return
	}
	activeID, active := routingActiveProfileID(db)

	tx, err := db.Begin()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the model")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if len(assignments) > 0 {
		values = append(values, id)
		if _, err := tx.Exec(`UPDATE models SET `+strings.Join(assignments, ", ")+` WHERE id = ?`, values...); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the model")
			return
		}
	}
	if body.FallbackEnabled != nil {
		if err := setModelChainMembership(tx, activeID, active, id, *body.FallbackEnabled); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the chain flag")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not commit the update")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

func setModelChainMembership(tx *sql.Tx, activeID int64, active bool, modelID int64, enabled bool) error {
	if active {
		if !enabled {
			// A removal from a profile is durable: record it so the Default
			// profile's boot-time auto-include cannot resurrect it.
			if _, err := tx.Exec(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, activeID, modelID); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT OR IGNORE INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`, activeID, modelID)
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO profile_models(profile_id, model_db_id, position)
			VALUES (?, ?, COALESCE((SELECT MAX(position) + 1 FROM profile_models WHERE profile_id = ?), 1))
			ON CONFLICT(profile_id, model_db_id) DO NOTHING`,
			activeID, modelID, activeID); err != nil {
			return err
		}
		// Explicitly including a model lifts any recorded exclusion.
		_, err := tx.Exec(`DELETE FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?`, activeID, modelID)
		return err
	}
	if !enabled {
		_, err := tx.Exec(`UPDATE fallback_config SET enabled = 0 WHERE model_db_id = ?`, modelID)
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO fallback_config(model_db_id, position, enabled)
		VALUES (?, COALESCE((SELECT MAX(position) + 1 FROM fallback_config), 1), 1)
		ON CONFLICT(model_db_id) DO UPDATE SET enabled = 1`, modelID)
	return err
}

func modelPatchAssignments(b modelPatchBody) ([]string, []any) {
	var cols []string
	var vals []any
	add := func(col string, v any) { cols = append(cols, col+" = ?"); vals = append(vals, v) }
	if b.DisplayName != nil {
		add("display_name", *b.DisplayName)
	}
	if b.IntelligenceRank != nil {
		add("intelligence_rank", *b.IntelligenceRank)
	}
	if b.SpeedRank != nil {
		add("speed_rank", *b.SpeedRank)
	}
	if b.SizeLabel != nil {
		add("size_label", *b.SizeLabel)
	}
	if b.RPMLimit != nil {
		add("rpm_limit", *b.RPMLimit)
	}
	if b.RPDLimit != nil {
		add("rpd_limit", *b.RPDLimit)
	}
	if b.TPMLimit != nil {
		add("tpm_limit", *b.TPMLimit)
	}
	if b.TPDLimit != nil {
		add("tpd_limit", *b.TPDLimit)
	}
	if b.MonthlyTokenBudget != nil {
		add("monthly_token_budget", *b.MonthlyTokenBudget)
	}
	if b.ContextWindow != nil {
		add("context_window", *b.ContextWindow)
	}
	if b.Enabled != nil {
		add("enabled", routingBoolToInt(*b.Enabled))
	}
	if b.SupportsVision != nil {
		add("supports_vision", routingBoolToInt(*b.SupportsVision))
	}
	if b.SupportsTools != nil {
		add("supports_tools", routingBoolToInt(*b.SupportsTools))
	}
	return cols, vals
}

// handleModelDelete removes a chat/catalog model and its chain rows
// (profile_models cascades on the models foreign key; models.ts:179-208).
//
// A catalog-owned row (source='catalog', empty endpoint_scope) is the shipped
// catalog SeedModels re-inserts on every boot, so deleting the row alone would
// silently resurrect it. Its deletion is recorded as a tombstone in the SAME
// transaction as the DELETE, so a crash can never leave the model gone but
// un-tombstoned - the intermittent resurrection bug. Operator-owned rows (a
// hand-added model or a custom endpoint) are never re-seeded, so they delete
// without a tombstone, and the response reports the tombstoned state truthfully.
func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	db := s.engine.DB()
	var platform, modelID, source, scope string
	if err := db.QueryRow(
		`SELECT platform, model_id, source, endpoint_scope FROM models WHERE id = ?`, id,
	).Scan(&platform, &modelID, &source, &scope); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Unknown model "+strconv.FormatInt(id, 10))
		return
	}
	catalogOwned := source == "catalog" && scope == ""

	ctx := r.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the model")
		return
	}
	defer func() { _ = tx.Rollback() }()
	if catalogOwned {
		if err := catalog.RecordModelTombstone(ctx, tx, catalog.TombstoneChat, platform, modelID, ""); err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the model")
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the model")
		return
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the model")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true, "tombstoned": catalogOwned})
}

// handleModelCustomDelete removes a custom relay model (models.ts:76-100).
func (s *Server) handleModelCustomDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := routingPathID(w, r)
	if !ok {
		return
	}
	db := s.engine.DB()
	var exists int64
	if err := db.QueryRow(`SELECT id FROM models WHERE id = ? AND platform = 'custom'`, id).Scan(&exists); err != nil {
		WriteError(w, http.StatusNotFound, TypeNotFound, "Unknown custom model "+strconv.FormatInt(id, 10))
		return
	}
	if _, err := db.Exec(`DELETE FROM models WHERE id = ? AND platform = 'custom'`, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete the custom model")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ── small helpers ───────────────────────────────────────────────────────────

// routingPathID reads the {id} path value, answering 400 on a non-integer.
// Named to stay independent of the sibling keys slice's own id helper.
func routingPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid id")
		return 0, false
	}
	return id, true
}

func routingBoolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
