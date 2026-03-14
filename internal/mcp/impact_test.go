package mcp

import (
	"encoding/json"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/store"
)

func setupImpactStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// lib.ts is called by auth.ts and main.ts
	// auth.ts is called by routes.ts
	fLib, _ := st.UpsertFile("src/lib.ts", "h1")
	fAuth, _ := st.UpsertFile("src/auth.ts", "h2")
	fMain, _ := st.UpsertFile("src/main.ts", "h3")
	fRoutes, _ := st.UpsertFile("src/routes.ts", "h4")

	st.InsertSymbols(fLib, []graph.Symbol{
		{Name: "hash", Kind: "function", FilePath: "src/lib.ts", StartLine: 1, IsExported: true},
	})

	// auth.ts CALLS lib.ts, main.ts CALLS lib.ts, routes.ts CALLS auth.ts
	st.UpsertEdge(fAuth, fLib, "CALLS")
	st.UpsertEdge(fMain, fLib, "CALLS")
	st.UpsertEdge(fRoutes, fAuth, "CALLS")

	// Communities
	st.InsertCommunity(0, "core", "core")
	st.InsertCommunity(1, "routes", "routes")
	st.InsertCommunityMember(fLib, 0)
	st.InsertCommunityMember(fAuth, 0)
	st.InsertCommunityMember(fMain, 0)
	st.InsertCommunityMember(fRoutes, 1)

	return st
}

func TestProwlImpact(t *testing.T) {
	st := setupImpactStore(t)
	defer st.Close()

	s := New(st, nil, t.TempDir())
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_impact","arguments":{"path":"src/lib.ts"}}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)

	var impact map[string]interface{}
	if err := json.Unmarshal([]byte(text), &impact); err != nil {
		t.Fatalf("not valid JSON: %v\nraw: %s", err, text)
	}

	if impact["target"] != "src/lib.ts" {
		t.Errorf("target = %v, want src/lib.ts", impact["target"])
	}

	directDeps := impact["direct_dependents"].([]interface{})
	if len(directDeps) != 2 {
		t.Errorf("direct_dependents count = %d, want 2 (auth.ts, main.ts)", len(directDeps))
	}

	transitiveDeps := impact["transitive_dependents"].([]interface{})
	if len(transitiveDeps) != 1 {
		t.Errorf("transitive_dependents count = %d, want 1 (routes.ts)", len(transitiveDeps))
	}

	if impact["cross_community"] != true {
		t.Error("expected cross_community = true (routes is in different community)")
	}
}

func TestProwlImpactMissingPath(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_impact","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing path")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}

// Ensure graph and store packages are used.
var _ graph.Symbol
var _ *store.Store
