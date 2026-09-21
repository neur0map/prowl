// Package extract turns parsed source files into symbols, resources, raw edges,
// and text chunks. Each language has an Extractor; languages without a dedicated
// grammar fall back to the line-oriented generic extractor.
package extract

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/neur0map/prowl/internal/parse"
)

// Symbol is a definition found in a file.
type Symbol struct {
	Name, Kind, Signature, Parent string
	StartLine, EndLine            int
	Complexity                    int
	Doc                           string
}

// Resource is a shared value (color/font/path/var) declared or used in a file.
type Resource struct {
	Kind, Name, Value string
	Line              int
}

// RawEdge is an unresolved relationship; Raw is the literal target string
// (path/module/command/var) that the resolution passes will resolve later.
// SrcName, if it matches a symbol in the same file, makes the edge symbol-sourced.
type RawEdge struct {
	SrcName, Kind, Raw string
	Line               int
}

// Chunk is a text window used for full-text search (and future embeddings).
type Chunk struct {
	StartLine, EndLine int
	Text               string
}

// Result is everything an extractor derives from one file.
type Result struct {
	Symbols   []Symbol
	Resources []Resource
	Edges     []RawEdge
	Chunks    []Chunk
}

// Extractor derives a Result from a file's bytes.
type Extractor interface {
	Lang() string
	Extract(src []byte) (Result, error)
}

var registry = map[string]Extractor{}

func register(e Extractor) { registry[e.Lang()] = e }

// For returns the extractor for lang, falling back to the generic extractor.
func For(lang string) (Extractor, bool) {
	if e, ok := registry[lang]; ok {
		return e, true
	}
	if e, ok := registry["generic"]; ok {
		return e, true
	}
	return nil, false
}

// SymbolRange returns the 1-based inclusive line range of the first symbol named
// `symbol` in data, detecting the language from path + content. It lets a caller
// resolve a symbol-based source anchor to the symbol's CURRENT range, so the
// anchor tracks the symbol across line shifts and stales only when its body
// changes. ok is false when the language has no extractor or no such symbol.
func SymbolRange(path string, data []byte, symbol string) (int, int, bool) {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	ex, ok := For(parse.Detect(path, head))
	if !ok {
		return 0, 0, false
	}
	res, _ := ex.Extract(data)
	for _, s := range res.Symbols {
		if s.Name == symbol {
			return s.StartLine, s.EndLine, true
		}
	}
	return 0, 0, false
}

// capture is a named capture node from a query match.
type capture struct {
	Name string
	Node sitter.Node
}

// queryEach parses src as lang, runs the scm query, and invokes fn once per
// match with that match's captures. The parse tree is created and closed here.
func queryEach(lang string, src, scm []byte, fn func(caps []capture)) error {
	lng, ok := parse.Language(lang)
	if !ok {
		return fmt.Errorf("no grammar for %q", lang)
	}
	q, err := compiledQuery(lang, lng, scm)
	if err != nil {
		return fmt.Errorf("query for %s: %w", lang, err)
	}
	tree, err := parse.Parse(lang, src)
	if err != nil {
		return err
	}
	defer tree.Close()
	qc := sitter.NewQueryCursor()
	matches := qc.Matches(q, tree.RootNode(), src)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		caps := make([]capture, len(m.Captures))
		for i, c := range m.Captures {
			caps[i] = capture{Name: q.CaptureNameForID(c.Index), Node: c.Node}
		}
		fn(caps)
	}
	return nil
}

// queries caches compiled tree-sitter queries by (lang, scm). A *sitter.Query is
// immutable and safe to share across goroutines; each caller runs it with its
// own cursor, so compiling once per process (not once per file) is both correct
// and much faster on a large tree.
var queries sync.Map // lang+scm -> *sitter.Query

func compiledQuery(lang string, lng *sitter.Language, scm []byte) (*sitter.Query, error) {
	key := lang + "\x00" + string(scm)
	if v, ok := queries.Load(key); ok {
		return v.(*sitter.Query), nil
	}
	q, err := sitter.NewQuery(lng, scm)
	if err != nil {
		return nil, err
	}
	actual, _ := queries.LoadOrStore(key, q)
	return actual.(*sitter.Query), nil
}

// capNode returns the node captured under name in this match.
func capNode(caps []capture, name string) (sitter.Node, bool) {
	for _, c := range caps {
		if c.Name == name {
			return c.Node, true
		}
	}
	return sitter.Node{}, false
}

// line is the 1-based start line of a node.
func line(n sitter.Node) int { return int(n.StartPoint().Row) + 1 }

// endLine is the 1-based end line of a node.
func endLine(n sitter.Node) int { return int(n.EndPoint().Row) + 1 }

