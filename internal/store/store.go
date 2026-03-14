package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/neur0map/prowl/internal/graph"
	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database for persisting the code graph.
type Store struct {
	db *sql.DB
}

// Open creates or opens a SQLite database at the given path.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.Exec("PRAGMA foreign_keys = ON")
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema init: %w", err)
	}
	return &Store{db: db}, nil
}

// Close shuts down the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertFile inserts or updates a file record, returning its ID.
func (s *Store) UpsertFile(path, hash string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO files (path, hash, last_indexed) VALUES (?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET hash = excluded.hash, last_indexed = excluded.last_indexed`,
		path, hash, now,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		// ON CONFLICT returns 0 for LastInsertId; query for actual ID
		row := s.db.QueryRow("SELECT id FROM files WHERE path = ?", path)
		row.Scan(&id)
	}
	return id, nil
}

// FileHash returns the stored hash for a file, or "" if not found.
func (s *Store) FileHash(path string) (string, error) {
	var hash string
	err := s.db.QueryRow("SELECT hash FROM files WHERE path = ?", path).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// FileID returns the ID for a file path, or 0 if not found.
func (s *Store) FileID(path string) (int64, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM files WHERE path = ?", path).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// InsertSymbols bulk-inserts symbols for a file.
func (s *Store) InsertSymbols(fileID int64, syms []graph.Symbol) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO symbols (name, kind, file_id, start_line, end_line, is_exported, signature)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, sym := range syms {
		_, err := stmt.Exec(sym.Name, sym.Kind, fileID, sym.StartLine, sym.EndLine, sym.IsExported, sym.Signature)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSymbolsForFile removes all symbols belonging to a file.
func (s *Store) DeleteSymbolsForFile(fileID int64) error {
	_, err := s.db.Exec("DELETE FROM symbols WHERE file_id = ?", fileID)
	return err
}

// DeleteEdgesFromFile removes all outgoing edges from a file.
func (s *Store) DeleteEdgesFromFile(fileID int64) error {
	_, err := s.db.Exec("DELETE FROM edges WHERE source_file_id = ?", fileID)
	return err
}

// UpsertEdge inserts an edge if it doesn't exist.
func (s *Store) UpsertEdge(sourceID, targetID int64, edgeType string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO edges (source_file_id, target_file_id, type) VALUES (?, ?, ?)`,
		sourceID, targetID, edgeType,
	)
	return err
}

// UpstreamOf returns file paths that import the given file (reverse edges).
func (s *Store) UpstreamOf(path string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT f.path FROM edges e
		JOIN files f ON f.id = e.source_file_id
		JOIN files t ON t.id = e.target_file_id
		WHERE t.path = ? AND e.type = 'IMPORTS'
		ORDER BY f.path
	`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// ImportsOf returns file paths that the given file imports.
func (s *Store) ImportsOf(path string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT t.path FROM edges e
		JOIN files f ON f.id = e.source_file_id
		JOIN files t ON t.id = e.target_file_id
		WHERE f.path = ? AND e.type = 'IMPORTS'
		ORDER BY t.path
	`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// SymbolsForFile returns all symbols for a given file path.
func (s *Store) SymbolsForFile(path string) ([]graph.Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.name, s.kind, s.start_line, s.end_line, s.is_exported, s.signature
		FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE f.path = ?
		ORDER BY s.start_line
	`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var syms []graph.Symbol
	for rows.Next() {
		var sym graph.Symbol
		sym.FilePath = path
		if err := rows.Scan(&sym.Name, &sym.Kind, &sym.StartLine, &sym.EndLine, &sym.IsExported, &sym.Signature); err != nil {
			return nil, err
		}
		syms = append(syms, sym)
	}
	return syms, rows.Err()
}

// AllFiles returns all indexed file paths.
func (s *Store) AllFiles() ([]string, error) {
	rows, err := s.db.Query("SELECT path FROM files ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// Stats returns counts of files, symbols, and edges.
func (s *Store) Stats() (files, symbols, edges int, err error) {
	s.db.QueryRow("SELECT COUNT(*) FROM files").Scan(&files)
	s.db.QueryRow("SELECT COUNT(*) FROM symbols").Scan(&symbols)
	s.db.QueryRow("SELECT COUNT(*) FROM edges").Scan(&edges)
	return
}
