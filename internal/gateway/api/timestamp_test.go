package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// dateOnWire matches the reference's TEXT timestamp format.
var dateOnWire = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

// TestKeyTimestampsAreDateStrings is a regression test for a crash, not a
// formatting preference. Prowl stores these as Unix integers, but the vendored
// client calls string methods on them - shipping a number made the whole Keys
// page fail with "e.includes is not a function" as soon as one key existed.
func TestKeyTimestampsAreDateStrings(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	_, err := s.engine.Vault().Add("google", "AIza-placeholder-key-value", gateway.AddOptions{Label: "demo"})
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/keys", "", authed(token))

	var keys []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &keys), "body was %s", body)
	require.Len(t, keys, 1)

	created, ok := keys[0]["createdAt"].(string)
	require.True(t, ok, "createdAt must be a date string, got %T", keys[0]["createdAt"])
	require.Regexp(t, dateOnWire, created)
}

// TestProfileTimestampsAreDateStrings covers the other surfaces that share
// the reference's TEXT format, so the same crash cannot reappear there.
func TestProfileTimestampsAreDateStrings(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	_, body := do(t, s, http.MethodPost, "/api/profiles", `{"name":"chain-a"}`, authed(token))
	require.NotEmpty(t, body)
	_, body = do(t, s, http.MethodGet, "/api/profiles", "", authed(token))
	var chains []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &chains), "body was %s", body)
	require.NotEmpty(t, chains)
	value, ok := chains[0]["created_at"].(string)
	require.True(t, ok, "created_at must be a date string, got %T", chains[0]["created_at"])
	require.Regexp(t, dateOnWire, value)
}

// TestRoutingChainIsSeeded guards the bug that made the Models page show no
// models even with a provider key: fallback_config IS the chain the page
// renders, and an empty one reports every model as fallbackEnabled false.
func TestRoutingChainIsSeeded(t *testing.T) {
	t.Parallel()

	s := testSeededServer(t, Options{MachineKey: compatMachineKey})

	var chained, enabled int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM fallback_config WHERE enabled = 1`).Scan(&chained))
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM models WHERE enabled = 1`).Scan(&enabled))

	require.Positive(t, chained, "a seeded install must have a routable chain")
	require.Equal(t, enabled, chained,
		"every enabled catalog model belongs to the default chain")
}
