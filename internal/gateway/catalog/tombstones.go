package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// TombstoneKind identifies which model table a tombstone guards. A catalog
// identity is (kind, platform, model_id); endpoint_scope is not part of it
// because only catalog-owned rows, which carry an empty endpoint_scope, are
// ever tombstoned.
type TombstoneKind string

const (
	// TombstoneChat guards a row in the chat `models` table.
	TombstoneChat TombstoneKind = "chat"
	// TombstoneMedia guards a row in the `media_models` table.
	TombstoneMedia TombstoneKind = "media"
)

// tombstoneSourceUser marks a deliberate operator deletion - the "keep it
// deleted across re-seeds" contract. A future 'upstream_eol' source would
// disable-and-keep instead, which is why source is stored rather than implied.
const tombstoneSourceUser = "user"

// Execer is the subset of *sql.DB / *sql.Tx the tombstone writers need, so a
// delete handler can record a tombstone inside the SAME transaction as its
// DELETE. If those two could diverge, a crash between them would leave a model
// deleted but un-tombstoned - the original resurrection bug, now intermittent.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RecordModelTombstone marks a catalog model as user-deleted so the next seed
// will not re-create it. Idempotent: a repeat delete refreshes the record.
func RecordModelTombstone(ctx context.Context, ex Execer, kind TombstoneKind, platform, modelID, reason string) error {
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO catalog_model_tombstones (kind, platform, model_id, source, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, platform, model_id) DO UPDATE SET
			source = excluded.source, reason = excluded.reason, created_at = excluded.created_at`,
		string(kind), platform, modelID, tombstoneSourceUser, reasonArg, time.Now().Unix()); err != nil {
		return fmt.Errorf("record %s tombstone %s/%s: %w", kind, platform, modelID, err)
	}
	return nil
}

// ClearModelTombstone lifts a user deletion so the model is seeded again on the
// next pass. It is the un-delete seam: a restore/re-add surface calls this,
// then re-seeds. The reference exposes the same operation (clearing a tombstone
// on re-declaration); Prowl has no dashboard restore yet, so this stays the
// ready hook rather than an endpoint nothing calls.
func ClearModelTombstone(ctx context.Context, ex Execer, kind TombstoneKind, platform, modelID string) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM catalog_model_tombstones WHERE kind = ? AND platform = ? AND model_id = ?`,
		string(kind), platform, modelID); err != nil {
		return fmt.Errorf("clear %s tombstone %s/%s: %w", kind, platform, modelID, err)
	}
	return nil
}

// tombstoneKey is the seed's lookup identity for a shipped row: the exact
// (kind, platform, model_id) tuple RecordModelTombstone stores. A seed pass
// builds this for each shipped model and skips the ones present in the loaded
// set, so an operator's delete survives the re-seed. endpoint_scope is
// intentionally absent - only catalog-owned rows, which carry an empty
// endpoint_scope, are tombstoned, so it never varies within a kind.
func tombstoneKey(kind TombstoneKind, platform, modelID string) string {
	return string(kind) + "\x00" + platform + "\x00" + modelID
}
