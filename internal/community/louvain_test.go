package community

import (
	"sort"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

// helper to build a graph with files, symbols, and edges.
func buildTestGraph(files []string, symbols map[string][]graph.Symbol, edges []graph.Edge) *graph.Graph {
	g := graph.New()
	for _, f := range files {
		g.AddFile(graph.FileRecord{Path: f, Hash: "h"})
	}
	for _, syms := range symbols {
		for _, s := range syms {
			g.AddSymbol(s)
		}
	}
	for _, e := range edges {
		g.AddEdge(e)
	}
	return g
}

func TestDetectCommunitiesBasic(t *testing.T) {
	// Two clear clusters:
	// Cluster 1: auth files calling each other
	// Cluster 2: db files calling each other
	// No cross-cluster calls
	files := []string{
		"src/auth/login.ts",
		"src/auth/session.ts",
		"src/auth/token.ts",
		"src/db/query.ts",
		"src/db/connection.ts",
		"src/db/migrate.ts",
	}
	symbols := map[string][]graph.Symbol{
		"src/auth/login.ts":      {{Name: "login", Kind: "function", FilePath: "src/auth/login.ts"}},
		"src/auth/session.ts":    {{Name: "createSession", Kind: "function", FilePath: "src/auth/session.ts"}},
		"src/auth/token.ts":      {{Name: "verifyToken", Kind: "function", FilePath: "src/auth/token.ts"}},
		"src/db/query.ts":        {{Name: "runQuery", Kind: "function", FilePath: "src/db/query.ts"}},
		"src/db/connection.ts":   {{Name: "connect", Kind: "function", FilePath: "src/db/connection.ts"}},
		"src/db/migrate.ts":      {{Name: "migrate", Kind: "function", FilePath: "src/db/migrate.ts"}},
	}
	edges := []graph.Edge{
		// Auth cluster: fully connected
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/session.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/session.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		// DB cluster: fully connected
		{SourcePath: "src/db/query.ts", TargetPath: "src/db/connection.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/db/query.ts", TargetPath: "src/db/migrate.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/db/connection.ts", TargetPath: "src/db/migrate.ts", Type: "CALLS", Confidence: 0.9},
	}

	g := buildTestGraph(files, symbols, edges)
	communities := DetectCommunities(g)

	if len(communities) != 2 {
		t.Fatalf("expected 2 communities, got %d: %+v", len(communities), communities)
	}

	// Verify each community has 3 members
	for _, c := range communities {
		if len(c.Members) != 3 {
			t.Errorf("community %q has %d members, expected 3: %v", c.Label, len(c.Members), c.Members)
		}
	}

	// Verify auth files are together and db files are together
	authComm := -1
	dbComm := -1
	for _, c := range communities {
		for _, m := range c.Members {
			if m == "src/auth/login.ts" {
				authComm = c.ID
			}
			if m == "src/db/query.ts" {
				dbComm = c.ID
			}
		}
	}
	if authComm == dbComm {
		t.Error("auth and db files should be in different communities")
	}
	if authComm == -1 || dbComm == -1 {
		t.Error("expected both auth and db communities to be found")
	}
}

func TestSingletonsSkipped(t *testing.T) {
	// Three files: two connected, one isolated
	files := []string{
		"src/auth/login.ts",
		"src/auth/session.ts",
		"src/utils/helper.ts",
	}
	symbols := map[string][]graph.Symbol{
		"src/auth/login.ts":   {{Name: "login", Kind: "function", FilePath: "src/auth/login.ts"}},
		"src/auth/session.ts": {{Name: "session", Kind: "function", FilePath: "src/auth/session.ts"}},
		"src/utils/helper.ts": {{Name: "help", Kind: "function", FilePath: "src/utils/helper.ts"}},
	}
	edges := []graph.Edge{
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/session.ts", Type: "CALLS", Confidence: 0.9},
	}

	g := buildTestGraph(files, symbols, edges)
	communities := DetectCommunities(g)

	// Should have 1 community (the auth pair), singleton helper.ts skipped
	if len(communities) != 1 {
		t.Fatalf("expected 1 community, got %d: %+v", len(communities), communities)
	}

	// Verify helper.ts is not in any community
	for _, c := range communities {
		for _, m := range c.Members {
			if m == "src/utils/helper.ts" {
				t.Error("singleton file helper.ts should not be in any community")
			}
		}
	}

	// Verify auth files are together
	members := communities[0].Members
	sort.Strings(members)
	if len(members) != 2 || members[0] != "src/auth/login.ts" || members[1] != "src/auth/session.ts" {
		t.Errorf("expected auth files in community, got %v", members)
	}
}

func TestCommunityLabeling(t *testing.T) {
	// Create a community where most files are under src/auth/
	files := []string{
		"src/auth/login.ts",
		"src/auth/session.ts",
		"src/auth/token.ts",
		"src/auth/middleware.ts",
		"src/utils/crypto.ts",
	}
	symbols := map[string][]graph.Symbol{
		"src/auth/login.ts":      {{Name: "login", Kind: "function", FilePath: "src/auth/login.ts"}},
		"src/auth/session.ts":    {{Name: "session", Kind: "function", FilePath: "src/auth/session.ts"}},
		"src/auth/token.ts":      {{Name: "token", Kind: "function", FilePath: "src/auth/token.ts"}},
		"src/auth/middleware.ts": {{Name: "middleware", Kind: "function", FilePath: "src/auth/middleware.ts"}},
		"src/utils/crypto.ts":   {{Name: "encrypt", Kind: "function", FilePath: "src/utils/crypto.ts"}},
	}
	// All connected in one big cluster
	edges := []graph.Edge{
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/session.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/session.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/middleware.ts", TargetPath: "src/auth/session.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/middleware.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/utils/crypto.ts", TargetPath: "src/auth/token.ts", Type: "CALLS", Confidence: 0.9},
		{SourcePath: "src/auth/login.ts", TargetPath: "src/utils/crypto.ts", Type: "CALLS", Confidence: 0.9},
	}

	g := buildTestGraph(files, symbols, edges)
	communities := DetectCommunities(g)

	if len(communities) == 0 {
		t.Fatal("expected at least 1 community")
	}

	// Find the community containing auth files
	found := false
	for _, c := range communities {
		hasAuth := false
		for _, m := range c.Members {
			if m == "src/auth/login.ts" {
				hasAuth = true
				break
			}
		}
		if hasAuth {
			if c.Label != "auth" {
				t.Errorf("expected label 'auth', got %q", c.Label)
			}
			found = true
		}
	}
	if !found {
		t.Error("expected to find a community containing auth files")
	}
}

func TestEmptyGraph(t *testing.T) {
	g := graph.New()
	communities := DetectCommunities(g)
	if len(communities) != 0 {
		t.Errorf("expected 0 communities for empty graph, got %d", len(communities))
	}
}

func TestFilesWithOnlyConstsSkipped(t *testing.T) {
	// Files with only consts/types/enums should not be included
	files := []string{
		"src/auth/login.ts",
		"src/auth/session.ts",
		"src/constants.ts",
	}
	symbols := map[string][]graph.Symbol{
		"src/auth/login.ts":   {{Name: "login", Kind: "function", FilePath: "src/auth/login.ts"}},
		"src/auth/session.ts": {{Name: "session", Kind: "function", FilePath: "src/auth/session.ts"}},
		"src/constants.ts":    {{Name: "MAX_RETRIES", Kind: "const", FilePath: "src/constants.ts"}},
	}
	edges := []graph.Edge{
		{SourcePath: "src/auth/login.ts", TargetPath: "src/auth/session.ts", Type: "CALLS", Confidence: 0.9},
		// Even though constants.ts has a CALLS edge, it shouldn't be included
		// because it only has const symbols
		{SourcePath: "src/constants.ts", TargetPath: "src/auth/login.ts", Type: "CALLS", Confidence: 0.5},
	}

	g := buildTestGraph(files, symbols, edges)
	communities := DetectCommunities(g)

	// Verify constants.ts is not in any community
	for _, c := range communities {
		for _, m := range c.Members {
			if m == "src/constants.ts" {
				t.Error("file with only const symbols should not be in any community")
			}
		}
	}
}

func TestExtendsAndImplementsEdges(t *testing.T) {
	// Verify that EXTENDS and IMPLEMENTS edges are considered
	files := []string{
		"src/models/base.ts",
		"src/models/user.ts",
		"src/models/admin.ts",
	}
	symbols := map[string][]graph.Symbol{
		"src/models/base.ts":  {{Name: "BaseModel", Kind: "class", FilePath: "src/models/base.ts"}},
		"src/models/user.ts":  {{Name: "User", Kind: "class", FilePath: "src/models/user.ts"}},
		"src/models/admin.ts": {{Name: "Admin", Kind: "class", FilePath: "src/models/admin.ts"}},
	}
	edges := []graph.Edge{
		{SourcePath: "src/models/user.ts", TargetPath: "src/models/base.ts", Type: "EXTENDS", Confidence: 1.0},
		{SourcePath: "src/models/admin.ts", TargetPath: "src/models/base.ts", Type: "EXTENDS", Confidence: 1.0},
		{SourcePath: "src/models/admin.ts", TargetPath: "src/models/user.ts", Type: "IMPLEMENTS", Confidence: 0.8},
	}

	g := buildTestGraph(files, symbols, edges)
	communities := DetectCommunities(g)

	if len(communities) != 1 {
		t.Fatalf("expected 1 community from EXTENDS/IMPLEMENTS edges, got %d", len(communities))
	}

	if len(communities[0].Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(communities[0].Members))
	}
}

func TestLabelCommunityFallback(t *testing.T) {
	// Test the fallback labeling when directory segments are all generic
	members := []string{"src/a.ts", "src/b.ts"}
	label := labelCommunity(members, 42)
	// Both files are directly in src/ which is skipped, so should fall back
	if label != "cluster-42" {
		t.Errorf("expected fallback label 'cluster-42', got %q", label)
	}
}

func TestLabelCommunitySharedPrefix(t *testing.T) {
	// Test shared prefix fallback
	members := []string{"handler_auth.ts", "handler_session.ts"}
	label := labelCommunity(members, 0)
	// Directory is "." which is skipped, so should try shared prefix
	if label != "handler_" {
		t.Errorf("expected shared prefix label 'handler_', got %q", label)
	}
}
