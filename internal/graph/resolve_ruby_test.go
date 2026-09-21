package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveRubyRequires(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "ruby", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	store_ := mk("lib/store.rb")
	app := mk("app.rb")

	// app require_relative 'lib/store' (project) and require 'json' (stdlib/gem).
	if err := s.ReplaceFileGraph(app, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "lib/store", Line: 1},
		{Kind: "includes", Raw: "json", Line: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	if in, _ := s.IncomingEdges("file", store_, "includes"); len(in) != 1 || in[0].File != "app.rb" {
		t.Fatalf("store.rb incoming = %+v, want one from app.rb", in)
	}
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "json" {
		t.Fatalf("unresolved = %+v, want only json", dang)
	}
}
