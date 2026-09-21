package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/provider"
)

// newInBandErrorUpstream serves HTTP 200 and then reports a failure INSIDE the
// stream, which is how NVIDIA announces an overload.
func newInBandErrorUpstream(t *testing.T, message string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "server_error"},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestInBandStreamErrorFailsOverInsteadOfSurfacing is the bug a live run hit:
// NVIDIA answered 200 and then wrote "service temporarily overloaded" as a
// stream frame. That frame was treated as content, which committed the
// response and made failover impossible, so the user saw a dead stream while
// healthy providers sat unused.
func TestInBandStreamErrorFailsOverInsteadOfSurfacing(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	broken, brokenCalls := newInBandErrorUpstream(t, "Service temporarily overloaded")
	working := newStreamingUpstream(t, "reco", "vered")

	seedRoute(t, s, "overloaded", broken.URL, "test-model", 1)
	seedRoute(t, s, "healthy", working.URL, "test-model-2", 2)
	// Pin the order: the point is that the OVERLOADED provider is tried first
	// and the run still recovers, which a score-ordered chain would not
	// guarantee.
	usePriorityOrder(t, s)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), brokenCalls.Load(), "the overloaded provider must have been tried")
	// The healthy provider streams the answer in separate frames, so assert
	// on the frames rather than on a joined string.
	require.Contains(t, body, `"reco"`,
		"the answer must come from the healthy provider, not the overloaded one")
	require.Contains(t, body, `"vered"`)
	require.NotContains(t, body, "overloaded",
		"the caller must never see the upstream's in-band error after a successful failover")
	require.NotContains(t, body, "stream_error",
		"a pre-commit in-band error is retryable, not a stream failure")
}

// TestInBandErrorOnTheLastCandidateIsReported: when there is nothing left to
// fail over to, the caller must be told - silence or an empty success would be
// worse than an error.
func TestInBandErrorOnTheLastCandidateIsReported(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	broken, _ := newInBandErrorUpstream(t, "Service temporarily overloaded")
	seedRoute(t, s, "only", broken.URL, "test-model", 1)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.NotEqual(t, http.StatusOK, resp.StatusCode,
		"a run where every candidate failed must not report success, body was %s", body)
	require.NotContains(t, body, "authentication_error",
		"a provider outage must not end the operator's dashboard session")
}

// TestAFailedRunIsRecorded is the other half of the same report: after the
// stream broke, the Logs page was empty. Only successes were persisted, so an
// outage produced a failing client next to an empty log - the worst pairing
// for diagnosing it.
func TestAFailedRunIsRecorded(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	broken := newFakeUpstream(t, http.StatusInternalServerError, "")
	seedRoute(t, s, "broken", broken.URL, "test-model", 1)

	_, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	require.NotEmpty(t, body)

	var rows, attempts int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM requests WHERE outcome = 'error'`).Scan(&rows))
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM request_attempts`).Scan(&attempts))

	require.Positive(t, rows, "a wholly failed run must leave a row an operator can find")
	require.Positive(t, attempts, "and its failed hops must be attached to it")

	var platform, kind string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT platform, COALESCE(error_kind, '') FROM requests WHERE outcome = 'error' LIMIT 1`).
		Scan(&platform, &kind))
	require.NotEmpty(t, platform, "the row must name the provider that ended the run")
	require.NotEmpty(t, kind, "and why it ended")
}

// providerChunk builds a stream frame from raw JSON, the way the provider's
// reader delivers one.
func providerChunk(t *testing.T, raw string) *provider.ChatChunk {
	t.Helper()
	var chunk provider.ChatChunk
	require.NoError(t, json.Unmarshal([]byte(raw), &chunk))
	chunk.Raw = []byte(raw)
	return &chunk
}

// TestInBandErrorDetection pins the classifier: a frame carrying content is
// content even if it also carries an error field, or a normal answer would be
// discarded as a failure.
func TestInBandErrorDetection(t *testing.T) {
	t.Parallel()

	errorOnly := providerChunk(t, `{"error":{"message":"overloaded"}}`)
	reason, isErr := inBandStreamError(errorOnly)
	require.True(t, isErr)
	require.Equal(t, "overloaded", reason)

	withContent := providerChunk(t, `{"choices":[{"index":0,"delta":{"content":"hi"}}],"error":{"message":"ignore me"}}`)
	_, isErr = inBandStreamError(withContent)
	require.False(t, isErr, "a frame with content is content")

	plain := providerChunk(t, `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	_, isErr = inBandStreamError(plain)
	require.False(t, isErr)

	vague := providerChunk(t, `{"error":{}}`)
	reason, isErr = inBandStreamError(vague)
	require.True(t, isErr, "an error frame with no detail is still a failure")
	require.NotEmpty(t, reason)
}

// newRoleThenErrorUpstream opens with a role-only frame and then reports a
// failure, which is the exact sequence a live OpenRouter/NVIDIA route produced.
func newRoleThenErrorUpstream(t *testing.T, message string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		opener, _ := json.Marshal(map[string]any{
			"id":      "chatcmpl-role",
			"object":  "chat.completion.chunk",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", opener)
		flush()
		frame, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "server_error"},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestRoleOnlyOpenerDoesNotForfeitFailover covers the harder half of the same
// live failure: the provider first sent a frame carrying nothing but the
// assistant role. Committing on that opener spent the only chance to reroute,
// so the overload notice that followed reached the user as a dead stream --
// and was booked as HTTP 200 in the logs.
func TestRoleOnlyOpenerDoesNotForfeitFailover(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	broken, brokenCalls := newRoleThenErrorUpstream(t, "Service temporarily overloaded")
	working := newStreamingUpstream(t, "still", " works")

	seedRoute(t, s, "overloaded", broken.URL, "test-model", 1)
	seedRoute(t, s, "healthy", working.URL, "test-model-2", 2)
	usePriorityOrder(t, s)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), brokenCalls.Load(), "the overloaded provider must have been tried")
	require.Contains(t, body, "still", "the healthy provider's answer must reach the caller")
	require.Contains(t, body, " works")
	require.NotContains(t, body, "overloaded", "the upstream failure must not surface")
}

