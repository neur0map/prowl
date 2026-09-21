-- +goose Up
-- +goose StatementBegin

-- Embedding models live in their OWN table, never in `models`. The separation
-- is the design: a chat request can never misroute into an embedding model
-- (whose vector space is not chat-compatible), and embedding traffic never
-- pollutes the chat token budget. The routing unit is the `family` -- one
-- logical model identity plus a fixed dimension -- because vectors from
-- different models cannot be mixed.
CREATE TABLE IF NOT EXISTS embedding_models (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    family           TEXT    NOT NULL,
    platform         TEXT    NOT NULL,
    model_id         TEXT    NOT NULL,
    display_name     TEXT    NOT NULL,
    -- Nullable on purpose. The reference discovers a custom endpoint's
    -- dimension with a live probe this management surface does not run, so the
    -- dimension is unknown until routing exists. NULL reads as "-" in the UI;
    -- storing 0 would lie that the vector has no dimensions.
    dimensions       INTEGER,
    max_input_tokens INTEGER,
    priority         INTEGER NOT NULL DEFAULT 0,
    enabled          INTEGER NOT NULL DEFAULT 1,
    quota_label      TEXT    NOT NULL DEFAULT '',
    -- Binds a custom-endpoint row to its credential; catalog rows leave it
    -- NULL. ON DELETE CASCADE mirrors `models`, so dropping a custom key
    -- sweeps the embedding models it served.
    key_id           INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    UNIQUE(platform, model_id)
);
CREATE INDEX IF NOT EXISTS idx_embedding_models_family ON embedding_models(family);

-- Generative-media models (image, video, audio/TTS, transcription) live in
-- their OWN table for the same separation reason: a chat request can never
-- misroute into an image model, and media traffic stays out of the chat
-- budget. `modality` is image|video|audio|transcription; `meta_json` carries
-- per-adapter identity (request flavour, subtitle formats, upload ceiling,
-- provider model id) so a build predating a modality ignores rather than
-- ingests those rows.
CREATE TABLE IF NOT EXISTS media_models (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    platform     TEXT    NOT NULL,
    model_id     TEXT    NOT NULL,
    display_name TEXT    NOT NULL,
    modality     TEXT    NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    quota_label  TEXT    NOT NULL DEFAULT '',
    meta_json    TEXT,
    key_id       INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    UNIQUE(platform, model_id)
);
CREATE INDEX IF NOT EXISTS idx_media_models_modality ON media_models(modality);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS media_models;
DROP TABLE IF EXISTS embedding_models;
-- +goose StatementEnd
