package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/store"
)

// entity is the thing the cursor denotes: a target file, a shared resource, or
// a symbol. It carries both identity (kind+id) and its declaration site.
type entity struct {
	kind  string // "file" | "resource" | "symbol"
	id    int64
	name  string
	file  string // declaration file (project-relative)
	line  int    // declaration line (1-based)
	role  string // file role
	value string // resource value
	sig   string // symbol signature
	rkind string // resource kind or symbol kind
}

func decode[T any](raw json.RawMessage) (T, bool) {
	var v T
	if json.Unmarshal(raw, &v) != nil {
		return v, false
	}
	return v, true
}

// resolveFile maps a document URI to its indexed file id.
func (s *Server) resolveFile(uri string) (rel string, fileID int64, ok bool) {
	rel, in := relFromURI(s.Root, uri)
	if !in {
		return "", 0, false
	}
	f, found, _ := s.store.GetFileByPath(rel)
	if !found {
		return rel, 0, false
	}
	return rel, f.ID, true
}

func (s *Server) edgesAtLine(fileID int64, line1 int) []store.EdgeRow {
	all, err := s.store.EdgesFromFile(fileID)
	if err != nil {
		return nil
	}
	var out []store.EdgeRow
	for _, e := range all {
		if e.Line == line1 {
			out = append(out, e)
		}
	}
	return out
}

func edgeMatchesToken(raw, tok string) bool {
	if raw == "" || tok == "" {
		return false
	}
	if raw == tok || strings.Contains(raw, tok) || strings.Contains(tok, raw) {
		return true
	}
	return path.Base(raw) == tok || path.Base(raw) == path.Base(tok)
}

// bestEdge picks the resolved edge on a line that best matches the token.
func bestEdge(edges []store.EdgeRow, tok string) (store.EdgeRow, bool) {
	var first *store.EdgeRow
	for i := range edges {
		if !edges[i].Resolved {
			continue
		}
		if first == nil {
			first = &edges[i]
		}
		if edgeMatchesToken(edges[i].Raw, tok) {
			return edges[i], true
		}
	}
	if first != nil {
		return *first, true
	}
	return store.EdgeRow{}, false
}

// entityAt resolves what the cursor points to, used by definition/references/hover.
func (s *Server) entityAt(fileID int64, text string, pos Position) (entity, bool) {
	tok, _, _ := tokenAt(lineText(text, pos.Line), pos.Character)
	if tok == "" {
		return entity{}, false
	}
	// 1. An outgoing edge on this line (include / exec / bind / resource use).
	if e, ok := bestEdge(s.edgesAtLine(fileID, pos.Line+1), tok); ok {
		switch e.DstType {
		case "file":
			if f, ok, _ := s.store.GetFileByID(e.DstID); ok {
				return entity{kind: "file", id: f.ID, name: path.Base(f.RelPath), file: f.RelPath, line: 1, role: f.Role}, true
			}
		case "resource":
			if r, ok, _ := s.store.ResourceByID(e.DstID); ok {
				return entity{kind: "resource", id: r.ID, name: r.Name, file: r.File, line: r.Line, value: r.Value, rkind: r.Kind}, true
			}
		}
	}
	// 2. A resource declared on this line.
	if rs, err := s.store.ResourcesInFile(fileID); err == nil {
		for _, r := range rs {
			if r.Line == pos.Line+1 && (r.Name == tok || strings.Contains(tok, r.Name)) {
				return entity{kind: "resource", id: r.ID, name: r.Name, file: r.File, line: r.Line, value: r.Value, rkind: r.Kind}, true
			}
		}
	}
	// 3. A symbol defined on this line.
	if syms, err := s.store.SymbolsInFile(fileID); err == nil {
		for _, sy := range syms {
			if sy.Line == pos.Line+1 && sy.Name == tok {
				return entity{kind: "symbol", id: sy.ID, name: sy.Name, file: sy.File, line: sy.Line, sig: sy.Signature, rkind: sy.Kind}, true
			}
		}
	}
	// 4. A resource declaration with this name anywhere.
	if r, ok, _ := s.store.ResourceDeclByName(tok); ok {
		return entity{kind: "resource", id: r.ID, name: r.Name, file: r.File, line: r.Line, value: r.Value, rkind: r.Kind}, true
	}
	// 5. A symbol with this name anywhere.
	if hits, _ := s.store.SymbolsByName(tok, 1); len(hits) > 0 {
		h := hits[0]
		return entity{kind: "symbol", id: h.ID, name: h.Name, file: h.File, line: h.Line, sig: h.Signature, rkind: h.Kind}, true
	}
	return entity{}, false
}