// newSilentUpstream accepts the stream, writes the SSE headers, and then sends
// nothing at all until the test ends.
func newSilentUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the connection open with no frames, which is the failure the
		// loop could not previously see.
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestSilentStreamFailsOverBeforeTheClientGivesUp covers a provider that
// accepts a request and then says nothing. Recv never returns, so no error was
// ever classified: the request sat there until the harness's own one-minute
// patience expired and the turn died with a healthy pool behind it.
func TestSilentStreamFailsOverBeforeTheClientGivesUp(t *testing.T) {
	// Not parallel: this test temporarily overrides a package-level timeout.

	s := testServer(t, Options{MachineKey: compatMachineKey})
	silent, silentCalls := newSilentUpstream(t)
	working := newStreamingUpstream(t, "after", " silence")

	seedRoute(t, s, "silent", silent.URL, "test-model", 1)
	seedRoute(t, s, "healthy", working.URL, "test-model-2", 2)
	usePriorityOrder(t, s)

	// The guard is 25s in production; the test drives it directly so it does
	// not have to wait that long.
	restore := streamStallTimeoutForTest(200 * time.Millisecond)
	t.Cleanup(restore)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), silentCalls.Load(), "the silent provider must have been tried")
	require.Contains(t, body, "after", "the healthy provider's answer must reach the caller")
	require.Contains(t, body, " silence")
}

// newStallsAfterContentUpstream sends one real frame and then goes quiet,
// which is what a provider does when it dies mid-generation.
func newStallsAfterContentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		frame, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "partial"},
			}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		flush()
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStallAfterCommitTerminatesAndIsRecorded covers the half of a silent
// provider that failover cannot rescue. Once bytes are out the request cannot
// be rerouted, but parking in Recv is worse than ending it: the caller's own
// timeout killed the turn and the gateway recorded nothing at all, so the
// failure was invisible in the logs.
func TestStallAfterCommitTerminatesAndIsRecorded(t *testing.T) {
	// Not parallel: this test temporarily overrides a package-level timeout.

	s := testServer(t, Options{MachineKey: compatMachineKey})
	stalling := newStallsAfterContentUpstream(t)
	seedRoute(t, s, "stalls", stalling.URL, "test-model", 1)
	usePriorityOrder(t, s)

	restore := streamIdleTimeoutForTest(250 * time.Millisecond)
	t.Cleanup(restore)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "partial", "the bytes already sent must reach the caller")
	require.Contains(t, body, "error", "the stream must be closed with an error frame, not left open")

	// And the run must be on the record as a failure, not booked as a 200.
	var kind string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COALESCE(error_kind, '') FROM requests ORDER BY id DESC LIMIT 1`).Scan(&kind))
	require.Equal(t, "stream_broken", kind,
		"a stream that died after committing must be recorded as broken")
}

// newDripFeedNoContentUpstream keeps sending frames that carry nothing a
// caller can use, which is how a provider can look alive while delivering
// zero output.
func newDripFeedNoContentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-drip", "object": "chat.completion.chunk",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant"},
			}},
		})
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDripFedEmptyFramesDoNotHoldTheRequestOpen is a regression for a guard
// that defeated itself: content-free frames refreshed the first-content
// deadline, so a provider emitting nothing but role announcements kept the
// request alive forever and the caller received zero bytes.
func TestDripFedEmptyFramesDoNotHoldTheRequestOpen(t *testing.T) {
	// Not parallel: this test temporarily overrides a package-level timeout.

	s := testServer(t, Options{MachineKey: compatMachineKey})
	drip := newDripFeedNoContentUpstream(t)
	working := newStreamingUpstream(t, "real", " answer")

	seedRoute(t, s, "drip", drip.URL, "test-model", 1)
	seedRoute(t, s, "healthy", working.URL, "test-model-2", 2)
	usePriorityOrder(t, s)

	restore := streamStallTimeoutForTest(300 * time.Millisecond)
	t.Cleanup(restore)

	resp, body := postCompat(t, s, "/v1/chat/completions",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "real", "the healthy provider's answer must reach the caller")
	require.Contains(t, body, " answer")
}

// TestUnmodelledDeltaFieldCountsAsContent keeps the gateway from swallowing a
// stream whose shape it does not recognise. Reasoning text arrives under
// several different keys across providers, and enumerating the known ones
// meant an unrecognised key read as "no content" and was withheld.
func TestUnmodelledDeltaFieldCountsAsContent(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"content", "reasoning", "reasoning_content",
		"thinking", "reasoning_details"} {
		chunk := &provider.ChatChunk{
			Choices: []provider.ChunkChoice{{
				Delta: json.RawMessage(
					`{"role":"assistant","` + field + `":"something"}`),
			}},
		}
		require.True(t, chunkCarriesContent(chunk),
			"a delta carrying %q must count as content", field)
	}

	roleOnly := &provider.ChatChunk{
		Choices: []provider.ChunkChoice{{
			Delta: json.RawMessage(`{"role":"assistant","content":""}`),
		}},
	}
	require.False(t, chunkCarriesContent(roleOnly),
		"an opening frame with no text must not commit the response")
}
