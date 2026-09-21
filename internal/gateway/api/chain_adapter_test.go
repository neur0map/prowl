package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// autoReq is the minimal "auto" chat request the router resolves over the
// active chain. userText seeds the first user message, which is what a sticky
// session keys on.
func autoReq(userText string) *chatRequestBody {
	return &chatRequestBody{
		Model:    "auto",
		Params:   map[string]any{},
		Messages: []map[string]any{{"role": "user", "content": userText}},
	}
}

// insertHealthyKey adds one enabled, healthy credential for a platform so a
// model can expand to more than the single key seedModel provides.
func insertHealthyKey(t *testing.T, db *sql.DB, platform string) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES (?, 'k', 'x', 'y', 'z', 'healthy', 1, ?)`, platform, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// firstRoute builds a fresh chain for one request and returns its first route.
func firstRoute(t *testing.T, s *Server, req *chatRequestBody) gateway.Route {
	t.Helper()
	chain, err := s.buildChain(context.Background(), req)
	require.NoError(t, err)
	route, err := chain.Route(0, gateway.NewSkipState())
	require.NoError(t, err)
	return route
}

// TestRouteRotatesKeysWhenNoBanditSignal proves the persisted key selection is
// applied in live routing: with several keys and no per-key history the router
// rotates fairly across requests instead of always returning the lowest key id
// (the pre-fix behaviour).
func TestRouteRotatesKeysWhenNoBanditSignal(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()

	modelID := seedModel(t, db, "groq", "m", "M", "medium") // seedModel adds one groq key
	insertHealthyKey(t, db, "groq")
	insertHealthyKey(t, db, "groq")
	seedFallback(t, db, modelID, 0, true)

	firsts := map[int64]bool{}
	for range 9 {
		firsts[firstRoute(t, s, autoReq("hi")).KeyID] = true
	}
	require.Len(t, firsts, 3,
		"rotation must spread the first-choice key across all three, not pin the lowest id")
}

// TestKeySelectionStrategyIsThreadedIntoRouting proves the exposed
// keySelectionStrategy control reaches the live routing configuration: the
// per-request chain carries the persisted strategy the key ordering reads.
func TestKeySelectionStrategyIsThreadedIntoRouting(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()
	modelID := seedModel(t, db, "groq", "m", "M", "medium")
	seedFallback(t, db, modelID, 0, true)

	chain, err := s.buildChain(context.Background(), autoReq("hi"))
	require.NoError(t, err)
	require.Equal(t, gateway.KeySelectAuto, chain.keyStrategy, "default is the per-key bandit")

	mustExec(t, s, `INSERT INTO settings(key,value,updated_at) VALUES('key_selection_strategy','least-remaining',0)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	chain, err = s.buildChain(context.Background(), autoReq("hi"))
	require.NoError(t, err)
	require.Equal(t, gateway.KeySelectLeastRemaining, chain.keyStrategy,
		"the persisted key-selection strategy must reach the live chain")
}

