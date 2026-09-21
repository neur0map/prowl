package api

import "time"

// streamStallTimeoutForTest shortens the silent-stream guard and returns a
// function that restores it.
func streamStallTimeoutForTest(d time.Duration) func() {
	previous := streamFirstContentTimeout
	streamFirstContentTimeout = d
	return func() { streamFirstContentTimeout = previous }
}

// streamIdleTimeoutForTest shortens the post-commit idle guard and returns a
// function that restores it.
func streamIdleTimeoutForTest(d time.Duration) func() {
	previous := streamIdleGap
	streamIdleGap = d
	return func() { streamIdleGap = previous }
}
