// Package query implements the structural queries Prowl exposes to
// agents. All results are deterministic and carry file:line provenance.
package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/store"
)

// Querier answers structural queries against an index.
type Querier struct {
	s                *store.Store
	inf              assist.Inferencer // optional; when set, SimilarCode is hybrid semantic
	requirePublished bool
	readGuard        store.ReadGuard
	// afterFirstRead is a package-private deterministic concurrency test seam.
	afterFirstRead func()
	// afterOverviewRead is a package-private deterministic cancellation seam.
	afterOverviewRead func()
}

// New wraps a store for structural (FTS-only) queries.
func New(s *store.Store) *Querier { return &Querier{s: s} }

// NewWithAssist wraps a store with a local inferencer for hybrid semantic search.
func NewWithAssist(s *store.Store, inf assist.Inferencer) *Querier {
	return &Querier{s: s, inf: inf}
}

// RequirePublishedGeneration makes every exported operation reject an index
// generation that a failed refresh deliberately left unpublished.
func (q *Querier) RequirePublishedGeneration() *Querier {
	q.requirePublished = true
	return q
}

// WithReadGuard pins each exported logical operation to one committed
// generation. Application assembly supplies a shared cross-process file lock;
// low-level callers remain compatible when no guard is configured.
func (q *Querier) WithReadGuard(guard store.ReadGuard) *Querier {
	q.readGuard = guard
	return q
}

func (q *Querier) beginRead(ctx context.Context) (func(), error) {
	release := func() {}
	if q.readGuard != nil {
		var err error
		release, err = q.readGuard(ctx)
		if err != nil {
			return nil, err
		}
	}
	if err := q.ready(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (q *Querier) ready() error {
	if q.requirePublished {
		return q.s.RequirePublishedGeneration()
	}
	return nil
}

// DefaultLimit bounds result sizes.
const DefaultLimit = 50

func (q *Querier) fileID(path string) (int64, bool, error) {
	f, ok, err := q.s.GetFileByPath(path)
	if err != nil || !ok {
		return 0, false, err
	}
	return f.ID, true, nil
}

// FindSymbol returns exact-name matches first, then FTS matches, then a
// substring fallback that catches camelCase/snake_case components (e.g.
// "cloud" finding "updateCloudClient") which the FTS tokenizer keeps whole.
// Results are then stably ranked so project code definitions outrank
// config/doc entries (settings, headings) and project files outrank vendored
// or generated ones, while the match-quality order is kept within each tier.
func (q *Querier) FindSymbol(name string) ([]store.SymbolHit, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	return q.findSymbolLocked(name)
}

// findSymbolLocked is FindSymbol's ranked resolution without acquiring the read
// guard, so another locked operation (Definition) can reuse it without nesting.
func (q *Querier) findSymbolLocked(name string) ([]store.SymbolHit, error) {
	exact, err := q.s.SymbolsByName(name, DefaultLimit)
	if err != nil {
		return nil, err
	}
	if q.afterFirstRead != nil {
		q.afterFirstRead()
	}
	seen := make(map[int64]bool, len(exact))
	out := make([]store.SymbolHit, 0, len(exact))
	add := func(hits []store.SymbolHit) {
		for _, h := range hits {
			if !seen[h.ID] {
				seen[h.ID] = true
				out = append(out, h)
			}
		}
	}
	add(exact)
	if fts, err := q.s.SearchSymbols(name, DefaultLimit); err == nil {
		add(fts)
	}
	if len(out) < DefaultLimit {
		if sub, err := q.s.SymbolsBySubstring(name, DefaultLimit); err == nil {
			add(sub)
		}
	}
	if len(out) == 0 {
		return q.fuzzySymbols(name), nil // last resort: a typo or approximate name
	}
	sort.SliceStable(out, func(i, j int) bool {
		return findRank(out[i]) < findRank(out[j])
	})
	return out, nil
}

// fuzzySymbols is find's last resort: when no exact, full-text, or substring
// match exists, return symbols whose name is within a small edit distance of the
// query (a typo or an approximate recollection), closest first. It runs only on
// a total miss, so it never displaces a real match, and skips very short queries
// where fuzzy matching is mostly noise.
func (q *Querier) fuzzySymbols(name string) []store.SymbolHit {
	term := strings.ToLower(strings.TrimSpace(name))
	n := len([]rune(term))
	if n < 4 {
		return nil
	}
	maxDist := 1
	if n >= 8 {
		maxDist = 2
	}
	names, err := q.s.DistinctSymbolNamesByLength(n-maxDist, n+maxDist)
	if err != nil {
		return nil
	}
	type scored struct {
		name string
		d    int
	}
	var near []scored
	for _, cand := range names {
		if d := osaDistance(term, strings.ToLower(cand)); d >= 1 && d <= maxDist {
			near = append(near, scored{cand, d})
		}
	}
	sort.SliceStable(near, func(i, j int) bool {
		if near[i].d != near[j].d {
			return near[i].d < near[j].d
		}
		return near[i].name < near[j].name
	})
	seen := map[int64]bool{}
	var out []store.SymbolHit
	for _, s := range near {
		hits, err := q.s.SymbolsByName(s.name, DefaultLimit)
		if err != nil {
			continue
		}
		for _, h := range hits {
			if !seen[h.ID] {
				seen[h.ID] = true
				out = append(out, h)
			}
		}
		if len(out) >= DefaultLimit {
			break
		}
	}
	return out
}

// osaDistance is the optimal string alignment (restricted Damerau-Levenshtein)
// distance between a and b: the fewest inserts, deletes, substitutions, and
// adjacent transpositions to turn one into the other.
func osaDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev2 := make([]int, lb+1)
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if v := curr[j-1] + 1; v < m {
				m = v
			}
			if v := prev[j-1] + cost; v < m {
				m = v
			}
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				if v := prev2[j-2] + 1; v < m {
					m = v
				}
			}
			curr[j] = m
		}
		prev2, prev, curr = prev, curr, prev2
	}
	return prev[lb]
}

