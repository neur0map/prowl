package query

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

// outlineDocFixture is a Go file with a documented function and a struct whose
// fields carry inline comments. It exercises both halves of task 7: the doc
// comment must reach OutlineSymbol.Doc, and the inline field comments must never
// leak into a struct's signature. It is written to a temp dir so the test does
// not track edits to any real repository source file.
func outlineDocFixture(t *testing.T) *Querier {
	t.Helper()
	dir := t.TempDir()
	src := "package sample\n\n" +
		"// Project holds the derived state for one indexed repository. It is the\n" +
		"// unit of freshness the querier reasons about.\n" +
		"type Project struct {\n" +
		"\tName               string    // Name is the repo's short name.\n" +
		"\tInferencerProvider Provider  // VectorProgress, when set, receives progress updates.\n" +
		"\tcount              int\n" +
		"}\n\n" +
		"// ensureFresh brings the derived index up to date before any query is\n" +
		"// served. A stale structural index is reindexed.\n" +
		"func ensureFresh(p *Project) error {\n" +
		"\treturn nil\n" +
		"}\n\n" +
		"func bare() int { return 0 }\n"
	if err := os.WriteFile(filepath.Join(dir, "project.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}
	return New(s)
}

// outline is the "should I care about this file" call. Returning shape without
// purpose forces a second call (def or read) for almost every symbol.
func TestOutlineCarriesPurpose(t *testing.T) {
	out, err := outlineDocFixture(t).Outline("project.go")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range out.Symbols {
		if s.Name == "ensureFresh" {
			found = true
			if s.Doc == "" {
				t.Fatal("ensureFresh has a doc comment but outline reported none")
			}
			if strings.Contains(s.Doc, "//") {
				t.Fatalf("comment markers leaked into doc: %q", s.Doc)
			}
			// One sentence, not the whole comment: an outline that pastes a
			// paragraph per symbol stops being an outline.
			if strings.Contains(s.Doc, "reindexed") {
				t.Fatalf("doc carried more than the first sentence: %q", s.Doc)
			}
		}
		// A symbol with no doc comment must degrade to an empty Doc, never a
		// placeholder like ABSENT.
		if s.Name == "bare" && s.Doc != "" {
			t.Fatalf("bare() has no doc comment but outline reported %q", s.Doc)
		}
	}
	if !found {
		t.Fatal("ensureFresh not found in outline")
	}
}

// Struct field comments used to leak into the signature, truncated and with
// markers inline, which wastes tokens and reads as garbage.
func TestOutlineSignatureHasNoCommentText(t *testing.T) {
	out, err := outlineDocFixture(t).Outline("project.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Symbols {
		if strings.Contains(s.Signature, "//") || strings.Contains(s.Signature, "/*") {
			t.Fatalf("%s signature contains comment text: %q", s.Name, s.Signature)
		}
	}
}

func TestOutlineNestsSymbols(t *testing.T) {
	dir := t.TempDir()
	// A class with two methods (nested) plus a top-level function.
	src := "class Widget:\n    def render(self):\n        return 1\n    def update(self):\n        return 2\n\ndef helper():\n    return 3\n"
	if err := os.WriteFile(filepath.Join(dir, "w.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}

	o, err := New(s).Outline("w.py")
	if err != nil {
		t.Fatal(err)
	}
	if o.File != "w.py" {
		t.Fatalf("file = %q, want w.py", o.File)
	}
	byName := map[string]OutlineSymbol{}
	for _, sym := range o.Symbols {
		byName[sym.Name] = sym
	}
	if w, ok := byName["Widget"]; !ok || w.Kind != "class" || w.Depth != 0 {
		t.Fatalf("Widget = %+v, want class at depth 0", w)
	}
	if r, ok := byName["render"]; !ok || r.Depth != 1 {
		t.Fatalf("render = %+v, want a method nested at depth 1", r)
	}
	if h, ok := byName["helper"]; !ok || h.Depth != 0 {
		t.Fatalf("helper = %+v, want a top-level function at depth 0", h)
	}
	// The skeleton carries no bodies: the struct has no content field, and the
	// method's own line range must be its signature+body span, not the file.
	if r := byName["render"]; r.LineStart < 2 || r.LineEnd > 5 {
		t.Fatalf("render line range %d-%d out of the class", r.LineStart, r.LineEnd)
	}

	if _, err := New(s).Outline("does/not/exist.py"); err == nil {
		t.Fatal("expected an error for an unindexed file")
	}
}
