package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServeProcessE2E builds the real binary, inits a fixture, then drives
// `prowl serve` over a real stdio subprocess via the MCP client.
func TestServeProcessE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process e2e in -short mode")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "prowl")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, "./cmd/prowl")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}

	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "sample-config"), root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	if _, err := RunInit(InitOptions{Root: root}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	serve := exec.Command(bin, "serve")
	serve.Dir = root
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &sdk.CommandTransport{Command: serve}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		if res.IsError || len(res.Content) == 0 {
			t.Fatalf("call %s: error or empty: %+v", name, res)
		}
		return res.Content[0].(*sdk.TextContent).Text
	}

	// The generated Prowl-only AGENTS.md block is intentionally excluded, so the
	// index contains only the fixture's 11 source/config files.
	if out := call("status", nil); !strings.Contains(out, "files: 11") {
		t.Fatalf("status over process: %s", out)
	}
	if out := call("blast_radius", map[string]any{"path": "hypr/colors.conf"}); !strings.Contains(out, "hypr/hyprland.conf") {
		t.Fatalf("blast_radius over process: %s", out)
	}
	if out := call("find_symbol", map[string]any{"name": "M.apply"}); !strings.Contains(out, "nvim/lua/opts.lua") {
		t.Fatalf("find_symbol over process: %s", out)
	}
}
