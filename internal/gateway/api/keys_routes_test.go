package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// session returns the bearer the gated routes accept: the pinned machine key
// of the harness (see keyedServer in server_test.go).
func session(t *testing.T, s *Server) string {
	t.Helper()
	return compatMachineKey
}

// TestKeysSurfaceRequiresACredential keeps the credential surface gated: an
// unauthenticated read is refused without leaking key metadata.
func TestKeysSurfaceRequiresACredential(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	resp, body := do(t, s, http.MethodGet, "/api/keys", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
}

// TestListedKeyIsMaskedAndCarriesNoPlaintext is the leak test: a key list must
// mask every credential and never emit the plaintext anywhere in the JSON.
func TestListedKeyIsMaskedAndCarriesNoPlaintext(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	const secret = "sk-supersecret-plaintext-0xDEADBEEF01234567"
	id, err := s.engine.Vault().Add("groq", secret, gateway.AddOptions{Label: "prod"})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/keys", "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, body, secret, "the plaintext must never appear in a list")

	var keys []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &keys))
	require.Len(t, keys, 1)
	require.EqualValues(t, id, keys[0]["id"])
	require.Equal(t, "groq", keys[0]["platform"])

	masked, _ := keys[0]["maskedKey"].(string)
	require.NotEmpty(t, masked)
	require.NotEqual(t, secret, masked)
	// cooldowns is always an array, never null: the client maps over it.
	_, ok := keys[0]["cooldowns"].([]any)
	require.True(t, ok, "cooldowns must serialise as an array")
}

// TestRevealReturnsPlaintextOnlyOnItsEndpoint proves the reveal endpoint is the
// one path that hands back a credential in the clear.
func TestRevealReturnsPlaintextOnlyOnItsEndpoint(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	const secret = "sk-reveal-me-9f8e7d6c5b4a"
	id, err := s.engine.Vault().Add("groq", secret, gateway.AddOptions{})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodPost, keyPath(id, "/reveal"), "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]string
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, secret, out["key"], "reveal must return the exact plaintext")
}

// TestAddKeyRejectsUnknownPlatform: an add for a platform no provider serves is
// refused with a message that names the offending platform.
func TestAddKeyRejectsUnknownPlatform(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/keys",
		`{"platform":"totally-made-up","key":"sk-x"}`, authed(token))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "totally-made-up", "the rejection must name the bad platform")
}

// TestValidatingUpstreamRejectedKeyIsNotAnAuthError is the trap this slice
// turns on: probing a key relays the provider's verdict. A provider-confirmed
// bad credential must come back as a 200 saying status "error", NEVER as a 401
// carrying authentication_error - that single combination is what signs the
// operator out, and testing a bad key must not do it.
func TestValidatingUpstreamRejectedKeyIsNotAnAuthError(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key"}}`))
	}))
	defer upstream.Close()

	id, err := s.engine.Vault().Add("custom", "sk-bad", gateway.AddOptions{BaseURL: upstream.URL})
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodPost, healthCheckPath(id), "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "a relayed 401 must not become a 401 here")
	require.NotContains(t, body, string(TypeAuthentication),
		"a relayed upstream rejection must not carry authentication_error")

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, string(gateway.StatusError), out["status"])

	// A single confirmed-bad probe records the verdict but leaves the key
	// enabled: it takes three in a row to auto-disable.
	row, ok, err := s.engine.Vault().Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, row.Enabled, "one bad probe must not disable the key")
	require.Equal(t, gateway.StatusError, row.Status)
}

// TestInconclusiveValidationDoesNotDisableTheKey: a transport failure says
// nothing about the credential, so repeated unreachable probes must never flip
// the status or climb the auto-disable counter.
func TestInconclusiveValidationDoesNotDisableTheKey(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	// A server that is closed immediately: every probe fails to connect, which
	// is inconclusive, not a rejection.
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := down.URL
	down.Close()

	id, err := s.engine.Vault().Add("custom", "sk-maybe", gateway.AddOptions{BaseURL: unreachable})
	require.NoError(t, err)

	for range 4 {
		resp, body := do(t, s, http.MethodPost, healthCheckPath(id), "", authed(token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &out))
		require.Equal(t, string(gateway.StatusUnknown), out["status"],
			"an inconclusive probe must leave the status untouched")
	}

	row, ok, err := s.engine.Vault().Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, row.Enabled, "an unreachable provider must never disable a key")
	require.Equal(t, gateway.StatusUnknown, row.Status)
	require.Equal(t, 0, row.ConsecutiveFailures, "inconclusive probes must not climb the disable counter")
}

