package gateway

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Quota is the rate-window and provider-cap ledger ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md;
// server/src/services/ratelimit.ts and provider-quota.ts).
//
// A free tier is metered on four windows at once - requests and tokens, per
// minute and per day - and a restart must not hand back quota the provider
// has already counted. So the counters are persisted, aligned to the same
// boundaries the provider resets on, and pruned as they roll over. On top of
// the persisted counters an in-flight lease makes a request that is already in
// the air visible to the next candidate, which is what stops N concurrent
// requests from each reading the same headroom and collectively blowing a
// limit.
//
// Window alignment (the boundaries a bucket is keyed on). The reference uses
// two deliberately different semantics and so do we:
//
//   - minute windows (RPM, TPM) are keyed on the wall-clock minute:
//     window_start = unix_seconds - unix_seconds%60 (ratelimit.ts:295-308).
//   - per-model day windows (RPD, TPD) are a TRAILING 24h, not a calendar day:
//     the reference counts `created_at_ms > now - DAY` (ratelimit.ts:423),
//     which slides. We approximate a slide with hourly buckets keyed on
//     window_start = unix_seconds - unix_seconds%3600 and read the day usage as
//     the sum of the trailing 25 hourly buckets (see modelDaySum). A model
//     whose daily allowance was spent at 23:00 must still be held at 00:00,
//     because a provider enforcing a rolling window will still 429 it.
//   - provider-account caps (daily requests/tokens) are keyed on midnight UTC:
//     window_start = unix_seconds - unix_seconds%86400. The Unix epoch is
//     itself midnight UTC and 86400 divides every following midnight, so the
//     modulo lands exactly on 00:00:00Z. Real account caps reset on the
//     wall-clock day, not a slide (ratelimit.ts:34-37, :640, :755): a burst at
//     23:00Z must not still count against the account at 22:00Z the next day.
//
// A limit of zero or below means "no limit declared": both the hard gate and
// the headroom ramp treat it as unmetered, matching ratelimit.ts keyWindowPressure
// (`limit == null || limit <= 0` is skipped, ratelimit.ts:494).

const (
	minuteWindow = "minute"
	// hourWindow buckets the per-model day axes; the day figure is their
	// trailing sum, which slides instead of resetting at midnight.
	hourWindow = "hour"
	// dayWindow is the midnight-aligned provider-account bucket.
	dayWindow = "day"

	windowMinuteSeconds int64 = 60
	windowHourSeconds   int64 = 60 * 60
	windowDaySeconds    int64 = 24 * 60 * 60

	// leaseMaxAge backstops a lease the fallback loop somehow never released:
	// without it one leaked reservation would count against a key forever. It
	// MUST outlast the longest attempt a live request can legitimately hold a
	// lease for, or a slow-but-healthy call has its reservation pruned while it
	// is still in the air and a concurrent candidate over-admits the freed
	// headroom. The longest supported per-attempt provider timeout is
	// Perplexity's 300s deep-research bound (provider/perplexity.go); the
	// custom/anthropic/ollama relays are 120s and the default is 60s. Six
	// minutes clears every one of them with margin. The reference's 2*MINUTE
	// (ratelimit.ts:76 LEASE_MAX_AGE_MS) predates the 300s adapter and would
	// prune a live Perplexity lease mid-flight.
	leaseMaxAge = 6 * time.Minute

	// usagePruneInterval throttles the retention sweep off the hot path; a
	// stale rolled-over bucket lingering for up to a minute costs nothing
	// because reads only ever look at live buckets (ratelimit.ts:203).
	usagePruneInterval int64 = 60

	// windowUsageTTL is how long the graded-headroom snapshot is reused.
	// Utilization moves on the timescale of a window, not a request, so a few
	// seconds of staleness is invisible to a guardrail while the query count
	// drops to one per burst (ratelimit.ts:397 WINDOW_USAGE_TTL_MS = 5000).
	windowUsageTTL int64 = 5
)

