package store

import (
	"math"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Insert a file
	fid, err := s.UpsertFile("src/auth.ts", "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if fid == 0 {
		t.Fatal("expected non-zero file ID")
	}

	// Insert symbols
	err = s.InsertSymbols(fid, []graph.Symbol{
		{Name: "login", Kind: "function", StartLine: 10, EndLine: 20, IsExported: true, Signature: "func login()"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Insert another file and an edge
	fid2, _ := s.UpsertFile("src/login.ts", "hash2")
	err = s.UpsertEdge(fid2, fid, "IMPORTS")
	if err != nil {
		t.Fatal(err)
	}

	// Query upstream
	upstream, err := s.UpstreamOf("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(upstream) != 1 || upstream[0] != "src/login.ts" {
		t.Errorf("expected upstream [src/login.ts], got %v", upstream)
	}

	// Query file hash
	hash, err := s.FileHash("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash1" {
		t.Errorf("expected hash 'hash1', got %q", hash)
	}
}

func TestUpsertEdgeWithConfidence(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid1, _ := s.UpsertFile("src/a.ts", "h1")
	fid2, _ := s.UpsertFile("src/b.ts", "h2")

	// Insert a CALLS edge with 0.9 confidence
	if err := s.UpsertEdgeWithConfidence(fid1, fid2, "CALLS", 0.9); err != nil {
		t.Fatal(err)
	}

	// Verify the edge is stored with correct confidence
	var confidence float64
	err = s.db.QueryRow(
		"SELECT confidence FROM edges WHERE source_file_id = ? AND target_file_id = ? AND type = ?",
		fid1, fid2, "CALLS",
	).Scan(&confidence)
	if err != nil {
		t.Fatal(err)
	}
	if confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", confidence)
	}

	// Upsert the same edge with different confidence — should update
	if err := s.UpsertEdgeWithConfidence(fid1, fid2, "CALLS", 0.75); err != nil {
		t.Fatal(err)
	}
	err = s.db.QueryRow(
		"SELECT confidence FROM edges WHERE source_file_id = ? AND target_file_id = ? AND type = ?",
		fid1, fid2, "CALLS",
	).Scan(&confidence)
	if err != nil {
		t.Fatal(err)
	}
	if confidence != 0.75 {
		t.Errorf("expected updated confidence 0.75, got %f", confidence)
	}

	// Verify default UpsertEdge sets confidence to 1.0
	if err := s.UpsertEdge(fid1, fid2, "IMPORTS"); err != nil {
		t.Fatal(err)
	}
	err = s.db.QueryRow(
		"SELECT confidence FROM edges WHERE source_file_id = ? AND target_file_id = ? AND type = ?",
		fid1, fid2, "IMPORTS",
	).Scan(&confidence)
	if err != nil {
		t.Fatal(err)
	}
	if confidence != 1.0 {
		t.Errorf("expected default confidence 1.0, got %f", confidence)
	}
}

func TestCommunityOperations(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Create files
	fid1, _ := s.UpsertFile("src/auth.ts", "h1")
	fid2, _ := s.UpsertFile("src/login.ts", "h2")
	fid3, _ := s.UpsertFile("src/api.ts", "h3")

	// Insert communities
	if err := s.InsertCommunity(0, "auth-cluster", "Authentication"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCommunity(1, "api-cluster", "API Layer"); err != nil {
		t.Fatal(err)
	}

	// Assign members
	if err := s.InsertCommunityMember(fid1, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCommunityMember(fid2, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCommunityMember(fid3, 1); err != nil {
		t.Fatal(err)
	}

	// CommunityOf
	name, err := s.CommunityOf("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if name != "auth-cluster" {
		t.Errorf("expected community 'auth-cluster', got %q", name)
	}

	// CommunityOf for unassigned file
	name, err = s.CommunityOf("src/nonexistent.ts")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Errorf("expected empty community for unassigned file, got %q", name)
	}

	// AllCommunities
	communities, err := s.AllCommunities()
	if err != nil {
		t.Fatal(err)
	}
	if len(communities) != 2 {
		t.Fatalf("expected 2 communities, got %d", len(communities))
	}
	if communities[0].Name != "auth-cluster" || communities[0].MemberCount != 2 {
		t.Errorf("expected auth-cluster with 2 members, got %+v", communities[0])
	}
	if communities[1].Name != "api-cluster" || communities[1].MemberCount != 1 {
		t.Errorf("expected api-cluster with 1 member, got %+v", communities[1])
	}

	// ClearCommunities
	if err := s.ClearCommunities(); err != nil {
		t.Fatal(err)
	}
	communities, err = s.AllCommunities()
	if err != nil {
		t.Fatal(err)
	}
	if len(communities) != 0 {
		t.Errorf("expected 0 communities after clear, got %d", len(communities))
	}

	// CommunityOf should return empty after clear
	name, err = s.CommunityOf("src/auth.ts")
	if err != nil {
		t.Fatal(err)
	}
	if name != "" {
		t.Errorf("expected empty community after clear, got %q", name)
	}
}

func TestCallsOfAndCallersOf(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Setup: a.ts calls b.ts and c.ts; d.ts calls b.ts
	fidA, _ := s.UpsertFile("src/a.ts", "h1")
	fidB, _ := s.UpsertFile("src/b.ts", "h2")
	fidC, _ := s.UpsertFile("src/c.ts", "h3")
	fidD, _ := s.UpsertFile("src/d.ts", "h4")

	s.UpsertEdgeWithConfidence(fidA, fidB, "CALLS", 0.95)
	s.UpsertEdgeWithConfidence(fidA, fidC, "CALLS", 0.80)
	s.UpsertEdgeWithConfidence(fidD, fidB, "CALLS", 0.70)

	// CallsOf: a.ts should call b.ts and c.ts
	calls, err := s.CallsOf("src/a.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls from a.ts, got %d", len(calls))
	}
	if calls[0] != "src/b.ts" || calls[1] != "src/c.ts" {
		t.Errorf("expected [src/b.ts, src/c.ts], got %v", calls)
	}

	// CallersOf: b.ts should be called by a.ts and d.ts
	callers, err := s.CallersOf("src/b.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 2 {
		t.Fatalf("expected 2 callers of b.ts, got %d", len(callers))
	}
	if callers[0] != "src/a.ts" || callers[1] != "src/d.ts" {
		t.Errorf("expected [src/a.ts, src/d.ts], got %v", callers)
	}

	// CallsOf: d.ts should only call b.ts
	calls, err = s.CallsOf("src/d.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "src/b.ts" {
		t.Errorf("expected [src/b.ts], got %v", calls)
	}

	// CallersOf: c.ts should only be called by a.ts
	callers, err = s.CallersOf("src/c.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0] != "src/a.ts" {
		t.Errorf("expected [src/a.ts], got %v", callers)
	}

	// CallsOf with no calls should return nil/empty
	calls, err = s.CallsOf("src/b.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("expected 0 calls from b.ts, got %d", len(calls))
	}
}

func TestDeleteFileSymbolsCascade(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid, _ := s.UpsertFile("src/auth.ts", "h")
	s.InsertSymbols(fid, []graph.Symbol{
		{Name: "login", Kind: "function", StartLine: 1, EndLine: 5},
	})

	err = s.DeleteSymbolsForFile(fid)
	if err != nil {
		t.Fatal(err)
	}

	syms, _ := s.SymbolsForFile("src/auth.ts")
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols after delete, got %d", len(syms))
	}
}

func TestDeleteFile(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fid, _ := st.UpsertFile("a.ts", "aaa")
	st.InsertSymbols(fid, []graph.Symbol{
		{FilePath: "a.ts", Name: "foo", Kind: "function", StartLine: 1, EndLine: 5},
	})
	bid, _ := st.UpsertFile("b.ts", "bbb")
	st.UpsertEdge(fid, bid, "IMPORTS")

	if err := st.DeleteFile("a.ts"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	id, _ := st.FileID("a.ts")
	if id != 0 {
		t.Fatal("file a.ts should be deleted")
	}
	syms, _ := st.SymbolsForFile("a.ts")
	if len(syms) != 0 {
		t.Fatal("symbols should be cascade-deleted")
	}
}

func TestAllEdges(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fid, _ := st.UpsertFile("a.ts", "aaa")
	bid, _ := st.UpsertFile("b.ts", "bbb")
	st.UpsertEdge(fid, bid, "IMPORTS")
	st.UpsertEdgeWithConfidence(fid, bid, "CALLS", 0.9)

	edges, err := st.AllEdges()
	if err != nil {
		t.Fatalf("AllEdges: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(edges))
	}

	hasImport, hasCalls := false, false
	for _, e := range edges {
		if e.SourcePath == "a.ts" && e.TargetPath == "b.ts" && e.Type == "IMPORTS" {
			hasImport = true
		}
		if e.SourcePath == "a.ts" && e.TargetPath == "b.ts" && e.Type == "CALLS" && e.Confidence == 0.9 {
			hasCalls = true
		}
	}
	if !hasImport {
		t.Fatal("missing IMPORTS edge")
	}
	if !hasCalls {
		t.Fatal("missing CALLS edge with confidence 0.9")
	}
}

// ---------------------------------------------------------------------------
// Embedding tests
// ---------------------------------------------------------------------------

func makeVec(dim int, val float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = val
	}
	// Normalize to unit length.
	norm := float32(math.Sqrt(float64(dim))) * val
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= norm
	}
	return v
}

func TestUpsertAndQueryEmbedding(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid, err := s.UpsertFile("src/auth.ts", "hash1")
	if err != nil {
		t.Fatal(err)
	}

	vec := makeVec(384, 1.0)
	if err := s.UpsertEmbedding(fid, vec, "abc123"); err != nil {
		t.Fatal(err)
	}

	all, err := s.AllEmbeddings()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 embedding, got %d", len(all))
	}
	if all[0].FilePath != "src/auth.ts" {
		t.Errorf("expected path 'src/auth.ts', got %q", all[0].FilePath)
	}
	if len(all[0].Vector) != 384 {
		t.Errorf("expected dim 384, got %d", len(all[0].Vector))
	}
	// Verify values round-trip correctly.
	for i := range vec {
		if math.Abs(float64(all[0].Vector[i]-vec[i])) > 1e-6 {
			t.Errorf("vector mismatch at index %d: got %f, want %f", i, all[0].Vector[i], vec[i])
			break
		}
	}
}

func TestEmbeddingTextHash(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid, _ := s.UpsertFile("src/a.ts", "h1")
	vec := makeVec(384, 1.0)

	// Store with initial hash.
	if err := s.UpsertEmbedding(fid, vec, "hash_v1"); err != nil {
		t.Fatal(err)
	}
	hash, err := s.EmbeddingTextHash(fid)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash_v1" {
		t.Errorf("expected 'hash_v1', got %q", hash)
	}

	// Update with new hash.
	if err := s.UpsertEmbedding(fid, vec, "hash_v2"); err != nil {
		t.Fatal(err)
	}
	hash, err = s.EmbeddingTextHash(fid)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash_v2" {
		t.Errorf("expected 'hash_v2', got %q", hash)
	}

	// Non-existent file returns empty string.
	hash, err = s.EmbeddingTextHash(9999)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Errorf("expected empty hash for non-existent file, got %q", hash)
	}
}

