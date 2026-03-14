package store

const schemaSQL = `
CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY,
    path         TEXT NOT NULL UNIQUE,
    hash         TEXT NOT NULL,
    last_indexed INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS symbols (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    file_id     INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    start_line  INTEGER NOT NULL,
    end_line    INTEGER NOT NULL,
    is_exported BOOLEAN NOT NULL DEFAULT 0,
    signature   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS edges (
    source_file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    target_file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    type           TEXT NOT NULL,
    PRIMARY KEY (source_file_id, target_file_id, type)
);

CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_file_id, type);
CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file_id);
CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
`
