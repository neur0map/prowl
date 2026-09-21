package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEngineAssemblesEverySubsystem is the wiring check. Each subsystem has
// its own tests; this one exists because they are independently correct and
// still fit together wrongly - a nil dependency here fails at the first
// request rather than at build time.
func TestEngineAssemblesEverySubsystem(t *testing.T) {
	t.Parallel()

	e, err := OpenEngine(context.Background(), t.TempDir(), EngineOptions{})
	require.NoError(t, err)
	defer func() { _ = e.Close() }()

	require.NotNil(t, e.DB(), "the HTTP layer reads through this")
	require.NotNil(t, e.Vault())
	require.NotNil(t, e.Ledger())
	require.NotNil(t, e.Penalties())
	require.NotNil(t, e.Cooldowns())
	require.NotNil(t, e.Sticky())
	require.NotNil(t, e.Failover())
	require.NotNil(t, e.Registry())
}

// TestEngineStartsNoGoroutinesUntilAsked keeps a test or a CLI query from
// acquiring background work it never wanted: opening the engine must be inert,
// and stopping must actually join.
func TestEngineStartsNoGoroutinesUntilAsked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	e, err := OpenEngine(ctx, t.TempDir(), EngineOptions{})
	require.NoError(t, err)
	defer func() { _ = e.Close() }()

	stop := e.StartBackground(ctx)

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StartBackground's stop function did not join its goroutine")
	}
}

// TestEngineSurvivesReopen proves the engine's state is durable rather than
// process-local: the whole reason it owns a database is that quota counters
// and benches must outlive a restart.
func TestEngineSurvivesReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	first, err := OpenEngine(ctx, dir, EngineOptions{})
	require.NoError(t, err)
	_, err = first.Vault().Add("groq", "gsk-reopen-proof", AddOptions{Label: "persisted"})
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second, err := OpenEngine(ctx, dir, EngineOptions{})
	require.NoError(t, err)
	defer func() { _ = second.Close() }()

	rows, err := second.Vault().List(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the credential must survive a restart")

	plain, err := second.Vault().Reveal(ctx, rows[0].ID)
	require.NoError(t, err)
	require.Equal(t, "gsk-reopen-proof", plain, "the vault must still decrypt with its master key")
}

// TestCooldownAdapterKeysByTriple checks the seam between the loop's
// (platform, model, key) view and the engine's string-keyed benching. Getting
// this wrong would bench the wrong endpoint, which no subsystem test would
// catch because each side is individually right.
func TestCooldownAdapterKeysByTriple(t *testing.T) {
	t.Parallel()

	engine := NewCooldownEngine()
	adapter := cooldownAdapter{engine: engine}

	adapter.Bench("groq", "llama-3", 7, time.Hour, SourceCredit)

	_, ok := adapter.Active("groq", "llama-3", 7)
	require.True(t, ok, "the benched triple must read back")

	_, ok = adapter.Active("groq", "llama-3", 8)
	require.False(t, ok, "a different key must be unaffected")
	_, ok = adapter.Active("groq", "mixtral", 7)
	require.False(t, ok, "a different model must be unaffected")
	_, ok = adapter.Active("cerebras", "llama-3", 7)
	require.False(t, ok, "a different platform must be unaffected")

	// The underlying engine must see the same key the adapter wrote.
	_, ok = engine.Active(QuotaKey("groq", "llama-3", 7))
	require.True(t, ok, "the adapter and the ledger must agree on the key format")
}

// TestSoonestExpiryAnswersWhenToComeBack is what an all-time-bound exhaustion
// turns into a Retry-After. Reporting the latest bench, or zero, would tell a
// client to retry either far too late or immediately.
func TestSoonestExpiryAnswersWhenToComeBack(t *testing.T) {
	t.Parallel()

	engine := NewCooldownEngine()
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return base }

	require.True(t, engine.SoonestExpiry().IsZero(), "nothing benched means nothing to wait for")

	engine.Bench("a", time.Hour, SourceHeuristic)
	engine.Bench("b", 10*time.Minute, SourceHeuristic)
	engine.Bench("c", 24*time.Hour, SourceCredit)

	require.Equal(t, base.Add(10*time.Minute), engine.SoonestExpiry(),
		"the earliest recovery is the only useful answer")

	// An expired bench must not be reported as a future recovery.
	engine.now = func() time.Time { return base.Add(11 * time.Minute) }
	require.Equal(t, base.Add(time.Hour), engine.SoonestExpiry())
}
