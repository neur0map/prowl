package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway/inject"
)

func setupRouteHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("PATH", "")
	return home
}

func TestSetupHarnessesReportsEmptyCatalogueAsUnroutable(t *testing.T) {
	setupRouteHome(t)
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: "local-token"})
	resp, body := do(t, s, http.MethodGet, "/api/setup/harnesses", "", authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload setupHarnessesResponse
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.False(t, payload.Routable)
	require.NotEmpty(t, payload.Reason)
	require.Len(t, payload.Harnesses, 8)
	for _, row := range payload.Harnesses {
		require.NotNil(t, row.Files)
		require.NotEmpty(t, row.Skills)
	}
}

func TestSetupHarnessActivationRequiresRoutableChainUnlessForced(t *testing.T) {
	home := setupRouteHome(t)
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: "local-token"})
	path := filepath.Join(home, ".config", "opencode", "opencode.json")

	resp, body := do(t, s, http.MethodPost, "/api/setup/harnesses/opencode", `{"activate":true}`, authed(compatMachineKey))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Contains(t, body, `"code":"not_routable"`)
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)

	resp, body = do(t, s, http.MethodPost, "/api/setup/harnesses/opencode", `{"activate":true,"force":true}`, authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.FileExists(t, path)
	require.Contains(t, body, `"active":true`)
	require.Contains(t, string(requireReadFile(t, path)), `prowl/auto`)

	resp, body = do(t, s, http.MethodDelete, "/api/setup/harnesses/opencode", "", authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoFileExists(t, path)
}

func TestSetupHarnessesReportsUnrefreshableStaleCredential(t *testing.T) {
	home := setupRouteHome(t)
	const localToken = "machine-local-token"
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: localToken})
	currentKey, err := s.UnifiedAPIKey(t.Context())
	require.NoError(t, err)
	oldKey := "prowlag-" + strings.Repeat("7", 48)
	if oldKey == currentKey {
		oldKey = "prowlag-" + strings.Repeat("6", 48)
	}

	staleOpts := inject.Options{
		Home:     home,
		BaseURL:  "http://127.0.0.1:8788/v1",
		Token:    oldKey,
		Activate: true,
		Models:   inject.RoutingModels(),
	}
	_, err = inject.Apply(staleOpts, "hermes")
	require.NoError(t, err)
	hermesPath := filepath.Join(home, ".hermes", "config.yaml")
	injected := string(requireReadFile(t, hermesPath))
	edited := strings.Replace(injected, "    discover_models: false\n",
		"    discover_models: false\n    request_timeout: 90\n", 1)
	require.NotEqual(t, injected, edited)
	require.NoError(t, os.WriteFile(hermesPath, []byte(edited), 0o600))

	freshOpts := inject.Options{
		Home:    home,
		BaseURL: "http://127.0.0.1:8788/v1",
		Token:   currentKey,
		Models:  inject.RoutingModels(),
	}
	_, err = inject.Apply(freshOpts, "omp")
	require.NoError(t, err)

	resp, body := do(t, s, http.MethodGet, "/api/setup/harnesses", "", authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var payload setupHarnessesResponse
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	rows := make(map[string]harnessRow, len(payload.Harnesses))
	for _, row := range payload.Harnesses {
		rows[row.ID] = row
	}

	require.True(t, rows["hermes"].CredentialStale)
	require.False(t, rows["hermes"].Active)
	require.Contains(t, rows["hermes"].Note, "no longer works")
	require.Contains(t, rows["hermes"].Note, "Reconnecting refreshes it")
	require.False(t, rows["omp"].CredentialStale)
	require.Equal(t, edited, string(requireReadFile(t, hermesPath)),
		"status inspection must not rewrite a diverged target")
}

func TestSetupHarnessUnknownIDIsNotFound(t *testing.T) {
	setupRouteHome(t)
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: "local-token"})
	resp, body := do(t, s, http.MethodPost, "/api/setup/harnesses/not-a-harness", `{}`, authed(compatMachineKey))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, body, `"code":"harness_not_found"`)
}

func TestSetupSkillsReturnsActionCounts(t *testing.T) {
	setupRouteHome(t)
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: "local-token"})
	resp, body := do(t, s, http.MethodPost, "/api/setup/skills", `{}`, authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var payload skillsSetupResponse
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.Zero(t, payload.Installed)
	require.Zero(t, payload.Updated)
	require.Zero(t, payload.Unchanged)
	require.Zero(t, payload.Conflicts)
	require.NotEmpty(t, payload.Message)
}

func requireReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}
