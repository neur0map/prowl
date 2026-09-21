package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/docs"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/sketch"
)

var falseHint = false

func readOnlyAnnotations(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &falseHint}
}

func registerCoreTools(server *sdk.Server, h *handlers) {
	sdk.AddTool(server, &sdk.Tool{Name: "search_context", Description: "Answer a 'where is X / how does X work / which files implement feature Y' question about this repo with a small, cited context packet (file:line evidence) instead of grepping and reading whole files. It ranks the whole repo by relevance, so it locates a feature, module, or concept even when the term is scattered across many files -- prefer it over grep for finding code. Prowl reindexes changed files before answering, so results reflect the current working tree. Set rerank=true on a relevance-sensitive question to have your own model reorder candidates (one extra model call, no local model needed). Then call get_context for a chosen item.", Annotations: readOnlyAnnotations("Search context")}, tracked(h, h.searchContext))
	sdk.AddTool(server, &sdk.Tool{Name: "find", Description: "Locate where a symbol is defined -- a function, type, class, component, setting, keybind, or id -- by name (exact or fuzzy), ranked and cited to file:line. Your first move for 'where is X defined' or 'which file defines X', instead of globbing filenames or grepping the tree. Returns matching definitions, not file contents; read one with read_symbol or a file's shape with outline. For a feature or concept rather than a named symbol, use search_context. Read-only.", Annotations: readOnlyAnnotations("Find symbol")}, tracked(h, h.findSymbol))
	sdk.AddTool(server, &sdk.Tool{Name: "get_context", Description: "Fetch fuller detail for specific context IDs returned by search_context, within an explicit token budget (mode compact, standard, or full). Use after search_context when the packet summary is not enough for a chosen item. This does not search; call search_context to find items first.", Annotations: readOnlyAnnotations("Get context")}, tracked(h, h.getContext))
	sdk.AddTool(server, &sdk.Tool{Name: "search_docs", Description: "Answer a question from external documentation ingested with `prowl docs add` (library/framework docs crawled to Markdown, or a local Markdown tree): a small, cited, budget-bounded context packet instead of fetching whole doc pages. Searches the shared documentation corpus, not this repo's code; use search_context for code. Read-only.", Annotations: readOnlyAnnotations("Search docs")}, tracked(h, h.searchDocs))
	sdk.AddTool(server, &sdk.Tool{Name: "analyze_change", Description: "Report the structural blast radius of a project-relative file: which files and subsystems depend on it and would be affected if you change it. Use before editing to size the risk. Read-only; returns dependents, not file contents.", Annotations: readOnlyAnnotations("Analyze change")}, tracked(h, h.analyzeChange))
	sdk.AddTool(server, &sdk.Tool{Name: "propose_knowledge_change", Description: "Propose durable project knowledge for human review (an OKF proposal); a person approves it later. This never writes accepted knowledge. Use to record a lasting architecture fact or decision, not transient notes.", Annotations: &sdk.ToolAnnotations{Title: "Propose knowledge change", ReadOnlyHint: false, DestructiveHint: &falseHint, OpenWorldHint: &falseHint}}, tracked(h, h.proposeKnowledge))
	sdk.AddTool(server, &sdk.Tool{Name: "validate_knowledge", Description: "Check that stored project knowledge is well-formed: evidence anchors still resolve, links are valid, and entries are current. Use before relying on or proposing knowledge. Read-only.", Annotations: readOnlyAnnotations("Validate knowledge")}, tracked(h, h.validateKnowledge))
	sdk.AddTool(server, &sdk.Tool{Name: "search_capabilities", Description: "List Prowl's built-in workflows and their metadata (token-lean) before fetching full details. Use to discover what Prowl offers; to search your code use search_context instead.", Annotations: readOnlyAnnotations("Search capabilities")}, tracked(h, h.searchCapabilities))
	sdk.AddTool(server, &sdk.Tool{Name: "read_symbol", Description: "Read one symbol's source (its signature and body), cited and bounded, resolved by name or by a numeric id from a find result. Use it to read a single function, type, or component instead of the whole file. Read-only.", Annotations: readOnlyAnnotations("Read symbol")}, tracked(h, h.readSymbol))
	sdk.AddTool(server, &sdk.Tool{Name: "span", Description: "Where a symbol currently is: its file, exact line range, and a content digest, resolved by name or by a numeric id from a find result. Use it to re-ground stale line numbers after your own edits and to detect drift before editing -- the digest hashes the symbol's current body bytes (not its position), so it stays the same when only surrounding lines move and changes when the body itself changes. When a name is ambiguous it returns every match so the choice is visible. Read-only.", Annotations: readOnlyAnnotations("Symbol span")}, tracked(h, h.span))
	sdk.AddTool(server, &sdk.Tool{Name: "history", Description: "Commits that touched a symbol, newest first (short SHA, author, relative date, subject): the 'why is this code the way it is' lookup, resolved by name or by a numeric id from a find result, instead of running git log by hand. The range traced is the symbol's CURRENT location in the index, and because `git log -L` cannot follow renames, history stops at a rename of the file. When a name matches several symbols, every exact match is traced and each commit is tagged with its file. Read-only.", Annotations: readOnlyAnnotations("Symbol history")}, tracked(h, h.history))
	sdk.AddTool(server, &sdk.Tool{Name: "outline", Description: "Show a file's structure: every symbol it defines with kind, signature, nesting depth, and line range, but no bodies. Use it to grasp a file from a handful of signature lines instead of reading the whole file (far cheaper on tokens), then read_symbol only the parts you need. Read-only.", Annotations: readOnlyAnnotations("Outline file")}, tracked(h, h.outline))
	sdk.AddTool(server, &sdk.Tool{Name: "find_references", Description: "Find where a symbol is used across the repo: cited {file, line, text} call sites (and reference edges for config/resource symbols), resolved by name or by a numeric id. Use it for the callers of a function or the blast radius of changing a symbol, instead of grepping and reading files. Read-only; call sites are name-usage based, not a language call graph, so a comment or same-named symbol may slip in.", Annotations: readOnlyAnnotations("Find references")}, tracked(h, h.findReferencesByName))
	sdk.AddTool(server, &sdk.Tool{Name: "sketch_ui", Description: "Sketch how a UI looks and behaves, from source, without a screenshot or running it. QML and React (jsx/tsx) give the element tree with each element's visual properties (layout, color, text) and behavior (handlers, animations, conditional rendering); a Go/lipgloss TUI gives the color palette and named styles; CSS/SCSS gives design tokens and rules. QML token references are resolved to their literal values. Argument is a component name (resolved through the index) or a file path. Use it to understand or replicate a UI instead of reading the whole file. Read-only.", Annotations: readOnlyAnnotations("Sketch UI")}, tracked(h, h.sketchUI))
}

