package gateway

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/store"
)

// ── Test doubles ─────────────────────────────────────────────────────────────

// fakeScorer returns fixed axis inputs per model, so ordering is deterministic.
type fakeScorer struct {
	axes map[int64]Axes
}

func (f fakeScorer) Axes(e *ChainEntry, _ bool) Axes {
	if a, ok := f.axes[e.ModelDBID]; ok {
		return a
	}
	return Axes{Headroom: 1, RateLimit: 1}
}

func orderIDs(chain []ChainEntry) []int64 {
	ids := make([]int64, len(chain))
	for i := range chain {
		ids[i] = chain[i].ModelDBID
	}
	return ids
}

func sameIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ── OrderChain: match tier ───────────────────────────────────────────────────

// A slug-resolved fallback (match_tier 1) must never outrank a directly
// requested model (match_tier 0), however much better its live score.
func TestOrderChain_MatchTierDominatesScore(t *testing.T) {
	chain := []ChainEntry{
		{ModelDBID: 1, Enabled: true, MatchTier: 1, Tier: TierFrontier}, // superb score
		{ModelDBID: 2, Enabled: true, MatchTier: 0, Tier: TierSmall},    // poor score
	}
	scorer := fakeScorer{axes: map[int64]Axes{
		1: {Reliability: 1, Speed: 1, Headroom: 1, RateLimit: 1},
		2: {Reliability: 0, Speed: 0, Headroom: 1, RateLimit: 1},
	}}
	got := orderIDs(OrderChain(chain, RoutingReliable, Weights{}, false, scorer, nil))
	if !sameIDs(got, 2, 1) {
		t.Fatalf("match_tier must dominate: got %v want [2 1]", got)
	}
}

// ── OrderChain: intelligence normalisation ──────────────────────────────────

// Intelligence is normalised across the request's own candidate set, so the
// same two models can order differently depending on what else is in the chain.
func TestOrderChain_IntelligenceNormalisationIsPerRequest(t *testing.T) {
	custom := Weights{Reliability: 0.5, Intelligence: 0.5}
	// B has a reliability edge; A has the intelligence edge.
	scorer := fakeScorer{axes: map[int64]Axes{
		1: {Reliability: 0, Headroom: 1, RateLimit: 1},   // A
		2: {Reliability: 0.5, Headroom: 1, RateLimit: 1}, // B
		3: {Reliability: 0, Headroom: 1, RateLimit: 1},   // C, low tier, only changes the range
	}}
	a := ChainEntry{ModelDBID: 1, Enabled: true, Tier: TierFrontier, IntelRank: 1}
	b := ChainEntry{ModelDBID: 2, Enabled: true, Tier: TierLarge, IntelRank: 1}
	c := ChainEntry{ModelDBID: 3, Enabled: true, Tier: TierSmall, IntelRank: 1}

	// {A,B}: B's intelligence normalises to 0, so A's intelligence dominance
	// wins despite B's reliability edge.
	got := orderIDs(OrderChain([]ChainEntry{a, b}, RoutingCustom, custom, false, scorer, nil))
	if !sameIDs(got, 1, 2) {
		t.Fatalf("two-model chain: got %v want [1 2]", got)
	}
	// {A,B,C}: adding a low-tier C widens the range, lifting B's normalised
	// intelligence enough that its reliability edge now wins. Same A and B,
	// opposite outcome - proof the normalisation is per-request.
	got = orderIDs(OrderChain([]ChainEntry{a, b, c}, RoutingCustom, custom, false, scorer, nil))
	if !sameIDs(got, 2, 1, 3) {
		t.Fatalf("three-model chain: got %v want [2 1 3]", got)
	}
}

// A single spread-less chain scores intelligence at 1 throughout and orders on
// the other axes, never panicking.
func TestOrderChain_EmptyAndSingle(t *testing.T) {
	if got := OrderChain(nil, RoutingBalanced, Weights{}, true, nil, nil); len(got) != 0 {
		t.Fatalf("nil chain must return empty, got %v", got)
	}
	one := []ChainEntry{{ModelDBID: 9, Enabled: true, Tier: TierMedium}}
	got := orderIDs(OrderChain(one, RoutingBalanced, Weights{}, true, nil, nil))
	if !sameIDs(got, 9) {
		t.Fatalf("single-entry chain mangled: %v", got)
	}
}

