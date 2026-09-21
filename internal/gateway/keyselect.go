package gateway

// Per-key selection, ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md).
//
// Once the model bandit (chain.go) has picked a model, a second, independent
// decision remains: which of that platform's several credentials to reach for.
// It is deliberately separate from the routing strategy (router.ts:505-509) -
// the strategy enum drives the MODEL bandit, so folding a key policy into it
// would make choosing a key policy also throw away the model ranking. Nothing
// here reads a model score, and nothing in chain.go reads a key.

import (
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strconv"
)

// KeySelectionStrategy chooses among a platform's keys after a model is picked.
type KeySelectionStrategy string

const (
	// KeySelectAuto is the per-key bandit: Thompson-sampled score when any key
	// has data, round-robin otherwise.
	KeySelectAuto KeySelectionStrategy = "auto"
	// KeySelectLeastRemaining additionally ranks by observed remaining quota,
	// roomiest key first, so the key nearest its cap is tried last.
	KeySelectLeastRemaining KeySelectionStrategy = "least-remaining"
)

// DefaultKeySelection matches the upstream default (router.ts:517).
const DefaultKeySelection = KeySelectAuto

// ParseKeySelection validates a persisted key-selection string, defaulting for
// anything unrecognised (router.ts:519-524).
func ParseKeySelection(raw string) KeySelectionStrategy {
	switch s := KeySelectionStrategy(raw); s {
	case KeySelectAuto, KeySelectLeastRemaining:
		return s
	default:
		return DefaultKeySelection
	}
}

// KeyCandidate is one credential eligible for a model, carrying the per-key
// stats the bandit needs (a slice of the same decay-weighted window the model
// bandit uses, observed through this one credential - KeyStats,
// router.ts:727-732) and its model scope. HasStats is whether the stats cache
// has an entry for this key at all: it can be true with zero successes and zero
// failures, which still counts as data for the round-robin fallback decision.
type KeyCandidate struct {
	ID int64
	// Scope is the key's model allow-list (parsed model_scope_json); nil/empty
	// means unscoped - the key serves every model of its platform.
	Scope []string

	Successes float64
	Failures  float64
	TokPerSec float64
	// TTFBMs is negative when no first-byte sample exists.
	TTFBMs   float64
	HasStats bool
}

// KEY_SCORE_WEIGHTS (router.ts:1329): reliability dominates because the point of
// per-key stats is catching a credential that FAILS (expired, drained,
// region-blocked); speed differences between keys of the same platform+model
// are second-order but still worth a nudge.
const (
	keyScoreReliabilityWeight = 0.75
	keyScoreSpeedWeight       = 0.25
)

// unknownQuotaHeadroom is the headroom assumed for a key the quota tracker has
// never seen (UNKNOWN_QUOTA_HEADROOM, router.ts:1363). Neutral on purpose: an
// unobserved budget is no reason to prefer a key (it could be drained) and none
// to avoid one (it could be untouched), so it sorts between an exhausted key and
// a fresh one.
const unknownQuotaHeadroom = 0.5

// QuotaHeadroom is the slice of the quota ledger that least-remaining key
// selection needs. The quota ledger satisfies it; this slice never imports the
// concrete type, so the two concerns stay decoupled.
type QuotaHeadroom interface {
	// KeyHeadroom reports the observed remaining-quota fraction in [0,1] for
	// each key of a platform, keyed by api_keys.id. An absent key means "never
	// observed"; the caller substitutes unknownQuotaHeadroom.
	KeyHeadroom(platform string) map[int64]float64
	// AccountScoped reports whether the platform's quota pool for a model is one
	// budget every key of the account draws down. When it is, "which key has
	// more left" has no answer and least-remaining weighting is skipped
	// (inferQuotaPoolKey(...).endsWith("::account"), router.ts:1374-1377).
	AccountScoped(platform, modelID string) bool
}

// OrderKeysByScore ranks a model's keys by a Thompson-sampled per-key score,
// mirroring the model bandit (orderKeysByScore, router.ts:1339-1356): reliability
// is a fresh draw from each key's Beta posterior - so exploration is automatic
// and proportional to uncertainty, a key with no data samples from the uniform
// prior and still gets traffic without ever preferring itself outright - while
// speed is deterministic. The second result is false, telling the caller to keep
// its round-robin rotation, when fewer than two keys are in play OR no key has
// recorded data at all (no signal, no ranking).
func OrderKeysByScore(keys []KeyCandidate, rng *rand.Rand) ([]KeyCandidate, bool) {
	if len(keys) < 2 {
		return keys, false
	}
	hasData := false
	for i := range keys {
		if keys[i].HasStats {
			hasData = true
			break
		}
	}
	if !hasData {
		return keys, false
	}

	type scored struct {
		k KeyCandidate
		s float64
	}
	ranked := make([]scored, len(keys))
	for i, k := range keys {
		rel := ReliabilityPosterior(k.Successes, k.Failures).Sample(rng)
		spd := SpeedScore(k.TokPerSec, k.TTFBMs)
		ranked[i] = scored{k: k, s: keyScoreReliabilityWeight*rel + keyScoreSpeedWeight*spd}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].s != ranked[b].s {
			return ranked[a].s > ranked[b].s
		}
		return ranked[a].k.ID < ranked[b].k.ID
	})
	out := make([]KeyCandidate, len(ranked))
	for i := range ranked {
		out[i] = ranked[i].k
	}
	return out, true
}

