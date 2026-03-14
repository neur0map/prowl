package community

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/graph"
)

// Community represents a detected community of related files.
type Community struct {
	ID      int
	Name    string
	Label   string
	Members []string // file paths
}

// DetectCommunities runs Louvain community detection on the code graph.
// Returns detected communities (excluding singletons).
func DetectCommunities(g *graph.Graph) []Community {
	nodes, neighbors, totalWeight := projectGraph(g)
	if len(nodes) == 0 || totalWeight == 0 {
		return nil
	}

	// Run Louvain
	assignments := louvain(nodes, neighbors, totalWeight)

	// Group by community
	groups := make(map[int][]string)
	for node, comm := range assignments {
		groups[comm] = append(groups[comm], node)
	}

	// Build result, skipping singletons
	var communities []Community
	id := 0
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		label := labelCommunity(members, id)
		communities = append(communities, Community{
			ID:      id,
			Name:    label,
			Label:   label,
			Members: members,
		})
		id++
	}

	// Sort for deterministic output
	sort.Slice(communities, func(i, j int) bool {
		return communities[i].Label < communities[j].Label
	})
	// Re-assign IDs after sorting
	for i := range communities {
		communities[i].ID = i
	}

	return communities
}

// qualifyingKinds are the symbol kinds that qualify a file for inclusion.
var qualifyingKinds = map[string]bool{
	"function":  true,
	"method":    true,
	"class":     true,
	"interface": true,
}

// projectGraph builds an undirected weighted graph from the code graph.
// Only files with qualifying symbols are included. Edges come from CALLS,
// EXTENDS, and IMPLEMENTS relationships. Returns the node list, adjacency
// map, and total edge weight (sum of unique undirected edges).
func projectGraph(g *graph.Graph) ([]string, map[string]map[string]float64, float64) {
	// Find qualifying files
	qualifying := make(map[string]bool)
	for _, f := range g.Files() {
		for _, sym := range g.SymbolsForFile(f.Path) {
			if qualifyingKinds[sym.Kind] {
				qualifying[f.Path] = true
				break
			}
		}
	}

	if len(qualifying) == 0 {
		return nil, nil, 0
	}

	// Build undirected weighted adjacency
	neighbors := make(map[string]map[string]float64)
	for path := range qualifying {
		neighbors[path] = make(map[string]float64)
	}

	addEdge := func(e graph.Edge) {
		src, tgt := e.SourcePath, e.TargetPath
		if !qualifying[src] || !qualifying[tgt] || src == tgt {
			return
		}
		conf := e.Confidence
		if conf <= 0 {
			conf = 0.5 // default weight for edges without confidence
		}
		neighbors[src][tgt] += conf
		neighbors[tgt][src] += conf
	}

	for _, e := range g.EdgesOfType("CALLS") {
		addEdge(e)
	}
	for _, e := range g.EdgesOfType("EXTENDS") {
		addEdge(e)
	}
	for _, e := range g.EdgesOfType("IMPLEMENTS") {
		addEdge(e)
	}

	// Compute total weight (each undirected edge counted once)
	var totalWeight float64
	seen := make(map[[2]string]bool)
	for src, adj := range neighbors {
		for tgt, w := range adj {
			key := [2]string{src, tgt}
			if src > tgt {
				key = [2]string{tgt, src}
			}
			if !seen[key] {
				seen[key] = true
				totalWeight += w
			}
		}
	}

	// Collect node list
	nodes := make([]string, 0, len(qualifying))
	for path := range qualifying {
		nodes = append(nodes, path)
	}
	sort.Strings(nodes)

	return nodes, neighbors, totalWeight
}

// louvainGraph is the internal representation used during Louvain iterations.
// Nodes are represented as integer indices for efficiency.
type louvainGraph struct {
	n         int                // number of nodes
	adj       []map[int]float64 // adjacency list: node -> neighbor -> weight
	degree    []float64          // weighted degree of each node
	m         float64            // total edge weight (each undirected edge counted once)
	selfLoops []float64          // self-loop weight for each node
}

// newLouvainGraph builds a louvainGraph from node names and adjacency map.
func newLouvainGraph(nodes []string, neighbors map[string]map[string]float64, m float64) (*louvainGraph, map[int]string) {
	n := len(nodes)
	nodeIndex := make(map[string]int, n)
	indexToNode := make(map[int]string, n)
	for i, name := range nodes {
		nodeIndex[name] = i
		indexToNode[i] = name
	}

	adj := make([]map[int]float64, n)
	degree := make([]float64, n)
	selfLoops := make([]float64, n)

	for i, name := range nodes {
		adj[i] = make(map[int]float64)
		for neighbor, w := range neighbors[name] {
			j, ok := nodeIndex[neighbor]
			if !ok {
				continue
			}
			if i == j {
				selfLoops[i] = w
				continue
			}
			adj[i][j] = w
			degree[i] += w
		}
		// Self-loops contribute to degree too
		degree[i] += selfLoops[i]
	}

	return &louvainGraph{
		n:         n,
		adj:       adj,
		degree:    degree,
		m:         m,
		selfLoops: selfLoops,
	}, indexToNode
}

