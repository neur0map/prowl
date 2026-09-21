package api

// The model-browse and provider-directory surface the redesigned dashboard's
// catalog page, per-model detail page and provider directory read. It is the
// one place that presents the WHOLE catalog - every model, keyed or not -
// unified by display name so a logical model carries every platform that
// serves it, with the live routing figures the router itself scores against.
//
// Every route is session-gated, so a validation refusal here carries
// TypeInvalidRequest and never signs the operator out (see RequireSession).

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/catalog"
)

func (s *Server) registerCatalogBrowseRoutes() {
	s.mux.HandleFunc("GET /api/catalog/models", s.RequireKey(s.handleCatalogModels))
	s.mux.HandleFunc("PUT /api/catalog/use", s.RequireKey(s.handleCatalogUse))
	s.mux.HandleFunc("GET /api/catalog/providers", s.RequireKey(s.handleCatalogProviders))
}

// ── wire shapes ─────────────────────────────────────────────────────────────

type catalogCapsOut struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

type catalogProviderOut struct {
	ModelDBID     int64    `json:"modelDbId"`
	Platform      string   `json:"platform"`
	Label         string   `json:"label"`
	HasKey        bool     `json:"hasKey"`
	KeyStatus     string   `json:"keyStatus"`
	PriceIn       *float64 `json:"priceIn"`
	PriceOut      *float64 `json:"priceOut"`
	ContextWindow *int64   `json:"contextWindow"`
	LatencyMs     *int64   `json:"latencyMs"`
	Throughput    *float64 `json:"throughput"`
	Reliability   *int     `json:"reliability"`
	Requests      *int     `json:"requests"`
	InGateway     bool     `json:"inGateway"`
}

type catalogModelOut struct {
	ID            int64                `json:"id"`
	CanonicalID   string               `json:"canonicalId"`
	Name          string               `json:"name"`
	Author        string               `json:"author"`
	Tier          string               `json:"tier"`
	ContextWindow *int64               `json:"contextWindow"`
	PriceIn       *float64             `json:"priceIn"`
	PriceOut      *float64             `json:"priceOut"`
	Caps          catalogCapsOut       `json:"caps"`
	Score         *float64             `json:"score"`
	Reliability   *int                 `json:"reliability"`
	Speed         *int                 `json:"speed"`
	Intelligence  *int                 `json:"intelligence"`
	InGateway     bool                 `json:"inGateway"`
	Routable      bool                 `json:"routable"`
	Providers     []catalogProviderOut `json:"providers"`
}

type catalogAvailabilityFacet struct {
	InGateway int `json:"inGateway"`
	Routable  int `json:"routable"`
	NeedsKey  int `json:"needsKey"`
}

type catalogProviderFacet struct {
	Platform string `json:"platform"`
	Label    string `json:"label"`
	Models   int    `json:"models"`
}

type catalogAuthorFacet struct {
	Author string `json:"author"`
	Models int    `json:"models"`
}

type catalogFacets struct {
	Tier         map[string]int           `json:"tier"`
	Availability catalogAvailabilityFacet `json:"availability"`
	Caps         map[string]int           `json:"caps"`
	Providers    []catalogProviderFacet   `json:"providers"`
	Authors      []catalogAuthorFacet     `json:"authors"`
	Modalities   map[string]int           `json:"modalities"`
}

type catalogModelsResponse struct {
	Total  int               `json:"total"`
	Models []catalogModelOut `json:"models"`
	Facets catalogFacets     `json:"facets"`
}

// catalogRow is one provider row (a models/embedding_models/media_models row)
// before rows are unified by display name into logical models.
type catalogRow struct {
	id            int64
	platform      string
	modelID       string
	keyID         *int64
	displayName   string
	tier          string
	priceIn       *float64
	priceOut      *float64
	contextWindow *int64
	caps          catalogCapsOut
	score         *float64
	reliability   *int
	speed         *int
	intelligence  *int
	latencyMs     *int64
	throughput    *float64
	requests      *int
	hasKey        bool
	keyStatus     string
	usable        bool
	inGateway     bool
}

// ── GET /api/catalog/models ─────────────────────────────────────────────────

func (s *Server) handleCatalogModels(w http.ResponseWriter, r *http.Request) {
	modality := strings.TrimSpace(r.URL.Query().Get("modality"))
	if modality == "" {
		modality = "chat"
	}
	if !catalogModalityValid(modality) {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			"modality must be one of chat, embeddings, image, video, audio")
		return
	}

	ctx := r.Context()
	db := s.engine.DB()
	keys := loadCatalogKeys(db)
	inGateway := s.catalogInGatewaySet(db)
	labels := s.catalogPlatformLabels()
	perf := catalogPerfByModel(ctx, db, time.Now().Add(-statsWindow).Unix())

	var rows []catalogRow
	switch modality {
	case "chat":
		weights := routingDisplayWeights(s.routingStrategy(ctx), routingCustomWeights(db))
		rows = s.catalogChatRows(ctx, db, keys, inGateway, perf, weights)
	case "embeddings":
		rows = catalogEmbeddingRows(db, keys, perf)
	default:
		rows = catalogMediaRows(db, modality, keys, perf)
	}

	models := groupCatalogRows(rows, labels)
	WriteJSON(w, http.StatusOK, catalogModelsResponse{
		Total:  len(models),
		Models: models,
		Facets: s.buildCatalogFacets(db, models, labels),
	})
}

