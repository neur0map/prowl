package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveTSAliases(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(rel, lang string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: lang, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	btn := mk("src/components/Button.tsx", "tsx")
	util := mk("src/lib/util.ts", "typescript")
	app := mk("src/app/page.tsx", "tsx")

	// A single root tsconfig: "@/*" -> "src/*".
	if err := s.SetMeta("ts_aliases", `[{"dir":".","aliases":{"@/":["src"]}}]`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(app, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "@/components/Button", Line: 1}, // -> src/components/Button.tsx
		{Kind: "includes", Raw: "@/lib/util", Line: 2},          // -> src/lib/util.ts (dir index/file)
		{Kind: "includes", Raw: "react", Line: 3},               // external
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.IncomingEdges("file", btn, "includes"); len(in) != 1 || in[0].File != "src/app/page.tsx" {
		t.Fatalf("Button.tsx incoming = %+v, want one from page.tsx (@/ alias)", in)
	}
	if in, _ := s.IncomingEdges("file", util, "includes"); len(in) != 1 {
		t.Fatalf("util.ts incoming = %+v, want one from page.tsx", in)
	}
	if dang, _ := s.UnresolvedEdges("includes"); len(dang) != 1 || dang[0].Raw != "react" {
		t.Fatalf("unresolved = %+v, want only react", dang)
	}
}

// TestResolveTSAliasesMonorepoScope checks that the same `@/` prefix in two
// packages resolves against each package's own tsconfig, not the other's.
func TestResolveTSAliasesMonorepoScope(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "typescript", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	aWidget := mk("apps/web/src/widget.ts")
	bWidget := mk("apps/api/src/widget.ts")
	aMain := mk("apps/web/src/main.ts")

	if err := s.SetMeta("ts_aliases", `[{"dir":"apps/web","aliases":{"@/":["apps/web/src"]}},{"dir":"apps/api","aliases":{"@/":["apps/api/src"]}}]`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(aMain, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "@/widget", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}
	// @/widget from apps/web resolves to web's widget, not api's.
	if in, _ := s.IncomingEdges("file", aWidget, "includes"); len(in) != 1 || in[0].File != "apps/web/src/main.ts" {
		t.Fatalf("web widget incoming = %+v, want one from web main.ts", in)
	}
	if in, _ := s.IncomingEdges("file", bWidget, "includes"); len(in) != 0 {
		t.Fatalf("api widget incoming = %+v, want none (alias is package-scoped)", in)
	}
}
