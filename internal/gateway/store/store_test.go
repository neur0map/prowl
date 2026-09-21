package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// TestSchemaApplies is the floor: the gateway cannot start without it.
func TestSchemaApplies(t *testing.T) {
	t.Parallel()

	s, err := Open(context.Background(), t.TempDir())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	for _, table := range []string{
		"api_keys", "models", "requests", "request_attempts", "request_hourly",
		"rate_limit_usage", "rate_limit_cooldowns", "provider_quota_state",
		"provider_quota_observations", "profiles", "profile_models",
		"fallback_config", "settings", "model_overrides",
	} {
		var name string
		err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table,
		).Scan(&name)
		require.NoError(t, err, "table %s must exist", table)
	}
}

// TestForeignKeysAreEnforced is why the pragma is in the DSN rather than
// assumed. Pruning the request trail relies on attempts cascading with their
// request; with foreign keys off, SQLite accepts the schema, cascades
// nothing, and the table fills with orphans that no query ever reaches.
func TestForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()

	s, err := Open(context.Background(), t.TempDir())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	db := s.DB()

	var on int
	require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&on))
	require.Equal(t, 1, on, "foreign_keys must be ON, or cascade pruning silently does nothing")

	now := time.Now().Unix()
	res, err := db.Exec(`INSERT INTO requests
		(created_at, platform, model_id, outcome) VALUES (?, 'groq', 'llama', 'success')`, now)
	require.NoError(t, err)
	requestID, err := res.LastInsertId()
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO request_attempts
		(request_id, attempt, platform, model_id, created_at) VALUES (?, 1, 'groq', 'llama', ?)`,
		requestID, now)
	require.NoError(t, err)

	// An attempt pointing at no request must be refused outright.
	_, err = db.Exec(`INSERT INTO request_attempts
		(request_id, attempt, platform, model_id, created_at) VALUES (424242, 1, 'x', 'y', ?)`, now)
	require.Error(t, err, "an orphan attempt must be rejected")

	// Deleting the request must take its attempts with it.
	_, err = db.Exec(`DELETE FROM requests WHERE id = ?`, requestID)
	require.NoError(t, err)

	var orphans int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM request_attempts WHERE request_id = ?`, requestID).Scan(&orphans))
	require.Zero(t, orphans, "attempts must cascade with their request")
}

// TestModelIdentityIncludesEndpointScope keeps two relays that serve the same
// model id from collapsing into one row. They have separate quota and
// separate reliability, so scoring them as one endpoint would attribute one
// relay's failures to the other.
func TestModelIdentityIncludesEndpointScope(t *testing.T) {
	t.Parallel()

	s, err := Open(context.Background(), t.TempDir())
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	insert := func(scope string) error {
		_, err := s.DB().Exec(`INSERT INTO models
			(platform, model_id, display_name, endpoint_scope) VALUES ('custom', 'llama-3', 'Llama 3', ?)`, scope)
		return err
	}

	require.NoError(t, insert("http://a.local/v1"))
	require.NoError(t, insert("http://b.local/v1"), "a second relay must be its own row")
	require.Error(t, insert("http://a.local/v1"), "the same model on the same endpoint must be unique")
}

// TestStateSurvivesReopen is the reason this is a database and not a JSON
// file: a restart must not hand back quota a provider has already counted, or
// forget that an endpoint is benched.
func TestStateSurvivesReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	s, err := Open(ctx, dir)
	require.NoError(t, err)
	_, err = s.DB().Exec(`INSERT INTO rate_limit_usage
		(quota_key, window_kind, window_start, requests, tokens) VALUES ('groq/llama/1', 'rpd', 100, 7, 9000)`)
	require.NoError(t, err)
	_, err = s.DB().Exec(`INSERT INTO rate_limit_cooldowns
		(quota_key, until, source, step, last_hit) VALUES ('groq/llama/1', 999999, 'authoritative', 2, 100)`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	again, err := Open(ctx, dir)
	require.NoError(t, err)
	defer func() { _ = again.Close() }()

	var requests, tokens int
	require.NoError(t, again.DB().QueryRow(
		`SELECT requests, tokens FROM rate_limit_usage WHERE quota_key='groq/llama/1'`).Scan(&requests, &tokens))
	require.Equal(t, 7, requests)
	require.Equal(t, 9000, tokens)

	var source string
	require.NoError(t, again.DB().QueryRow(
		`SELECT source FROM rate_limit_cooldowns WHERE quota_key='groq/llama/1'`).Scan(&source))
	require.Equal(t, "authoritative", source,
		"a provider-stated bench must survive a restart with its provenance")
}

