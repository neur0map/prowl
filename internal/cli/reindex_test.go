package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/workspace"
)

func TestReindexer(t *testing.T) {
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\nfunc Main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	project, err := openServeProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	if err := project.Store.SetMeta("index_version", ""); err != nil {
		t.Fatal(err)
	}
	r := reindexer(project)

	msg, err := r(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "indexed=1") || !strings.Contains(msg, "parsed=1") {
		t.Fatalf("first pass = %q, want indexed=1 parsed=1", msg)
	}
	if msg2, _ := r(context.Background()); !strings.Contains(msg2, "skipped=1") {
		t.Fatalf("second pass = %q, want skipped=1", msg2)
	}
}

func TestMaybeInferencerDisabled(t *testing.T) {
	if inf := maybeInferencer(context.Background(), config.Config{}); inf != nil {
		t.Fatal("inferencer should be nil when AI is disabled")
	}
}
