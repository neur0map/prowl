package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveDartPackages(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "dart", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// A melos-style monorepo: app depends on a sibling package and on its own lib.
	user := mk("packages/core/lib/models/user.dart")
	helpers := mk("packages/app/lib/util/helpers.dart")
	main := mk("packages/app/lib/main.dart")

	// app's pubspec is "app"; the sibling package is "core".
	if err := s.SetMeta("dart_packages", `{"app":"packages/app","core":"packages/core"}`); err != nil {
		t.Fatal(err)
	}

	if err := s.ReplaceFileGraph(main, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "package:core/models/user.dart", Line: 1}, // cross-package
		{Kind: "includes", Raw: "package:app/util/helpers.dart", Line: 2}, // own package
		{Kind: "includes", Raw: "util/helpers.dart", Line: 3},             // relative to lib/
		{Kind: "includes", Raw: "dart:async", Line: 4},                    // SDK
		{Kind: "includes", Raw: "package:flutter/material.dart", Line: 5}, // third-party
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// package:core/... resolves to the core package's lib file.
	if in, _ := s.IncomingEdges("file", user, "includes"); len(in) != 1 || in[0].File != "packages/app/lib/main.dart" {
		t.Fatalf("user.dart incoming = %+v, want one from main.dart (cross-package)", in)
	}
	// Both the package: self-import and the relative import land on helpers.dart.
	if in, _ := s.IncomingEdges("file", helpers, "includes"); len(in) != 2 {
		t.Fatalf("helpers.dart incoming = %+v, want two (package: self-import + relative)", in)
	}
	// SDK and third-party imports match no workspace package and stay unresolved.
	unresolved := map[string]bool{}
	dang, _ := s.UnresolvedEdges("includes")
	for _, d := range dang {
		unresolved[d.Raw] = true
	}
	if !unresolved["dart:async"] || !unresolved["package:flutter/material.dart"] {
		t.Fatalf("SDK/third-party should stay unresolved: %+v", dang)
	}
	if unresolved["package:core/models/user.dart"] {
		t.Fatalf("cross-package import should have resolved")
	}
}
