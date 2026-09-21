package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// models_seed.json is the shipped model catalog: every chat, embedding and
// generative-media model the gateway can route to out of the box. It is
// generated from the reference install's own SQLite (not hand-transcribed) so
// the row set and every value match what the upstream product ships.
//
//go:embed models_seed.json
var modelsSeedJSON []byte

// settingCatalogSeedSHA records which shipped catalog last seeded. It is a
// fact about the binary, not a claim about the tables: the row count is the
// only source of truth for "populated vs empty", and this checksum can never
// contradict it. It lets a status surface report the catalog version and lets
// future logic tell "the shipped catalog changed since last boot" cheaply.
const settingCatalogSeedSHA = "catalog_seed_sha"

// seedChat is one shipped chat-model row. The limit and pricing columns are
// pointers so the catalog's SQL NULLs survive the round trip: a NULL rpm_limit
// means "no published per-minute cap", which the router must not read as a cap
// of zero.
type seedChat struct {
	Platform           string   `json:"platform"`
	ModelID            string   `json:"model_id"`
	DisplayName        string   `json:"display_name"`
	IntelligenceRank   int      `json:"intelligence_rank"`
	SpeedRank          int      `json:"speed_rank"`
	SizeLabel          string   `json:"size_label"`
	RPMLimit           *int64   `json:"rpm_limit"`
	RPDLimit           *int64   `json:"rpd_limit"`
	TPMLimit           *int64   `json:"tpm_limit"`
	TPDLimit           *int64   `json:"tpd_limit"`
	MonthlyTokenBudget string   `json:"monthly_token_budget"`
	ContextWindow      *int64   `json:"context_window"`
	Enabled            int      `json:"enabled"`
	SupportsVision     int      `json:"supports_vision"`
	SupportsTools      int      `json:"supports_tools"`
	SupportsReasoning  int      `json:"supports_reasoning"`
	PaidInputPerM      *float64 `json:"paid_input_per_m"`
	PaidOutputPerM     *float64 `json:"paid_output_per_m"`
}

// seedEmbedding is one shipped embedding-model row. Dimensions and the input
// ceiling are pointers for the same reason: an unknown dimension is NULL, not
// zero.
type seedEmbedding struct {
	Family         string `json:"family"`
	Platform       string `json:"platform"`
	ModelID        string `json:"model_id"`
	DisplayName    string `json:"display_name"`
	Dimensions     *int64 `json:"dimensions"`
	MaxInputTokens *int64 `json:"max_input_tokens"`
	Priority       int    `json:"priority"`
	Enabled        int    `json:"enabled"`
	QuotaLabel     string `json:"quota_label"`
}

// seedMedia is one shipped generative-media row (image, audio, transcription).
// MetaJSON carries per-adapter identity (subtitle formats, upload ceiling,
// request flavour) and is NULL when the adapter needs none.
type seedMedia struct {
	Platform    string  `json:"platform"`
	ModelID     string  `json:"model_id"`
	DisplayName string  `json:"display_name"`
	Modality    string  `json:"modality"`
	Priority    int     `json:"priority"`
	Enabled     int     `json:"enabled"`
	QuotaLabel  string  `json:"quota_label"`
	MetaJSON    *string `json:"meta_json"`
}

type seedData struct {
	Chat       []seedChat      `json:"chat"`
	Embeddings []seedEmbedding `json:"embeddings"`
	Media      []seedMedia     `json:"media"`
}

// loadSeed parses the embedded catalog once. A parse failure or an empty set
// is a corrupt or truncated asset - a build-time mistake - so it is surfaced
// as an error rather than silently seeding nothing.
var loadSeed = sync.OnceValues(func() (seedData, error) {
	var data seedData
	if err := json.Unmarshal(modelsSeedJSON, &data); err != nil {
		return seedData{}, fmt.Errorf("parse embedded model catalog: %w", err)
	}
	if len(data.Chat) == 0 || len(data.Embeddings) == 0 || len(data.Media) == 0 {
		return seedData{}, fmt.Errorf(
			"embedded model catalog is empty or truncated: chat=%d embeddings=%d media=%d",
			len(data.Chat), len(data.Embeddings), len(data.Media))
	}
	return data, nil
})

// seedSHA is the checksum of the shipped catalog asset, computed once.
var seedSHA = sync.OnceValue(func() string {
	sum := sha256.Sum256(modelsSeedJSON)
	return hex.EncodeToString(sum[:])
})

