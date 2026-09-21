-- +goose Up
-- +goose StatementBegin

-- server_logs.id was application-assigned: a process-local counter seeded from
-- MAX(id) at boot and minted under a mutex. That had two durability faults the
-- poller's sinceId cursor could not survive.
--
--   * Concurrent records minted an id under the lock but committed the row
--     outside it, so a higher id could reach disk before a lower one. A poll
--     that observed the higher id first advanced its cursor past the lower row,
--     which then never satisfied `id > sinceId` again -- a permanent skip.
--   * A DELETE (the Clear button) followed by a restart re-seeded the counter
--     from an empty MAX(id) = 0, so ids restarted at 1. Every client still
--     holding a larger cursor silently skipped every row minted after the wrap.
--
-- Handing allocation to SQLite via AUTOINCREMENT fixes both: ids are assigned
-- in commit order (a row with id N is committed before id N+1 exists), and the
-- high-water mark lives in sqlite_sequence, which a DELETE never lowers and a
-- restart never forgets. SQLite cannot ALTER a column into AUTOINCREMENT, so
-- the table is rebuilt; copying the rows with their existing ids carries
-- sqlite_sequence forward to the current maximum so new ids continue past it.
ALTER TABLE server_logs RENAME TO server_logs_legacy;

CREATE TABLE server_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    level         TEXT    CHECK (level IN ('warn', 'error')),
    source        TEXT,
    provider      TEXT,
    model         TEXT,
    event         TEXT,
    request_id    TEXT,
    message       TEXT    NOT NULL,
    created_at_ms INTEGER NOT NULL
);

INSERT INTO server_logs
    (id, level, source, provider, model, event, request_id, message, created_at_ms)
SELECT id, level, source, provider, model, event, request_id, message, created_at_ms
  FROM server_logs_legacy;

DROP TABLE server_logs_legacy;

CREATE INDEX IF NOT EXISTS idx_server_logs_created ON server_logs(created_at_ms, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Rebuild without AUTOINCREMENT, preserving the rows and their ids. Dropping
-- the AUTOINCREMENT table also drops its sqlite_sequence entry.
ALTER TABLE server_logs RENAME TO server_logs_autoinc;

CREATE TABLE server_logs (
    id            INTEGER PRIMARY KEY,
    level         TEXT    CHECK (level IN ('warn', 'error')),
    source        TEXT,
    provider      TEXT,
    model         TEXT,
    event         TEXT,
    request_id    TEXT,
    message       TEXT    NOT NULL,
    created_at_ms INTEGER NOT NULL
);

INSERT INTO server_logs
    (id, level, source, provider, model, event, request_id, message, created_at_ms)
SELECT id, level, source, provider, model, event, request_id, message, created_at_ms
  FROM server_logs_autoinc;

DROP TABLE server_logs_autoinc;

CREATE INDEX IF NOT EXISTS idx_server_logs_created ON server_logs(created_at_ms, id);

-- +goose StatementEnd
