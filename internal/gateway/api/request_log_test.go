package api

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServerLogCursorSurvivesClearAndRestart is the durable-id regression. The
// old store seeded a process-local counter from MAX(id) at boot, so a Clear
// (DELETE) followed by a restart re-seeded it from an empty table and minted ids
// starting at 1 again. A poller still holding a larger sinceId then skipped every
// new row forever. With SQLite AUTOINCREMENT the sequence high-water survives the
// DELETE, so a post-clear row is guaranteed an id above any held cursor.
func TestServerLogCursorSurvivesClearAndRestart(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})
	db := s.engine.DB()
	ctx := t.Context()

	st := logStoreFor(db)
	for i := range 5 {
		_, err := st.record(ctx, serverLogRecord{Level: "warn", Message: fmt.Sprintf("line %d", i)})
		require.NoError(t, err)
	}
	cursor := st.maxID(ctx)
	require.EqualValues(t, 5, cursor)

	require.NoError(t, st.clear(ctx))

	// A fresh store stands in for a process restart: id allocation must resume
	// from the durable sequence, not from MAX(id) over the emptied table.
	restarted := &serverLogStore{db: db}
	entry, err := restarted.record(ctx, serverLogRecord{Level: "error", Message: "after clear"})
	require.NoError(t, err)
	require.Greater(t, entry.ID, cursor,
		"ids must not be reused after clear+restart, or a held cursor strands the new row")

	rows, err := restarted.query(ctx, &cursor, nil, "", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a client holding the pre-clear cursor must still see the post-clear row")
	require.Equal(t, "after clear", rows[0].Message)
	require.Equal(t, entry.ID, rows[0].ID)
}

// TestServerLogConcurrentRecordsMintUniqueGaplessIds proves concurrent records
// get unique, gapless ids assigned in commit order, and that a client walking
// from a pre-burst cursor surfaces every row exactly once. The old in-memory
// counter minted an id under a lock and committed the row outside it, so a
// higher id could be handed out before a lower row committed; SQLite assigns the
// id at commit, closing that gap.
func TestServerLogConcurrentRecordsMintUniqueGaplessIds(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: "prowl-test-machine-key"})
	db := s.engine.DB()
	ctx := t.Context()

	st := logStoreFor(db)
	require.NoError(t, st.clear(ctx))
	base := st.maxID(ctx)

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.record(ctx, serverLogRecord{Level: "warn", Message: fmt.Sprintf("burst-%d", i)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var count, distinct, span int64
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT id), COALESCE(MAX(id)-MIN(id)+1, 0)
		   FROM server_logs WHERE message LIKE 'burst-%'`).Scan(&count, &distinct, &span))
	require.EqualValues(t, n, count)
	require.EqualValues(t, n, distinct, "concurrent records must never share an id")
	require.EqualValues(t, n, span, "ids must be gapless: assigned in commit order with no hole for an uncommitted row")

	rows, err := st.query(ctx, &base, nil, "", "", n+50)
	require.NoError(t, err)
	got := map[string]int{}
	for _, r := range rows {
		got[r.Message]++
	}
	for i := range n {
		require.Equal(t, 1, got[fmt.Sprintf("burst-%d", i)],
			"every burst row must be surfaced exactly once from a held cursor")
	}
}
