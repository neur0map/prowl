package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleTrackerFiresAfterIdle(t *testing.T) {
	var fired atomic.Int32
	it := newIdleTracker(50*time.Millisecond, func() {
		fired.Add(1)
	})
	defer it.Stop()

	it.MarkDirty()

	// Wait for idle timer to fire
	time.Sleep(100 * time.Millisecond)

	if fired.Load() != 1 {
		t.Fatalf("expected callback to fire once, got %d", fired.Load())
	}
}

func TestIdleTrackerResetsOnActivity(t *testing.T) {
	var fired atomic.Int32
	it := newIdleTracker(80*time.Millisecond, func() {
		fired.Add(1)
	})
	defer it.Stop()

	it.MarkDirty()
	time.Sleep(50 * time.Millisecond) // 50ms in, not yet fired
	it.MarkDirty()                     // reset timer
	time.Sleep(50 * time.Millisecond) // 50ms more (100ms total, but only 50ms since reset)

	if fired.Load() != 0 {
		t.Fatal("should not have fired yet -- timer was reset")
	}

	time.Sleep(50 * time.Millisecond) // now 100ms since last reset
	if fired.Load() != 1 {
		t.Fatalf("expected callback to fire once, got %d", fired.Load())
	}
}

func TestIdleTrackerNoFireIfNotDirty(t *testing.T) {
	var fired atomic.Int32
	it := newIdleTracker(50*time.Millisecond, func() {
		fired.Add(1)
	})
	defer it.Stop()

	// Don't mark dirty
	time.Sleep(100 * time.Millisecond)

	if fired.Load() != 0 {
		t.Fatal("should not fire if never marked dirty")
	}
}