// louvain runs the Louvain modularity optimization algorithm.
// Returns a map from original node name -> community ID.
func louvain(nodes []string, neighbors map[string]map[string]float64, totalWeight float64) map[string]int {
	lg, indexToNode := newLouvainGraph(nodes, neighbors, totalWeight)

	// Initialize: each node in its own community
	comm := make([]int, lg.n)
	for i := range comm {
		comm[i] = i
	}

	// Track the mapping from original nodes to current-level nodes.
	// At each level, originalMapping[originalIdx] = currentLevelNode
	// Initially each original node maps to itself.
	originalMapping := make([]int, lg.n)
	for i := range originalMapping {
		originalMapping[i] = i
	}
	originalCount := lg.n

	for {
		improved := phase1(lg, comm)
		if !improved {
			break
		}

		// Phase 2: aggregate
		newLg, oldToNew := aggregate(lg, comm)
		if newLg.n >= lg.n {
			// No reduction, stop
			break
		}

		// Update original mapping: each original node now maps through
		// the old community assignment + old-to-new mapping
		for i := 0; i < originalCount; i++ {
			oldNode := originalMapping[i]
			oldComm := comm[oldNode]
			originalMapping[i] = oldToNew[oldComm]
		}

		lg = newLg
		// Reset community assignments for new graph
		comm = make([]int, lg.n)
		for i := range comm {
			comm[i] = i
		}
	}

	// Build final assignments: original node name -> community ID
	// The community assignment is comm[originalMapping[i]]
	result := make(map[string]int, originalCount)
	for i := 0; i < originalCount; i++ {
		currentNode := originalMapping[i]
		result[indexToNode[i]] = comm[currentNode]
	}

	return result
}

// phase1 performs local node moves to maximize modularity.
// Returns true if any node was moved.
func phase1(lg *louvainGraph, comm []int) bool {
	m := lg.m
	if m == 0 {
		return false
	}

	// Precompute sigmaTot for each community:
	// sum of degrees of all nodes in that community
	sigmaTot := make(map[int]float64)
	for i := 0; i < lg.n; i++ {
		sigmaTot[comm[i]] += lg.degree[i]
	}

	// Precompute sigmaIn for each community:
	// sum of weights of edges inside the community (each internal edge counted once)
	sigmaIn := make(map[int]float64)
	for i := 0; i < lg.n; i++ {
		sigmaIn[comm[i]] += lg.selfLoops[i]
		for j, w := range lg.adj[i] {
			if comm[j] == comm[i] && j > i {
				sigmaIn[comm[i]] += w
			}
		}
	}

	anyImproved := false
	for {
		improved := false
		for i := 0; i < lg.n; i++ {
			currentComm := comm[i]
			ki := lg.degree[i]

			// Compute ki,in for neighboring communities
			kiIn := make(map[int]float64)
			for j, w := range lg.adj[i] {
				kiIn[comm[j]] += w
			}

			// Try removing node i from its current community
			// and adding it to each neighboring community.
			// Use the standard Louvain gain formula:
			//   ΔQ = [ki,in_new/m - σtot_new * ki / (2*m²)]
			//      - [ki,in_cur/m - (σtot_cur - ki) * ki / (2*m²)]
			//
			// Simplified: ΔQ = (ki,in_new - ki,in_cur)/m - ki*(σtot_new - σtot_cur + ki)/(2*m²)

			bestComm := currentComm
			bestGain := 0.0

			kiInCurrent := kiIn[currentComm]
			sigmaTotCurrent := sigmaTot[currentComm] - ki // remove self from current community

			for candidateComm, kiInCandidate := range kiIn {
				if candidateComm == currentComm {
					continue
				}
				sTot := sigmaTot[candidateComm]

				gain := (kiInCandidate - kiInCurrent) / m
				gain -= ki * (sTot - sigmaTotCurrent) / (2 * m * m)

				if gain > bestGain {
					bestGain = gain
					bestComm = candidateComm
				}
			}

			if bestComm != currentComm {
				// Move node i from currentComm to bestComm
				// Update sigmaTot
				sigmaTot[currentComm] -= ki
				sigmaTot[bestComm] += ki

				// Update sigmaIn
				// Remove internal edges that node i had with currentComm
				sigmaIn[currentComm] -= kiIn[currentComm]
				// Add internal edges that node i has with bestComm
				sigmaIn[bestComm] += kiIn[bestComm]

				comm[i] = bestComm
				improved = true
				anyImproved = true
			}
		}
		if !improved {
			break
		}
	}
	return anyImproved
}