// TestDeletingAKeyRemovesItsRows deletes the credential and, through the
// ON DELETE CASCADE on models.key_id, its custom relay models with it.
func TestDeletingAKeyRemovesItsRows(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	id, err := s.engine.Vault().Add("custom", "sk-c", gateway.AddOptions{BaseURL: "http://127.0.0.1:59999/v1"})
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(
		`INSERT INTO models(platform, model_id, display_name, key_id, endpoint_scope) VALUES('custom','m1','M1',?,'ep')`, id)
	require.NoError(t, err)

	resp, _ := do(t, s, http.MethodDelete, keyPath(id, ""), "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, found, err := s.engine.Vault().Get(context.Background(), id)
	require.NoError(t, err)
	require.False(t, found, "the key row must be gone")

	var n int
	require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM models WHERE key_id = ?`, id).Scan(&n))
	require.Equal(t, 0, n, "the key's custom models must cascade away with it")
}

// TestClearingCooldownsLiftsOnlyHeuristicBenches: the clear operation withdraws
// our own guesses and nothing else, so a provider-stated or out-of-credit bench
// survives the button press.
func TestClearingCooldownsLiftsOnlyHeuristicBenches(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	id, err := s.engine.Vault().Add("groq", "sk-g", gateway.AddOptions{})
	require.NoError(t, err)
	for _, m := range []string{"llama-3", "mixtral"} {
		_, err := s.engine.DB().Exec(
			`INSERT INTO models(platform, model_id, display_name) VALUES('groq',?,?)`, m, m)
		require.NoError(t, err)
	}

	cds := s.engine.Cooldowns()
	cds.Bench(gateway.QuotaKey("groq", "llama-3", id), time.Hour, gateway.SourceHeuristic)
	cds.Bench(gateway.QuotaKey("groq", "mixtral", id), 24*time.Hour, gateway.SourceCredit)

	resp, body := do(t, s, http.MethodDelete, keyPath(id, "/cooldowns"), "", authed(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]int
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.Equal(t, 1, out["cleared"], "only the heuristic bench is ours to lift")

	_, heuristicStillActive := cds.Active(gateway.QuotaKey("groq", "llama-3", id))
	require.False(t, heuristicStillActive, "the heuristic bench must be gone")
	credit, creditStillActive := cds.Active(gateway.QuotaKey("groq", "mixtral", id))
	require.True(t, creditStillActive, "an out-of-credit bench must survive a clear")
	require.Equal(t, gateway.SourceCredit, credit.Source)
}

// TestListedKeySurfacesAnActiveCooldown proves the list explains why a healthy
// enabled key is idle: its active bench appears in the cooldowns array with a
// live remaining time.
func TestListedKeySurfacesAnActiveCooldown(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	id, err := s.engine.Vault().Add("groq", "sk-idle", gateway.AddOptions{})
	require.NoError(t, err)
	_, err = s.engine.DB().Exec(`INSERT INTO models(platform, model_id, display_name) VALUES('groq','llama-3','Llama 3')`)
	require.NoError(t, err)
	s.engine.Cooldowns().Bench(gateway.QuotaKey("groq", "llama-3", id), 10*time.Minute, gateway.SourceHeuristic)

	_, body := do(t, s, http.MethodGet, "/api/keys", "", authed(token))
	var keys []struct {
		Cooldowns []struct {
			ModelID     string `json:"modelId"`
			RemainingMs int64  `json:"remainingMs"`
		} `json:"cooldowns"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &keys))
	require.Len(t, keys, 1)
	require.Len(t, keys[0].Cooldowns, 1)
	require.Equal(t, "llama-3", keys[0].Cooldowns[0].ModelID)
	require.Positive(t, keys[0].Cooldowns[0].RemainingMs, "an active bench must report time left")
}