type contextSearchIn struct {
	Query        string `json:"query" jsonschema:"project question"`
	Mode         string `json:"mode,omitempty" jsonschema:"compact, standard, or full"`
	BudgetTokens int    `json:"budget_tokens,omitempty" jsonschema:"estimated token budget"`
	BudgetBytes  int    `json:"budget_bytes,omitempty" jsonschema:"optional byte budget"`
	Synthesize   bool   `json:"synthesize,omitempty" jsonschema:"request optional client-side semantic synthesis"`
	Rerank       bool   `json:"rerank,omitempty" jsonschema:"reorder results for better relevance; uses the configured AI backend (local model or a coding-agent CLI) when AI is enabled, else your host model via sampling if supported"`
}

type contextGetIn struct {
	IDs          []string `json:"ids" jsonschema:"context item IDs"`
	Mode         string   `json:"mode,omitempty" jsonschema:"compact, standard, or full"`
	BudgetTokens int      `json:"budget_tokens,omitempty"`
	BudgetBytes  int      `json:"budget_bytes,omitempty"`
}

type anchorIn struct {
	Path      string `json:"path" jsonschema:"bundle-relative source path"`
	Symbol    string `json:"symbol,omitempty" jsonschema:"symbol to track (preferred over a line range)"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
}

type proposalIn struct {
	Target    string     `json:"target" jsonschema:"bundle-relative destination path"`
	Candidate string     `json:"candidate,omitempty" jsonschema:"complete OKF Markdown candidate; omit to author from the fields below"`
	Type      string     `json:"type,omitempty" jsonschema:"candidate type when authoring from fields (Decision, Claim, ...)"`
	Title     string     `json:"title,omitempty" jsonschema:"candidate title when authoring from fields"`
	Body      string     `json:"body,omitempty" jsonschema:"candidate body text when authoring from fields"`
	Resource  string     `json:"resource,omitempty" jsonschema:"resource evidence URI (e.g. file://path)"`
	Tags      []string   `json:"tags,omitempty"`
	Anchors   []anchorIn `json:"anchors,omitempty" jsonschema:"source anchors; each needs a path plus a symbol or line range"`
	Author    string     `json:"author,omitempty"`
}

type capabilitySearchIn struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type readSymbolIn struct {
	Symbol string `json:"symbol" jsonschema:"symbol name, or a numeric id from a find result"`
}

type spanIn struct {
	Symbol string `json:"symbol" jsonschema:"symbol name, or a numeric id from a find result"`
}

type spansOut struct {
	Spans []query.SymbolSpan `json:"spans"`
}

type historyIn struct {
	Symbol string `json:"symbol" jsonschema:"symbol name, or a numeric id from a find result"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max commits per matching symbol (0 = default)"`
}

