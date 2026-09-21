package api

import (
	"context"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// The chain adapter binds the ported chain resolution to one request.
//
// The failover loop asks only for "the next route, honouring what I have
// already skipped". Everything behind that question - resolving the model
// string, scoring the candidates, gating them, and choosing among a model's
// keys - lives here, so the loop stays a control structure rather than a
// router.

// buildChain resolves this request's candidates into the loop's view of them.
func (s *Server) buildChain(ctx context.Context, req *chatRequestBody) (*requestChain, error) {
	resolved, err := gateway.ResolveChain(s.engine.DB(), req.Model, s.routingStrategy(ctx))
	if err != nil {
		return nil, err
	}
	if len(resolved.Chain) == 0 {
		// Name the actual gap. "No keys configured" is wrong whenever the
		// operator has keys but none of them serves what the list points at,
		// which is the usual way a hand-built or subscription list ends up
		// empty.
		return nil, &gateway.ChainError{
			Status:  503,
			Message: gateway.ExplainUnroutableChain(s.engine.DB(), resolved.StrategyKey),
		}
	}
	if len(req.excludePlatforms) > 0 {
		filtered := make([]gateway.ChainEntry, 0, len(resolved.Chain))
		for _, entry := range resolved.Chain {
			if !req.excludePlatforms[entry.Platform] {
				filtered = append(filtered, entry)
			}
		}
		resolved.Chain = filtered
		if len(resolved.Chain) == 0 {
			return nil, &gateway.ChainError{
				Status:  503,
				Message: "no eligible models remain after research handoff",
			}
		}
	}

	estimatedTokens := estimateTokens(req)
	profile := gateway.ClassifyPrompt(req.Messages, req.Params)
	scorer := s.newAxisScorer(ctx)

	// Exploration is the operator's toggle AND live fleet health folded into
	// one knob: a degraded fleet suppresses the Thompson draw so the retry
	// budget is not spent proving what an outage already made obvious, and so
	// models that were never at fault keep their reliability posteriors. Both
	// feed the SAME ordering input (scorer.Axes sampled), so the control the
	// dashboard advertises is the control the router obeys.
	sampled := routingExploreEnabled(s.engine.DB()) && !s.fleetDegraded(ctx)

	var (
		ordered     []gateway.ChainEntry
		smartScores map[int64]gateway.SmartScore
	)
	if resolved.OrderBy == gateway.RoutingSmartest {
		benchmarks := gateway.LoadBenchmarkScores(ctx, s.engine.DB(), resolved.Chain)
		ordered, smartScores = gateway.OrderSmartChain(
			resolved.Chain, profile, benchmarks, sampled, scorer)
	} else {
		// Resolve the weight vector once - the preset for a named strategy, the
		// operator's saved vector for custom - then apply the peak-hour shift so
		// both the custom sliders AND the peak window reach live routing rather
		// than only the dashboard. Fastest/reliable are exempt and priority /
		// cheapest ignore the vector, so this only moves balanced and custom.
		base := gateway.WeightsFor(resolved.OrderBy, routingCustomWeights(s.engine.DB()))
		weights, _ := gateway.PeakAdjustedWeights(base, resolved.OrderBy, s.peakHoursConfig(), time.Now())
		ordered = gateway.OrderChainWeighted(
			resolved.Chain, resolved.OrderBy, weights, sampled, scorer, s.engine.Penalties())
	}

	// A continuing conversation keeps the model it started on while that model
	// is still routable, so a mid-conversation failover does not hand the next
	// turn to a model with no idea it is continuing someone else's work.
	if pinned, ok := s.stickyPreference(req, ordered); ok {
		ordered = gateway.PreferFront(ordered, pinned)
	}

	return &requestChain{
		server:      s,
		entries:     ordered,
		profile:     profile,
		strategy:    resolved.OrderBy,
		keyStrategy: gateway.ParseKeySelection(routingKeySelection(s.engine.DB())),
		smartScores: smartScores,
		gate: gateway.RequestGate{
			EstimatedTokens: estimatedTokens,
			RequireVision:   requestHasImages(req),
			RequireTools:    profile.UsesTools,
		},
		rng: rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9e3779b9)),
	}, nil
}

// requestChain is the per-request chain the loop walks. Ordering and skip
// state are both request-scoped, so nothing here is shared between requests.
type requestChain struct {
	server      *Server
	entries     []gateway.ChainEntry
	gate        gateway.RequestGate
	profile     gateway.PromptProfile
	strategy    gateway.RoutingStrategy
	keyStrategy gateway.KeySelectionStrategy
	smartScores map[int64]gateway.SmartScore
	rng         *rand.Rand

	// rrBase caches this request's round-robin rotation base per model, so the
	// process-lived key cursor advances once per request rather than once per
	// failover retry, and a request's retries keep a stable rotation.
	rrBase map[int64]int

	mu sync.Mutex
}

