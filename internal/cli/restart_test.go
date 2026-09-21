package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/application"
)

func TestMatchProwlServer(t *testing.T) {
	bin := "/home/u/.local/bin/prowl"
	cases := []struct {
		name  string
		args  []string
		cwd   string
		scope string
		want  bool
	}{
		{"all-scope serve", []string{bin, "serve"}, "/any", "", true},
		{"all-scope lsp", []string{bin, "lsp"}, "/any", "", true},
		{"init is not a server", []string{bin, "init"}, "/any", "", false},
		{"non-prowl binary", []string{"/usr/bin/serve", "serve"}, "/any", "", false},
		{"too few args", []string{bin}, "/any", "", false},
		{"scoped match in root", []string{bin, "serve"}, "/proj/a", "/proj/a", true},
		{"scoped match nested", []string{bin, "lsp"}, "/proj/a/sub", "/proj/a", true},
		{"scoped no match outside", []string{bin, "serve"}, "/proj/b", "/proj/a", false},
		{"scoped no match prefix trick", []string{bin, "serve"}, "/proj/ab", "/proj/a", false},
	}
	for _, c := range cases {
		if got := matchProwlServer(c.args, c.cwd, c.scope); got != c.want {
			t.Errorf("%s: matchProwlServer(%v, %q, %q) = %v, want %v", c.name, c.args, c.cwd, c.scope, got, c.want)
		}
	}
}

func TestRestartRefreshesThroughProject(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc BeforeRestart() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunInit(InitOptions{Root: root, IntegrationsSet: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package main\nfunc AfterRestart() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	cmd := newRestartCmd("test")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("restart: %v\n%s", err, out.String())
	}
	project, err := application.OpenProject(context.Background(), root, application.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	hits, err := project.Query.FindSymbol("AfterRestart")
	if err != nil || len(hits) != 1 {
		t.Fatalf("rebuilt symbol = %+v, %v\n%s", hits, err, out.String())
	}
}
