package output

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/graph"
)

// WriteContext writes the .prowl/context/ filesystem from the in-memory graph.
// contextDir is the root of the context output (e.g. /project/.prowl/context).
func WriteContext(contextDir string, g *graph.Graph) error {
	// Clean previous output
	os.RemoveAll(contextDir)

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

		indexLines = append(indexLines, f.Path)
	}

	// _meta/index.txt
	metaDir := filepath.Join(contextDir, "_meta")
	os.MkdirAll(metaDir, 0755)
	return writeLines(filepath.Join(metaDir, "index.txt"), indexLines)
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
