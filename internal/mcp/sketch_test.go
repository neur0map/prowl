package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// TestSketchUITool exercises the sketch_ui MCP tool end to end through a real
// client session: a QML component resolved both by file path and by name.
func TestSketchUITool(t *testing.T) {
	dir := t.TempDir()
	qml := `// A labeled toggle row.
import QtQuick
Rectangle {
    id: root
    width: 200
    color: "#1e1e2e"
    Toggle {
        id: sw
        onToggled: root.on = checked
    }
}
`
	if err := os.WriteFile(filepath.Join(dir, "ToggleRow.qml"), []byte(qml), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, dir, nil); err != nil {
		t.Fatal(err)
	}

	srv := NewServerWithOptions(query.New(s), s, "test", nil, nil, nil, ServerOptions{Surface: SurfaceLegacy, Root: dir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	call := func(target string) string {
		t.Helper()
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "sketch_ui", Arguments: map[string]any{"target": target}})
		if err != nil {
			t.Fatalf("call sketch_ui %q: %v", target, err)
		}
		if res.IsError {
			t.Fatalf("sketch_ui %q error: %+v", target, res.Content)
		}
		tc, ok := res.Content[0].(*sdk.TextContent)
		if !ok {
			t.Fatalf("content not text: %T", res.Content[0])
		}
		return tc.Text
	}

	// By absolute path.
	out := call(filepath.Join(dir, "ToggleRow.qml"))
	if !strings.Contains(out, "Rectangle") || !strings.Contains(out, "onToggled") {
		t.Fatalf("path sketch missing structure/behavior:\n%s", out)
	}
	// By component name, resolved through the index.
	out = call("ToggleRow")
	if !strings.Contains(out, "Rectangle") || !strings.Contains(out, "onToggled") {
		t.Fatalf("name sketch missing structure/behavior:\n%s", out)
	}
}
