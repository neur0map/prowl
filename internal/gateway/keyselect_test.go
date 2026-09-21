package gateway

import (
	"math/rand/v2"
	"testing"
)

type fakeQuota struct {
	headroom map[int64]float64
	account  bool
}

func (f fakeQuota) KeyHeadroom(string) map[int64]float64 { return f.headroom }
func (f fakeQuota) AccountScoped(_, _ string) bool       { return f.account }

func keyIDs(keys []KeyCandidate) []int64 {
	ids := make([]int64, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
	}
	return ids
}

func eqIDs(a []int64, b ...int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The per-key bandit must rank by score once any key has data, and fall back to
// round-robin (ranked=false) when there is no signal to rank on.
func TestOrderKeysByScore_FallbackConditions(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))

	// Fewer than two keys: nothing to rank.
	if _, ok := OrderKeysByScore([]KeyCandidate{{ID: 1, HasStats: true}}, rng); ok {
		t.Fatal("single key should not be ranked")
	}
	// Two keys, neither observed: keep the rotation.
	if _, ok := OrderKeysByScore([]KeyCandidate{{ID: 1}, {ID: 2}}, rng); ok {
		t.Fatal("no key with data should not be ranked")
	}
	// Two keys, one observed: rank (D2 - two-of-three with data still ranks).
	if _, ok := OrderKeysByScore([]KeyCandidate{{ID: 1, HasStats: true}, {ID: 2}}, rng); !ok {
		t.Fatal("one key with data should rank")
	}
}

// Thompson sampling explores: a fresh key (Beta(1,1)) wins first place
// sometimes, so it earns the data that would settle its rank - but it does not
// win outright, a key with a strong history is preferred most of the time.
func TestOrderKeysByScore_ExploresWithoutPreferring(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	const trials = 4000
	freshWins, strongWins := 0, 0
	for range trials {
		ordered, ok := OrderKeysByScore([]KeyCandidate{
			{ID: 1, HasStats: true, TTFBMs: -1},                // fresh, no history
			{ID: 2, HasStats: true, Successes: 50, TTFBMs: -1}, // strong history
		}, rng)
		if !ok {
			t.Fatal("expected ranking with observed keys")
		}
		switch ordered[0].ID {
		case 1:
			freshWins++
		case 2:
			strongWins++
		}
	}
	if freshWins == 0 {
		t.Fatal("fresh key never explored - Thompson sampling not exploring")
	}
	if freshWins >= strongWins {
		t.Fatalf("fresh key preferred outright: fresh=%d strong=%d", freshWins, strongWins)
	}
	if freshWins > trials/2 {
		t.Fatalf("fresh key won too often (%d/%d)", freshWins, trials)
	}
}

// least-remaining ranks the key with the most quota left first; an unobserved
// key sorts at the neutral midpoint, and equal-headroom keys keep their order.
func TestOrderKeysByRemainingQuota(t *testing.T) {
	keys := []KeyCandidate{{ID: 1}, {ID: 2}, {ID: 3}}
	// key1 nearly exhausted, key2 roomy, key3 never observed (→ 0.5).
	headroom := map[int64]float64{1: 0.2, 2: 0.9}
	got := keyIDs(OrderKeysByRemainingQuota(keys, headroom))
	if !eqIDs(got, 2, 3, 1) {
		t.Fatalf("roomiest-first order wrong: got %v want [2 3 1]", got)
	}
	// No observations at all: the list is returned untouched.
	same := keyIDs(OrderKeysByRemainingQuota(keys, nil))
	if !eqIDs(same, 1, 2, 3) {
		t.Fatalf("empty headroom must not reorder: got %v", same)
	}
}

// OrderKeys layers least-remaining on top of the round-robin rotation when
// there is no bandit data, and skips the weighting entirely for an
// account-scoped pool where per-key remaining is meaningless.
func TestOrderKeys_LeastRemaining(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	keys := []KeyCandidate{{ID: 1}, {ID: 2}, {ID: 3}} // no stats → no bandit ranking
	qh := fakeQuota{headroom: map[int64]float64{1: 0.1, 2: 0.8, 3: 0.5}}

	ordered, ok := OrderKeys(keys, KeySelectLeastRemaining, "groq", "m", 0, qh, rng)
	if !ok {
		t.Fatal("least-remaining should produce a usable order")
	}
	if got := keyIDs(ordered); !eqIDs(got, 2, 3, 1) {
		t.Fatalf("least-remaining order wrong: got %v want [2 3 1]", got)
	}

	// Account-scoped pool: weighting does not apply, caller keeps round-robin.
	acct := fakeQuota{headroom: map[int64]float64{1: 0.1, 2: 0.8}, account: true}
	if _, ok := OrderKeys(keys, KeySelectLeastRemaining, "google", "m", 0, acct, rng); ok {
		t.Fatal("account-scoped pool must not trigger quota weighting")
	}
	// auto strategy never applies quota weighting.
	if QuotaWeightingApplies(KeySelectAuto, "groq", "m", qh) {
		t.Fatal("auto strategy must not apply quota weighting")
	}
}

// EligibleKeys drops a key scoped away from the model and a key already failed
// this request, and keeps everything else.
func TestEligibleKeys(t *testing.T) {
	keys := []KeyCandidate{
		{ID: 1},                        // unscoped
		{ID: 2, Scope: []string{"m1"}}, // scoped to the requested model
		{ID: 3, Scope: []string{"m2"}}, // scoped to another model
	}
	got := keyIDs(EligibleKeys(keys, "m1", nil))
	if !eqIDs(got, 1, 2) {
		t.Fatalf("scope gate wrong: got %v want [1 2]", got)
	}
	got = keyIDs(EligibleKeys(keys, "m1", map[int64]bool{2: true}))
	if !eqIDs(got, 1) {
		t.Fatalf("skip gate wrong: got %v want [1]", got)
	}
}

func TestScopeAllows(t *testing.T) {
	if !ScopeAllows(nil, "anything") {
		t.Fatal("unscoped key must serve any model")
	}
	if !ScopeAllows([]string{"a", "b"}, "b") {
		t.Fatal("scoped key must serve a listed model")
	}
	if ScopeAllows([]string{"a", "b"}, "c") {
		t.Fatal("scoped key must not serve an unlisted model")
	}
}

func TestParseModelScope(t *testing.T) {
	if ParseModelScope("") != nil {
		t.Fatal("empty is unscoped")
	}
	if ParseModelScope("[]") != nil {
		t.Fatal("empty array is unscoped")
	}
	if ParseModelScope("not json") != nil {
		t.Fatal("corrupt is unscoped")
	}
	got := ParseModelScope(`["a","","b"]`)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected [a b], got %v", got)
	}
}

func TestSkipKey(t *testing.T) {
	if got := SkipKey("groq", "llama-3", 5); got != "groq:llama-3:5" {
		t.Fatalf("SkipKey = %q", got)
	}
}

func TestParseKeySelection(t *testing.T) {
	if ParseKeySelection("least-remaining") != KeySelectLeastRemaining {
		t.Fatal("valid strategy not parsed")
	}
	if ParseKeySelection("nonsense") != DefaultKeySelection {
		t.Fatal("unknown strategy must default")
	}
}
