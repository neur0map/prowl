package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
)

// Enrolling a login only added a credential, leaving the router a key with
// nothing to route to it. Seeded rows are marked `source = 'login'` and carry
// the key id, so withdrawing the login removes them through the existing
// cascade.
//
// A login's models are stored under their own endpoint scope, distinct from
// the shipped catalogue's empty scope, so a login that shares a
// (platform, model_id) with a catalogue row is a SEPARATE routing candidate
// rather than an overwrite. Without this, enrolling rebound the catalogue row
// to the login key and flipped its source to 'login', so withdrawing the login
// cascade-deleted the shipped model - and its fallback entry - with it. The
// scope is not a base URL: the linked credential still routes through its
// platform adapter (chain routing resolves the endpoint from the key, not this
// scope), so a login never impersonates a custom endpoint.

const loginModelSource = "login"

// loginEndpointScope is the stable, non-URL identity every login-backed model
// row carries. Prefixing the platform keeps it distinct from the catalogue's
// empty scope and from any custom-endpoint base URL, and one login per
// platform makes it stable across re-enrolment.
func loginEndpointScope(platform string) string {
	return linkedPrefix + platform
}

// SeedLoginModels writes an enrolled login's models into the catalogue and
// appends them to the active routing chain. Returns how many rows landed.
func (e *Engine) SeedLoginModels(ctx context.Context, keyID int64, platform string, models []LinkedModel) (int, error) {
	if len(models) == 0 {
		return 0, nil
	}

	ranked := rankLinkedModels(models)
	scope := loginEndpointScope(platform)

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Appended, not prepended: a new enrolment must not silently outrank
	// models the operator already ordered.
	var nextPosition int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(position), 0) + 1 FROM fallback_config").Scan(&nextPosition); err != nil {
		return 0, err
	}

	// A newly enrolled login must route without waiting for the next boot's
	// EnsureDefaultProfile pass, so its models also join the DEFAULT
	// (auto-include) profile immediately - the chain that is meant to accumulate
	// every model. They are NOT pushed into a curated named set, even when one is
	// active: a set the operator hand-picked (say "Quick chores") must not be
	// flooded with an account's whole catalogue on every boot. New login models
	// still land in the global fallback chain above, and the operator adds them
	// to a curated set deliberately. Exclusions are still honoured, and a login
	// never re-adds a model the operator removed from Default.
	defaultProfileID, hasDefaultProfile := defaultProfileForSeed(ctx, tx)
	var nextProfilePos int64
	if hasDefaultProfile {
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(position), 0) FROM profile_models WHERE profile_id = ?",
			defaultProfileID).Scan(&nextProfilePos); err != nil {
			return 0, err
		}
	}

	seeded := 0
	for _, m := range ranked {
		// A row a prior retirement marked unavailable is restored here (the ON
		// CONFLICT clause sets available = 1); the operator's own enabled flag
		// and chain/profile preference are never touched, so a model the operator
		// disabled stays disabled across a provider drop-and-relist.
		var modelDBID int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO models(platform, model_id, display_name, intelligence_rank,
				speed_rank, size_label, context_window, enabled, supports_vision,
				supports_tools, supports_reasoning, key_id, paid_input_per_m,
				paid_output_per_m, source, endpoint_scope, available)
			VALUES(?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(platform, model_id, endpoint_scope) DO UPDATE SET
				available = 1,
				display_name = excluded.display_name,
				intelligence_rank = excluded.intelligence_rank,
				speed_rank = excluded.speed_rank,
				context_window = excluded.context_window,
				supports_vision = excluded.supports_vision,
				supports_tools = excluded.supports_tools,
				supports_reasoning = excluded.supports_reasoning,
				key_id = excluded.key_id,
				paid_input_per_m = excluded.paid_input_per_m,
				paid_output_per_m = excluded.paid_output_per_m,
				source = excluded.source
			RETURNING id`,
			platform, m.model.ID, displayName(m.model), m.intelligence, m.speed,
			linkedModelSize(m.model), nullableInt(m.model.ContextWindow), boolInt(m.model.Attachments),
			boolInt(m.model.Tools), boolInt(m.model.CanReason), keyID,
			nullablePrice(m.model.InputPerM), nullablePrice(m.model.OutputPerM),
			loginModelSource, scope,
		).Scan(&modelDBID)
		if err != nil {
			return seeded, fmt.Errorf("seed %s/%s: %w", platform, m.model.ID, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fallback_config(model_db_id, position, enabled)
			VALUES(?, ?, 1)
			ON CONFLICT(model_db_id) DO NOTHING`, modelDBID, nextPosition); err != nil {
			return seeded, fmt.Errorf("chain %s/%s: %w", platform, m.model.ID, err)
		}
		if hasDefaultProfile {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO profile_models (profile_id, model_db_id, position)
				SELECT ?, ?, ?
				 WHERE NOT EXISTS (SELECT 1 FROM profile_models WHERE profile_id = ? AND model_db_id = ?)
				   AND NOT EXISTS (SELECT 1 FROM profile_exclusions WHERE profile_id = ? AND model_db_id = ?)`,
				defaultProfileID, modelDBID, nextProfilePos+1,
				defaultProfileID, modelDBID, defaultProfileID, modelDBID)
			if err != nil {
				return seeded, fmt.Errorf("profile %s/%s: %w", platform, m.model.ID, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				nextProfilePos++
			}
		}
		nextPosition++
		seeded++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	slog.Info("Seeded models for an enrolled login",
		"platform", platform, "key_id", keyID, "models", seeded)
	return seeded, nil
}

// defaultProfileForSeed returns the id of the Default (auto-include) profile
// within a seeding tx. Login models auto-join only this profile - the chain
// meant to accumulate every model - never a curated named set, so activating a
// hand-picked set does not turn each boot's seed pass into a flood of an
// account's whole catalogue. When no Default profile exists yet (a fresh DB
// before EnsureDefaultProfile has run) the login falls to the global fallback
// chain until the profile is created. Matched case-insensitively on name to
// mirror EnsureDefaultProfile, and kept SQL-only so it shares the seeding
// transaction's view.
func defaultProfileForSeed(ctx context.Context, tx *sql.Tx) (int64, bool) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM profiles WHERE LOWER(name) = 'default' ORDER BY id LIMIT 1`).Scan(&id)
	if err != nil {
		return 0, false
	}
	return id, true
}

