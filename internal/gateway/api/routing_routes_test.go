package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/gateway/catalog"
	"github.com/stretchr/testify/require"
)

// routingServer builds a session-authenticated surface for the routing family.
// It presents the token in the session header rather than as a bearer, which
// is the transport the dashboard itself uses.
func routingServer(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s, headers := keyedServer(t)
	return s, headers
}

// seedModel adds a catalogue model and makes sure its platform has a usable
// key. A chain candidate is a (model, key) pair, so a model whose platform
// holds no credential is correctly absent from every chain.
func seedModel(t *testing.T, db *sql.DB, platform, modelID, name, sizeLabel string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO models (platform, model_id, display_name, size_label, enabled) VALUES (?, ?, ?, ?, 1)`,
		platform, modelID, name, sizeLabel)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)

	var keys int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE platform = ?`, platform).Scan(&keys))
	if keys == 0 {
		_, err = db.Exec(`
			INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
			VALUES (?, 'test', 'x', 'y', 'z', 'healthy', 1, 0)`, platform)
		require.NoError(t, err)
	}
	return id
}

func seedFallback(t *testing.T, db *sql.DB, modelDBID int64, position int64, enabled bool) {
	t.Helper()
	e := 0
	if enabled {
		e = 1
	}
	_, err := db.Exec(`INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, ?, ?)`,
		modelDBID, position, e)
	require.NoError(t, err)
}

func seedProfile(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO profiles (name, active, created_at) VALUES (?, 0, ?)`, name, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestRoutingStrategyPersistsAndReadsBack: a valid strategy round-trips through
// the settings table and is reported on the next read.
func TestRoutingStrategyPersistsAndReadsBack(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)

	resp, body := do(t, s, http.MethodPut, "/api/fallback/routing", `{"strategy":"smartest"}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	resp, body = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var data struct {
		Strategy string      `json:"strategy"`
		Weights  *weightsOut `json:"weights"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Equal(t, "smartest", data.Strategy)
	// smartest is a bandit preset, so weights are the preset vector, not null.
	require.NotNil(t, data.Weights)
	require.InDelta(t, 0.55, data.Weights.Intelligence, 1e-9)
}

// TestUnknownStrategyIsRejected: a typo is a 400, not a silent coercion to the
// default - the operator must never think they set one thing while the router
// does another. The stored strategy is left untouched.
func TestUnknownStrategyIsRejected(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)

	_, _ = do(t, s, http.MethodPut, "/api/fallback/routing", `{"strategy":"reliable"}`, auth)

	resp, body := do(t, s, http.MethodPut, "/api/fallback/routing", `{"strategy":"turbo"}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))

	_, body = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	var data struct {
		Strategy string `json:"strategy"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Equal(t, "reliable", data.Strategy, "a rejected strategy must not overwrite the saved one")
}

// TestCustomWeightsNormalizeAndRejectAllZero: a custom vector is renormalised to
// sum one, and an all-zero vector is refused rather than dividing by zero.
func TestCustomWeightsNormalizeAndRejectAllZero(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)

	resp, body := do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"custom","weights":{"reliability":2,"speed":1,"intelligence":1}}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	_, body = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	var data struct {
		CustomWeights weightsOut `json:"customWeights"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.InDelta(t, 0.5, data.CustomWeights.Reliability, 1e-9)
	require.InDelta(t, 0.25, data.CustomWeights.Speed, 1e-9)
	require.InDelta(t, 1.0,
		data.CustomWeights.Reliability+data.CustomWeights.Speed+data.CustomWeights.Intelligence, 1e-9)

	resp, _ = do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"custom","weights":{"reliability":0,"speed":0,"intelligence":0}}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestChainReadPrefersActiveProfileOverGlobalConfig: with a profile active the
