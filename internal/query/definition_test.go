package query

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

func TestDefinitionReturnsSymbolBody(t *testing.T) {
	dir := t.TempDir()
	src := "package widget\n\n// Battery renders the charge level.\nfunc Battery() string {\n\treturn \"charge\"\n}\n\nfunc Other() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "battery.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module w\n\ngo 1.25\n"), 0o644); err != nil {
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
	q := New(s)

	def, err := q.Definition(dir, "Battery")
	if err != nil {
		t.Fatal(err)
	}
	if def.Name != "Battery" || def.File != "battery.go" {
		t.Fatalf("def = %+v, want Battery in battery.go", def)
	}
	if !strings.Contains(def.Code, "func Battery() string") || !strings.Contains(def.Code, `return "charge"`) {
		t.Fatalf("code missing the symbol body: %q", def.Code)
	}
	if strings.Contains(def.Code, "func Other") {
		t.Fatalf("code leaked a neighboring symbol: %q", def.Code)
	}

	hits, err := q.FindSymbol("Battery")
	if err != nil || len(hits) == 0 {
		t.Fatalf("find Battery = %v, %v", hits, err)
	}
	byID, err := q.Definition(dir, strconv.FormatInt(hits[0].ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if byID.Name != "Battery" || byID.File != "battery.go" {
		t.Fatalf("by-id def = %+v", byID)
	}

	if _, err := q.Definition(dir, "NoSuchSymbol"); err == nil {
		t.Fatal("expected an error for an unknown symbol")
	}
}

func TestDefinitionQMLComponentReturnsWholeFile(t *testing.T) {
	dir := t.TempDir()
	qml := "import QtQuick\nItem {\n  property int spacing: 8\n  width: 100\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "Panel.qml"), []byte(qml), 0o644); err != nil {
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
	def, err := New(s).Definition(dir, "Panel")
	if err != nil {
		t.Fatal(err)
	}
	if def.Kind != "component" {
		t.Fatalf("kind=%q, want component", def.Kind)
	}
	if !strings.Contains(def.Code, "property int spacing") || !strings.Contains(def.Code, "width: 100") {
		t.Fatalf("QML component body should be the whole file, got: %q", def.Code)
	}
	if def.LineEnd < 4 {
		t.Fatalf("line_end=%d, want the whole file", def.LineEnd)
	}
}

func TestDefinitionIncludesDocComment(t *testing.T) {
	dir := t.TempDir()
	src := "package widget\n\n// Battery reports the charge level.\n// Second doc line.\nfunc Battery() int {\n\treturn 1\n}\n\nfunc Plain() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module w\n\ngo 1.25\n"), 0o644); err != nil {
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
	q := New(s)

	def, err := q.Definition(dir, "Battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def.Code, "Battery reports the charge level") || !strings.Contains(def.Code, "Second doc line") {
		t.Fatalf("doc comment not included above the symbol: %q", def.Code)
	}
	if def.LineStart != 3 {
		t.Fatalf("line_start=%d, want 3 (the first doc line)", def.LineStart)
	}

	// A symbol with a blank line above it must not absorb unrelated lines.
	plain, err := q.Definition(dir, "Plain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Code, "doc line") || strings.Contains(plain.Code, "func Battery") {
		t.Fatalf("Plain over-reached into earlier lines: %q", plain.Code)
	}
}
