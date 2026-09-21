package gateway

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteLogsToFileKeepsDiagnosticsOffTerminalLogger(t *testing.T) {
	previous := slog.Default()
	path, restore, err := RouteLogsToFile(t.TempDir())
	require.NoError(t, err)
	slog.Warn("credential refresh failed", "provider", "hyper")
	restore()
	require.Same(t, previous, slog.Default())

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(content), "credential refresh failed"))
	require.True(t, strings.Contains(string(content), "provider=hyper"))
}