func catalogModalityValid(m string) bool {
	switch m {
	case "chat", "embeddings", "image", "video", "audio":
		return true
	default:
		return false
	}
}

// catalogChatRows scores every enabled chat model the way the routing table
// does - the decay-weighted trail from aggregateTrail for reliability/speed,
// the tier+rank composite for intelligence - so a model with no evidence
// reports null figures rather than a fabricated zero. Intelligence is
// normalised against the range present in this modality, computed once.
func (s *Server) catalogChatRows(ctx context.Context, db *sql.DB, keys catalogKeys,
	inGateway map[int64]bool, perf map[string]pmPerf, weights gateway.Weights,
) []catalogRow {
	type raw struct {
		id            int64
		platform      string
		modelID       string
		keyID         *int64
		name          string
		intelRank     int
		sizeLabel     string
		contextWindow *int64
		caps          catalogCapsOut
		source        string
		priceIn       *float64
		priceOut      *float64
	}

	rowsData, err := db.QueryContext(ctx, `
		SELECT id, platform, model_id, display_name, intelligence_rank, size_label,
		       context_window, supports_vision, supports_tools, supports_reasoning,
		       source, paid_input_per_m, paid_output_per_m, key_id
		  FROM models WHERE enabled = 1 AND available = 1`)
	if err != nil {
		return nil
	}
	defer func() { _ = rowsData.Close() }()

	var raws []raw
	for rowsData.Next() {
		var rw raw
		var vision, tools, reasoning int
		if err := rowsData.Scan(&rw.id, &rw.platform, &rw.modelID, &rw.name, &rw.intelRank,
			&rw.sizeLabel, &rw.contextWindow, &vision, &tools, &reasoning,
			&rw.source, &rw.priceIn, &rw.priceOut, &rw.keyID); err != nil {
			break
		}
		rw.caps = catalogCapsOut{Tools: tools != 0, Vision: vision != 0, Reasoning: reasoning != 0}
		raws = append(raws, rw)
	}

	intelMin, intelMax := math.Inf(1), math.Inf(-1)
	for _, rw := range raws {
		c := gateway.IntelligenceComposite(scoringTier(rw.sizeLabel), rw.intelRank)
		intelMin, intelMax = math.Min(intelMin, c), math.Max(intelMax, c)
	}

	byModel := s.catalogChatStats(ctx)
	out := make([]catalogRow, 0, len(raws))
	for _, rw := range raws {
		row := catalogRow{
			id: rw.id, keyID: rw.keyID, platform: rw.platform, modelID: rw.modelID, displayName: rw.name,
			tier:          catalogTier(rw.source, rw.priceIn, rw.priceOut),
			priceIn:       rw.priceIn,
			priceOut:      rw.priceOut,
			contextWindow: rw.contextWindow,
			caps:          rw.caps,
			inGateway:     inGateway[rw.id],
		}
		row.hasKey, row.keyStatus, row.usable = catalogKeyState(keys, rw.platform, rw.modelID, rw.keyID)
		if st, ok := byModel[rw.id]; ok && st.Total() > 0 {
			rel := gateway.ReliabilityPosterior(st.successes, st.failures).Expected()
			ttfb := -1.0
			if st.hasTTFB {
				ttfb = st.ttfbMs
			}
			speed := gateway.SpeedScore(st.tokPerSec, ttfb)
			composite := gateway.IntelligenceComposite(scoringTier(rw.sizeLabel), rw.intelRank)
			intel := gateway.IntelligenceScore(composite, intelMin, intelMax)
			row.score = new(gateway.Combine(weights, rel, speed, intel, 1, 1).Effective)
			row.reliability = pct(rel)
			row.speed = pct(speed)
			row.intelligence = pct(intel)
		}
		catalogApplyPerf(&row, perf)
		out = append(out, row)
	}
	return out
}

