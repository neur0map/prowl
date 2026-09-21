-- +goose Up
-- +goose StatementBegin

-- When the operator deletes a catalog model from the dashboard, removing the
-- row alone is not enough: the shipped catalog is re-seeded on every boot and
-- would silently re-create it. A tombstone records the deletion by catalog
-- identity so the seed keeps it gone. `source` distinguishes a deliberate
-- 'user' deletion (stays deleted) from a future 'upstream_eol' retirement
-- (disable-and-keep); `kind` names the table the identity belongs to
-- ('chat' -> models, 'media' -> media_models). endpoint_scope is not part of
-- the identity because only catalog-owned rows (scope='') are ever tombstoned.
CREATE TABLE IF NOT EXISTS catalog_model_tombstones (
    kind       TEXT    NOT NULL DEFAULT 'chat',
    platform   TEXT    NOT NULL,
    model_id   TEXT    NOT NULL,
    source     TEXT    NOT NULL DEFAULT 'user',
    reason     TEXT,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, platform, model_id)
);
CREATE INDEX IF NOT EXISTS idx_catalog_model_tombstones_platform_model
    ON catalog_model_tombstones(platform, model_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS catalog_model_tombstones;
-- +goose StatementEnd