// read reflects the profile's membership and order, not the global
// fallback_config, so the dashboard never shows one chain's rows under another.
func TestChainReadPrefersActiveProfileOverGlobalConfig(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	m2 := seedModel(t, db, "p", "model-2", "Two", "Large")
	m3 := seedModel(t, db, "p", "model-3", "Three", "Large")
	// Global chain order: m1, m2 (m3 not in it).
	seedFallback(t, db, m1, 1, true)
	seedFallback(t, db, m2, 2, true)

	type feRow struct {
		ModelDBID int64 `json:"modelDbId"`
		Enabled   bool  `json:"enabled"`
	}
	enabledOrder := func(body string) []int64 {
		var rows []feRow
		require.NoError(t, json.Unmarshal([]byte(body), &rows))
		var out []int64
		for _, r := range rows {
			if r.Enabled {
				out = append(out, r.ModelDBID)
			}
		}
		return out
	}

	// Before any profile is active the read is the global chain.
	_, body := do(t, s, http.MethodGet, "/api/fallback", "", auth)
	require.Equal(t, []int64{m1, m2}, enabledOrder(body))

	// A profile with a different membership and order: m3, then m1.
	p := seedProfile(t, db, "coding")
	_, err := db.Exec(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, 1), (?, ?, 2)`,
		p, m3, p, m1)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodPost, "/api/profiles/active", fmt.Sprintf(`{"profileId":%d}`, p), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	_, body = do(t, s, http.MethodGet, "/api/fallback", "", auth)
	require.Equal(t, []int64{m3, m1}, enabledOrder(body),
		"the active profile's chain must win over the global fallback_config")
}

func TestModelPatchControlsStateAndVisibleChainIndependently(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	modelID := seedModel(t, db, "p", "model-1", "One", "Large")
	profileID := seedProfile(t, db, "coding")

	resp, body := do(t, s, http.MethodPost, "/api/profiles/active",
		fmt.Sprintf(`{"profileId":%d}`, profileID), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	patch := func(body string) {
		resp, responseBody := do(t, s, http.MethodPatch,
			fmt.Sprintf("/api/models/%d", modelID), body, auth)
		require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", responseBody)
	}
	membership := func() int {
		var count int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`,
			profileID, modelID).Scan(&count))
		return count
	}

	patch(`{"fallbackEnabled":true}`)
	require.Equal(t, 1, membership())

	patch(`{"enabled":false}`)
	var enabled int
	require.NoError(t, db.QueryRow(`SELECT enabled FROM models WHERE id = ?`, modelID).Scan(&enabled))
	require.Zero(t, enabled)
	require.Equal(t, 1, membership(), "disabling a model must preserve its saved chain membership")

	patch(`{"enabled":true}`)
	patch(`{"fallbackEnabled":false}`)
	require.Zero(t, membership())

	resp, body = do(t, s, http.MethodPost, "/api/profiles/active", `{"profileId":null}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	patch(`{"fallbackEnabled":true}`)
	var fallbackEnabled int
	require.NoError(t, db.QueryRow(
		`SELECT enabled FROM fallback_config WHERE model_db_id = ?`, modelID).Scan(&fallbackEnabled))
	require.Equal(t, 1, fallbackEnabled, "adding a model must create a missing global-chain row")
}

// TestProfileReorderIsAtomicAndPositional: a reorder numbers positions densely
// with no duplicates and never renumbers models.id; a reorder that would collide
// rolls back whole, leaving the prior membership intact.
func TestProfileReorderIsAtomicAndPositional(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	m2 := seedModel(t, db, "p", "model-2", "Two", "Large")
	m3 := seedModel(t, db, "p", "model-3", "Three", "Large")
	p := seedProfile(t, db, "coding")

	positions := func() map[int64]int64 {
		rows, err := db.Query(`SELECT model_db_id, position FROM profile_models WHERE profile_id = ?`, p)
		require.NoError(t, err)
		defer rows.Close()
		out := map[int64]int64{}
		for rows.Next() {
			var id, pos int64
			require.NoError(t, rows.Scan(&id, &pos))
			out[id] = pos
		}
		return out
	}

	body := fmt.Sprintf(`[{"modelDbId":%d,"priority":1,"enabled":true},{"modelDbId":%d,"priority":2,"enabled":true},{"modelDbId":%d,"priority":3,"enabled":true}]`, m3, m1, m2)
	resp, rb := do(t, s, http.MethodPut, fmt.Sprintf("/api/profiles/%d/reorder", p), body, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", rb)

	got := positions()
	require.Equal(t, map[int64]int64{m3: 1, m1: 2, m2: 3}, got)
	// Positions are unique.
	seen := map[int64]bool{}
	for _, pos := range got {
		require.False(t, seen[pos], "duplicate position %d", pos)
		seen[pos] = true
	}
	// models.id is addressed by the chain tables, so it must be untouched.
	var ids []int64
	rows, err := db.Query(`SELECT id FROM models ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []int64{m1, m2, m3}, ids)

	// A duplicate model id collides on the (profile_id, model_db_id) key; the
	// whole write must roll back, leaving the prior membership exactly.
	bad := fmt.Sprintf(`[{"modelDbId":%d,"priority":1,"enabled":true},{"modelDbId":%d,"priority":2,"enabled":true}]`, m3, m3)
	resp, _ = do(t, s, http.MethodPut, fmt.Sprintf("/api/profiles/%d/reorder", p), bad, auth)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, map[int64]int64{m3: 1, m1: 2, m2: 3}, positions(),
		"a failed reorder must not partially rewrite the chain")
}

