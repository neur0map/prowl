package daemon

import (
	"sync"
	"time"
)

// idleTracker fires a callback after a period of no activity.
// Each call to MarkDirty resets the timer.
type idleTracker struct {
	timeout  time.Duration
	callback func()
	timer    *time.Timer
	dirty    bool
	mu       sync.Mutex
	stopped  bool
}

func newIdleTracker(timeout time.Duration, callback func()) *idleTracker {
	return &idleTracker{
		timeout:  timeout,
		callback: callback,
	}
}

// MarkDirty signals that work happened. Resets the idle timer.
func (it *idleTracker) MarkDirty() {
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.stopped {
		return
	}

	it.dirty = true

	if it.timer != nil {
		it.timer.Stop()
	}
	it.timer = time.AfterFunc(it.timeout, func() {
		it.mu.Lock()
		shouldFire := it.dirty && !it.stopped
		if shouldFire {
			it.dirty = false
		}
		it.mu.Unlock()
		// Run callback outside lock to avoid holding it during expensive work
		if shouldFire {
			it.callback()
		}
	})
}

// Stop cancels the timer and prevents future callbacks.
func (it *idleTracker) Stop() {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.stopped = true
	if it.timer != nil {
		it.timer.Stop()
		it.timer = nil
	}
}