// QuotaKey identifies the per-(platform, model, key) meter. It is the row key
// of rate_limit_usage for the per-model minute and hourly windows.
func QuotaKey(platform, modelID string, keyID int64) string {
	return platform + "/" + modelID + "/" + strconv.FormatInt(keyID, 10)
}

// PoolKey identifies the credential pool a provider-wide cap applies to. Some
// free tiers meter one shared allowance across every model on the account, so
// the pool is keyed by (platform, key) with no model, and every model's usage
// accrues into the same bucket.
func PoolKey(platform string, keyID int64) string {
	return platform + "/" + strconv.FormatInt(keyID, 10)
}

func minuteWindowStart(now time.Time) int64 {
	s := now.Unix()
	return s - s%windowMinuteSeconds
}

func hourWindowStart(now time.Time) int64 {
	s := now.Unix()
	return s - s%windowHourSeconds
}

func dayWindowStart(now time.Time) int64 {
	s := now.Unix()
	return s - s%windowDaySeconds
}

// WindowLimits is a model's four declared ceilings. A field <= 0 means the
// provider publishes no limit on that axis, so it is not enforced.
type WindowLimits struct {
	RPM int64
	RPD int64
	TPM int64
	TPD int64
}

func (w WindowLimits) none() bool {
	return w.RPM <= 0 && w.RPD <= 0 && w.TPM <= 0 && w.TPD <= 0
}

// Admission is one candidate the ledger is asked to admit and, on success,
// lease. It carries the model's limits so the gate is pure arithmetic over
// numbers the router already has in hand.
type Admission struct {
	Platform        string
	ModelID         string
	KeyID           int64
	EstimatedTokens int64
	Limits          WindowLimits

	// TokenMultiplier bills the provider-visible token drain for account-wide
	// token caps whose per-model rate differs from the OpenAI usage total
	// (NavyAI). Zero or below means one-to-one. See NavyTokenMultiplier.
	TokenMultiplier float64
}

// Lease is a request reserved against the ledger between key selection and the
// recorded outcome. Its projected tokens count toward the gates while it is
// live, so a concurrent candidate sees it; Settle turns the projection into a
// recorded fact and Release drops an abandoned reservation without recording.
type Lease struct {
	l          *Ledger
	id         int64
	platform   string
	modelID    string
	keyID      int64
	tokens     int64
	multiplier float64
	createdAt  time.Time
	settled    bool
}

// Ledger is the concurrency-safe rate-window and quota state. It is shared by
// every request goroutine: the in-memory lease map and snapshot are guarded by
// mu, and no database I/O happens while mu is held.
type Ledger struct {
	db    *sql.DB
	clock func() time.Time

	mu          sync.Mutex
	leases      map[int64]*Lease
	nextLeaseID int64
	lastPrune   int64
	snap        *windowSnapshot

	// settleGen is bumped every time a Settle removes a lease after committing
	// its persisted write. Acquire captures it before its lock-free DB
	// snapshot and re-checks it under the lock: a mismatch means a Settle
	// landed a durable write AND retired its lease in the gap, so the snapshot
	// misses a request that is no longer a live lease either. See Acquire.
	settleGen int64

	// afterSnapshotHook, when non-nil, runs inside Acquire after the lock-free
	// persisted snapshot is taken and before the lock is acquired. Production
	// leaves it nil; it exists solely so the concurrency regression can drive a
	// Settle into that exact window deterministically.
	afterSnapshotHook func()
}

// NewLedger returns a ledger backed by db, using wall-clock time.
func NewLedger(db *sql.DB) *Ledger {
	return &Ledger{
		db:          db,
		clock:       time.Now,
		leases:      make(map[int64]*Lease),
		nextLeaseID: 1,
	}
}

// ── Admission and leasing ────────────────────────────────────────────────────

// admitCounts is the persisted side of an admission decision, read once
// outside the lock. Persisted totals only ever grow (on Settle), so reading
// them before taking the lock cannot let a request slip through a limit: the
// dynamic in-flight portion is what the lock serialises.
type admitCounts struct {
	qkMinReq, qkMinTok int64
	qkDayReq, qkDayTok int64
	pkMinReq           int64
	pkDayReq, pkDayTok int64

	dailyReqCap, minReqCap, dailyTokCap     int64
	hasDailyReqCap, hasMinReqCap, hasTokCap bool
}