// Route returns the next candidate that passes this request's gates, is not
// already skipped, and is not benched. Model order comes from the bandit;
// among a chosen model's several keys the operator's key-selection strategy
// decides which credential leads, so a model spreads load and prefers its
// healthiest key instead of always reaching for the lowest row id.
func (c *requestChain) Route(_ int, skip *gateway.SkipState) (gateway.Route, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// SkipState holds presence sets while RequestGate takes bool maps, so
	// convert once per call rather than once per candidate.
	gate := c.gate
	gate.SkipModels = make(map[int64]bool, len(skip.Models))
	for id := range skip.Models {
		gate.SkipModels[id] = true
	}
	gate.SkipPlatforms = make(map[string]bool, len(skip.Platforms))
	for name := range skip.Platforms {
		gate.SkipPlatforms[name] = true
	}

	eligible := gateway.Eligible(c.entries, gate)

	// Walk models in bandit order, each processed once on first sight over all
	// of its keys. Custom relays that share a model id are distinct model rows,
	// so grouping by model_db_id keeps their key pools isolated.
	seen := map[int64]bool{}
	for i := range eligible {
		if eligible[i].KeyID == nil || seen[eligible[i].ModelDBID] {
			continue
		}
		modelDBID := eligible[i].ModelDBID
		seen[modelDBID] = true

		group := make([]gateway.ChainEntry, 0, 4)
		for j := range eligible {
			if eligible[j].ModelDBID == modelDBID && eligible[j].KeyID != nil {
				group = append(group, eligible[j])
			}
		}
		if route, ok := c.selectKey(group, skip); ok {
			return route, nil
		}
	}

	return gateway.Route{}, &gateway.RouteError{
		Status:  503,
		Message: "every candidate is skipped, benched, or out of quota",
	}
}

// selectKey chooses one credential for a single model's key group, honouring
// the persisted key-selection strategy: the per-key Thompson bandit when any
// key has history, a fair round-robin rotation otherwise, and least-remaining
// quota weighting layered on top when the operator asked for it and the
// platform meters its keys separately. A key already failed this request is
// dropped before the ranking; a benched key is skipped during the walk.
func (c *requestChain) selectKey(group []gateway.ChainEntry, skip *gateway.SkipState) (gateway.Route, bool) {
	platform := group[0].Platform
	modelID := group[0].ModelID
	modelDBID := group[0].ModelDBID

	entryByKey := make(map[int64]gateway.ChainEntry, len(group))
	cands := make([]gateway.KeyCandidate, 0, len(group))
	for i := range group {
		keyID := *group[i].KeyID
		if _, dup := entryByKey[keyID]; dup {
			continue
		}
		if _, gone := skip.Keys[gateway.RouteKey{Platform: platform, ModelID: modelID, KeyID: keyID}]; gone {
			continue
		}
		entryByKey[keyID] = group[i]
		st, has := c.server.stats.forKey(platform, modelID, keyID)
		ttfb := -1.0
		if st.hasTTFB {
			ttfb = st.ttfbMs
		}
		cands = append(cands, gateway.KeyCandidate{
			ID:        keyID,
			Successes: st.successes,
			Failures:  st.failures,
			TokPerSec: st.tokPerSec,
			TTFBMs:    ttfb,
			HasStats:  has,
		})
	}
	if len(cands) == 0 {
		return gateway.Route{}, false
	}
	// Rotate over a canonical key order. Model-level ordering may randomise a
	// model's key-entries (the Thompson draw explores across requests), but the
	// round-robin base is only fair over a stable list: without this, the same
	// base indexes a different key each request and rotation neither spreads
	// load nor is reproducible. Sorting by id gives every request the same key
	// order for the base to rotate; the bandit path re-ranks and only inherits
	// this as its tie-break.
	sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })

	ordered := gateway.OrderKeysForWalk(cands, c.keyStrategy, platform, modelID,
		c.rotationBase(modelDBID), c.server.engine.Ledger(), c.rng)

	for i := range ordered {
		keyID := ordered[i].ID
		if _, benched := c.server.engine.Cooldowns().Active(
			gateway.QuotaKey(platform, modelID, keyID)); benched {
			continue
		}
		e := entryByKey[keyID]
		return gateway.Route{
			Platform:          platform,
			ModelID:           modelID,
			ModelDBID:         modelDBID,
			KeyID:             keyID,
			BaseURL:           c.server.keyBaseURL(keyID),
			EndpointScope:     e.EndpointScope,
			RPMLimit:          e.RPMLimit,
			RPDLimit:          e.RPDLimit,
			TPMLimit:          e.TPMLimit,
			TPDLimit:          e.TPDLimit,
			TokenMultiplier:   navyTokenMultiplier(platform, e),
			SupportsReasoning: e.SupportsReasoning,
		}, true
	}
	return gateway.Route{}, false
}

