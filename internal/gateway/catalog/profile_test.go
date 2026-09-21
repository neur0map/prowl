package catalog

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureDefaultProfileCreatesTheBaselineChain covers the gap that made the
// dashboard's fallback-chains card disappear: no chain existed at all.
func TestEnsureDefaultProfileCreatesTheBaselineChain(t *testing.T) {
	t.Parallel()
	db := seedDB(t)
	ctx := context.Background()

	_, err := SeedModels(ctx, db)
	require.NoError(t, err)

	added, err := EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)
	require.Positive(t, added)

	var name string
	var active int
	require.NoError(t, db.QueryRow(
		`SELECT name, active FROM profiles`).Scan(&name, &active))
	require.Equal(t, "Default", name)
	require.Equal(t, 1, active, "a chain nothing selects would make the card misleading")

	var inChain, enabled int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profile_models`).Scan(&inChain))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM models WHERE enabled = 1`).Scan(&enabled))
	require.Equal(t, enabled, inChain, "every enabled catalog model belongs to the baseline chain")
}

// TestEnsureDefaultProfileIsIdempotent keeps a second boot from duplicating
// rows or resetting the operator's chain.
func TestEnsureDefaultProfileIsIdempotent(t *testing.T) {
	t.Parallel()
	db := seedDB(t)
	ctx := context.Background()

	_, err := SeedModels(ctx, db)
	require.NoError(t, err)
	_, err = EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)

	var before int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profile_models`).Scan(&before))

	added, err := EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)
	require.Zero(t, added, "a second boot adds nothing")

	var after, profiles int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profile_models`).Scan(&after))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&profiles))
	require.Equal(t, before, after)
	require.Equal(t, 1, profiles, "the baseline chain is created once, not per boot")
}

// TestEnsureDefaultProfileKeepsOperatorRemovals: a model the operator removed -
// recorded as a durable exclusion, exactly as every membership-mutation path now
// does - stays out across reboots, while a genuinely new catalogue model is
// still appended.
func TestEnsureDefaultProfileKeepsOperatorRemovals(t *testing.T) {
	t.Parallel()
	db := seedDB(t)
	ctx := context.Background()

	_, err := SeedModels(ctx, db)
	require.NoError(t, err)
	_, err = EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)

	var profileID, victim int64
	require.NoError(t, db.QueryRow(`SELECT id FROM profiles WHERE LOWER(name) = 'default'`).Scan(&profileID))
	require.NoError(t, db.QueryRow(
		`SELECT model_db_id FROM profile_models WHERE profile_id = ? LIMIT 1`, profileID).Scan(&victim))

	// The operator removes it: the membership row goes AND the removal is
	// recorded, which is what the handlers now do on every removal.
	_, err = db.Exec(`DELETE FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, victim)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO profile_exclusions (profile_id, model_db_id) VALUES (?, ?)`, profileID, victim)
	require.NoError(t, err)

	// A genuinely new catalogue model appears before the next boot.
	fresh, err := db.Exec(`INSERT INTO models(platform, model_id, display_name, enabled, source, endpoint_scope)
		VALUES('acme','brand-new','Brand New',1,'catalog','')`)
	require.NoError(t, err)
	freshID, _ := fresh.LastInsertId()

	added, err := EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, added, "only the genuinely new model is appended, never the excluded one")

	var victimBack, freshIn int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, victim).Scan(&victimBack))
	require.Zero(t, victimBack, "an operator removal recorded as an exclusion stays removed across boots")
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM profile_models WHERE profile_id = ? AND model_db_id = ?`, profileID, freshID).Scan(&freshIn))
	require.Equal(t, 1, freshIn, "a genuinely new model still appends")
}

// TestProfileNameCaseInsensitiveUniqueness proves two profiles whose names
// differ only in case cannot coexist at the SQLite constraint level, so a
// concurrent create that races past the application check still cannot create a
// case-variant duplicate.
func TestProfileNameCaseInsensitiveUniqueness(t *testing.T) {
	t.Parallel()
	db := seedDB(t)

	_, err := db.Exec(`INSERT INTO profiles(name, active, created_at) VALUES('Coding', 0, 0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO profiles(name, active, created_at) VALUES('coding', 0, 0)`)
	require.Error(t, err, "a case variant of an existing profile name must be rejected by the constraint")

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM profiles WHERE LOWER(name) = 'coding'`).Scan(&n))
	require.Equal(t, 1, n, "only one profile of a given case-insensitive name can exist")
}

// TestEnsureDefaultProfileSetsActiveProfileID pins the fix for the two readers
// of "which chain is active" disagreeing on a fresh install: the profiles.active
// flag and the active_profile_id setting that the engine's ResolveChain reads.
// Writing only the flag left auto: routing on the implicit full-catalogue order
// while the dashboard card claimed Default was active.
func TestEnsureDefaultProfileSetsActiveProfileID(t *testing.T) {
	t.Parallel()
	db := seedDB(t)
	ctx := context.Background()

	_, err := SeedModels(ctx, db)
	require.NoError(t, err)
	_, err = EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)

	var profileID int64
	require.NoError(t, db.QueryRow(`SELECT id FROM profiles WHERE name = 'Default'`).Scan(&profileID))

	// The setting the router resolves auto: through must name the seeded chain,
	// not be absent (which resolves to no active profile and legacy order).
	var setting string
	require.NoError(t, db.QueryRow(
		`SELECT value FROM settings WHERE key = 'active_profile_id'`).Scan(&setting))
	resolved, err := strconv.ParseInt(setting, 10, 64)
	require.NoError(t, err)
	require.Equal(t, profileID, resolved,
		"active_profile_id must name the seeded Default profile so every reader agrees")

	// The flag reader agrees with the setting reader.
	var active int
	require.NoError(t, db.QueryRow(`SELECT active FROM profiles WHERE id = ?`, profileID).Scan(&active))
	require.Equal(t, 1, active)
}

// TestEnsureDefaultProfileLeavesOperatorActivationAlone: the active_profile_id
// setting is written once, when the Default profile is created. A later boot's
// ensure pass must not drag the active chain back to Default after the operator
// activated another one.
func TestEnsureDefaultProfileLeavesOperatorActivationAlone(t *testing.T) {
	t.Parallel()
	db := seedDB(t)
	ctx := context.Background()

	_, err := SeedModels(ctx, db)
	require.NoError(t, err)
	_, err = EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)

	// Operator creates and activates their own chain.
	res, err := db.Exec(`INSERT INTO profiles (name, active, created_at) VALUES ('Mine', 1, unixepoch())`)
	require.NoError(t, err)
	mineID, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES ('active_profile_id', ?, unixepoch())
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		strconv.FormatInt(mineID, 10))
	require.NoError(t, err)

	// A later boot re-runs the ensure pass.
	_, err = EnsureDefaultProfile(ctx, db)
	require.NoError(t, err)

	var setting string
	require.NoError(t, db.QueryRow(
		`SELECT value FROM settings WHERE key = 'active_profile_id'`).Scan(&setting))
	require.Equal(t, strconv.FormatInt(mineID, 10), setting,
		"a re-boot must not steal the operator's active chain")
}
