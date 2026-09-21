package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/config"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/workspace"
)

func TestOpenQuerierReturnsMalformedConfigError(t *testing.T) {
	assertMalformedProjectOpen(t, func(ctx context.Context) error {
		q, ws, database, closeStore, err := openQuerier(ctx, false)
		if err == nil {
			if closeStore != nil {
				_ = closeStore()
			}
			t.Fatalf("openQuerier = (%v, %v, %v), want malformed config error", q, ws, database)
		}
		return err
	})
}

func TestOpenServeProjectReturnsMalformedConfigError(t *testing.T) {
	assertMalformedProjectOpen(t, func(ctx context.Context) error {
		project, err := openServeProject(ctx)
		if err == nil {
			if project != nil {
				_ = project.Close()
			}
			t.Fatal("openServeProject succeeded with malformed config")
		}
		return err
	})
}

func TestOpenLSPProjectReturnsMalformedConfigError(t *testing.T) {
	assertMalformedProjectOpen(t, func(ctx context.Context) error {
		project, err := openLSPProject(ctx)
		if err == nil {
			if project != nil {
				_ = project.Close()
			}
			t.Fatal("openLSPProject succeeded with malformed config")
		}
		return err
	})
}

func assertMalformedProjectOpen(t *testing.T, open func(context.Context) error) {
	t.Helper()
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.Path, "config.toml"), []byte("languages = [\"go\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := open(context.Background()); err == nil || !strings.Contains(err.Error(), "load project config") {
		t.Fatalf("error = %v, want malformed config context", err)
	}
}

func TestCLI_MCP_LSPProjectParity(t *testing.T) {
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\nfunc SharedSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	cliQuery, _, _, closeQuery, err := openQuerier(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	cliHits, err := cliQuery.FindSymbol("SharedSymbol")
	if closeErr := closeQuery(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	cliContext, closeContext, err := openContextService(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cliPacket, err := cliContext.Search(contextpacket.Request{Question: "SharedSymbol", Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	closeContext()
	if err != nil {
		t.Fatal(err)
	}

	mcpProject, err := openServeProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer mcpProject.Close()
	mcpHits, err := mcpProject.Query.FindSymbol("SharedSymbol")
	if err != nil {
		t.Fatal(err)
	}
	mcpPacket, err := mcpProject.Context.Search(contextpacket.Request{Question: "SharedSymbol", Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}

	lspProject, err := openLSPProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lspProject.Close()
	lspHits, err := lspProject.Query.FindSymbol("SharedSymbol")
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(cliHits, mcpHits) || !reflect.DeepEqual(cliHits, lspHits) {
		t.Fatalf("query adapter mismatch: CLI=%+v MCP=%+v LSP=%+v", cliHits, mcpHits, lspHits)
	}
	if !reflect.DeepEqual(packetItemIDs(cliPacket), packetItemIDs(mcpPacket)) {
		t.Fatalf("context adapter mismatch: CLI=%+v MCP=%+v", packetItemIDs(cliPacket), packetItemIDs(mcpPacket))
	}
}

func packetItemIDs(packet contextpacket.Packet) []string {
	ids := make([]string, len(packet.Items))
	for index, item := range packet.Items {
		ids[index] = item.ID
	}
	return ids
}

func TestStatusRefreshesThroughProject(t *testing.T) {
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "main.go")
	if err := os.WriteFile(source, []byte("package fixture\nfunc BeforeStatus() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := application.OpenProject(context.Background(), root, application.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package fixture\nfunc AfterStatus() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cmd := newStatusCmd("test")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	project, err = application.OpenProject(context.Background(), root, application.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	hits, err := project.Query.FindSymbol("AfterStatus")
	if err != nil || len(hits) != 1 {
		t.Fatalf("status did not refresh: %+v, %v", hits, err)
	}
}

func TestContextTracesReturnsMalformedConfigError(t *testing.T) {
	assertMalformedProjectOpen(t, func(context.Context) error {
		cmd := newContextTracesCmd()
		cmd.SetOut(&bytes.Buffer{})
		return cmd.Execute()
	})
}

func TestDoctorRejectsMalformedRules(t *testing.T) {
	root, state := rulesFixture(t)
	if err := os.WriteFile(filepath.Join(state.Path, "rules.toml"), []byte("[boundaries\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cmd := newDoctorCmd("test")
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("doctor accepted malformed rules")
	}
}

func TestLSPRejectsMalformedRules(t *testing.T) {
	root, state := rulesFixture(t)
	if err := os.WriteFile(filepath.Join(state.Path, "rules.toml"), []byte("[boundaries\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin; _ = r.Close() }()
	cmd := newLSPCmd("test")
	if err := cmd.Execute(); err == nil {
		t.Fatal("lsp accepted malformed rules")
	}
}

func rulesFixture(t *testing.T) (string, *workspace.Workspace) {
	t.Helper()
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\nfunc Fixture() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, state
}
