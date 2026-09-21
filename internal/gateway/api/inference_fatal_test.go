package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// TestAFatalUpstreamVerdictReachesTheClient covers the failure mode that is
// worst to debug: the request WAS dispatched and the provider explained why it
// refused, but the loop's fatal path carries no exhaustion body - so the reply
// used to read "every provider attempt failed", which says the opposite of
// what happened.
func TestAFatalUpstreamVerdictReachesTheClient(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"max_tokens: must be >= 1","code":"invalid_parameter"}}`)
	}))
	defer upstream.Close()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	usePriorityOrder(t, s)
	seedRoute(t, s, "only", upstream.URL, "only-model", 1)

	resp, body := do(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatMachineKey})

	require.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"the provider's own status must survive: body was %s", body)

	// Decoded, not substring-matched: the JSON encoder escapes `>`.
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.Equal(t, "max_tokens: must be >= 1", envelope.Error.Message,
		"the reply must carry the provider's sentence, unwrapped from its body")
	require.NotContains(t, body, "every provider attempt failed",
		"a dispatched, rejected request must not be reported as if nothing was tried")
}

// TestAFatalFailureIsRecordedAgainstItsProvider keeps the failure visible in
// the product: an unattributed error row cannot be diagnosed, and the fatal
// path leaves no trail hop to attribute it from.
func TestAFatalFailureIsRecordedAgainstItsProvider(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"tools are not supported on this deployment"}}`)
	}))
	defer upstream.Close()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	usePriorityOrder(t, s)
	seedRoute(t, s, "only", upstream.URL, "only-model", 1)

	resp, _ := do(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatMachineKey})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var (
		platform, modelID, message string
		status                     int
	)
	require.NoError(t, s.engine.DB().QueryRow(`
		SELECT platform, model_id, status, error_message
		  FROM requests WHERE outcome = 'error' ORDER BY id DESC LIMIT 1`,
	).Scan(&platform, &modelID, &status, &message))

	require.Equal(t, "custom", platform, "the row must name the provider that refused")
	require.Equal(t, "only-model", modelID, "and the model it refused for")
	require.Equal(t, http.StatusBadRequest, status,
		"the recorded status must be the provider's, not a blanket 502")
	require.Contains(t, message, "tools are not supported",
		"the recorded reason must be the provider's own text")
}

// TestAFatalErrorStillCarriesAnErrorEnvelope keeps the compat surface honest:
// clients parse the envelope, so the new path must not emit a bare body.
func TestAFatalErrorStillCarriesAnErrorEnvelope(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"bad request"}}`)
	}))
	defer upstream.Close()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	usePriorityOrder(t, s)
	seedRoute(t, s, "only", upstream.URL, "only-model", 1)

	_, body := do(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatMachineKey})

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	require.NotEmpty(t, envelope.Error.Message)
	require.NotEmpty(t, envelope.Error.Type)
}

// TestAnUndispatchableCandidateDoesNotHaltTheRun is the rule failover exists
// for: a candidate the gateway cannot reach at all (no adapter for its
// platform) must be stepped over, not treated as the request's verdict.
//
// This was live: enrolling a login seeded models on a platform with no wire
// adapter, and because the miss classified as fatal, a list containing one
// halted the whole request while healthy candidates sat behind it.
func TestAnUndispatchableCandidateDoesNotHaltTheRun(t *testing.T) {
	t.Parallel()

	healthy := newFakeUpstream(t, http.StatusOK, "served")

	s := testServer(t, Options{MachineKey: compatMachineKey})
	usePriorityOrder(t, s)

	// First candidate: a platform with no adapter registered.
	_, err := s.engine.DB().Exec(`
		INSERT INTO models (platform, model_id, display_name, size_label,
			intelligence_rank, enabled, supports_tools)
		VALUES ('nonexistent-wire', 'ghost', 'Ghost', 'Medium', 9, 1, 1)`)
	require.NoError(t, err)
	var ghostID int64
	require.NoError(t, s.engine.DB().QueryRow(
		"SELECT id FROM models WHERE model_id = 'ghost'").Scan(&ghostID))
	_, err = s.engine.Vault().Add("nonexistent-wire", "ghost-key", gateway.AddOptions{Label: "ghost"})
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(
		"INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, 1, 1)", ghostID)
	require.NoError(t, err)

	// Second candidate: reachable.
	seedRoute(t, s, "healthy", healthy.URL, "healthy-model", 2)

	resp, body := do(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + compatMachineKey})

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"the run must advance past the unreachable candidate: body was %s", body)
	require.Contains(t, body, "served")
	require.Equal(t, int64(1), healthy.calls.Load(),
		"the reachable candidate must actually have been called")
}