// TestProviderCatalogueListsEveryRegisteredPlatform: the directory must show
// every built-in provider and exclude the per-key custom placeholder.
func TestProviderCatalogueListsEveryRegisteredPlatform(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	_, body := do(t, s, http.MethodGet, "/api/keys/providers", "", authed(token))
	var out struct {
		Providers []struct {
			Platform string `json:"platform"`
			Name     string `json:"name"`
		} `json:"providers"`
		Summary map[string]int `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))

	registered := map[string]bool{}
	for _, p := range s.engine.Registry().All() {
		if p.Platform() == "custom" {
			continue // a per-key placeholder, excluded from the checklist
		}
		registered[p.Platform()] = true
	}
	require.Equal(t, len(registered), len(out.Providers), "every registered platform must be listed")

	got := map[string]bool{}
	for _, p := range out.Providers {
		require.NotEqual(t, "custom", p.Platform, "custom is not a checklist provider")
		got[p.Platform] = true
	}
	for platform := range registered {
		require.True(t, got[platform], "missing platform %q from the catalogue", platform)
	}
	require.Equal(t, len(out.Providers), out.Summary["total"])
}

// TestHealthRollupReportsHonestNullsAndDegradation covers the health rollup's
// two quiet contracts: a never-probed key reports lastCheckedAt as null rather
// than 0, and the ported-out quota view is an honest empty array while the
// degradation snapshot is computed live but never enters degraded mode.
func TestHealthRollupReportsHonestNullsAndDegradation(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	token := session(t, s)

	_, err := s.engine.Vault().Add("groq", "sk-h", gateway.AddOptions{Label: "prod"})
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/health", "", authed(token))
	var out struct {
		Platforms []struct {
			Platform    string `json:"platform"`
			HasProvider bool   `json:"hasProvider"`
			TotalKeys   int    `json:"totalKeys"`
			UnknownKeys int    `json:"unknownKeys"`
		} `json:"platforms"`
		Keys []struct {
			Status        string `json:"status"`
			LastCheckedAt *int64 `json:"lastCheckedAt"`
		} `json:"keys"`
		QuotaStates []any `json:"quotaStates"`
		Degradation struct {
			State            string  `json:"state"`
			TotalProviders   int     `json:"totalProviders"`
			HealthyProviders int     `json:"healthyProviders"`
			Ratio            float64 `json:"ratio"`
		} `json:"degradation"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))

	require.Len(t, out.Platforms, 1)
	require.Equal(t, "groq", out.Platforms[0].Platform)
	require.True(t, out.Platforms[0].HasProvider)
	require.Equal(t, 1, out.Platforms[0].TotalKeys)
	require.Equal(t, 1, out.Platforms[0].UnknownKeys)

	require.Len(t, out.Keys, 1)
	require.Equal(t, "unknown", out.Keys[0].Status)
	require.Nil(t, out.Keys[0].LastCheckedAt, "a never-probed key must report null, not 0")

	require.NotNil(t, out.QuotaStates)
	require.Empty(t, out.QuotaStates, "the quota view is honestly empty, not fabricated")

	// An unknown key counts as usable, so the sole provider is healthy and the
	// gateway is not degraded.
	require.Equal(t, "normal", out.Degradation.State)
	require.Equal(t, 1, out.Degradation.TotalProviders)
	require.Equal(t, 1, out.Degradation.HealthyProviders)
	require.Equal(t, 1.0, out.Degradation.Ratio)
}

func keyPath(id int64, suffix string) string {
	return "/api/keys/" + strconv.FormatInt(id, 10) + suffix
}

func healthCheckPath(id int64) string {
	return "/api/health/check/" + strconv.FormatInt(id, 10)
}

