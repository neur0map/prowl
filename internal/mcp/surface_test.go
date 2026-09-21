package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestParseSurfaceRejectsUnknownValues(t *testing.T) {
	for _, value := range []string{"legacy", "core", "all"} {
		surface, err := ParseSurface(value)
		if err != nil || string(surface) != value {
			t.Fatalf("ParseSurface(%q) = %q, %v", value, surface, err)
		}
	}
	if _, err := ParseSurface("everything"); err == nil {
		t.Fatal("unknown MCP surface accepted")
	}
}

func TestCoreSurfaceIsSmallerAndLegacyStaysStable(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	service := &contextpacket.Service{Store: database, Root: t.TempDir()}
	legacy := NewServer(query.New(database), database, "test", nil, nil, nil)
	core := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Context: service, Capabilities: catalog})

	legacyTools := listToolDescriptors(t, legacy)
	coreTools := listToolDescriptors(t, core)
	if len(legacyTools) != 20 {
		t.Fatalf("legacy tool count = %d, want 20", len(legacyTools))
	}
	want := []string{"analyze_change", "find", "find_references", "get_context", "history", "outline", "propose_knowledge_change", "read_symbol", "search_capabilities", "search_context", "search_docs", "sketch_ui", "span", "validate_knowledge"}
	got := make([]string, len(coreTools))
	for index, tool := range coreTools {
		got[index] = tool.Name
	}
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Fatalf("core tools = %v, want %v", got, want)
	}
	coreJSON, _ := json.Marshal(coreTools)
	// The core surface is smaller in TOOL COUNT, which is the property that
	// matters: an agent chooses among fewer, better tools. It is deliberately not
	// smaller in descriptor BYTES -- core's context and knowledge tools carry
	// richer schemas than any legacy tool, so core runs heavier per tool. That
	// inequality held incidentally until span and history were added and is not a
	// design goal, so asserting it would fail on every honest new capability.
	if len(coreTools) >= len(legacyTools) {
		t.Fatalf("core tool count = %d, legacy = %d; core must expose fewer tools", len(coreTools), len(legacyTools))
	}
	// A budget instead, so descriptor growth stays visible and deliberate. Every
	// MCP client pays these bytes at connect. Raise this consciously, and if it is
	// startup cost you are chasing, the four biggest tools are half the payload.
	if len(coreJSON) > 24000 {
		t.Fatalf("core descriptor bytes = %d, budget 24000; trim a schema or raise the budget on purpose", len(coreJSON))
	}
}

func listToolDescriptors(t *testing.T, server *sdk.Server) []*sdk.Tool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "v2-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Tools
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