// OrderKeysByRemainingQuota re-sorts an already-ordered list by observed
// remaining quota, roomiest first (orderKeysByRemainingQuota, router.ts:1391-1398).
// The sort is stable, so keys with equal headroom - including the common case of
// no observations at all - keep exactly the order they came in with (the bandit
// ranking, or the round-robin rotation). An empty headroom map is returned
// unchanged.
func OrderKeysByRemainingQuota(keys []KeyCandidate, headroom map[int64]float64) []KeyCandidate {
	if len(headroom) == 0 {
		return keys
	}
	room := make(map[int64]float64, len(keys))
	for _, k := range keys {
		if h, ok := headroom[k.ID]; ok {
			room[k.ID] = h
		} else {
			room[k.ID] = unknownQuotaHeadroom
		}
	}
	out := make([]KeyCandidate, len(keys))
	copy(out, keys)
	sort.SliceStable(out, func(a, b int) bool {
		return room[out[a].ID] > room[out[b].ID]
	})
	return out
}

// QuotaWeightingApplies reports whether least-remaining re-sorting is meaningful
// for this model on this platform: the operator asked for it AND the platform
// meters its keys separately rather than against one shared account pool
// (quotaWeightingApplies, router.ts:1374-1377).
func QuotaWeightingApplies(strategy KeySelectionStrategy, platform, modelID string, qh QuotaHeadroom) bool {
	if strategy != KeySelectLeastRemaining {
		return false
	}
	if qh == nil {
		return false
	}
	return !qh.AccountScoped(platform, modelID)
}

// OrderKeys is the full key-ordering decision for one model (the ordering part
// of selectKeyForModel, router.ts:1456-1470): the per-key bandit ranking layered
// with least-remaining quota weighting. The second result is whether the
// returned order should be walked as-is; when it is false the caller keeps its
// own round-robin rotation over keys (there was neither per-key data nor quota
// weighting to justify a re-sort).
//
// rrStart is the caller's live round-robin cursor. It only seeds the rotation
// that least-remaining re-sorts when there is no bandit data, so a stable-sort
// tie still falls back to the rotation rather than to row id.
func OrderKeys(keys []KeyCandidate, strategy KeySelectionStrategy, platform, modelID string, rrStart int, qh QuotaHeadroom, rng *rand.Rand) ([]KeyCandidate, bool) {
	ranked, ok := OrderKeysByScore(keys, rng)
	if len(keys) > 1 && QuotaWeightingApplies(strategy, platform, modelID, qh) {
		base := ranked
		if !ok {
			base = rotate(keys, rrStart)
		}
		ranked = OrderKeysByRemainingQuota(base, qh.KeyHeadroom(platform))
		ok = true
	}
	return ranked, ok
}

// OrderKeysForWalk returns the final key order the caller walks, applying the
// round-robin rotation itself when OrderKeys reports no ranking signal. It
// exists so a caller outside this package never has to reach for the
// unexported rotate: OrderKeys deliberately returns ok=false to mean "no
// ranking, keep your rotation", and this folds that rotation in so the result
// is always directly walkable. rrStart is the caller's live round-robin cursor.
func OrderKeysForWalk(keys []KeyCandidate, strategy KeySelectionStrategy, platform, modelID string, rrStart int, qh QuotaHeadroom, rng *rand.Rand) []KeyCandidate {
	ordered, ranked := OrderKeys(keys, strategy, platform, modelID, rrStart, qh, rng)
	if ranked {
		return ordered
	}
	return rotate(keys, rrStart)
}

// rotate returns keys rotated to start at index start (the round-robin base), so
// the current cursor position leads. start is taken modulo the length and
// normalised for negatives.
func rotate(keys []KeyCandidate, start int) []KeyCandidate {
	n := len(keys)
	if n == 0 {
		return keys
	}
	start = ((start % n) + n) % n
	out := make([]KeyCandidate, n)
	for i := range n {
		out[i] = keys[(start+i)%n]
	}
	return out
}

// EligibleKeys drops keys that cannot serve this model before the walk: a key
// scoped to other models (#657) and a key already ruled out this request
// (skipKeys). Both are the reference's pre-walk filters (selectKeyForModel
// router.ts:1433, :1487). skip holds the key ids already failed for this
// (platform, model); build it with SkipKey.
func EligibleKeys(keys []KeyCandidate, modelID string, skip map[int64]bool) []KeyCandidate {
	out := make([]KeyCandidate, 0, len(keys))
	for _, k := range keys {
		if !ScopeAllows(k.Scope, modelID) {
			continue
		}
		if skip[k.ID] {
			continue
		}
		out = append(out, k)
	}
	return out
}

// ScopeAllows reports whether a key with this parsed model scope may serve the
// model (scopeAllows, model-scope.ts:25-27). An empty scope is unscoped: the key
// serves every model of its platform.
func ScopeAllows(scope []string, modelID string) bool {
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

// ParseModelScope parses a stored model_scope_json into an allow-list, or nil
// when the key is unscoped (parseModelScope, model-scope.ts:12-22). An empty,
// empty-array, or corrupt value degrades to unscoped rather than benching the
// key, and empty strings are dropped.
func ParseModelScope(jsonText string) []string {
	if jsonText == "" {
		return nil
	}
	var raw []string
	if err := json.Unmarshal([]byte(jsonText), &raw); err != nil {
		return nil
	}
	ids := make([]string, 0, len(raw))
	for _, s := range raw {
		if s != "" {
			ids = append(ids, s)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// SkipKey is the request-scoped id for a (platform, model, key) that failed this
// request (selectKeyForModel skipId, router.ts:1486). It is not the quota key -
// Slice A owns QuotaKey/PoolKey - it is the transient skip identity the failover
// loop carries and never persists.
func SkipKey(platform, modelID string, keyID int64) string {
	return platform + ":" + modelID + ":" + strconv.FormatInt(keyID, 10)
}