// aggregate creates a new louvainGraph where each community becomes a super-node.
// Returns the new graph and a mapping from old community ID -> new node index.
func aggregate(lg *louvainGraph, comm []int) (*louvainGraph, map[int]int) {
	// Find unique communities and assign new indices
	commSet := make(map[int]bool)
	for _, c := range comm {
		commSet[c] = true
	}

	// Sort community IDs for deterministic mapping
	commIDs := make([]int, 0, len(commSet))
	for c := range commSet {
		commIDs = append(commIDs, c)
	}
	sort.Ints(commIDs)

	oldToNew := make(map[int]int, len(commIDs))
	for newIdx, oldComm := range commIDs {
		oldToNew[oldComm] = newIdx
	}

	newN := len(commIDs)
	newAdj := make([]map[int]float64, newN)
	newSelfLoops := make([]float64, newN)
	for i := range newAdj {
		newAdj[i] = make(map[int]float64)
	}

	// Aggregate edges
	for i := 0; i < lg.n; i++ {
		ci := oldToNew[comm[i]]
		// Self-loops from old graph
		newSelfLoops[ci] += lg.selfLoops[i]

		for j, w := range lg.adj[i] {
			cj := oldToNew[comm[j]]
			if ci == cj {
				// Internal edge becomes self-loop (count once: i < j)
				if i < j {
					newSelfLoops[ci] += w
				}
			} else {
				newAdj[ci][cj] += w
			}
		}
	}

	// Since adj stores both directions of each undirected edge,
	// the aggregated inter-community edges are also double-counted.
	// We need to halve them and then store both directions.
	// Actually, in our adjacency list each undirected edge (i,j) is stored
	// as adj[i][j] and adj[j][i], both with weight w.
	// When aggregating, edge (i,j) with comm[i]=A, comm[j]=B contributes
	// w to newAdj[A][B] (from the i iteration) and w to newAdj[B][A]
	// (from the j iteration). So newAdj already has both directions. Good.

	// Compute degrees
	newDegree := make([]float64, newN)
	for i := 0; i < newN; i++ {
		for _, w := range newAdj[i] {
			newDegree[i] += w
		}
		newDegree[i] += newSelfLoops[i]
	}

	// Total weight stays the same through aggregation
	newM := lg.m

	return &louvainGraph{
		n:         newN,
		adj:       newAdj,
		degree:    newDegree,
		m:         newM,
		selfLoops: newSelfLoops,
	}, oldToNew
}

// labelCommunity derives a human-readable label for a community from its members.
func labelCommunity(members []string, fallbackID int) string {
	// Count directory segment occurrences
	dirCount := make(map[string]int)
	// Directories to skip as they're too generic
	skip := map[string]bool{
		".": true, "src": true, "lib": true, "internal": true,
		"pkg": true, "app": true, "cmd": true,
	}

	for _, path := range members {
		dir := filepath.Dir(path)
		parts := strings.Split(filepath.ToSlash(dir), "/")
		seen := make(map[string]bool) // count each segment once per file
		for _, p := range parts {
			if p == "" || skip[p] {
				continue
			}
			if !seen[p] {
				seen[p] = true
				dirCount[p]++
			}
		}
	}

	// Find most common directory segment
	bestDir := ""
	bestCount := 0
	for dir, count := range dirCount {
		if count > bestCount || (count == bestCount && dir < bestDir) {
			bestCount = count
			bestDir = dir
		}
	}

	if bestDir != "" {
		return bestDir
	}

	// Fallback: shared prefix of filenames
	if prefix := sharedPrefix(members); prefix != "" {
		return prefix
	}

	return fmt.Sprintf("cluster-%d", fallbackID)
}

// sharedPrefix finds the longest shared filename prefix among members.
func sharedPrefix(members []string) string {
	if len(members) == 0 {
		return ""
	}

	// Extract base names without extension
	names := make([]string, len(members))
	for i, m := range members {
		base := filepath.Base(m)
		ext := filepath.Ext(base)
		names[i] = strings.TrimSuffix(base, ext)
	}

	prefix := names[0]
	for _, name := range names[1:] {
		for !strings.HasPrefix(name, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}

	// Only return if the prefix is meaningful (at least 3 chars)
	if len(prefix) >= 3 {
		return prefix
	}
	return ""
}
