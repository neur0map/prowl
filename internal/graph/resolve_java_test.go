package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveJavaImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "java", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	store_ := mk("src/main/java/com/foo/Store.java")
	api := mk("src/main/java/com/foo/Api.java")

	// Api imports com.foo.Store (in-project, Maven layout) and java.util.List (JDK).
	if err := s.ReplaceFileGraph(api, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "com.foo.Store", Line: 2},
		{Kind: "includes", Raw: "java.util.List", Line: 3},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	if in, _ := s.IncomingEdges("file", store_, "includes"); len(in) != 1 || in[0].File != "src/main/java/com/foo/Api.java" {
		t.Fatalf("Store incoming = %+v, want one from Api.java", in)
	}
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "java.util.List" {
		t.Fatalf("unresolved = %+v, want only java.util.List", dang)
	}
}

func TestResolveJavaMultiModuleAndWildcard(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "java", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Two Gradle modules; the consumer module imports a class from the core module.
	core := mk("core/src/main/java/com/foo/Retrofit.java")
	conv := mk("converters/gson/src/main/java/com/foo/conv/GsonConverter.java")
	other := mk("core/src/main/java/com/foo/Call.java")

	if err := s.ReplaceFileGraph(conv, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "com.foo.Retrofit", Line: 1}, // cross-module class
		{Kind: "includes", Raw: "com.foo.*", Line: 2},        // wildcard package
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// The cross-module class import resolves to core's Retrofit.java.
	if in, _ := s.IncomingEdges("file", core, "includes"); len(in) != 1 || in[0].File != "converters/gson/src/main/java/com/foo/conv/GsonConverter.java" {
		t.Fatalf("Retrofit incoming = %+v, want one from GsonConverter.java", in)
	}
	// The wildcard import fans out to every file in package com.foo (pkg edges).
	inPkg, _ := s.IncomingEdges("file", other, "pkg")
	if len(inPkg) != 1 || inPkg[0].File != "converters/gson/src/main/java/com/foo/conv/GsonConverter.java" {
		t.Fatalf("Call.java pkg incoming = %+v, want wildcard fan-out from GsonConverter.java", inPkg)
	}
}
