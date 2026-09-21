package gateway

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

// TestRemovePIDFileIfPIDCannotDeleteReplacementRecord proves the conditional
// removal is atomic against a replacement daemon's write: while the replacement
// holds the per-port pid lock and swaps in its own record, the old owner's
// removal must wait and then observe the new pid, never delete it.
func TestRemovePIDFileIfPIDCannotDeleteReplacementRecord(t *testing.T) {
	dir := t.TempDir()
	const port = 17342
	old := os.Getpid()
	newPID := old + 1

	require.NoError(t, WritePIDFile(dir, port)) // record on disk names the old owner

	// Simulate the replacement daemon holding the per-port pid lock while it
	// installs its own record - the exact window the old owner's conditional
	// removal must not cut into.
	lock := flock.New(PIDFilePath(dir, port) + ".lock")
	require.NoError(t, lock.Lock())

	removed := make(chan error, 1)
	go func() { removed <- RemovePIDFileIfPID(dir, old, port) }()

	// While the replacement holds the lock, the old owner's removal must block.
	select {
	case err := <-removed:
		t.Fatalf("conditional removal ran while a replacement held the pid lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// The replacement installs its record, then hands over the lock.
	blob, err := json.Marshal(PIDRecord{PID: newPID, Port: port})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(PIDFilePath(dir, port), blob, 0o600))
	require.NoError(t, lock.Unlock())

	require.NoError(t, <-removed)

	rec, err := ReadPIDRecord(dir, port)
	require.NoError(t, err)
	require.Equal(t, newPID, rec.PID, "the replacement record must survive the old owner's removal")
	require.Equal(t, port, rec.Port)
}
