package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCooldownsSurviveRestart is the bug this persistence closes: benches
// lived only in memory, so restarting -- which happens every time the harness
// restarts -- handed a rate-limited key straight back to the router, earning
// another 429 and another bench. The table existed and the dashboard read it;
// nothing ever wrote it.
func TestCooldownsSurviveRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	key := QuotaKey("groq", "llama-3.3-70b", 7)

	first, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	bench := first.Cooldowns().Bench(key, 2*time.Hour, SourceAuthoritative)
	require.True(t, bench.Until.After(time.Now()))
	require.NoError(t, first.Close())

	// Reopening the same directory is a restart.
	second, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)

	restored, ok := second.Cooldowns().Active(key)
	require.True(t, ok, "the bench must still hold after a restart")
	require.WithinDuration(t, bench.Until, restored.Until, time.Second)
	require.Equal(t, SourceAuthoritative, restored.Source)

	// A served request lifts it, and that must stick too. Closing first
	// drains the queued delete, which is what makes the next open a real
	// restart rather than a race against the writer.
	second.Cooldowns().Succeeded(key, time.Now())
	require.NoError(t, second.Close())

	third, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = third.Close() })
	_, ok = third.Cooldowns().Active(key)
	require.False(t, ok, "a lifted bench must not come back")
}

// TestExpiredCooldownsAreNotRestored keeps the table from resurrecting stale
// benches, which would hold keys back long after the provider let go.
func TestExpiredCooldownsAreNotRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	key := QuotaKey("groq", "llama-3.3-70b", 7)
	first, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	first.Cooldowns().Bench(key, time.Hour, SourceHeuristic)
	// Closing drains the persistence queue, so the row is on disk afterwards.
	require.NoError(t, first.Close())

	// Backdate it past its expiry, the way a bench that lapsed while the
	// gateway was stopped would look.
	backdate, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	res, err := backdate.DB().Exec(
		"UPDATE rate_limit_cooldowns SET until = ? WHERE quota_key = ?",
		time.Now().Add(-time.Minute).Unix(), key)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "the bench must have been persisted")
	require.NoError(t, backdate.Close())

	second, err := OpenEngine(ctx, dir, EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	_, ok := second.Cooldowns().Active(key)
	require.False(t, ok, "an expired bench must not be restored")

	var rows int
	require.NoError(t, second.DB().QueryRow(
		"SELECT COUNT(*) FROM rate_limit_cooldowns").Scan(&rows))
	require.Zero(t, rows, "expired rows must be pruned, not accumulated")
}

// reentrantSink calls back into the engine from inside the sink, which is what
// a sink doing real I/O effectively does: it needs a resource another
// goroutine may hold while that goroutine waits for the cooldown lock.
type reentrantSink struct {
	engine *CooldownEngine
	hits   chan struct{}
}

func (r *reentrantSink) SaveCooldown(key string, _ Cooldown, _ int, _ time.Time) {
	// Re-entering Active needs the same lock Bench holds. If the engine calls
	// the sink while holding it, this never returns.
	_, _ = r.engine.Active(key)
	select {
	case r.hits <- struct{}{}:
	default:
	}
}

func (r *reentrantSink) DropCooldown(string) {}

// TestPersistenceDoesNotRunUnderTheCooldownLock pins the invariant a live
// deadlock taught: the store runs SQLite with a single connection, so holding
// the cooldown lock across a write lets a request that already owns the
// connection -- and then needs this lock -- park the whole gateway. Every
// goroutine ended up waiting on the connection pool.
func TestPersistenceDoesNotRunUnderTheCooldownLock(t *testing.T) {
	t.Parallel()

	engine := NewCooldownEngine()
	sink := &reentrantSink{engine: engine, hits: make(chan struct{}, 1)}
	engine.SetSink(sink, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.Bench(QuotaKey("groq", "m", 1), time.Minute, SourceHeuristic)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Bench blocked: the sink is being called while the cooldown lock is held")
	}

	select {
	case <-sink.hits:
	case <-time.After(5 * time.Second):
		t.Fatal("the bench was never handed to the sink")
	}
}
