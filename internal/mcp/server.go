// Package mcp exposes Prowl's structural queries to coding agents as MCP
// tools over stdio, using the official MCP Go SDK.
package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// ReindexFunc refreshes the index and returns a human-readable summary.
type ReindexFunc func(ctx context.Context) (string, error)

// DoctorFunc runs health diagnostics for the project.
type DoctorFunc func(ctx context.Context) (doctor.Report, error)

type handlers struct {
	q            *query.Querier
	store        *store.Store
	reindex      ReindexFunc
	doctor       DoctorFunc
	onCall       func()
	beforeCall   func(context.Context) error
	context      *contextpacket.Service
	knowledge    *knowledge.Repository
	capabilities *capability.Catalog
	root         string
}

// Empty is the input type for tools that take no arguments.
type Empty struct{}

// NewServer builds the compatibility MCP server used by existing generated configs.
func NewServer(q *query.Querier, st *store.Store, version string, reindex ReindexFunc, doctorFn DoctorFunc, onCall func()) *sdk.Server {
	return NewServerWithOptions(q, st, version, reindex, doctorFn, onCall, ServerOptions{Surface: SurfaceLegacy})
}

// NewServerWithOptions builds legacy, core, or combined MCP surfaces.
func NewServerWithOptions(q *query.Querier, st *store.Store, version string, reindex ReindexFunc, doctorFn DoctorFunc, onCall func(), options ServerOptions) *sdk.Server {
	options.normalize()
	h := &handlers{q: q, store: st, reindex: reindex, doctor: doctorFn, onCall: onCall, beforeCall: options.BeforeCall, context: options.Context, knowledge: options.Knowledge, capabilities: options.Capabilities, root: options.Root}
	var server *sdk.Server
	if options.Surface == SurfaceLegacy || options.Surface == SurfaceAll {
		server = newLegacyServer(h, version)
	} else {
		server = sdk.NewServer(&sdk.Implementation{Name: "prowl", Version: version}, nil)
	}
	if options.Surface == SurfaceCore || options.Surface == SurfaceAll {
		registerCoreTools(server, h)
	}
	registerResources(server, h)
	registerPrompts(server)
	return server
}

