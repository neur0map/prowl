package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/store"
)

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fid1, _ := st.UpsertFile("src/auth.ts", "hash1")
	fid2, _ := st.UpsertFile("src/db.ts", "hash2")
	st.InsertSymbols(fid1, []graph.Symbol{
		{Name: "handleLogin", Kind: "function", FilePath: "src/auth.ts", StartLine: 5, IsExported: true, Signature: "export function handleLogin()"},
	})
	st.InsertSymbols(fid2, []graph.Symbol{
		{Name: "query", Kind: "function", FilePath: "src/db.ts", StartLine: 3, IsExported: true, Signature: "export function query()"},
	})
	st.UpsertEdge(fid1, fid2, "IMPORTS")
	st.UpsertEmbedding(fid1, []float32{0.1, 0.2}, "h1")
	st.InsertCommunity(0, "auth", "auth")
	st.InsertCommunityMember(fid1, 0)
	st.InsertCommunityMember(fid2, 0)
	return st
}

func setupTestContext(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "src", "auth.ts")
	os.MkdirAll(authDir, 0o755)
	os.WriteFile(filepath.Join(authDir, ".exports"), []byte("func handleLogin (line 5)\n"), 0o644)
	os.WriteFile(filepath.Join(authDir, ".signatures"), []byte("export function handleLogin() (line 5)\n"), 0o644)
	os.WriteFile(filepath.Join(authDir, ".imports"), []byte("src/db.ts\n"), 0o644)
	os.WriteFile(filepath.Join(authDir, ".calls"), []byte("src/db.ts\n"), 0o644)
	os.WriteFile(filepath.Join(authDir, ".callers"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(authDir, ".upstream"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(authDir, ".community"), []byte("auth\n"), 0o644)
	dbDir := filepath.Join(tmpDir, "src", "db.ts")
	os.MkdirAll(dbDir, 0o755)
	os.WriteFile(filepath.Join(dbDir, ".exports"), []byte("func query (line 3)\n"), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".signatures"), []byte("export function query() (line 3)\n"), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".imports"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".calls"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".callers"), []byte("src/auth.ts\n"), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".upstream"), []byte("src/auth.ts\n"), 0o644)
	os.WriteFile(filepath.Join(dbDir, ".community"), []byte("auth\n"), 0o644)
	metaDir := filepath.Join(tmpDir, "_meta")
	os.MkdirAll(metaDir, 0o755)
	os.WriteFile(filepath.Join(metaDir, "communities.txt"), []byte("community: auth (id=0)\nmembers:\n  src/auth.ts\n  src/db.ts\n"), 0o644)
	os.WriteFile(filepath.Join(metaDir, "processes.txt"), []byte("process: main [entry_chain]\nentry: src/auth.ts\nsteps:\n  -> src/auth.ts\n  -> src/db.ts\n"), 0o644)
	return tmpDir
}

func TestProwlOverview(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()
	contextDir := setupTestContext(t)
	s := New(st, nil, contextDir, "dev")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_overview","arguments":{}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	var overview map[string]interface{}
	if err := json.Unmarshal([]byte(text), &overview); err != nil {
		t.Fatalf("overview is not valid JSON: %v\nraw: %s", err, text)
	}
	if int(overview["files"].(float64)) != 2 {
		t.Errorf("files = %v, want 2", overview["files"])
	}
	if int(overview["symbols"].(float64)) != 2 {
		t.Errorf("symbols = %v, want 2", overview["symbols"])
	}
	if int(overview["edges"].(float64)) != 1 {
		t.Errorf("edges = %v, want 1", overview["edges"])
	}
	if int(overview["embeddings"].(float64)) != 1 {
		t.Errorf("embeddings = %v, want 1", overview["embeddings"])
	}
	communities, ok := overview["communities"].([]interface{})
	if !ok || len(communities) == 0 {
		t.Fatal("expected communities array")
	}
	// Verify community members use glance digest format
	comm := communities[0].(map[string]interface{})
	members := comm["members"].([]interface{})
	if len(members) == 0 {
		t.Fatal("expected community members")
	}
	firstMember := members[0].(string)
	if !strings.Contains(firstMember, " | ") || !strings.Contains(firstMember, "exports") {
		t.Errorf("expected digest format, got %q", firstMember)
	}
}

func TestProwlFileContext(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()
	contextDir := setupTestContext(t)

	s := New(st, nil, contextDir, "dev")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_file_context","arguments":{"path":"src/auth.ts"}}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)

	var fc FileContext
	if err := json.Unmarshal([]byte(text), &fc); err != nil {
		t.Fatalf("response is not valid JSON: %v\nraw: %s", err, text)
	}

	if fc.Path != "src/auth.ts" {
		t.Errorf("path = %q, want src/auth.ts", fc.Path)
	}
	if fc.Community != "auth" {
		t.Errorf("community = %q, want auth", fc.Community)
	}
	if len(fc.Exports) != 1 {
		t.Errorf("exports = %d, want 1", len(fc.Exports))
	}
	if len(fc.Calls) != 1 {
		t.Errorf("calls = %d, want 1", len(fc.Calls))
	}
}

func TestProwlFileContextMissing(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()

	s := New(st, nil, t.TempDir(), "dev")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_file_context","arguments":{"path":"nonexistent.ts"}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestProwlFileContextMissingPath(t *testing.T) {
	s := New(nil, nil, "", "dev")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_file_context","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing path parameter")
	}
}