// Usages is where a symbol is referenced: graph edges for config/resource
// references, or, for code symbols (which have no language-level call graph),
// full-text call sites of the symbol's name.
type Usages struct {
	Symbol    string     `json:"symbol"`
	Edges     []EdgeView `json:"edges,omitempty"`
	CallSites []CallSite `json:"call_sites,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// CallSite is a precise source location where a symbol name appears: the file,
// the line, that line's text, and the enclosing function or type (`in`) when
// the usage sits inside one, so the agent sees which functions call a symbol
// instead of a 40-line chunk.
type CallSite struct {
	File string `json:"file"`
	In   string `json:"in"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// FindReferences returns where a symbol is used. It prefers symbol-reference
// graph edges (config variables, shared resources); when there are none -- the
// usual case for code, since prowl has no call graph -- it falls back to
// full-text usages of the symbol name, excluding the definition's own chunk, so
// `find <name>` -> `references <id>` answers "what calls this" instead of empty.
func (q *Querier) FindReferences(symbolID int64) (Usages, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Usages{}, err
	}
	defer release()
	var u Usages
	sym, ok, err := q.s.SymbolByID(symbolID)
	if err != nil {
		return u, err
	}
	if ok {
		u.Symbol = sym.Name
	}
	edges, err := q.s.IncomingEdges("symbol", symbolID)
	if err != nil {
		return u, err
	}
	if len(edges) > 0 {
		u.Edges = edgeViews(edges)
		return u, nil
	}
	if !ok || sym.Name == "" {
		return u, nil
	}
	// A callable's blast radius is its call sites, so match the name invoked with
	// `(`, not every bare mention (a config field or struct field that shares the
	// name is not a caller). Non-callable symbols keep the whole-word match.
	callable := sym.Kind == "function" || sym.Kind == "method"
	matches := containsWord
	if callable {
		matches = containsCall
	}
	chunks, err := q.s.SearchChunkText(sym.Name, DefaultLimit)
	if err != nil {
		return u, err
	}
	const maxSites = 40
	spansByFile := make(map[string][]store.SymbolSpan)
	spansFor := func(file string) []store.SymbolSpan {
		s, ok := spansByFile[file]
		if !ok {
			s, _ = q.s.SymbolSpans(file)
			spansByFile[file] = s
		}
		return s
	}
collect:
	for _, ch := range chunks {
		spans := spansFor(ch.File)
		for i, ln := range strings.Split(ch.Text, "\n") {
			abs := ch.StartLine + i
			if ch.File == sym.File && sym.Line <= abs && abs <= sym.EndLine {
				continue // the definition's own body, not a usage
			}
			if !matches(ln, sym.Name) {
				continue // not a usage (or, for a callable, not a call)
			}
			if definesName(spans, abs, sym.Name) {
				continue // a same-named definition elsewhere (e.g. an override), not a call
			}
			in := enclosingName(spans, abs)
			if in == sym.Name {
				in = ""
			}
			u.CallSites = append(u.CallSites, CallSite{File: ch.File, In: in, Line: abs, Text: strings.TrimSpace(ln)})
			if len(u.CallSites) >= maxSites {
				break collect
			}
		}
	}
	if len(u.CallSites) > 0 {
		u.Note = "call_sites are name usages found in source (no language call graph); some may be comments or docs"
		if callable {
			u.Note = "call_sites are where the name is invoked with `(` (no language call graph; a comment or a same-named call elsewhere may slip in)"
		}
	}
	return u, nil
}

// References resolves a symbol by numeric id (from a find result) or by name
// (best-ranked match) and returns where it is used, so a caller need not run
// find first. Read-only.
func (q *Querier) References(target string) (Usages, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Usages{}, fmt.Errorf("a symbol name or id is required")
	}
	if id, err := strconv.ParseInt(target, 10, 64); err == nil {
		return q.FindReferences(id)
	}
	hits, err := q.FindSymbol(target)
	if err != nil {
		return Usages{}, err
	}
	if len(hits) == 0 {
		return Usages{}, fmt.Errorf("no symbol named %q; run find to see candidates", target)
	}
	return q.FindReferences(hits[0].ID)
}

// enclosingName returns the innermost code definition whose line range contains
// line, or "" when the line sits at file scope. Auxiliary config/doc kinds are
// skipped, and the tightest range wins so a method outranks its enclosing type.
func enclosingName(spans []store.SymbolSpan, line int) string {
	name, best := "", 0
	for _, sp := range spans {
		if auxiliaryKind(sp.Kind) || sp.StartLine > line || line > sp.EndLine {
			continue
		}
		if size := sp.EndLine - sp.StartLine; name == "" || size < best {
			name, best = sp.Name, size
		}
	}
	return name
}