// navyTokenMultiplier derives the billed-token multiplier the ledger applies
// against a platform's account-wide token cap. It is one-to-one everywhere
// except a platform that meters a single shared token pool (NavyAI), where a
// model's provider-visible drain can exceed its OpenAI usage total; there the
// per-model rate (an explicit "2x" budget label, or the ratio of the account
// cap to the model's own daily limit) must reach admission or a heavy model
// silently overruns the pool. A platform with no account cap has no pool to
// meter, so the multiplier is left at one.
func navyTokenMultiplier(platform string, e gateway.ChainEntry) float64 {
	dailyCap, ok := gateway.ProviderDailyTokenCap(platform)
	if !ok {
		return 1
	}
	return gateway.NavyTokenMultiplier(e.MonthlyBudget, derefLimit(e.TPDLimit), dailyCap)
}

// rotationBase returns this request's round-robin base for a model, advancing
// the process-lived key cursor once per request and caching it so failover
// retries within the request keep a stable rotation.
func (c *requestChain) rotationBase(modelDBID int64) int {
	if c.rrBase == nil {
		c.rrBase = map[int64]int{}
	}
	if b, ok := c.rrBase[modelDBID]; ok {
		return b
	}
	b := c.server.keyRotation.next(strconv.FormatInt(modelDBID, 10))
	c.rrBase[modelDBID] = b
	return b
}

// RoutableKeys is the full key set a model-level bench must span, ignoring the
// transient gates: benching a model on only the key that just failed would
// leave the others to hit the same broken model.
func (c *requestChain) RoutableKeys(modelDBID int64) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[int64]bool{}
	var out []int64
	for _, entry := range c.entries {
		if entry.ModelDBID != modelDBID || entry.KeyID == nil || seen[*entry.KeyID] {
			continue
		}
		seen[*entry.KeyID] = true
		out = append(out, *entry.KeyID)
	}
	return out
}

// HasOtherUsableKey gates the model penalty. A sibling key that can still
// serve this model means the model is not what failed, so demoting it would
// punish the wrong thing.
func (c *requestChain) HasOtherUsableKey(modelDBID, failedKeyID int64, skipped map[gateway.RouteKey]struct{}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, entry := range c.entries {
		if entry.ModelDBID != modelDBID || entry.KeyID == nil || !entry.Enabled {
			continue
		}
		keyID := *entry.KeyID
		if keyID == failedKeyID {
			continue
		}
		if _, gone := skipped[gateway.RouteKey{
			Platform: entry.Platform, ModelID: entry.ModelID, KeyID: keyID,
		}]; gone {
			continue
		}
		if _, benched := c.server.engine.Cooldowns().Active(
			gateway.QuotaKey(entry.Platform, entry.ModelID, keyID)); benched {
			continue
		}
		return true
	}
	return false
}