// TestActivatingAProfileDeactivatesThePrevious: there is exactly one active
// profile, and activating a second one replaces the first.
func TestActivatingAProfileDeactivatesThePrevious(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	a := seedProfile(t, db, "alpha")
	b := seedProfile(t, db, "beta")

	activeID := func() *int64 {
		_, body := do(t, s, http.MethodGet, "/api/profiles/active", "", auth)
		var data struct {
			ActiveProfileID *int64 `json:"activeProfileId"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &data))
		return data.ActiveProfileID
	}

	resp, _ := do(t, s, http.MethodPost, "/api/profiles/active", fmt.Sprintf(`{"profileId":%d}`, a), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, activeID())
	require.Equal(t, a, *activeID())

	resp, _ = do(t, s, http.MethodPost, "/api/profiles/active", fmt.Sprintf(`{"profileId":%d}`, b), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, b, *activeID())

	// The active flag column agrees: exactly one profile is active, and it is b.
	var count, activeCol int64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profiles WHERE active = 1`).Scan(&count))
	require.Equal(t, int64(1), count)
	require.NoError(t, db.QueryRow(`SELECT id FROM profiles WHERE active = 1`).Scan(&activeCol))
	require.Equal(t, b, activeCol)
}

// TestPenaltyInspectorReflectsAHitAndClearEmptiesIt: a recorded rate-limit hit
// shows up as a demotion, and the clear operation removes it.
func TestPenaltyInspectorReflectsAHitAndClearEmptiesIt(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	m1 := seedModel(t, db, "p", "model-1", "One", "Large")

	s.engine.Penalties().RecordRateLimitHit(m1)

	type inspResp struct {
		Rows []struct {
			ModelDBID *int64 `json:"modelDbId"`
			Penalty   struct {
				Hits            int     `json:"hits"`
				Value           float64 `json:"value"`
				RateLimitFactor float64 `json:"rateLimitFactor"`
			} `json:"penalty"`
		} `json:"rows"`
	}

	_, body := do(t, s, http.MethodGet, "/api/fallback/penalty-inspector", "", auth)
	var got inspResp
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Rows, 1)
	require.NotNil(t, got.Rows[0].ModelDBID)
	require.Equal(t, m1, *got.Rows[0].ModelDBID)
	require.Equal(t, 1, got.Rows[0].Penalty.Hits)
	require.Greater(t, got.Rows[0].Penalty.Value, 0.0)
	// A demotion damps the score below one but never removes the model.
	require.Less(t, got.Rows[0].Penalty.RateLimitFactor, 1.0)
	require.GreaterOrEqual(t, got.Rows[0].Penalty.RateLimitFactor, 0.4)

	resp, body := do(t, s, http.MethodDelete, "/api/fallback/penalty-inspector", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var cleared struct {
		Penalties int `json:"penalties"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &cleared))
	require.GreaterOrEqual(t, cleared.Penalties, 1)

	_, body = do(t, s, http.MethodGet, "/api/fallback/penalty-inspector", "", auth)
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Empty(t, got.Rows, "clear must empty the inspector")
}

// TestScoringViewReportsPerAxisAndUsesStableExpectedReliability: the breakdown
// carries each axis and both guardrail multipliers separately, and reliability
// is the posterior mean - deterministic across refreshes, not a Thompson draw
// that would jitter with nothing changed.
func TestScoringViewReportsPerAxisAndUsesStableExpectedReliability(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	seedFallback(t, db, m1, 1, true)

	now := time.Now().Unix()
	for range 9 {
		_, err := db.Exec(
			`INSERT INTO requests (created_at, platform, model_id, status, outcome, latency_ms) VALUES (?, 'p', 'model-1', 200, 'success', 100)`,
			now)
		require.NoError(t, err)
	}
	_, err := db.Exec(
		`INSERT INTO requests (created_at, platform, model_id, status, outcome, latency_ms) VALUES (?, 'p', 'model-1', 500, 'error', 100)`,
		now)
	require.NoError(t, err)

	type scoreRow struct {
		ModelDBID     int64   `json:"modelDbId"`
		Reliability   float64 `json:"reliability"`
		Speed         float64 `json:"speed"`
		Intelligence  float64 `json:"intelligence"`
		Headroom      float64 `json:"headroom"`
		RateLimit     float64 `json:"rateLimit"`
		Score         float64 `json:"score"`
		TotalRequests int     `json:"totalRequests"`
	}
	type routingResp struct {
		Scores []scoreRow `json:"scores"`
	}
	read := func() scoreRow {
		_, body := do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
		var data routingResp
		require.NoError(t, json.Unmarshal([]byte(body), &data))
		require.Len(t, data.Scores, 1)
		return data.Scores[0]
	}

	got := read()
	require.Equal(t, m1, got.ModelDBID)
	require.Equal(t, 10, got.TotalRequests)
	// Expected of Beta(9+1, 1+1) = 10/12; a Thompson draw would almost never
	// land on this exact value.
	require.InDelta(t, 10.0/12.0, got.Reliability, 1e-9)
	// Each axis and guardrail is a separate figure, not folded into the score.
	require.Greater(t, got.Speed, 0.0)
	require.GreaterOrEqual(t, got.Intelligence, 0.0)
	require.LessOrEqual(t, got.Intelligence, 1.0)
	require.InDelta(t, 1.0, got.Headroom, 1e-9)
	require.InDelta(t, 1.0, got.RateLimit, 1e-9)
	require.Greater(t, got.Score, 0.0)

	// Stable: a second read returns the identical reliability, because it is the
	// posterior mean rather than a sample.
	require.Equal(t, got.Reliability, read().Reliability)
}

// TestRateLimitUsageSerialisesAbsentWindowsAsNull: a window with no published
// limit is null, never {used:0,limit:0} - the client renders "-" for null and a
// number for a real limit, so a zero would misreport an unmetered axis as spent.
func TestRateLimitUsageSerialisesAbsentWindowsAsNull(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	// A model with an RPM ceiling but no TPM ceiling.
	_, err := db.Exec(
		`INSERT INTO models (platform, model_id, display_name, rpm_limit, tpm_limit, enabled) VALUES ('p', 'model-1', 'One', 60, NULL, 1)`)
	require.NoError(t, err)
	// One eligible key so the model is routable and its RPM window is reported.
	_, err = db.Exec(
		`INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at) VALUES ('p', 'k', 'x', 'y', 'z', 'healthy', 1, ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/fallback/rate-limit-usage", "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var data struct {
		Rows []struct {
			ModelDBID int64               `json:"modelDbId"`
			RPM       *rateLimitWindowOut `json:"rpm"`
			RPD       *rateLimitWindowOut `json:"rpd"`
			TPM       *rateLimitWindowOut `json:"tpm"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Len(t, data.Rows, 1)
	row := data.Rows[0]
	// RPM has a limit, so it is an object with the real ceiling.
	require.NotNil(t, row.RPM)
	require.Equal(t, int64(60), row.RPM.Limit)
	// TPM and RPD have no limit, so they are null, not a zeroed object.
	require.Nil(t, row.TPM)
	require.Nil(t, row.RPD)
	// And the wire form is literally null, not {"used":0,"limit":0}.
	require.Contains(t, body, `"tpm":null`)
	require.NotContains(t, body, `"tpm":{`)
}

// TestRelayedProviderFailureNeverEndsTheSession is the batch's load-bearing
// contract: only a genuine dashboard-session failure may carry
// TypeAuthentication. A session-gated routing route must answer a bad session
// with 401 authentication_error (so the client signs out), but must never emit
// that type for anything else.
func TestCredentialGateNeverEndsASession(t *testing.T) {
	t.Parallel()
	s, _ := routingServer(t)

	// No credential: the gate answers 401 - but with a type that cannot be
	// read as "your session ended", because there is no session.
	resp, body := do(t, s, http.MethodGet, "/api/fallback/routing", "", map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))

	// A validation failure on a routing route is a 400 with a different type, so
	// a client testing input is never signed out.
	s2, auth := routingServer(t)
	resp, body = do(t, s2, http.MethodPut, "/api/fallback/routing", `{"strategy":"nope"}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
}

// TestModelsListActiveProfileShowsNoGlobalFallbackLeak: with a profile active,
// a row's chain membership is EXACTLY its profile membership. A model enabled
// in the global fallback_config but absent from the active profile must read as
// out-of-chain, never leaking the global chain into a set.
func TestModelsListActiveProfileShowsNoGlobalFallbackLeak(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	m2 := seedModel(t, db, "p", "model-2", "Two", "Large")
	// The GLOBAL chain enables both.
	seedFallback(t, db, m1, 1, true)
	seedFallback(t, db, m2, 2, true)
	// A profile whose membership is only m1, then activate it.
	p := seedProfile(t, db, "coding")
	_, err := db.Exec(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, 1)`, p, m1)
	require.NoError(t, err)
	resp, body := do(t, s, http.MethodPost, "/api/profiles/active", fmt.Sprintf(`{"profileId":%d}`, p), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	_, body = do(t, s, http.MethodGet, "/api/models", "", auth)
	type modelRow struct {
		ID              int64 `json:"id"`
		FallbackEnabled bool  `json:"fallbackEnabled"`
	}
	var rows []modelRow
	require.NoError(t, json.Unmarshal([]byte(body), &rows))
	fb := map[int64]bool{}
	for _, r := range rows {
		fb[r.ID] = r.FallbackEnabled
	}
	require.True(t, fb[m1], "m1 is a member of the active profile, so it is in the chain")
	require.False(t, fb[m2], "m2 is only in the global fallback; it must not leak into the active profile's chain")
}

// TestTwoProfilesEditWithoutLeakage: two distinct sets can be shaped through
// /reorder, and reading each back shows exactly its own membership - neither is
// activated and neither leaks into the other.
func TestTwoProfilesEditWithoutLeakage(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	m2 := seedModel(t, db, "p", "model-2", "Two", "Large")
	a := seedProfile(t, db, "alpha")
	b := seedProfile(t, db, "beta")

	resp, rb := do(t, s, http.MethodPut, fmt.Sprintf("/api/profiles/%d/reorder", a),
		fmt.Sprintf(`[{"modelDbId":%d,"priority":1,"enabled":true}]`, m1), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", rb)
	resp, rb = do(t, s, http.MethodPut, fmt.Sprintf("/api/profiles/%d/reorder", b),
		fmt.Sprintf(`[{"modelDbId":%d,"priority":1,"enabled":true}]`, m2), auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", rb)

	members := func(id int64) []int64 {
		_, body := do(t, s, http.MethodGet, fmt.Sprintf("/api/profiles/%d/models", id), "", auth)
		var rows []struct {
			ModelDBID int64 `json:"model_db_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &rows))
		var out []int64
		for _, r := range rows {
			out = append(out, r.ModelDBID)
		}
		return out
	}
	require.Equal(t, []int64{m1}, members(a))
	require.Equal(t, []int64{m2}, members(b))

	// Neither set was activated by editing.
	_, body := do(t, s, http.MethodGet, "/api/profiles/active", "", auth)
	var active struct {
		ActiveProfileID *int64 `json:"activeProfileId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &active))
	require.Nil(t, active.ActiveProfileID, "editing a set through /reorder must never activate it")
}

// TestProfileModelsIncludeGloballyDisabledMembers: a member disabled
// catalogue-wide stays in the set's membership with its true (disabled) state,
// so a reorder built from this read never silently drops it.
func TestProfileModelsIncludeGloballyDisabledMembers(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	m1 := seedModel(t, db, "p", "model-1", "One", "Large")
	m2 := seedModel(t, db, "p", "model-2", "Two", "Large")
	p := seedProfile(t, db, "coding")
	_, err := db.Exec(`INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, 1), (?, ?, 2)`,
		p, m1, p, m2)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE models SET enabled = 0 WHERE id = ?`, m2)
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, fmt.Sprintf("/api/profiles/%d/models", p), "", auth)
	type memberRow struct {
		ModelDBID int64 `json:"model_db_id"`
		Enabled   bool  `json:"enabled"`
	}
	var rows []memberRow
	require.NoError(t, json.Unmarshal([]byte(body), &rows))
	require.Len(t, rows, 2, "a globally disabled member must still be listed, not silently dropped")
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ModelDBID] = r.Enabled
	}
	require.True(t, got[m1], "m1 is enabled catalogue-wide")
	require.False(t, got[m2], "m2 must report its true, disabled catalogue state")
}

