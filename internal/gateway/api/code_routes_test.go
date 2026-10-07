package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/application"
)

func isolateProjectRegistry(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	return home
}

func indexedCodeFixture(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package sample\n\nfunc ApplyConfig() string { return \"ready\" }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "worker.go"), []byte("package sample\n\nfunc RunWorker() { ApplyConfig() }\n"), 0o644))
	_, err := application.InitializeProject(context.Background(), application.InitOptions{
		Root:               root,
		IntegrationsSet:    true,
		InferencerProvider: application.DefaultInferencer,
	})
	require.NoError(t, err)
}

func shutdownServer(t *testing.T, server *Server) {
	t.Helper()
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
}

func responseErrorCode(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	return envelope.Error.Code
}

func TestCodeRoutesResolveRegisteredProjects(t *testing.T) {
	home := isolateProjectRegistry(t)
	first := filepath.Join(home, "one", "shared")
	second := filepath.Join(home, "two", "shared")
	unique := filepath.Join(home, "unique")
	indexedCodeFixture(t, first)
	indexedCodeFixture(t, second)
	indexedCodeFixture(t, unique)

	server := testServer(t, Options{MachineKey: compatMachineKey})
	shutdownServer(t, server)
	headers := authed(compatMachineKey)

	resp, body := do(t, server, http.MethodGet, "/api/code/repos", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var repos struct {
		Repos []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"repos"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &repos))
	require.Len(t, repos.Repos, 3)

	resp, body = do(t, server, http.MethodGet, "/api/code/status", "", headers)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "repo_required", responseErrorCode(t, body))

	resp, body = do(t, server, http.MethodGet, "/api/code/status?repo=shared", "", headers)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "repo_required", responseErrorCode(t, body))

	resp, body = do(t, server, http.MethodGet, "/api/code/status?repo=missing", "", headers)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "repo_not_found", responseErrorCode(t, body))

	resp, body = do(t, server, http.MethodGet, "/api/code/status?repo=unique", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, body = do(t, server, http.MethodGet, "/api/code/status?repo="+url.QueryEscape(first), "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	resp, _ = do(t, server, http.MethodPost, "/api/code/status?repo="+url.QueryEscape(first), "{}", headers)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestCodeRoutesDefaultSingleProjectAndAnswerQuery(t *testing.T) {
	home := isolateProjectRegistry(t)
	indexedCodeFixture(t, filepath.Join(home, "project"))

	server := testServer(t, Options{MachineKey: compatMachineKey})
	shutdownServer(t, server)
	resp, body := do(t, server, http.MethodGet, "/api/code/find?q=ApplyConfig", "", authed(compatMachineKey))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	var hits []struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &hits))
	require.NotEmpty(t, hits)
	require.Contains(t, hits, struct {
		Name string `json:"name"`
		File string `json:"file"`
	}{Name: "ApplyConfig", File: "main.go"})
}
