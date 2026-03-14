package graph

import "sort"

// CommunityInfo holds the community assignment for a file.
type CommunityInfo struct {
	ID    int
	Name  string
	Label string
}

// Graph holds the in-memory representation of files, symbols, and edges.
type Graph struct {
	files       map[string]FileRecord      // path -> file
	symbols     map[string][]Symbol        // filePath -> symbols
	edges       map[string][]Edge          // sourcePath -> outgoing edges
	reverse     map[string][]string        // targetPath -> list of source paths (upstream)
	communities map[string]CommunityInfo   // filePath -> community assignment
}

// New creates an empty graph.
func New() *Graph {
	return &Graph{
		files:       make(map[string]FileRecord),
		symbols:     make(map[string][]Symbol),
		edges:       make(map[string][]Edge),
		reverse:     make(map[string][]string),
		communities: make(map[string]CommunityInfo),
	}
}

// AddFile registers a file.
func (g *Graph) AddFile(f FileRecord) {
	g.files[f.Path] = f
}

// AddSymbol adds a symbol to its file's symbol list.
func (g *Graph) AddSymbol(s Symbol) {
	g.symbols[s.FilePath] = append(g.symbols[s.FilePath], s)
}

// AddEdge adds a directed edge and updates the reverse index.
func (g *Graph) AddEdge(e Edge) {
	g.edges[e.SourcePath] = append(g.edges[e.SourcePath], e)
	g.reverse[e.TargetPath] = append(g.reverse[e.TargetPath], e.SourcePath)
}

// ReplaceFile clears all symbols and outgoing edges for a file, then
// re-registers it with the new record. Also cleans up the reverse index.
func (g *Graph) ReplaceFile(path string, f FileRecord) {
	// Remove outgoing edges from reverse index
	for _, e := range g.edges[path] {
		g.removeReverse(e.TargetPath, path)
	}
	delete(g.edges, path)
	delete(g.symbols, path)
	delete(g.communities, path)
	g.files[path] = f
}

func (g *Graph) removeReverse(target, source string) {
	rev := g.reverse[target]
	for i, s := range rev {
		if s == source {
			g.reverse[target] = append(rev[:i], rev[i+1:]...)
			return
		}
	}
}

// Files returns all registered files sorted by path.
func (g *Graph) Files() []FileRecord {
	out := make([]FileRecord, 0, len(g.files))
	for _, f := range g.files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// File returns a single file record, and whether it exists.
func (g *Graph) File(path string) (FileRecord, bool) {
	f, ok := g.files[path]
	return f, ok
}

// SymbolsForFile returns symbols defined in the given file.
func (g *Graph) SymbolsForFile(path string) []Symbol {
	return g.symbols[path]
}

// ImportsOf returns the list of file paths that the given file imports.
func (g *Graph) ImportsOf(path string) []string {
	edges := g.edges[path]
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e.Type == "IMPORTS" {
			out = append(out, e.TargetPath)
		}
	}
	sort.Strings(out)
	return out
}

// UpstreamOf returns files that import the given file (reverse edges).
func (g *Graph) UpstreamOf(path string) []string {
	out := make([]string, len(g.reverse[path]))
	copy(out, g.reverse[path])
	sort.Strings(out)
	return out
}

// AllEdges returns all edges in the graph.
func (g *Graph) AllEdges() []Edge {
	var out []Edge
	for _, edges := range g.edges {
		out = append(out, edges...)
	}
	return out
}

// Stats returns summary counts.
func (g *Graph) Stats() (files, symbols, edges int) {
	files = len(g.files)
	for _, s := range g.symbols {
		symbols += len(s)
	}
	for _, e := range g.edges {
		edges += len(e)
	}
	return
}

// EdgesOfType returns all edges of a specific type (e.g. "CALLS", "IMPORTS").
func (g *Graph) EdgesOfType(edgeType string) []Edge {
	var out []Edge
	for _, edges := range g.edges {
		for _, e := range edges {
			if e.Type == edgeType {
				out = append(out, e)
			}
		}
	}
	return out
}

// AllSymbols returns all symbols across all files.
func (g *Graph) AllSymbols() []Symbol {
	var out []Symbol
	for _, syms := range g.symbols {
		out = append(out, syms...)
	}
	return out
}

// SetCommunity assigns a community to a file.
func (g *Graph) SetCommunity(filePath string, c CommunityInfo) {
	g.communities[filePath] = c
}

// CommunityOf returns the community assignment for a file, and whether one exists.
func (g *Graph) CommunityOf(filePath string) (CommunityInfo, bool) {
	c, ok := g.communities[filePath]
	return c, ok
}

// AllCommunities returns a deduplicated map of community ID to CommunityInfo.
func (g *Graph) AllCommunities() map[int]CommunityInfo {
	out := make(map[int]CommunityInfo)
	for _, c := range g.communities {
		out[c.ID] = c
	}
	return out
}
