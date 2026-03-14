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