// retireAbsentLoginModels marks unavailable a login's catalogue rows that an
// AUTHORITATIVE discovery no longer lists, so a model the provider retired stops
// being a routing candidate. keep is the model-id set the latest authoritative
// discovery returned; the caller must never pass a fallback or failed discovery,
// or a transient catalogue outage would retire models the login still serves.
//
// The row is marked NOT available, never deleted and never disabled. Deletion
// would cascade away the operator's chain disable (fallback_config.enabled) and
// default-profile exclusion (profile_exclusions), both keyed by the model's db
// id, and a later relist under a fresh id would resurrect a model the operator
// had turned off. Clearing models.enabled would conflate provider retirement
// with the operator's own disable, and SeedLoginModels runs on every reconcile,
// so it would keep re-enabling a model the operator disabled. Availability is
// therefore its own flag: retirement clears it, SeedLoginModels restores it, and
// the operator's enabled flag is never touched. Every candidate query requires
// both flags. Returns how many rows were retired.
func (e *Engine) retireAbsentLoginModels(ctx context.Context, keyID int64, keep []LinkedModel) (int, error) {
	present := make(map[string]struct{}, len(keep))
	for _, m := range keep {
		present[m.ID] = struct{}{}
	}

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Read the whole set before writing: the store caps SQLite at one
	// connection, so a held cursor across a write deadlocks. Only currently
	// available rows are considered, so a repeated authoritative discovery of the
	// same absent model does not re-retire it.
	//
	// A row under any scope other than the login's own is a legacy duplicate:
	// login models were once stored under the catalogue's empty scope, and an
	// install that enrolled before the scoped identity arrived carries both.
	// Those rows share every model id with the scoped set, so they are retired
	// by scope rather than by id - otherwise every login model listed twice.
	rows, err := tx.QueryContext(ctx,
		"SELECT id, model_id, platform, endpoint_scope FROM models WHERE key_id = ? AND source = ? AND available = 1",
		keyID, loginModelSource)
	if err != nil {
		return 0, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var modelID, platform, scope string
		if err := rows.Scan(&id, &modelID, &platform, &scope); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if _, ok := present[modelID]; !ok || scope != loginEndpointScope(platform) {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	if len(stale) == 0 {
		return 0, tx.Commit()
	}
	upd, err := tx.PrepareContext(ctx, "UPDATE models SET available = 0 WHERE id = ?")
	if err != nil {
		return 0, err
	}
	defer func() { _ = upd.Close() }()
	for _, id := range stale {
		if _, err := upd.ExecContext(ctx, id); err != nil {
			return 0, fmt.Errorf("retire login model %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	slog.Info("Retired login models no longer served", "key_id", keyID, "models", len(stale))
	return len(stale), nil
}

// rankedModel pairs a model with the ranks derived for it.
type rankedModel struct {
	model        LinkedModel
	intelligence int
	speed        int
}

// rankLinkedModels orders a provider's models using the vendor's own signals,
// in the order those signals are trustworthy.
//
// Catalogue POSITION comes first. The provider publishes its models in a
// curated order - newest and most capable first - and that is the only signal
// that tracks a vendor's current range. Price looked like the obvious proxy
// and is not: Anthropic's newer flagships are cheaper than its older ones, so
// a price ordering put claude-opus-4-1 above claude-opus-5 and would have
// sent the hardest work to the previous generation.
//
// The vendor's declared large/small defaults override position, since they are
// an explicit statement rather than an inference, and price is kept only as a
// tiebreak inside one position group.
//
// All of it is a starting order. The Models page ranks by live reliability and
// speed once traffic exists, and the operator can reorder or disable any row.
func rankLinkedModels(models []LinkedModel) []rankedModel {
	type entry struct {
		model    LinkedModel
		position int
	}
	entries := make([]entry, 0, len(models))
	for i, m := range models {
		entries = append(entries, entry{model: m, position: i})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.model.Flagship != b.model.Flagship {
			return a.model.Flagship
		}
		if a.model.Small != b.model.Small {
			return b.model.Small
		}
		if a.position != b.position {
			return a.position < b.position
		}
		return a.model.OutputPerM > b.model.OutputPerM
	})

	// Ranks use the catalogue's ordinal contract: lower is better. The small end
	// of a vendor's range gets the best speed rank; declared flagships get the
	// capability tier the scorer expects instead of falling into "unknown".
	out := make([]rankedModel, 0, len(entries))
	for i, e := range entries {
		intelligence := i + 1
		speed := len(entries) - i
		out = append(out, rankedModel{model: e.model, intelligence: intelligence, speed: speed})
	}
	return out
}

func linkedModelSize(m LinkedModel) string {
	switch {
	case m.Flagship:
		return "Frontier"
	case m.Small:
		return "Small"
	default:
		return "Large"
	}
}

func displayName(m LinkedModel) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// nullableInt keeps an unknown context window NULL rather than storing zero,
// which the request gate would read as "no room for anything".
func nullableInt(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// nullablePrice distinguishes "free or included in a subscription" from
// "priced at zero", which would make a paid model look free.
func nullablePrice(v float64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// LoginModelCount reports how many provider-offered catalogue rows an enrolled
// login owns, for the dashboard to show what enrolling actually did. A model the
// provider retired is marked unavailable, not deleted (see
// retireAbsentLoginModels), so it is excluded here rather than lingering in the
// count; an operator-disabled model the provider still offers is retained.
func (e *Engine) LoginModelCount(ctx context.Context, keyID int64) int {
	var n int
	err := e.store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM models WHERE key_id = ? AND source = ? AND available = 1",
		keyID, loginModelSource).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0
	}
	return n
}
