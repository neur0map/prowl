package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubscriptionWindowsAreObserved covers the reason an active subscription
// showed no budget at all: Anthropic publishes rolling-window UTILISATION,
// not requests or tokens remaining, over two windows at once. The
// counts-shaped header table could not express that, so nothing was recorded.
// Header names and shapes here are copied from a live 200 response.
func TestSubscriptionWindowsAreObserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.22")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1789497000")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.8")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1789927200")
	headers.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed_warning")

	stored := eng.Ledger().ObserveUnifiedWindows("anthropic", 16, headers)
	require.Equal(t, 2, stored, "both windows must be recorded")

	states, err := eng.Ledger().QuotaStates(ctx)
	require.NoError(t, err)

	byPool := map[string]QuotaState{}
	for _, st := range states {
		byPool[st.PoolKey] = st
	}

	five, ok := byPool["anthropic/16::5h"]
	require.True(t, ok, "the 5-hour window must have its own pool")
	require.NotNil(t, five.RequestsRemaining)
	require.Equal(t, int64(78), *five.RequestsRemaining, "22% used leaves 78% of the window")
	require.NotNil(t, five.RequestsLimit)
	require.Equal(t, int64(100), *five.RequestsLimit)

	week, ok := byPool["anthropic/16::7d"]
	require.True(t, ok, "the weekly window must have its own pool")
	require.Equal(t, int64(20), *week.RequestsRemaining, "80% used leaves 20%")
	require.NotNil(t, week.ResetsAt)
	require.Equal(t, int64(1789927200), *week.ResetsAt)

	// The windows must be labelled as shares, not mistaken for request counts.
	require.Equal(t, "5-hour window", WindowLabel("anthropic/16::5h"))
	require.Equal(t, "weekly window", WindowLabel("anthropic/16::7d"))
	require.Empty(t, WindowLabel("groq/4"), "an ordinary pool is not a window")
}

// TestSubscriptionWindowsIgnoreProvidersThatPublishNone keeps the observation
// silent rather than inventing a window for a provider that reports counts.
func TestSubscriptionWindowsIgnoreProvidersThatPublishNone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	headers := http.Header{}
	headers.Set("x-ratelimit-remaining-requests", "40")
	require.Zero(t, eng.Ledger().ObserveUnifiedWindows("groq", 4, headers))

	states, err := eng.Ledger().QuotaStates(ctx)
	require.NoError(t, err)
	require.Empty(t, states)
}

// TestExhaustedWindowReadsAsZero covers a spent allowance: utilisation can
// exceed 1, which must not produce a negative remainder.
func TestExhaustedWindowReadsAsZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	headers := http.Header{}
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.4")
	require.Equal(t, 1, eng.Ledger().ObserveUnifiedWindows("anthropic", 16, headers))

	states, err := eng.Ledger().QuotaStates(ctx)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Zero(t, *states[0].RequestsRemaining)
}

// TestNonfiniteUtilizationIsRejected covers a malformed or hostile header: a
// NaN slips past a `< 0` guard and casts to a garbage int64 allowance, and an
// infinity reads as fully used. Every nonfinite utilization must be dropped
// before any arithmetic or persistence, leaving no bogus state behind.
func TestNonfiniteUtilizationIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eng, err := OpenEngine(ctx, t.TempDir(), EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	for _, raw := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "inf", "infinity"} {
		headers := http.Header{}
		headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", raw)
		stored := eng.Ledger().ObserveUnifiedWindows("anthropic", 21, headers)
		require.Zerof(t, stored, "nonfinite utilization %q must be rejected, not stored", raw)
	}

	states, err := eng.Ledger().QuotaStates(ctx)
	require.NoError(t, err)
	require.Empty(t, states, "no nonfinite utilization may persist a bogus allowance")
}
