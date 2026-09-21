package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sample-config"))
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "index.db")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := index.Index(s, root, nil); err != nil {
		t.Fatal(err)
	}
	return New(root, "test", s, config.DefaultRules(), nil), root
}

func req(t *testing.T, srv *Server, method string, params any) any {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, rerr := srv.handleRequest(context.Background(), method, raw)
	if rerr != nil {
		t.Fatalf("%s: rpc error: %s", method, rerr.Message)
	}
	return res
}

func pos(root, rel string, line, ch int) textDocumentPositionParams {
	return textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uriForRel(root, rel)},
		Position:     Position{Line: line, Character: ch},
	}
}

func TestDefinitionInclude(t *testing.T) {
	srv, root := newTestServer(t)
	// hyprland.conf line 2: source = ~/.config/hypr/colors.conf
	res := req(t, srv, "textDocument/definition", pos(root, "hypr/hyprland.conf", 1, 14))
	loc, ok := res.(Location)
	if !ok {
		t.Fatalf("want Location, got %T", res)
	}
	if !strings.HasSuffix(uriToPath(loc.URI), "hypr/colors.conf") {
		t.Errorf("include definition -> %s, want hypr/colors.conf", loc.URI)
	}
}

func TestDefinitionResourceUse(t *testing.T) {
	srv, root := newTestServer(t)
	// hyprland.conf line 6: col.active_border = $accent
	res := req(t, srv, "textDocument/definition", pos(root, "hypr/hyprland.conf", 5, 22))
	loc, ok := res.(Location)
	if !ok {
		t.Fatalf("want Location, got %T", res)
	}
	if !strings.HasSuffix(uriToPath(loc.URI), "hypr/colors.conf") {
		t.Errorf("resource definition -> %s, want hypr/colors.conf", loc.URI)
	}
	if loc.Range.Start.Line != 0 {
		t.Errorf("$accent declared on line 1 (0-based 0), got %d", loc.Range.Start.Line)
	}
}

func TestDefinitionExecScript(t *testing.T) {
	srv, root := newTestServer(t)
	// hyprland.conf line 10: bind = $mod, S, exec, ~/.config/hypr/scripts/screenshot.sh
	res := req(t, srv, "textDocument/definition", pos(root, "hypr/hyprland.conf", 9, 40))
	loc, ok := res.(Location)
	if !ok {
		t.Fatalf("want Location, got %T", res)
	}
	if !strings.HasSuffix(uriToPath(loc.URI), "hypr/scripts/screenshot.sh") {
		t.Errorf("exec definition -> %s, want screenshot.sh", loc.URI)
	}
}

func TestReferencesResource(t *testing.T) {
	srv, root := newTestServer(t)
	// colors.conf line 1: $accent declaration; used in hyprland.conf line 6.
	rp := referenceParams{textDocumentPositionParams: pos(root, "hypr/colors.conf", 0, 1)}
	rp.Context.IncludeDeclaration = true
	res := req(t, srv, "textDocument/references", rp)
	locs, ok := res.([]Location)
	if !ok {
		t.Fatalf("want []Location, got %T", res)
	}
	var hasUse bool
	for _, l := range locs {
		if strings.HasSuffix(uriToPath(l.URI), "hypr/hyprland.conf") {
			hasUse = true
		}
	}
	if !hasUse {
		t.Errorf("references to $accent should include hyprland.conf, got %d locs", len(locs))
	}
}

func TestHoverResource(t *testing.T) {
	srv, root := newTestServer(t)
	res := req(t, srv, "textDocument/hover", pos(root, "hypr/colors.conf", 0, 1))
	h, ok := res.(Hover)
	if !ok {
		t.Fatalf("want Hover, got %T", res)
	}
	if !strings.Contains(h.Contents.Value, "rgb(1e1e2e)") {
		t.Errorf("hover should show the value, got %q", h.Contents.Value)
	}
}

func TestDocumentSymbol(t *testing.T) {
	srv, root := newTestServer(t)
	files, _ := srv.store.AllFiles()
	var rel string
	for _, f := range files {
		if syms, _ := srv.store.SymbolsInFile(f.ID); len(syms) > 0 {
			rel = f.RelPath
			break
		}
	}
	if rel == "" {
		t.Skip("fixture has no symbols")
	}
	res := req(t, srv, "textDocument/documentSymbol", documentSymbolParams{TextDocument: textDocumentIdentifier{URI: uriForRel(root, rel)}})
	ds, ok := res.([]DocumentSymbol)
	if !ok {
		t.Fatalf("want []DocumentSymbol, got %T", res)
	}
	if len(ds) == 0 {
		t.Errorf("documentSymbol(%s) returned nothing", rel)
	}
}

func TestWorkspaceSymbol(t *testing.T) {
	srv, _ := newTestServer(t)
	files, _ := srv.store.AllFiles()
	var name string
	for _, f := range files {
		if syms, _ := srv.store.SymbolsInFile(f.ID); len(syms) > 0 {
			name = syms[0].Name
			break
		}
	}
	if name == "" {
		t.Skip("fixture has no symbols")
	}
	q := name
	if len(q) > 3 {
		q = q[:3]
	}
	res := req(t, srv, "workspace/symbol", workspaceSymbolParams{Query: q})
	si, ok := res.([]SymbolInformation)
	if !ok {
		t.Fatalf("want []SymbolInformation, got %T", res)
	}
	if len(si) == 0 {
		t.Errorf("workspace/symbol %q found nothing", q)
	}
}

