-- +goose Up
-- +goose StatementBegin

-- Metadata index plus payload for the SQL-dump backup feature. The reference
-- keeps the dump on disk and stores only a filepath; this port keeps the
-- structured dump in the row itself, so there is no on-disk path to confine or
-- traverse and the payload survives a restore because this table is internal
-- (never dumped, never restored).
CREATE TABLE IF NOT EXISTS backups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    filename    TEXT    NOT NULL,
    filesize    INTEGER NOT NULL,
    is_full     INTEGER NOT NULL DEFAULT 1,
    source      TEXT    NOT NULL DEFAULT 'manual',
    created_at  INTEGER NOT NULL,
    tables_json TEXT    NOT NULL DEFAULT '[]',
    payload     TEXT    NOT NULL
);

-- Bearer tokens embedded in shareable dashboard URLs. Only the SHA-256 of the
-- raw token is stored, so a copy of this database cannot reconstruct a working
-- URL; the raw token is shown once at mint. This table is on the dump exclusion
-- list alongside users and sessions: it grants inference access, and a restore
-- must not resurrect revoked tokens or carry live ones into a backup file.
CREATE TABLE IF NOT EXISTS url_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash   TEXT    NOT NULL UNIQUE,
    label        TEXT    NOT NULL DEFAULT '',
    token_prefix TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_url_tokens_active ON url_tokens(token_hash, revoked_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS url_tokens;
DROP TABLE IF EXISTS backups;
-- +goose StatementEnd
