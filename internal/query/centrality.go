package query

import (
	"sort"

	"github.com/neur0map/prowl/internal/store"
)

// fileCentrality computes a PageRank-style importance score per file from
// resolved file-to-file dependency edges: a file scores high when many
// (transitively important) files depend on it. Unlike a raw in-degree count
// (which excludes component instantiation as UI-tree noise), this captures the
// QML/desktop coupling that instantiation and singleton use create, so the
// building-block components surface as central. It also returns the plain
// in-degree per file. Deterministic: fixed damping and iteration count, and the
// per-node sum is order-independent.
func fileCentrality(paths []string, edges []store.FileEdge) (map[string]float64, map[string]int) {
	const damping = 0.85
	const iterations = 40
	nodes := make(map[string]bool, len(paths))
	for _, p := range paths {
		nodes[p] = true
	}
	out := make(map[string]int, len(paths))
	in := make(map[string][]string, len(paths))
	inDegree := make(map[string]int, len(paths))
	for _, e := range edges {
		if e.SrcFile == e.DstFile || !nodes[e.SrcFile] || !nodes[e.DstFile] {
			continue
		}
		out[e.SrcFile]++
		in[e.DstFile] = append(in[e.DstFile], e.SrcFile)
		inDegree[e.DstFile]++
	}
	n := len(paths)
	score := make(map[string]float64, n)
	if n == 0 {
		return score, inDegree
	}
	init := 1.0 / float64(n)
	for _, p := range paths {
		score[p] = init
	}
	base := (1 - damping) / float64(n)
	for range iterations {
		var dangling float64
		for _, p := range paths {
			if out[p] == 0 {
				dangling += score[p]
			}
		}
		share := damping * dangling / float64(n)
		next := make(map[string]float64, n)
		for _, p := range paths {
			sum := 0.0
			for _, src := range in[p] {
				sum += score[src] / float64(out[src])
			}
			next[p] = base + share + damping*sum
		}
		score = next
	}
	return score, inDegree
}

// sortByCentrality orders paths by score (desc), then in-degree (desc), then
// path, in place. The final path tiebreak keeps the ordering deterministic.
func sortByCentrality(paths []string, score map[string]float64, degree map[string]int) {
	sort.Slice(paths, func(i, j int) bool {
		a, b := paths[i], paths[j]
		if score[a] != score[b] {
			return score[a] > score[b]
		}
		if degree[a] != degree[b] {
			return degree[a] > degree[b]
		}
		return a < b
	})
}

// centralFiles ranks non-vendored files by centrality (then in-degree, then
// path) and returns the top n as fan rows carrying their in-degree.
func centralFiles(files []store.File, edges []store.FileEdge, n int) []store.FanRow {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.RelPath)
	}
	score, degree := fileCentrality(paths, edges)
	ranked := make([]string, 0, len(paths))
	for _, p := range paths {
		if !isVendored(p) {
			ranked = append(ranked, p)
		}
	}
	sortByCentrality(ranked, score, degree)
	out := make([]store.FanRow, 0, n)
	for _, p := range ranked {
		out = append(out, store.FanRow{File: p, In: degree[p]})
		if len(out) >= n {
			break
		}
	}
	return out
}

// centralFromEdges ranks files by centrality using only the edge set to derive
// the node universe, so a caller that already holds a bounded edge slice (the
// overview) needs no extra file read. Non-vendored files rank first; ties break
// by in-degree then path.
func centralFromEdges(edges []store.FileEdge, n int) []store.FanRow {
	nodeSet := make(map[string]bool)
	for _, e := range edges {
		nodeSet[e.SrcFile] = true
		nodeSet[e.DstFile] = true
	}
	paths := make([]string, 0, len(nodeSet))
	for p := range nodeSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	score, degree := fileCentrality(paths, edges)
	ranked := make([]string, 0, len(paths))
	for _, p := range paths {
		if !isVendored(p) {
			ranked = append(ranked, p)
		}
	}
	sortByCentrality(ranked, score, degree)
	out := make([]store.FanRow, 0, n)
	for _, p := range ranked {
		out = append(out, store.FanRow{File: p, In: degree[p]})
		if len(out) >= n {
			break
		}
	}
	return out
}
