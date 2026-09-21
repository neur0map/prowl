package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPeakAdjustedWeights proves the peak-hour shift moves 0.6 of the speed
// weight onto reliability for a non-exempt strategy inside the window, leaves
// intelligence alone, and does nothing when the strategy is exempt, the config
// is disabled, or the clock is outside the window.
func TestPeakAdjustedWeights(t *testing.T) {
	t.Parallel()
	base := WeightsBalanced // {0.5, 0.25, 0.25}
	cfg := PeakHoursConfig{Enabled: true, StartHour: 0, EndHour: 23, Timezone: "UTC"}
	inPeak := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	w, adj := PeakAdjustedWeights(base, RoutingBalanced, cfg, inPeak)
	require.True(t, adj, "balanced inside the window must adjust")
	require.InDelta(t, 0.65, w.Reliability, 1e-9, "0.6 of speed (0.15) moves onto reliability")
	require.InDelta(t, 0.10, w.Speed, 1e-9)
	require.InDelta(t, 0.25, w.Intelligence, 1e-9, "intelligence is untouched")

	// A custom vector adjusts too.
	custom := Weights{Reliability: 0.2, Speed: 0.5, Intelligence: 0.3}
	w, adj = PeakAdjustedWeights(custom, RoutingCustom, cfg, inPeak)
	require.True(t, adj)
	require.InDelta(t, 0.5, w.Reliability, 1e-9) // 0.2 + 0.5*0.6
	require.InDelta(t, 0.2, w.Speed, 1e-9)       // 0.5 - 0.3

	// fastest and reliable are exempt: reweighting them would make one a noisy
	// copy of the other.
	if _, adj := PeakAdjustedWeights(WeightsFastest, RoutingFastest, cfg, inPeak); adj {
		t.Error("fastest must be exempt from the peak shift")
	}
	if _, adj := PeakAdjustedWeights(WeightsReliable, RoutingReliable, cfg, inPeak); adj {
		t.Error("reliable must be exempt from the peak shift")
	}

	// Disabled config or a time outside the window leaves the base untouched.
	off := cfg
	off.Enabled = false
	if _, adj := PeakAdjustedWeights(base, RoutingBalanced, off, inPeak); adj {
		t.Error("a disabled peak config must not adjust")
	}
	narrow := PeakHoursConfig{Enabled: true, StartHour: 18, EndHour: 20, Timezone: "UTC"}
	if _, adj := PeakAdjustedWeights(base, RoutingBalanced, narrow, inPeak); adj {
		t.Error("a time outside the window must not adjust")
	}
}

// TestIsPeakHours covers the window logic: an empty window is nothing, the end
// hour is exclusive, a window whose end precedes its start spans midnight, and
// the hour is read in the configured timezone rather than the host clock.
func TestIsPeakHours(t *testing.T) {
	t.Parallel()
	utc := "UTC"
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 30, 0, 0, time.UTC) }

	if IsPeakHours(PeakHoursConfig{Enabled: true, StartHour: 6, EndHour: 6, Timezone: utc}, at(6)) {
		t.Error("an empty window (start == end) must never be peak")
	}

	day := PeakHoursConfig{Enabled: true, StartHour: 9, EndHour: 17, Timezone: utc}
	require.True(t, IsPeakHours(day, at(12)))
	require.False(t, IsPeakHours(day, at(8)))
	require.False(t, IsPeakHours(day, at(17)), "the end hour is exclusive")

	night := PeakHoursConfig{Enabled: true, StartHour: 18, EndHour: 6, Timezone: utc}
	require.True(t, IsPeakHours(night, at(23)))
	require.True(t, IsPeakHours(night, at(3)))
	require.False(t, IsPeakHours(night, at(12)))

	if _, err := time.LoadLocation("America/New_York"); err == nil {
		ny := PeakHoursConfig{Enabled: true, StartHour: 6, EndHour: 9, Timezone: "America/New_York"}
		require.True(t, IsPeakHours(ny, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)),
			"12:00 UTC is 07:00 in New York (winter), inside a 6-9 local window")
	}
}
