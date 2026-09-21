package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// TestNoAdapterHopEntersTrailAndFallbackHeaders proves a candidate rejected
// BEFORE dispatch -- here a platform with no wire adapter -- is booked through
// the same failure epilogue as a dispatched failure. The next attempt succeeds,
// and that success must still advertise and persist the skipped hop; before the
// fix the pre-dispatch return skipped the trail entirely, so the fallback
// disappeared from both the header and the stored trail.
func TestNoAdapterHopEntersTrailAndFallbackHeaders(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	// A working custom endpoint at position 2.
	working := newFakeUpstream(t, http.StatusOK, "served")
	seedRoute(t, s, "ok", working.URL, "m", 2)

	// A candidate on a platform with no registered adapter at position 1. It
	// reuses a real key row so it is a live chain candidate; Resolve fails for
	// the unknown platform, which is the pre-dispatch failure under test.
	var customKeyID int64
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT key_id FROM models WHERE platform = 'custom' AND model_id = 'm'`).Scan(&customKeyID))
	res, err := s.engine.DB().Exec(
		`INSERT INTO models (platform, model_id, display_name, key_id, endpoint_scope,
		                     size_label, intelligence_rank, enabled, supports_tools)
		 VALUES ('ghost', 'm', 'Ghost M', ?, '', 'Medium', 9, 1, 1)`, customKeyID)
	require.NoError(t, err)
	ghostDBID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(
		`INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, 1, 1)`, ghostDBID)
	require.NoError(t, err)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, machineKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "1", resp.Header.Get("X-Fallback-Attempts"),
		"a candidate rejected before dispatch must still count as a fallback attempt")

	var ghostAttempts int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM request_attempts WHERE platform = 'ghost'`).Scan(&ghostAttempts))
	require.Equal(t, 1, ghostAttempts, "the no-adapter hop must be persisted in the stored trail")
}

// TestFallbackHeaderUsesKeyOrdinalNotRawKeyID proves the success path's
// X-Fallback-Trail uses the per-request key ordinal, never the raw key database
// id. The succeeding endpoint is seeded first so it takes key id 1 and the
// failing endpoint takes key id 2, making a raw-id leak ("key2") visibly
// distinct from the sanitised ordinal ("key1").
func TestFallbackHeaderUsesKeyOrdinalNotRawKeyID(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	s := testServer(t, Options{MachineKey: machineKey})

	ok := newFakeUpstream(t, http.StatusOK, "served") // key id 1, position 2
	seedRoute(t, s, "ok", ok.URL, "m", 2)
	bad := newFakeUpstream(t, http.StatusInternalServerError, "") // key id 2, position 1
	seedRoute(t, s, "bad", bad.URL, "m", 1)
	usePriorityOrder(t, s)

	resp, body := postChat(t, s, machineKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	require.Equal(t, "1", resp.Header.Get("X-Fallback-Attempts"))
	trail := resp.Header.Get("X-Fallback-Trail")
	require.NotEmpty(t, trail)
	require.Contains(t, trail, "key1", "the trail must use the per-request key ordinal")
	require.NotContains(t, trail, "key2", "the trail must never leak the raw key database id")
}

// TestFinishInferenceClientGoneRendersNothing proves a client that hung up mid
// flight produces neither a rendered error nor a stored failure row: there is no
// socket to answer, and a request nobody awaited is not a provider outage.
func TestFinishInferenceClientGoneRendersNothing(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})
	rec := httptest.NewRecorder()
	relay := &chatRelay{
		server:     s,
		writer:     rec,
		shaper:     openAIShaper{},
		requestCtx: context.Background(),
	}

	s.finishInference(rec, relay, &gateway.Result{Status: gateway.StatusClientGone})

	require.Empty(t, rec.Body.String(), "a client-gone result must not render an error body")

	var count int
	require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count))
	require.Zero(t, count, "a client-gone result must not record a failed request")
}