type historyOut struct {
	Commits []query.SymbolCommit `json:"commits"`
}

type outlineIn struct {
	Path string `json:"path" jsonschema:"repo-relative file path"`
}

type findReferencesIn struct {
	Symbol string `json:"symbol" jsonschema:"symbol name, or a numeric id from a find result"`
}

type sketchIn struct {
	Target string `json:"target" jsonschema:"a UI component name (resolved through the index) or a file path; QML, React (jsx/tsx), Go/lipgloss, or CSS"`
}

type sketchOut struct {
	File   string `json:"file"`
	Sketch string `json:"sketch"`
}

type proposalOut struct {
	Proposal *knowledge.Proposal `json:"proposal"`
	Diff     string              `json:"diff"`
}

type findingsOut struct {
	Findings []knowledge.Finding `json:"findings"`
}

type capabilitiesOut struct {
	Capabilities []capability.Summary `json:"capabilities"`
}

func (h *handlers) searchContext(ctx context.Context, request *sdk.CallToolRequest, in contextSearchIn) (*sdk.CallToolResult, contextpacket.Packet, error) {
	if h.context == nil {
		return nil, contextpacket.Packet{}, fmt.Errorf("context service unavailable")
	}
	mode := contextpacket.Mode(in.Mode)
	if mode == "" {
		mode = contextpacket.ModeCompact
	}
	if in.BudgetTokens == 0 && in.BudgetBytes == 0 && mode != contextpacket.ModeFull {
		in.BudgetTokens = 1800
	}
	searchRequest := contextpacket.Request{Question: in.Query, Mode: mode, BudgetTokens: in.BudgetTokens, BudgetBytes: in.BudgetBytes}
	// Reranking prefers the local model (a direct LLM integration -- the MCP
	// 2026-07-28 replacement for deprecated sampling), which the service already
	// applies when AI is enabled. Fall back to host sampling only when no local
	// model is configured and the client advertises the capability.
	if in.Rerank && (h.context.Reranker == nil) && request != nil && sessionSupportsSampling(request.Session) {
		searchRequest.Reranker = samplingReranker{ctx: ctx, session: request.Session}
	}
	packet, err := h.context.Search(searchRequest)
	if err == nil && in.Synthesize {
		packet = synthesizePacket(ctx, request, packet)
	}
	packet = contextpacket.CanonicalProjection(packet)
	return packetResourceLinks(packet), packet, err
}

func (h *handlers) readSymbol(_ context.Context, _ *sdk.CallToolRequest, in readSymbolIn) (*sdk.CallToolResult, query.Definition, error) {
	def, err := h.q.Definition(h.root, in.Symbol)
	if err != nil {
		return nil, query.Definition{}, err
	}
	return nil, def, nil
}

func (h *handlers) span(_ context.Context, _ *sdk.CallToolRequest, in spanIn) (*sdk.CallToolResult, spansOut, error) {
	spans, err := h.q.Span(h.root, in.Symbol)
	if err != nil {
		return nil, spansOut{}, err
	}
	return nil, spansOut{Spans: spans}, nil
}