func catalogEmbeddingRows(db *sql.DB, keys catalogKeys, perf map[string]pmPerf) []catalogRow {
	rows, err := db.Query(`SELECT id, platform, model_id, display_name, key_id FROM embedding_models WHERE enabled = 1`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	// Embeddings carry no price, caps, score or routing-chain membership in
	// this schema, so those fields stay null; only the live trail applies.
	var out []catalogRow
	for rows.Next() {
		var row catalogRow
		row.tier = "free"
		if err := rows.Scan(&row.id, &row.platform, &row.modelID, &row.displayName, &row.keyID); err != nil {
			return out
		}
		row.hasKey, row.keyStatus, row.usable = catalogKeyState(keys, row.platform, row.modelID, row.keyID)
		catalogApplyPerf(&row, perf)
		out = append(out, row)
	}
	return out
}

func catalogMediaRows(db *sql.DB, modality string, keys catalogKeys, perf map[string]pmPerf) []catalogRow {
	modalities := []string{modality}
	if modality == "audio" {
		// The catalog has no separate transcription tab, so speech-to-text
		// models are browsable alongside text-to-speech under audio.
		modalities = []string{"audio", "transcription"}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(modalities)), ",")
	args := make([]any, len(modalities))
	for i, m := range modalities {
		args[i] = m
	}
	rows, err := db.Query(
		`SELECT id, platform, model_id, display_name, key_id FROM media_models
		  WHERE enabled = 1 AND modality IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []catalogRow
	for rows.Next() {
		var row catalogRow
		row.tier = "free"
		if err := rows.Scan(&row.id, &row.platform, &row.modelID, &row.displayName, &row.keyID); err != nil {
			return out
		}
		row.hasKey, row.keyStatus, row.usable = catalogKeyState(keys, row.platform, row.modelID, row.keyID)
		catalogApplyPerf(&row, perf)
		out = append(out, row)
	}
	return out
}

func catalogApplyPerf(row *catalogRow, perf map[string]pmPerf) {
	if p, ok := perf[pmKey(row.platform, row.modelID)]; ok {
		row.requests = new(p.requests)
		row.latencyMs = p.latencyMs
		row.throughput = p.throughput
	}
}

// groupCatalogRows unifies provider rows by a hardened key of their
// suffix-stripped display name, so the same model tagged per provider
// ("GPT-OSS 120B (Groq)", "GPT Oss 120B", "gpt oss 120b") collapses to one
// entry. The headline price/context/score/axes come from the group's best
// provider - highest live score, then cheapest - the headline name is the
// most common surviving spelling, and canonicalId is chosen score-independently
// so a detail-page URL never rots when traffic shifts.
func groupCatalogRows(rows []catalogRow, labels map[string]string) []catalogModelOut {
	order := make([]string, 0, len(rows))
	groups := map[string][]catalogRow{}
	for _, rw := range rows {
		key := catalogGroupKey(catalogStripSuffix(rw.displayName))
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], rw)
	}

	out := make([]catalogModelOut, 0, len(order))
	for _, key := range order {
		g := groups[key]
		modelIDs := make([]string, len(g))
		names := make([]string, len(g))
		providers := make([]catalogProviderOut, len(g))
		var caps catalogCapsOut
		routable, inGateway := false, false
		for i, rw := range g {
			modelIDs[i] = rw.modelID
			names[i] = catalogStripSuffix(rw.displayName)
			caps.Tools = caps.Tools || rw.caps.Tools
			caps.Vision = caps.Vision || rw.caps.Vision
			caps.Reasoning = caps.Reasoning || rw.caps.Reasoning
			routable = routable || rw.usable
			inGateway = inGateway || rw.inGateway
			providers[i] = catalogProviderOut{
				ModelDBID: rw.id, Platform: rw.platform, Label: catalogLabel(labels, rw.platform),
				HasKey: rw.hasKey, KeyStatus: rw.keyStatus,
				PriceIn: rw.priceIn, PriceOut: rw.priceOut, ContextWindow: rw.contextWindow,
				LatencyMs: rw.latencyMs, Throughput: rw.throughput, Reliability: rw.reliability,
				Requests: rw.requests, InGateway: rw.inGateway,
			}
		}
		best := g[catalogBestProvider(g)]
		canonical := catalogCanonicalID(modelIDs)
		out = append(out, catalogModelOut{
			ID: best.id, CanonicalID: canonical, Name: catalogHeadlineName(names),
			Author: catalogAuthor(canonical, best.platform), Tier: best.tier,
			ContextWindow: best.contextWindow, PriceIn: best.priceIn, PriceOut: best.priceOut,
			Caps: caps, Score: best.score, Reliability: best.reliability, Speed: best.speed,
			Intelligence: best.intelligence, InGateway: inGateway, Routable: routable,
			Providers: providers,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].CanonicalID < out[j].CanonicalID
	})
	return out
}

func catalogBestProvider(rows []catalogRow) int {
	best := 0
	for i := 1; i < len(rows); i++ {
		if catalogProviderBetter(rows[i], rows[best]) {
			best = i
		}
	}
	return best
}

// catalogProviderBetter ranks a provider ahead of another by live score, then
// by cheapest price. An unmeasured score sorts last; a null (free) price sorts
// as cheapest, so a free provider wins a score tie and its null price becomes
// the model's headline.
func catalogProviderBetter(a, b catalogRow) bool {
	if sa, sb := catalogScoreKey(a.score), catalogScoreKey(b.score); sa != sb {
		return sa > sb
	}
	if pa, pb := catalogPriceKey(a.priceOut), catalogPriceKey(b.priceOut); pa != pb {
		return pa < pb
	}
	if pa, pb := catalogPriceKey(a.priceIn), catalogPriceKey(b.priceIn); pa != pb {
		return pa < pb
	}
	return a.id < b.id
}

func catalogScoreKey(s *float64) float64 {
	if s == nil {
		return -1
	}
	return *s
}

func catalogPriceKey(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// catalogCanonicalID is the group's stable id - the id a client sends. It
// prefers the cleanest vendor-qualified member (exactly "author/model", no
// vendor-route "@" prefix, e.g. "openai/gpt-oss-120b"), then any
// slash-qualified id, then the bare id, always taking the lexicographically
// smallest within the best rank so it stays stable when scores reorder the
// group. A route-specific form like "@cf/openai/gpt-oss-120b" is never chosen
// while a portable one exists.
func catalogCanonicalID(modelIDs []string) string {
	best, rank := "", 0
	for _, m := range modelIDs {
		r := 1
		if strings.Contains(m, "/") {
			r = 2
			if !strings.HasPrefix(m, "@") && strings.Count(m, "/") == 1 {
				r = 3
			}
		}
		if r > rank || (r == rank && (best == "" || m < best)) {
			best, rank = m, r
		}
	}
	return best
}

// catalogAuthor is the family author: the canonicalId's leading segment, or
// the platform when the id is unqualified. A vendor-route prefix ("@cf/…") or
// a namespaced 3+-segment id ("@cf/openai/gpt-oss-120b") takes its author from
// the second segment, so the row reads "openai" rather than the route prefix.
func catalogAuthor(canonicalID, platform string) string {
	segments := strings.Split(canonicalID, "/")
	if len(segments) < 2 {
		return platform
	}
	idx := 0
	if strings.HasPrefix(segments[0], "@") || len(segments) >= 3 {
		idx = 1
	}
	if segments[idx] != "" {
		return segments[idx]
	}
	return platform
}

// catalogStripSuffix removes a single trailing "(...)" provider tag from a
// display name ("GPT-OSS 120B (Groq)" -> "GPT-OSS 120B"), leaving inner
// parentheticals alone and keeping the original when stripping empties it.
func catalogStripSuffix(name string) string {
	trimmed := strings.TrimRight(name, " \t")
	if !strings.HasSuffix(trimmed, ")") {
		return name
	}
	open := strings.LastIndex(trimmed, "(")
	if open <= 0 {
		return name
	}
	stripped := strings.TrimRight(trimmed[:open], " \t")
	if stripped == "" {
		return name
	}
	return stripped
}

// catalogGroupKey is the grouping identity of a stripped name: casefolded with
// every non-alphanumeric character removed, so "GPT-OSS 120B", "GPT Oss 120B"
// and "gpt oss 120b" all key to "gptoss120b" while 120B and 20B stay apart.
func catalogGroupKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// catalogHeadlineName picks a real display name for a group: the most common
// surviving spelling among members, then the longest, then the smallest, so
// the headline is never the punctuation-stripped key.
func catalogHeadlineName(names []string) string {
	counts := map[string]int{}
	for _, n := range names {
		counts[n]++
	}
	best := ""
	for _, n := range names {
		switch {
		case best == "":
			best = n
		case counts[n] != counts[best]:
			if counts[n] > counts[best] {
				best = n
			}
		case len(n) != len(best):
			if len(n) > len(best) {
				best = n
			}
		case n < best:
			best = n
		}
	}
	return best
}

func catalogTier(source string, priceIn, priceOut *float64) string {
	switch {
	case source == "login":
		return "subscription"
	case priceOut != nil || priceIn != nil:
		return "paid"
	default:
		return "free"
	}
}

func catalogLabel(labels map[string]string, platform string) string {
	if l := labels[platform]; l != "" {
		return l
	}
	return platform
}

// ── facets ──────────────────────────────────────────────────────────────────

func (s *Server) buildCatalogFacets(db *sql.DB, models []catalogModelOut, labels map[string]string) catalogFacets {
	f := catalogFacets{
		Tier:       map[string]int{"free": 0, "paid": 0, "subscription": 0},
		Caps:       map[string]int{"tools": 0, "vision": 0, "reasoning": 0},
		Modalities: catalogModalityCounts(db),
	}
	providerCounts := map[string]int{}
	authorCounts := map[string]int{}
	for _, m := range models {
		f.Tier[m.Tier]++
		if m.Caps.Tools {
			f.Caps["tools"]++
		}
		if m.Caps.Vision {
			f.Caps["vision"]++
		}
		if m.Caps.Reasoning {
			f.Caps["reasoning"]++
		}
		if m.InGateway {
			f.Availability.InGateway++
		}
		if m.Routable {
			f.Availability.Routable++
		} else {
			f.Availability.NeedsKey++
		}
		authorCounts[m.Author]++
		seen := map[string]bool{}
		for _, p := range m.Providers {
			if seen[p.Platform] {
				continue
			}
			seen[p.Platform] = true
			providerCounts[p.Platform]++
		}
	}
	f.Providers = sortedProviderFacets(providerCounts, labels)
	f.Authors = sortedAuthorFacets(authorCounts)
	return f
}

// catalogModalityCounts counts logical (suffix-stripped, name-unified) models
// per modality for the tab badges, so every modality's count is present even
// when the current request is for another. The grouping is done in Go, since
// SQL cannot strip the trailing provider parenthetical the way the browse does.
func catalogModalityCounts(db *sql.DB) map[string]int {
	return map[string]int{
		"chat":       catalogGroupCount(db, `SELECT display_name FROM models WHERE enabled = 1 AND available = 1`),
		"embeddings": catalogGroupCount(db, `SELECT display_name FROM embedding_models WHERE enabled = 1`),
		"image":      catalogGroupCount(db, `SELECT display_name FROM media_models WHERE enabled = 1 AND modality = 'image'`),
		"video":      catalogGroupCount(db, `SELECT display_name FROM media_models WHERE enabled = 1 AND modality = 'video'`),
		"audio":      catalogGroupCount(db, `SELECT display_name FROM media_models WHERE enabled = 1 AND modality IN ('audio', 'transcription')`),
	}
}

func catalogGroupCount(db *sql.DB, query string, args ...any) int {
	rows, err := db.Query(query, args...)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return len(seen)
		}
		seen[catalogGroupKey(catalogStripSuffix(name))] = true
	}
	return len(seen)
}

func sortedProviderFacets(counts map[string]int, labels map[string]string) []catalogProviderFacet {
	out := make([]catalogProviderFacet, 0, len(counts))
	for platform, n := range counts {
		out = append(out, catalogProviderFacet{Platform: platform, Label: catalogLabel(labels, platform), Models: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Models != out[j].Models {
			return out[i].Models > out[j].Models
		}
		return out[i].Platform < out[j].Platform
	})
	return out
}

func sortedAuthorFacets(counts map[string]int) []catalogAuthorFacet {
	out := make([]catalogAuthorFacet, 0, len(counts))
	for author, n := range counts {
		out = append(out, catalogAuthorFacet{Author: author, Models: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Models != out[j].Models {
			return out[i].Models > out[j].Models
		}
		return out[i].Author < out[j].Author
	})
	return out
}

// ── PUT /api/catalog/use ────────────────────────────────────────────────────

type catalogUseBody struct {
	ModelDBIDs []int64 `json:"modelDbIds"`
	Use        *bool   `json:"use"`
}

// handleCatalogUse adds or removes the given rows from the ACTIVE routing list
// (the active profile's membership, or the global fallback when no profile is
// active). It is idempotent: an existing member is left in place, a non-member
// removal is a no-op, and only new members are appended after the current last
// position, so untouched members never renumber.
func (s *Server) handleCatalogUse(w http.ResponseWriter, r *http.Request) {
	var body catalogUseBody
	if !DecodeJSON(w, r, &body) {
		return
	}
	if body.Use == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "use is required")
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
	var writeErr error
	if active {
		writeErr = catalogUseProfile(tx, activeID, body, known)
	} else {
		writeErr = catalogUseFallback(tx, body, known)
	}
	if writeErr != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the routing list")
		return
	}
	if err := tx.Commit(); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not commit the routing list")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]any{"inGateway": s.catalogInGatewayCount(db)})
}

func catalogUseProfile(tx *sql.Tx, profileID int64, body catalogUseBody, known map[int64]bool) error {
	if !*body.Use {
		del, err := tx.Prepare(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`)
		if err != nil {
			return err
		}
		defer del.Close()
		// A removal is durable: record it so the Default profile's boot-time
		// auto-include cannot resurrect it.
		excl, err := tx.Prepare(`INSERT OR IGNORE INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`)
		if err != nil {
			return err
		}
		defer excl.Close()
		for _, id := range body.ModelDBIDs {
			if _, err := del.Exec(profileID, id); err != nil {
				return err
			}
			if _, err := excl.Exec(profileID, id); err != nil {
				return err
			}
		}
		return nil
	}

	members := map[int64]bool{}
	rows, err := tx.Query(`SELECT model_db_id FROM profile_models WHERE profile_id = ?`, profileID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		members[id] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	var maxPos sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(position) FROM profile_models WHERE profile_id = ?`, profileID).Scan(&maxPos); err != nil {
		return err
	}
	next := maxPos.Int64
	ins, err := tx.Prepare(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	// Explicitly including a model lifts any durable exclusion recorded when it
	// was previously removed, so a later boot's auto-include treats it normally.
	unexcl, err := tx.Prepare(`DELETE FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?`)
	if err != nil {
		return err
	}
	defer unexcl.Close()
	for _, id := range body.ModelDBIDs {
		if !known[id] {
			continue
		}
		if _, err := unexcl.Exec(profileID, id); err != nil {
			return err
		}
		if members[id] {
			continue
		}
		next++
		if _, err := ins.Exec(profileID, id, next); err != nil {
			return err
		}
		members[id] = true
	}
	return nil
}

func catalogUseFallback(tx *sql.Tx, body catalogUseBody, known map[int64]bool) error {
	if !*body.Use {
		upd, err := tx.Prepare(`UPDATE fallback_config SET enabled = 0 WHERE model_db_id = ?`)
		if err != nil {
			return err
		}
		defer upd.Close()
		for _, id := range body.ModelDBIDs {
			if _, err := upd.Exec(id); err != nil {
				return err
			}
		}
		return nil
	}

	existing := map[int64]bool{}
	rows, err := tx.Query(`SELECT model_db_id FROM fallback_config`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	var maxPos sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(position) FROM fallback_config`).Scan(&maxPos); err != nil {
		return err
	}
	next := maxPos.Int64
	enable, err := tx.Prepare(`UPDATE fallback_config SET enabled = 1 WHERE model_db_id = ?`)
	if err != nil {
		return err
	}
	defer enable.Close()
	ins, err := tx.Prepare(`INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, ?, 1)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, id := range body.ModelDBIDs {
		if !known[id] {
			continue
		}
		if existing[id] {
			if _, err := enable.Exec(id); err != nil {
				return err
			}
			continue
		}
		next++
		if _, err := ins.Exec(id, next); err != nil {
			return err
		}
		existing[id] = true
	}
	return nil
}

// ── GET /api/catalog/providers ──────────────────────────────────────────────

type catalogProviderDirEntry struct {
	Platform        string  `json:"platform"`
	Label           string  `json:"label"`
	Access          string  `json:"access"`
	HasKey          bool    `json:"hasKey"`
	KeyStatus       string  `json:"keyStatus"`
	Models          int     `json:"models"`
	RoutableModels  int     `json:"routableModels"`
	InGatewayModels int     `json:"inGatewayModels"`
	LatencyMs       *int64  `json:"latencyMs"`
	Reliability     *int    `json:"reliability"`
	Requests        *int    `json:"requests"`
	QuotaRemaining  *int64  `json:"quotaRemaining"`
	QuotaLimit      *int64  `json:"quotaLimit"`
	RequiresCard    bool    `json:"requiresCard"`
	SignupURL       *string `json:"signupUrl"`
	DocsURL         *string `json:"docsUrl"`
	Source          string  `json:"source"`
}

func (s *Server) handleCatalogProviders(w http.ResponseWriter, r *http.Request) {
	entries, err := catalog.Directory()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the provider directory")
		return
	}

	ctx := r.Context()
	db := s.engine.DB()
	keys := loadCatalogKeys(db)
	inGateway := s.catalogInGatewaySet(db)
	chatByPlatform := catalogChatModelsByPlatform(db)
	modelTotals := catalogModelTotalsByPlatform(ctx, db)
	servedByPlatform := catalogServedModelIDsByPlatform(ctx, db)
	platPerf := catalogPerfByPlatform(ctx, db, time.Now().Add(-statsWindow).Unix())

	out := make([]catalogProviderDirEntry, 0, len(entries))
	for _, e := range entries {
		platform := directoryPlatform(e.ID, s)
		pid := platform
		if pid == "" {
			pid = e.ID
		}
		hasKey, status, _ := catalogPlatformKeyState(keys, platform)

		routable := 0
		for _, m := range servedByPlatform[platform] {
			if _, _, usable := catalogKeyState(keys, platform, m.modelID, m.keyID); usable {
				routable++
			}
		}
		// In-gateway membership is chat-only: profile_models and fallback_config
		// reference the chat models table, so an embedding or media row can
		// never be in the routing chain and its id must not be matched here.
		inGwCount := 0
		for _, m := range chatByPlatform[platform] {
			if inGateway[m.id] {
				inGwCount++
			}
		}

		entry := catalogProviderDirEntry{
			Platform: pid, Label: e.Name, Access: catalogAccess(e, keys.linked, platform),
			HasKey: hasKey, KeyStatus: status,
			Models: modelTotals[platform], RoutableModels: routable, InGatewayModels: inGwCount,
			RequiresCard: e.Friction == "card",
			SignupURL:    strOrNil(e.APIKeyURL), DocsURL: strOrNil(e.DocsURL),
			Source: strings.Join(e.Sources, ", "),
		}
		if p, ok := platPerf[platform]; ok {
			entry.Requests = new(p.requests)
			entry.LatencyMs = p.latencyMs
			entry.Reliability = new(int(math.Round(gateway.ReliabilityPosterior(
				float64(p.successes), float64(p.requests-p.successes)).Expected() * 100)))
		}
		if q, ok := s.observedQuota(ctx, platform); ok {
			entry.QuotaRemaining = q.Remaining
			entry.QuotaLimit = q.Limit
		}
		out = append(out, entry)
	}

	WriteJSON(w, http.StatusOK, map[string]any{"total": len(out), "providers": out})
}

// catalogAccess classes a provider for the directory: a login Prowl already
// holds (a link: reference in the vault) or an OAuth-class provider reads as a
// subscription, a paid-class provider as paid, everything else as free.
func catalogAccess(e catalog.DirectoryEntry, linked map[string]bool, platform string) string {
	if (platform != "" && linked[platform]) || e.Class == catalog.ClassOAuth {
		return "subscription"
	}
	if e.Class == catalog.ClassPaid {
		return "paid"
	}
	return "free"
}

type catalogModelRef struct {
	id      int64
	modelID string
}

// catalogServedModel is one served model id and the key it is bound to (nil for
// an unbound catalogue row), so routable-eligibility can apply the router's own
// key-binding and linked-key rules.
type catalogServedModel struct {
	modelID string
	keyID   *int64
}

func catalogChatModelsByPlatform(db *sql.DB) map[string][]catalogModelRef {
	out := map[string][]catalogModelRef{}
	rows, err := db.Query(`SELECT id, platform, model_id FROM models WHERE enabled = 1 AND available = 1`)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref catalogModelRef
		var platform string
		if err := rows.Scan(&ref.id, &platform, &ref.modelID); err != nil {
			return out
		}
		out[platform] = append(out[platform], ref)
	}
	return out
}

func catalogModelTotalsByPlatform(ctx context.Context, db *sql.DB) map[string]int {
	out := map[string]int{}
	add := func(query string) {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var platform string
			var n int
			if err := rows.Scan(&platform, &n); err != nil {
				return
			}
			out[platform] += n
		}
	}
	add(`SELECT platform, COUNT(*) FROM models WHERE enabled = 1 AND available = 1 GROUP BY platform`)
	add(`SELECT platform, COUNT(*) FROM embedding_models WHERE enabled = 1 GROUP BY platform`)
	add(`SELECT platform, COUNT(*) FROM media_models WHERE enabled = 1 GROUP BY platform`)
	return out
}

// catalogServedModelIDsByPlatform returns every enabled model id a platform
// serves - chat, embedding and media - as the routable-eligibility set for the
// provider directory. Embedding and media models can be routed to once a
// credential covers them, so a provider that serves only those modalities is
// still routable; the routing-chain membership count, by contrast, stays
// chat-only.
func catalogServedModelIDsByPlatform(ctx context.Context, db *sql.DB) map[string][]catalogServedModel {
	out := map[string][]catalogServedModel{}
	add := func(query string) {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var platform string
			var m catalogServedModel
			if err := rows.Scan(&platform, &m.modelID, &m.keyID); err != nil {
				return
			}
			out[platform] = append(out[platform], m)
		}
	}
	add(`SELECT platform, model_id, key_id FROM models WHERE enabled = 1 AND available = 1`)
	add(`SELECT platform, model_id, key_id FROM embedding_models WHERE enabled = 1`)
	add(`SELECT platform, model_id, key_id FROM media_models WHERE enabled = 1`)
	return out
}

// ── shared helpers ──────────────────────────────────────────────────────────

// catalogKey is one api_keys row reduced to what eligibility needs.
type catalogKey struct {
	id      int64
	status  string
	enabled bool
	scope   []string
	linked  bool
}

type catalogKeys struct {
	byPlatform map[string][]catalogKey
	linked     map[string]bool
}

func loadCatalogKeys(db *sql.DB) catalogKeys {
	out := catalogKeys{byPlatform: map[string][]catalogKey{}, linked: map[string]bool{}}
	rows, err := db.Query(`SELECT id, platform, status, enabled, COALESCE(model_scope_json, ''), encrypted_key FROM api_keys`)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var platform, status, scopeRaw, encrypted string
		var id int64
		var enabled int
		if err := rows.Scan(&id, &platform, &status, &enabled, &scopeRaw, &encrypted); err != nil {
			return out
		}
		k := catalogKey{id: id, status: status, enabled: enabled != 0}
		if scopeRaw != "" {
			_ = json.Unmarshal([]byte(scopeRaw), &k.scope)
		}
		if _, ok := gateway.LinkedRef(encrypted); ok {
			k.linked = true
		}
		out.byPlatform[platform] = append(out.byPlatform[platform], k)
		if k.enabled && k.linked {
			out.linked[platform] = true
		}
	}
	return out
}

// catalogKeyState reports the key state for one model, mirroring the router's
// chain expansion (expandChainKeys): a model already bound to a key - a login
// or custom-relay row - routes ONLY through that key, and an unbound catalogue
// model is served by any enabled, non-error, in-scope key of its platform
// EXCEPT a linked-login credential, which is authorised solely for the models
// it seeded. Without both rules the catalog would claim a bound or linked-only
// route can serve models the router never would. Status ranks healthy > unknown
// > error; usable means routable - an enabled, non-error, in-scope key.
func catalogKeyState(keys catalogKeys, platform, modelID string, boundKey *int64) (bool, string, bool) {
	if boundKey != nil {
		for _, k := range keys.byPlatform[platform] {
			if k.id != *boundKey || !k.enabled || !catalogScopeAllows(k.scope, modelID) {
				continue
			}
			st := catalogNormalizeStatus(k.status)
			return true, st, st != "error"
		}
		return false, "none", false
	}
	best, bestRank := "", 0
	for _, k := range keys.byPlatform[platform] {
		if !k.enabled || k.linked || !catalogScopeAllows(k.scope, modelID) {
			continue
		}
		st := catalogNormalizeStatus(k.status)
		if r := catalogStatusRank(st); r > bestRank {
			bestRank, best = r, st
		}
	}
	if best == "" {
		return false, "none", false
	}
	return true, best, best != "error"
}

// catalogPlatformKeyState is the provider-level key state, ignoring per-model
// scope, for the provider directory.
func catalogPlatformKeyState(keys catalogKeys, platform string) (bool, string, bool) {
	best, bestRank := "", 0
	for _, k := range keys.byPlatform[platform] {
		if !k.enabled {
			continue
		}
		st := catalogNormalizeStatus(k.status)
		if r := catalogStatusRank(st); r > bestRank {
			bestRank, best = r, st
		}
	}
	if best == "" {
		return false, "none", false
	}
	return true, best, best != "error"
}

// catalogScopeAllows mirrors the router's keyServesModel: an empty scope serves
// every model of the platform, otherwise the model id must be listed.
func catalogScopeAllows(scope []string, modelID string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, allowed := range scope {
		if allowed == modelID {
			return true
		}
	}
	return false
}

func catalogNormalizeStatus(status string) string {
	switch status {
	case "healthy":
		return "healthy"
	case "error":
		return "error"
	default:
		return "unknown"
	}
}

func catalogStatusRank(status string) int {
	switch status {
	case "healthy":
		return 3
	case "unknown":
		return 2
	case "error":
		return 1
	default:
		return 0
	}
}

// scoringTier maps a catalog size label to the bandit capability tier the
// intelligence axis reads. It mirrors the router's own unexported tierFor: an
// unrecognised label floors to Unknown rather than excluding the model.
func scoringTier(sizeLabel string) gateway.Tier {
	switch sizeLabel {
	case "Frontier":
		return gateway.TierFrontier
	case "Large":
		return gateway.TierLarge
	case "Medium":
		return gateway.TierMedium
	case "Small":
		return gateway.TierSmall
	default:
		return gateway.TierUnknown
	}
}

func (s *Server) catalogChatStats(ctx context.Context) map[int64]routeStats {
	_, byModel, err := aggregateTrail(ctx, s)
	if err != nil {
		return map[int64]routeStats{}
	}
	return byModel
}

func (s *Server) catalogInGatewaySet(db *sql.DB) map[int64]bool {
	// A retired login model keeps its chain rows (its operator preference lives
	// there), so the chain-membership set must exclude models the provider no
	// longer offers, or the in-gateway count would include a model that can no
	// longer route.
	if activeID, active := routingActiveProfileID(db); active {
		set, _ := idSet(db, `SELECT pm.model_db_id FROM profile_models pm
			JOIN models m ON m.id = pm.model_db_id AND m.available = 1
			WHERE pm.profile_id = ?`, activeID)
		return set
	}
	set, _ := idSet(db, `SELECT fc.model_db_id FROM fallback_config fc
		JOIN models m ON m.id = fc.model_db_id AND m.available = 1
		WHERE fc.enabled = 1`)
	return set
}

func (s *Server) catalogInGatewayCount(db *sql.DB) int {
	return len(s.catalogInGatewaySet(db))
}

func (s *Server) catalogPlatformLabels() map[string]string {
	out := map[string]string{}
	entries, err := catalog.Directory()
	if err != nil {
		return out
	}
	for _, e := range entries {
		if p := directoryPlatform(e.ID, s); p != "" {
			if _, ok := out[p]; !ok {
				out[p] = e.Name
			}
		}
	}
	return out
}

// pmPerf is the live trail for one (platform, model_id): a raw request count
// and, over recent successful rows only, the mean latency and mean throughput.
type pmPerf struct {
	requests   int
	latencyMs  *int64
	throughput *float64
}

func catalogPerfByModel(ctx context.Context, db *sql.DB, since int64) map[string]pmPerf {
	out := map[string]pmPerf{}
	rows, err := db.QueryContext(ctx, `
		SELECT platform, model_id, COUNT(*),
		       AVG(CASE WHEN outcome = 'success' AND latency_ms > 0 THEN latency_ms END),
		       AVG(CASE WHEN outcome = 'success' AND latency_ms > 0 AND output_tokens > 0
		                THEN output_tokens * 1000.0 / latency_ms END)
		  FROM requests
		 WHERE created_at >= ? AND outcome <> 'canceled'
		 GROUP BY platform, model_id`, since)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var platform, modelID string
		var n int
		var latency, throughput *float64
		if err := rows.Scan(&platform, &modelID, &n, &latency, &throughput); err != nil {
			return out
		}
		p := pmPerf{requests: n}
		if latency != nil {
			p.latencyMs = new(int64(math.Round(*latency)))
		}
		if throughput != nil {
			p.throughput = new(*throughput)
		}
		out[pmKey(platform, modelID)] = p
	}
	return out
}

// platPerf is the per-platform trail for the provider directory: request count,
// success count (for the reliability posterior) and mean success latency.
type platPerf struct {
	requests  int
	successes int
	latencyMs *int64
}

func catalogPerfByPlatform(ctx context.Context, db *sql.DB, since int64) map[string]platPerf {
	out := map[string]platPerf{}
	rows, err := db.QueryContext(ctx, `
		SELECT platform, COUNT(*),
		       SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END),
		       AVG(CASE WHEN outcome = 'success' AND latency_ms > 0 THEN latency_ms END)
		  FROM requests
		 WHERE created_at >= ? AND outcome <> 'canceled'
		 GROUP BY platform`, since)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var platform string
		var n, succ int
		var latency *float64
		if err := rows.Scan(&platform, &n, &succ, &latency); err != nil {
			return out
		}
		p := platPerf{requests: n, successes: succ}
		if latency != nil {
			p.latencyMs = new(int64(math.Round(*latency)))
		}
		out[platform] = p
	}
	return out
}

func pmKey(platform, modelID string) string {
	return platform + "\x00" + modelID
}

func pct(v float64) *int {
	return new(int(math.Round(v * 100)))
}