func (l *Ledger) liveCounts(a Admission, now time.Time) admitCounts {
	minStart := minuteWindowStart(now)
	dayStart := dayWindowStart(now)
	qk := QuotaKey(a.Platform, a.ModelID, a.KeyID)
	var c admitCounts

	if a.Limits.RPM > 0 || a.Limits.TPM > 0 {
		c.qkMinReq, c.qkMinTok = l.bucket(qk, minuteWindow, minStart)
	}
	if a.Limits.RPD > 0 || a.Limits.TPD > 0 {
		c.qkDayReq, c.qkDayTok = l.modelDaySum(qk, now)
	}

	c.dailyReqCap, c.hasDailyReqCap = ProviderDailyRequestCap(a.Platform)
	c.minReqCap, c.hasMinReqCap = ProviderMinuteRequestCap(a.Platform)
	c.dailyTokCap, c.hasTokCap = ProviderDailyTokenCap(a.Platform)

	pk := PoolKey(a.Platform, a.KeyID)
	if c.hasMinReqCap {
		c.pkMinReq, _ = l.bucket(pk, minuteWindow, minStart)
	}
	if c.hasDailyReqCap || c.hasTokCap {
		c.pkDayReq, c.pkDayTok = l.bucket(pk, dayWindow, dayStart)
	}
	return c
}

// admitLocked applies the gates. It is pure arithmetic over the persisted
// counts plus the live lease totals, so it must run with mu held.
func (l *Ledger) admitLocked(a Admission, c admitCounts) bool {
	inReq, inTok := provisionalModel(l.leases, a.Platform, a.ModelID, a.KeyID)

	// Per-model windows. In-flight leases count against both the minute and
	// the day: a request in the air belongs to this minute and to today
	// (ratelimit.ts:312-356). Requests reject at >=, tokens at > because a
	// token estimate that exactly fills the window is still admissible.
	if a.Limits.RPM > 0 && c.qkMinReq+inReq >= a.Limits.RPM {
		return false
	}
	if a.Limits.RPD > 0 && c.qkDayReq+inReq >= a.Limits.RPD {
		return false
	}
	if a.Limits.TPM > 0 && c.qkMinTok+inTok+a.EstimatedTokens > a.Limits.TPM {
		return false
	}
	if a.Limits.TPD > 0 && c.qkDayTok+inTok+a.EstimatedTokens > a.Limits.TPD {
		return false
	}

	// Provider-wide caps, shared by every model on the account+key.
	if c.hasMinReqCap || c.hasDailyReqCap || c.hasTokCap {
		poolReq, poolBilled := provisionalPool(l.leases, a.Platform, a.KeyID)
		if c.hasMinReqCap && c.pkMinReq+poolReq >= c.minReqCap {
			return false
		}
		if c.hasDailyReqCap && c.pkDayReq+poolReq >= c.dailyReqCap {
			return false
		}
		if c.hasTokCap {
			// Each in-flight lease already billed itself at its own model's
			// multiplier; only the new estimate uses the candidate's. Applying
			// one multiplier to the whole pool would mis-bill every sibling
			// lease of a different model sharing this NavyAI account.
			billed := poolBilled + providerBilledTokens(a.Platform, a.TokenMultiplier, a.EstimatedTokens)
			if c.pkDayTok+billed > c.dailyTokCap {
				return false
			}
		}
	}
	return true
}

// Admit reports whether a candidate may run right now, without reserving
// anything. It is the read-only guardrail the router filters chains against.
func (l *Ledger) Admit(a Admission) bool {
	now := l.clock()
	c := l.liveCounts(a, now)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLeasesLocked(now)
	return l.admitLocked(a, c)
}