func (h *handlers) history(ctx context.Context, _ *sdk.CallToolRequest, in historyIn) (*sdk.CallToolResult, historyOut, error) {
	commits, err := h.q.History(ctx, h.root, in.Symbol, in.Limit)
	if err != nil {
		return nil, historyOut{}, err
	}
	return nil, historyOut{Commits: commits}, nil
}

func (h *handlers) outline(_ context.Context, _ *sdk.CallToolRequest, in outlineIn) (*sdk.CallToolResult, query.FileOutline, error) {
	o, err := h.q.Outline(in.Path)
	if err != nil {
		return nil, query.FileOutline{}, err
	}
	return nil, o, nil
}

func (h *handlers) findReferencesByName(_ context.Context, _ *sdk.CallToolRequest, in findReferencesIn) (*sdk.CallToolResult, query.Usages, error) {
	u, err := h.q.References(in.Symbol)
	if err != nil {
		return nil, query.Usages{}, err
	}
	return nil, u, nil
}

func (h *handlers) sketchUI(_ context.Context, _ *sdk.CallToolRequest, in sketchIn) (*sdk.CallToolResult, sketchOut, error) {
	path, src, err := h.resolveSketch(in.Target)
	if err != nil {
		return nil, sketchOut{}, err
	}
	sk, err := sketch.Of(path, src)
	if err != nil {
		return nil, sketchOut{}, err
	}
	if qs, ok := sk.(*sketch.Sketch); ok && h.root != "" {
		qs.Resolve(sketch.DirSingletonSource(h.root, path))
	}
	return nil, sketchOut{File: path, Sketch: sk.Text()}, nil
}

// resolveSketch turns a sketch target into a file path and its bytes: a path on
// disk (absolute or project-relative) is read directly; otherwise the target is
// a component name resolved through the index.
func (h *handlers) resolveSketch(target string) (string, []byte, error) {
	for _, p := range []string{target, filepath.Join(h.root, target)} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			src, err := os.ReadFile(p)
			if err != nil {
				return "", nil, err
			}
			return p, src, nil
		}
	}
	if strings.ContainsRune(target, filepath.Separator) || strings.Contains(target, ".") {
		return "", nil, fmt.Errorf("file not found: %s", target)
	}
	def, err := h.q.Definition(h.root, target)
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(h.root, filepath.FromSlash(def.File))
	src, err := os.ReadFile(p)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", def.File, err)
	}
	return def.File, src, nil
}

func synthesizePacket(ctx context.Context, request *sdk.CallToolRequest, packet contextpacket.Packet) contextpacket.Packet {
	if request == nil || request.Session == nil {
		return packet
	}
	initialized := request.Session.InitializeParams()
	if initialized == nil || initialized.Capabilities == nil || initialized.Capabilities.Sampling == nil {
		return packet
	}
	type compactItem struct {
		Title     string                   `json:"title"`
		Summary   string                   `json:"summary"`
		Freshness string                   `json:"freshness"`
		Citations []contextpacket.Citation `json:"citations"`
	}
	items := make([]compactItem, 0, min(len(packet.Items), 12))
	for _, item := range packet.Items {
		if len(items) == 12 {
			break
		}
		items = append(items, compactItem{Title: boundedText(item.Title, 160), Summary: boundedText(item.Summary, 320), Freshness: item.Freshness, Citations: item.Citations})
	}
	payload, _ := json.Marshal(map[string]any{"deterministic_summary": boundedText(packet.Summary, 500), "items": items})
	result, err := request.Session.CreateMessage(ctx, &sdk.CreateMessageParams{
		MaxTokens:    160,
		SystemPrompt: "Summarize only the supplied local evidence in at most three sentences. Preserve uncertainty and freshness warnings. Do not add facts.",
		Messages:     []*sdk.SamplingMessage{{Role: sdk.Role("user"), Content: &sdk.TextContent{Text: string(payload)}}},
		Temperature:  0.1,
		ModelPreferences: &sdk.ModelPreferences{
			CostPriority: 0.7, SpeedPriority: 0.7, IntelligencePriority: 0.4,
		},
	})
	if err != nil {
		return packet
	}
	text, ok := result.Content.(*sdk.TextContent)
	if !ok || strings.TrimSpace(text.Text) == "" {
		return packet
	}
	packet.Summary = boundedText(strings.TrimSpace(text.Text), 1200)
	return packet
}

