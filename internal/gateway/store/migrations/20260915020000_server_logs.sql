-- +goose Up
-- +goose StatementBegin

-- The dashboard's server-log viewer store: durable warn/error lines behind the
-- session gate, never the inference key -- these lines name providers, models,
-- key ids and failure reasons.
--
-- id is assigned by the application, NOT AUTOINCREMENT: the log store keeps its
-- own id counter seeded from MAX(id) at boot and hands the same id space to
-- both the persisted rows and any in-memory view, so letting SQLite mint ids
-- would let the two disagree after a restart (persistence spec 7.4 trap 4).
--
-- level is nullable and CHECK-constrained to the two tiers worth a durable row.
-- created_at_ms is epoch milliseconds, not the seconds every other table uses,
-- because the viewer renders sub-second timestamps and a seconds column would
-- silently round them away.
CREATE TABLE IF NOT EXISTS server_logs (
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
CREATE INDEX IF NOT EXISTS idx_server_logs_created ON server_logs(created_at_ms, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS server_logs;
-- +goose StatementEnd
