package daemon

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/fsnotify/fsnotify"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/ignore"
	"github.com/neur0map/prowl/internal/output"
	"github.com/neur0map/prowl/internal/parser"
	"github.com/neur0map/prowl/internal/pipeline"
	"github.com/neur0map/prowl/internal/store"
)

// Daemon watches for file changes and incrementally updates the index.
type Daemon struct {
	projectDir string
	prowlDir   string
	contextDir string
	debounce   time.Duration
	watcher    *fsnotify.Watcher
	store      *store.Store
	ig         *ignore.Checker
	memGraph   *graph.Graph
	stop       chan struct{}
	mu         sync.Mutex
}

// New creates a daemon for the given project directory.
func New(projectDir string, debounce time.Duration) (*Daemon, error) {
	absDir, _ := filepath.Abs(projectDir)
	prowlDir := filepath.Join(absDir, ".prowl")
	dbPath := filepath.Join(prowlDir, "prowl.db")

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		st.Close()
		return nil, err
	}

	ig := ignore.New(absDir)

	return &Daemon{
		projectDir: absDir,
		prowlDir:   prowlDir,
		contextDir: filepath.Join(prowlDir, "context"),
		debounce:   debounce,
		watcher:    w,
		store:      st,
		ig:         ig,
		memGraph:   graph.New(),
		stop:       make(chan struct{}),
	}, nil
}

// Run starts the watcher loop. Blocks until Stop is called.
func (d *Daemon) Run() {
	// Add all directories to the watcher
	filepath.WalkDir(d.projectDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(d.projectDir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if rel != "" && d.ig.ShouldIgnore(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			d.watcher.Add(path)
		}
		return nil
	})

	// Load existing graph state from SQLite into memory
	d.loadGraphFromStore()

	// Debounce map: path -> timer
	pending := make(map[string]*time.Timer)
	var pendingMu sync.Mutex

	for {
		select {
		case event, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			rel, err := filepath.Rel(d.projectDir, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)

			if d.ig.ShouldIgnore(rel) {
				continue
			}

			lang := parser.DetectLanguage(rel)
			if lang == "" {
				continue
			}

			pendingMu.Lock()
			if t, exists := pending[rel]; exists {
				t.Stop()
			}
			pending[rel] = time.AfterFunc(d.debounce, func() {
				pendingMu.Lock()
				delete(pending, rel)
				pendingMu.Unlock()
				d.processFile(rel)
			})
			pendingMu.Unlock()

		case _, ok := <-d.watcher.Errors:
			if !ok {
				return
			}

		case <-d.stop:
			return
		}
	}
}

// Stop signals the daemon to shut down.
func (d *Daemon) Stop() {
	close(d.stop)
	d.watcher.Close()
	d.store.Close()
}

func (d *Daemon) loadGraphFromStore() {
	files, _ := d.store.AllFiles()
	for _, path := range files {
		hash, _ := d.store.FileHash(path)
		d.memGraph.AddFile(graph.FileRecord{Path: path, Hash: hash})

		syms, _ := d.store.SymbolsForFile(path)
		for _, s := range syms {
			d.memGraph.AddSymbol(s)
		}

		imports, _ := d.store.ImportsOf(path)
		for _, imp := range imports {
			d.memGraph.AddEdge(graph.Edge{SourcePath: path, TargetPath: imp, Type: "IMPORTS"})
		}
	}
}

func (d *Daemon) processFile(relPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	absPath := filepath.Join(d.projectDir, filepath.FromSlash(relPath))
	content, err := os.ReadFile(absPath)
	if err != nil {
		return
	}

	// Check hash — skip if unchanged
	newHash := fmt.Sprintf("%x", xxhash.Sum64(content))
	if f, ok := d.memGraph.File(relPath); ok && f.Hash == newHash {
		return
	}

	// Compute old upstream set before changes
	oldImports := d.memGraph.ImportsOf(relPath)

	// Clear old data and re-add
	d.memGraph.ReplaceFile(relPath, graph.FileRecord{Path: relPath, Hash: newHash})

	// Re-parse
	result, err := parser.ParseFile(relPath, content)
	if err != nil || result == nil {
		return
	}

	for _, sym := range result.Symbols {
		d.memGraph.AddSymbol(sym)
	}

	// Resolve imports
	knownPaths := make(map[string]bool)
	for _, f := range d.memGraph.Files() {
		knownPaths[f.Path] = true
	}

	var newImports []string
	for _, rawImport := range result.Imports {
		resolved := resolveImport(relPath, rawImport, knownPaths)
		if resolved != "" {
			d.memGraph.AddEdge(graph.Edge{SourcePath: relPath, TargetPath: resolved, Type: "IMPORTS"})
			newImports = append(newImports, resolved)
		}
	}

	// Update SQLite
	fid, _ := d.store.UpsertFile(relPath, newHash)
	d.store.DeleteSymbolsForFile(fid)
	d.store.InsertSymbols(fid, result.Symbols)
	d.store.DeleteEdgesFromFile(fid)
	for _, imp := range newImports {
		tgtID, _ := d.store.FileID(imp)
		if tgtID > 0 {
			d.store.UpsertEdge(fid, tgtID, "IMPORTS")
		}
	}

	// Rewrite context for the changed file
	output.WriteFileContext(d.contextDir, d.memGraph, relPath)

	// Find files whose .upstream changed and rewrite them
	affectedFiles := make(map[string]bool)
	for _, imp := range oldImports {
		affectedFiles[imp] = true
	}
	for _, imp := range newImports {
		affectedFiles[imp] = true
	}
	for path := range affectedFiles {
		upstream := d.memGraph.UpstreamOf(path)
		output.WriteUpstream(d.contextDir, path, upstream)
	}

	fmt.Printf("[daemon] updated %s (%d symbols, %d imports)\n", relPath, len(result.Symbols), len(newImports))
}

// resolveImport maps a raw import specifier to a project file path.
// Duplicated from pipeline — extract to shared package in M2.
func resolveImport(fromFile, specifier string, known map[string]bool) string {
	if !strings.HasPrefix(specifier, ".") {
		return ""
	}
	dir := filepath.Dir(fromFile)
	joined := filepath.ToSlash(filepath.Clean(filepath.Join(dir, specifier)))
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

// runIndex is used by tests to trigger a full index.
func runIndex(dir string) error {
	return pipeline.Index(dir)
}
