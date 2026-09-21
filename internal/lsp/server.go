package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/store"
)

// Server is a Language Server backed by the prowl index. It is safe for the
// single reader goroutine plus a background reindex/publish path.
type Server struct {
	Root    string
	Version string

	store       *store.Store
	rules       config.Rules
	reindex     func() error // incremental reindex (index + resolve); may be nil
	readGuard   store.ReadGuard
	stateMu     sync.Mutex
	indexErr    error
	nextRefresh uint64

	mu   sync.Mutex
	docs map[string]string // uri -> full text (textDocumentSync = full)

	wmu sync.Mutex
	out *bufio.Writer
}

// New builds a Server over an open store. reindex, when non-nil, is run on save
// to refresh the index before diagnostics are recomputed.
func New(root, version string, s *store.Store, rules config.Rules, reindex func() error) *Server {
	return &Server{Root: root, Version: version, store: s, rules: rules, reindex: reindex, docs: map[string]string{}}
}

// WithReadGuard pins each logical LSP request to one published generation.
func (s *Server) WithReadGuard(guard store.ReadGuard) *Server {
	s.readGuard = guard
	return s
}

// BeginIndexRefresh reserves a monotonic completion ID before refresh work starts.
func (s *Server) BeginIndexRefresh() uint64 {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.nextRefresh++
	return s.nextRefresh
}

// CompleteIndexRefresh applies a result only if no newer-started refresh has
// already completed. It returns whether this result became authoritative.
func (s *Server) CompleteIndexRefresh(id uint64, err error) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if id != s.nextRefresh {
		return false
	}
	s.indexErr = err
	return true
}

// SetIndexError records an asynchronous watcher/runtime error as the newest
// health observation. Refresh callers should use Begin/CompleteIndexRefresh.
func (s *Server) SetIndexError(err error) {
	id := s.BeginIndexRefresh()
	s.CompleteIndexRefresh(id, err)
}

func (s *Server) beginRead(ctx context.Context) (func(), error) {
	s.stateMu.Lock()
	stateErr := s.indexErr
	s.stateMu.Unlock()
	if stateErr != nil {
		return nil, stateErr
	}
	release := func() {}
	if s.readGuard != nil {
		var err error
		release, err = s.readGuard(ctx)
		if err != nil {
			return nil, err
		}
		if err := s.store.RequirePublishedGeneration(); err != nil {
			release()
			return nil, err
		}
	}
	return release, nil
}

// RepublishOpen recomputes diagnostics for every open document, used after an
// external reindex (file watcher) so editor squiggles stay current.
func (s *Server) RepublishOpen() {
	s.mu.Lock()
	uris := make([]string, 0, len(s.docs))
	for u := range s.docs {
		uris = append(uris, u)
	}
	s.mu.Unlock()
	for _, u := range uris {
		s.publishDiagnostics(u)
	}
}

// Run reads LSP frames from in and writes replies to out until EOF or exit.
// Inputs may implement CancelRead to provide an OS-specific interruption path;
// ordinary closers retain close-to-cancel behavior for in-memory pipes/tests.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	s.out = bufio.NewWriter(out)
	stopCancel := make(chan struct{})
	var cancelRead func() error
	if input, ok := in.(interface{ CancelRead() error }); ok {
		cancelRead = input.CancelRead
	} else if closer, ok := in.(io.Closer); ok {
		cancelRead = closer.Close
	}
	if cancelRead != nil {
		cancelDone := make(chan struct{})
		go func() {
			defer close(cancelDone)
			select {
			case <-ctx.Done():
				_ = cancelRead()
			case <-stopCancel:
			}
		}()
		defer func() {
			close(stopCancel)
			<-cancelDone
		}()
	} else {
		close(stopCancel)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, err := readFrame(r)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		if stop := s.dispatch(ctx, frame); stop {
			return nil
		}
	}
}

