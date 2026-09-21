package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLocalTokenAuthenticatesTheInferencePlane is why the token exists: Prowl
// registers the gateway as its own provider with it, and before this it would
// have been rejected, so Prowl's own agent could not use its own gateway.
func TestLocalTokenAuthenticatesTheInferencePlane(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-unified-key", LocalToken: "local-bootstrap-token"})

	// It gets past authentication; the 503 that follows is the absence of a
	// provider key, which is what proves it authenticated - an unauthenticated
	// request never reaches routing.
	resp, body := do(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			"Authorization": "Bearer local-bootstrap-token",
			"Content-Type":  "application/json",
		})
	require.NotEqual(t, http.StatusUnauthorized, resp.StatusCode,
		"the local token must authenticate the inference plane, body was %s", body)
}

// TestLocalTokenGrantsTheWholeSurface states this port's deliberate deviation:
// with no browser and no session, the machine-local token and the unified key
// are interchangeable credentials for everything. That costs nothing at the
// trust boundary - whoever can read the token file (0600 in the state dir) can
// already read master.key and decrypt every provider credential directly.
func TestLocalTokenGrantsTheWholeSurface(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-unified-key", LocalToken: "local-bootstrap-token"})

	for _, path := range []string{"/api/keys", "/api/settings/api-key"} {
		resp, _ := do(t, s, http.MethodGet, path, "",
			map[string]string{"Authorization": "Bearer local-bootstrap-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"%s must accept the machine-local credential", path)
	}
}

// TestAnEmptyLocalTokenNeverAuthenticates covers the fallback-to-permissive
// shape that CVE-2026-59822 was: an unset credential must not match an absent
// one. A gateway built without a token must reject every presented value.
func TestAnEmptyLocalTokenNeverAuthenticates(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-unified-key"})

	for _, presented := range []string{"", " ", "local-bootstrap-token"} {
		headers := map[string]string{"Content-Type": "application/json"}
		if presented != "" {
			headers["Authorization"] = "Bearer " + presented
		}
		resp, body := do(t, s, http.MethodPost, "/v1/chat/completions",
			`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`, headers)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"an unset local token must authenticate nothing, got %s", body)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
			"an application's bad key must not sign the operator out")
	}
}

// TestBothInferenceCredentialsWork keeps the two independent: rotating or
// regenerating one must not be required to use the other.
func TestBothInferenceCredentialsWork(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-unified-key", LocalToken: "local-bootstrap-token"})

	for _, credential := range []string{"prowl-unified-key", "local-bootstrap-token"} {
		resp, body := do(t, s, http.MethodGet, "/v1/models", "",
			map[string]string{"Authorization": "Bearer " + credential})
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"%s must authenticate /v1, body was %s", credential, body)
	}
}
