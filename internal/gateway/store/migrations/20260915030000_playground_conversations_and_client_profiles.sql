-- +goose Up
-- +goose StatementBegin

-- The Playground page's saved conversations. The page is the only client: it
-- lists summaries in a sidebar (never the bodies), loads one transcript when
-- you switch to it, and PUTs the whole thing back after each exchange. The
-- transcript is the client's own ChatMessage[] stored verbatim as JSON so a
-- restored conversation renders identically to the live one (routing meta,
-- reasoning and image thumbnails included).
--
-- created_at_ms / updated_at_ms are epoch MILLISECONDS, not the Unix seconds
-- every other gateway table uses, because the sidebar renders a relative time
-- ("3 minutes ago") by subtracting the value from the browser's Date.now(),
-- which is in milliseconds; a seconds column would read as ~57 years ago. This
-- mirrors the reference (conversations.ts:168-188) and the client contract
-- (playground-conversations.ts relativeTime).
CREATE TABLE IF NOT EXISTS playground_conversations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    title         TEXT    NOT NULL DEFAULT '',
    messages_json TEXT    NOT NULL DEFAULT '[]',
    model         TEXT,
    system_prompt TEXT,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

-- The sidebar lists newest-first; the index serves that ordering directly.
CREATE INDEX IF NOT EXISTS idx_playground_conversations_updated
    ON playground_conversations(updated_at_ms DESC, id DESC);

-- Per-client API keys (sk-cp-...) that carry a server-enforced system prompt.
-- A profile key is a second machine credential: it authenticates ONLY the
-- inference plane, never the /api dashboard surface. token_hash (SHA-256 of the
-- full key) is what the inference path resolves against, so the lookup is
-- indexed and does not depend on the secret's bytes (reference
-- system-prompt.ts:62-64), and rotating a profile overwrites it so the previous
-- key stops resolving on the next request.
--
-- The plaintext key is deliberately NOT stored: it is returned once from create
-- and rotate and never again. masked_key holds only the display form
-- (sk-c...<4 hex>), so this table carries no working credential at rest. That
-- is what lets a policy-filtered backup include it (the reference excludes only
-- users/sessions/url_tokens; spec-persistence.md §6.1) without a dump ever
-- leaking a live key, while a restored profile's key keeps authenticating
-- because its hash survives.
--
-- system_prompt is prepended server-side to every request made with this key;
-- NULL/empty injects nothing. A disabled row is rejected at auth exactly like
-- an unknown key.
CREATE TABLE IF NOT EXISTS client_profiles (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    token_hash    TEXT    NOT NULL UNIQUE,
    masked_key    TEXT    NOT NULL,
    system_prompt TEXT,
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS client_profiles;
DROP TABLE IF EXISTS playground_conversations;
-- +goose StatementEnd