// ── OrderChain: priority strategy + penalty ─────────────────────────────────

// Priority mode dense-ranks the manual order, then adds the penalty to the
// rank, so one penalty position is one position regardless of how the manual
// priorities are spaced.
func TestOrderChain_PriorityDenseRankPenalty(t *testing.T) {
	chain := []ChainEntry{
		{ModelDBID: 1, Priority: 10, Enabled: true},
		{ModelDBID: 2, Priority: 20, Enabled: true},
		{ModelDBID: 3, Priority: 30, Enabled: true},
	}
	p := NewPenaltyStore()
	// Without penalties the manual order stands.
	if got := orderIDs(OrderChain(chain, RoutingPriority, Weights{}, true, nil, p)); !sameIDs(got, 1, 2, 3) {
		t.Fatalf("unpenalised priority order: got %v want [1 2 3]", got)
	}
	// Penalise the middle model by 3 (a 429). Dense ranks are 1,2,3, so its
	// eff becomes 2+3=5, past model 3's eff of 3 - even though the raw
	// priorities are spaced 10 apart, where adding 3 to 20 would have left it
	// ahead of 30. That inertness is the bug the dense rank fixes.
	p.RecordRateLimitHit(2)
	if got := orderIDs(OrderChain(chain, RoutingPriority, Weights{}, true, nil, p)); !sameIDs(got, 1, 3, 2) {
		t.Fatalf("penalised priority order: got %v want [1 3 2]", got)
	}
}

// A penalised model loses its place while an unpenalised model with a
// marginally worse manual position keeps ahead.
func TestOrderChain_PenaltyDemotesPosition(t *testing.T) {
	chain := []ChainEntry{
		{ModelDBID: 1, Priority: 1, Enabled: true}, // best position
		{ModelDBID: 2, Priority: 2, Enabled: true}, // marginally worse position
	}
	p := NewPenaltyStore()
	p.RecordRateLimitHit(1) // +3 to the leader
	got := orderIDs(OrderChain(chain, RoutingPriority, Weights{}, true, nil, p))
	if !sameIDs(got, 2, 1) {
		t.Fatalf("penalised leader must drop: got %v want [2 1]", got)
	}
}

// ── PenaltyStore ────────────────────────────────────────────────────────────

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestPenalties(c *fakeClock) *PenaltyStore {
	return &PenaltyStore{now: c.now, m: map[int64]*penaltyEntry{}}
}

func TestPenaltyStore_AmountsCapAndLazyDecay(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := newTestPenalties(c)

	p.RecordRateLimitHit(1)
	if got := p.Penalty(1); got != 3 {
		t.Fatalf("429 penalty = %v want 3", got)
	}
	p.RecordModelFailure(1)
	if got := p.Penalty(1); got != 4 {
		t.Fatalf("+fail penalty = %v want 4", got)
	}

	// Lazy decay on READ: advancing the clock alone drops one unit per 2 min,
	// with no intervening record call.
	c.advance(2 * time.Minute)
	if got := p.Penalty(1); got != 3 {
		t.Fatalf("after 2m decay = %v want 3", got)
	}
	c.advance(4 * time.Minute) // 6 min total from lastHit → 3 steps off the stored 4
	if got := p.Penalty(1); got != 1 {
		t.Fatalf("after 6m decay = %v want 1", got)
	}
	// Never below zero, and the entry is dropped.
	c.advance(1 * time.Hour)
	if got := p.Penalty(1); got != 0 {
		t.Fatalf("decay floor = %v want 0", got)
	}

	// Cap at maxPenalty regardless of how many hits land in one interval.
	for range 5 {
		p.RecordRateLimitHit(2)
	}
	if got := p.Penalty(2); got != maxPenalty {
		t.Fatalf("penalty cap = %v want %v", got, maxPenalty)
	}
}

func TestPenaltyStore_SuccessRemovesOneUnit(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := newTestPenalties(c)
	p.RecordRateLimitHit(1) // 3
	p.RecordSuccess(1)
	if got := p.Penalty(1); got != 2 {
		t.Fatalf("one success should leave 2, got %v", got)
	}
	p.RecordSuccess(1)
	p.RecordSuccess(1)
	if got := p.Penalty(1); got != 0 {
		t.Fatalf("three successes should clear, got %v", got)
	}
	if got := len(p.Snapshot()); got != 0 {
		t.Fatalf("cleared model should not appear in snapshot, got %d", got)
	}
}