// TestProfileRenameValidatesAndPersists: a rename is a real mutation with the
// same name rules as create, refuses another set's name, treats an unchanged
// name as a no-op success, and rejects an illegal name.
func TestProfileRenameValidatesAndPersists(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	a := seedProfile(t, db, "alpha")
	seedProfile(t, db, "beta")

	resp, body := do(t, s, http.MethodPatch, fmt.Sprintf("/api/profiles/%d", a), `{"name":"gamma"}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM profiles WHERE id = ?`, a).Scan(&name))
	require.Equal(t, "gamma", name)

	resp, _ = do(t, s, http.MethodPatch, fmt.Sprintf("/api/profiles/%d", a), `{"name":"beta"}`, auth)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.NoError(t, db.QueryRow(`SELECT name FROM profiles WHERE id = ?`, a).Scan(&name))
	require.Equal(t, "gamma", name, "a rejected rename must not change the stored name")

	resp, _ = do(t, s, http.MethodPatch, fmt.Sprintf("/api/profiles/%d", a), `{"name":"gamma"}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "renaming to the current name is a no-op success")

	resp, body = do(t, s, http.MethodPatch, fmt.Sprintf("/api/profiles/%d", a), `{"name":"has spaces"}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
}

// TestModelsListAccessClassification: access is subscription for a login-sourced
// model (even without a price), paid for a priced model, and free otherwise.
func TestModelsListAccessClassification(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()
	free := seedModel(t, db, "p", "free-1", "Free", "Large")
	paid := seedModel(t, db, "p", "paid-1", "Paid", "Large")
	sub := seedModel(t, db, "p", "sub-1", "Sub", "Large")
	_, err := db.Exec(`UPDATE models SET paid_output_per_m = 5.0 WHERE id = ?`, paid)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE models SET source = 'login' WHERE id = ?`, sub)
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/models", "", auth)
	type modelRow struct {
		ID     int64  `json:"id"`
		Access string `json:"access"`
	}
	var rows []modelRow
	require.NoError(t, json.Unmarshal([]byte(body), &rows))
	access := map[int64]string{}
	for _, r := range rows {
		access[r.ID] = r.Access
	}
	require.Equal(t, "free", access[free])
	require.Equal(t, "paid", access[paid])
	require.Equal(t, "subscription", access[sub], "a login-sourced model is a subscription even without a price")
}

// tombstoneCount reports how many tombstones guard a given catalog identity.
func tombstoneCount(t *testing.T, db *sql.DB, kind, platform, modelID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM catalog_model_tombstones WHERE kind = ? AND platform = ? AND model_id = ?`,
		kind, platform, modelID).Scan(&n))
	return n
}

