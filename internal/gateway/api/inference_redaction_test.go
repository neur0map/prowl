package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// TestAProviderCannotEchoItsKeyBackToTheCaller is the exposure case that
// matters most on a gateway holding many credentials. Providers routinely
// quote the rejected key in their error body. That body is the text the
// router relays, so without scrubbing, a caller who holds only their own
// gateway token would read a working provider key out of an error response.
//
// The key planted here is deliberately shapeless - no sk-, no gsk_, nothing
// the patterns recognise - because a custom endpoint's key is whatever its
// operator chose. Only exact-value redaction can catch it.
func TestAProviderCannotEchoItsKeyBackToTheCaller(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	const planted = "test-key-leaky"

	s := testServer(t, Options{MachineKey: machineKey})

	// The upstream rejects, quoting the credential it was given.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w,
			`{"error":{"message":"Invalid API key: %s. Check your credentials."}}`,
			r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)

	seedRoute(t, s, "leaky", upstream.URL, "test-model", 1)

	_, body := postChat(t, s, machineKey, chatBody)

	require.NotContains(t, body, planted,
		"a provider's error body must never carry its key to the caller")
	require.NotContains(t, body, "Bearer "+planted,
		"nor inside an Authorization header the provider quoted back")
}

// TestAFailedHealthCheckDoesNotStoreTheKey covers the same leak on the path
// that persists rather than relays: last_health_error is written to the
// database and shown on the keys page, so an unredacted provider rejection
// would leave a working credential on disk indefinitely.
func TestAFailedHealthCheckDoesNotStoreTheKey(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"rejected key %s"}}`,
			r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)

	keyID, err := s.engine.Vault().Add("custom", "test-key-leaky-health",
		gateway.AddOptions{Label: "leaky-health", BaseURL: upstream.URL})
	require.NoError(t, err)

	_, _ = s.engine.Vault().CheckKey(context.Background(), keyID)

	var stored *string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT last_health_error FROM api_keys WHERE id = ?`, keyID).Scan(&stored))
	if stored != nil {
		require.NotContains(t, *stored, "test-key-leaky-health",
			"a rejected key must not be stored in its own health error")
	}
}

// TestAFailedAttemptDoesNotStoreTheKeyInTheTrail covers the persisted failover
// trail. A provider that quotes its rejected credential back becomes a
// request_attempts row -- the Logs view and any trail reader render it. The
// planted key is shapeless, so only exact-value redaction with the keys this
// request used can scrub it; the shape patterns never match it.
func TestAFailedAttemptDoesNotStoreTheKeyInTheTrail(t *testing.T) {
	t.Parallel()

	const machineKey = "prowl-test-machine-key"
	const planted = "test-key-leaky-attempt" // seedRoute mints "test-key-" + label

	s := testServer(t, Options{MachineKey: machineKey})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w,
			`{"error":{"message":"Invalid API key: %s. Check your credentials."}}`,
			r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)

	seedRoute(t, s, "leaky-attempt", upstream.URL, "test-model", 1)

	resp, _ := postChat(t, s, machineKey, chatBody)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)

	var attempts int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM request_attempts`).Scan(&attempts))
	require.Positive(t, attempts, "a failed hop must leave a request_attempts row to redact")

	var stored string
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COALESCE(error_message, '') FROM request_attempts ORDER BY id DESC LIMIT 1`).Scan(&stored))
	require.NotContains(t, stored, planted,
		"a rejected key must never be persisted in its own attempt's error message")
	require.NotContains(t, stored, "Bearer "+planted)
}
