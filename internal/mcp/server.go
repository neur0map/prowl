package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/store"
)

type accessInfo struct {
	count    int
	lastSeen time.Time
}

// projectProp is the shared JSON schema for the optional "project" parameter.
var projectProp = map[string]interface{}{
	"type":        "string",
	"description": "Which project to query: \"primary\" (default) or \"comparison\" (cloned repo)",
	"enum":        []string{"primary", "comparison"},
}

// Server handles MCP protocol communication over stdio.
type Server struct {
	store      *store.Store
	embedder   *embed.Embedder
	contextDir string
	heat       map[string]accessInfo

	// Comparison repo (single slot, set by prowl_clone).
	compareStore      *store.Store
	compareContextDir string
	compareRepo       string // "owner/repo" label
}

// New creates an MCP server.
func New(st *store.Store, embedder *embed.Embedder, contextDir string) *Server {
	return &Server{store: st, embedder: embedder, contextDir: contextDir, heat: make(map[string]accessInfo)}
}

// recordAccess tracks that a file was accessed via an MCP tool.
func (s *Server) recordAccess(path string) {
	info := s.heat[path]
	info.count++
	info.lastSeen = time.Now()
	s.heat[path] = info
}

// heatScore returns the heat score for a file (0.0 to ~1.0).
// Formula: sigmoid(ln(1 + count)) * exp(-age / halfLife)
// Half-life is 1 hour.
func (s *Server) heatScore(path string) float64 {
	info, ok := s.heat[path]
	if !ok {
		return 0.0
	}
	sigmoid := 1.0 / (1.0 + math.Exp(-math.Log1p(float64(info.count))))
	age := time.Since(info.lastSeen).Seconds()
	decay := math.Exp(-age / (3600.0 * math.Ln2))
	return sigmoid * decay
}

// Close releases resources held by the server (e.g. comparison store).
func (s *Server) Close() {
	if s.compareStore != nil {
		s.compareStore.Close()
		s.compareStore = nil
		s.compareRepo = ""
		s.compareContextDir = ""
	}
}

// storeFor inspects the "project" field in raw JSON arguments and returns
// the appropriate store and context directory. Returns the primary store by
// default, or the comparison store when project=="comparison".
func (s *Server) storeFor(params json.RawMessage) (*store.Store, string, error) {
	var p struct {
		Project string `json:"project"`
	}
	if len(params) > 0 {
		json.Unmarshal(params, &p)
	}
	if p.Project == "" || p.Project == "primary" {
		return s.store, s.contextDir, nil
	}
	if p.Project == "comparison" {
		if s.compareStore == nil {
			return nil, "", fmt.Errorf("no comparison repo loaded — call prowl_clone first")
		}
		return s.compareStore, s.compareContextDir, nil
	}
	return nil, "", fmt.Errorf("unknown project value: %s (use \"primary\" or \"comparison\")", p.Project)
}

// Run starts the JSON-RPC stdio loop using stdin/stdout. Blocks until stdin closes.
func (s *Server) Run() error {
	return s.RunWith(os.Stdin, os.Stdout)
}

// RunWith starts the JSON-RPC loop using the provided reader and writer.
// This allows testing without stdin/stdout.
func (s *Server) RunWith(r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.writeError(w, nil, -32700, "Parse error")
			continue
		}

		s.handleRequest(w, req)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC types
// ---------------------------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// Request dispatch
// ---------------------------------------------------------------------------

func (s *Server) handleRequest(w io.Writer, req jsonRPCRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "notifications/initialized":
		// notification, no response needed
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, req)
	default:
		s.writeError(w, req.ID, -32601, "Method not found: "+req.Method)
	}
}

// ---------------------------------------------------------------------------
// Method handlers
// ---------------------------------------------------------------------------

func (s *Server) handleInitialize(w io.Writer, req jsonRPCRequest) {
	s.writeResult(w, req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "prowl",
			"version": "0.1.0",
		},
	})
}

