package resolve

import (
	"strings"

	"github.com/neur0map/prowl/internal/graph"
)

// ResolvedEdge is a CALLS, EXTENDS, or IMPLEMENTS edge with confidence.
type ResolvedEdge struct {
	SourcePath string
	TargetPath string
	Type       string  // "CALLS", "EXTENDS", "IMPLEMENTS"
	Confidence float64
}

// ResolveCalls takes parsed call/heritage refs and resolves them to cross-file edges.
// g is the in-memory graph (has files, symbols, IMPORTS edges from phases 1-3).
// callsByFile maps filePath -> []CallRef, heritageByFile maps filePath -> []HeritageRef.
func ResolveCalls(
	g *graph.Graph,
	callsByFile map[string][]graph.CallRef,
	heritageByFile map[string][]graph.HeritageRef,
) []ResolvedEdge {
	// Build name index: symbol name -> list of file paths that define it.
	nameIndex := buildNameIndex(g)

	var result []ResolvedEdge

	// Resolve call refs.
	for fromFile, refs := range callsByFile {
		seen := make(map[string]bool) // track target files to deduplicate
		for _, ref := range refs {
			edges := resolveCallRef(g, nameIndex, fromFile, ref.CalleeName)
			for _, e := range edges {
				key := e.TargetPath
				if seen[key] {
					continue
				}
				seen[key] = true
				result = append(result, e)
			}
		}
	}

	// Resolve heritage refs.
	for fromFile, refs := range heritageByFile {
		for _, ref := range refs {
			edges := resolveHeritageRef(nameIndex, fromFile, ref)
			result = append(result, edges...)
		}
	}

	return result
}

// buildNameIndex creates a mapping from symbol name to the file paths that define it.
func buildNameIndex(g *graph.Graph) map[string][]string {
	idx := make(map[string][]string)
	for _, sym := range g.AllSymbols() {
		idx[sym.Name] = append(idx[sym.Name], sym.FilePath)
	}
	return idx
}

// resolveCallRef resolves a single call reference using 3-tier lookup.
// Returns zero or more ResolvedEdges (never includes self-file edges).
func resolveCallRef(
	g *graph.Graph,
	nameIndex map[string][]string,
	fromFile string,
	calleeName string,
) []ResolvedEdge {
	// Strip method receiver / qualifier: "foo.bar" -> "bar"
	if dot := strings.LastIndex(calleeName, "."); dot >= 0 {
		calleeName = calleeName[dot+1:]
	}

	// Tier 1: Check imported files for a matching exported symbol.
	imports := g.ImportsOf(fromFile)
	for _, impFile := range imports {
		for _, sym := range g.SymbolsForFile(impFile) {
			if sym.Name == calleeName {
				return []ResolvedEdge{{
					SourcePath: fromFile,
					TargetPath: impFile,
					Type:       "CALLS",
					Confidence: 0.9,
				}}
			}
		}
	}

	// Tier 2: Check same-file symbols — skip, since CALLS edges are file-to-file.
	for _, sym := range g.SymbolsForFile(fromFile) {
		if sym.Name == calleeName {
			// Intra-file call: no cross-file edge needed.
			return nil
		}
	}

	// Tier 3: Project-wide search.
	candidates := nameIndex[calleeName]
	// Filter out the calling file itself.
	var external []string
	for _, fp := range candidates {
		if fp != fromFile {
			external = append(external, fp)
		}
	}

	if len(external) == 0 {
		return nil
	}

	if len(external) == 1 {
		return []ResolvedEdge{{
			SourcePath: fromFile,
			TargetPath: external[0],
			Type:       "CALLS",
			Confidence: 0.5,
		}}
	}

	// Ambiguous: multiple candidates, emit all with low confidence.
	var edges []ResolvedEdge
	for _, fp := range external {
		edges = append(edges, ResolvedEdge{
			SourcePath: fromFile,
			TargetPath: fp,
			Type:       "CALLS",
			Confidence: 0.3,
		})
	}
	return edges
}

