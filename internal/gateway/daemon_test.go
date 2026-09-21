package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPIDRecordCarriesPortAndReadsLegacyRecords(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WritePIDFile(dir, 17342))

	record, err := ReadPIDRecord(dir, 17342)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), record.PID)
	require.Equal(t, 17342, record.Port)

	require.NoError(t, os.WriteFile(PIDFilePath(dir), []byte("1234"), 0o600))
	record, err = ReadPIDRecord(dir)
	require.NoError(t, err)
	require.Equal(t, PIDRecord{PID: 1234}, record)
}

func TestPIDRecordPortCompatibility(t *testing.T) {
	t.Run("explicit requested port", func(t *testing.T) {
		require.True(t, pidRecordMatchesPort(PIDRecord{PID: 1234, Port: 17342}, 17342))
		require.False(t, pidRecordMatchesPort(PIDRecord{PID: 1234, Port: 17342}, 17343))
	})
	t.Run("legacy default port", func(t *testing.T) {
		legacy := PIDRecord{PID: 1234}
		require.True(t, pidRecordMatchesPort(legacy, DefaultPort))
		require.False(t, pidRecordMatchesPort(legacy, 17342))
	})
}

func TestStopTrackedDaemonRefusesUnverifiableLivePID(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(PIDFilePath(dir), []byte(strconv.Itoa(os.Getpid())), 0o600))

	_, err := stopTrackedDaemon(context.Background(), dir, DefaultPort)
	require.ErrorContains(t, err, "refusing to signal")
	require.True(t, ProcessAlive(os.Getpid()), "the caller must not signal an unverified reused pid")
	require.FileExists(t, PIDFilePath(dir), "an unverifiable live record is preserved for diagnosis or retry")
}

func TestStopTrackedDaemonRejectsPIDMismatchAndCleansOnlyStaleRecord(t *testing.T) {
	dir := t.TempDir()
	token, err := EnsureToken(dir)
	require.NoError(t, err)
	var port int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		proof := PingProof(token, port, r.Header.Get(PingChallengeHeader))
		_, _ = fmt.Fprintf(w, `{"status":"ok","pid":%d,"proof":%q}`, os.Getpid()+1, proof)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err = strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	require.NoError(t, WritePIDFile(dir, port))
	require.False(t, trackedDaemonRunning(context.Background(), dir, port))

	result, err := stopTrackedDaemon(context.Background(), dir, port)
	require.NoError(t, err)
	require.Equal(t, DaemonStale, result.State)
	require.Equal(t, os.Getpid(), result.PID)
	require.True(t, ProcessAlive(os.Getpid()), "a mismatched health identity must not be signalled")
	_, err = os.Stat(PIDFilePath(dir, port))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStopTrackedDaemonRefusesSpoofedUnauthenticatedPing(t *testing.T) {
	dir := t.TempDir()
	token, err := EnsureToken(dir)
	require.NoError(t, err)
	seenHeaders := make(chan http.Header, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","pid":%d}`, os.Getpid())
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	require.NoError(t, WritePIDFile(dir, port))
	require.False(t, trackedDaemonRunning(context.Background(), dir, port))
	_, err = stopTrackedDaemon(context.Background(), dir, port)
	require.ErrorContains(t, err, "did not prove its identity")
	require.True(t, ProcessAlive(os.Getpid()), "an unauthenticated listener must never trigger a signal")
	require.FileExists(t, PIDFilePath(dir, port), "an unverifiable record stays available for diagnosis")
	require.NotEmpty(t, seenHeaders)
	for len(seenHeaders) > 0 {
		headers := <-seenHeaders
		require.Empty(t, headers.Get(tokenHeader), "the bootstrap token must never be sent to an unverified listener")
		require.Empty(t, headers.Get("Authorization"))
		require.NotContains(t, fmt.Sprint(headers), token)
		require.NotEmpty(t, headers.Get(PingChallengeHeader))
	}
}

func TestPIDCleanupDoesNotRemoveReplacementRecord(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WritePIDFile(dir, 17342))
	originalPID := os.Getpid()
	replacement := fmt.Sprintf(`{"pid":%d,"port":17342}`, originalPID+1)
	require.NoError(t, os.WriteFile(PIDFilePath(dir, 17342), []byte(replacement), 0o600))

	require.NoError(t, RemovePIDFileIfPID(dir, originalPID, 17342))
	record, err := ReadPIDRecord(dir, 17342)
	require.NoError(t, err)
	require.Equal(t, originalPID+1, record.PID)
	require.Equal(t, 17342, record.Port)
}

func TestCustomPortsKeepIndependentPIDRecords(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WritePIDFile(dir, 17342))
	require.NoError(t, WritePIDFile(dir, 17343))

	left, err := ReadPIDRecord(dir, 17342)
	require.NoError(t, err)
	right, err := ReadPIDRecord(dir, 17343)
	require.NoError(t, err)
	require.Equal(t, 17342, left.Port)
	require.Equal(t, 17343, right.Port)
	require.NotEqual(t, PIDFilePath(dir, 17342), PIDFilePath(dir, 17343))

	require.NoError(t, RemovePIDFileIfPID(dir, os.Getpid(), 17342))
	left, err = ReadPIDRecord(dir, 17342)
	require.NoError(t, err)
	require.Zero(t, left.PID)
	right, err = ReadPIDRecord(dir, 17343)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), right.PID)
}