// Decay is anchored to the last recorded hit, never to reads: polling the score
// many times must not decay a penalty any faster than leaving it alone. If a
// read reset the clock, a busy gateway would rehabilitate models faster than an
// idle one - exactly backwards.
func TestPenaltyStore_ReadsDoNotAdvanceClock(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := newTestPenalties(c)
	p.RecordRateLimitHit(1) // 3 at t0

	// Hammer the read at t0+1min (below one decay step). Were reads to reset
	// lastHit, each of these would restart the interval.
	c.advance(1 * time.Minute)
	for range 50 {
		if got := p.Penalty(1); got != 3 {
			t.Fatalf("read below a decay step should stay 3, got %v", got)
		}
	}
	// Now at exactly t0+2min the penalty is one step down - proof the 50 reads
	// did not move the anchor forward, and equally did not reset it back.
	c.advance(1 * time.Minute)
	if got := p.Penalty(1); got != 2 {
		t.Fatalf("read at t0+2min should be 2, got %v", got)
	}
	if got := p.Penalty(1); got != 2 {
		t.Fatalf("repeated read at the same instant must be stable, got %v", got)
	}
}

func TestPenaltyStore_Snapshot(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	p := newTestPenalties(c)
	p.RecordRateLimitHit(1) // 3
	p.RecordModelFailure(2) // 1
	snap := p.Snapshot()
	if len(snap) != 2 || snap[0].ModelDBID != 1 || snap[1].ModelDBID != 2 {
		t.Fatalf("snapshot should be heaviest-first: %+v", snap)
	}
}

// ── Eligible gates ──────────────────────────────────────────────────────────

func baseChain() []ChainEntry {
	cw := int64(100000)
	tpm := int64(50000)
	return []ChainEntry{
		{ModelDBID: 1, Enabled: true, Platform: "a", SupportsVision: true, SupportsTools: true, ContextWindow: &cw, TPMLimit: &tpm},
		{ModelDBID: 2, Enabled: true, Platform: "b", SupportsVision: false, SupportsTools: false},
	}
}

func TestEligible_EachGate(t *testing.T) {
	present := func(chain []ChainEntry, id int64) bool {
		for i := range chain {
			if chain[i].ModelDBID == id {
				return true
			}
		}
		return false
	}

	// Disabled row.
	c := baseChain()
	c[0].Enabled = false
	if got := Eligible(c, RequestGate{}); present(got, 1) {
		t.Fatal("disabled row must be excluded")
	}

	// Skip model / skip platform.
	if got := Eligible(baseChain(), RequestGate{SkipModels: map[int64]bool{1: true}}); present(got, 1) {
		t.Fatal("skipped model must be excluded")
	}
	if got := Eligible(baseChain(), RequestGate{SkipPlatforms: map[string]bool{"b": true}}); present(got, 2) {
		t.Fatal("skipped platform must be excluded")
	}

	// Vision / tools requirements exclude only the incapable model.
	if got := Eligible(baseChain(), RequestGate{RequireVision: true}); present(got, 2) || !present(got, 1) {
		t.Fatalf("vision gate wrong: %v", orderIDs(got))
	}
	if got := Eligible(baseChain(), RequestGate{RequireTools: true}); present(got, 2) || !present(got, 1) {
		t.Fatalf("tools gate wrong: %v", orderIDs(got))
	}

	// Context window: a request bigger than model 1's window excludes it, while
	// model 2 (unknown window) is never filtered.
	if got := Eligible(baseChain(), RequestGate{EstimatedTokens: 200000}); present(got, 1) || !present(got, 2) {
		t.Fatalf("context gate wrong: %v", orderIDs(got))
	}
	// tpm ceiling: a request over model 1's tpm_limit but within its context
	// window still excludes it.
	if got := Eligible(baseChain(), RequestGate{EstimatedTokens: 60000}); present(got, 1) {
		t.Fatal("tpm gate must exclude a model whose per-minute cap is below the request")
	}
}