// ResolveCallsForFile resolves CALLS and heritage edges for a single file.
// Takes parsed CallRefs and HeritageRefs since these come from parser output.
func ResolveCallsForFile(g *graph.Graph, path string, calls []graph.CallRef, heritage []graph.HeritageRef) []graph.Edge {
	// Build name index for ALL symbols (matching existing buildNameIndex behavior)
	nameIndex := buildNameIndex(g)

	seen := make(map[string]bool) // dedup key: "target|type"
	var edges []graph.Edge

	for _, call := range calls {
		name := call.CalleeName
		// Strip qualifier: "foo.bar" -> "bar"
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}

		edge, ok := resolveOneCall(g, path, name, nameIndex, seen)
		if ok {
			edges = append(edges, edge)
		}
	}

	for _, h := range heritage {
		edgeType := "EXTENDS"
		if h.Type == "implements" {
			edgeType = "IMPLEMENTS"
		}

		// Search imported files first, then global
		var targetPath string
		var confidence float64

		for _, imp := range g.ImportsOf(path) {
			for _, sym := range g.SymbolsForFile(imp) {
				if sym.Name == h.ParentName {
					targetPath = imp
					confidence = 0.9
					break
				}
			}
			if targetPath != "" {
				break
			}
		}

		if targetPath == "" {
			if candidates, ok := nameIndex[h.ParentName]; ok {
				// Filter to external candidates (excluding self)
				var external []string
				for _, c := range candidates {
					if c != path {
						external = append(external, c)
					}
				}
				if len(external) > 0 {
					targetPath = external[0]
					// Match existing resolveHeritageRef confidence: 0.9 unique, 0.5 ambiguous
					confidence = 0.9
					if len(external) > 1 {
						confidence = 0.5
					}
				}
			}
		}

		if targetPath != "" {
			key := targetPath + "|" + edgeType
			if !seen[key] {
				seen[key] = true
				edges = append(edges, graph.Edge{
					SourcePath: path,
					TargetPath: targetPath,
					Type:       edgeType,
					Confidence: confidence,
				})
			}
		}
	}

	return edges
}

func resolveOneCall(g *graph.Graph, sourcePath, name string, nameIndex map[string][]string, seen map[string]bool) (graph.Edge, bool) {
	// Tier 1: imported files (check ALL symbols, matching existing resolveCallRef)
	for _, imp := range g.ImportsOf(sourcePath) {
		for _, sym := range g.SymbolsForFile(imp) {
			if sym.Name == name {
				key := imp + "|CALLS"
				if seen[key] {
					return graph.Edge{}, false
				}
				seen[key] = true
				return graph.Edge{
					SourcePath: sourcePath,
					TargetPath: imp,
					Type:       "CALLS",
					Confidence: 0.9,
				}, true
			}
		}
	}

	// Tier 2: same-file symbol — skip
	for _, sym := range g.SymbolsForFile(sourcePath) {
		if sym.Name == name {
			return graph.Edge{}, false
		}
	}

	// Tier 3: global search — count external candidates (excluding self)
	candidates, ok := nameIndex[name]
	if !ok {
		return graph.Edge{}, false
	}
	var external []string
	for _, c := range candidates {
		if c != sourcePath {
			external = append(external, c)
		}
	}
	if len(external) == 0 {
		return graph.Edge{}, false
	}
	conf := 0.5
	if len(external) > 1 {
		conf = 0.3
	}
	target := external[0]
	key := target + "|CALLS"
	if seen[key] {
		return graph.Edge{}, false
	}
	seen[key] = true
	return graph.Edge{
		SourcePath: sourcePath,
		TargetPath: target,
		Type:       "CALLS",
		Confidence: conf,
	}, true
}

// resolveHeritageRef resolves an extends/implements ref to a cross-file edge.
func resolveHeritageRef(
	nameIndex map[string][]string,
	fromFile string,
	ref graph.HeritageRef,
) []ResolvedEdge {
	candidates := nameIndex[ref.ParentName]
	// Filter out self-file.
	var external []string
	for _, fp := range candidates {
		if fp != fromFile {
			external = append(external, fp)
		}
	}

	if len(external) == 0 {
		return nil
	}

	edgeType := "EXTENDS"
	if ref.Type == "implements" {
		edgeType = "IMPLEMENTS"
	}

	// Use the first match; heritage is typically unambiguous.
	conf := 0.9
	if len(external) > 1 {
		conf = 0.5
	}

	return []ResolvedEdge{{
		SourcePath: fromFile,
		TargetPath: external[0],
		Type:       edgeType,
		Confidence: conf,
	}}
}
