package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

func TestBriefScopesToPath(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module brieffix\n\ngo 1.25\n")
	write("README.md", "# Brief Fixture\nArchitecture overview here.\n")
	write("bar/widget.go", "package bar\nfunc Widget() {}\n")
	write("bar/util.go", "package bar\nfunc Util() {}\n")
	write("other/thing.go", "package other\nfunc Thing() {}\n")

	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}
	q := New(s)

	b, err := q.Brief("bar")
	if err != nil {
		t.Fatal(err)
	}
	if b.Scope != "bar" {
		t.Fatalf("scope=%q want bar", b.Scope)
	}
	if b.Files != 2 {
		t.Fatalf("files=%d want 2 (bar/*.go)", b.Files)
	}
	if len(b.Languages) != 1 || b.Languages[0].Lang != "go" || b.Languages[0].Files != 2 {
		t.Fatalf("languages=%+v want go:2", b.Languages)
	}
	if len(b.KeyFiles) == 0 {
		t.Fatal("no key files in brief")
	}
	for _, k := range b.KeyFiles {
		if k.File != "bar/widget.go" && k.File != "bar/util.go" {
			t.Fatalf("key file %q is outside scope bar/", k.File)
		}
	}
	foundReadme := false
	for _, g := range b.Guides {
		if g == "README.md" {
			foundReadme = true
		}
	}
	if !foundReadme {
		t.Fatalf("guides=%v want README.md", b.Guides)
	}

	all, err := q.Brief(".")
	if err != nil {
		t.Fatal(err)
	}
	if all.Files < 3 {
		t.Fatalf("whole-repo brief files=%d want >=3", all.Files)
	}

	if _, err := q.Brief("nope"); err == nil {
		t.Fatal("expected an error for a scope with no indexed files")
	}
}
