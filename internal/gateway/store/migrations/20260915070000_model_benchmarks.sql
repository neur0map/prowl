-- +goose Up
-- +goose StatementBegin

-- Current independent capability scores matched to local catalogue rows. The
-- source identity and timestamp make every routing decision auditable and let a
-- stale cache remain useful during an upstream outage.
CREATE TABLE IF NOT EXISTS model_benchmarks (
    model_db_id       INTEGER PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
    source            TEXT    NOT NULL,
    source_model_id   TEXT    NOT NULL,
    source_model_name TEXT    NOT NULL,
    index_version     REAL,
    intelligence      REAL,
    coding            REAL,
    agentic           REAL,
    math              REAL,
    multilingual      REAL,
    refreshed_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_model_benchmarks_refreshed
    ON model_benchmarks(refreshed_at);

CREATE TABLE IF NOT EXISTS benchmark_refresh_state (
    source        TEXT PRIMARY KEY,
    status        TEXT    NOT NULL,
    last_attempt  INTEGER NOT NULL,
    last_success  INTEGER,
    matched       INTEGER NOT NULL DEFAULT 0,
    available     INTEGER NOT NULL DEFAULT 0,
    error         TEXT,
    index_version REAL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS benchmark_refresh_state;
DROP TABLE IF EXISTS model_benchmarks;
-- +goose StatementEnd
