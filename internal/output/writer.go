package output

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/graph"
)

// CommunityData is a simplified community for output (avoids circular import).
type CommunityData struct {
	ID      int
	Name    string
	Members []string
}

// ProcessData is a simplified process for output (avoids circular import).
type ProcessData struct {
	Name  string
	Entry string
	Steps []string
	Type  string
}

// WriteContext writes the .prowl/context/ filesystem from the in-memory graph.
// contextDir is the root of the context output (e.g. /project/.prowl/context).
// communities and processes can be nil if phases 6-7 haven't run.
func WriteContext(contextDir string, g *graph.Graph, communities []CommunityData, processes []ProcessData) error {
	// Clean previous output
	os.RemoveAll(contextDir)

	// Build calls/callers maps from graph CALLS edges
	callsMap := map[string][]string{}   // file -> files it calls
	callersMap := map[string][]string{} // file -> files that call it
	for _, e := range g.EdgesOfType("CALLS") {
		callsMap[e.SourcePath] = append(callsMap[e.SourcePath], e.TargetPath)
		callersMap[e.TargetPath] = append(callersMap[e.TargetPath], e.SourcePath)
	}
	// Deduplicate and sort
	for k, v := range callsMap {
		callsMap[k] = uniqueSorted(v)
	}
	for k, v := range callersMap {
		callersMap[k] = uniqueSorted(v)
	}

	// Build file-to-community lookup
	fileToCommunity := map[string]CommunityData{}
	for _, c := range communities {
		for _, member := range c.Members {
			fileToCommunity[member] = c
		}
	}

	files := g.Files()
	var indexLines []string

	for _, f := range files {
		fileDir := filepath.Join(contextDir, f.Path)
		if err := os.MkdirAll(fileDir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", fileDir, err)
		}

		syms := g.SymbolsForFile(f.Path)

		// .exports — only exported symbols
		if err := writeExports(fileDir, syms); err != nil {
			return err
		}

		// .signatures — all symbols with their signatures
		if err := writeSignatures(fileDir, syms); err != nil {
			return err
		}

		// .imports
		imports := g.ImportsOf(f.Path)
		if err := writeLines(filepath.Join(fileDir, ".imports"), imports); err != nil {
			return err
		}

		// .upstream (reverse imports)
		upstream := g.UpstreamOf(f.Path)
		if err := writeLines(filepath.Join(fileDir, ".upstream"), upstream); err != nil {
			return err
		}

		// .calls — files this file's functions call into
		if err := writeLines(filepath.Join(fileDir, ".calls"), callsMap[f.Path]); err != nil {
			return err
		}

		// .callers — files that call into this file's functions
		if err := writeLines(filepath.Join(fileDir, ".callers"), callersMap[f.Path]); err != nil {
			return err
		}

		// .community — community assignment
		if c, ok := fileToCommunity[f.Path]; ok {
			communityLine := fmt.Sprintf("%s (id: %d)", c.Name, c.ID)
			if err := writeLines(filepath.Join(fileDir, ".community"), []string{communityLine}); err != nil {
				return err
			}
		} else {
			if err := writeLines(filepath.Join(fileDir, ".community"), nil); err != nil {
				return err
			}
		}

		indexLines = append(indexLines, f.Path)
	}

	// _meta/index.txt
	metaDir := filepath.Join(contextDir, "_meta")
	os.MkdirAll(metaDir, 0755)
	if err := writeLines(filepath.Join(metaDir, "index.txt"), indexLines); err != nil {
		return err
	}

	// _meta/communities.txt
	if err := writeCommunities(metaDir, communities); err != nil {
		return err
	}

	// _meta/processes.txt
	return writeProcesses(metaDir, processes)
}

// WriteFileContext writes context files for a single file.
// Used by the daemon for incremental updates.
func WriteFileContext(contextDir string, g *graph.Graph, filePath string) error {
	fileDir := filepath.Join(contextDir, filePath)
	os.MkdirAll(fileDir, 0755)

	syms := g.SymbolsForFile(filePath)
	if err := writeExports(fileDir, syms); err != nil {
		return err
	}
	if err := writeSignatures(fileDir, syms); err != nil {
		return err
	}
	if err := writeLines(filepath.Join(fileDir, ".imports"), g.ImportsOf(filePath)); err != nil {
		return err
	}
	return writeLines(filepath.Join(fileDir, ".upstream"), g.UpstreamOf(filePath))
}

// WriteUpstream rewrites just the .upstream file for a single file.
func WriteUpstream(contextDir string, filePath string, upstream []string) error {
	fileDir := filepath.Join(contextDir, filePath)
	os.MkdirAll(fileDir, 0755)
	return writeLines(filepath.Join(fileDir, ".upstream"), upstream)
}

func kindAbbrev(kind string) string {
	switch kind {
	case "function":
		return "func"
	default:
		return kind
	}
}

func writeExports(dir string, syms []graph.Symbol) error {
	var lines []string
	for _, s := range syms {
		if s.IsExported {
			lines = append(lines, fmt.Sprintf("%s %s (line %d)", kindAbbrev(s.Kind), s.Name, s.StartLine))
		}
	}
	return writeLines(filepath.Join(dir, ".exports"), lines)
}

func writeSignatures(dir string, syms []graph.Symbol) error {
	var lines []string
	for _, s := range syms {
		sig := s.Signature
		if sig == "" {
			sig = fmt.Sprintf("%s %s", s.Kind, s.Name)
		}
		lines = append(lines, fmt.Sprintf("%s (line %d)", sig, s.StartLine))
	}
	return writeLines(filepath.Join(dir, ".signatures"), lines)
}

func writeLines(path string, lines []string) error {
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// writeCommunities writes _meta/communities.txt.
func writeCommunities(metaDir string, communities []CommunityData) error {
	var lines []string
	for _, c := range communities {
		lines = append(lines, fmt.Sprintf("%s (%d members)", c.Name, len(c.Members)))
	}
	return writeLines(filepath.Join(metaDir, "communities.txt"), lines)
}

// writeProcesses writes _meta/processes.txt.
func writeProcesses(metaDir string, processes []ProcessData) error {
	var lines []string
	for _, p := range processes {
		lines = append(lines, fmt.Sprintf("process: %s [%s]", p.Name, p.Type))
		lines = append(lines, fmt.Sprintf("entry: %s", p.Entry))
		lines = append(lines, "steps:")
		for _, step := range p.Steps {
			lines = append(lines, fmt.Sprintf("  -> %s", step))
		}
		lines = append(lines, "") // blank line between processes
	}
	return writeLines(filepath.Join(metaDir, "processes.txt"), lines)
}

// uniqueSorted deduplicates and sorts a string slice.
func uniqueSorted(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(s))
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
