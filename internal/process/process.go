package process

import (
	"sort"

	"github.com/neur0map/prowl/internal/graph"
)

// Process represents a detected execution flow.
type Process struct {
	Name  string   // derived from entry point symbol name
	Entry string   // entry point file path
	Steps []string // ordered file paths in the execution flow
	Type  string   // "intra_community" or "cross_community"
}

// rawProcess is an intermediate representation before classification.
type rawProcess struct {
	name  string
	entry string
	steps []string
	score float64
}

const (
	maxDepth     = 10
	maxBranching = 4
	maxProcesses = 75
)

// DetectProcesses traces execution flows from high-scoring entry points.
// communities maps filePath -> communityID (from community detection phase).
func DetectProcesses(g *graph.Graph, communities map[string]int) []Process {
	scored := ScoreEntryPoints(g)

	// Filter to seeds with score > 0.
	var seeds []ScoredEntry
	for _, s := range scored {
		if s.Score > 0 {
			seeds = append(seeds, s)
		}
	}

	// Build per-source outgoing CALLS edges map for efficient BFS lookup.
	callEdgesBySource := buildCallEdgeMap(g)

	// Trace a process from each seed.
	var raw []rawProcess
	for _, seed := range seeds {
		steps := bfsTrace(callEdgesBySource, seed.FilePath, maxDepth, maxBranching)
		if len(steps) < 2 {
			continue // skip trivial single-file processes
		}
		raw = append(raw, rawProcess{
			name:  seed.Symbol.Name,
			entry: seed.FilePath,
			steps: steps,
			score: seed.Score,
		})
	}

	// Prune subsumed paths: if A's steps are a subset of B's steps, drop A.
	raw = pruneSubsumed(raw)

	// Keep top maxProcesses by score.
	sort.Slice(raw, func(i, j int) bool {
		return raw[i].score > raw[j].score
	})
	if len(raw) > maxProcesses {
		raw = raw[:maxProcesses]
	}

	// Classify and build final Process structs.
	processes := make([]Process, 0, len(raw))
	for _, r := range raw {
		pType := classifyProcess(r.steps, communities)
		processes = append(processes, Process{
			Name:  r.name,
			Entry: r.entry,
			Steps: r.steps,
			Type:  pType,
		})
	}

	return processes
}

// buildCallEdgeMap groups CALLS edges by source path.
func buildCallEdgeMap(g *graph.Graph) map[string][]graph.Edge {
	m := make(map[string][]graph.Edge)
	for _, e := range g.EdgesOfType("CALLS") {
		m[e.SourcePath] = append(m[e.SourcePath], e)
	}
	return m
}

// bfsTrace performs a breadth-first trace from startFile through CALLS edges.
func bfsTrace(callEdges map[string][]graph.Edge, startFile string, maxDep, maxBranch int) []string {
	visited := map[string]bool{startFile: true}
	steps := []string{startFile}
	frontier := []string{startFile}

	for depth := 0; depth < maxDep && len(frontier) > 0; depth++ {
		var nextFrontier []string
		for _, file := range frontier {
			edges := callEdges[file]
			// Sort by confidence descending.
			sorted := make([]graph.Edge, len(edges))
			copy(sorted, edges)
			sort.Slice(sorted, func(i, j int) bool {
				return sorted[i].Confidence > sorted[j].Confidence
			})
			taken := 0
			for _, e := range sorted {
				if taken >= maxBranch {
					break
				}
				if visited[e.TargetPath] {
					continue
				}
				visited[e.TargetPath] = true
				steps = append(steps, e.TargetPath)
				nextFrontier = append(nextFrontier, e.TargetPath)
				taken++
			}
		}
		frontier = nextFrontier
	}

	return steps
}

// pruneSubsumed removes processes whose steps are a subset of another process's steps.
func pruneSubsumed(raw []rawProcess) []rawProcess {
	// Build step sets.
	type entry struct {
		proc rawProcess
		set  map[string]bool
	}
	entries := make([]entry, len(raw))
	for i, r := range raw {
		s := make(map[string]bool, len(r.steps))
		for _, st := range r.steps {
			s[st] = true
		}
		entries[i] = entry{proc: r, set: s}
	}

	var result []rawProcess
	for i, a := range entries {
		subsumed := false
		for j, b := range entries {
			if i == j {
				continue
			}
			if len(a.set) >= len(b.set) {
				continue
			}
			if isSubset(a.set, b.set) {
				subsumed = true
				break
			}
		}
		if !subsumed {
			result = append(result, a.proc)
		}
	}
	return result
}

// isSubset returns true if all keys in a are present in b.
func isSubset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// classifyProcess determines whether a process spans multiple communities.
func classifyProcess(steps []string, communities map[string]int) string {
	if len(steps) == 0 {
		return "intra_community"
	}
	firstCommunity, hasFirst := communities[steps[0]]
	if !hasFirst {
		// If no community info, consider all steps.
		// If any step has a different or missing community, treat as cross.
		for _, s := range steps[1:] {
			if _, ok := communities[s]; ok {
				return "cross_community"
			}
		}
		return "intra_community"
	}
	for _, s := range steps[1:] {
		c, ok := communities[s]
		if !ok || c != firstCommunity {
			return "cross_community"
		}
	}
	return "intra_community"
}
