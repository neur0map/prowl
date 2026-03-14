package mcp

import (
	"encoding/json"
	"io"
)

// impactResponse is the JSON structure returned by prowl_impact.
type impactResponse struct {
	Target               string                `json:"target"`
	Symbol               string                `json:"symbol,omitempty"`
	DirectDependents     []impactDependent     `json:"direct_dependents"`
	TransitiveDependents []transitiveDependent `json:"transitive_dependents"`
	AffectedCommunities  []string              `json:"affected_communities"`
	CrossCommunity       bool                  `json:"cross_community"`
}

type impactDependent struct {
	Path       string   `json:"path"`
	EdgeTypes  []string `json:"edge_types"`
	Exports    []string `json:"exports,omitempty"`
	Signatures []string `json:"signatures,omitempty"`
}

type transitiveDependent struct {
	Path    string   `json:"path"`
	Via     string   `json:"via"`
	Exports []string `json:"exports,omitempty"`
}

func (s *Server) handleImpact(w io.Writer, id interface{}, params json.RawMessage) {
	var args struct {
		Path   string `json:"path"`
		Symbol string `json:"symbol"`
	}
	json.Unmarshal(params, &args)
	if args.Path == "" {
		s.writeError(w, id, -32602, "Missing required parameter: path")
		return
	}

	s.recordAccess(args.Path)

	// Step 1: Find direct dependents (files with edges pointing TO target)
	callers, _ := s.store.CallersOf(args.Path)

	// Symbol filtering: if symbol is provided, only include callers if the
	// target file actually contains that symbol (heuristic — edges are file-level).
	if args.Symbol != "" {
		syms, _ := s.store.SymbolsForFile(args.Path)
		hasSymbol := false
		for _, sym := range syms {
			if sym.Name == args.Symbol {
				hasSymbol = true
				break
			}
		}
		if !hasSymbol {
			callers = nil
		}
	}

	upstream, _ := s.store.UpstreamOf(args.Path)

	// Build direct dependents with edge types
	directMap := map[string]map[string]bool{}
	for _, caller := range callers {
		if directMap[caller] == nil {
			directMap[caller] = map[string]bool{}
		}
		directMap[caller]["CALLS"] = true
	}
	for _, importer := range upstream {
		if directMap[importer] == nil {
			directMap[importer] = map[string]bool{}
		}
		directMap[importer]["IMPORTS"] = true
	}

	// Assemble direct dependents with context
	var directs []impactDependent
	for path, edgeSet := range directMap {
		var edgeTypes []string
		for et := range edgeSet {
			edgeTypes = append(edgeTypes, et)
		}
		dep := impactDependent{
			Path:      path,
			EdgeTypes: edgeTypes,
		}
		if fc, err := readFileContext(s.contextDir, path); err == nil {
			dep.Exports = fc.Exports
			dep.Signatures = fc.Signatures
		}
		directs = append(directs, dep)
	}

	// Step 2: Find transitive dependents
	transitiveMap := map[string]string{} // path -> via
	for directPath := range directMap {
		transCallers, _ := s.store.CallersOf(directPath)
		for _, tc := range transCallers {
			if _, isDirect := directMap[tc]; !isDirect && tc != args.Path {
				if _, seen := transitiveMap[tc]; !seen {
					transitiveMap[tc] = directPath
				}
			}
		}
		transUpstream, _ := s.store.UpstreamOf(directPath)
		for _, tu := range transUpstream {
			if _, isDirect := directMap[tu]; !isDirect && tu != args.Path {
				if _, seen := transitiveMap[tu]; !seen {
					transitiveMap[tu] = directPath
				}
			}
		}
	}

	var transitives []transitiveDependent
	for path, via := range transitiveMap {
		td := transitiveDependent{
			Path: path,
			Via:  via,
		}
		if fc, err := readFileContext(s.contextDir, path); err == nil {
			td.Exports = fc.Exports
		}
		transitives = append(transitives, td)
	}

	// Step 3: Classify communities
	targetComm, _ := s.store.CommunityOf(args.Path)
	commSet := map[string]bool{}
	if targetComm != "" {
		commSet[targetComm] = true
	}
	for path := range directMap {
		c, _ := s.store.CommunityOf(path)
		if c != "" {
			commSet[c] = true
		}
	}
	for path := range transitiveMap {
		c, _ := s.store.CommunityOf(path)
		if c != "" {
			commSet[c] = true
		}
	}

	var affectedComms []string
	for c := range commSet {
		affectedComms = append(affectedComms, c)
	}

	crossCommunity := false
	for _, c := range affectedComms {
		if c != targetComm {
			crossCommunity = true
			break
		}
	}

	resp := impactResponse{
		Target:               args.Path,
		Symbol:               args.Symbol,
		DirectDependents:     directs,
		TransitiveDependents: transitives,
		AffectedCommunities:  affectedComms,
		CrossCommunity:       crossCommunity,
	}

	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}