// Acquire atomically re-checks admission and, if the candidate fits, reserves
// its projected tokens as an in-flight lease. It returns (nil, false) when
// admitting would breach a window or a provider cap. The check and the
// reservation happen under one lock, so N racing callers cannot each observe
// the same free headroom and collectively over-admit: the K-th caller sees the
// K-1 reservations the earlier callers already took.
//
// The persisted snapshot is read outside the lock (liveCounts does DB I/O and
// mu must never be held across it). That opens a narrow race: a concurrent
// Settle can commit its durable write AND retire its lease in the gap between
// the snapshot and the lock, leaving the just-settled request invisible on
// BOTH axes - the snapshot predates its DB write and the live-lease map no
// longer holds it - so admitLocked would hand back headroom the provider has
// already counted. settleGen closes it: it is captured before the snapshot and
// re-read under the lock; every lease-retiring Settle bumps it after its write
// commits. A mismatch means such a Settle landed in the gap, so the snapshot is
// stale and we re-read it. Capturing before the snapshot is what makes this
// sound: a Settle whose write is missing from the snapshot always bumps the
// generation after we captured it (the bump follows the write in program
// order), so it can never slip past both the snapshot and the generation check.
func (l *Ledger) Acquire(a Admission) (*Lease, bool) {
	for {
		gen := atomic.LoadInt64(&l.settleGen)
		now := l.clock()
		c := l.liveCounts(a, now)
		if l.afterSnapshotHook != nil {
			l.afterSnapshotHook()
		}
		l.mu.Lock()
		l.pruneLeasesLocked(now)
		if atomic.LoadInt64(&l.settleGen) != gen {
			// A Settle committed a write and retired its lease after our
			// snapshot was taken; c may miss it. Drop the lock and re-snapshot
			// against the now-current counters.
			l.mu.Unlock()
			continue
		}
		if !l.admitLocked(a, c) {
			l.mu.Unlock()
			return nil, false
		}
		id := l.nextLeaseID
		l.nextLeaseID++
		ls := &Lease{
			l:          l,
			id:         id,
			platform:   a.Platform,
			modelID:    a.ModelID,
			keyID:      a.KeyID,
			tokens:     a.EstimatedTokens,
			multiplier: a.TokenMultiplier,
			createdAt:  now,
		}
		l.leases[id] = ls
		l.mu.Unlock()
		return ls, true
	}
}

// Settle records the request against the persisted counters using the real
// token count and retires the reservation. The order matters: the durable
// write happens first and the lease is dropped after, so for a microsecond the
// request is counted twice - which errs toward under-dispatching by one, the
// safe direction for a free tier (ratelimit.ts:159-161). A second Settle is a
// no-op, so an overlapping cleanup path cannot double-record.
func (ls *Lease) Settle(actualTokens int64) {
	l := ls.l
	l.mu.Lock()
	if ls.settled {
		l.mu.Unlock()
		return
	}
	ls.settled = true
	l.mu.Unlock()

	if actualTokens < 0 {
		actualTokens = 0
	}
	now := l.clock()
	billed := providerBilledTokens(ls.platform, ls.multiplier, actualTokens)
	l.record(
		QuotaKey(ls.platform, ls.modelID, ls.keyID),
		PoolKey(ls.platform, ls.keyID),
		minuteWindowStart(now), hourWindowStart(now), dayWindowStart(now),
		actualTokens, billed, now,
	)

	l.mu.Lock()
	delete(l.leases, ls.id)
	// Signal Acquire that a durable write landed and this lease is now gone,
	// so any in-flight snapshot taken before record() committed is stale. The
	// bump follows the DB commit in program order, so a snapshot that missed
	// the write is guaranteed to see the generation move.
	atomic.AddInt64(&l.settleGen, 1)
	l.mu.Unlock()
}

// Release drops an in-flight reservation without recording usage. It is the
// error path - a request that never completed must not spend quota - and is
// safe to call in a finally after Settle, where it finds the lease already
// gone and does nothing.
func (ls *Lease) Release() {
	l := ls.l
	l.mu.Lock()
	delete(l.leases, ls.id)
	l.mu.Unlock()
}

func (l *Ledger) pruneLeasesLocked(now time.Time) {
	for id, ls := range l.leases {
		if now.Sub(ls.createdAt) > leaseMaxAge {
			delete(l.leases, id)
		}
	}
}