func newLegacyServer(h *handlers, version string) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "prowl", Version: version}, nil)

	sdk.AddTool(s, &sdk.Tool{Name: "find_symbol",
		Description: "Find symbols (functions, settings, keybinds, components, ids) by name."}, tracked(h, h.findSymbol))
	sdk.AddTool(s, &sdk.Tool{Name: "find_references",
		Description: "Where a symbol (id from find_symbol) is used: config/resource reference edges, or cited file:line:text call sites for code symbols, each tagged with its enclosing (calling) function when the usage sits inside one."}, tracked(h, h.findReferences))
	sdk.AddTool(s, &sdk.Tool{Name: "find_callers",
		Description: "Configs/scripts that include, exec, or bind to a file."}, tracked(h, h.findCallers))
	sdk.AddTool(s, &sdk.Tool{Name: "find_callees",
		Description: "What a file includes, execs, or binds to."}, tracked(h, h.findCallees))
	sdk.AddTool(s, &sdk.Tool{Name: "file_relations",
		Description: "A file's defined symbols and include neighbors."}, tracked(h, h.fileRelations))
	sdk.AddTool(s, &sdk.Tool{Name: "blast_radius",
		Description: "Change impact of a file: total dependent count, a breakdown by subsystem (the hubs driving it), and the direct importers."}, tracked(h, h.blastRadius))
	sdk.AddTool(s, &sdk.Tool{Name: "entrypoints_for",
		Description: "Root files from which a file is reachable (its entry points)."}, tracked(h, h.entrypointsFor))
	sdk.AddTool(s, &sdk.Tool{Name: "tests_for",
		Description: "Test files covering a file: colocated tests or tests that import it (conventional match first). For a config/script with no tests, the configs/keybinds that launch it."}, tracked(h, h.testsFor))
	sdk.AddTool(s, &sdk.Tool{Name: "similar_code",
		Description: "Search file content. Hybrid vector and full-text when the semantic layer is enabled, else full-text. Returns cited snippets."}, tracked(h, h.similarCode))
	sdk.AddTool(s, &sdk.Tool{Name: "smart_search",
		Description: "Assist-augmented search: rewrites the query, runs hybrid retrieval, and reranks. Best for fuzzy/natural-language queries. Falls back to full-text when the semantic layer is off."}, tracked(h, h.smartSearch))
	sdk.AddTool(s, &sdk.Tool{Name: "search_docs",
		Description: "Search external documentation ingested via `prowl docs add` (crawled library/framework docs or a local Markdown tree): a small, cited, budget-bounded context packet. For this repo's code use search_context or similar_code."}, tracked(h, h.searchDocs))
	sdk.AddTool(s, &sdk.Tool{Name: "architecture_violations",
		Description: "Dangling references, orphan scripts, and hardcoded colors."}, tracked(h, h.architectureViolations))
	sdk.AddTool(s, &sdk.Tool{Name: "repo_hotspots",
		Description: "Central files, largest files, largest functions, and most complex functions (cyclomatic)."}, tracked(h, h.repoHotspots))
	sdk.AddTool(s, &sdk.Tool{Name: "status",
		Description: "Index freshness, counts, languages, and AI status."}, tracked(h, h.status))
	sdk.AddTool(s, &sdk.Tool{Name: "overview",
		Description: "High-level map of the project: role breakdown, entrypoints, clusters, color palette, keybind count, languages, and hotspots. A good first call on a new project."}, tracked(h, h.overview))
	sdk.AddTool(s, &sdk.Tool{Name: "clusters",
		Description: "Group related files into subsystems (connected via includes, exec chains, and shared resources)."}, tracked(h, h.clusters))
	sdk.AddTool(s, &sdk.Tool{Name: "reindex",
		Description: "Re-scan the project and refresh the index incrementally."}, tracked(h, h.reindexTool))
	sdk.AddTool(s, &sdk.Tool{Name: "doctor",
		Description: "Health diagnostics: cyclic includes, fan-in/out risk, oversized files, duplicate keybinds, broken commands, orphan scripts, dangling references, hardcoded colors, hardcoded secrets, forbidden crossings, churn hotspots. Returns findings and a 0-100 score."}, tracked(h, h.doctorTool))
	sdk.AddTool(s, &sdk.Tool{Name: "investigate_wip",
		Description: "Investigate uncommitted work in progress: touched files with git status, the unfinished-work markers (TODO, FIXME, and friends) inside them, and the blast radius of each indexed file. A good first call when resuming a task.", Annotations: readOnlyAnnotations("Investigate WIP")}, tracked(h, h.investigateWip))
	sdk.AddTool(s, &sdk.Tool{Name: "sketch_ui",
		Description: "Sketch how a UI looks and behaves, from source, without a screenshot: QML and React (jsx/tsx) give the element tree with visual properties and behavior (handlers, animations, conditional rendering); a Go/lipgloss TUI gives the palette and named styles; CSS/SCSS gives design tokens and rules. Argument is a component name or file path. Read-only.", Annotations: readOnlyAnnotations("Sketch UI")}, tracked(h, h.sketchUI))
	return s
}

// Serve runs the server over stdio until the client disconnects.
func Serve(ctx context.Context, s *sdk.Server) error {
	return s.Run(ctx, &sdk.StdioTransport{})
}

// --- inputs ---

type nameIn struct {
	Name string `json:"name" jsonschema:"symbol name to find"`
}
type symbolIn struct {
	SymbolID int64 `json:"symbol_id" jsonschema:"symbol id from find_symbol"`
}
type pathIn struct {
	Path string `json:"path" jsonschema:"file path relative to the project root"`
}
type queryIn struct {
	Query  string `json:"query" jsonschema:"free-text search query"`
	Detail string `json:"detail,omitempty" jsonschema:"result detail: 'compact' for file:line only, 'full' for snippets (default full)"`
}

// --- outputs ---

type symbolsOut struct {
	Symbols []store.SymbolHit `json:"symbols"`
}
type edgesOut struct {
	Edges []query.EdgeView `json:"edges"`
}
type entrypointsOut = query.EntrypointSet
type chunksOut struct {
	Matches []store.ChunkHit `json:"matches"`
}
type violationsOut struct {
	Violations []query.Violation `json:"violations"`
}
type messageOut struct {
	Message string `json:"message"`
}

// --- handlers ---

func (h *handlers) findSymbol(ctx context.Context, _ *sdk.CallToolRequest, in nameIn) (*sdk.CallToolResult, symbolsOut, error) {
	hits, err := h.q.FindSymbol(in.Name)
	return nil, symbolsOut{Symbols: hits}, err
}

func (h *handlers) findReferences(ctx context.Context, _ *sdk.CallToolRequest, in symbolIn) (*sdk.CallToolResult, query.Usages, error) {
	u, err := h.q.FindReferences(in.SymbolID)
	return nil, u, err
}

