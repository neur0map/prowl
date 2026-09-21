package gateway

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// Benches lived in memory only, so a restart - which happens whenever the
// harness restarts - retried a key the provider had just limited for an hour,
// earning another bench. The dashboard's cooldown panel reads this table and
// was therefore always empty.

// CooldownSink persists bench state. It is an interface so the engine stays
// testable without a database.
type CooldownSink interface {
	SaveCooldown(key string, cd Cooldown, step int, lastHit time.Time)
	DropCooldown(key string)
}

// SetSink installs the persistence sink and replays whatever it holds.
func (c *CooldownEngine) SetSink(sink CooldownSink, restore map[string]PersistedCooldown) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sink = sink
	now := c.now()
	for key, row := range restore {
		if !row.Until.After(now) {
			continue
		}
		c.active[key] = Cooldown{Until: row.Until, Source: row.Source}
		c.steps[key] = ladderState{step: row.Step, lastHit: row.LastHit}
	}
}

// PersistedCooldown is one stored bench.
type PersistedCooldown struct {
	Until   time.Time
	Source  CooldownSource
	Step    int
	LastHit time.Time
}

// sqlCooldowns stores benches in the gateway database, through a single
// background writer.
//
// The writes MUST NOT happen inline. The store runs SQLite with one connection
// (SetMaxOpenConns(1)), and the engine records a bench while holding the
// cooldown lock, so writing there would hold that lock across a wait for the
// only connection. A request that already holds the connection and then needs
// the same lock completes the cycle: a live gateway deadlocked with every
// goroutine parked on the connection pool. Queueing keeps the lock free of
// I/O and keeps the router's hot path off the disk.
type sqlCooldowns struct {
	db      *sql.DB
	writes  chan cooldownWrite
	stopped chan struct{}
}

type cooldownWrite struct {
	key     string
	drop    bool
	cd      Cooldown
	step    int
	lastHit time.Time
}

// cooldownWriteQueue is deep enough to absorb a burst of benches from one
// exhausted chain without blocking the router.
const cooldownWriteQueue = 256

func newSQLCooldowns(db *sql.DB) *sqlCooldowns {
	s := &sqlCooldowns{
		db:      db,
		writes:  make(chan cooldownWrite, cooldownWriteQueue),
		stopped: make(chan struct{}),
	}
	go s.drain()
	return s
}

func (s *sqlCooldowns) drain() {
	defer close(s.stopped)
	for w := range s.writes {
		if w.drop {
			if _, err := s.db.Exec(
				"DELETE FROM rate_limit_cooldowns WHERE quota_key = ?", w.key); err != nil {
				slog.Debug("Could not clear a persisted cooldown", "quota_key", w.key, "error", err)
			}
			continue
		}
		if _, err := s.db.Exec(`
			INSERT INTO rate_limit_cooldowns(quota_key, until, source, step, last_hit, reason)
			VALUES(?, ?, ?, ?, ?, NULL)
			ON CONFLICT(quota_key) DO UPDATE SET
				until = excluded.until, source = excluded.source,
				step = excluded.step, last_hit = excluded.last_hit`,
			w.key, w.cd.Until.Unix(), string(w.cd.Source), w.step, w.lastHit.Unix()); err != nil {
			slog.Debug("Could not persist a cooldown", "quota_key", w.key, "error", err)
		}
	}
}

// enqueue never blocks. Persistence is best effort: the authoritative bench
// lives in memory, and stalling the router to record it would trade a
// correctness nicety for an outage.
func (s *sqlCooldowns) enqueue(w cooldownWrite) {
	select {
	case s.writes <- w:
	default:
		slog.Debug("Cooldown persistence queue is full; dropping a write",
			"quota_key", w.key)
	}
}

func (s *sqlCooldowns) SaveCooldown(key string, cd Cooldown, step int, lastHit time.Time) {
	s.enqueue(cooldownWrite{key: key, cd: cd, step: step, lastHit: lastHit})
}

func (s *sqlCooldowns) DropCooldown(key string) {
	s.enqueue(cooldownWrite{key: key, drop: true})
}

// Close stops the writer and waits for the queued writes to land.
func (s *sqlCooldowns) Close() {
	close(s.writes)
	<-s.stopped
}

// loadCooldowns reads the benches that have not expired and prunes the rest,
// so the table does not grow without bound.
func loadCooldowns(ctx context.Context, db *sql.DB) (map[string]PersistedCooldown, error) {
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx,
		"DELETE FROM rate_limit_cooldowns WHERE until <= ?", now); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		"SELECT quota_key, until, source, step, last_hit FROM rate_limit_cooldowns WHERE until > ?", now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]PersistedCooldown{}
	for rows.Next() {
		var (
			key            string
			until, lastHit int64
			source         string
			step           int
		)
		if err := rows.Scan(&key, &until, &source, &step, &lastHit); err != nil {
			return nil, err
		}
		out[key] = PersistedCooldown{
			Until:   time.Unix(until, 0).UTC(),
			Source:  CooldownSource(source),
			Step:    step,
			LastHit: time.Unix(lastHit, 0).UTC(),
		}
	}
	return out, rows.Err()
}
