package process

import (
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

// helper to build a graph with files, symbols, and edges.
func newTestGraph() *graph.Graph {
	return graph.New()
}

func addFile(g *graph.Graph, path string) {
	g.AddFile(graph.FileRecord{Path: path, Hash: "abc"})
}

func addSymbol(g *graph.Graph, name, kind, filePath string, exported bool) {
	g.AddSymbol(graph.Symbol{
		Name:       name,
		Kind:       kind,
		FilePath:   filePath,
		IsExported: exported,
	})
}

func addCallEdge(g *graph.Graph, src, tgt string, confidence float64) {
	g.AddEdge(graph.Edge{
		SourcePath: src,
		TargetPath: tgt,
		Type:       "CALLS",
		Confidence: confidence,
	})
}

func TestScoreEntryPoints(t *testing.T) {
	g := newTestGraph()

	// handleRequest: promoted name, exported, in /handlers/ directory
	addFile(g, "src/handlers/auth.go")
	addSymbol(g, "handleRequest", "function", "src/handlers/auth.go", true)
	// This file calls out to 3 other files, nobody calls it
	addFile(g, "src/services/user.go")
	addFile(g, "src/services/db.go")
	addFile(g, "src/services/cache.go")
	addCallEdge(g, "src/handlers/auth.go", "src/services/user.go", 0.9)
	addCallEdge(g, "src/handlers/auth.go", "src/services/db.go", 0.8)
	addCallEdge(g, "src/handlers/auth.go", "src/services/cache.go", 0.7)

	// getUser: demoted name, not exported
	addSymbol(g, "getUser", "function", "src/services/user.go", false)
	// This file calls 1 file, but is called by 1 file
	addCallEdge(g, "src/services/user.go", "src/services/db.go", 0.8)

	// helper: default name, not exported, no framework path
	addFile(g, "src/utils/helper.go")
	addSymbol(g, "helper", "function", "src/utils/helper.go", false)
	// No outgoing calls, called by nobody

	entries := ScoreEntryPoints(g)

	if len(entries) == 0 {
		t.Fatal("expected scored entries, got none")
	}

	// handleRequest should be first (highest score)
	if entries[0].Symbol.Name != "handleRequest" {
		t.Errorf("expected handleRequest as top entry, got %s", entries[0].Symbol.Name)
	}

	// Verify handleRequest's score components:
	// ratio = 3 / (0 + 1) = 3.0
	// visFactor = 1.5 (exported)
	// nameFactor = 2.0 (promotePrefix: handleR...)
	// fwFactor = 2.0 (/handlers/)
	// score = 3.0 * 1.5 * 2.0 * 2.0 = 18.0
	expectedScore := 18.0
	if entries[0].Score != expectedScore {
		t.Errorf("expected handleRequest score %.1f, got %.1f", expectedScore, entries[0].Score)
	}

	// getUser should have a lower score due to demotion
	var getUserScore float64
	for _, e := range entries {
		if e.Symbol.Name == "getUser" {
			getUserScore = e.Score
			break
		}
	}
	if getUserScore >= entries[0].Score {
		t.Errorf("getUser score %.1f should be less than handleRequest %.1f", getUserScore, entries[0].Score)
	}

	// helper should have score 0 (no outgoing CALLS → ratio = 0)
	var helperScore float64
	for _, e := range entries {
		if e.Symbol.Name == "helper" {
			helperScore = e.Score
			break
		}
	}
	if helperScore != 0 {
		t.Errorf("expected helper score 0, got %.1f", helperScore)
	}
}

func TestScoreEntryPoints_SkipsNonScorableKinds(t *testing.T) {
	g := newTestGraph()
	addFile(g, "src/types.go")
	addSymbol(g, "UserType", "type", "src/types.go", true)
	addSymbol(g, "MaxRetries", "const", "src/types.go", true)
	addSymbol(g, "Status", "enum", "src/types.go", true)
	addSymbol(g, "IService", "interface", "src/types.go", true)

	entries := ScoreEntryPoints(g)
	if len(entries) != 0 {
		t.Errorf("expected no scored entries for non-scorable kinds, got %d", len(entries))
	}
}

func TestBFSTrace(t *testing.T) {
	g := newTestGraph()

	addFile(g, "a.go")
	addFile(g, "b.go")
	addFile(g, "c.go")
	addFile(g, "d.go")

	addCallEdge(g, "a.go", "b.go", 0.9)
	addCallEdge(g, "b.go", "c.go", 0.8)
	addCallEdge(g, "c.go", "d.go", 0.7)

	callEdges := buildCallEdgeMap(g)
	steps := bfsTrace(callEdges, "a.go", 10, 4)

	expected := []string{"a.go", "b.go", "c.go", "d.go"}
	if len(steps) != len(expected) {
		t.Fatalf("expected %d steps, got %d: %v", len(expected), len(steps), steps)
	}
	for i, s := range expected {
		if steps[i] != s {
			t.Errorf("step[%d]: expected %s, got %s", i, s, steps[i])
		}
	}
}

func TestBFSTrace_CycleAvoidance(t *testing.T) {
	g := newTestGraph()

	addFile(g, "a.go")
	addFile(g, "b.go")

	addCallEdge(g, "a.go", "b.go", 0.9)
	addCallEdge(g, "b.go", "a.go", 0.8) // cycle back

	callEdges := buildCallEdgeMap(g)
	steps := bfsTrace(callEdges, "a.go", 10, 4)

	// Should visit a.go and b.go, then stop (b→a is a cycle).
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps (cycle avoidance), got %d: %v", len(steps), steps)
	}
}