func provisionalModel(leases map[int64]*Lease, platform, modelID string, keyID int64) (reqs, tokens int64) {
	for _, ls := range leases {
		if ls.platform == platform && ls.modelID == modelID && ls.keyID == keyID {
			reqs++
			tokens += ls.tokens
		}
	}
	return
}

// provisionalPool totals the in-flight leases of one credential pool. Tokens
// are returned already BILLED - each lease converted through its own model's
// multiplier - because a NavyAI account pools models whose per-model token
// rates differ, so a candidate's multiplier must not be retro-applied to a
// sibling lease that was leased at a different rate.
func provisionalPool(leases map[int64]*Lease, platform string, keyID int64) (reqs, billedTokens int64) {
	for _, ls := range leases {
		if ls.platform == platform && ls.keyID == keyID {
			reqs++
			billedTokens += providerBilledTokens(platform, ls.multiplier, ls.tokens)
		}
	}
	return
}

// ── Persisted counters ───────────────────────────────────────────────────────

func (l *Ledger) bucket(quotaKey, kind string, windowStart int64) (reqs, tokens int64) {
	row := l.db.QueryRow(
		`SELECT requests, tokens FROM rate_limit_usage
		  WHERE quota_key = ? AND window_kind = ? AND window_start = ?`,
		quotaKey, kind, windowStart,
	)
	if err := row.Scan(&reqs, &tokens); err != nil {
		return 0, 0
	}
	return
}

// modelDaySum reads a per-model day axis as the sum of the trailing 25 hourly
// buckets. 25, not 24: 24 buckets span only 23-24h of history and so
// UNDER-count at the hour boundary, which would over-admit; 25 over-counts by
// up to an hour, which merely leaves a sliver of a free tier unspent. For
// quota the conservative direction is the correct one, so this must not be
// "optimised" down to 24. This slides the way the reference's
// `created_at_ms > now - DAY` does (ratelimit.ts:423), unlike the
// midnight-aligned provider caps.
func (l *Ledger) modelDaySum(quotaKey string, now time.Time) (reqs, tokens int64) {
	cutoff := hourWindowStart(now) - windowDaySeconds
	row := l.db.QueryRow(
		`SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(tokens), 0)
		   FROM rate_limit_usage
		  WHERE quota_key = ? AND window_kind = ? AND window_start >= ?`,
		quotaKey, hourWindow, cutoff,
	)
	if err := row.Scan(&reqs, &tokens); err != nil {
		return 0, 0
	}
	return
}

// record increments the buckets one request touches: the per-model meter in
// its minute and trailing-hour windows, and the pool meter in its minute and
// midnight-aligned day windows - all in one transaction, then it sweeps
// rolled-out buckets. A write failure is best-effort: the counter simply is
// not recorded, which under-counts rather than over-admits.
func (l *Ledger) record(qk, pk string, minStart, hourStart, dayStart, raw, billed int64, now time.Time) {
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

	if upsertUsage(tx, qk, minuteWindow, minStart, raw) != nil {
		return
	}
	if upsertUsage(tx, qk, hourWindow, hourStart, raw) != nil {
		return
	}
	if upsertUsage(tx, pk, minuteWindow, minStart, billed) != nil {
		return
	}
	if upsertUsage(tx, pk, dayWindow, dayStart, billed) != nil {
		return
	}
	if l.shouldPrune(now) {
		// Minute and midnight-day buckets are read only at their current
		// window_start, so anything earlier is dead. Hourly buckets are read
		// as a trailing sum, so they must survive until they fall out of the
		// 25-bucket window.
		if _, err := tx.Exec(
			`DELETE FROM rate_limit_usage
			  WHERE (window_kind = ? AND window_start < ?)
			     OR (window_kind = ? AND window_start < ?)
			     OR (window_kind = ? AND window_start < ?)`,
			minuteWindow, minStart,
			dayWindow, dayStart,
			hourWindow, hourStart-windowDaySeconds,
		); err != nil {
			return
		}
	}
	if tx.Commit() != nil {
		return
	}
	committed = true
}