func TestSearchSimilar(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fid1, _ := s.UpsertFile("src/auth.ts", "h1")
	fid2, _ := s.UpsertFile("src/db.ts", "h2")
	fid3, _ := s.UpsertFile("src/util.ts", "h3")

	// Create three distinct vectors.
	// vec1: heavy on first dimensions.
	vec1 := make([]float32, 384)
	vec1[0] = 1.0
	// vec2: heavy on middle dimensions.
	vec2 := make([]float32, 384)
	vec2[192] = 1.0
	// vec3: heavy on last dimensions.
	vec3 := make([]float32, 384)
	vec3[383] = 1.0

	s.UpsertEmbedding(fid1, vec1, "")
	s.UpsertEmbedding(fid2, vec2, "")
	s.UpsertEmbedding(fid3, vec3, "")

	// Query similar to vec1 — should rank auth.ts first.
	query := make([]float32, 384)
	query[0] = 1.0

	results, err := s.SearchSimilar(query, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].FilePath != "src/auth.ts" {
		t.Errorf("expected first result 'src/auth.ts', got %q", results[0].FilePath)
	}
	if results[0].Score < 0.99 {
		t.Errorf("expected score ~1.0 for exact match, got %f", results[0].Score)
	}

	// Limit to 1 result.
	results, err = s.SearchSimilar(query, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with limit, got %d", len(results))
	}
}
