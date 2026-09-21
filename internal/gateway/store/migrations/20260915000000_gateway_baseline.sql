-- +goose Up
-- +goose StatementBegin

-- Provider credentials. Many keys per provider is the norm, not the
-- exception: free tiers are small, so operators stack several and let the
-- router spread load across them.
CREATE TABLE IF NOT EXISTS api_keys (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    platform         TEXT    NOT NULL,
    label            TEXT    NOT NULL DEFAULT '',
    encrypted_key    TEXT    NOT NULL,
    iv               TEXT    NOT NULL,
    auth_tag         TEXT    NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'unknown',
    enabled          INTEGER NOT NULL DEFAULT 1,
    base_url         TEXT,
    model_scope_json TEXT,
    last_checked_at  INTEGER,
    last_health_error TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_keys_platform ON api_keys(platform);

-- The chat model catalog, plus one row per custom relay model. endpoint_scope
-- is part of the identity so two relays serving the same model_id are scored
-- and rate-limited independently.
CREATE TABLE IF NOT EXISTS models (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    platform             TEXT    NOT NULL,
    model_id             TEXT    NOT NULL,
    display_name         TEXT    NOT NULL,
    intelligence_rank    INTEGER NOT NULL DEFAULT 0,
    speed_rank           INTEGER NOT NULL DEFAULT 0,
    size_label           TEXT    NOT NULL DEFAULT '',
    rpm_limit            INTEGER,
    rpd_limit            INTEGER,
    tpm_limit            INTEGER,
    tpd_limit            INTEGER,
    monthly_token_budget TEXT    NOT NULL DEFAULT '',
    context_window       INTEGER,
    enabled              INTEGER NOT NULL DEFAULT 1,
    supports_vision      INTEGER NOT NULL DEFAULT 0,
    supports_tools       INTEGER NOT NULL DEFAULT 0,
    supports_reasoning   INTEGER NOT NULL DEFAULT 0,
    key_id               INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    paid_input_per_m     REAL,
    paid_output_per_m    REAL,
    source               TEXT    NOT NULL DEFAULT 'catalog',
    endpoint_scope       TEXT    NOT NULL DEFAULT '',
    UNIQUE(platform, model_id, endpoint_scope)
);
CREATE INDEX IF NOT EXISTS idx_models_endpoint_scope
    ON models(endpoint_scope) WHERE endpoint_scope <> '';
CREATE INDEX IF NOT EXISTS idx_models_platform ON models(platform);

-- One row per inference call. This table IS the reliability and speed
-- evidence: the router's decay-weighted posteriors are computed from it, so
-- its retention window bounds how much history routing can see.
CREATE TABLE IF NOT EXISTS requests (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at     INTEGER NOT NULL,
    platform       TEXT    NOT NULL,
    model_id       TEXT    NOT NULL,
    endpoint_scope TEXT    NOT NULL DEFAULT '',
    key_id         INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
    status         INTEGER NOT NULL DEFAULT 0,
    outcome        TEXT    NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    estimated      INTEGER NOT NULL DEFAULT 0,
    latency_ms     INTEGER NOT NULL DEFAULT 0,
    ttfb_ms        INTEGER,
    cost_usd       REAL    NOT NULL DEFAULT 0,
    attempts       INTEGER NOT NULL DEFAULT 1,
    routed_from    TEXT,
    class          TEXT,
    effort         TEXT,
    error_kind     TEXT,
    error_message  TEXT
);
-- The scoring query buckets by (platform, model, key, age), so this index is
-- what keeps the per-request refresh off the interactive path.
CREATE INDEX IF NOT EXISTS idx_requests_scoring
    ON requests(platform, model_id, endpoint_scope, key_id, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at);

-- Each upstream hop of a request, so a failover trail can be reconstructed.
CREATE TABLE IF NOT EXISTS request_attempts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id  INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    attempt     INTEGER NOT NULL,
    platform    TEXT    NOT NULL,
    model_id    TEXT    NOT NULL,
    key_id      INTEGER,
    status      INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    error_kind  TEXT,
    error_message TEXT,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_attempts_request ON request_attempts(request_id);

-- Hourly rollup. Analytics read this rather than the raw trail so pruning
-- the trail does not erase the charts.
CREATE TABLE IF NOT EXISTS request_hourly (
    hour_start     INTEGER NOT NULL,
    platform       TEXT    NOT NULL,
    model_id       TEXT    NOT NULL,
    endpoint_scope TEXT    NOT NULL DEFAULT '',
    requests       INTEGER NOT NULL DEFAULT 0,
    successes      INTEGER NOT NULL DEFAULT 0,
    failures       INTEGER NOT NULL DEFAULT 0,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_usd       REAL    NOT NULL DEFAULT 0,
    latency_ms_sum INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hour_start, platform, model_id, endpoint_scope)
);