func upsertUsage(tx *sql.Tx, quotaKey, kind string, windowStart, tokens int64) error {
	_, err := tx.Exec(
		`INSERT INTO rate_limit_usage (quota_key, window_kind, window_start, requests, tokens)
		 VALUES (?, ?, ?, 1, ?)
		 ON CONFLICT(quota_key, window_kind, window_start)
		 DO UPDATE SET requests = requests + 1, tokens = tokens + excluded.tokens`,
		quotaKey, kind, windowStart, tokens,
	)
	return err
}

func (l *Ledger) shouldPrune(now time.Time) bool {
	s := now.Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	// s < lastPrune covers a backward clock step (NTP correction, a suspended
	// host resuming) so a forward jump cannot wedge the sweep off forever.
	if s-l.lastPrune >= usagePruneInterval || s < l.lastPrune {
		l.lastPrune = s
		return true
	}
	return false
}

// ── Graded rate-window headroom (scoring guardrail) ──────────────────────────

type keyWindowUsage struct {
	rpm, rpd, tpm, tpd int64
}

type windowSnapshot struct {
	at    int64
	usage map[string]keyWindowUsage
}

// Headroom is the fraction of a candidate's headroom left, in [0,1], for the
// scoring guardrail to multiply the base score by. It reads the current
// windows of the eligible key with the MOST headroom - a platform with one
// exhausted key and one idle key routes fine, so the binding question is how
// close the key the router would pick next is to unroutable (ratelimit.ts:507-511)
// - takes the worst-loaded of that key's four windows, and ramps the result
// through the same monthly-budget ramp so demotion is gradual. It returns 1
// (no demotion) when the model declares no window limits or has no routable
// key: a number there would be fiction.
func (l *Ledger) Headroom(platform, modelID string, keyIDs []int64, limits WindowLimits) float64 {
	if limits.none() || len(keyIDs) == 0 {
		return 1
	}
	snap := l.snapshot(l.clock())
	if snap == nil {
		return 1
	}
	best := math.Inf(1)
	for _, keyID := range keyIDs {
		pressure := keyWindowPressure(snap.usage[QuotaKey(platform, modelID, keyID)], limits)
		if pressure < best {
			best = pressure
		}
		if best == 0 {
			break // an untouched key is as roomy as it gets
		}
	}
	if math.IsInf(best, 1) {
		return 1
	}
	used := math.Min(1, best)
	return headroomRamp(1 - used)
}

// keyWindowPressure is the busiest window of one key: the ratio that would
// reject its next request first (ratelimit.ts:485-499).
func keyWindowPressure(u keyWindowUsage, limits WindowLimits) float64 {
	worst := 0.0
	consider := func(used, limit int64) {
		if limit <= 0 {
			return
		}
		if r := float64(used) / float64(limit); r > worst {
			worst = r
		}
	}
	consider(u.rpm, limits.RPM)
	consider(u.rpd, limits.RPD)
	consider(u.tpm, limits.TPM)
	consider(u.tpd, limits.TPD)
	return worst
}

func (l *Ledger) snapshot(now time.Time) *windowSnapshot {
	nowSec := now.Unix()
	l.mu.Lock()
	if l.snap != nil && nowSec >= l.snap.at && nowSec-l.snap.at < windowUsageTTL {
		s := l.snap
		l.mu.Unlock()
		return s
	}
	l.mu.Unlock()

	minStart := minuteWindowStart(now)
	hourCutoff := hourWindowStart(now) - windowDaySeconds
	rows, err := l.db.Query(
		`SELECT quota_key, window_kind, requests, tokens
		   FROM rate_limit_usage
		  WHERE (window_kind = ? AND window_start = ?)
		     OR (window_kind = ? AND window_start >= ?)`,
		minuteWindow, minStart, hourWindow, hourCutoff,
	)
	if err != nil {
		return nil
	}
	usage := make(map[string]keyWindowUsage)
	for rows.Next() {
		var quotaKey, kind string
		var reqs, tokens int64
		if err := rows.Scan(&quotaKey, &kind, &reqs, &tokens); err != nil {
			rows.Close()
			return nil
		}
		u := usage[quotaKey]
		switch kind {
		case minuteWindow:
			u.rpm, u.tpm = reqs, tokens
		case hourWindow:
			// The day axis is the trailing sum of hourly buckets, so these
			// accumulate rather than overwrite.
			u.rpd += reqs
			u.tpd += tokens
		}
		usage[quotaKey] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil
	}

	snap := &windowSnapshot{at: nowSec, usage: usage}
	l.mu.Lock()
	l.snap = snap
	l.mu.Unlock()
	return snap
}