// TestStickyPreferenceAndUpdate proves a continuing conversation prefers its
// pinned model when still routable, that a served success updates the pin, and
// that a first turn (nothing to continue) ignores any pin.
func TestStickyPreferenceAndUpdate(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()
	// Priority order is deterministic, so the sticky pin is the only thing that
	// can move the leader.
	mustExec(t, s, `INSERT INTO settings(key,value,updated_at) VALUES('routing_strategy','priority',0)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	idA := seedModel(t, db, "groq", "ma", "MA", "medium")
	idB := seedModel(t, db, "groq", "mb", "MB", "medium")
	seedFallback(t, db, idA, 0, true) // A leads by manual priority
	seedFallback(t, db, idB, 1, true)

	ctx := context.Background()
	continuing := &chatRequestBody{Model: "auto", Params: map[string]any{}, Messages: []map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi"},
		{"role": "user", "content": "more"},
	}}

	chain, err := s.buildChain(ctx, continuing)
	require.NoError(t, err)
	require.Equal(t, idA, chain.entries[0].ModelDBID, "with no pin, priority leads with A")

	// A success served by B pins the conversation to B.
	s.recordStickySuccess(ctx, continuing, gateway.Route{ModelDBID: idB})

	chain, err = s.buildChain(ctx, continuing)
	require.NoError(t, err)
	require.Equal(t, idB, chain.entries[0].ModelDBID,
		"a continuing turn must prefer the pinned model even though priority ranks A first")

	// A brand-new conversation, keyed the same way but without an assistant
	// turn, has nothing to stick to.
	chain, err = s.buildChain(ctx, autoReq("hello"))
	require.NoError(t, err)
	require.Equal(t, idA, chain.entries[0].ModelDBID, "a first turn ignores the pin")
}

// TestStickyPinDroppedWhenModelLeavesChain proves a pin to a model that is no
// longer routable is dropped rather than forcing a disabled model.
func TestStickyPinDroppedWhenModelLeavesChain(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()
	mustExec(t, s, `INSERT INTO settings(key,value,updated_at) VALUES('routing_strategy','priority',0)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	idA := seedModel(t, db, "groq", "ma", "MA", "medium")
	idB := seedModel(t, db, "groq", "mb", "MB", "medium")
	seedFallback(t, db, idA, 0, true)
	seedFallback(t, db, idB, 1, true)

	ctx := context.Background()
	continuing := &chatRequestBody{Model: "auto", Params: map[string]any{}, Messages: []map[string]any{
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi"},
		{"role": "user", "content": "more"},
	}}
	s.recordStickySuccess(ctx, continuing, gateway.Route{ModelDBID: idB})

	// Disable B: the pin is no longer routable.
	_, err := db.Exec(`UPDATE models SET enabled = 0 WHERE id = ?`, idB)
	require.NoError(t, err)

	chain, err := s.buildChain(ctx, continuing)
	require.NoError(t, err)
	require.Equal(t, idA, chain.entries[0].ModelDBID, "a pin to a disabled model must be dropped")
}

// TestExplorationToggleGatesModelSampling proves the exposed exploration control
// gates the Thompson draw in live routing: two identical models can only be
// separated by the draw, so exploration on lets both lead while exploration off
// collapses to a deterministic order - the same suppression a degraded fleet
// imposes through the shared sampled gate.
func TestExplorationToggleGatesModelSampling(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()
	mustExec(t, s, `INSERT INTO settings(key,value,updated_at) VALUES('routing_strategy','balanced',0)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	idA := seedModel(t, db, "groq", "ma", "MA", "medium")
	idB := seedModel(t, db, "groq", "mb", "MB", "medium")
	seedFallback(t, db, idA, 0, true)
	seedFallback(t, db, idB, 1, true)

	leaders := func() map[int64]int {
		seen := map[int64]int{}
		for range 60 {
			chain, err := s.buildChain(context.Background(), autoReq("hi"))
			require.NoError(t, err)
			seen[chain.entries[0].ModelDBID]++
		}
		return seen
	}

	require.Len(t, leaders(), 2, "with exploration on, the draw's variance must let both models lead")

	mustExec(t, s, `INSERT INTO settings(key,value,updated_at) VALUES('routing_explore_enabled','false',0)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	require.Len(t, leaders(), 1, "with exploration off, ordering must be deterministic")
}

// TestFleetHealthFeedsDegradationFromRouting proves the degradation monitor is
// fed from the routing path (not only when the dashboard is read) and that a
// healthy fleet is not degraded, so exploration proceeds.
func TestFleetHealthFeedsDegradationFromRouting(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})
	db := s.engine.DB()
	seedModel(t, db, "groq", "m", "M", "medium") // one healthy provider

	snap := s.fleetHealthSnapshot(context.Background())
	require.Equal(t, 1, snap.Total)
	require.Equal(t, 1, snap.Healthy)
	require.False(t, s.fleetDegraded(context.Background()),
		"a healthy fleet must not be degraded, so exploration is allowed")
}
