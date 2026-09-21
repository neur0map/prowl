// Package store is the gateway's relational state.
//
// The routing engine ported from FreeLLMAPI decides from evidence, not from
// configuration: reliability is a decay-weighted posterior over past
// requests, speed is measured throughput and time-to-first-byte, and quota
// headroom is a running count against a provider's windows. None of that
// survives in flat files - a restart would hand back quota the provider has
// already counted, and would forget which endpoint is benched and why. So the
// gateway keeps its own SQLite database beside the agent's.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	gooseOnce sync.Once

	// memSeq names in-memory databases uniquely within the process.
	memSeq atomic.Int64
)

// pragmas match the agent database's settings. WAL matters most: the router
// reads scoring history on the request path while a background refresh
// writes, and without it those two would serialise.
var pragmas = map[string]string{
	"foreign_keys": "ON",
	"journal_mode": "WAL",
	"temp_store":   "MEMORY",
	"synchronous":  "NORMAL",
	"busy_timeout": "30000",
}

// Store is a handle on the gateway's database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the gateway database under dir and applies
// every pending migration.
func Open(ctx context.Context, dir string) (*Store, error) {
	// 0700 because this directory holds the encrypted key store, the master
	// key and the request trail. Another user on the machine has no business
	// listing it, let alone reading it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create gateway state directory: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "gateway.db")))
	if err != nil {
		return nil, fmt.Errorf("open gateway database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open gateway database: %w", err)
	}
	configurePool(db)

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// OpenMemory returns an isolated in-memory store, for tests.
//
// Each call gets its own database. A shared-cache in-memory DSN would make
// every caller in the process share one database, which silently couples
// parallel tests -- the first symptom is a UNIQUE violation from a row another
// test inserted.
func OpenMemory(ctx context.Context) (*Store, error) {
	name := fmt.Sprintf("prowl-gateway-%d-%d", os.Getpid(), memSeq.Add(1))
	db, err := sql.Open("sqlite", dsn("/"+name)+"&mode=memory&cache=shared")
	if err != nil {
		return nil, err
	}
	configurePool(db)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// poolSize bounds the connections the gateway opens on its database.
//
// It was 1, on the reasoning that SQLite serialises writes anyway. But the
// pool does not distinguish reads from writes, so a single cap serialises
// EVERY read behind whatever is in flight: one slow write, one health pass
// that touches the database, or a second process holding the write lock, and
// the whole dashboard stops answering while each request waits its turn. That
// is what "the page never finishes loading" looked like.
//
// WAL lets readers run concurrently with a writer, which is the property the
// cap was throwing away. Writes stay serialised by SQLite itself, take the
// write lock up front via _txlock=immediate, and wait out a competing writer
// through busy_timeout instead of failing.
const poolSize = 8

func configurePool(db *sql.DB) {
	db.SetMaxOpenConns(poolSize)
	db.SetMaxIdleConns(poolSize)
	// A connection idle this long is almost certainly from a burst that has
	// passed; closing it keeps the WAL reader count down.
	db.SetConnMaxIdleTime(2 * time.Minute)
}

// dsn builds a connection string with the same pragmas the agent database
// uses. foreign_keys in particular is load-bearing rather than cosmetic: the
// request trail cascades to its attempts, and pruning silently leaves orphans
// without it.
func dsn(path string) string {
	params := url.Values{}
	for name, value := range pragmas {
		params.Add("_pragma", fmt.Sprintf("%s(%s)", name, value))
	}
	// BEGIN IMMEDIATE so a writer takes the reserved lock up front instead of
	// upgrading mid-transaction and deadlocking against another writer.
	params.Set("_txlock", "immediate")
	return fmt.Sprintf("file:%s?%s", path, params.Encode())
}

func migrate(ctx context.Context, db *sql.DB) error {
	gooseOnce.Do(func() {
		if testing.Testing() {
			goose.SetLogger(goose.NopLogger())
		}
	})
	// A per-call provider rather than goose's global base FS: the agent
	// database registers its own, and sharing that global would make
	// whichever package initialised last win.
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("gateway migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return fmt.Errorf("gateway migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("gateway migrations: %w", err)
	}
	return nil
}

// DB exposes the handle for the packages that own queries against it.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }
