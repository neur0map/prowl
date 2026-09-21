// Package store is the SQLite persistence layer: schema, files/graph writes,
// FTS search, and graph read queries for Prowl's per-folder index.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// SchemaVersion is bumped whenever Open must migrate an existing cache.
const SchemaVersion = 5

// ErrGenerationIncomplete indicates that a failed refresh deliberately withheld
// publication and readers must wait for a successful repair.
var ErrGenerationIncomplete = errors.New("index generation is incomplete")

// ReadGuard pins one published generation for a complete logical read. The
// returned release function must be called after the operation's final query.
type ReadGuard func(context.Context) (release func(), err error)

type sqlRunner interface {
	Exec(string, ...any) (sql.Result, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type writeRunner interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	Prepare(string) (*sql.Stmt, error)
	QueryRow(string, ...any) *sql.Row
}

// Store wraps a SQLite connection to a single project's index.db. A generation
// store reuses the same database through one transaction and must be completed
// through Generation.Commit or Generation.Rollback rather than closed directly.
type Store struct {
	db *sql.DB
	tx *sql.Tx
}

func (s *Store) sql() sqlRunner {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

func (s *Store) writeTransaction(fn func(writeRunner) error) error {
	if s.tx != nil {
		return fn(s.tx)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Generation owns an atomic derived-index publication transaction.
type Generation struct {
	Store *Store
	tx    *sql.Tx
	done  bool
}

// BeginGeneration starts a transaction whose writes remain invisible to readers
// until Commit. The returned Store can be passed to the normal indexing APIs.
func (s *Store) BeginGeneration(ctx context.Context) (*Generation, error) {
	if s.tx != nil {
		return nil, fmt.Errorf("nested store generation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Generation{Store: &Store{db: s.db, tx: tx}, tx: tx}, nil
}

// Commit atomically publishes every write in the generation.
func (g *Generation) Commit() error {
	if g == nil || g.done {
		return fmt.Errorf("store generation already completed")
	}
	g.done = true
	return g.tx.Commit()
}

// Rollback discards every write in the generation.
func (g *Generation) Rollback() error {
	if g == nil || g.done {
		return nil
	}
	g.done = true
	return g.tx.Rollback()
}

// Open opens (creating if needed) the index database at path, applies the
// schema, and records the schema version. WAL mode lets `index` write while
// the MCP server reads.
func Open(path string) (*Store, error) {
	_, statErr := os.Stat(path)
	existed := statErr == nil
	dsn := "file:" + path + "?_journal_mode=WAL&_foreign_keys=on&_recursive_triggers=on&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	current := currentSchemaVersion(db)
	if current > SchemaVersion {
		db.Close()
		return nil, fmt.Errorf("database schema version %d is newer than supported version %d", current, SchemaVersion)
	}
	if existed && current < SchemaVersion {
		if _, err := backupDatabase(db, path, current); err != nil {
			db.Close()
			return nil, fmt.Errorf("backup schema v%d database: %w", current, err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	// Migrating an older index: drop the pre-fts_docs symbol triggers so the
	// CREATE TRIGGER IF NOT EXISTS in the schema recreates them docs-aware. IF
	// NOT EXISTS alone would keep a stale, docs-unaware trigger and leave
	// fts_docs permanently empty. Gated on the same condition as the backup so a
	// steady-state open performs no schema write.
	if existed && current < SchemaVersion {
		if _, err := tx.Exec(`DROP TRIGGER IF EXISTS symbols_ai; DROP TRIGGER IF EXISTS symbols_ad`); err != nil {
			_ = tx.Rollback()
			db.Close()
			return nil, fmt.Errorf("drop legacy symbol triggers: %w", err)
		}
	}
	if _, err := tx.Exec(schemaSQL); err != nil {
		_ = tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Additive migration for indexes created before the symbols.complexity column
	// existed. CREATE TABLE IF NOT EXISTS above does not alter an existing table,
	// so add the column here; the duplicate-column error on fresh DBs is ignored.
	if _, err := tx.Exec(`ALTER TABLE symbols ADD COLUMN complexity INTEGER NOT NULL DEFAULT 1`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("migrate symbols complexity: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE files ADD COLUMN redacted INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("migrate files redacted: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE symbols ADD COLUMN doc TEXT`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		_ = tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("migrate symbols doc: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, SchemaVersion); err != nil {
		_ = tx.Rollback()
		db.Close()
		return nil, fmt.Errorf("set schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("commit schema migration: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenContext opens and migrates a store while honoring a startup deadline.
// Open retains its existing behavior and five-second busy timeout.
func OpenContext(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, statErr := os.Stat(path)
	existed := statErr == nil
	busyMillis := int64(5000)
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline).Milliseconds()
		if remaining < 1 {
			remaining = 1
		}
		if remaining < busyMillis {
			busyMillis = remaining
		}
	}
	dsn := "file:" + path + "?_journal_mode=WAL&_foreign_keys=on&_recursive_triggers=on&_busy_timeout=" + strconv.FormatInt(busyMillis, 10)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) {
		return nil, errors.Join(boundedStoreOpenError(ctx, err), db.Close())
	}
	current, err := currentSchemaVersionContext(ctx, db)
	if err != nil {
		return fail(err)
	}
	if current > SchemaVersion {
		return fail(fmt.Errorf("database schema version %d is newer than supported version %d", current, SchemaVersion))
	}
	if existed && current < SchemaVersion {
		if _, err := backupDatabaseContext(ctx, db, path, current); err != nil {
			return fail(fmt.Errorf("backup schema v%d database: %w", current, err))
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	rollback := func(err error) (*Store, error) {
		return nil, errors.Join(boundedStoreOpenError(ctx, err), tx.Rollback(), db.Close())
	}
	// See Open: drop the pre-fts_docs symbol triggers only when migrating so the
	// schema's CREATE TRIGGER IF NOT EXISTS recreates them docs-aware, while a
	// steady-state open stays free of schema writes.
	if existed && current < SchemaVersion {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS symbols_ai; DROP TRIGGER IF EXISTS symbols_ad`); err != nil {
			return rollback(fmt.Errorf("drop legacy symbol triggers: %w", err))
		}
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return rollback(fmt.Errorf("apply schema: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE symbols ADD COLUMN complexity INTEGER NOT NULL DEFAULT 1`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return rollback(fmt.Errorf("migrate symbols complexity: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE files ADD COLUMN redacted INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return rollback(fmt.Errorf("migrate files redacted: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE symbols ADD COLUMN doc TEXT`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return rollback(fmt.Errorf("migrate symbols doc: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, SchemaVersion); err != nil {
		return rollback(fmt.Errorf("set schema_version: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("commit schema migration: %w", err))
	}
	return &Store{db: db}, nil
}

func boundedStoreOpenError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && (sqliteErr.Code == sqlite3.ErrBusy || sqliteErr.Code == sqlite3.ErrLocked) {
			return errors.Join(context.DeadlineExceeded, err)
		}
	}
	return err
}

func currentSchemaVersionContext(ctx context.Context, db *sql.DB) (int, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var value string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	version, _ := strconv.Atoi(value)
	return version, nil
}

func backupDatabaseContext(ctx context.Context, db *sql.DB, path string, version int) (string, error) {
	base := fmt.Sprintf("%s.bak-v%d", path, version)
	backup := base
	if _, err := os.Stat(backup); err == nil {
		backup = fmt.Sprintf("%s-%d", base, time.Now().UTC().UnixNano())
	}
	quoted := strings.ReplaceAll(backup, "'", "''")
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return "", err
	}
	return backup, nil
}

func currentSchemaVersion(db *sql.DB) int {
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&exists); err != nil || exists == 0 {
		return 0
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&value); err != nil {
		return 0
	}
	version, _ := strconv.Atoi(value)
	return version
}

func backupDatabase(db *sql.DB, path string, version int) (string, error) {
	base := fmt.Sprintf("%s.bak-v%d", path, version)
	backup := base
	if _, err := os.Stat(backup); err == nil {
		backup = fmt.Sprintf("%s-%d", base, time.Now().UTC().UnixNano())
	}
	// VACUUM INTO captures committed WAL contents and produces a self-contained,
	// verified SQLite file rather than a potentially inconsistent byte copy.
	quoted := strings.ReplaceAll(backup, "'", "''")
	if _, err := db.Exec(`VACUUM INTO '` + quoted + `'`); err != nil {
		return "", err
	}
	return backup, nil
}

// RestoreBackup atomically replaces a closed database with a migration backup.
// Callers must close every Store using dbPath before invoking it.
func RestoreBackup(dbPath, backupPath string) error {
	in, err := os.Open(backupPath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dbPath + ".restore-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	previous := dbPath + ".restore-previous"
	_ = os.Remove(previous)
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, previous); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		_ = os.Rename(previous, dbPath)
		return err
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// SetMeta upserts a key/value into the meta table.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.sql().Exec(
		`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// SetMetaContext is SetMeta with cancellation support.
func (s *Store) SetMetaContext(ctx context.Context, key, value string) error {
	_, err := s.sql().ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// GetMeta returns the value for key, or "" if absent.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.sql().QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// RequirePublishedGeneration fails closed after a refresh withheld publication.
func (s *Store) RequirePublishedGeneration() error {
	state, err := s.GetMeta("index_state")
	if err != nil {
		return err
	}
	if state != "complete" {
		return ErrGenerationIncomplete
	}
	return nil
}