// readFrame reads one Content-Length framed JSON-RPC message body.
func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLen := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.IndexByte(line, ':'); i >= 0 &&
			strings.EqualFold(strings.TrimSpace(line[:i]), "Content-Length") {
			contentLen, _ = strconv.Atoi(strings.TrimSpace(line[i+1:]))
		}
	}
	if contentLen < 0 {
		return nil, fmt.Errorf("lsp: missing Content-Length header")
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// dispatch routes one frame. It returns true when the loop should stop (exit).
func (s *Server) dispatch(ctx context.Context, frame []byte) bool {
	var msg rpcMessage
	if err := json.Unmarshal(frame, &msg); err != nil {
		return false
	}
	if msg.Method == "" {
		return false // a response to a server->client request; we send none that need replies
	}
	if len(msg.ID) == 0 {
		return s.handleNotification(msg.Method, msg.Params)
	}
	result, rerr := s.handleRequest(ctx, msg.Method, msg.Params)
	s.writeResponse(msg.ID, result, rerr)
	return false
}

func (s *Server) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	if method != "initialize" && method != "shutdown" {
		release, err := s.beginRead(ctx)
		if err != nil {
			return nil, &rpcError{Code: errInternal, Message: err.Error()}
		}
		defer release()
	}
	switch method {
	case "initialize":
		return s.initialize(), nil
	case "shutdown":
		return nil, nil
	case "textDocument/definition":
		return s.definition(params)
	case "textDocument/references":
		return s.references(params)
	case "textDocument/hover":
		return s.hover(params)
	case "textDocument/documentSymbol":
		return s.documentSymbol(params)
	case "workspace/symbol":
		return s.workspaceSymbol(params)
	case "textDocument/codeLens":
		return s.codeLens(params)
	case "textDocument/completion":
		return s.completion(params)
	default:
		return nil, &rpcError{Code: errMethodNotFound, Message: "unsupported method: " + method}
	}
}

func (s *Server) handleNotification(method string, params json.RawMessage) (stop bool) {
	switch method {
	case "exit":
		return true
	case "initialized":
		return false
	case "textDocument/didOpen":
		var p didOpenParams
		if json.Unmarshal(params, &p) == nil {
			s.setDoc(p.TextDocument.URI, p.TextDocument.Text)
			s.publishDiagnostics(p.TextDocument.URI)
		}
	case "textDocument/didChange":
		var p didChangeParams
		if json.Unmarshal(params, &p) == nil && len(p.ContentChanges) > 0 {
			s.setDoc(p.TextDocument.URI, p.ContentChanges[len(p.ContentChanges)-1].Text)
		}
	case "textDocument/didSave":
		var p didSaveParams
		if json.Unmarshal(params, &p) == nil {
			if p.Text != nil {
				s.setDoc(p.TextDocument.URI, *p.Text)
			}
			if s.reindex != nil {
				refreshID := s.BeginIndexRefresh()
				err := s.reindex()
				if !s.CompleteIndexRefresh(refreshID, err) || err != nil {
					return false
				}
			}
			s.publishDiagnostics(p.TextDocument.URI)
		}
	case "textDocument/didClose":
		var p didCloseParams
		if json.Unmarshal(params, &p) == nil {
			s.delDoc(p.TextDocument.URI)
			s.sendDiagnostics(p.TextDocument.URI, []Diagnostic{})
		}
	}
	return false
}

func (s *Server) initialize() initializeResult {
	return initializeResult{
		Capabilities: serverCapabilities{
			TextDocumentSync:        1, // full
			DefinitionProvider:      true,
			ReferencesProvider:      true,
			HoverProvider:           true,
			DocumentSymbolProvider:  true,
			WorkspaceSymbolProvider: true,
			CodeLensProvider:        &codeLensOptions{ResolveProvider: false},
			CompletionProvider:      &completionOptions{TriggerCharacters: []string{"$", "@"}},
		},
		ServerInfo: serverInfo{Name: "prowl", Version: s.Version},
	}
}

// --- document cache ---

func (s *Server) setDoc(uri, text string) {
	s.mu.Lock()
	s.docs[uri] = text
	s.mu.Unlock()
}

func (s *Server) delDoc(uri string) {
	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

// docText returns the open buffer for uri, falling back to the file on disk.
func (s *Server) docText(uri string) string {
	s.mu.Lock()
	t, ok := s.docs[uri]
	s.mu.Unlock()
	if ok {
		return t
	}
	if b, err := os.ReadFile(uriToPath(uri)); err == nil {
		return string(b)
	}
	return ""
}

// --- writers ---

func (s *Server) writeResponse(id json.RawMessage, result any, rerr *rpcError) {
	s.writeMessage(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
}

func (s *Server) writeMessage(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.out == nil {
		return
	}
	fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(data))
	s.out.Write(data)
	s.out.Flush()
}