// invalidateSnapshot drops the memoised window usage so the next Headroom call
// rebuilds it. Production relies on the TTL; tests use this to make a write
// visible immediately.
func (l *Ledger) invalidateSnapshot() {
	l.mu.Lock()
	l.snap = nil
	l.mu.Unlock()
}

// ── Provider-wide caps ───────────────────────────────────────────────────────

// Some providers meter one shared allowance across the whole account rather
// than per model, which the per-model windows cannot see. The defaults are the
// live-observed values from the reference; each is overridable per platform by
// an env var, and 0 disables the cap.

// DEFAULT_PROVIDER_DAILY_REQUEST_CAPS (ratelimit.ts:561-567): OpenRouter's
// free tier is ~1000 requests/day across the account; ModelScope is 2000/day,
// held to 1800 for validation-probe margin.
var defaultDailyRequestCaps = map[string]int64{
	"openrouter": 1000,
	"modelscope": 1800,
}

// DEFAULT_PROVIDER_DAILY_TOKEN_CAPS (ratelimit.ts:569-574): NavyAI's free plan
// is one 150K-token/day pool per key.
var defaultDailyTokenCaps = map[string]int64{
	"navy": 150_000,
}

// DEFAULT_PROVIDER_MINUTE_REQUEST_CAPS (ratelimit.ts:582-584): NVIDIA NIM
// meters ~40 requests/minute across the whole account.
var defaultMinuteRequestCaps = map[string]int64{
	"nvidia": 40,
}

// ProviderDailyRequestCap returns the account-wide daily request cap, or ok
// false when the platform has none.
func ProviderDailyRequestCap(platform string) (int64, bool) {
	return providerCap("PROVIDER_DAILY_REQUEST_CAP_", platform, defaultDailyRequestCaps)
}

// ProviderMinuteRequestCap returns the account-wide per-minute request cap.
func ProviderMinuteRequestCap(platform string) (int64, bool) {
	return providerCap("PROVIDER_MINUTE_REQUEST_CAP_", platform, defaultMinuteRequestCaps)
}

// ProviderDailyTokenCap returns the account-wide daily token cap.
func ProviderDailyTokenCap(platform string) (int64, bool) {
	return providerCap("PROVIDER_DAILY_TOKEN_CAP_", platform, defaultDailyTokenCaps)
}

func providerCap(prefix, platform string, defaults map[string]int64) (int64, bool) {
	// An env value that parses as a non-negative number wins; 0 disables. A
	// blank or unparseable value falls through to the built-in default, so a
	// typo cannot silently drop a cap (ratelimit.ts:586-593).
	if raw := strings.TrimSpace(os.Getenv(prefix + strings.ToUpper(platform))); raw != "" {
		if n, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0 {
			if n == 0 {
				return 0, false
			}
			return int64(n), true
		}
	}
	if def, ok := defaults[strings.ToLower(platform)]; ok {
		return def, true
	}
	return 0, false
}

// providerBilledTokens converts a raw token count into what the provider's
// account-wide token cap actually meters. It is one-to-one except for NavyAI,
// whose per-model token multiplier can make the provider-visible drain larger
// than the OpenAI usage total (ratelimit.ts:716-722).
func providerBilledTokens(platform string, multiplier float64, raw int64) int64 {
	if raw <= 0 {
		return 0
	}
	m := multiplier
	if strings.ToLower(platform) != "navy" || m <= 0 {
		m = 1
	}
	if m == 1 {
		return raw
	}
	return int64(math.Ceil(float64(raw) * m))
}