// ── Sticky sessions ─────────────────────────────────────────────────────────

func TestSessionKey(t *testing.T) {
	// A session id header wins over the message, and folds in the strategy key.
	if got := SessionKey("hi", "sess-1", "auto:smart"); got != "hdr:sess-1::auto:smart" {
		t.Fatalf("header key = %q", got)
	}
	if got := SessionKey("hi", "sess-1", ""); got != "hdr:sess-1" {
		t.Fatalf("header key without strategy = %q", got)
	}
	// No header: the message is hashed, and the strategy key changes the hash.
	a := SessionKey("first message", "", "auto")
	b := SessionKey("first message", "", "auto:smart")
	if a == "" || a == b {
		t.Fatalf("message-derived keys must differ by strategy: %q %q", a, b)
	}
	// Nothing to key on.
	if got := SessionKey("", "", "auto"); got != "" {
		t.Fatalf("empty session must key to empty, got %q", got)
	}
}

func TestStickyStore_TTL(t *testing.T) {
	c := &fakeClock{t: time.Unix(2_000_000, 0)}
	s := &StickyStore{ttl: StickyTTL, now: c.now, entries: map[string]stickyEntry{}}
	s.Set("k", 7)

	c.advance(29 * time.Minute)
	if id, ok := s.Get("k"); !ok || id != 7 {
		t.Fatalf("sticky pin should survive inside TTL, got %d %v", id, ok)
	}
	c.advance(2 * time.Minute) // 31 min total, past the 30 min TTL
	if _, ok := s.Get("k"); ok {
		t.Fatal("sticky pin should lapse after TTL")
	}
}

func TestResolveStickyPreference(t *testing.T) {
	chain := []ChainEntry{
		{ModelDBID: 5, Enabled: true},
		{ModelDBID: 6, Enabled: false},
	}
	if id, ok := ResolveStickyPreference(5, chain); !ok || id != 5 {
		t.Fatal("enabled member should resolve")
	}
	if _, ok := ResolveStickyPreference(6, chain); ok {
		t.Fatal("disabled member must not resolve")
	}
	if _, ok := ResolveStickyPreference(99, chain); ok {
		t.Fatal("non-member must not resolve")
	}
}

func TestPreferFront(t *testing.T) {
	chain := []ChainEntry{{ModelDBID: 1}, {ModelDBID: 2}, {ModelDBID: 3}}
	if got := orderIDs(PreferFront(chain, 2)); !sameIDs(got, 2, 1, 3) {
		t.Fatalf("prefer front: got %v want [2 1 3]", got)
	}
	if got := orderIDs(PreferFront(chain, 1)); !sameIDs(got, 1, 2, 3) {
		t.Fatalf("already-front must be unchanged: %v", got)
	}
	if got := orderIDs(PreferFront(chain, 99)); !sameIDs(got, 1, 2, 3) {
		t.Fatalf("non-member must be unchanged: %v", got)
	}
}

// ── ResolveChain ────────────────────────────────────────────────────────────

func openTestStore(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st.DB()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// insertModel adds a catalogue model and makes sure its platform has a usable
// key, because a chain candidate is a (model, key) pair: a model whose
// platform has no credential is not routable and is correctly absent from a
// chain.
func insertModel(t *testing.T, db *sql.DB, platform, modelID, size string, rank int) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO models(platform, model_id, display_name, intelligence_rank, size_label, enabled)
		 VALUES(?,?,?,?,?,1)`, platform, modelID, modelID, rank, size)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	id, _ := res.LastInsertId()
	insertPlatformKey(t, db, platform)
	return id
}

func insertPlatformKey(t *testing.T, db *sql.DB, platform string) {
	t.Helper()
	var exists int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE platform = ?`, platform).Scan(&exists); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if exists > 0 {
		return
	}
	if _, err := db.Exec(`
		INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES (?, 'test', 'x', 'y', 'z', 'healthy', 1, 0)`, platform); err != nil {
		t.Fatalf("insert key: %v", err)
	}
}

