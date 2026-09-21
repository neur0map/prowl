package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveElixirModules(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "elixir", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	user := mk("lib/my_app/accounts/user.ex")
	accounts := mk("lib/my_app/accounts.ex")
	app := mk("lib/my_app/application.ex")

	// user.ex declares module MyApp.Accounts.User.
	if err := s.ReplaceFileGraph(user, nil, []store.Resource{
		{Kind: "namespace", Name: "MyApp.Accounts.User", Line: 1},
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// accounts.ex declares MyApp.Accounts, aliases the User module, and uses an
	// external framework module that is not in the project.
	if err := s.ReplaceFileGraph(accounts, nil, []store.Resource{
		{Kind: "namespace", Name: "MyApp.Accounts", Line: 1},
	}, []store.RawEdge{
		{Kind: "includes", Raw: "MyApp.Accounts.User", Line: 2},
		{Kind: "includes", Raw: "Ecto.Query", Line: 3},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// application.ex aliases the Accounts context.
	if err := s.ReplaceFileGraph(app, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "MyApp.Accounts", Line: 5},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// accounts.ex depends on user.ex via `alias MyApp.Accounts.User`.
	in, _ := s.IncomingEdges("file", user, "pkg")
	if len(in) != 1 || in[0].File != "lib/my_app/accounts.ex" {
		t.Fatalf("user.ex callers = %+v, want one from accounts.ex", in)
	}
	// application.ex depends on accounts.ex via `alias MyApp.Accounts`.
	inAcc, _ := s.IncomingEdges("file", accounts, "pkg")
	if len(inAcc) != 1 || inAcc[0].File != "lib/my_app/application.ex" {
		t.Fatalf("accounts.ex callers = %+v, want one from application.ex", inAcc)
	}
	// The external framework module resolves to no project file and stays
	// unresolved (informational).
	dang, _ := s.UnresolvedEdges("includes")
	var sawEcto bool
	for _, d := range dang {
		if d.Raw == "Ecto.Query" {
			sawEcto = true
		}
	}
	if !sawEcto {
		t.Errorf("Ecto.Query should remain unresolved, dangling=%+v", dang)
	}
}