// modelCount counts chat rows for a catalog identity, across every scope.
func modelCount(t *testing.T, db *sql.DB, platform, modelID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM models WHERE platform = ? AND model_id = ?`,
		platform, modelID).Scan(&n))
	return n
}

// deleteResp is the truthful shape the delete route returns.
type deleteResp struct {
	Success    bool `json:"success"`
	Tombstoned bool `json:"tombstoned"`
}

// TestModelDeleteTombstonesShippedModelAndSurvivesReseed is the durability
// contract's route end: deleting a shipped catalog model records a tombstone,
// reports tombstoned truthfully, and - because that tombstone is honored by the
// seed - the model stays gone after SeedModels runs the whole catalog again.
func TestModelDeleteTombstonesShippedModelAndSurvivesReseed(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	// Seed a catalog-owned row at a real shipped identity (source='catalog',
	// empty endpoint_scope), so the later full seed would try to re-create it.
	id := seedModel(t, db, "google", "gemini-2.5-flash", "Gemini 2.5 Flash", "Large")

	resp, body := do(t, s, http.MethodDelete, fmt.Sprintf("/api/models/%d", id), "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var out deleteResp
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.True(t, out.Success)
	require.True(t, out.Tombstoned, "deleting a shipped catalog model must report tombstoned:true")

	require.Equal(t, 1, tombstoneCount(t, db, "chat", "google", "gemini-2.5-flash"))
	require.Zero(t, modelCount(t, db, "google", "gemini-2.5-flash"))

	// The whole shipped catalog seeds, and the deleted model stays absent.
	_, err := catalog.SeedModels(context.Background(), db)
	require.NoError(t, err)
	require.Zero(t, modelCount(t, db, "google", "gemini-2.5-flash"),
		"a model deleted through the API must remain absent after SeedModels")
	var total int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM models`).Scan(&total))
	require.Positive(t, total, "the rest of the catalog still seeded")
}