// definesName reports whether a symbol with the given name begins on line, so a
// definition header (an interface method, an override) is not mistaken for a
// call site of that name.
func definesName(spans []store.SymbolSpan, line int, name string) bool {
	for _, sp := range spans {
		if sp.StartLine == line && sp.Name == name {
			return true
		}
	}
	return false
}

// containsWord reports whether name appears in line as a whole identifier token,
// not as a substring of a longer identifier.
func containsWord(line, name string) bool {
	for from := 0; from <= len(line)-len(name); {
		i := strings.Index(line[from:], name)
		if i < 0 {
			return false
		}
		i += from
		beforeOK := i == 0 || !isIdentByte(line[i-1])
		end := i + len(name)
		afterOK := end >= len(line) || !isIdentByte(line[end])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
	return false
}

// containsCall reports whether name appears in line as a whole identifier token
// immediately followed (ignoring spaces) by `(`, i.e. an actual call. This
// excludes field accesses and bare references that merely share the name, which
// a signature-change blast radius must not count as callers.
func containsCall(line, name string) bool {
	for from := 0; from <= len(line)-len(name); {
		i := strings.Index(line[from:], name)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(name)
		beforeOK := i == 0 || !isIdentByte(line[i-1])
		afterOK := end >= len(line) || !isIdentByte(line[end])
		if beforeOK && afterOK {
			j := end
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			if j < len(line) && line[j] == '(' {
				return true
			}
		}
		from = i + 1
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// depKinds are the dependency edges traversed for callers, entrypoint roots, and
// blast radius: includes/execs/binds/autostarts/references, QML component
// instantiation and singleton/type member use, and the synthetic "pkg" fan-out
// (an importer to every file of an imported package). calleeKinds omits "pkg":
// for "what does this file use" the direct edges are the answer, and the pkg
// fan-out would repeat every file of every imported package.
var depKinds = []string{"includes", "execs", "binds", "autostarts", "references", "instantiates", "uses", "pkg"}
var calleeKinds = []string{"includes", "execs", "binds", "autostarts", "references", "instantiates", "uses"}

// EdgeView is the agent-facing shape of a dependency edge: the related file, the
// kind, the line, the literal target, and whether it resolved. It drops the
// internal node ids/types so the result stays a uniform TOON table even when a
// file mixes resolved (in-project) and unresolved (external) edges.
type EdgeView struct {
	File     string `json:"file"`
	Kind     string `json:"kind"`
	Line     int    `json:"line"`
	Raw      string `json:"raw"`
	Resolved bool   `json:"resolved"`
}

func edgeViews(rows []store.EdgeRow) []EdgeView {
	out := make([]EdgeView, len(rows))
	for i, e := range rows {
		out[i] = EdgeView{File: e.File, Kind: e.Kind, Line: e.Line, Raw: e.Raw, Resolved: e.Resolved}
	}
	return out
}

// FindCallers returns configs/scripts that include, exec, or bind to a file
// (including cross-package importers via the synthetic pkg edge).
func (q *Querier) FindCallers(path string) ([]EdgeView, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := q.s.IncomingEdges("file", id, depKinds...)
	return edgeViews(rows), err
}

// FindCallees returns what a file directly includes, execs, or binds to.
func (q *Querier) FindCallees(path string) ([]EdgeView, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := q.s.EdgesFromFile(id, calleeKinds...)
	return edgeViews(rows), err
}

// Relations is the neighborhood of a file.
type Relations struct {
	File       string            `json:"file"`
	Exists     bool              `json:"exists"`
	Symbols    []store.SymbolHit `json:"symbols"`
	Includes   []EdgeView        `json:"includes"`
	IncludedBy []EdgeView        `json:"included_by"`
}

// FileRelations returns a file's symbols and include neighbors.
func (q *Querier) FileRelations(path string) (Relations, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Relations{}, err
	}
	defer release()
	r := Relations{File: path}
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return r, err
	}
	r.Exists = true
	r.Symbols, _ = q.s.SymbolsInFile(id)
	inc, _ := q.s.EdgesFromFile(id, "includes")
	r.Includes = edgeViews(inc)
	by, _ := q.s.IncomingEdges("file", id, "includes")
	r.IncludedBy = edgeViews(by)
	return r, nil
}

// BlastRadius returns the full list of files that transitively depend on a file.
func (q *Querier) BlastRadius(path string) ([]store.Dep, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return nil, err
	}
	return q.s.TransitiveDependents(id)
}

// SubsystemCount pairs a subsystem (top directory) with its dependent count.
type SubsystemCount struct {
	Subsystem string `json:"subsystem"`
	Count     int    `json:"count"`
}

// BlastSummary is a token-lean blast-radius overview: the total dependent count,
// a breakdown by subsystem (which reveals the dependency hubs that drive the
// radius), and the direct importers (the actionable depth-1 set). The full file
// list is available with --all. The graph is package-granular for code, so this
// counts files that compile-depend on the target, not symbol-level callers.
type BlastSummary struct {
	File        string           `json:"file"`
	Total       int              `json:"total"`
	Direct      int              `json:"direct"`
	BySubsystem []SubsystemCount `json:"by_subsystem"`
	DirectFiles []string         `json:"direct_files"`
}