func (h *handlers) findCallers(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, edgesOut, error) {
	e, err := h.q.FindCallers(in.Path)
	return nil, edgesOut{Edges: e}, err
}

func (h *handlers) findCallees(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, edgesOut, error) {
	e, err := h.q.FindCallees(in.Path)
	return nil, edgesOut{Edges: e}, err
}

func (h *handlers) fileRelations(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, query.Relations, error) {
	r, err := h.q.FileRelations(in.Path)
	return nil, r, err
}

func (h *handlers) blastRadius(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, query.BlastSummary, error) {
	d, err := h.q.BlastSummarize(in.Path)
	return nil, d, err
}

func (h *handlers) entrypointsFor(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, entrypointsOut, error) {
	e, err := h.q.EntrypointsFor(in.Path)
	return nil, e, err
}

func (h *handlers) testsFor(ctx context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, query.TestsResult, error) {
	r, err := h.q.TestsFor(in.Path)
	return nil, r, err
}

func (h *handlers) similarCode(ctx context.Context, _ *sdk.CallToolRequest, in queryIn) (*sdk.CallToolResult, chunksOut, error) {
	m, err := h.q.SimilarCode(ctx, in.Query)
	return nil, chunksOut{Matches: compactIf(in.Detail, m)}, err
}

func (h *handlers) smartSearch(ctx context.Context, _ *sdk.CallToolRequest, in queryIn) (*sdk.CallToolResult, query.SmartResult, error) {
	r, err := h.q.SmartSearch(ctx, in.Query)
	r.Matches = compactIf(in.Detail, r.Matches)
	return nil, r, err
}

func (h *handlers) overview(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, query.Overview, error) {
	o, err := h.q.Overview()
	return nil, o, err
}

type clustersOut struct {
	Clusters []query.Cluster `json:"clusters"`
}

func (h *handlers) clusters(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, clustersOut, error) {
	c, err := h.q.Clusters()
	return nil, clustersOut{Clusters: c}, err
}

// compactIf strips snippets when detail == "compact", for token-lean results.
func compactIf(detail string, hits []store.ChunkHit) []store.ChunkHit {
	if detail != "compact" {
		return hits
	}
	out := make([]store.ChunkHit, len(hits))
	for i, h := range hits {
		h.Snippet = ""
		out[i] = h
	}
	return out
}

func (h *handlers) architectureViolations(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, violationsOut, error) {
	v, err := h.q.ArchitectureViolations()
	return nil, violationsOut{Violations: v}, err
}

func (h *handlers) repoHotspots(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, query.Hotspots, error) {
	hs, err := h.q.RepoHotspots()
	return nil, hs, err
}

func (h *handlers) status(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, query.Status, error) {
	st, err := h.q.Status()
	return nil, st, err
}

func (h *handlers) reindexTool(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, messageOut, error) {
	if h.reindex == nil {
		return nil, messageOut{Message: "reindex not available"}, nil
	}
	msg, err := h.reindex(ctx)
	return nil, messageOut{Message: msg}, err
}

func (h *handlers) doctorTool(ctx context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, doctor.Report, error) {
	if h.doctor == nil {
		return nil, doctor.Report{Summary: map[string]int{}}, nil
	}
	r, err := h.doctor(ctx)
	return nil, r, err
}

// tracked wraps a tool handler so each successful answer updates the savings
// counters: the bytes prowl returned, and the size of the files it pointed at.
func tracked[In, Out any](h *handlers, fn func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, Out, error)) func(context.Context, *sdk.CallToolRequest, In) (*sdk.CallToolResult, Out, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		if h.beforeCall != nil {
			if err := h.beforeCall(ctx); err != nil {
				var zero Out
				return nil, zero, err
			}
		}
		if h.onCall != nil {
			h.onCall()
		}
		res, out, err := fn(ctx, req, in)
		if err == nil {
			h.recordStats(out)
			// When the handler did not choose its own (compact) content, the SDK
			// would serialize the output as JSON for the model to read. Emit it as
			// TOON instead, which is cheaper for the same data.
			if res == nil {
				if toonRes := toonFallback(out); toonRes != nil {
					res = toonRes
				}
			}
		}
		return res, out, err
	}
}

// recordStats measures one answer for the savings report (serialized size and the
// size of the files it pointed at). The measurement is shared with the CLI path.
func (h *handlers) recordStats(out any) {
	if h.store == nil {
		return
	}
	_ = h.store.RecordAnswer(out)
}