func (s *Server) countIncoming(t string, id int64, kinds ...string) int {
	e, _ := s.store.IncomingEdges(t, id, kinds...)
	return len(e)
}

func lineRange(line1 int) Range {
	l := line1 - 1
	if l < 0 {
		l = 0
	}
	return Range{Start: Position{Line: l, Character: 0}, End: Position{Line: l, Character: 0}}
}

// --- v1: definition / references / hover / diagnostics ---

func (s *Server) definition(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[textDocumentPositionParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	_, fileID, ok := s.resolveFile(p.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	ent, ok := s.entityAt(fileID, s.docText(p.TextDocument.URI), p.Position)
	if !ok {
		return nil, nil
	}
	return Location{URI: uriForRel(s.Root, ent.file), Range: lineRange(ent.line)}, nil
}

func (s *Server) references(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[referenceParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	_, fileID, ok := s.resolveFile(p.TextDocument.URI)
	if !ok {
		return []Location{}, nil
	}
	ent, ok := s.entityAt(fileID, s.docText(p.TextDocument.URI), p.Position)
	if !ok {
		return []Location{}, nil
	}
	inc, _ := s.store.IncomingEdges(ent.kind, ent.id)
	locs := []Location{}
	seen := map[string]bool{}
	add := func(rel string, line1 int) {
		if rel == "" {
			return
		}
		key := rel + ":" + fmt.Sprint(line1)
		if seen[key] {
			return
		}
		seen[key] = true
		locs = append(locs, Location{URI: uriForRel(s.Root, rel), Range: lineRange(line1)})
	}
	for _, e := range inc {
		add(e.File, e.Line)
	}
	if p.Context.IncludeDeclaration {
		add(ent.file, ent.line)
	}
	return locs, nil
}

func (s *Server) hover(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[textDocumentPositionParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	_, fileID, ok := s.resolveFile(p.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	ent, ok := s.entityAt(fileID, s.docText(p.TextDocument.URI), p.Position)
	if !ok {
		return nil, nil
	}
	var b strings.Builder
	switch ent.kind {
	case "resource":
		uses := s.countIncoming("resource", ent.id, "uses_resource")
		fmt.Fprintf(&b, "**%s**\n\n", ent.name)
		if ent.value != "" {
			fmt.Fprintf(&b, "%s `%s`\n\n", ent.rkind, ent.value)
		} else {
			fmt.Fprintf(&b, "%s\n\n", ent.rkind)
		}
		fmt.Fprintf(&b, "Declared in `%s:%d` · used %d time(s)", ent.file, ent.line, uses)
	case "file":
		deps := s.countIncoming("file", ent.id)
		fmt.Fprintf(&b, "**%s**\n\n", ent.file)
		if ent.role != "" {
			fmt.Fprintf(&b, "_%s_\n\n", ent.role)
		}
		fmt.Fprintf(&b, "Depended on by %d file(s)", deps)
	case "symbol":
		fmt.Fprintf(&b, "**%s** _%s_", ent.name, ent.rkind)
		if ent.sig != "" {
			fmt.Fprintf(&b, "\n\n`%s`", ent.sig)
		}
		fmt.Fprintf(&b, "\n\nDefined in `%s:%d`", ent.file, ent.line)
	}
	return Hover{Contents: MarkupContent{Kind: "markdown", Value: b.String()}}, nil
}

// publishDiagnostics runs doctor and sends findings for one document.
func (s *Server) publishDiagnostics(uri string) {
	release, err := s.beginRead(context.Background())
	if err != nil {
		return
	}
	defer release()
	rel, in := relFromURI(s.Root, uri)
	if !in {
		return
	}
	rep, err := doctor.Run(s.store, s.rules, doctor.Options{Root: s.Root})
	if err != nil {
		return
	}
	var diags []Diagnostic
	for _, f := range rep.Findings {
		if f.File != rel {
			continue
		}
		diags = append(diags, Diagnostic{
			Range:    lineRange(f.Line),
			Severity: diagSeverity(f.Severity),
			Source:   "prowl",
			Code:     f.Check,
			Message:  f.Detail,
		})
	}
	s.sendDiagnostics(uri, diags)
}

func (s *Server) sendDiagnostics(uri string, diags []Diagnostic) {
	if diags == nil {
		diags = []Diagnostic{}
	}
	s.writeMessage(rpcNotification{
		JSONRPC: "2.0",
		Method:  "textDocument/publishDiagnostics",
		Params:  publishDiagnosticsParams{URI: uri, Diagnostics: diags},
	})
}

func diagSeverity(sev doctor.Severity) int {
	switch sev {
	case doctor.SevError:
		return diagError
	case doctor.SevWarn:
		return diagWarn
	default:
		return diagInfo
	}
}

// --- v2: document symbols / workspace symbols / code lens ---

func (s *Server) documentSymbol(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[documentSymbolParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	_, fileID, ok := s.resolveFile(p.TextDocument.URI)
	if !ok {
		return []DocumentSymbol{}, nil
	}
	syms, err := s.store.SymbolsInFile(fileID)
	if err != nil {
		return []DocumentSymbol{}, nil
	}
	text := s.docText(p.TextDocument.URI)
	out := []DocumentSymbol{}
	for _, sy := range syms {
		startLine := sy.Line - 1
		if startLine < 0 {
			startLine = 0
		}
		lt := lineText(text, startLine)
		nameCol := strings.Index(lt, sy.Name)
		if nameCol < 0 {
			nameCol = 0
		}
		full := Range{
			Start: Position{Line: startLine, Character: 0},
			End:   Position{Line: startLine, Character: len(lt)},
		}
		sel := Range{
			Start: Position{Line: startLine, Character: nameCol},
			End:   Position{Line: startLine, Character: nameCol + len(sy.Name)},
		}
		out = append(out, DocumentSymbol{
			Name:           sy.Name,
			Detail:         sy.Signature,
			Kind:           lspSymbolKind(sy.Kind),
			Range:          full,
			SelectionRange: sel,
		})
	}
	return out, nil
}

func (s *Server) workspaceSymbol(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[workspaceSymbolParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	out := []SymbolInformation{}
	if strings.TrimSpace(p.Query) == "" {
		return out, nil
	}
	hits, err := s.store.SymbolsLike(p.Query, 200)
	if err != nil {
		return out, nil
	}
	for _, h := range hits {
		out = append(out, SymbolInformation{
			Name:     h.Name,
			Kind:     lspSymbolKind(h.Kind),
			Location: Location{URI: uriForRel(s.Root, h.File), Range: lineRange(h.Line)},
		})
	}
	return out, nil
}

func (s *Server) codeLens(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[documentSymbolParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	_, fileID, ok := s.resolveFile(p.TextDocument.URI)
	if !ok {
		return []CodeLens{}, nil
	}
	out := []CodeLens{}
	res, _ := s.store.ResourcesInFile(fileID)
	for _, r := range res {
		uses := s.countIncoming("resource", r.ID, "uses_resource")
		if uses == 0 {
			continue
		}
		out = append(out, CodeLens{
			Range:   lineRange(r.Line),
			Command: &command{Title: fmt.Sprintf("%d use(s)", uses)},
		})
	}
	deps := s.countIncoming("file", fileID)
	if deps > 0 {
		out = append(out, CodeLens{
			Range:   lineRange(1),
			Command: &command{Title: fmt.Sprintf("depended on by %d file(s)", deps)},
		})
	}
	return out, nil
}

// --- v3: completion ---

func (s *Server) completion(raw json.RawMessage) (any, *rpcError) {
	p, ok := decode[completionParams](raw)
	if !ok {
		return nil, &rpcError{Code: errInvalidParams, Message: "bad params"}
	}
	text := s.docText(p.TextDocument.URI)
	prefix, _ := wordPrefix(lineText(text, p.Position.Line), p.Position.Character)
	bare := strings.TrimLeft(prefix, "$@~")
	items := []CompletionItem{}

	res, _ := s.store.NamedResources(1000)
	for _, r := range res {
		if bare != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(bare)) {
			continue
		}
		kind := cmplVariable
		if strings.Contains(r.Kind, "color") {
			kind = cmplColor
		}
		items = append(items, CompletionItem{
			Label:    r.Name,
			Kind:     kind,
			Detail:   r.Value,
			SortText: "0" + r.Name,
		})
		if len(items) >= 400 {
			break
		}
	}
	if bare != "" {
		if syms, err := s.store.SymbolsLike(bare, 100); err == nil {
			for _, sy := range syms {
				items = append(items, CompletionItem{
					Label:    sy.Name,
					Kind:     completionSymbolKind(sy.Kind),
					Detail:   sy.Signature,
					SortText: "1" + sy.Name,
				})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].SortText < items[j].SortText })
	return completionList{IsIncomplete: false, Items: items}, nil
}

func lspSymbolKind(kind string) int {
	switch kind {
	case "function", "method", "exec":
		return symFunction
	case "constant", "const":
		return symConstant
	case "keybind", "bind":
		return symKey
	case "component", "widget", "class", "module":
		return symModule
	case "string":
		return symString
	case "section", "table", "object":
		return symObject
	case "file":
		return symFile
	default:
		return symVariable
	}
}

func completionSymbolKind(kind string) int {
	switch kind {
	case "function", "method", "exec":
		return cmplFunction
	default:
		return cmplVariable
	}
}
