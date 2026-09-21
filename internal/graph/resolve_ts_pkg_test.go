package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// TestResolveTSWorkspacePackages checks that a bare import of a workspace
// package (`@scope/pkg`, `pkg/subpath`) resolves to that package's source entry
// by the src/ convention, mirroring a pnpm/turbo monorepo layout. External
// packages stay informational.
func TestResolveTSWorkspacePackages(t *testing.T) {
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
	// A monorepo with two workspace packages: @acme/server and @acme/client.
	serverIdx := mk("packages/server/src/index.ts", "typescript")
	serverHTTP := mk("packages/server/src/http.ts", "typescript")
	serverObs := mk("packages/server/src/observable/index.ts", "typescript")
	serverLocale := mk("packages/server/src/locales/fr.ts", "typescript")
	clientIdx := mk("packages/client/src/index.ts", "typescript")
	app := mk("apps/web/src/app.tsx", "tsx")

	// Record the workspace package map exactly as the indexer would.
	if err := s.SetMeta("ts_packages", `{"@acme/server":"packages/server","@acme/client":"packages/client"}`); err != nil {
		t.Fatal(err)
	}

	// app imports the package root, a file subpath, a directory subpath, the
	// other package, and an external package.
	if err := s.ReplaceFileGraph(app, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "@acme/server", Line: 1},
		{Kind: "includes", Raw: "@acme/server/http", Line: 2},
		{Kind: "includes", Raw: "@acme/server/observable", Line: 3},
		{Kind: "includes", Raw: "@acme/server/locales/fr.js", Line: 6}, // ESM .js -> .ts source
		{Kind: "includes", Raw: "@acme/client", Line: 4},
		{Kind: "includes", Raw: "react", Line: 5},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// The server package also imports the client package (cross-package).
	if err := s.ReplaceFileGraph(serverIdx, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "@acme/client", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	_ = serverHTTP
	_ = serverObs
	_ = serverLocale

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	wantIncoming := func(target int64, file, label string) {
		t.Helper()
		in, _ := s.IncomingEdges("file", target, "includes")
		found := false
		for _, e := range in {
			if e.File == file {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: %q not among incoming edges %+v", label, file, in)
		}
	}
	wantIncoming(serverIdx, "apps/web/src/app.tsx", "@acme/server root -> src/index.ts")
	wantIncoming(serverHTTP, "apps/web/src/app.tsx", "@acme/server/http -> src/http.ts")
	wantIncoming(serverObs, "apps/web/src/app.tsx", "@acme/server/observable -> src/observable/index.ts")
	wantIncoming(serverLocale, "apps/web/src/app.tsx", "@acme/server/locales/fr.js -> src/locales/fr.ts")
	wantIncoming(clientIdx, "apps/web/src/app.tsx", "@acme/client root from app")
	wantIncoming(clientIdx, "packages/server/src/index.ts", "@acme/client root from server (cross-pkg)")

	// Only the external react import remains unresolved.
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "react" {
		t.Fatalf("unresolved = %+v, want only react", dang)
	}
}

func TestTSImportPackage(t *testing.T) {
	cases := []struct {
		raw, name, sub string
	}{
		{"@scope/pkg", "@scope/pkg", ""},
		{"@scope/pkg/sub/path", "@scope/pkg", "sub/path"},
		{"pkg", "pkg", ""},
		{"pkg/sub", "pkg", "sub"},
		{"./relative", "", ""},
		{"../up", "", ""},
		{"/abs", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		name, sub := tsImportPackage(c.raw)
		if name != c.name || sub != c.sub {
			t.Errorf("tsImportPackage(%q) = (%q,%q), want (%q,%q)", c.raw, name, sub, c.name, c.sub)
		}
	}
}
