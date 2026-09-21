package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolvePHPNamespaces(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: "php", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	user := mk("src/Model/User.php")
	post := mk("src/Model/Post.php") // same namespace as User; forces basename disambiguation
	mailer := mk("src/Service/Mailer.php")
	index := mk("public/index.php")
	helpers := mk("src/helpers.php")

	// User.php and Post.php both declare namespace App\Model.
	for _, id := range []int64{user, post} {
		if err := s.ReplaceFileGraph(id, nil, []store.Resource{
			{Kind: "namespace", Name: "App\\Model", Line: 1},
		}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Mailer.php declares App\Service, imports App\Model\User and an external
	// vendor class, and requires a relative helper file.
	if err := s.ReplaceFileGraph(mailer, nil, []store.Resource{
		{Kind: "namespace", Name: "App\\Service", Line: 1},
	}, []store.RawEdge{
		{Kind: "includes", Raw: "App\\Model\\User", Line: 2},
		{Kind: "includes", Raw: "Symfony\\Component\\Mailer\\Mailer", Line: 3},
		{Kind: "includes", Raw: "../helpers.php", Line: 4},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// index.php imports App\Service\Mailer (leading separator form).
	if err := s.ReplaceFileGraph(index, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "\\App\\Service\\Mailer", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	_ = helpers

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// App\Model\User resolved to User.php (not Post.php, despite sharing the namespace).
	if in, _ := s.IncomingEdges("file", user, "includes"); len(in) != 1 || in[0].File != "src/Service/Mailer.php" {
		t.Fatalf("User.php incoming = %+v, want one from Mailer.php", in)
	}
	if in, _ := s.IncomingEdges("file", post, "includes"); len(in) != 0 {
		t.Fatalf("Post.php incoming = %+v, want none (basename disambiguation)", in)
	}
	// index.php -> Mailer.php (leading-backslash FQCN).
	if in, _ := s.IncomingEdges("file", mailer, "includes"); len(in) != 1 || in[0].File != "public/index.php" {
		t.Fatalf("Mailer.php incoming = %+v, want one from index.php", in)
	}
	// The relative require resolved to the helper file (path pass).
	if in, _ := s.IncomingEdges("file", helpers, "includes"); len(in) != 1 || in[0].File != "src/Service/Mailer.php" {
		t.Fatalf("helpers.php incoming = %+v, want one from Mailer.php", in)
	}
	// The vendor class matches no declared namespace and stays unresolved.
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "Symfony\\Component\\Mailer\\Mailer" {
		t.Fatalf("unresolved = %+v, want only the vendor class", dang)
	}
}
