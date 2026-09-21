package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestReadsDoNotQueueBehindAWrite is the outage this pool size fixes. With one
// connection, every read waited for whatever was in flight: a health pass or a
// second process holding the write lock stopped the dashboard answering at
// all, which read as a page that never finished loading. WAL allows readers
// alongside a writer, and the pool has to be wide enough to use it.
func TestReadsDoNotQueueBehindAWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()

	// Hold a write transaction open, the way a slow multi-statement update or
	// a competing process would.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx,
		"INSERT INTO settings(key, value, updated_at) VALUES('pool-probe', '1', 0)")
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// A reader must answer while that transaction is still open. One second is
	// far below the 30s busy timeout a starved read would burn.
	done := make(chan error, 1)
	go func() {
		var n int
		done <- db.QueryRowContext(ctx, "SELECT COUNT(*) FROM models").Scan(&n)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("a read blocked behind an open write transaction: the pool is serialising reads")
	}
}

// TestPoolAdmitsConcurrentReaders keeps the cap from silently returning to one,
// which is invisible until the product is under load.
func TestPoolAdmitsConcurrentReaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	st, err := Open(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	require.Greater(t, st.DB().Stats().MaxOpenConnections, 1,
		"reads must be able to run concurrently under WAL")
}
