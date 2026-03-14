package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/neur0map/prowl/internal/graph"
	"github.com/viant/sqlite-vec/engine"
	"github.com/viant/sqlite-vec/vector"
	_ "modernc.org/sqlite"
)

func init() {
	engine.RegisterVectorFunctions(nil)
}

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

// UpsertEdge inserts an edge if it doesn't exist (confidence defaults to 1.0).
func (s *Store) UpsertEdge(sourceID, targetID int64, edgeType string) error {
	return s.UpsertEdgeWithConfidence(sourceID, targetID, edgeType, 1.0)
}

// UpsertEdgeWithConfidence inserts an edge with a confidence score.
func (s *Store) UpsertEdgeWithConfidence(sourceID, targetID int64, edgeType string, confidence float64) error {
	_, err := s.db.Exec(
		`INSERT INTO edges (source_file_id, target_file_id, type, confidence) VALUES (?, ?, ?, ?)
		 ON CONFLICT(source_file_id, target_file_id, type) DO UPDATE SET confidence = excluded.confidence`,
		sourceID, targetID, edgeType, confidence,
	)
	return err
}

// InsertCommunity inserts a community record.
func (s *Store) InsertCommunity(id int, name, label string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO communities (id, name, label) VALUES (?, ?, ?)`,
		id, name, label,
	)
	return err
}

// InsertCommunityMember links a file to a community.
func (s *Store) InsertCommunityMember(fileID int64, communityID int) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO community_members (file_id, community_id) VALUES (?, ?)`,
		fileID, communityID,
	)
	return err
}

// ClearCommunities removes all community data (called before re-running community detection).
func (s *Store) ClearCommunities() error {
	_, err := s.db.Exec("DELETE FROM community_members")
	if err != nil {
		return err
	}
	_, err = s.db.Exec("DELETE FROM communities")
	return err
}

// CommunityRow represents a community with its member count.
type CommunityRow struct {
	ID          int
	Name        string
	Label       string
	MemberCount int
}

// CallsOf returns file paths that the given file calls into (outgoing CALLS edges).
func (s *Store) CallsOf(path string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT t.path FROM edges e
		JOIN files f ON f.id = e.source_file_id
		JOIN files t ON t.id = e.target_file_id
		WHERE f.path = ? AND e.type = 'CALLS'
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

// CallersOf returns file paths that call into the given file (incoming CALLS edges).
func (s *Store) CallersOf(path string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT f.path FROM edges e
		JOIN files f ON f.id = e.source_file_id
		JOIN files t ON t.id = e.target_file_id
		WHERE t.path = ? AND e.type = 'CALLS'
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

// CommunityOf returns the community name for a file, or "" if not assigned.
func (s *Store) CommunityOf(path string) (string, error) {
	var name string
	err := s.db.QueryRow(`
		SELECT c.name FROM communities c
		JOIN community_members cm ON cm.community_id = c.id
		JOIN files f ON f.id = cm.file_id
		WHERE f.path = ?
	`, path).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// AllCommunities returns all communities with member counts.
func (s *Store) AllCommunities() ([]CommunityRow, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.name, c.label, COUNT(cm.file_id) AS member_count
		FROM communities c
		LEFT JOIN community_members cm ON cm.community_id = c.id
		GROUP BY c.id, c.name, c.label
		ORDER BY c.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CommunityRow
	for rows.Next() {
		var cr CommunityRow
		if err := rows.Scan(&cr.ID, &cr.Name, &cr.Label, &cr.MemberCount); err != nil {
			return nil, err
		}
		result = append(result, cr)
	}
	return result, rows.Err()
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

// DeleteFile removes a file by path. CASCADE constraints handle symbols, edges, embeddings, and community_members.
func (s *Store) DeleteFile(path string) error {
	_, err := s.db.Exec("DELETE FROM files WHERE path = ?", path)
	return err
}

// AllEdges returns every edge with source/target paths resolved from file IDs.
func (s *Store) AllEdges() ([]graph.Edge, error) {
	rows, err := s.db.Query(`
		SELECT f.path, t.path, e.type, e.confidence
		FROM edges e
		JOIN files f ON f.id = e.source_file_id
		JOIN files t ON t.id = e.target_file_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []graph.Edge
	for rows.Next() {
		var e graph.Edge
		if err := rows.Scan(&e.SourcePath, &e.TargetPath, &e.Type, &e.Confidence); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// ---------------------------------------------------------------------------
// Embedding storage
// ---------------------------------------------------------------------------

// StoredEmbedding represents a file's embedding from the database.
type StoredEmbedding struct {
	FileID   int64
	FilePath string
	Vector   []float32
}

// SearchResult represents a semantic search result.
type SearchResult struct {
	FilePath   string
	Score      float64
	Signatures string
}

// UpsertEmbedding stores a vector for a file.
func (s *Store) UpsertEmbedding(fileID int64, vec []float32, textHash string) error {
	blob, err := vector.EncodeEmbedding(vec)
	if err != nil {
		return fmt.Errorf("encode embedding: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO embeddings (file_id, vector, text_hash) VALUES (?, ?, ?)
		 ON CONFLICT(file_id) DO UPDATE SET vector = excluded.vector, text_hash = excluded.text_hash`,
		fileID, blob, textHash,
	)
	return err
}

// DeleteEmbedding removes a file's vector.
func (s *Store) DeleteEmbedding(fileID int64) error {
	_, err := s.db.Exec("DELETE FROM embeddings WHERE file_id = ?", fileID)
	return err
}

// EmbeddingTextHash returns the stored text hash for a file.
func (s *Store) EmbeddingTextHash(fileID int64) (string, error) {
	var hash string
	err := s.db.QueryRow("SELECT text_hash FROM embeddings WHERE file_id = ?", fileID).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return hash, err
}

// AllEmbeddings returns all stored embeddings for search.
func (s *Store) AllEmbeddings() ([]StoredEmbedding, error) {
	rows, err := s.db.Query(`
		SELECT e.file_id, f.path, e.vector
		FROM embeddings e
		JOIN files f ON f.id = e.file_id
		ORDER BY f.path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []StoredEmbedding
	for rows.Next() {
		var se StoredEmbedding
		var blob []byte
		if err := rows.Scan(&se.FileID, &se.FilePath, &blob); err != nil {
			return nil, err
		}
		vec, err := vector.DecodeEmbedding(blob)
		if err != nil {
			return nil, fmt.Errorf("decode embedding for %s: %w", se.FilePath, err)
		}
		se.Vector = vec
		result = append(result, se)
	}
	return result, rows.Err()
}

// SearchSimilar finds the most similar files to a query vector.
// Computes cosine similarity in Go using all stored embeddings.
func (s *Store) SearchSimilar(queryVec []float32, limit int) ([]SearchResult, error) {
	all, err := s.AllEmbeddings()
	if err != nil {
		return nil, err
	}

	type scored struct {
		filePath string
		score    float64
	}
	results := make([]scored, 0, len(all))
	for _, emb := range all {
		sim, err := vector.CosineSimilarity(queryVec, emb.Vector)
		if err != nil {
			continue
		}
		results = append(results, scored{filePath: emb.FilePath, score: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}

	// Gather signatures for top results.
	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{FilePath: r.filePath, Score: r.score}
		var sigs sql.NullString
		err := s.db.QueryRow(`
			SELECT GROUP_CONCAT(s.signature, char(10))
			FROM symbols s
			JOIN files f ON f.id = s.file_id
			WHERE f.path = ?
		`, r.filePath).Scan(&sigs)
		if err == nil && sigs.Valid {
			out[i].Signatures = sigs.String
		}
	}
	return out, nil
}