func boundedText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func (h *handlers) getContext(_ context.Context, _ *sdk.CallToolRequest, in contextGetIn) (*sdk.CallToolResult, contextpacket.Packet, error) {
	if h.context == nil {
		return nil, contextpacket.Packet{}, fmt.Errorf("context service unavailable")
	}
	mode := contextpacket.Mode(in.Mode)
	if mode == "" {
		mode = contextpacket.ModeCompact
	}
	if in.BudgetTokens == 0 && in.BudgetBytes == 0 && mode != contextpacket.ModeFull {
		in.BudgetTokens = 1800
	}
	packet, err := h.context.Get(contextpacket.Request{IDs: in.IDs, Mode: mode, BudgetTokens: in.BudgetTokens, BudgetBytes: in.BudgetBytes})
	packet = contextpacket.CanonicalProjection(packet)
	return packetResourceLinks(packet), packet, err
}

type docsSearchIn struct {
	Query        string `json:"query" jsonschema:"question to answer from ingested documentation"`
	BudgetTokens int    `json:"budget_tokens,omitempty" jsonschema:"estimated token budget (default 1800)"`
}

func (h *handlers) searchDocs(_ context.Context, _ *sdk.CallToolRequest, in docsSearchIn) (*sdk.CallToolResult, contextpacket.Packet, error) {
	home, err := docs.Home()
	if err != nil {
		return nil, contextpacket.Packet{}, err
	}
	budget := in.BudgetTokens
	if budget == 0 {
		budget = 1800
	}
	packet, err := docs.Search(home, in.Query, budget)
	if err != nil {
		return nil, contextpacket.Packet{}, err
	}
	packet = contextpacket.CanonicalProjection(packet)
	return packetResourceLinks(packet), packet, nil
}

func (h *handlers) analyzeChange(_ context.Context, _ *sdk.CallToolRequest, in pathIn) (*sdk.CallToolResult, query.BlastSummary, error) {
	if h.q == nil {
		return nil, query.BlastSummary{}, fmt.Errorf("query service unavailable")
	}
	result, err := h.q.BlastSummarize(in.Path)
	return resourceLinkResult(sourceLink(in.Path)), result, err
}

func (h *handlers) proposeKnowledge(ctx context.Context, request *sdk.CallToolRequest, in proposalIn) (*sdk.CallToolResult, proposalOut, error) {
	if h.knowledge == nil {
		return nil, proposalOut{}, fmt.Errorf("knowledge repository unavailable")
	}
	candidate := in.Candidate
	if candidate == "" {
		anchors := make([]knowledge.Anchor, 0, len(in.Anchors))
		for _, a := range in.Anchors {
			anchors = append(anchors, knowledge.Anchor{Path: a.Path, Symbol: a.Symbol, LineStart: a.LineStart, LineEnd: a.LineEnd})
		}
		built, err := okfv01.BuildCandidate(okfv01.CaptureInput{
			Type: in.Type, Title: in.Title, Body: in.Body, Resource: in.Resource, Tags: in.Tags, Anchors: anchors,
		})
		if err != nil {
			return nil, proposalOut{}, err
		}
		candidate = string(built)
	}
	if int64(len(candidate)) > knowledge.MaxDocumentBytes {
		return nil, proposalOut{}, fmt.Errorf("knowledge candidate exceeds %d bytes", knowledge.MaxDocumentBytes)
	}
	if err := approveProposal(ctx, request, in.Target); err != nil {
		return nil, proposalOut{}, err
	}
	staged, err := os.CreateTemp(filepath.Dir(h.knowledge.Root), ".prowl-proposal-*.md")
	if err != nil {
		return nil, proposalOut{}, err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)
	if _, err := staged.WriteString(candidate); err != nil {
		staged.Close()
		return nil, proposalOut{}, err
	}
	if err := staged.Close(); err != nil {
		return nil, proposalOut{}, err
	}
	inbox := knowledge.NewReviewInbox(filepath.Join(filepath.Dir(h.knowledge.Root), "proposals"), h.knowledge)
	proposal, diff, err := inbox.Propose(stagedPath, in.Target, in.Author, h.root, extract.SymbolRange, time.Now())
	return resourceLinkResult(knowledgeIndexLink()), proposalOut{Proposal: proposal, Diff: diff}, err
}