func TestResolveChain_ProfileBeatsFallbackBeatsAlias(t *testing.T) {
	db := openTestStore(t)
	// Three models so each source can hold a distinct membership.
	mProfile := insertModel(t, db, "p", "profile-model", "Large", 1)
	mFallback := insertModel(t, db, "f", "fallback-model", "Medium", 1)
	mExtra := insertModel(t, db, "x", "extra-model", "Small", 1)

	// Global fallback holds fallback-model + extra-model.
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,0,1)`, mFallback)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,1,1)`, mExtra)

	// A profile holds only profile-model.
	res, err := db.Exec(`INSERT INTO profiles(name, active, created_at) VALUES('coding',0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	profID, _ := res.LastInsertId()
	mustExec(t, db, `INSERT INTO profile_models(profile_id, model_db_id, position) VALUES(?,?,0)`, profID, mProfile)

	// No active profile → `auto` resolves to the fallback chain.
	rc, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatalf("auto without profile: %v", err)
	}
	if !sameIDs(orderIDs(rc.Chain), mFallback, mExtra) {
		t.Fatalf("auto without profile should be fallback: %v", orderIDs(rc.Chain))
	}

	// Activate the profile → `auto` now resolves to the profile chain,
	// beating the fallback.
	mustExec(t, db, `INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, profID)
	rc, err = ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatalf("auto with profile: %v", err)
	}
	if !sameIDs(orderIDs(rc.Chain), mProfile) {
		t.Fatalf("active profile should beat fallback: %v", orderIDs(rc.Chain))
	}

	// An alias spans the whole enabled catalog regardless of the active
	// profile, and forces its own ordering strategy.
	rc, err = ResolveChain(db, "auto:smart", RoutingBalanced)
	if err != nil {
		t.Fatalf("auto:smart: %v", err)
	}
	if rc.OrderBy != RoutingSmartest || rc.StrategyKey != "auto:smart" {
		t.Fatalf("alias should force smartest: OrderBy=%v key=%v", rc.OrderBy, rc.StrategyKey)
	}
	if len(rc.Chain) != 3 {
		t.Fatalf("alias should span the whole catalog, got %d", len(rc.Chain))
	}
}

// TestExpandChainKeysExcludesLinkedCredentials proves a linked-login credential
// never soaks up a catalogue model of its platform: a link: key is authorised
// only for the models it discovered (its own key-bound, login-scoped rows), so
// a catalogue model expands only to ordinary keys, while the login's own model
// still routes through the linked key.
func TestExpandChainKeysExcludesLinkedCredentials(t *testing.T) {
	db := openTestStore(t)

	normalRes, err := db.Exec(`INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES ('anthropic', 'byok', 'cipher', 'iv', 'tag', 'healthy', 1, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	normalKey, _ := normalRes.LastInsertId()
	linkedRes, err := db.Exec(`INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES ('anthropic', 'sub', 'link:anthropic', '', '', 'healthy', 1, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	linkedKey, _ := linkedRes.LastInsertId()

	catRes, err := db.Exec(`INSERT INTO models(platform, model_id, display_name, intelligence_rank, size_label, enabled, source, endpoint_scope)
		VALUES('anthropic','claude-opus-5','Opus',1,'Large',1,'catalog','')`)
	if err != nil {
		t.Fatal(err)
	}
	catModel, _ := catRes.LastInsertId()
	loginRes, err := db.Exec(`INSERT INTO models(platform, model_id, display_name, intelligence_rank, size_label, enabled, source, endpoint_scope, key_id)
		VALUES('anthropic','claude-mythos-5','Mythos',1,'Large',1,'login','link:anthropic',?)`, linkedKey)
	if err != nil {
		t.Fatal(err)
	}
	loginModel, _ := loginRes.LastInsertId()

	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,0,1)`, catModel)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,1,1)`, loginModel)

	rc, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	var catKeys, loginKeys []int64
	for _, e := range rc.Chain {
		if e.KeyID == nil {
			t.Fatalf("every candidate must carry a key, %s had none", e.ModelID)
		}
		switch e.ModelDBID {
		case catModel:
			catKeys = append(catKeys, *e.KeyID)
		case loginModel:
			loginKeys = append(loginKeys, *e.KeyID)
		}
	}

	if !sameIDs(catKeys, normalKey) {
		t.Fatalf("catalogue model must expand only to the ordinary key %d, got %v (linked key %d must be excluded)", normalKey, catKeys, linkedKey)
	}
	if !sameIDs(loginKeys, linkedKey) {
		t.Fatalf("login model must route through its linked key %d, got %v", linkedKey, loginKeys)
	}
}

