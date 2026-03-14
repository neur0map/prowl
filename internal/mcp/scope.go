package mcp

import (
	"encoding/json"
	"io"
	"sort"

	"github.com/neur0map/prowl/internal/store"
)

// scopeResponse is the JSON structure returned by prowl_scope.
type scopeResponse struct {
	Task  string      `json:"task"`
	Files []scopeFile `json:"files"`
}

type scopeFile struct {
	Path       string   `json:"path"`
	Reason     string   `json:"reason"`
	Score      float64  `json:"score,omitempty"`
	Depth      int      `json:"depth"`
	Community  string   `json:"community,omitempty"`
	Exports    []string `json:"exports"`
	Signatures []string `json:"signatures"`
	Calls      []string `json:"calls"`
	Callers    []string `json:"callers"`
	Imports    []string `json:"imports"`
	Upstream   []string `json:"upstream"`
}

func (s *Server) handleScope(w io.Writer, id interface{}, params json.RawMessage) {
	var args struct {
		Task  string `json:"task"`
		Limit int    `json:"limit"`
	}
	json.Unmarshal(params, &args)
	if args.Task == "" {
		s.writeError(w, id, -32602, "Missing required parameter: task")
		return
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}

	if s.embedder == nil {
		s.writeError(w, id, -32603, "Embedder not available")
		return
	}

	// Step 1: Semantic search
	vecs, err := s.embedder.Encode([]string{args.Task})
	if err != nil {
		s.writeError(w, id, -32603, "Encode error: "+err.Error())
		return
	}

	searchLimit := 5
	if args.Limit < searchLimit {
		searchLimit = args.Limit
	}
	results, err := s.store.SearchSimilar(vecs[0], searchLimit)
	if err != nil {
		s.writeError(w, id, -32603, "Search error: "+err.Error())
		return
	}

	// Step 2: Build hit set and expand 1-hop
	type fileEntry struct {
		path           string
		reason         string
		score          float64
		hops           int // number of edges connecting to search hits
		communityBonus int // 1 if shares community with a search hit
	}

	seen := map[string]*fileEntry{}
	var hitPaths []string

	for _, r := range results {
		seen[r.FilePath] = &fileEntry{
			path:   r.FilePath,
			reason: "search_hit",
			score:  r.Score,
		}
		hitPaths = append(hitPaths, r.FilePath)
	}

	// Step 3: 1-hop expansion for each search hit
	for _, hitPath := range hitPaths {
		// Outgoing calls
		calls, _ := s.store.CallsOf(hitPath)
		for _, target := range calls {
			if _, ok := seen[target]; !ok {
				seen[target] = &fileEntry{
					path:   target,
					reason: "1-hop:called_by:" + hitPath,
				}
			}
			seen[target].hops++
		}

		// Outgoing imports
		imports, _ := s.store.ImportsOf(hitPath)
		for _, target := range imports {
			if _, ok := seen[target]; !ok {
				seen[target] = &fileEntry{
					path:   target,
					reason: "1-hop:imported_by:" + hitPath,
				}
			}
			seen[target].hops++
		}

		// Incoming callers
		callers, _ := s.store.CallersOf(hitPath)
		for _, source := range callers {
			if _, ok := seen[source]; !ok {
				seen[source] = &fileEntry{
					path:   source,
					reason: "1-hop:calls:" + hitPath,
				}
			}
			seen[source].hops++
		}

		// Incoming imports (upstream)
		upstream, _ := s.store.UpstreamOf(hitPath)
		for _, source := range upstream {
			if _, ok := seen[source]; !ok {
				seen[source] = &fileEntry{
					path:   source,
					reason: "1-hop:imports:" + hitPath,
				}
			}
			seen[source].hops++
		}
	}

	// Step 3.5: Compute community bonus for expanded files
	hitCommunities := map[string]bool{}
	for _, hitPath := range hitPaths {
		comm, _ := s.store.CommunityOf(hitPath)
		if comm != "" {
			hitCommunities[comm] = true
		}
	}
	for _, e := range seen {
		if e.reason != "search_hit" {
			comm, _ := s.store.CommunityOf(e.path)
			if comm != "" && hitCommunities[comm] {
				e.communityBonus = 1
			}
		}
	}

	// Step 4: Rank — search hits first (by blended score), then expanded (by communityBonus+hops)
	var entries []*fileEntry
	for _, e := range seen {
		entries = append(entries, e)
	}
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

	// Step 5: Cap at limit
	if len(entries) > args.Limit {
		entries = entries[:args.Limit]
	}

	// Step 5.5: Record heat for returned files
	for _, e := range entries {
		s.recordAccess(e.path)
	}

	// Step 5.6: Compute dependency depth
	var resultPaths []string
	for _, e := range entries {
		resultPaths = append(resultPaths, e.path)
	}
	depthMap := computeDepth(s.store, resultPaths)

	// Step 6: Assemble response with context
	var files []scopeFile
	for _, e := range entries {
		sf := scopeFile{
			Path:   e.path,
			Reason: e.reason,
			Score:  e.score,
			Depth:  depthMap[e.path],
		}

		fc, err := readFileContext(s.contextDir, e.path)
		if err == nil {
			sf.Community = fc.Community
			sf.Exports = fc.Exports
			sf.Signatures = fc.Signatures
			sf.Calls = fc.Calls
			sf.Callers = fc.Callers
			sf.Imports = fc.Imports
			sf.Upstream = fc.Upstream
		}

		files = append(files, sf)
	}

	// Final sort: depth ascending, preserve score order within same depth
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Depth < files[j].Depth
	})

	resp := scopeResponse{
		Task:  args.Task,
		Files: files,
	}

	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}

// computeDepth runs Kahn's algorithm on the result set to assign dependency depth.
// Depth 0 = leaf files (no dependencies within the set). Higher depth = depends on lower.
// Files in cycles get the same depth as the deepest non-cycle file + 1.
func computeDepth(st *store.Store, paths []string) map[string]int {
	pathSet := map[string]bool{}
	for _, p := range paths {
		pathSet[p] = true
	}

	// For each file, count how many in-set files it depends on (calls/imports)
	depCount := map[string]int{}
	dependents := map[string][]string{} // file -> files that depend on it

	for _, p := range paths {
		depCount[p] = 0
	}

	for _, p := range paths {
		calls, _ := st.CallsOf(p)
		imports, _ := st.ImportsOf(p)
		seen := map[string]bool{}
		for _, t := range append(calls, imports...) {
			if pathSet[t] && t != p && !seen[t] {
				seen[t] = true
				depCount[p]++
				dependents[t] = append(dependents[t], p)
			}
		}
	}

	// Kahn's: start with files that have depCount == 0 (no in-set dependencies)
	depth := map[string]int{}
	tentative := map[string]int{} // track max incoming depth before node resolves

	var queue []string
	for _, p := range paths {
		if depCount[p] == 0 {
			queue = append(queue, p)
			depth[p] = 0
		}
	}

	for len(queue) > 0 {
		var next []string
		for _, p := range queue {
			for _, dep := range dependents[p] {
				depCount[dep]--
				if d := depth[p] + 1; d > tentative[dep] {
					tentative[dep] = d
				}
				if depCount[dep] == 0 {
					depth[dep] = tentative[dep]
					next = append(next, dep)
				}
			}
		}
		queue = next
	}

	// Handle cycles: any file not fully resolved gets max_depth + 1
	maxDepth := 0
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}
	for _, p := range paths {
		if _, ok := depth[p]; !ok {
			depth[p] = maxDepth + 1
		}
	}

	return depth
}