// BlastSummarize returns a grouped blast-radius summary instead of the full list.
func (q *Querier) BlastSummarize(path string) (BlastSummary, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return BlastSummary{}, err
	}
	defer release()
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return BlastSummary{File: path}, err
	}
	deps, err := q.s.TransitiveDependents(id)
	if err != nil {
		return BlastSummary{File: path}, err
	}
	sum := BlastSummary{File: path, Total: len(deps)}
	byDir := map[string]int{}
	for _, d := range deps {
		byDir[subsystem(d.File)]++
		if d.Depth == 1 {
			sum.Direct++
			sum.DirectFiles = append(sum.DirectFiles, d.File)
		}
	}
	for dir, n := range byDir {
		sum.BySubsystem = append(sum.BySubsystem, SubsystemCount{Subsystem: dir, Count: n})
	}
	sort.Slice(sum.BySubsystem, func(i, j int) bool {
		if sum.BySubsystem[i].Count != sum.BySubsystem[j].Count {
			return sum.BySubsystem[i].Count > sum.BySubsystem[j].Count
		}
		return sum.BySubsystem[i].Subsystem < sum.BySubsystem[j].Subsystem
	})
	if len(sum.BySubsystem) > 15 {
		sum.BySubsystem = sum.BySubsystem[:15]
	}
	sort.Strings(sum.DirectFiles)
	// The Direct count above is the full depth-1 total; inline only a sample so a
	// hub file (hundreds of direct importers) does not balloon the summary. The
	// complete list is available with --all.
	if len(sum.DirectFiles) > 20 {
		sum.DirectFiles = sum.DirectFiles[:20]
	}
	return sum, nil
}

// subsystem maps a file to a coarse subsystem label: its first two path segments
// (e.g. pkg/gui), or the first segment / "." for shallow paths.
func subsystem(file string) string {
	parts := strings.Split(file, "/")
	switch len(parts) {
	case 1:
		return "."
	case 2:
		return parts[0]
	default:
		return parts[0] + "/" + parts[1]
	}
}

// EntrypointSet is the set of root files from which a file is reachable: the full
// count plus a shallow-first sample. A widely-used utility is reachable from
// nearly every root, so the inline list is capped to keep the answer token-lean.
type EntrypointSet struct {
	File        string   `json:"file"`
	Count       int      `json:"count"`
	Entrypoints []string `json:"entrypoints"`
}

// EntrypointsFor returns the root files (no incoming dependency edges) from which
// path is reachable, as a count plus a shallow-first sample.
func (q *Querier) EntrypointsFor(path string) (EntrypointSet, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return EntrypointSet{}, err
	}
	defer release()
	out := EntrypointSet{File: path}
	id, ok, err := q.fileID(path)
	if err != nil || !ok {
		return out, err
	}
	deps, err := q.s.TransitiveDependents(id)
	if err != nil {
		return out, err
	}
	if len(deps) == 0 {
		out.Count, out.Entrypoints = 1, []string{path} // nothing depends on it -> it is the entrypoint
		return out, nil
	}
	var roots []string
	for _, d := range deps {
		did, err := q.s.FileID(d.File)
		if err != nil {
			continue
		}
		if in, _ := q.s.IncomingEdges("file", did, depKinds...); len(in) == 0 {
			roots = append(roots, d.File)
		}
	}
	out.Count = len(roots)
	sort.Slice(roots, func(i, j int) bool {
		if a, b := strings.Count(roots[i], "/"), strings.Count(roots[j], "/"); a != b {
			return a < b
		}
		return roots[i] < roots[j]
	})
	if len(roots) > 20 {
		roots = roots[:20]
	}
	out.Entrypoints = roots
	return out, nil
}

// TestsResult reports the test files covering a file -- colocated tests or tests
// that import it -- and, for a config/script with no tests, the configs/keybinds
// that launch it (the dotfile analogue).
type TestsResult struct {
	File    string          `json:"file"`
	Tests   []string        `json:"tests,omitempty"`
	Runners []store.EdgeRow `json:"runners,omitempty"`
	Limited bool            `json:"limited,omitempty"`
	Note    string          `json:"note"`
}

