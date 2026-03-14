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
	"github.com/neur0map/prowl/internal/community"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/ignore"
	"github.com/neur0map/prowl/internal/output"
	"github.com/neur0map/prowl/internal/parser"
	"github.com/neur0map/prowl/internal/pipeline"
	"github.com/neur0map/prowl/internal/process"
	"github.com/neur0map/prowl/internal/resolve"
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
	embedder   *lazyEmbedder
	idle       *idleTracker
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
	modelDir := filepath.Join(os.Getenv("HOME"), ".prowl", "models")

	d := &Daemon{
		projectDir: absDir,
		prowlDir:   prowlDir,
		contextDir: filepath.Join(prowlDir, "context"),
		debounce:   debounce,
		watcher:    w,
		store:      st,
		ig:         ig,
		memGraph:   graph.New(),
		stop:       make(chan struct{}),
	}
	d.embedder = newLazyEmbedder(modelDir, 10*time.Minute)
	d.idle = newIdleTracker(30*time.Second, func() {
		d.runGlobalPhases()
	})

	return d, nil
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

			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// Check if file still exists (atomic saves use rename-and-replace)
				absPath := filepath.Join(d.projectDir, filepath.FromSlash(rel))
				if _, statErr := os.Stat(absPath); os.IsNotExist(statErr) {
					// Cancel any pending debounce for this file
					pendingMu.Lock()
					if t, exists := pending[rel]; exists {
						t.Stop()
						delete(pending, rel)
					}
					pendingMu.Unlock()
					d.deleteFile(rel)
				}
				continue
			}

			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
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
	d.idle.Stop()
	d.embedder.Close()
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
	}

	// Load ALL edge types (IMPORTS, CALLS, EXTENDS, IMPLEMENTS)
	edges, _ := d.store.AllEdges()
	for _, e := range edges {
		d.memGraph.AddEdge(e)
	}
}

// runGlobalPhases runs community detection and process detection across the entire graph.
func (d *Daemon) runGlobalPhases() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Phase 6: Community detection
	communities := community.DetectCommunities(d.memGraph)
	d.store.ClearCommunities()
	commMap := make(map[string]int)
	for _, c := range communities {
		d.store.InsertCommunity(c.ID, c.Name, c.Label)
		for _, member := range c.Members {
			fid, _ := d.store.FileID(member)
			if fid > 0 {
				d.store.InsertCommunityMember(fid, c.ID)
			}
			d.memGraph.SetCommunity(member, graph.CommunityInfo{
				ID: c.ID, Name: c.Name, Label: c.Label,
			})
			commMap[member] = c.ID
		}
	}

	// Phase 7: Process detection
	processes := process.DetectProcesses(d.memGraph, commMap)

	// Convert for output
	comData := make([]output.CommunityData, len(communities))
	for i, c := range communities {
		comData[i] = output.CommunityData{ID: c.ID, Name: c.Name, Members: c.Members}
	}
	procData := make([]output.ProcessData, len(processes))
	for i, p := range processes {
		procData[i] = output.ProcessData{
			Name:  p.Name,
			Type:  p.Type,
			Entry: p.Entry,
			Steps: p.Steps,
		}
	}

	// Rewrite _meta files
	output.WriteMetaCommunities(d.contextDir, comData)
	output.WriteMetaProcesses(d.contextDir, procData)

	// Rewrite .community for all files
	for _, f := range d.memGraph.Files() {
		if ci, ok := d.memGraph.CommunityOf(f.Path); ok {
			output.WriteCommunity(d.contextDir, f.Path, fmt.Sprintf("%s (ID: %d)", ci.Name, ci.ID))
		}
	}

	fmt.Println("[daemon] global phases complete — communities and processes updated")
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

	// Check if this is a new file (for index update)
	_, isExistingFile := d.memGraph.File(relPath)

	// Snapshot old edges for cascading updates (before ReplaceFile clears them)
	oldImports := d.memGraph.ImportsOf(relPath)
	oldCallEdges := d.memGraph.EdgesFromFile(relPath, "CALLS")
	oldCallTargets := make(map[string]bool)
	for _, e := range oldCallEdges {
		oldCallTargets[e.TargetPath] = true
	}

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

	// Phase 4: Resolve calls + heritage for this file
	callEdges := resolve.ResolveCallsForFile(d.memGraph, relPath, result.Calls, result.Heritage)
	for _, e := range callEdges {
		d.memGraph.AddEdge(e)
	}

	// Track new call targets for cascading
	newCallTargets := make(map[string]bool)
	for _, e := range callEdges {
		if e.Type == "CALLS" {
			newCallTargets[e.TargetPath] = true
		}
	}

	// Update SQLite
	fid, _ := d.store.UpsertFile(relPath, newHash)
	d.store.DeleteSymbolsForFile(fid)
	d.store.InsertSymbols(fid, result.Symbols)
	d.store.DeleteEdgesFromFile(fid)

	// Persist ALL edges (imports + calls + heritage)
	for _, imp := range newImports {
		tgtID, _ := d.store.FileID(imp)
		if tgtID > 0 {
			d.store.UpsertEdge(fid, tgtID, "IMPORTS")
		}
	}
	for _, e := range callEdges {
		tgtID, _ := d.store.FileID(e.TargetPath)
		if tgtID > 0 {
			d.store.UpsertEdgeWithConfidence(fid, tgtID, e.Type, e.Confidence)
		}
	}

	// Phase 8: Re-embed if signatures changed
	d.reembedFile(fid, relPath)

	// Rewrite context for the changed file
	output.WriteFileContext(d.contextDir, d.memGraph, relPath)

	// Write .calls for this file
	newCallEdges := d.memGraph.EdgesFromFile(relPath, "CALLS")
	callPaths := make([]string, 0, len(newCallEdges))
	for _, e := range newCallEdges {
		callPaths = append(callPaths, e.TargetPath)
	}
	output.WriteCalls(d.contextDir, relPath, callPaths)

	// Cascade .upstream updates
	affectedUpstream := make(map[string]bool)
	for _, imp := range oldImports {
		affectedUpstream[imp] = true
	}
	for _, imp := range newImports {
		affectedUpstream[imp] = true
	}
	for path := range affectedUpstream {
		upstream := d.memGraph.UpstreamOf(path)
		output.WriteUpstream(d.contextDir, path, upstream)
	}

	// Cascade .callers updates — use store.CallersOf for efficiency
	affectedCallers := make(map[string]bool)
	for t := range oldCallTargets {
		affectedCallers[t] = true
	}
	for t := range newCallTargets {
		affectedCallers[t] = true
	}
	for path := range affectedCallers {
		callers, _ := d.store.CallersOf(path)
		output.WriteCallers(d.contextDir, path, callers)
	}

	// Update _meta/index.txt if this is a new file
	if !isExistingFile {
		var allPaths []string
		for _, f := range d.memGraph.Files() {
			allPaths = append(allPaths, f.Path)
		}
		output.WriteIndexFile(d.contextDir, allPaths)
	}

	// Mark dirty for global phases
	d.idle.MarkDirty()

	fmt.Printf("[daemon] updated %s (%d symbols, %d imports, %d calls)\n",
		relPath, len(result.Symbols), len(newImports), len(callEdges))
}