-- Sliding rate-limit counters per window. Persisted because a restart must
-- not hand back quota the provider has already counted.
CREATE TABLE IF NOT EXISTS rate_limit_usage (
    quota_key    TEXT    NOT NULL,
    window_kind  TEXT    NOT NULL,
    window_start INTEGER NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,
    tokens       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (quota_key, window_kind, window_start)
);
CREATE INDEX IF NOT EXISTS idx_rate_limit_usage_window ON rate_limit_usage(window_start);

-- Active benches. Persisted with their provenance so a restart cannot
-- resurrect an endpoint the provider told us to leave alone.
CREATE TABLE IF NOT EXISTS rate_limit_cooldowns (
    quota_key  TEXT    NOT NULL PRIMARY KEY,
    until      INTEGER NOT NULL,
    source     TEXT    NOT NULL,
    step       INTEGER NOT NULL DEFAULT 0,
    last_hit   INTEGER NOT NULL,
    reason     TEXT
);
CREATE INDEX IF NOT EXISTS idx_rate_limit_cooldowns_until ON rate_limit_cooldowns(until);

-- What a provider itself last told us about remaining quota, keyed by the
-- pool the limit applies to (often an account, not a model).
CREATE TABLE IF NOT EXISTS provider_quota_state (
    quota_pool_key    TEXT    NOT NULL PRIMARY KEY,
    platform          TEXT    NOT NULL,
    requests_limit    INTEGER,
    requests_remaining INTEGER,
    tokens_limit      INTEGER,
    tokens_remaining  INTEGER,
    resets_at         INTEGER,
    observed_at       INTEGER NOT NULL
);

-- The raw header observations behind that state, kept briefly so a wrong
-- inference can be explained rather than just corrected.
CREATE TABLE IF NOT EXISTS provider_quota_observations (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    quota_pool_key TEXT    NOT NULL,
    observed_at    INTEGER NOT NULL,
    headers_json   TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_provider_quota_observations_at
    ON provider_quota_observations(observed_at);

-- Named fallback chains. A profile is how an operator says "for coding, try
-- these models in this order".
CREATE TABLE IF NOT EXISTS profiles (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    active     INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS profile_models (
    profile_id  INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    model_db_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    PRIMARY KEY (profile_id, model_db_id)
);

-- The single global chain, used when no profile is active.
CREATE TABLE IF NOT EXISTS fallback_config (
    model_db_id INTEGER NOT NULL PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1
);

-- Free-form operator settings, including the routing strategy and the
-- lifetime usage totals that survive trail pruning.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Per-model routing weight overrides, so one bad model can be demoted
-- without abandoning the strategy.
CREATE TABLE IF NOT EXISTS model_overrides (
    model_id TEXT NOT NULL PRIMARY KEY,
    weight   REAL NOT NULL DEFAULT 1
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS model_overrides;
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS fallback_config;
DROP TABLE IF EXISTS profile_models;
DROP TABLE IF EXISTS profiles;
DROP TABLE IF EXISTS provider_quota_observations;
DROP TABLE IF EXISTS provider_quota_state;
DROP TABLE IF EXISTS rate_limit_cooldowns;
DROP TABLE IF EXISTS rate_limit_usage;
DROP TABLE IF EXISTS request_hourly;
DROP TABLE IF EXISTS request_attempts;
DROP TABLE IF EXISTS requests;
DROP TABLE IF EXISTS models;
DROP TABLE IF EXISTS api_keys;
-- +goose StatementEnd
