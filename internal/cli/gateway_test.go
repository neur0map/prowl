package cli

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

func TestGatewayInjectReturnsFailureWhenRequestedTargetFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	cmd := newGatewayCmd("test", false)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"inject", "not-a-harness"})

	err := cmd.Execute()

	require.ErrorContains(t, err, "gateway injection failed for 1 of 1")
	require.Contains(t, stderr.String(), `no provider config writer for "not-a-harness"`)
	require.NotContains(t, stdout.String(), "start the gateway",
		"a failed request must not print success guidance")
}

// TestGatewayServeRejectsSpoofedOccupiedPort proves the foreground serve probes
// an occupied port with the machine-local token: a hostile local listener that
// answers the liveness check "ok" but cannot prove knowledge of the token is not
// mistaken for a running gateway, so serve surfaces the bind failure instead of
// silently backing off from a port it should own.
func TestGatewayServeRejectsSpoofedOccupiedPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	spoof := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "ok" with no valid proof - exactly what an impostor can forge.
		_, _ = fmt.Fprintf(w, `{"status":"ok","pid":%d}`, os.Getpid())
	}))
	t.Cleanup(spoof.Close)
	parsed, err := url.Parse(spoof.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	cmd := newGatewayCmd("test", false)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"serve", "--port", strconv.Itoa(port)})

	err = cmd.Execute()
	require.Error(t, err, "a spoofed occupied port must not be treated as an already-running gateway")
	require.ErrorContains(t, err, "cannot bind")
	require.NotContains(t, stdout.String(), "already running",
		"an unauthenticated listener must never be reported as a running gateway")
}

// TestGatewayServeFailsWhenPIDRecordCannotBeWritten proves an unrecordable pid
// is a startup failure, not a warning: serve refuses to run an unmanageable
// daemon and releases the port, so no orphaned listener is left behind.
func TestGatewayServeFailsWhenPIDRecordCannotBeWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	// A free loopback port for the serve attempt to bind.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := probe.Addr().(*net.TCPAddr).Port
	require.NoError(t, probe.Close())

	// Force the pid-record write to fail by putting a directory where the pid
	// file must be written, so os.WriteFile cannot create it.
	dir := gateway.Dir()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Mkdir(gateway.PIDFilePath(dir, port), 0o700))

	cmd := newGatewayCmd("test", false)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"serve", "--port", strconv.Itoa(port)})

	err = cmd.Execute()
	require.ErrorContains(t, err, "record gateway pid",
		"an unrecordable pid must fail startup, not serve an unmanageable daemon")
	require.NotContains(t, stdout.String(), "listening",
		"startup must not announce a listener it then abandons")

	// The listener must have been released: the port is bindable again.
	reclaim, err := gateway.ListenLoopback(port)
	require.NoError(t, err, "a pid-record failure must leave no listener bound")
	require.NoError(t, reclaim.Close())
}
