package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/store"
)

// Definition is a symbol's cited source: its location, signature, and body,
// bounded so a large symbol cannot blow the budget. It lets an agent read one
// function or component instead of the whole file, which is prowl's core promise
// applied to the find-then-read step: locate a symbol, then read only it.
type Definition struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Signature string `json:"signature,omitempty"`
	Code      string `json:"code"`
	Truncated bool   `json:"truncated,omitempty"`
}

// DefinitionMaxLines bounds the body a single Definition returns; a larger
// symbol is truncated with Truncated set, and LineEnd still reports its real
// end so the caller knows how much remains.
const DefinitionMaxLines = 200

// Definition resolves a symbol by numeric id (from a find result) or by name
// (best-ranked match) and returns its cited source, read from root and bounded
// to DefinitionMaxLines. Read-only; the body reflects the current file.
func (q *Querier) Definition(root, target string) (Definition, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Definition{}, err
	}
	defer release()

	target = strings.TrimSpace(target)
	if target == "" {
		return Definition{}, fmt.Errorf("a symbol name or id is required")
	}

	var hit store.SymbolHit
	if id, convErr := strconv.ParseInt(target, 10, 64); convErr == nil {
		h, ok, err := q.s.SymbolByID(id)
		if err != nil {
			return Definition{}, err
		}
		if !ok {
			return Definition{}, fmt.Errorf("no symbol with id %d", id)
		}
		hit = h
	} else {
		hits, err := q.findSymbolLocked(target)
		if err != nil {
			return Definition{}, err
		}
		if len(hits) == 0 {
			return Definition{}, fmt.Errorf("no symbol named %q (try prowl find)", target)
		}
		hit = hits[0]
	}

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(hit.File)))
	if err != nil {
		return Definition{}, fmt.Errorf("read %s: %w", hit.File, err)
	}
	lines := strings.Split(string(data), "\n")
	start := hit.Line
	if start < 1 {
		start = 1
	}
	end := hit.EndLine
	if end < start {
		end = start
	}
	// A QML component symbol is filename-derived with a placeholder 1-1 range;
	// its real definition is the whole file, so read to the end.
	if hit.Kind == "component" && end <= 1 {
		end = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}
	start = extract.DocCommentStart(lines, start)
	body := lines[start-1 : end]
	truncated := false
	if len(body) > DefinitionMaxLines {
		body = body[:DefinitionMaxLines]
		truncated = true
	}
	return Definition{
		Name:      hit.Name,
		Kind:      hit.Kind,
		File:      hit.File,
		LineStart: start,
		LineEnd:   end,
		Signature: hit.Signature,
		Code:      strings.Join(body, "\n"),
		Truncated: truncated,
	}, nil
}
