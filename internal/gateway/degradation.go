package gateway

import (
	"sync"
	"time"
)

// Degraded mode, ported from FreeLLMAPI
// (github.com/tashfeenahmed/freellmapi, MIT, v0.9.9 - see NOTICE.md;
// server/src/services/degradation.ts).
//
// This is a fleet-level judgement, distinct from any single provider being
// benched: when most of the configured providers are unhealthy at once, the
// cause is usually local - no network, a captive portal, a dead DNS resolver.
// In that state, exploring unmeasured models is worse than useless. It spends
// the retry budget proving the obvious and poisons the reliability posteriors
// of models that were never at fault.
//
// Both transitions are deliberately slow. A ratio that dips for one request
// must not flip the fleet, and a recovery must hold before exploration
// resumes, or the gateway oscillates between modes at exactly the moment an
// operator is trying to understand what is wrong.
const (
	// degradedHealthyRatio is the share of providers that must be healthy to
	// stay in normal mode (degradation.ts:50).
	degradedHealthyRatio = 0.5

	// degradedMinProviders is the fleet size below which the ratio is not
	// meaningful: with two providers, one bad key is 50% and would look like
	// an outage (degradation.ts:53).
	degradedMinProviders = 3

	// degradedEntryGrace and degradedExitGrace are how long a condition must
	// hold before the mode changes (degradation.ts:56,59).
	degradedEntryGrace = 60 * time.Second
	degradedExitGrace  = 120 * time.Second
)

// DegradationState is the fleet's current mode.
type DegradationState string

const (
	// StateNormal is healthy operation.
	StateNormal DegradationState = "normal"
	// StateDegraded means most providers are unhealthy at once.
	StateDegraded DegradationState = "degraded"
)

// HealthSnapshot is what the monitor is told about the fleet.
type HealthSnapshot struct {
	// Total is the number of configured providers; Healthy is how many are
	// usable right now. A provider of unknown status counts as healthy:
	// "not yet probed" is not evidence of failure, and treating it as one
	// would declare a fresh install degraded before it served a request.
	Total   int
	Healthy int
}

// Ratio is the healthy share, or 1 when nothing is configured.
func (h HealthSnapshot) Ratio() float64 {
	if h.Total <= 0 {
		return 1
	}
	return float64(h.Healthy) / float64(h.Total)
}

// DegradationMonitor tracks the fleet mode with hysteresis.
type DegradationMonitor struct {
	mu    sync.Mutex
	now   func() time.Time
	state DegradationState

	// since marks when the current candidate transition began. Zero means the
	// fleet is comfortably in its present state.
	since time.Time
}

// NewDegradationMonitor returns a monitor in normal mode.
func NewDegradationMonitor() *DegradationMonitor {
	return &DegradationMonitor{now: time.Now, state: StateNormal}
}

// Observe feeds the monitor a fleet snapshot and returns the resulting mode.
func (d *DegradationMonitor) Observe(h HealthSnapshot) DegradationState {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()

	// Too small a fleet to judge: one bad key out of two is not an outage.
	if h.Total < degradedMinProviders {
		d.state, d.since = StateNormal, time.Time{}
		return d.state
	}

	unhealthy := h.Ratio() < degradedHealthyRatio

	switch d.state {
	case StateNormal:
		if !unhealthy {
			d.since = time.Time{}
			return d.state
		}
		if d.since.IsZero() {
			d.since = now
			return d.state
		}
		if now.Sub(d.since) >= degradedEntryGrace {
			d.state, d.since = StateDegraded, time.Time{}
		}
	case StateDegraded:
		if unhealthy {
			d.since = time.Time{}
			return d.state
		}
		if d.since.IsZero() {
			d.since = now
			return d.state
		}
		if now.Sub(d.since) >= degradedExitGrace {
			d.state, d.since = StateNormal, time.Time{}
		}
	}
	return d.state
}

// State reports the current mode without feeding an observation.
func (d *DegradationMonitor) State() DegradationState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// IsDegraded is the question the router asks before exploring an unmeasured
// model: in a degraded fleet, exploration spends the retry budget proving
// what is already known and blames models that were never at fault.
func (d *DegradationMonitor) IsDegraded() bool {
	return d.State() == StateDegraded
}

// PendingTransition reports whether a change is being timed out, and how long
// it has been pending. The dashboard shows this so an operator can see the
// gateway noticing a problem before it acts on it.
func (d *DegradationMonitor) PendingTransition() (bool, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.since.IsZero() {
		return false, 0
	}
	return true, d.now().Sub(d.since)
}
