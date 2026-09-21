package query

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/store"
)

// OutlineSymbol is one entry in a file's structural skeleton: a symbol's kind,
// name, signature, one-sentence purpose, and line range, with Depth giving its
// nesting under enclosing symbols (a method inside a class has depth 1). No body
// is included. Doc is the first sentence of the symbol's leading doc comment, so
// an outline answers "should I care about this file" with why, not only shape;
// like Signature it is always emitted (no omitempty) so a file's symbols stay a
// uniform TOON table rather than degrading to the verbose per-item form.
type OutlineSymbol struct {
	Depth     int    `json:"depth"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Doc       string `json:"doc"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// FileOutline is a token-efficient skeletal view of one file: every symbol it
// defines, in source order, nested by line-range containment. It lets an agent
// grasp a file's shape from a handful of signature lines instead of reading the
// whole file, which is prowl's core promise applied to file-level structure.
type FileOutline struct {
	File    string          `json:"file"`
	Symbols []OutlineSymbol `json:"symbols"`
}

// Outline returns a file's structural skeleton from the index alone: no file
// read, no bodies. Nesting depth is derived deterministically from line-range
// containment, so it is language-agnostic. The path is repo-relative, like
// impact and callers.
func (q *Querier) Outline(path string) (FileOutline, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return FileOutline{}, err
	}
	defer release()
	path = strings.TrimSpace(path)
	if path == "" {
		return FileOutline{}, fmt.Errorf("a file path is required")
	}
	rel := filepath.ToSlash(path)
	fileID, err := q.s.FileID(rel)
	if err != nil {
		return FileOutline{}, fmt.Errorf("file not indexed: %s", path)
	}
	syms, err := q.s.SymbolsInFile(fileID)
	if err != nil {
		return FileOutline{}, err
	}
	// Doc comments are fetched once for the whole file and joined by symbol id,
	// so an outline states each symbol's purpose without a per-symbol query.
	docs, err := q.s.SymbolDocsInFile(fileID)
	if err != nil {
		return FileOutline{}, err
	}
	out := FileOutline{File: rel, Symbols: make([]OutlineSymbol, 0, len(syms))}
	var stack []store.SymbolHit
	for _, s := range syms {
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.Line < s.Line && s.EndLine <= top.EndLine {
				break // top encloses s: s nests under it
			}
			stack = stack[:len(stack)-1]
		}
		out.Symbols = append(out.Symbols, OutlineSymbol{
			Depth:     len(stack),
			Kind:      s.Kind,
			Name:      s.Name,
			Signature: s.Signature,
			Doc:       extract.DocSentence(docs[s.ID]),
			LineStart: s.Line,
			LineEnd:   s.EndLine,
		})
		stack = append(stack, s)
	}
	return out, nil
}
