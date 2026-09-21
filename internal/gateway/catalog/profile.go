package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// defaultProfileName is the chain every install starts with. The reference
// creates it on first boot and marks it type=default so the dashboard renders
// it as the undeletable baseline; our profile wire shape derives that type from
// the name, so the name is load-bearing.
const defaultProfileName = "Default"

// EnsureDefaultProfile creates the baseline fallback chain and keeps it in step
// with the catalogue.
//
// Without it the dashboard's fallback-chains card has nothing to show and
// auto:default resolves to no chain at all, which reads as a missing feature
// rather than an empty one. New catalogue models are added to it on later boots
// because the profile is declared auto-include; a model the operator removed
// stays removed because that removal is recorded in profile_exclusions and this
// pass appends only enabled models that are neither members nor excluded - so a
// genuinely new model still lands while an operator deletion is never
// resurrected.
func EnsureDefaultProfile(ctx context.Context, db *sql.DB) (added int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var profileID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM profiles WHERE LOWER(name) = LOWER(?)`, defaultProfileName).Scan(&profileID)
	switch {
	case err == sql.ErrNoRows:
		// The first profile is also the active one. Two readers decide "active":
		// the profiles.active flag and the active_profile_id setting that
		// ResolveChain actually reads. Writing only the flag left them
		// disagreeing - routing fell back to the implicit full-catalogue order
		// even though the card showed Default as active. Set both here, in this
		// transaction, so every reader agrees from first boot.
		res, insErr := tx.ExecContext(ctx, `
			INSERT INTO profiles (name, active, created_at)
			VALUES (?, 1, unixepoch())`, defaultProfileName)
		if insErr != nil {
			return 0, fmt.Errorf("create default profile: %w", insErr)
		}
		profileID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES ('active_profile_id', ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			strconv.FormatInt(profileID, 10), time.Now().Unix()); err != nil {
			return 0, fmt.Errorf("set active profile: %w", err)
		}
	case err != nil:
		return 0, err
	}

	// Read the whole gap before writing: the store caps SQLite at one
	// connection, so a held cursor across an insert deadlocks.
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id FROM models m
		 WHERE m.enabled = 1 AND m.available = 1
		   AND m.id NOT IN (SELECT model_db_id FROM profile_models WHERE profile_id = ?)
		   AND m.id NOT IN (SELECT model_db_id FROM profile_exclusions WHERE profile_id = ?)
		 ORDER BY m.id ASC`, profileID, profileID)
	if err != nil {
		return 0, err
	}
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		missing = append(missing, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	if len(missing) == 0 {
		return 0, tx.Commit()
	}

	var nextPosition int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM profile_models WHERE profile_id = ?`,
		profileID).Scan(&nextPosition); err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO profile_models (profile_id, model_db_id, position) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	for _, modelID := range missing {
		nextPosition++
		if _, err := stmt.ExecContext(ctx, profileID, modelID, nextPosition); err != nil {
			return 0, fmt.Errorf("add model %d to default profile: %w", modelID, err)
		}
	}
	return len(missing), tx.Commit()
}

// EnsureRoutingChain puts every enabled catalogue model into the default
// routing chain.
//
// fallback_config IS the chain the dashboard's model table renders and the
// router walks, so an empty one means the Models page shows no models even
// with a provider key configured, and every model reports fallbackEnabled
// false. The reference ships a row per catalogue model for exactly this
// reason.
//
// A model the operator disabled in the chain keeps that state: only missing
// rows are added.
func EnsureRoutingChain(ctx context.Context, db *sql.DB) (added int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM models
		 WHERE enabled = 1 AND available = 1
		   AND id NOT IN (SELECT model_db_id FROM fallback_config)
		 ORDER BY intelligence_rank ASC, id ASC`)
	if err != nil {
		return 0, err
	}
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		missing = append(missing, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	if len(missing) == 0 {
		return 0, tx.Commit()
	}

	var position int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM fallback_config`).Scan(&position); err != nil {
		return 0, err
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO fallback_config (model_db_id, position, enabled) VALUES (?, ?, 1)`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	for _, modelID := range missing {
		position++
		if _, err := stmt.ExecContext(ctx, modelID, position); err != nil {
			return 0, fmt.Errorf("add model %d to the routing chain: %w", modelID, err)
		}
	}
	return len(missing), tx.Commit()
}