func TestCodeLens(t *testing.T) {
	srv, root := newTestServer(t)
	// colors.conf: $accent used once, file included once -> at least one lens.
	res := req(t, srv, "textDocument/codeLens", documentSymbolParams{TextDocument: textDocumentIdentifier{URI: uriForRel(root, "hypr/colors.conf")}})
	cl, ok := res.([]CodeLens)
	if !ok {
		t.Fatalf("want []CodeLens, got %T", res)
	}
	if len(cl) == 0 {
		t.Errorf("expected code lenses for colors.conf")
	}
}

func TestCompletionResources(t *testing.T) {
	srv, root := newTestServer(t)
	res := req(t, srv, "textDocument/completion", completionParams{textDocumentPositionParams: pos(root, "hypr/colors.conf", 0, 0)})
	cl, ok := res.(completionList)
	if !ok {
		t.Fatalf("want completionList, got %T", res)
	}
	var found bool
	for _, it := range cl.Items {
		if it.Label == "$accent" || it.Label == "--accent" {
			found = true
		}
	}
	if !found {
		t.Errorf("completion should offer known resource names, got %d items", len(cl.Items))
	}
}

func TestPublishDiagnostics(t *testing.T) {
	srv, root := newTestServer(t)
	var buf bytes.Buffer
	srv.out = bufio.NewWriter(&buf)

	rep, err := doctor.Run(srv.store, srv.rules, doctor.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var ff string
	for _, f := range rep.Findings {
		if f.File != "" {
			ff = f.File
			break
		}
	}
	if ff == "" {
		t.Skip("fixture produced no file findings")
	}
	srv.publishDiagnostics(uriForRel(root, ff))

	out := buf.String()
	i := strings.Index(out, "\r\n\r\n")
	if i < 0 {
		t.Fatalf("no LSP frame written: %q", out)
	}
	var note struct {
		Method string                   `json:"method"`
		Params publishDiagnosticsParams `json:"params"`
	}
	if err := json.Unmarshal([]byte(out[i+4:]), &note); err != nil {
		t.Fatal(err)
	}
	if note.Method != "textDocument/publishDiagnostics" {
		t.Errorf("method = %q", note.Method)
	}
	if len(note.Params.Diagnostics) == 0 {
		t.Errorf("expected diagnostics for %s", ff)
	}
	if d := note.Params.Diagnostics; len(d) > 0 && d[0].Source != "prowl" {
		t.Errorf("diagnostic source = %q", d[0].Source)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{out: bufio.NewWriter(&buf)}
	srv.writeMessage(map[string]any{"jsonrpc": "2.0", "method": "x"})
	frame, err := readFrame(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatal(err)
	}
	if got["method"] != "x" {
		t.Errorf("roundtrip lost data: %v", got)
	}
}

func TestLSPRefreshFailureBlocksReadsUntilRecovery(t *testing.T) {
	srv, root := newTestServer(t)
	fail := true
	sentinel := "refresh failed"
	srv.reindex = func() error {
		if fail {
			return errors.New(sentinel)
		}
		return nil
	}
	uri := pathToURI(filepath.Join(root, "hypr", "hyprland.conf"))
	params, _ := json.Marshal(didSaveParams{TextDocument: textDocumentIdentifier{URI: uri}})
	srv.handleNotification("textDocument/didSave", params)
	request, _ := json.Marshal(textDocumentPositionParams{TextDocument: textDocumentIdentifier{URI: uri}})
	if _, rerr := srv.handleRequest(context.Background(), "textDocument/definition", request); rerr == nil || !strings.Contains(rerr.Message, sentinel) {
		t.Fatalf("request after failed refresh = %+v, want fail-closed error", rerr)
	}
	fail = false
	srv.handleNotification("textDocument/didSave", params)
	if _, rerr := srv.handleRequest(context.Background(), "textDocument/definition", request); rerr != nil {
		t.Fatalf("request after recovery = %+v", rerr)
	}
}

func TestLSPRefreshCompletionIgnoresStaleResults(t *testing.T) {
	srv, _ := newTestServer(t)
	stale := errors.New("stale refresh failed")
	oldID := srv.BeginIndexRefresh()
	newID := srv.BeginIndexRefresh()
	if !srv.CompleteIndexRefresh(newID, nil) {
		t.Fatal("new success was not applied")
	}
	if srv.CompleteIndexRefresh(oldID, stale) {
		t.Fatal("stale failure was applied")
	}
	if release, err := srv.beginRead(context.Background()); err != nil {
		t.Fatalf("stale failure blocked reads: %v", err)
	} else {
		release()
	}

	oldID = srv.BeginIndexRefresh()
	newID = srv.BeginIndexRefresh()
	if !srv.CompleteIndexRefresh(newID, stale) {
		t.Fatal("new failure was not applied")
	}
	if srv.CompleteIndexRefresh(oldID, nil) {
		t.Fatal("stale success was applied")
	}
	if _, err := srv.beginRead(context.Background()); !errors.Is(err, stale) {
		t.Fatalf("read error = %v, want newest failure", err)
	}
}

type notifyingReadCloser struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *notifyingReadCloser) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

func TestLSPRunCancellationInterruptsIdleInput(t *testing.T) {
	srv, _ := newTestServer(t)
	pipeReader, writer := io.Pipe()
	reader := &notifyingReadCloser{ReadCloser: pipeReader, started: make(chan struct{})}
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, reader, io.Discard) }()
	<-reader.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run remained blocked on idle input after cancellation")
	}
}
