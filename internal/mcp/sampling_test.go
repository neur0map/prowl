package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestSearchContextUsesOptionalSamplingWithDeterministicFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(root, ".prowl", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := &contextpacket.Service{Store: database, Root: root}
	server := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Context: service, Root: root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	sampled := 0
	client := sdk.NewClient(&sdk.Implementation{Name: "sampling-test", Version: "1"}, &sdk.ClientOptions{
		CreateMessageHandler: func(_ context.Context, request *sdk.CreateMessageRequest) (*sdk.CreateMessageResult, error) {
			sampled++
			if request.Params.MaxTokens > 160 || len(request.Params.Messages) != 1 {
				t.Fatalf("unbounded sampling request: %+v", request.Params)
			}
			return &sdk.CreateMessageResult{Role: sdk.Role("assistant"), Model: "fixture", Content: &sdk.TextContent{Text: "Sampled local synthesis."}}, nil
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "search_context", Arguments: map[string]any{"query": "storage", "synthesize": true}})
	if err != nil || result.IsError {
		t.Fatalf("sampled result = %+v err=%v", result, err)
	}
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok || payload["summary"] != "Sampled local synthesis." || sampled != 1 {
		t.Fatalf("payload=%+v sampled=%d", payload, sampled)
	}
}