// deleteFile removes a file from the graph, store, and context directory,
// then cascades updates to callers/upstream context files.
func (d *Daemon) deleteFile(relPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Snapshot edges BEFORE RemoveFile (which clears them)
	oldCallEdges := d.memGraph.EdgesFromFile(relPath, "CALLS")
	oldImports := d.memGraph.ImportsOf(relPath)

	// Snapshot reverse callers BEFORE store.DeleteFile (CASCADE deletes these edges)
	reverseCallers, _ := d.store.CallersOf(relPath)

	// Remove from in-memory graph (clears all edges in both directions)
	d.memGraph.RemoveFile(relPath)

	// Remove from SQLite (CASCADE handles symbols, edges, embeddings, community_members)
	d.store.DeleteFile(relPath)

	// Remove context directory
	contextPath := filepath.Join(d.contextDir, relPath)
	os.RemoveAll(contextPath)

	// Update .callers for files the deleted file previously called
	// Edges already deleted from store by CASCADE, so remaining callers are exactly right
	for _, e := range oldCallEdges {
		callers, _ := d.store.CallersOf(e.TargetPath)
		output.WriteCallers(d.contextDir, e.TargetPath, callers)
	}

	// Update .calls for files that called INTO the deleted file (reverse direction)
	// Their CALLS edges to the deleted file are gone from both graph and store,
	// so re-reading from graph gives the correct remaining targets
	for _, caller := range reverseCallers {
		callEdges := d.memGraph.EdgesFromFile(caller, "CALLS")
		callPaths := make([]string, 0, len(callEdges))
		for _, e := range callEdges {
			callPaths = append(callPaths, e.TargetPath)
		}
		output.WriteCalls(d.contextDir, caller, callPaths)
	}

	// Update .upstream for files the deleted file imported
	for _, imp := range oldImports {
		upstream := d.memGraph.UpstreamOf(imp)
		output.WriteUpstream(d.contextDir, imp, upstream)
	}

	// Update _meta/index.txt
	var allPaths []string
	for _, f := range d.memGraph.Files() {
		allPaths = append(allPaths, f.Path)
	}
	output.WriteIndexFile(d.contextDir, allPaths)

	// Mark dirty for global recomputation
	d.idle.MarkDirty()

	fmt.Printf("[daemon] deleted %s\n", relPath)
}

// reembedFile re-embeds a file if its signature text has changed.
func (d *Daemon) reembedFile(fid int64, relPath string) {
	syms := d.memGraph.SymbolsForFile(relPath)
	if len(syms) == 0 {
		return
	}

	var sigParts []string
	for _, s := range syms {
		if s.Signature != "" {
			sigParts = append(sigParts, s.Signature)
		} else {
			sigParts = append(sigParts, fmt.Sprintf("%s %s", s.Kind, s.Name))
		}
	}
	sigText := strings.Join(sigParts, "\n")
	textHash := fmt.Sprintf("%x", xxhash.Sum64String(sigText))

	storedHash, _ := d.store.EmbeddingTextHash(fid)
	if storedHash == textHash {
		return
	}

	emb, err := d.embedder.Get()
	if err != nil || emb == nil {
		return // model not available, skip silently
	}

	vecs, err := emb.Encode([]string{sigText})
	if err != nil || len(vecs) == 0 {
		return
	}

	d.store.UpsertEmbedding(fid, vecs[0], textHash)
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
