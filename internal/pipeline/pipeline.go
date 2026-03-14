package pipeline

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/ignore"
	"github.com/neur0map/prowl/internal/output"
	"github.com/neur0map/prowl/internal/parser"
	"github.com/neur0map/prowl/internal/store"
)

// Index runs the full indexing pipeline on a project directory.
func Index(projectDir string) error {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	prowlDir := filepath.Join(absDir, ".prowl")
	contextDir := filepath.Join(prowlDir, "context")
	dbPath := filepath.Join(prowlDir, "prowl.db")

	os.MkdirAll(prowlDir, 0755)

	// Open SQLite store
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	ig := ignore.New(absDir)
	g := graph.New()

	// Phase 1: Walk the file tree
	fmt.Println("Phase 1: Scanning file tree...")
	var sourceFiles []struct {
		relPath string
		content []byte
	}

	filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		rel, _ := filepath.Rel(absDir, path)
		rel = filepath.ToSlash(rel)

		if rel == "." {
			return nil
		}

		if ig.ShouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Only process files we have a parser for
		lang := parser.DetectLanguage(rel)
		if lang == "" {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		sourceFiles = append(sourceFiles, struct {
			relPath string
			content []byte
		}{relPath: rel, content: content})

		return nil
	})

	fmt.Printf("  Found %d source files\n", len(sourceFiles))

	// Phase 2: Parse symbols (cache results for phase 3)
	fmt.Println("Phase 2: Extracting symbols...")
	type parseEntry struct {
		relPath string
		result  *parser.ParseResult
	}
	var parsed []parseEntry

	for _, sf := range sourceFiles {
		hash := fmt.Sprintf("%x", xxhash.Sum64(sf.content))
		g.AddFile(graph.FileRecord{Path: sf.relPath, Hash: hash})

		result, err := parser.ParseFile(sf.relPath, sf.content)
		if err != nil || result == nil {
			continue
		}

		for _, sym := range result.Symbols {
			g.AddSymbol(sym)
		}

		parsed = append(parsed, parseEntry{relPath: sf.relPath, result: result})
	}

	// Build a set of known file paths for import resolution
	knownPaths := make(map[string]bool)
	for _, f := range g.Files() {
		knownPaths[f.Path] = true
	}

	// Phase 3: Resolve imports (using cached parse results)
	fmt.Println("Phase 3: Resolving imports...")
	for _, pe := range parsed {
		for _, rawImport := range pe.result.Imports {
			resolved := resolveImport(pe.relPath, rawImport, knownPaths)
			if resolved != "" {
				g.AddEdge(graph.Edge{
					SourcePath: pe.relPath,
					TargetPath: resolved,
					Type:       "IMPORTS",
				})
			}
		}
	}

	// Persist to SQLite
	fmt.Println("Persisting to SQLite...")
	for _, f := range g.Files() {
		fid, err := st.UpsertFile(f.Path, f.Hash)
		if err != nil {
			continue
		}
		st.DeleteSymbolsForFile(fid)
		st.InsertSymbols(fid, g.SymbolsForFile(f.Path))
		st.DeleteEdgesFromFile(fid)
	}
	for _, e := range g.AllEdges() {
		srcID, _ := st.FileID(e.SourcePath)
		tgtID, _ := st.FileID(e.TargetPath)
		if srcID > 0 && tgtID > 0 {
			st.UpsertEdge(srcID, tgtID, e.Type)
		}
	}

	// Write filesystem output
	fmt.Println("Writing .prowl/context/...")
	if err := output.WriteContext(contextDir, g); err != nil {
		return fmt.Errorf("write context: %w", err)
	}

	fCount, sCount, eCount := g.Stats()
	fmt.Printf("Done! %d files, %d symbols, %d edges\n", fCount, sCount, eCount)
	return nil
}

// resolveImport maps a raw import specifier to a project file path.
// Returns "" if the import is external (not in the project).
func resolveImport(fromFile, specifier string, known map[string]bool) string {
	// Only resolve relative imports for now
	if !strings.HasPrefix(specifier, ".") {
		return ""
	}

	dir := filepath.Dir(fromFile)
	joined := filepath.Join(dir, specifier)
	joined = filepath.ToSlash(filepath.Clean(joined))

	// Try exact match, then with extensions
	candidates := []string{
		joined,
		joined + ".ts", joined + ".tsx",
		joined + ".js", joined + ".jsx",
		joined + ".go",
		joined + "/index.ts", joined + "/index.tsx",
		joined + "/index.js", joined + "/index.jsx",
	}

	for _, c := range candidates {
		if known[c] {
			return c
		}
	}

	return ""
}