// firstChildOfType returns the first named child of n with the given type.
func firstChildOfType(n sitter.Node, typ string) (sitter.Node, bool) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		if ch.Type() == typ {
			return ch, true
		}
	}
	return sitter.Node{}, false
}

// chunkStructured splits src into text chunks aligned to symbol boundaries, so a
// function, type, or component stays whole in one chunk when it fits. Fixed-size
// windows split a symbol across chunks, fragmenting its full-text and embedding
// signal; structure-aware chunks keep syntactic units intact (cAST, EMNLP 2025).
// A symbol up to twice the window stays whole; larger symbols and symbol-less
// regions are split into windows, so a file with no symbols matches fixed
// windows. Adjacent small atoms are packed together up to window lines.
func chunkStructured(src []byte, symbols []Symbol, window int) []Chunk {
	if window <= 0 {
		window = 40
	}
	lines := strings.Split(string(src), "\n")
	n := len(lines)
	if n == 0 {
		return nil
	}
	var out []Chunk
	curA, curB := 0, 0
	emit := func() {
		text := strings.Join(lines[curA-1:curB], "\n")
		// A run of blank lines between symbols carries nothing retrievable, and an
		// empty chunk is actively harmful: a static embedder maps it to its
		// degenerate mean vector, which sits nearer an arbitrary query than real
		// code does, so blank chunks monopolize vector KNN and crowd real matches
		// out of every result page.
		if strings.TrimSpace(text) != "" {
			out = append(out, Chunk{StartLine: curA, EndLine: curB, Text: text})
		}
		curA, curB = 0, 0
	}
	for _, at := range buildAtoms(mergeSymbolSpans(symbols, n), n, window) {
		switch {
		case curA == 0:
			curA, curB = at.a, at.b
		case at.b-curA+1 <= window:
			curB = at.b
		default:
			emit()
			curA, curB = at.a, at.b
		}
	}
	if curA != 0 {
		emit()
	}
	return out
}

// symbolSpan is a merged, disjoint line range covering one or more symbols.
type symbolSpan struct{ a, b int }

// mergeSymbolSpans returns disjoint, sorted line spans for the symbols, clamped
// to [1,n]. Overlapping or nested symbols (a type and its methods) are unioned
// so the file tiling never overlaps; adjacent distinct symbols stay separate.
func mergeSymbolSpans(symbols []Symbol, n int) []symbolSpan {
	spans := make([]symbolSpan, 0, len(symbols))
	for _, s := range symbols {
		a, b := s.StartLine, s.EndLine
		if b < a {
			b = a
		}
		if a < 1 {
			a = 1
		}
		if b > n {
			b = n
		}
		if a > n {
			continue
		}
		spans = append(spans, symbolSpan{a, b})
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].a != spans[j].a {
			return spans[i].a < spans[j].a
		}
		return spans[i].b < spans[j].b
	})
	var merged []symbolSpan
	for _, s := range spans {
		if len(merged) > 0 && s.a <= merged[len(merged)-1].b {
			if s.b > merged[len(merged)-1].b {
				merged[len(merged)-1].b = s.b
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// atom is an ordered, contiguous piece of the file to be packed into a chunk.
type atom struct{ a, b int }

// buildAtoms turns merged symbol regions and the gaps around them into ordered,
// contiguous atoms covering [1,n]. A symbol region up to window*2 lines becomes
// one atom so it stays whole; larger regions and symbol-less gaps are split into
// window-sized atoms. The caller packs adjacent atoms up to window lines.
func buildAtoms(regions []symbolSpan, n, window int) []atom {
	hard := window * 2
	var atoms []atom
	windowSplit := func(a, b int) {
		for s := a; s <= b; s += window {
			e := s + window - 1
			if e > b {
				e = b
			}
			atoms = append(atoms, atom{s, e})
		}
	}
	cursor := 1
	for _, r := range regions {
		if r.a > cursor {
			windowSplit(cursor, r.a-1)
		}
		if r.b-r.a+1 <= hard {
			atoms = append(atoms, atom{r.a, r.b})
		} else {
			windowSplit(r.a, r.b)
		}
		cursor = r.b + 1
	}
	if cursor <= n {
		windowSplit(cursor, n)
	}
	return atoms
}

// Sexpr returns the parse tree's S-expression for a snippet, a debugging aid
// for authoring queries against a grammar's real node names.
func Sexpr(lang string, src []byte) (string, error) {
	tree, err := parse.Parse(lang, src)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	return tree.RootNode().String(), nil
}

// unquote trims surrounding whitespace, then strips surrounding quotes from a
// string literal's text (config string values often carry leading whitespace).
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
