package mcp

import (
	"sort"
	"testing"
)

func TestScopeCommunityBoost(t *testing.T) {
	s := New(nil, nil, "")

	// Simulate fileEntry ranking: search hits first, then expanded files
	// ranked by communityBonus + hops, with heat as tiebreaker.
	type fileEntry struct {
		path           string
		reason         string
		score          float64
		hops           int
		communityBonus int
	}

	entries := []*fileEntry{
		{path: "a.go", reason: "1-hop:called_by:x", hops: 1, communityBonus: 0},
		{path: "b.go", reason: "1-hop:called_by:x", hops: 1, communityBonus: 1},
		{path: "c.go", reason: "search_hit", score: 0.8},
		{path: "d.go", reason: "1-hop:imports:x", hops: 2, communityBonus: 0},
	}

	// Pre-record heat for a.go to test heat tiebreaker
	s.recordAccess("a.go")

	sort.Slice(entries, func(i, j int) bool {
		iHit := entries[i].reason == "search_hit"
		jHit := entries[j].reason == "search_hit"
		if iHit != jHit {
			return iHit
		}
		if iHit {
			iScore := 0.85*entries[i].score + 0.15*s.heatScore(entries[i].path)
			jScore := 0.85*entries[j].score + 0.15*s.heatScore(entries[j].path)
			return iScore > jScore
		}
		iRank := entries[i].communityBonus + entries[i].hops
		jRank := entries[j].communityBonus + entries[j].hops
		if iRank != jRank {
			return iRank > jRank
		}
		return s.heatScore(entries[i].path) > s.heatScore(entries[j].path)
	})

	// Search hits come first
	if entries[0].path != "c.go" {
		t.Errorf("entries[0] = %s, want c.go (search_hit)", entries[0].path)
	}

	// b.go has communityBonus=1 + hops=1 = 2, d.go has 0+2 = 2, a.go has 0+1 = 1
	// b.go and d.go tie at rank=2; d.go has no heat, b.go has no heat → stable order
	// a.go should be last among expanded (rank=1)
	if entries[1].path != "d.go" && entries[1].path != "b.go" {
		t.Errorf("entries[1] = %s, want b.go or d.go (rank=2)", entries[1].path)
	}

	// a.go should be last (rank=1, lowest)
	if entries[3].path != "a.go" {
		t.Errorf("entries[3] = %s, want a.go (lowest rank)", entries[3].path)
	}
}

func TestProwlScopeWithoutEmbedder(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()

	s := New(st, nil, t.TempDir())
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_scope","arguments":{"task":"fix auth"}}}`)

	if resp.Error == nil {
		t.Fatal("expected error when embedder is nil")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603", resp.Error.Code)
	}
}

func TestProwlScopeMissingTask(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_scope","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing task")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}
