package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func decodeProjectResponse(t *testing.T, body string) projectResponse {
	t.Helper()
	var response struct {
		Project projectResponse `json:"project"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &response))
	return response.Project
}

func waitForProjectState(t *testing.T, server *Server, root, state string) projectResponse {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, body := do(t, server, http.MethodGet, "/api/projects", "", authed(compatMachineKey))
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var list struct {
			Projects []projectResponse `json:"projects"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &list))
		for _, project := range list.Projects {
			if project.Root == root && project.State == state {
				return project
			}
			if project.Root == root && project.State == "error" {
				t.Fatalf("project job failed: %s", project.Error)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("project %s did not reach state %s", root, state)
	return projectResponse{}
}

func TestProjectsAddReindexAndDelete(t *testing.T) {
	home := isolateProjectRegistry(t)
	root := filepath.Join(home, "source")
	require.NoError(t, os.MkdirAll(root, 0o755))
	source := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(source, []byte("package project\n\nfunc Ready() bool { return true }\n"), 0o644))

	server := testServer(t, Options{MachineKey: compatMachineKey})
	shutdownServer(t, server)
	headers := authed(compatMachineKey)
	body := `{"root":` + quoteJSONString(t, root) + `,"integrations":[]}`
	resp, raw := do(t, server, http.MethodPost, "/api/projects", body, headers)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, raw)
	accepted := decodeProjectResponse(t, raw)
	require.Equal(t, "indexing", accepted.State)
	require.NotNil(t, accepted.Job)
	require.Equal(t, "index", accepted.Job.Kind)

	ready := waitForProjectState(t, server, root, "ready")
	require.NotNil(t, ready.Status)
	require.Equal(t, "static:potion-code-16M", ready.EmbedModel)
	_, err := os.Stat(filepath.Join(root, "AGENTS.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	require.NoError(t, err)
	require.Contains(t, string(ignore), ".prowl/index.db*")

	resp, raw = do(t, server, http.MethodPost, "/api/projects/reindex", `{"root":`+quoteJSONString(t, root)+`}`, headers)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, raw)
	require.Equal(t, "reindex", decodeProjectResponse(t, raw).Job.Kind)
	waitForProjectState(t, server, root, "ready")

	resp, raw = do(t, server, http.MethodDelete, "/api/projects?root="+urlQuery(root), "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, raw)
	require.JSONEq(t, `{"removed":true}`, raw)
	_, err = os.Stat(source)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, ".prowl"))
	require.NoError(t, err)

	resp, raw = do(t, server, http.MethodGet, "/api/projects", "", headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, raw)
	var list struct {
		Projects []projectResponse `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &list))
	require.Empty(t, list.Projects)
}

func TestProjectsRejectInvalidRoots(t *testing.T) {
	home := isolateProjectRegistry(t)
	server := testServer(t, Options{MachineKey: compatMachineKey})
	shutdownServer(t, server)

	for name, root := range map[string]string{
		"relative": "project",
		"missing":  filepath.Join(home, "missing"),
		"root":     string(filepath.Separator),
		"home":     home,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"root":` + quoteJSONString(t, root) + `,"integrations":[]}`
			resp, raw := do(t, server, http.MethodPost, "/api/projects", body, authed(compatMachineKey))
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, raw)
		})
	}
}

func TestProjectsRejectConcurrentJob(t *testing.T) {
	home := isolateProjectRegistry(t)
	root := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(root, 0o755))

	server := testServer(t, Options{MachineKey: compatMachineKey})
	shutdownServer(t, server)
	_, started := server.startProjectJob(root, "index", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	require.True(t, started)

	body := `{"root":` + quoteJSONString(t, root) + `,"integrations":[]}`
	resp, raw := do(t, server, http.MethodPost, "/api/projects", body, authed(compatMachineKey))
	require.Equal(t, http.StatusConflict, resp.StatusCode, raw)
	require.Equal(t, "job_running", responseErrorCode(t, raw))
}

func quoteJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func urlQuery(value string) string {
	return url.QueryEscape(value)
}