func TestResolveChain_NamedProfileAndErrors(t *testing.T) {
	db := openTestStore(t)
	m := insertModel(t, db, "p", "m1", "Large", 1)
	res, _ := db.Exec(`INSERT INTO profiles(name, active, created_at) VALUES('coding',0,0)`)
	profID, _ := res.LastInsertId()
	mustExec(t, db, `INSERT INTO profile_models(profile_id, model_db_id, position) VALUES(?,?,0)`, profID, m)

	rc, err := ResolveChain(db, "auto:coding", RoutingReliable)
	if err != nil {
		t.Fatalf("named profile: %v", err)
	}
	if !sameIDs(orderIDs(rc.Chain), m) || rc.StrategyKey != "auto:coding" || rc.OrderBy != RoutingReliable {
		t.Fatalf("named profile resolution wrong: %v key=%v order=%v", orderIDs(rc.Chain), rc.StrategyKey, rc.OrderBy)
	}

	// Unknown profile is a clear ChainError, not a panic.
	_, err = ResolveChain(db, "auto:missing", RoutingBalanced)
	var ce *ChainError
	if !asChainError(err, &ce) || ce.Status != 400 {
		t.Fatalf("unknown profile should be a 400 ChainError, got %v", err)
	}
}

func TestResolveChain_EmptyChainsDegradeCleanly(t *testing.T) {
	db := openTestStore(t)

	// A profile with a model that is disabled: the active chain has nothing
	// enabled, which is a clear error rather than a silent catalog fallthrough.
	res, _ := db.Exec(`INSERT INTO profiles(name, active, created_at) VALUES('empty',0,0)`)
	profID, _ := res.LastInsertId()
	mustExec(t, db, `INSERT INTO settings(key, value, updated_at) VALUES('active_profile_id', ?, 0)`, profID)

	_, err := ResolveChain(db, "auto", RoutingBalanced)
	var ce *ChainError
	if !asChainError(err, &ce) || ce.Status != 400 {
		t.Fatalf("empty active profile should be a 400 ChainError, got %v", err)
	}

	// A global sort with no enabled models is also a clear error.
	_, err = ResolveChain(db, "auto:smart", RoutingBalanced)
	if !asChainError(err, &ce) {
		t.Fatalf("empty global sort should be a ChainError, got %v", err)
	}

	// A legacy install with no active profile and an empty fallback returns an
	// empty chain (no error), matching the reference's exhaustion path - and
	// ordering it does not panic.
	mustExec(t, db, `DELETE FROM settings WHERE key = 'active_profile_id'`)
	rc, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatalf("legacy empty fallback should not error: %v", err)
	}
	if got := OrderChain(rc.Chain, RoutingBalanced, Weights{}, true, nil, nil); len(got) != 0 {
		t.Fatalf("ordering an empty chain should be empty, got %v", orderIDs(got))
	}
}

// TestResolveChain_UnavailableModelIsNotACandidate proves the availability flag
// gates routing: a login model the provider retired (available = 0) keeps its
// catalogue row and chain rows for the operator's preference, but it is never a
// routing candidate - even though its own enabled flag is untouched.
func TestResolveChain_UnavailableModelIsNotACandidate(t *testing.T) {
	db := openTestStore(t)
	avail := insertModel(t, db, "p", "available-model", "Large", 1)
	retired := insertModel(t, db, "p", "retired-model", "Large", 2)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,0,1)`, avail)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,1,1)`, retired)
	// The provider retired the second model: availability cleared, the operator's
	// enabled flag left intact.
	mustExec(t, db, `UPDATE models SET available = 0 WHERE id = ?`, retired)

	rc, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatalf("resolve chain: %v", err)
	}
	ids := map[int64]bool{}
	for _, e := range rc.Chain {
		ids[e.ModelDBID] = true
	}
	if !ids[avail] {
		t.Fatalf("the available model must be a routing candidate")
	}
	if ids[retired] {
		t.Fatalf("a retired (unavailable) model must never be a routing candidate")
	}
}

