package gateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testMonitor(t *testing.T) (*DegradationMonitor, *time.Time) {
	t.Helper()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m := NewDegradationMonitor()
	m.now = func() time.Time { return now }
	return m, &now
}

// TestOneBadMomentDoesNotDegradeTheFleet is the reason for the entry grace. A
// ratio that dips for a single request must not flip the whole fleet into a
// mode that disables exploration.
func TestOneBadMomentDoesNotDegradeTheFleet(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	bad := HealthSnapshot{Total: 6, Healthy: 1}

	require.Equal(t, StateNormal, m.Observe(bad), "the first bad reading must only start the clock")

	*now = now.Add(degradedEntryGrace - time.Second)
	require.Equal(t, StateNormal, m.Observe(bad), "still inside the grace period")

	*now = now.Add(2 * time.Second)
	require.Equal(t, StateDegraded, m.Observe(bad), "a sustained outage must degrade the fleet")
}

// TestRecoveryResetsTheEntryClock stops a flapping fleet from accumulating
// its way into degraded mode across unrelated dips.
func TestRecoveryResetsTheEntryClock(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	bad := HealthSnapshot{Total: 6, Healthy: 1}
	good := HealthSnapshot{Total: 6, Healthy: 6}

	m.Observe(bad)
	*now = now.Add(degradedEntryGrace - time.Second)
	m.Observe(good) // recovered, so the pending transition is abandoned

	*now = now.Add(2 * time.Second)
	require.Equal(t, StateNormal, m.Observe(bad),
		"a recovery in between must restart the entry clock, not carry it forward")
}

// TestExitIsSlowerThanEntry keeps the gateway from oscillating at exactly the
// moment an operator is trying to understand what is wrong.
func TestExitIsSlowerThanEntry(t *testing.T) {
	t.Parallel()
	require.Greater(t, degradedExitGrace, degradedEntryGrace,
		"leaving degraded mode must be slower than entering it")

	m, now := testMonitor(t)
	bad := HealthSnapshot{Total: 6, Healthy: 1}
	good := HealthSnapshot{Total: 6, Healthy: 6}

	m.Observe(bad)
	*now = now.Add(degradedEntryGrace + time.Second)
	require.Equal(t, StateDegraded, m.Observe(bad))

	*now = now.Add(time.Second)
	require.Equal(t, StateDegraded, m.Observe(good), "one good reading must not lift degraded mode")

	*now = now.Add(degradedExitGrace - 2*time.Second)
	require.Equal(t, StateDegraded, m.Observe(good), "still inside the exit grace")

	*now = now.Add(3 * time.Second)
	require.Equal(t, StateNormal, m.Observe(good), "a sustained recovery must lift it")
}

// TestASmallFleetIsNeverJudged covers the case the ratio cannot describe: with
// two providers, one bad key is 50% and would read as an outage.
func TestASmallFleetIsNeverJudged(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	for _, snapshot := range []HealthSnapshot{
		{Total: 1, Healthy: 0},
		{Total: 2, Healthy: 0},
	} {
		*now = now.Add(degradedEntryGrace * 3)
		require.Equal(t, StateNormal, m.Observe(snapshot),
			"a fleet of %d cannot be judged by ratio", snapshot.Total)
	}

	// At the threshold it becomes judgeable.
	bad := HealthSnapshot{Total: degradedMinProviders, Healthy: 0}
	m.Observe(bad)
	*now = now.Add(degradedEntryGrace + time.Second)
	require.Equal(t, StateDegraded, m.Observe(bad))
}

// TestUnprobedProvidersCountAsHealthy stops a fresh install from declaring
// itself degraded before it has served a single request.
func TestUnprobedProvidersCountAsHealthy(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	// The caller counts unknown-status keys in Healthy, so a brand new
	// install looks fully healthy.
	fresh := HealthSnapshot{Total: 5, Healthy: 5}

	m.Observe(fresh)
	*now = now.Add(degradedEntryGrace * 5)
	require.Equal(t, StateNormal, m.Observe(fresh))

	require.Equal(t, 1.0, HealthSnapshot{}.Ratio(),
		"an empty fleet must not read as zero healthy and degrade")
}

// TestPendingTransitionIsVisible is what lets the dashboard show the gateway
// noticing a problem before it acts on it.
func TestPendingTransitionIsVisible(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	pending, _ := m.PendingTransition()
	require.False(t, pending, "a comfortable fleet has nothing pending")

	m.Observe(HealthSnapshot{Total: 6, Healthy: 1})
	*now = now.Add(20 * time.Second)

	pending, elapsed := m.PendingTransition()
	require.True(t, pending)
	require.Equal(t, 20*time.Second, elapsed)
}

// TestRatioBoundary pins the comparison: exactly at the threshold is healthy,
// below it is not.
func TestRatioBoundary(t *testing.T) {
	t.Parallel()

	m, now := testMonitor(t)
	atThreshold := HealthSnapshot{Total: 6, Healthy: 3} // ratio 0.5

	m.Observe(atThreshold)
	*now = now.Add(degradedEntryGrace * 2)
	require.Equal(t, StateNormal, m.Observe(atThreshold),
		"exactly at the healthy ratio must not degrade")

	below := HealthSnapshot{Total: 6, Healthy: 2} // ratio 0.33
	m.Observe(below)
	*now = now.Add(degradedEntryGrace + time.Second)
	require.Equal(t, StateDegraded, m.Observe(below))
}