// routingStrategy reads the operator's choice. A malformed stored value falls
// back to the default rather than failing the request: the setting is not
// worth a 500.
func (s *Server) routingStrategy(ctx context.Context) gateway.RoutingStrategy {
	var raw string
	if err := s.engine.DB().QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'routing_strategy'`).Scan(&raw); err != nil {
		return gateway.DefaultRoutingStrategy
	}
	return gateway.ParseRoutingStrategy(raw)
}

// requestHasImages reports whether any message carries image content, which is
// what gates vision-incapable models out of the chain.
func requestHasImages(req *chatRequestBody) bool {
	for _, message := range req.Messages {
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := m["type"].(string); kind == "image_url" || kind == "image" {
				return true
			}
		}
	}
	return false
}

// forKey returns the per-(platform, model, key) evidence and whether the stats
// cache holds any entry for that exact credential. Unlike forEntry it never
// falls back to the model aggregate: per-key selection must tell a key with no
// history of its own apart from the model's pooled record, so a fresh key keeps
// sampling from its prior instead of inheriting a sibling's rank.
func (c *statsCache) forKey(platform, modelID string, keyID int64) (routeStats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.byRoute[gateway.RouteKey{Platform: platform, ModelID: modelID, KeyID: keyID}]
	return st, ok
}

// fleetHealthSnapshot counts configured providers and how many are usable right
// now. An enabled provider is a platform with at least one key; a usable one has
// at least one enabled key not parked at error (unknown counts as usable - "not
// yet probed" is not evidence of failure). It is the single source the
// degradation monitor is fed from, on the routing path and the dashboard alike.
func (s *Server) fleetHealthSnapshot(ctx context.Context) gateway.HealthSnapshot {
	total, healthy := 0, 0
	rows, err := s.engine.DB().QueryContext(ctx, `
		SELECT SUM(CASE WHEN enabled = 1 AND status IN ('healthy','unknown') THEN 1 ELSE 0 END)
		  FROM api_keys GROUP BY platform`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var usable int
			if err := rows.Scan(&usable); err != nil {
				continue
			}
			total++
			if usable > 0 {
				healthy++
			}
		}
	}
	return gateway.HealthSnapshot{Total: total, Healthy: healthy}
}

// fleetDegraded feeds the monitor a live snapshot on the routing path and
// reports the resulting mode. Observing here, on every request, keeps degraded
// mode current between the occasional dashboard reads: the monitor's hysteresis
// has to be told about the fleet to notice an outage, and the request path is
// where an outage actually bites.
func (s *Server) fleetDegraded(ctx context.Context) bool {
	return s.engine.Degradation().Observe(s.fleetHealthSnapshot(ctx)) == gateway.StateDegraded
}

// stickyPreference resolves this request's sticky pin to a still-routable model.
// It applies only to a continuing conversation - a first turn has nothing to
// stick to - and drops a pin whose model has left the chain or been disabled,
// so a stale pin never overrides live routing.
func (s *Server) stickyPreference(req *chatRequestBody, chain []gateway.ChainEntry) (int64, bool) {
	if !hasAssistantTurn(req.Messages) {
		return 0, false
	}
	key := s.sessionKeyFor(req)
	if key == "" {
		return 0, false
	}
	pinned, ok := s.engine.Sticky().Get(key)
	if !ok {
		return 0, false
	}
	return gateway.ResolveStickyPreference(pinned, chain)
}

// recordStickySuccess pins the model that actually served this request to its
// conversation, so the next turn prefers it. It runs on the dispatcher's
// success path - the only point that knows which model answered, since failover
// may have moved off the preferred one. The session key is re-derived from the
// request under the same namespace the preference was read.
func (s *Server) recordStickySuccess(_ context.Context, req *chatRequestBody, route gateway.Route) {
	key := s.sessionKeyFor(req)
	if key == "" {
		return
	}
	s.engine.Sticky().Set(key, route.ModelDBID)
}

// sessionKeyFor derives the sticky-session key for a request. A client-supplied
// x-session-id takes precedence - a chat client that pins one keeps its
// conversation on one model even when the first user message is empty or shifts
// - and the key otherwise falls to the SHA-1 of the first user message,
// namespaced by the routing intent so the same conversation under "auto" and
// "auto:smart" does not share a pin (getSessionKey).
func (s *Server) sessionKeyFor(req *chatRequestBody) string {
	return gateway.SessionKey(firstUserText(req.Messages), req.sessionID, gateway.StrategyKeyFor(req.Model))
}

// firstUserText returns the text of the first user message, flattening the
// OpenAI content-parts array to its text so a multimodal first turn still keys a
// stable session.
func firstUserText(messages []map[string]any) string {
	for _, m := range messages {
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		return messageText(m["content"])
	}
	return ""
}

// hasAssistantTurn reports whether the conversation already carries an assistant
// message, the precondition for applying a sticky pin: without a prior turn
// there is nothing to continue.
func hasAssistantTurn(messages []map[string]any) bool {
	for _, m := range messages {
		if role, _ := m["role"].(string); role == "assistant" {
			return true
		}
	}
	return false
}

// messageText flattens a message's content to plain text, accepting both the
// bare string form and the content-parts array.
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := pm["type"].(string); kind != "text" {
				continue
			}
			if text, ok := pm["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// keyRotator is the process-lived round-robin cursor per model's key pool. It
// gives fair rotation across requests when the per-key bandit has no signal to
// rank on, so a model with several unmeasured keys spreads load instead of
// always leading with the lowest row id. The zero value is usable.
type keyRotator struct {
	mu sync.Mutex
	n  map[string]int
}

// next returns the current cursor for a pool and advances it.
func (r *keyRotator) next(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	v := r.n[key]
	r.n[key] = v + 1
	return v
}