// TestsFor returns the test files covering a file: first the test files in its
// own directory (the common layout where tests sit beside source), then, if
// there are none, the test files that import it (Java/C# src/test mirrors, an
// external test package). The conventional match (os.go -> os_test.go) is listed
// first. With no test files -- the usual case for a config or script -- it falls
// back to the configs/keybinds that launch or reload the file.
func (q *Querier) TestsFor(path string) (TestsResult, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return TestsResult{}, err
	}
	defer release()
	res := TestsResult{File: path}
	seen := map[string]bool{path: true}
	collect := func(p string) {
		if isTestPath(p) && !seen[p] {
			seen[p] = true
			res.Tests = append(res.Tests, p)
		}
	}
	dir := dirOf(path)
	if files, err := q.s.AllFiles(); err == nil {
		for _, f := range files {
			if dirOf(f.RelPath) == dir {
				collect(f.RelPath)
			}
		}
	}
	if len(res.Tests) == 0 {
		if id, ok, err := q.fileID(path); err == nil && ok {
			if in, err := q.s.IncomingEdges("file", id, depKinds...); err == nil {
				for _, e := range in {
					collect(e.File)
				}
			}
		}
	}
	if len(res.Tests) > 0 {
		stem := baseStem(path)
		sort.SliceStable(res.Tests, func(i, j int) bool {
			return testCoversStem(res.Tests[i], stem) && !testCoversStem(res.Tests[j], stem)
		})
		const maxTests = 25
		if len(res.Tests) > maxTests {
			res.Tests = res.Tests[:maxTests]
		}
		res.Note = "test files covering this file (colocated, or importing it); run them after changing it"
		return res, nil
	}
	res.Limited = true
	res.Note = "no test files detected; showing configs/keybinds that launch or reload this file"
	if id, ok, err := q.fileID(path); err == nil && ok {
		res.Runners, _ = q.s.IncomingEdges("file", id, "execs", "binds", "autostarts")
	}
	return res, nil
}

// dirOf returns the directory portion of a slash path ("" for a bare filename).
func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// baseStem returns a path's filename without directory or extension, cutting at
// the first dot (so foo.test.ts -> foo).
func baseStem(p string) string {
	b := p
	if i := strings.LastIndex(b, "/"); i >= 0 {
		b = b[i+1:]
	}
	if i := strings.IndexByte(b, '.'); i >= 0 {
		b = b[:i]
	}
	return b
}

// testCoversStem reports whether a test file is the conventional test for a
// source file with the given stem: os.go<->os_test.go, foo<->test_foo /
// foo.test / foo.spec / foo_spec / FooTest.
func testCoversStem(testPath, stem string) bool {
	lt, ls := strings.ToLower(baseStem(testPath)), strings.ToLower(stem)
	if lt == ls {
		return true // foo.test.ts / foo.spec.ts -> baseStem "foo"
	}
	for _, suf := range []string{"_test", "_spec", "test"} {
		if strings.TrimSuffix(lt, suf) == ls {
			return true
		}
	}
	for _, pre := range []string{"test_", "spec_"} {
		if strings.TrimPrefix(lt, pre) == ls {
			return true
		}
	}
	return false
}

// SimilarCode returns ranked snippets. With an inferencer and a local vector
// index it fuses semantic (vector KNN) and lexical (FTS) results via reciprocal
// rank fusion; otherwise it falls back to lexical FTS only. It never spawns an
// inference call on the default path: the expensive agent rewrite+rerank is
// reserved for SmartSearch (--smart), so plain `search` stays fast enough to sit
// in an agent's inner loop even on the no-local-model (agent) tier.
func (q *Querier) SimilarCode(ctx context.Context, text string) ([]store.ChunkHit, error) {
	release, err := q.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if q.inf == nil || !q.s.VectorsReady() {
		fts, err := q.searchChunksRanked(text, DefaultLimit)
		if err != nil {
			return nil, err
		}
		// With AI disabled the lexical ranking is the base; the doc signal still
		// runs, as a recall appendix that never reorders it (see mergeDocRecall).
		return mergeDocRecall(fts, q.docChunks(text, DefaultLimit), DefaultLimit), nil
	}
	return q.hybrid(ctx, text, DefaultLimit)
}

// rewriteQuery turns a natural-language query into a short keyword query via the
// inferencer, returning the original text unchanged on any failure. It is the
// cheap, single-call first half of the SmartSearch (--smart) assist pipeline.
func (q *Querier) rewriteQuery(ctx context.Context, text string) string {
	if q.inf == nil {
		return text
	}
	rw, err := q.inf.Generate(ctx, rewritePrompt(text))
	if err != nil {
		return text
	}
	if c := cleanRewrite(rw); c != "" {
		return c
	}
	return text
}

