package query

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/store"
)

// SymbolSpan is a symbol's current, exact location plus a digest of the bytes
// that currently occupy that range. It is the answer to "where is symbol X right
// now" -- the cheap re-grounding an agent needs after its own edits have shifted
// line numbers, and the drift check that keeps it from editing a stale range.
type SymbolSpan struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Digest    string `json:"digest"`
}

// SpanDigestLen is how many hex characters of the SHA-256 sum the digest keeps.
// Twelve hex chars (48 bits) sit on one output line and make an accidental
// collision between two versions of one symbol's body negligible at repo scale.
const SpanDigestLen = 12

// Span reports where every symbol matching target currently lives, plus a
// content digest so a caller holding an earlier span can detect drift without
// re-reading the file. target is a numeric id (from a find result -- pins exactly
// one symbol) or a name, resolved exactly like `find`/`def`: exact name, then
// FTS, then substring, stably tiered, with fuzzy matching only on a total miss.
// When several symbols share a name, every match is returned so the choice is
// visible rather than silently made. It needs root to read the current bytes.
//
// The digest is deliberately CONTENT-ONLY, never position. It is the first
// SpanDigestLen hex chars of sha256 over the symbol's current body: the lines
// [LineStart, LineEnd] read from disk, each stripped of a trailing '\r' (so a
// CRLF-vs-LF re-save does not churn it) and joined with '\n', with no trailing
// newline. Nothing about the file path or the line numbers enters the hash.
//
// The consequences are the whole point of the command:
//   - A reindex that changed nothing leaves the same bytes, so the digest is stable.
//   - Any edit inside the body -- including indentation -- changes the bytes, so
//     the digest changes. Whitespace is not normalised away: in a
//     whitespace-significant language an indentation change IS a body change, and
//     a digest that hid it would be dangerous.
//   - Inserting or deleting lines ABOVE the symbol moves LineStart/LineEnd but not
//     the body bytes, so the digest is UNCHANGED while the range shifts. That is
//     exactly the signal a caller wants: same code, new coordinates. Position
//     drift shows up in the line numbers; true drift shows up in the digest.
func (q *Querier) Span(root, target string) ([]SymbolSpan, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()

	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("a symbol name or id is required")
	}

	var hits []store.SymbolHit
	if id, convErr := strconv.ParseInt(target, 10, 64); convErr == nil {
		h, ok, err := q.s.SymbolByID(id)
		if err != nil {
			return nil, err
		}
		if ok {
			hits = []store.SymbolHit{h}
		}
	} else {
		hits, err = q.findSymbolLocked(target)
		if err != nil {
			return nil, err
		}
	}

	out := make([]SymbolSpan, 0, len(hits))
	for _, h := range hits {
		body, start, end, ok := readSpan(root, h)
		if !ok {
			continue // a file removed since indexing is not a span
		}
		sum := sha256.Sum256(body)
		out = append(out, SymbolSpan{
			Name:      h.Name,
			Kind:      h.Kind,
			File:      h.File,
			LineStart: start,
			LineEnd:   end,
			Digest:    hex.EncodeToString(sum[:])[:SpanDigestLen],
		})
	}
	return out, nil
}

// readSpan reads the current bytes occupying a symbol's line range from disk and
// returns them normalised for hashing, plus the clamped 1-based range actually
// covered. ok is false when the file cannot be read (deleted since indexing),
// which is not a span. Clamping mirrors Definition: a range is pinned inside the
// file's real bounds, and a QML component's placeholder 1-1 range means "the
// whole file". Normalisation strips a trailing '\r' per line and rejoins with
// '\n' so a CRLF-vs-LF re-save does not change the digest; nothing else is
// altered, so an in-body whitespace edit still moves it.
func readSpan(root string, h store.SymbolHit) (body []byte, start, end int, ok bool) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(h.File)))
	if err != nil {
		return nil, 0, 0, false
	}
	lines := strings.Split(string(data), "\n")
	n := len(lines) // strings.Split never returns fewer than one element
	start, end = h.Line, h.EndLine
	if h.Kind == "component" && end <= 1 {
		end = n
	}
	if start < 1 {
		start = 1
	}
	if start > n {
		start = n
	}
	if end < start {
		end = start
	}
	if end > n {
		end = n
	}
	span := make([]string, 0, end-start+1)
	for _, line := range lines[start-1 : end] {
		span = append(span, strings.TrimSuffix(line, "\r"))
	}
	return []byte(strings.Join(span, "\n")), start, end, true
}