func TestProcessDetection(t *testing.T) {
	g := newTestGraph()

	// Entry point 1: handleAuth → auth service → db
	addFile(g, "handlers/auth.go")
	addFile(g, "services/auth.go")
	addFile(g, "db/query.go")
	addSymbol(g, "handleAuth", "function", "handlers/auth.go", true)
	addCallEdge(g, "handlers/auth.go", "services/auth.go", 0.9)
	addCallEdge(g, "services/auth.go", "db/query.go", 0.8)

	// Entry point 2: handleUsers → user service → db (different path)
	addFile(g, "handlers/users.go")
	addFile(g, "services/users.go")
	addSymbol(g, "handleUsers", "function", "handlers/users.go", true)
	addCallEdge(g, "handlers/users.go", "services/users.go", 0.9)
	addCallEdge(g, "services/users.go", "db/query.go", 0.7)

	communities := map[string]int{
		"handlers/auth.go":  0,
		"services/auth.go":  0,
		"db/query.go":       1,
		"handlers/users.go": 0,
		"services/users.go": 0,
	}

	processes := DetectProcesses(g, communities)

	if len(processes) != 2 {
		t.Fatalf("expected 2 processes, got %d", len(processes))
	}

	// Both processes should be cross_community (handlers→db spans communities 0 and 1).
	for _, p := range processes {
		if p.Type != "cross_community" {
			t.Errorf("process %s: expected cross_community, got %s", p.Name, p.Type)
		}
		if len(p.Steps) < 2 {
			t.Errorf("process %s: expected at least 2 steps, got %d", p.Name, len(p.Steps))
		}
	}
}

func TestProcessDetection_IntraCommunity(t *testing.T) {
	g := newTestGraph()

	addFile(g, "handlers/auth.go")
	addFile(g, "handlers/middleware.go")
	addSymbol(g, "handleAuth", "function", "handlers/auth.go", true)
	addCallEdge(g, "handlers/auth.go", "handlers/middleware.go", 0.9)

	communities := map[string]int{
		"handlers/auth.go":       0,
		"handlers/middleware.go": 0,
	}

	processes := DetectProcesses(g, communities)
	if len(processes) != 1 {
		t.Fatalf("expected 1 process, got %d", len(processes))
	}
	if processes[0].Type != "intra_community" {
		t.Errorf("expected intra_community, got %s", processes[0].Type)
	}
}