// TestModelDeleteLeavesOperatorRowsUntombstoned keeps custom/user deletion
// semantics: an operator-created row (hand-added model or custom endpoint) is
// never re-seeded, so deleting it records NO tombstone and reports
// tombstoned:false.
func TestModelDeleteLeavesOperatorRowsUntombstoned(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	// A hand-added model (source='user', empty scope).
	userID := seedModel(t, db, "p", "hand-added", "Hand Added", "Large")
	_, err := db.Exec(`UPDATE models SET source = 'user' WHERE id = ?`, userID)
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodDelete, fmt.Sprintf("/api/models/%d", userID), "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	var out deleteResp
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.False(t, out.Tombstoned, "an operator-owned row must not be tombstoned")
	require.Zero(t, tombstoneCount(t, db, "chat", "p", "hand-added"))

	// A custom endpoint (non-empty endpoint_scope) reached through the generic
	// delete route is likewise never tombstoned.
	res, err := db.Exec(
		`INSERT INTO models (platform, model_id, display_name, enabled, source, endpoint_scope)
		 VALUES ('google', 'gemini-2.5-flash', 'My Relay', 1, 'user', 'relay-1')`)
	require.NoError(t, err)
	relayID, err := res.LastInsertId()
	require.NoError(t, err)

	resp, body = do(t, s, http.MethodDelete, fmt.Sprintf("/api/models/%d", relayID), "", auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.False(t, out.Tombstoned, "a custom endpoint deletion must not be tombstoned")
	require.Zero(t, tombstoneCount(t, db, "chat", "google", "gemini-2.5-flash"),
		"deleting a scoped relay must not tombstone the catalog identity")
}

// insertCustomKey adds one custom-platform credential at a given health status.
func insertCustomKey(t *testing.T, db *sql.DB, status string) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES ('custom', 'k', 'x', 'y', 'z', ?, 1, ?)`, status, time.Now().Unix())
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// seedCustomRelay adds a custom relay model bound to one endpoint scope and key.
// Two relays may share a model id: their identity is (platform, model_id,
// endpoint_scope), and each carries its own credential.
func seedCustomRelay(t *testing.T, db *sql.DB, modelID, scope string, keyID int64) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO models (platform, model_id, display_name, size_label, endpoint_scope, key_id, source, enabled)
		VALUES ('custom', ?, 'Relay', 'medium', ?, ?, 'custom', 1)`, modelID, scope, keyID)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// TestRoutingPutRejectsInvalidLaterFieldWithoutMutating proves a routing PUT is