// searchChunksRanked runs the FTS query over a generous pool, then stably ranks
// the chunks so a project's own implementation leads: source code first, then
// tests, then docs and i18n/locale string tables, then vendored/generated code.
// Lexical search otherwise floats string tables, translated docs, and test files
// (which carry the human-readable concept words) above the code that implements
// the thing searched for. The FTS order is kept within each tier and the pool is
// truncated to limit; nothing is dropped.
func (q *Querier) searchChunksRanked(text string, limit int) ([]store.ChunkHit, error) {
	pool := limit * 4
	if pool < 200 {
		pool = 200
	}
	hits, err := q.s.SearchChunks(text, pool)
	if err != nil {
		return nil, err
	}
	// Surface files whose path names the concept (stash-download.sh for
	// "download"), even when their text does not match the exact query tokens --
	// the case pure FTS-over-text ranking misses entirely, so the file never
	// appears at any rank.
	terms := conceptTerms(text)
	if len(terms) > 0 {
		if pathHits, perr := q.s.ChunksByPathTerms(terms, 40); perr == nil {
			seen := make(map[string]bool, len(hits))
			for _, h := range hits {
				seen[h.File+"\x00"+strconv.Itoa(h.StartLine)] = true
			}
			for _, h := range pathHits {
				if k := h.File + "\x00" + strconv.Itoa(h.StartLine); !seen[k] {
					seen[k] = true
					hits = append(hits, h)
				}
			}
		}
	}
	// Rank project source first (tier), then float files whose path names the
	// concept, keeping the FTS order within equal buckets.
	sort.SliceStable(hits, func(i, j int) bool {
		if ti, tj := searchTier(hits[i].File), searchTier(hits[j].File); ti != tj {
			return ti < tj
		}
		return pathConceptScore(terms, hits[i].File) > pathConceptScore(terms, hits[j].File)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

var searchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true, "does": true,
	"this": true, "that": true, "from": true, "into": true, "use": true, "using": true,
	"get": true, "set": true, "new": true, "add": true, "are": true, "was": true,
	"you": true, "your": true, "what": true, "where": true, "when": true,
}

// conceptTerms extracts distinctive lowercase terms from a query, plus a light
// stem of each (downloader -> download), for matching against file paths.
func conceptTerms(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		if len(f) < 3 || searchStopwords[f] {
			continue
		}
		for _, v := range []string{f, stemTerm(f)} {
			if len(v) >= 3 && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// stemTerm strips a common English suffix, keeping a stem of at least 4 chars.
func stemTerm(s string) string {
	for _, suf := range []string{"ing", "ers", "er", "ed", "es", "s"} {
		if strings.HasSuffix(s, suf) && len(s)-len(suf) >= 4 {
			return s[:len(s)-len(suf)]
		}
	}
	return s
}

// pathConceptScore scores how strongly a file path names the query concept: a
// term in the basename counts double, a term elsewhere in the path counts one.
func pathConceptScore(terms []string, path string) int {
	if len(terms) == 0 {
		return 0
	}
	lp := strings.ToLower(path)
	base := lp
	if i := strings.LastIndexByte(lp, '/'); i >= 0 {
		base = lp[i+1:]
	}
	score := 0
	for _, t := range terms {
		switch {
		case strings.Contains(base, t):
			score += 2
		case strings.Contains(lp, t):
			score++
		}
	}
	return score
}

// searchTier ranks a path for content search so a project's own implementation
// leads: source (0), tests (1), docs and i18n/locale string tables (2), then
// vendored or generated code (3). Demotion only -- a test or doc still appears,
// just below the code that implements what was searched for.
func searchTier(p string) int {
	switch {
	case isVendored(p):
		return 3
	case isDocOrLocalePath(p):
		return 2
	case isTestPath(p):
		return 1
	default:
		return 0
	}
}

// isTestPath reports whether a path is a test file or lives under a test dir.
func isTestPath(p string) bool {
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	for _, m := range []string{"_test.", ".test.", "_spec.", ".spec."} {
		if strings.Contains(base, m) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "specs", "testdata", "integration_test":
			return true
		}
	}
	return false
}

// isDocOrLocalePath reports whether a path is documentation or an i18n/locale
// string table -- text that describes or contains feature wording rather than
// implementing it, which lexical search otherwise ranks high for concept queries.
func isDocOrLocalePath(p string) bool {
	lp := strings.ToLower(p)
	for _, ext := range []string{".md", ".mdx", ".markdown", ".rst", ".adoc", ".txt"} {
		if strings.HasSuffix(lp, ext) {
			return true
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "docs", "doc", "i18n", "l10n", "locale", "locales", "translations":
			return true
		}
	}
	return false
}

// hybrid embeds the query, runs vector KNN, and fuses the vector and lexical
// signals by RRF (falling back to lexical alone if embedding fails); the doc
// signal is then applied as a recall appendix that cannot reorder the fused base
// (see mergeDocRecall, constraint C5).
func (q *Querier) hybrid(ctx context.Context, text string, k int) ([]store.ChunkHit, error) {
	fts, err := q.searchChunksRanked(text, k)
	if err != nil {
		return nil, err
	}
	docs := q.docChunks(text, k)
	vecs, err := q.inf.Embed(ctx, []string{text})
	if err != nil || len(vecs) == 0 {
		return mergeDocRecall(fts, docs, k), nil
	}
	vhits, err := q.s.VectorSearch(vecs[0], k)
	if err != nil {
		return mergeDocRecall(fts, docs, k), nil
	}
	return mergeDocRecall(fuseRRF(k, vhits, fts), docs, k), nil
}

// SmartResult is the assist-augmented search result.
type SmartResult struct {
	Query     string           `json:"query"`
	Rewritten string           `json:"rewritten,omitempty"`
	Matches   []store.ChunkHit `json:"matches"`
}

// SmartSearch runs the full assist pipeline: a query rewrite, candidate
// retrieval, then an agent/model rerank. Candidate retrieval fuses vector+FTS
// when a local vector index exists, and otherwise ranks lexically -- so --smart
// still rewrites and reranks through the attached coding agent even with no
// local embed model (the agent tier), which is the whole point of a borrowed
// backend. Only a missing inferencer drops it to plain FTS. Every model output
// is constrained (a query string, an index ordering); the model never invents
// or edits results.
func (q *Querier) SmartSearch(ctx context.Context, text string) (SmartResult, error) {
	release, err := q.beginRead(ctx)
	if err != nil {
		return SmartResult{}, err
	}
	defer release()
	res := SmartResult{Query: text}
	if q.inf == nil {
		hits, err := q.searchChunksRanked(text, DefaultLimit)
		if err != nil {
			return res, err
		}
		// AI fully disabled: the lexical ranking is the base, and the doc signal
		// runs as the same recall appendix plain `search` (SimilarCode) already
		// applies, so --smart is not silently missing doc recall its sibling
		// entry point has on the same tier (see mergeDocRecall).
		res.Matches = mergeDocRecall(hits, q.docChunks(text, DefaultLimit), DefaultLimit)
		return res, nil
	}
	search := text
	if rw := q.rewriteQuery(ctx, text); rw != text {
		search, res.Rewritten = rw, rw
	}
	var cand []store.ChunkHit
	if q.s.VectorsReady() {
		cand, err = q.hybrid(ctx, search, 20)
	} else {
		// No local vector index (the agent tier): rank lexically, then apply the
		// same doc recall appendix hybrid and SimilarCode use, so --smart matches
		// plain `search` on this tier. The model rerank below reorders the pool
		// regardless, so this only widens recall; it cannot break C5's base order.
		var fts []store.ChunkHit
		fts, err = q.searchChunksRanked(search, 20)
		if err == nil {
			cand = mergeDocRecall(fts, q.docChunks(search, 20), 20)
		}
	}
	if err != nil {
		return res, err
	}
	if len(cand) > 1 {
		docs := make([]string, len(cand))
		for i, c := range cand {
			docs[i] = c.Snippet
		}
		if order, err := q.inf.Rerank(ctx, text, docs); err == nil && len(order) == len(cand) {
			reordered := make([]store.ChunkHit, 0, len(cand))
			for _, idx := range order {
				reordered = append(reordered, cand[idx])
			}
			cand = reordered
		}
	}
	if len(cand) > DefaultLimit {
		cand = cand[:DefaultLimit]
	}
	res.Matches = cand
	return res, nil
}

// rewritePrompt asks for a keyword form of a natural-language question. It must
// describe the corpus accurately: this prompt used to say "dotfiles/config index"
// (prowl began as a dotfiles indexer), which steered rewrites of code questions
// toward configuration vocabulary the repository does not contain.
func rewritePrompt(q string) string {
	return "Rewrite this into a short keyword search query for a source-code index. " +
		"Prefer identifiers, type and function names, and domain nouns a programmer " +
		"would have written in the code. Reply with only the keywords, no punctuation " +
		"or explanation.\nQuery: " + q
}

func cleanRewrite(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > 120 {
		return ""
	}
	return s
}

// Violation is a deterministic architecture/health finding.
type Violation struct {
	Kind   string `json:"kind"`
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	Detail string `json:"detail"`
}

// ArchitectureViolations returns dangling references, orphan scripts, and
// hardcoded colors that duplicate a declared variable.
func (q *Querier) ArchitectureViolations() ([]Violation, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	var v []Violation
	dang, err := q.s.UnresolvedEdges("includes", "references", "uses_resource")
	if err != nil {
		return nil, err
	}
	files, err := q.s.AllFiles()
	if err != nil {
		return nil, err
	}
	langByID := make(map[int64]string, len(files))
	for _, f := range files {
		langByID[f.ID] = f.Lang
	}
	for _, e := range dang {
		if e.Kind == "includes" && store.ModuleImportLang(langByID[e.FileID]) {
			continue // external module import, not a broken project reference
		}
		if e.Kind == "uses_resource" || looksPathy(e.Raw) {
			v = append(v, Violation{Kind: "dangling_" + e.Kind, File: e.File, Line: e.Line, Detail: e.Raw})
		}
	}
	orphans, err := q.s.OrphanFiles("script")
	if err != nil {
		return nil, err
	}
	for _, f := range orphans {
		v = append(v, Violation{Kind: "orphan_script", File: f.RelPath, Detail: "script not referenced by any config or keybind"})
	}
	res, err := q.s.AllResources()
	if err != nil {
		return nil, err
	}
	declByValue := make(map[string]string)
	for _, r := range res {
		if r.Name != "" && r.Value != "" {
			declByValue[r.Value] = r.Name
		}
	}
	for _, r := range res {
		if r.Name == "" && r.Value != "" {
			if name, ok := declByValue[r.Value]; ok {
				v = append(v, Violation{Kind: "hardcoded_color", File: r.File, Line: r.Line, Detail: r.Value + " (use " + name + ")"})
			}
		}
	}
	return v, nil
}

func looksPathy(s string) bool {
	return strings.ContainsAny(s, "/") || strings.HasPrefix(s, "$") || strings.HasPrefix(s, "@")
}

// Hotspots ranks structurally central and large files.
type Hotspots struct {
	Central          []store.FanRow   `json:"central"`
	FanIn            []store.FanRow   `json:"fan_in"`
	Largest          []FileSize       `json:"largest"`
	LargestFunctions []store.FuncSpan `json:"largest_functions"`
	ComplexFunctions []store.FuncSpan `json:"complex_functions,omitempty"`
}

// FileSize pairs a file with its byte size.
type FileSize struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// isVendored reports whether a path is third-party or generated code, which is
// noise in hotspots (you do not refactor your dependencies). Such files stay
// indexed and queryable; they are only dropped from the central/largest/complex
// rankings.
func isVendored(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "vendor", "third_party", "third-party", "node_modules", ".venv", "venv", "site-packages", ".yarn":
			return true
		}
	}
	return strings.HasSuffix(p, ".pb.go") || strings.HasSuffix(p, "_pb2.py") || strings.HasSuffix(p, ".g.dart")
}

// auxiliaryKind reports whether a symbol kind is a config or documentation
// entry (a setting, keybind, color, heading, or config section) rather than a
// code definition. These dominate config-heavy repos and FTS ranks the tiny
// rows high, so find floats real definitions above them without dropping them.
func auxiliaryKind(kind string) bool {
	switch kind {
	case "setting", "keybind", "color", "heading", "config_section":
		return true
	}
	return false
}

// findRank orders FindSymbol results into four tiers: project code definitions,
// then project config/doc entries, then vendored definitions, then vendored
// config/doc. A stable sort keeps the prior order (exact, FTS, substring)
// within each tier, so the highest-precision match still leads.
func findRank(h store.SymbolHit) int {
	r := 0
	if auxiliaryKind(h.Kind) {
		r++
	}
	if isVendored(h.File) {
		r += 2
	}
	return r
}

// RepoHotspots returns fan-in and size rankings over the project's own code
// (vendored and generated files are excluded as noise). Git churn arrives in M3.
func (q *Querier) RepoHotspots() (Hotspots, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Hotspots{}, err
	}
	defer release()
	var h Hotspots
	const top, pool = 10, 200
	fan, err := q.s.FanIn(pool)
	if err != nil {
		return h, err
	}
	for _, r := range fan {
		if isVendored(r.File) {
			continue
		}
		if h.FanIn = append(h.FanIn, r); len(h.FanIn) >= top {
			break
		}
	}
	files, err := q.s.AllFiles()
	if err != nil {
		return h, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	for _, f := range files {
		if isVendored(f.RelPath) {
			continue
		}
		if h.Largest = append(h.Largest, FileSize{File: f.RelPath, Size: f.Size}); len(h.Largest) >= top {
			break
		}
	}
	lf, _ := q.s.LargestFunctions(pool)
	for _, r := range lf {
		if isVendored(r.File) {
			continue
		}
		if h.LargestFunctions = append(h.LargestFunctions, r); len(h.LargestFunctions) >= top {
			break
		}
	}
	mc, _ := q.s.MostComplex(pool)
	for _, r := range mc {
		if isVendored(r.File) {
			continue
		}
		if h.ComplexFunctions = append(h.ComplexFunctions, r); len(h.ComplexFunctions) >= top {
			break
		}
	}
	edges, err := q.s.FileDepEdges()
	if err != nil {
		return h, err
	}
	h.Central = centralFiles(files, edges, top)
	return h, nil
}

// Status summarizes index freshness and coverage.
type Status struct {
	Counts    store.Counts `json:"counts"`
	LastIndex string       `json:"last_index"`
	AIEnabled bool         `json:"ai_enabled"`
	Savings   Savings      `json:"savings"`
	// Semantic reports how much of the index is embedded, so a partially built
	// semantic index is visible rather than looking like a silent hang.
	Semantic SemanticCoverage `json:"semantic"`
}

// SemanticCoverage describes vector-search readiness. Embedded chunks are
// searchable semantically; the rest are still covered by lexical ranking.
type SemanticCoverage struct {
	Chunks    int  `json:"chunks"`
	Embedded  int  `json:"embedded"`
	Remaining int  `json:"remaining"`
	Complete  bool `json:"complete"`
}

// Savings estimates tokens saved versus reading the files each answer pointed at.
// Tokens are bytes/4 (the usual rough rule). The figure is deliberately
// conservative: it reports only a fraction (savingsMargin) of the measured byte
// difference, so it under-counts rather than over-claims.
type Savings struct {
	Queries      int64 `json:"queries"`
	AnswerTokens int64 `json:"answer_tokens"`
	SavedTokens  int64 `json:"saved_tokens"`
}

// savingsMargin is the fraction of the raw byte difference we report, leaving
// headroom for files an agent would only partly read, result overlap, and
// tokenizer variance.
const savingsMargin = 0.7

// ComputeSavings turns raw usage counters into a conservative savings estimate.
func ComputeSavings(s store.Stats) Savings {
	raw := s.BaselineBytes - s.AnswerBytes
	if raw < 0 {
		raw = 0
	}
	return Savings{
		Queries:      s.Queries,
		AnswerTokens: s.AnswerBytes / 4,
		SavedTokens:  int64(float64(raw) * savingsMargin / 4),
	}
}

// Status returns the index summary.
func (q *Querier) Status() (Status, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Status{}, err
	}
	defer release()
	c, err := q.s.Counts()
	if err != nil {
		return Status{}, err
	}
	last, _ := q.s.GetMeta("last_index")
	ai, _ := q.s.GetMeta("ai_enabled")
	stats, _ := q.s.Stats()
	remaining, _ := q.s.CountChunksWithoutVectors()
	embeddable, _ := q.s.CountEmbeddableChunks()
	return Status{
		Counts: c, LastIndex: last, AIEnabled: ai == "true",
		Savings: ComputeSavings(stats),
		Semantic: SemanticCoverage{
			Chunks:    embeddable,
			Embedded:  embeddable - remaining,
			Remaining: remaining,
			Complete:  q.s.VectorsComplete(),
		},
	}, nil
}
