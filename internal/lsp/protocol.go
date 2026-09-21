// Package lsp serves Prowl's index to editors over the Language Server
// Protocol (stdio). It is the human-facing counterpart to the MCP server: both
// read the same per-project index, so a developer gets cross-file navigation,
// references, hover, and inline health for config formats that usually have no
// language server.
package lsp

import "encoding/json"

// rpcMessage is a decoded JSON-RPC 2.0 frame. id is absent for notifications.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Position is zero-based line and character (UTF-16; treated as byte for ASCII).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type contentChange struct {
	Text string `json:"text"`
}

type didChangeParams struct {
	TextDocument   textDocumentIdentifier `json:"textDocument"`
	ContentChanges []contentChange        `json:"contentChanges"`
}

type didSaveParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Text         *string                `json:"text,omitempty"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type referenceParams struct {
	textDocumentPositionParams
	Context struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}

type completionParams struct {
	textDocumentPositionParams
}

// MarkupContent carries hover text (markdown).
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// DocumentSymbol is a hierarchical outline entry.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// SymbolInformation is a flat workspace symbol with a location.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

type command struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}

// CodeLens is an actionable annotation above a line.
type CodeLens struct {
	Range   Range    `json:"range"`
	Command *command `json:"command,omitempty"`
}

// CompletionItem is a single completion candidate.
type CompletionItem struct {
	Label         string         `json:"label"`
	Kind          int            `json:"kind,omitempty"`
	Detail        string         `json:"detail,omitempty"`
	Documentation *MarkupContent `json:"documentation,omitempty"`
	InsertText    string         `json:"insertText,omitempty"`
	SortText      string         `json:"sortText,omitempty"`
}

type completionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []CompletionItem `json:"items"`
}

// Diagnostic is one inline problem.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Source   string `json:"source"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
	ServerInfo   serverInfo         `json:"serverInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type serverCapabilities struct {
	TextDocumentSync        int                `json:"textDocumentSync"`
	DefinitionProvider      bool               `json:"definitionProvider"`
	ReferencesProvider      bool               `json:"referencesProvider"`
	HoverProvider           bool               `json:"hoverProvider"`
	DocumentSymbolProvider  bool               `json:"documentSymbolProvider"`
	WorkspaceSymbolProvider bool               `json:"workspaceSymbolProvider"`
	CodeLensProvider        *codeLensOptions   `json:"codeLensProvider,omitempty"`
	CompletionProvider      *completionOptions `json:"completionProvider,omitempty"`
}

type codeLensOptions struct {
	ResolveProvider bool `json:"resolveProvider"`
}

type completionOptions struct {
	TriggerCharacters []string `json:"triggerCharacters,omitempty"`
}

// LSP enum subsets used by handlers.
const (
	diagError = 1
	diagWarn  = 2
	diagInfo  = 3
	diagHint  = 4

	symFile     = 1
	symModule   = 2
	symFunction = 12
	symVariable = 13
	symConstant = 14
	symString   = 15
	symKey      = 20
	symObject   = 19

	cmplVariable = 6
	cmplColor    = 16
	cmplFunction = 3
	cmplFile     = 17
	cmplValue    = 12
)

const (
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)