func approveProposal(ctx context.Context, request *sdk.CallToolRequest, target string) error {
	if request != nil && request.Session != nil {
		initialized := request.Session.InitializeParams()
		if initialized != nil && initialized.Capabilities != nil && initialized.Capabilities.Elicitation != nil {
			result, err := request.Session.Elicit(ctx, &sdk.ElicitParams{
				Mode:    "form",
				Message: fmt.Sprintf("Create a review-only knowledge proposal for %q? This will not accept it.", target),
				RequestedSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "description": "Confirm proposal creation"}},
					"required":   []string{"confirm"},
				},
			})
			if err != nil {
				return fmt.Errorf("proposal elicitation failed: %w", err)
			}
			confirmed, _ := result.Content["confirm"].(bool)
			if result.Action != "accept" || !confirmed {
				return fmt.Errorf("proposal creation was not approved")
			}
			return nil
		}
	}
	return fmt.Errorf("client does not support host-approved elicitation; use `prowl knowledge propose` locally")
}

func (h *handlers) validateKnowledge(_ context.Context, _ *sdk.CallToolRequest, _ Empty) (*sdk.CallToolResult, findingsOut, error) {
	if h.knowledge == nil {
		return nil, findingsOut{Findings: []knowledge.Finding{}}, fmt.Errorf("knowledge repository unavailable")
	}
	findings, err := h.knowledge.Lint(h.root, extract.SymbolRange)
	return resourceLinkResult(knowledgeIndexLink()), findingsOut{Findings: findings}, err
}

func (h *handlers) searchCapabilities(_ context.Context, _ *sdk.CallToolRequest, in capabilitySearchIn) (*sdk.CallToolResult, capabilitiesOut, error) {
	if h.capabilities == nil {
		return nil, capabilitiesOut{Capabilities: []capability.Summary{}}, fmt.Errorf("capability catalog unavailable")
	}
	return resourceLinkResult(overviewLink()), capabilitiesOut{Capabilities: h.capabilities.Search(in.Query, in.Limit)}, nil
}

func packetResourceLinks(packet contextpacket.Packet) *sdk.CallToolResult {
	links := make([]*sdk.ResourceLink, 0, len(packet.Items))
	seen := map[string]bool{}
	for _, item := range packet.Items {
		if item.DetailResource == "" || seen[item.DetailResource] {
			continue
		}
		seen[item.DetailResource] = true
		links = append(links, &sdk.ResourceLink{URI: item.DetailResource, Name: item.ID, Title: item.Title, Description: item.Summary})
	}
	if len(links) == 0 {
		links = append(links, overviewLink())
	}
	return resourceLinkResult(links...)
}

func resourceLinkResult(links ...*sdk.ResourceLink) *sdk.CallToolResult {
	content := make([]sdk.Content, 0, len(links))
	for _, link := range links {
		if link != nil {
			content = append(content, link)
		}
	}
	return &sdk.CallToolResult{Content: content}
}

func sourceLink(path string) *sdk.ResourceLink {
	clean := filepath.ToSlash(path)
	return &sdk.ResourceLink{URI: "prowl://workspace/current/source/" + url.PathEscape(clean), Name: clean, Title: "Changed source"}
}

func knowledgeIndexLink() *sdk.ResourceLink {
	return &sdk.ResourceLink{URI: "prowl://workspace/current/knowledge/index", Name: "knowledge-index", Title: "Knowledge index", MIMEType: "text/markdown"}
}

func overviewLink() *sdk.ResourceLink {
	return &sdk.ResourceLink{URI: "prowl://workspace/current/overview", Name: "workspace-overview", Title: "Workspace overview", MIMEType: "application/json"}
}