func TestResolveChain_WeightOverrideAttached(t *testing.T) {
	db := openTestStore(t)
	m := insertModel(t, db, "p", "demote-me", "Large", 1)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,0,1)`, m)
	mustExec(t, db, `INSERT INTO model_overrides(model_id, weight) VALUES('demote-me', 0.5)`)
	// An out-of-range override is dropped, leaving the model at its natural score.
	insertModel(t, db, "p", "bad-override", "Large", 1)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES((SELECT id FROM models WHERE model_id='bad-override'),1,1)`)
	mustExec(t, db, `INSERT INTO model_overrides(model_id, weight) VALUES('bad-override', 9)`)

	rc, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatal(err)
	}
	var demote, bad *ChainEntry
	for i := range rc.Chain {
		switch rc.Chain[i].ModelID {
		case "demote-me":
			demote = &rc.Chain[i]
		case "bad-override":
			bad = &rc.Chain[i]
		}
	}
	if demote == nil || demote.WeightOverride == nil || *demote.WeightOverride != 0.5 {
		t.Fatalf("valid override not attached: %+v", demote)
	}
	if bad == nil || bad.WeightOverride != nil {
		t.Fatalf("out-of-range override should be dropped: %+v", bad)
	}
}

// ── OrderChain: cheapest (real price evidence) ──────────────────────────────

// auto:cheap must order by the catalogue's published price, not fall through to
// Balanced. The smartest candidate is the most expensive, so a Balanced-shaped
// ordering would lead with it; a price ordering must lead with the cheap one,
// and a candidate with no published price must sort last - an unknown price is
// not evidence of being cheap and must never outrank a known cheaper route.
func TestOrderChain_CheapestOrdersByPriceNotBalanced(t *testing.T) {
	price := func(v float64) *float64 { return &v }
	chain := []ChainEntry{
		{ModelDBID: 1, Enabled: true, Tier: TierFrontier, IntelRank: 1,
			PaidInputPerM: price(10), PaidOutputPerM: price(30), Priority: 0},
		{ModelDBID: 2, Enabled: true, Tier: TierSmall, IntelRank: 9,
			PaidInputPerM: price(1), PaidOutputPerM: price(3), Priority: 1},
		{ModelDBID: 3, Enabled: true, Tier: TierFrontier, IntelRank: 1, Priority: 2},
	}

	// Precondition: Balanced leads with the frontier model on intelligence.
	if got := orderIDs(OrderChain(chain, RoutingBalanced, Weights{}, false, nil, nil)); got[0] != 1 {
		t.Fatalf("precondition: balanced should lead with the frontier model, got %v", got)
	}
	// Cheapest leads with the least expensive KNOWN route; unknown price last.
	got := orderIDs(OrderChain(chain, RoutingCheapest, Weights{}, false, nil, nil))
	if !sameIDs(got, 2, 1, 3) {
		t.Fatalf("cheapest must order by price with unknown last: got %v want [2 1 3]", got)
	}
}

// Equal prices (and equally-unknown prices) fall back to the manual chain
// priority, and every priced route still precedes every unpriced one.
func TestOrderChain_CheapestPriorityBreaksTies(t *testing.T) {
	price := func(v float64) *float64 { return &v }
	chain := []ChainEntry{
		{ModelDBID: 1, Enabled: true, PaidInputPerM: price(2), PaidOutputPerM: price(2), Priority: 5},
		{ModelDBID: 2, Enabled: true, PaidInputPerM: price(2), PaidOutputPerM: price(2), Priority: 1},
		{ModelDBID: 3, Enabled: true, Priority: 9}, // unknown price
		{ModelDBID: 4, Enabled: true, Priority: 4}, // unknown price
	}
	got := orderIDs(OrderChain(chain, RoutingCheapest, Weights{}, false, nil, nil))
	if !sameIDs(got, 2, 1, 4, 3) {
		t.Fatalf("cheapest ties break on priority, unknown after known: got %v want [2 1 4 3]", got)
	}
}