// all-or-nothing: a valid strategy in a body whose LATER field is invalid must
// leave every setting exactly as it was, not commit the strategy before the bad
// field is reached.
func TestRoutingPutRejectsInvalidLaterFieldWithoutMutating(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)

	resp, body := do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"reliable","keySelectionStrategy":"least-remaining","cooldownCeilingMs":120000}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", body)

	// Valid strategy, invalid later field.
	resp, body = do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"smartest","keySelectionStrategy":"bogus"}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))

	var data struct {
		Strategy             string `json:"strategy"`
		KeySelectionStrategy string `json:"keySelectionStrategy"`
		CooldownCeilingMs    *int64 `json:"cooldownCeilingMs"`
	}
	_, body = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Equal(t, "reliable", data.Strategy, "a valid strategy in a rejected PUT must not persist")
	require.Equal(t, "least-remaining", data.KeySelectionStrategy, "the key strategy must be unchanged")
	require.NotNil(t, data.CooldownCeilingMs)
	require.Equal(t, int64(120000), *data.CooldownCeilingMs, "the cooldown ceiling must be unchanged")

	// An out-of-range peak hour after a valid strategy is the same guarantee.
	resp, _ = do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"balanced","peakStartHour":99}`, auth)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_, body = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	require.Equal(t, "reliable", data.Strategy, "a rejected peak-hour PUT must not change the strategy")
}

// TestTokenUsageIsolatesCustomRelaysSharingModelID proves the token-usage
// surface attributes spend by (platform, model, endpoint), so two custom relays
// that share a model id do not report each other's usage.
func TestTokenUsageIsolatesCustomRelaysSharingModelID(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	keyA := insertCustomKey(t, db, "healthy")
	keyB := insertCustomKey(t, db, "healthy")
	idA := seedCustomRelay(t, db, "shared", "http://relay-a", keyA)
	idB := seedCustomRelay(t, db, "shared", "http://relay-b", keyB)
	seedFallback(t, db, idA, 0, true)
	seedFallback(t, db, idB, 1, true)

	_, err := s.RecordRequest(context.Background(), RequestLog{
		Platform: "custom", ModelID: "shared", EndpointScope: "http://relay-a", KeyID: &keyA,
		Outcome: "success", StatusCode: 200, InputTokens: 100, OutputTokens: 50, LatencyMs: 10,
	})
	require.NoError(t, err)

	_, body := do(t, s, http.MethodGet, "/api/fallback/token-usage", "", auth)
	var out struct {
		Models []struct {
			ModelDBID int64 `json:"modelDbId"`
			Used      int64 `json:"used"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	used := map[int64]int64{}
	for _, m := range out.Models {
		used[m.ModelDBID] = m.Used
	}
	require.Equal(t, int64(150), used[idA], "relay A's spend must be attributed to relay A")
	require.Equal(t, int64(0), used[idB], "relay B must not inherit relay A's spend")
}