// TestDiscoveryUpsertsModelsWithoutMarkingHealthy is the key-add discovery
// proof: a successful authenticated /models list turns into operator-discovered
// rows so a provider that shipped none becomes routable, while the credential
// stays unknown - listing models is not evidence the key works, and several
// providers serve /models publicly.
func TestDiscoveryUpsertsModelsWithoutMarkingHealthy(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})

	const secret = "sk-discover-abcdef0123456789"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/models" {
			http.NotFound(w, req)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"disc-a","name":"Disc A"},{"id":"disc-b"}]}`)
	}))
	defer upstream.Close()

	id, err := s.engine.Vault().Add("custom", secret, gateway.AddOptions{BaseURL: upstream.URL})
	require.NoError(t, err)

	s.discoverModels(context.Background(), "custom", id)

	var count int
	require.NoError(t, s.engine.DB().QueryRow(
		`SELECT COUNT(*) FROM models WHERE platform='custom' AND source='discovered'`).Scan(&count))
	require.Equal(t, 2, count, "a live list must upsert the advertised models as discovered rows")

	row, ok, err := s.engine.Vault().Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, gateway.StatusUnknown, row.Status, "discovery must never fabricate health")
}

// TestDiscoveryOnAuthFailureInsertsNothing proves an unusable credential is
// never rewarded with model rows: a rejected key discovers nothing and still is
// not marked healthy.
func TestDiscoveryOnAuthFailureInsertsNothing(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"revoked"}}`)
	}))
	defer upstream.Close()

	id, err := s.engine.Vault().Add("custom", "sk-rejected-key", gateway.AddOptions{BaseURL: upstream.URL})
	require.NoError(t, err)

	s.discoverModels(context.Background(), "custom", id)

	var count int
	require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM models`).Scan(&count))
	require.Zero(t, count, "an auth failure must insert no models")

	row, ok, err := s.engine.Vault().Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, gateway.StatusUnknown, row.Status, "a rejected credential must not become healthy")
}

// TestDiscoveredModelsBecomeRoutable is the integration guard for the routing
// gate: a discovered model must join the fallback chain and every profile so it
// is actually selectable, not merely present in the models table. It proves the
// resolved active chain contains the discovered ids.
func TestDiscoveredModelsBecomeRoutable(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	db := s.engine.DB()

	// An inactive profile: discovery must still add the models to its list, so
	// the chain routes them whether or not a profile is later activated.
	pr, err := db.Exec(`INSERT INTO profiles (name, active, created_at) VALUES ('disc-profile', 0, ?)`, time.Now().Unix())
	require.NoError(t, err)
	profileID, err := pr.LastInsertId()
	require.NoError(t, err)

	const secret = "sk-routable-0123456789abcdef"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"route-a"},{"id":"route-b"}]}`)
	}))
	defer upstream.Close()

	id, err := s.engine.Vault().Add("custom", secret, gateway.AddOptions{BaseURL: upstream.URL})
	require.NoError(t, err)

	s.discoverModels(context.Background(), "custom", id)

	// Both discovered models are in the global fallback chain, enabled.
	var chained int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM fallback_config fc
		  JOIN models m ON m.id = fc.model_db_id
		 WHERE m.source = 'discovered' AND fc.enabled = 1`).Scan(&chained))
	require.Equal(t, 2, chained, "discovered models must join the fallback chain, enabled")

	// And in the existing profile's membership.
	var inProfile int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM profile_models pm
		  JOIN models m ON m.id = pm.model_db_id
		 WHERE pm.profile_id = ? AND m.source = 'discovered'`, profileID).Scan(&inProfile))
	require.Equal(t, 2, inProfile, "discovered models must join every profile's list")

	// The resolved active chain (no profile active -> the global fallback
	// chain) actually contains them: proof of end-to-end selectability.
	resolved, err := gateway.ResolveChain(db, "", gateway.DefaultRoutingStrategy)
	require.NoError(t, err)
	routed := map[string]bool{}
	for _, e := range resolved.Chain {
		routed[e.ModelID] = true
	}
	require.True(t, routed["route-a"] && routed["route-b"],
		"the resolved chain must contain the discovered models, got %v", routed)
}

// TestDiscoveredModelsGetConservativeRanks guards the scoring trap: the schema
// defaults intelligence_rank/speed_rank to 0, and the scorer treats a lower
// ordinal rank as more capable, so an unknown model left at 0 would look like
// the best in its tier. Discovery must seed the catalogue's worst rank + 1 so
// an unknown model sorts at the bottom, never the top.
func TestDiscoveredModelsGetConservativeRanks(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	db := s.engine.DB()

	// A strong catalogue model: low ranks mean high capability.
	_, err := db.Exec(`INSERT INTO models
		(platform, model_id, display_name, intelligence_rank, speed_rank, size_label, enabled, source)
		VALUES ('groq', 'strong', 'Strong', 5, 7, 'Large', 1, 'catalog')`)
	require.NoError(t, err)

	const secret = "sk-ranks-0123456789abcdef"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"unknown-a"},{"id":"unknown-b"}]}`)
	}))
	defer upstream.Close()

	id, err := s.engine.Vault().Add("custom", secret, gateway.AddOptions{BaseURL: upstream.URL})
	require.NoError(t, err)

	s.discoverModels(context.Background(), "custom", id)

	var minIntel, minSpeed int
	require.NoError(t, db.QueryRow(
		`SELECT MIN(intelligence_rank), MIN(speed_rank) FROM models WHERE source = 'discovered'`).
		Scan(&minIntel, &minSpeed))
	// MAX over the catalogue was 5 (intel) / 7 (speed), so discovered rows seed 6 / 8.
	require.Equal(t, 6, minIntel, "discovered intelligence rank must be catalogue MAX+1, not 0")
	require.Equal(t, 8, minSpeed, "discovered speed rank must be catalogue MAX+1, not 0")
	require.Greater(t, minIntel, 5, "an unknown model must never out-rank a real catalogue model")
}