// Every cheap spelling - cheap, cheapest, price, budget - must resolve to the
// price strategy, not silently to Balanced.
func TestResolveChain_CheapAliasesOrderByPrice(t *testing.T) {
	db := openTestStore(t)
	insertModel(t, db, "p", "m1", "Large", 1)

	for _, alias := range []string{"auto:cheap", "auto:cheapest", "auto:price", "auto:budget"} {
		rc, err := ResolveChain(db, alias, RoutingBalanced)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if rc.OrderBy != RoutingCheapest {
			t.Fatalf("%s must order by price, got OrderBy=%v (regressed to balanced?)", alias, rc.OrderBy)
		}
		if rc.StrategyKey != "auto:cheap" {
			t.Fatalf("%s should canonicalise to auto:cheap, got %v", alias, rc.StrategyKey)
		}
	}
}

// ── ResolveChain: concrete-model preference ─────────────────────────────────

// A request that names a concrete model the gateway advertises must place that
// model first, ahead of the active chain's strategy, while keeping the rest of
// the chain for failover. The requested model here is the LESS capable one, so
// only the preferred-model wiring - not the Balanced strategy - can lead with it.
func TestResolveChain_ConcreteModelPreferredAheadOfStrategy(t *testing.T) {
	db := openTestStore(t)
	frontier := insertModel(t, db, "openai", "frontier", "Frontier", 1)
	small := insertModel(t, db, "openai", "small", "Small", 1)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,0,1)`, frontier)
	mustExec(t, db, `INSERT INTO fallback_config(model_db_id, position, enabled) VALUES(?,1,1)`, small)

	// Baseline: "auto" leaves the frontier model leading under Balanced.
	auto, err := ResolveChain(db, "auto", RoutingBalanced)
	if err != nil {
		t.Fatal(err)
	}
	if got := orderIDs(OrderChain(auto.Chain, RoutingBalanced, Weights{}, false, nil, nil)); got[0] != frontier {
		t.Fatalf("baseline: balanced should lead with the frontier model, got %v", got)
	}

	// Requesting the concrete small model - by bare id and platform-qualified -
	// must lead with it and retain the frontier model behind it for failover.
	for _, name := range []string{"small", "openai/small"} {
		rc, err := ResolveChain(db, name, RoutingBalanced)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := orderIDs(OrderChain(rc.Chain, RoutingBalanced, Weights{}, false, nil, nil))
		if !sameIDs(got, small, frontier) {
			t.Fatalf("%s must lead and keep failover: got %v want [%d %d]", name, got, small, frontier)
		}
	}
}

// preferConcreteModel tiers only when the request names a member, and never for
// "", "auto", or a non-member - those route the active chain untouched.
func TestPreferConcreteModel_TiersOnlyNamedMember(t *testing.T) {
	base := func() []ChainEntry {
		return []ChainEntry{
			{ModelDBID: 1, Platform: "openai", ModelID: "gpt-4o"},
			{ModelDBID: 2, Platform: "anthropic", ModelID: "claude"},
			{ModelDBID: 3, Platform: "openai", ModelID: "gpt-4o"}, // a second key of the same model
		}
	}
	// A named member: all its candidates stay tier 0, the rest are demoted.
	c := base()
	preferConcreteModel(c, "gpt-4o")
	if c[0].MatchTier != 0 || c[2].MatchTier != 0 || c[1].MatchTier != 1 {
		t.Fatalf("requested model must stay tier 0, others demoted: %d %d %d", c[0].MatchTier, c[1].MatchTier, c[2].MatchTier)
	}
	// The platform-qualified spelling matches too.
	c = base()
	preferConcreteModel(c, "anthropic/claude")
	if c[1].MatchTier != 0 || c[0].MatchTier != 1 || c[2].MatchTier != 1 {
		t.Fatalf("qualified name must match: %d %d %d", c[0].MatchTier, c[1].MatchTier, c[2].MatchTier)
	}
	// "", "auto" and non-members leave every tier untouched.
	for _, req := range []string{"", "auto", "unknown-model"} {
		c = base()
		preferConcreteModel(c, req)
		if c[0].MatchTier != 0 || c[1].MatchTier != 0 || c[2].MatchTier != 0 {
			t.Fatalf("%q must not tier anything: %d %d %d", req, c[0].MatchTier, c[1].MatchTier, c[2].MatchTier)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func asChainError(err error, target **ChainError) bool {
	if err == nil {
		return false
	}
	ce, ok := err.(*ChainError)
	if ok {
		*target = ce
	}
	return ok
}
