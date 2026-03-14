package mcp

import (
	"encoding/json"
	"io"
	"sort"
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
		path   string
		reason string
		score  float64
		hops   int // number of edges connecting to search hits
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

	// Step 4: Rank — search hits first (by score desc), then expanded (by hops desc)
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
			return entries[i].score > entries[j].score
		}
		return entries[i].hops > entries[j].hops
	})

	// Step 5: Cap at limit
	if len(entries) > args.Limit {
		entries = entries[:args.Limit]
	}

	// Step 6: Assemble response with context
	var files []scopeFile
	for _, e := range entries {
		sf := scopeFile{
			Path:   e.path,
			Reason: e.reason,
			Score:  e.score,
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