func TestMaxBranchingLimit(t *testing.T) {
	g := newTestGraph()

	addFile(g, "src/entry.go")
	// Add 6 target files
	targets := []string{"t1.go", "t2.go", "t3.go", "t4.go", "t5.go", "t6.go"}
	for i, tgt := range targets {
		addFile(g, tgt)
		// Confidence descending so we know which 4 should be picked
		addCallEdge(g, "src/entry.go", tgt, float64(6-i)*0.1) // 0.6, 0.5, 0.4, 0.3, 0.2, 0.1
	}

	callEdges := buildCallEdgeMap(g)
	steps := bfsTrace(callEdges, "src/entry.go", 10, 4)

	// Should be: entry + top 4 by confidence = 5 steps total
	if len(steps) != 5 {
		t.Fatalf("expected 5 steps (1 start + 4 branches), got %d: %v", len(steps), steps)
	}

	// The top 4 by confidence are t1, t2, t3, t4
	expectedTargets := map[string]bool{
		"t1.go": true,
		"t2.go": true,
		"t3.go": true,
		"t4.go": true,
	}
	for _, s := range steps[1:] {
		if !expectedTargets[s] {
			t.Errorf("unexpected step %s in BFS results (should only have top 4 by confidence)", s)
		}
	}

	// t5 and t6 should NOT appear
	for _, s := range steps {
		if s == "t5.go" || s == "t6.go" {
			t.Errorf("step %s should have been pruned by branching limit", s)
		}
	}
}

func TestSubsumedPruning(t *testing.T) {
	// Process A: steps [x, y]
	// Process B: steps [x, y, z]
	// Process A should be pruned because it's a subset of B.

	raw := []rawProcess{
		{name: "procA", entry: "x", steps: []string{"x", "y"}, score: 5.0},
		{name: "procB", entry: "x", steps: []string{"x", "y", "z"}, score: 3.0},
	}

	result := pruneSubsumed(raw)

	if len(result) != 1 {
		t.Fatalf("expected 1 process after pruning, got %d", len(result))
	}
	if result[0].name != "procB" {
		t.Errorf("expected procB to survive pruning, got %s", result[0].name)
	}
}

func TestSubsumedPruning_NoSubset(t *testing.T) {
	// Neither is a subset of the other.
	raw := []rawProcess{
		{name: "procA", entry: "a", steps: []string{"a", "b"}, score: 5.0},
		{name: "procB", entry: "c", steps: []string{"c", "d"}, score: 3.0},
	}

	result := pruneSubsumed(raw)

	if len(result) != 2 {
		t.Fatalf("expected 2 processes (no pruning), got %d", len(result))
	}
}

func TestNameFactor(t *testing.T) {
	tests := []struct {
		name     string
		expected float64
	}{
		{"getUser", 0.3},         // demoted
		{"setConfig", 0.3},       // demoted
		{"isValid", 0.3},         // demoted
		{"parseJSON", 0.3},       // demoted
		{"handleRequest", 2.0},   // promoted prefix
		{"main", 2.0},            // promoted exact
		{"init", 2.0},            // promoted exact
		{"AuthHandler", 2.0},     // promoted suffix
		{"UserController", 2.0},  // promoted suffix
		{"apiMiddleware", 2.0},   // promoted suffix (ends with Middleware)
		{"doSomething", 1.0},     // default
		{"processData", 2.0},     // promoted prefix
	}

	for _, tt := range tests {
		got := computeNameFactor(tt.name)
		if got != tt.expected {
			t.Errorf("computeNameFactor(%q) = %.1f, want %.1f", tt.name, got, tt.expected)
		}
	}
}

func TestFWFactor(t *testing.T) {
	tests := []struct {
		path     string
		expected float64
	}{
		{"src/handlers/auth.go", 2.0},
		{"src/routes/api.go", 2.0},
		{"src/controllers/user.go", 2.0},
		{"cmd/server/main.go", 2.0},
		{"src/api/v1/handler.go", 2.0},
		{"main.go", 2.0},
		{"server.go", 2.0},
		{"app.go", 2.0},
		{"src/utils/helper.go", 1.0},
		{"src/models/user.go", 1.0},
	}

	for _, tt := range tests {
		got := computeFWFactor(tt.path)
		if got != tt.expected {
			t.Errorf("computeFWFactor(%q) = %.1f, want %.1f", tt.path, got, tt.expected)
		}
	}
}
