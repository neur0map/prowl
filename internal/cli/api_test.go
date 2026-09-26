package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// TestAPIServerAnswers reads the HTTP surface the same way a client program
// does: one open project, bearer auth, JSON answers that match the querier.
// It indexes the sample fixture first so the answers are real, not mocked.
func TestAPIServerAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping api index test in -short mode")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "proj")
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("HOME", tmp)

	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}

	srv, err := newAPIServer(context.Background(), []string{root}, "test-token", time.Hour)
	if err != nil {
		t.Fatalf("newAPIServer: %v", err)
	}
	t.Cleanup(srv.close)
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)

	get := func(path, token string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, hs.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		buf, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp, buf
	}

	// /health is ungated liveness.
	if resp, _ := get("/health", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	// Everything else needs the machine token.
	if resp, _ := get("/api/status", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	resp, body := get("/api/find?q=apply", "test-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("find status = %d body=%s", resp.StatusCode, body)
	}
	var hits []struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := json.Unmarshal(body, &hits); err != nil {
		t.Fatalf("find decode: %v: %s", err, body)
	}
	if len(hits) == 0 {
		t.Fatalf("find returned no hits: %s", body)
	}

	// Missing argument: exactly one JSON error body, status 400.
	resp, body = get("/api/find", "test-token")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("find-no-q status = %d", resp.StatusCode)
	}
	var errDoc struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errDoc) != nil || errDoc.Error == "" {
		t.Fatalf("find-no-q body = %s", body)
	}

	// Write verbs are refused: the surface is read-only by design.
	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/find", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}

	// The provider directory answers without touching the index at all.
	resp, body = get("/api/providers", "test-token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("providers status = %d", resp.StatusCode)
	}
	var entries []map[string]any
	if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("providers decode: %v (%d entries)", err, len(entries))
	}
}

// TestAPIRepoRefreshDue pins the TTL gate: the first request always refreshes,
// then only after the TTL ages out.
func TestAPIRepoRefreshDue(t *testing.T) {
	rp := &apiRepo{ttl: 20 * time.Second}
	now := time.Now()
	if !rp.refreshDue(now) {
		t.Fatal("a never-served repo must refresh")
	}
	rp.last = now
	if rp.refreshDue(now.Add(time.Second)) {
		t.Fatal("inside the TTL must not refresh")
	}
	if !rp.refreshDue(now.Add(21 * time.Second)) {
		t.Fatal("past the TTL must refresh")
	}
}
