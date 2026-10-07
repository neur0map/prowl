package service

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/inject"
)

func TestServeRefreshesStaleHarnessCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	svc, err := Open(t.Context(), Options{Dir: filepath.Join(home, "gateway-state"), SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, svc.Close()) })
	oldKey, err := svc.UnifiedAPIKey(t.Context())
	require.NoError(t, err)

	const baseURL = "http://127.0.0.1:9832/v1"
	_, err = inject.Apply(inject.Options{
		Home:    home,
		BaseURL: baseURL,
		Token:   oldKey,
		Models:  inject.RoutingModels(),
	}, "hermes")
	require.NoError(t, err)

	newKey := "prowlag-" + strings.Repeat("9", 48)
	if newKey == oldKey {
		newKey = "prowlag-" + strings.Repeat("8", 48)
	}
	_, err = svc.Engine().DB().ExecContext(t.Context(),
		"UPDATE settings SET value = ? WHERE key = ?", newKey, "unified_api_key")
	require.NoError(t, err)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- svc.Serve(ctx, listener) }()

	path := filepath.Join(home, ".hermes", "config.yaml")
	require.Eventually(t, func() bool {
		content, readErr := os.ReadFile(path)
		return readErr == nil && strings.Contains(string(content), newKey)
	}, 2*time.Second, 10*time.Millisecond)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(content), oldKey)
	require.Contains(t, string(content), baseURL)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop after cancellation")
	}
}