var navyLabelMultiplier = regexp.MustCompile(`(?i)(?:^|[·(\s])(\d+(?:\.\d+)?)x\b`)

// NavyTokenMultiplier derives the per-model billed-token multiplier the
// account-wide token cap should use: an explicit "2x" in the budget label wins,
// else the ratio of the account cap to the model's own daily token limit when
// that is the tighter figure (ratelimit.ts:689-701).
func NavyTokenMultiplier(budgetLabel string, tpdLimit, dailyCap int64) float64 {
	if m := navyLabelMultiplier.FindStringSubmatch(budgetLabel); m != nil {
		if n, err := strconv.ParseFloat(m[1], 64); err == nil && n > 0 {
			return n
		}
	}
	if tpdLimit > 0 && dailyCap > 0 && tpdLimit < dailyCap {
		return float64(dailyCap) / float64(tpdLimit)
	}
	return 1
}

// ── Learned limits from error bodies ─────────────────────────────────────────

// LearnedLimit is a provider-stated ceiling pulled out of an error body.
type LearnedLimit struct {
	Kind  string // one of tpm, tpd, rpm, rpd
	Limit int64
}

// Order matters: the per-day axes are checked before per-minute so "tokens per
// day" is not shadowed by the tpm alternative, and tokens before requests so a
// body mentioning both lands on the more specific token ceiling
// (ratelimit.ts:1462-1467).
var limitAxisPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"tpd", regexp.MustCompile(`(?i)tokens?\s*per\s*day|\btpd\b`)},
	{"tpm", regexp.MustCompile(`(?i)tokens?\s*per\s*min(?:ute)?|\btpm\b`)},
	{"rpd", regexp.MustCompile(`(?i)requests?\s*per\s*day|\brpd\b`)},
	{"rpm", regexp.MustCompile(`(?i)requests?\s*per\s*min(?:ute)?|\brpm\b`)},
}

var limitNumberPattern = regexp.MustCompile(`(?i)\blimit[:\s]+([\d,]+)`)

// ParseProviderLimit pulls a provider-reported ceiling out of an error message.
// It returns ok false unless BOTH a numeric "Limit N" and a confident axis are
// present: guessing the axis would write the wrong column and mis-route every
// future request (ratelimit.ts:1475-1485).
func ParseProviderLimit(message string) (LearnedLimit, bool) {
	if message == "" {
		return LearnedLimit{}, false
	}
	m := limitNumberPattern.FindStringSubmatch(message)
	if m == nil {
		return LearnedLimit{}, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil || n <= 0 {
		return LearnedLimit{}, false
	}
	for _, p := range limitAxisPatterns {
		if p.re.MatchString(message) {
			return LearnedLimit{Kind: p.kind, Limit: n}, true
		}
	}
	return LearnedLimit{}, false
}

var limitColumn = map[string]string{
	"tpm": "tpm_limit",
	"tpd": "tpd_limit",
	"rpm": "rpm_limit",
	"rpd": "rpd_limit",
}

// LearnLimitFromError persists a provider-reported limit onto the model row,
// but ONLY when it makes routing more conservative: it fills a NULL limit or
// lowers one that was too high, never raises. Hitting a ceiling means the
// pre-check already let too much through, so the true limit is at or below what
// was used (ratelimit.ts:1493-1511). It returns the learned limit when a row
// actually changed.
func (l *Ledger) LearnLimitFromError(modelDBID int64, message string) (LearnedLimit, bool) {
	parsed, ok := ParseProviderLimit(message)
	if !ok {
		return LearnedLimit{}, false
	}
	col := limitColumn[parsed.Kind] // whitelisted: no injection surface
	res, err := l.db.Exec(
		fmt.Sprintf(
			"UPDATE models SET %s = ? WHERE id = ? AND (%s IS NULL OR %s > ?)",
			col, col, col,
		),
		parsed.Limit, modelDBID, parsed.Limit,
	)
	if err != nil {
		return LearnedLimit{}, false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return parsed, true
	}
	return LearnedLimit{}, false
}