// TestMigrationsAreIdempotent keeps a second launch from failing on an
// already-applied schema.
func TestMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()
	for range 3 {
		s, err := Open(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}
}

// migrateTo brings a fresh gateway database up to (and including) version, using
// its own goose provider over the embedded migrations, so a test can reproduce
// the state of an install created under an older schema.
func migrateTo(t *testing.T, version int64) (*sql.DB, *goose.Provider) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "gateway.db")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.PingContext(ctx))
	configurePool(db)
	sub, err := fs.Sub(migrationsFS, "migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sub, goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, version)
	require.NoError(t, err)
	return db, provider
}

// priorToExclusions is the last migration version before the profile-exclusions
// migration, and exclusionsVersion is that migration
// (20260915100000_profile_exclusions_and_ci_names.sql).
const (
	priorToExclusions int64 = 20260915090000
	exclusionsVersion int64 = 20260915100000
)

// TestProfileExclusionMigrationBackfillsExistingRemovals proves the migration is
// additive for already-created state: an install whose Default profile already
// had a model removed - with no exclusion recorded, because the table did not
// exist yet - has that removal captured as a durable exclusion on upgrade, so
// the next boot's auto-include does not resurrect it. A current member is never
// excluded.
func TestProfileExclusionMigrationBackfillsExistingRemovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, provider := migrateTo(t, priorToExclusions)

	_, err := db.ExecContext(ctx, `INSERT INTO profiles(name, active, created_at) VALUES('Default', 1, 0)`)
	require.NoError(t, err)
	var pid int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM profiles WHERE name='Default'`).Scan(&pid))
	kept, err := db.ExecContext(ctx, `INSERT INTO models(platform, model_id, display_name, enabled, source, endpoint_scope) VALUES('p','kept','Kept',1,'catalog','')`)
	require.NoError(t, err)
	keptID, _ := kept.LastInsertId()
	removed, err := db.ExecContext(ctx, `INSERT INTO models(platform, model_id, display_name, enabled, source, endpoint_scope) VALUES('p','removed','Removed',1,'catalog','')`)
	require.NoError(t, err)
	removedID, _ := removed.LastInsertId()
	// Only the kept model is a member; the removed one was taken out pre-upgrade.
	_, err = db.ExecContext(ctx, `INSERT INTO profile_models(profile_id, model_db_id, position) VALUES(?,?,0)`, pid, keptID)
	require.NoError(t, err)

	_, err = provider.UpTo(ctx, exclusionsVersion) // apply the exclusions migration
	require.NoError(t, err)

	var removedExcluded, keptExcluded int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_exclusions WHERE profile_id=? AND model_db_id=?`, pid, removedID).Scan(&removedExcluded))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_exclusions WHERE profile_id=? AND model_db_id=?`, pid, keptID).Scan(&keptExcluded))
	require.Equal(t, 1, removedExcluded, "an already-removed Default member is captured as a durable exclusion on upgrade")
	require.Zero(t, keptExcluded, "a current member is never excluded")
}

// TestProfileNameCaseIndexDisambiguatesExistingCollisions proves the migration
// is safe for existing state that the old case-SENSITIVE constraint allowed:
// two profiles named "Coding" and "coding" are disambiguated (the earliest kept,
// the later suffixed) so the new case-insensitive unique index builds without
// failing, and both profiles survive.
func TestProfileNameCaseIndexDisambiguatesExistingCollisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, provider := migrateTo(t, priorToExclusions)

	_, err := db.ExecContext(ctx, `INSERT INTO profiles(name, active, created_at) VALUES('Coding', 0, 0)`)
	require.NoError(t, err)
	res, err := db.ExecContext(ctx, `INSERT INTO profiles(name, active, created_at) VALUES('coding', 0, 0)`)
	require.NoError(t, err, "the old schema allowed a case variant")
	laterID, _ := res.LastInsertId()

	_, err = provider.UpTo(ctx, exclusionsVersion) // apply the exclusions + CI-name migration
	require.NoError(t, err, "the migration must not fail on a pre-existing case collision")

	var profiles int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profiles`).Scan(&profiles))
	require.Equal(t, 2, profiles, "both profiles are preserved, only the later one is renamed")
	var laterName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT name FROM profiles WHERE id=?`, laterID).Scan(&laterName))
	require.NotEqual(t, "coding", laterName, "the later duplicate is renamed so the index can build")

	// The index is now enforcing case-insensitive uniqueness.
	_, err = db.ExecContext(ctx, `INSERT INTO profiles(name, active, created_at) VALUES('CODING', 0, 0)`)
	require.Error(t, err, "a further case variant is rejected by the new constraint")
}