func (s *Server) handleToolsList(w io.Writer, req jsonRPCRequest) {
	s.writeResult(w, req.ID, map[string]interface{}{
		"tools": []map[string]interface{}{
			{
				"name":        "prowl_overview",
				"description": "Get a structured summary of the entire codebase. Agent's first call on any project. Returns file/symbol/edge counts, language breakdown, community clusters, and key processes.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"project": projectProp,
					},
				},
			},
			{
				"name":        "prowl_file_context",
				"description": "Get all context for a single file: exports, signatures, imports, calls, callers, upstream dependencies, and community. Returns structured JSON.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Project-relative file path (e.g. src/auth.ts)",
						},
						"project": projectProp,
					},
					"required": []string{"path"},
				},
			},
			{
				"name":        "prowl_scope",
				"description": "Given a task description, returns exactly the files and context needed. Combines semantic search with 1-hop graph expansion. One call replaces the entire exploration phase.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task": map[string]interface{}{
							"type":        "string",
							"description": "Natural language task description (e.g. 'fix the template installer')",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of files to return (default: 10)",
							"default":     10,
						},
						"project": projectProp,
					},
					"required": []string{"task"},
				},
			},
			{
				"name":        "prowl_impact",
				"description": "Blast radius analysis. Given a file (and optionally a symbol), returns all direct and transitive dependents that would be affected by changing it. Use before making edits to understand risk.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{
							"type":        "string",
							"description": "Project-relative file path to analyze",
						},
						"symbol": map[string]interface{}{
							"type":        "string",
							"description": "Optional symbol name to narrow the analysis (e.g. function name)",
						},
						"project": projectProp,
					},
					"required": []string{"path"},
				},
			},
			{
				"name":        "prowl_semantic_search",
				"description": "Search the codebase by meaning. Use when filesystem navigation of .prowl/context/ isn't enough for fuzzy semantic queries like 'where is the auth logic?' or 'password hashing'.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Natural language search query",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of results (default: 5)",
							"default":     5,
						},
						"project": projectProp,
					},
					"required": []string{"query"},
				},
			},
			{
				"name":        "prowl_clone",
				"description": "Clone a GitHub repo as a comparison target. Downloads as tarball (no .git), indexes with the full pipeline, and makes it queryable via project:\"comparison\" on all other tools.",
				"inputSchema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"repo": map[string]interface{}{
							"type":        "string",
							"description": "GitHub repo: owner/repo, full URL, or URL with /tree/branch",
						},
						"ref": map[string]interface{}{
							"type":        "string",
							"description": "Optional git ref (branch, tag, or commit SHA). Defaults to HEAD.",
						},
						"token": map[string]interface{}{
							"type":        "string",
							"description": "Optional GitHub token for private repos",
						},
					},
					"required": []string{"repo"},
				},
			},
			{
				"name":        "prowl_clone_status",
				"description": "Check if a comparison repo is loaded and get its stats.",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
			{
				"name":        "prowl_clone_close",
				"description": "Close the comparison repo and free its resources.",
				"inputSchema": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	})
}

func (s *Server) handleToolsCall(w io.Writer, req jsonRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(w, req.ID, -32602, "Invalid params")
		return
	}

	switch params.Name {
	case "prowl_overview":
		s.handleOverview(w, req.ID, params.Arguments)
	case "prowl_file_context":
		s.handleFileContext(w, req.ID, params.Arguments)
	case "prowl_scope":
		s.handleScope(w, req.ID, params.Arguments)
	case "prowl_impact":
		s.handleImpact(w, req.ID, params.Arguments)
	case "prowl_semantic_search":
		s.handleSemanticSearch(w, req.ID, params.Arguments)
	case "prowl_clone":
		s.handleClone(w, req.ID, params.Arguments)
	case "prowl_clone_status":
		s.handleCloneStatus(w, req.ID)
	case "prowl_clone_close":
		s.handleCloneClose(w, req.ID)
	default:
		s.writeError(w, req.ID, -32602, "Unknown tool: "+params.Name)
	}
}

func (s *Server) handleSemanticSearch(w io.Writer, id interface{}, params json.RawMessage) {
	st, _, err := s.storeFor(params)
	if err != nil {
		s.writeError(w, id, -32602, err.Error())
		return
	}

	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	json.Unmarshal(params, &args)
	if args.Query == "" {
		s.writeError(w, id, -32602, "Missing required parameter: query")
		return
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}

	if s.embedder == nil {
		s.writeError(w, id, -32603, "Embedder not available")
		return
	}

	vecs, err := s.embedder.Encode([]string{args.Query})
	if err != nil {
		s.writeError(w, id, -32603, "Encode error: "+err.Error())
		return
	}

	results, err := st.SearchSimilar(vecs[0], args.Limit)
	if err != nil {
		s.writeError(w, id, -32603, "Search error: "+err.Error())
		return
	}

	var content strings.Builder
	if len(results) == 0 {
		content.WriteString("No results found.")
	} else {
		for i, r := range results {
			fmt.Fprintf(&content, "%d. %s (score: %.4f)\n", i+1, r.FilePath, r.Score)
			if r.Signatures != "" {
				for _, line := range strings.Split(r.Signatures, "\n") {
					fmt.Fprintf(&content, "   %s\n", line)
				}
			}
			content.WriteString("\n")
		}
	}

	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": content.String(),
			},
		},
	})
}

// ---------------------------------------------------------------------------
// Write helpers
// ---------------------------------------------------------------------------

func (s *Server) writeResult(w io.Writer, id interface{}, result interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "%s\n", data)
}

func (s *Server) writeError(w io.Writer, id interface{}, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "%s\n", data)
}
