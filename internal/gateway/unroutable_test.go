package gateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnUnroutableListNamesTheMissingProvider is the message an operator acts
// on. "No provider keys configured yet" was returned to installs holding
// seven healthy keys, which sent people looking in the wrong place: the real
// cause is that nothing serves the platform the list points at.
func TestAnUnroutableListNamesTheMissingProvider(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	db := ledger.db

	_, err := db.Exec(`
		INSERT INTO models(platform, model_id, display_name, enabled)
		VALUES ('anthropic','claude','Claude',1), ('hyper','qwen','Qwen',1)`)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO profiles(name, active, created_at) VALUES ('subs', 0, 0)")
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO profile_models(profile_id, model_db_id, position)
		SELECT (SELECT id FROM profiles WHERE name='subs'), id, id FROM models`)
	require.NoError(t, err)

	reason := ExplainUnroutableChain(db, "auto:subs")
	require.Contains(t, reason, "anthropic")
	require.Contains(t, reason, "hyper")
	require.Contains(t, reason, "Keys page", "the reason must say where the fix lives")
	require.NotContains(t, reason, "no provider keys configured",
		"an install with models but no matching key must not be told it has no keys")
}

// TestAnEmptyListSaysSo separates the two causes: nothing in the list at all
// is a Models-page problem, not a Keys-page one.
func TestAnEmptyListSaysSo(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	_, err := ledger.db.Exec("INSERT INTO profiles(name, active, created_at) VALUES ('empty', 0, 0)")
	require.NoError(t, err)

	reason := ExplainUnroutableChain(ledger.db, "auto:empty")
	require.Contains(t, reason, "no models in it")
	require.Contains(t, reason, "Models page")
}

// TestAScopedAwayListIsDistinguished covers the third cause: keys exist for
// the platform, so the loss came from a key's model scope.
func TestAScopedAwayListIsDistinguished(t *testing.T) {
	t.Parallel()

	ledger, _ := newQuotaTestLedger(t)
	db := ledger.db

	_, err := db.Exec(`
		INSERT INTO models(platform, model_id, display_name, enabled)
		VALUES ('groq','allowed','Allowed',1)`)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO api_keys(platform, label, encrypted_key, iv, auth_tag, status,
			enabled, consecutive_failures, created_at, model_scope_json)
		VALUES ('groq','k','c','i','t','healthy',1,0,0,'["something-else"]')`)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO profiles(name, active, created_at) VALUES ('scoped', 0, 0)")
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO profile_models(profile_id, model_db_id, position)
		SELECT (SELECT id FROM profiles WHERE name='scoped'), id, 1 FROM models`)
	require.NoError(t, err)

	reason := ExplainUnroutableChain(db, "auto:scoped")
	require.Contains(t, reason, "model scope",
		"a key that exists but is scoped away is a different fix from a missing key")
}