// SeedSHA returns the checksum of the shipped catalog asset.
func SeedSHA() string { return seedSHA() }

// SeedResult reports what one reconciliation pass changed, so the caller can
// log a seeded catalog distinctly from a no-op re-seed.
type SeedResult struct {
	ChatInserted      int
	ChatUpdated       int
	EmbeddingInserted int
	EmbeddingUpdated  int
	MediaInserted     int
	MediaUpdated      int
	AssetSHA          string
}

// Inserted reports the total number of catalog rows newly created this pass.
func (r SeedResult) Inserted() int {
	return r.ChatInserted + r.EmbeddingInserted + r.MediaInserted
}

// Updated reports the total number of existing catalog rows refreshed.
func (r SeedResult) Updated() int {
	return r.ChatUpdated + r.EmbeddingUpdated + r.MediaUpdated
}

// SeedModels loads the shipped catalog into the gateway database, idempotently.
//
// The reconciliation rule, which is the decision that outlives this code:
//
//   - A row the catalog owns (models.source='catalog' with empty
//     endpoint_scope; embedding_models/media_models rows with no key_id) has
//     its catalog metadata - display name, ranks, limits, context window,
//     capability flags, pricing - refreshed to the shipped values on every
//     boot, while the operator's own enabled/disabled choice is preserved. A
//     catalog that ships a model disabled (retired upstream) forces it
//     disabled regardless, matching the reference's "catalog disable wins".
//   - A row the operator created - a custom endpoint (non-empty endpoint_scope
//     or a bound key_id) or a hand-added model whose source is not 'catalog'
//   - is never read for update and never touched.
//   - A shipped model missing from the database is inserted with the catalog's
//     shipped enabled state - UNLESS the operator deleted it through the API,
//     which records a tombstone (see tombstones.go). A tombstoned shipped
//     identity is skipped on insert, so a deliberate deletion survives every
//     re-seed instead of silently resurrecting on the next boot.
//
// Tombstones vs. disable: a DISABLE keeps the row and survives as the routing
// switch; an API DELETE removes the row and records a tombstone so it stays
// gone. Only the API delete path writes tombstones - a bare row deletion
// outside that path leaves no tombstone and still reappears on re-seed, which
// is why the durable removal is the tombstone-recording delete, not a raw
// DELETE against the table.
//
// The whole pass runs in one transaction, so a failure can never leave a
// half-populated catalog - which would look like success and be worse than an
// empty one. Because the store caps SQLite at a single connection, every read
// result set is fully consumed and closed before any write is issued; holding
// a cursor open across a write on that one connection would deadlock as a hang.
func SeedModels(ctx context.Context, db *sql.DB) (SeedResult, error) {
	data, err := loadSeed()
	if err != nil {
		return SeedResult{}, err
	}
	res := SeedResult{AssetSHA: seedSHA()}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SeedResult{}, fmt.Errorf("begin catalog seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Read the whole reconciliation set up front and close each cursor before
	// writing (single-connection constraint above).
	type chatState struct {
		enabled int
		source  string
	}
	existingChat := map[string]chatState{}
	if err := scanRows(ctx, tx,
		`SELECT platform, model_id, enabled, source FROM models WHERE endpoint_scope = ''`,
		func(rows *sql.Rows) error {
			var platform, modelID, source string
			var enabled int
			if err := rows.Scan(&platform, &modelID, &enabled, &source); err != nil {
				return err
			}
			existingChat[platform+"\x00"+modelID] = chatState{enabled: enabled, source: source}
			return nil
		}); err != nil {
		return SeedResult{}, fmt.Errorf("read existing chat models: %w", err)
	}

	// For embeddings and media, a bound key_id marks an operator-created custom
	// endpoint; those are never touched.
	type ownedState struct {
		enabled int
		custom  bool
	}
	existingEmb := map[string]ownedState{}
	if err := scanRows(ctx, tx,
		`SELECT platform, model_id, enabled, key_id FROM embedding_models`,
		func(rows *sql.Rows) error {
			var platform, modelID string
			var enabled int
			var keyID sql.NullInt64
			if err := rows.Scan(&platform, &modelID, &enabled, &keyID); err != nil {
				return err
			}
			existingEmb[platform+"\x00"+modelID] = ownedState{enabled: enabled, custom: keyID.Valid}
			return nil
		}); err != nil {
		return SeedResult{}, fmt.Errorf("read existing embedding models: %w", err)
	}

	existingMedia := map[string]ownedState{}
	if err := scanRows(ctx, tx,
		`SELECT platform, model_id, enabled, key_id FROM media_models`,
		func(rows *sql.Rows) error {
			var platform, modelID string
			var enabled int
			var keyID sql.NullInt64
			if err := rows.Scan(&platform, &modelID, &enabled, &keyID); err != nil {
				return err
			}
			existingMedia[platform+"\x00"+modelID] = ownedState{enabled: enabled, custom: keyID.Valid}
			return nil
		}); err != nil {
		return SeedResult{}, fmt.Errorf("read existing media models: %w", err)
	}

	// A shipped model the operator deleted through the API carries a tombstone
	// keyed by (kind, platform, model_id). Re-inserting it below is the exact
	// resurrection bug the table exists to prevent, so load the set up front
	// (cursor closed before any write, per the single-connection rule) and skip
	// any shipped identity present in it. The set spans both catalog kinds, so
	// each insert branch checks under its own kind.
	tombstoned := map[string]bool{}
	if err := scanRows(ctx, tx,
		`SELECT kind, platform, model_id FROM catalog_model_tombstones`,
		func(rows *sql.Rows) error {
			var kind, platform, modelID string
			if err := rows.Scan(&kind, &platform, &modelID); err != nil {
				return err
			}
			tombstoned[tombstoneKey(TombstoneKind(kind), platform, modelID)] = true
			return nil
		}); err != nil {
		return SeedResult{}, fmt.Errorf("read model tombstones: %w", err)
	}

	updModel, err := tx.PrepareContext(ctx, `
		UPDATE models SET
			display_name = ?, intelligence_rank = ?, speed_rank = ?, size_label = ?,
			rpm_limit = ?, rpd_limit = ?, tpm_limit = ?, tpd_limit = ?,
			monthly_token_budget = ?, context_window = ?,
			supports_vision = ?, supports_tools = ?, supports_reasoning = ?,
			paid_input_per_m = ?, paid_output_per_m = ?, enabled = ?
		WHERE platform = ? AND model_id = ? AND endpoint_scope = ''`)
	if err != nil {
		return SeedResult{}, err
	}
	defer updModel.Close()
	insModel, err := tx.PrepareContext(ctx, `
		INSERT INTO models
			(platform, model_id, display_name, intelligence_rank, speed_rank, size_label,
			 rpm_limit, rpd_limit, tpm_limit, tpd_limit, monthly_token_budget, context_window,
			 enabled, supports_vision, supports_tools, supports_reasoning, paid_input_per_m, paid_output_per_m,
			 source, endpoint_scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'catalog', '')`)
	if err != nil {
		return SeedResult{}, err
	}
	defer insModel.Close()

	for _, m := range data.Chat {
		key := m.Platform + "\x00" + m.ModelID
		if cur, ok := existingChat[key]; ok {
			if cur.source != "catalog" {
				// Operator hand-added this identity; the catalog neither
				// clobbers nor adopts it.
				continue
			}
			if _, err := updModel.ExecContext(ctx,
				m.DisplayName, m.IntelligenceRank, m.SpeedRank, m.SizeLabel,
				m.RPMLimit, m.RPDLimit, m.TPMLimit, m.TPDLimit,
				m.MonthlyTokenBudget, m.ContextWindow,
				m.SupportsVision, m.SupportsTools, m.SupportsReasoning,
				m.PaidInputPerM, m.PaidOutputPerM, catalogEnabled(m.Enabled, cur.enabled),
				m.Platform, m.ModelID); err != nil {
				return SeedResult{}, fmt.Errorf("update model %s/%s: %w", m.Platform, m.ModelID, err)
			}
			res.ChatUpdated++
			continue
		}
		if tombstoned[tombstoneKey(TombstoneChat, m.Platform, m.ModelID)] {
			// Operator deleted this shipped model through the API; keep it gone.
			continue
		}
		if _, err := insModel.ExecContext(ctx,
			m.Platform, m.ModelID, m.DisplayName, m.IntelligenceRank, m.SpeedRank, m.SizeLabel,
			m.RPMLimit, m.RPDLimit, m.TPMLimit, m.TPDLimit, m.MonthlyTokenBudget, m.ContextWindow,
			m.Enabled, m.SupportsVision, m.SupportsTools, m.SupportsReasoning, m.PaidInputPerM, m.PaidOutputPerM); err != nil {
			return SeedResult{}, fmt.Errorf("insert model %s/%s: %w", m.Platform, m.ModelID, err)
		}
		res.ChatInserted++
	}

	updEmb, err := tx.PrepareContext(ctx, `
		UPDATE embedding_models SET
			family = ?, display_name = ?, dimensions = ?, max_input_tokens = ?,
			priority = ?, quota_label = ?, enabled = ?
		WHERE platform = ? AND model_id = ?`)
	if err != nil {
		return SeedResult{}, err
	}
	defer updEmb.Close()
	insEmb, err := tx.PrepareContext(ctx, `
		INSERT INTO embedding_models
			(family, platform, model_id, display_name, dimensions, max_input_tokens,
			 priority, enabled, quota_label)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return SeedResult{}, err
	}
	defer insEmb.Close()

	for _, m := range data.Embeddings {
		key := m.Platform + "\x00" + m.ModelID
		if cur, ok := existingEmb[key]; ok {
			if cur.custom {
				continue
			}
			if _, err := updEmb.ExecContext(ctx,
				m.Family, m.DisplayName, m.Dimensions, m.MaxInputTokens,
				m.Priority, m.QuotaLabel, catalogEnabled(m.Enabled, cur.enabled),
				m.Platform, m.ModelID); err != nil {
				return SeedResult{}, fmt.Errorf("update embedding %s/%s: %w", m.Platform, m.ModelID, err)
			}
			res.EmbeddingUpdated++
			continue
		}
		if _, err := insEmb.ExecContext(ctx,
			m.Family, m.Platform, m.ModelID, m.DisplayName, m.Dimensions, m.MaxInputTokens,
			m.Priority, m.Enabled, m.QuotaLabel); err != nil {
			return SeedResult{}, fmt.Errorf("insert embedding %s/%s: %w", m.Platform, m.ModelID, err)
		}
		res.EmbeddingInserted++
	}

	updMedia, err := tx.PrepareContext(ctx, `
		UPDATE media_models SET
			display_name = ?, modality = ?, priority = ?, quota_label = ?,
			meta_json = ?, enabled = ?
		WHERE platform = ? AND model_id = ?`)
	if err != nil {
		return SeedResult{}, err
	}
	defer updMedia.Close()
	insMedia, err := tx.PrepareContext(ctx, `
		INSERT INTO media_models
			(platform, model_id, display_name, modality, priority, enabled, quota_label, meta_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return SeedResult{}, err
	}
	defer insMedia.Close()

	for _, m := range data.Media {
		key := m.Platform + "\x00" + m.ModelID
		if cur, ok := existingMedia[key]; ok {
			if cur.custom {
				continue
			}
			if _, err := updMedia.ExecContext(ctx,
				m.DisplayName, m.Modality, m.Priority, m.QuotaLabel, m.MetaJSON,
				catalogEnabled(m.Enabled, cur.enabled),
				m.Platform, m.ModelID); err != nil {
				return SeedResult{}, fmt.Errorf("update media %s/%s: %w", m.Platform, m.ModelID, err)
			}
			res.MediaUpdated++
			continue
		}
		if tombstoned[tombstoneKey(TombstoneMedia, m.Platform, m.ModelID)] {
			// Operator deleted this shipped media model; keep it gone.
			continue
		}
		if _, err := insMedia.ExecContext(ctx,
			m.Platform, m.ModelID, m.DisplayName, m.Modality, m.Priority, m.Enabled,
			m.QuotaLabel, m.MetaJSON); err != nil {
			return SeedResult{}, fmt.Errorf("insert media %s/%s: %w", m.Platform, m.ModelID, err)
		}
		res.MediaInserted++
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingCatalogSeedSHA, res.AssetSHA, time.Now().Unix()); err != nil {
		return SeedResult{}, fmt.Errorf("record catalog seed checksum: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return SeedResult{}, fmt.Errorf("commit catalog seed: %w", err)
	}
	return res, nil
}

// catalogEnabled resolves the enabled bit for a catalog-owned row being
// refreshed: a catalog that ships the model disabled forces it disabled (dead
// upstream), otherwise the operator's own choice is preserved.
func catalogEnabled(shipped, current int) int {
	if shipped == 0 {
		return 0
	}
	return current
}

// scanRows runs a read query and hands each row to fn, guaranteeing the cursor
// is closed before it returns. Callers rely on that close: the store's single
// connection deadlocks if a write is issued while a cursor is still open.
func scanRows(ctx context.Context, tx *sql.Tx, query string, fn func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