// TestModelsListAvailabilityFollowsCustomRelayKey proves a custom relay's
// availability tracks ITS bound key, so a relay whose own credential is dead is
// not made to look live by a sibling relay on the same platform.
func TestModelsListAvailabilityFollowsCustomRelayKey(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)
	db := s.engine.DB()

	keyA := insertCustomKey(t, db, "healthy")
	keyB := insertCustomKey(t, db, "error") // B's own credential is dead
	idA := seedCustomRelay(t, db, "shared", "http://relay-a", keyA)
	idB := seedCustomRelay(t, db, "shared", "http://relay-b", keyB)

	_, body := do(t, s, http.MethodGet, "/api/models", "", auth)
	var models []struct {
		ID        int64 `json:"id"`
		Available bool  `json:"available"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &models))
	avail := map[int64]bool{}
	for _, m := range models {
		avail[m.ID] = m.Available
	}
	require.True(t, avail[idA], "relay A with a healthy key must be available")
	require.False(t, avail[idB], "relay B must be unavailable when its own key is dead, not masked by relay A")
}

// TestPeakAdjustmentReachesDashboardWeights proves the peak-hour control reaches
// the live routing configuration and is reported truthfully: inside the window a
// non-exempt strategy's weights shift 0.6 of speed onto reliability, and the
// PeakAdjusted flag flips with it. Live routing consumes the same
// PeakAdjustedWeights through OrderChainWeighted.
func TestPeakAdjustmentReachesDashboardWeights(t *testing.T) {
	t.Parallel()
	s, auth := routingServer(t)

	// A window that contains the current UTC hour with margin, so the test is
	// insensitive to when it runs.
	h := time.Now().UTC().Hour()
	body := fmt.Sprintf(
		`{"strategy":"balanced","peakHoursAdjust":true,"peakStartHour":%d,"peakEndHour":%d,"peakTimezone":"UTC"}`,
		h, (h+2)%24)
	resp, rb := do(t, s, http.MethodPut, "/api/fallback/routing", body, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", rb)

	var data struct {
		PeakAdjusted bool        `json:"peakAdjusted"`
		Weights      *weightsOut `json:"weights"`
	}
	_, gb := do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	require.NoError(t, json.Unmarshal([]byte(gb), &data))
	require.True(t, data.PeakAdjusted, "balanced inside the peak window must report adjusted")
	require.NotNil(t, data.Weights)
	require.InDelta(t, 0.65, data.Weights.Reliability, 1e-9, "0.6 of the speed weight moves onto reliability")
	require.InDelta(t, 0.10, data.Weights.Speed, 1e-9)

	// Turning the adjustment off restores the base preset and the flag.
	resp, rb = do(t, s, http.MethodPut, "/api/fallback/routing",
		`{"strategy":"balanced","peakHoursAdjust":false}`, auth)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %q", rb)
	_, gb = do(t, s, http.MethodGet, "/api/fallback/routing", "", auth)
	require.NoError(t, json.Unmarshal([]byte(gb), &data))
	require.False(t, data.PeakAdjusted, "with the adjustment off the flag must be false")
	require.InDelta(t, 0.50, data.Weights.Reliability, 1e-9)
	require.InDelta(t, 0.25, data.Weights.Speed, 1e-9)
}
