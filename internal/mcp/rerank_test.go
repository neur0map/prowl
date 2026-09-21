package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestSearchContextRerankAppliesHostSamplingOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(root, ".prowl", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// Three sources that match the query equally so only the host ordering
	// changes their relative rank.
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		text := "storage " + name + " notes"
		fileID, err := database.UpsertFile(store.File{RelPath: name + ".txt", Lang: "text", Hash: name, Size: int64(len(text)), MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.ReplaceFileGraph(fileID, nil, nil, nil, []store.Chunk{{StartLine: 1, EndLine: 1, Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	service := &contextpacket.Service{Store: database, Root: root}
	server := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Context: service, Root: root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	// The host ranks charlie best, then bravo, then alpha, regardless of the
	// order the candidates were presented in.
	want := []string{"charlie.txt", "bravo.txt", "alpha.txt"}
	sampled := 0
	client := sdk.NewClient(&sdk.Implementation{Name: "rerank-test", Version: "1"}, &sdk.ClientOptions{
		CreateMessageHandler: func(_ context.Context, request *sdk.CreateMessageRequest) (*sdk.CreateMessageResult, error) {
			sampled++
			if request.Params.MaxTokens == 0 || request.Params.MaxTokens > 256 || len(request.Params.Messages) != 1 {
				t.Errorf("unbounded rerank request: %+v", request.Params)
			}
			text, ok := request.Params.Messages[0].Content.(*sdk.TextContent)
			if !ok {
				return nil, errors.New("expected text sampling content")
			}
			return &sdk.CreateMessageResult{Role: sdk.Role("assistant"), Model: "fixture", Content: &sdk.TextContent{Text: hostRerankOrder(text.Text, want)}}, nil
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "search_context", Arguments: map[string]any{"query": "storage", "rerank": true}})
	if err != nil || result.IsError {
		t.Fatalf("rerank result = %+v err=%v", result, err)
	}
	if sampled != 1 {
		t.Fatalf("expected one sampling call, got %d", sampled)
	}
	payload, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("unexpected structured content: %+v", result.StructuredContent)
	}
	items, ok := payload["items"].([]any)
	if !ok || len(items) != len(want) {
		t.Fatalf("expected %d items, got %+v", len(want), payload["items"])
	}
	for i, title := range want {
		item, _ := items[i].(map[string]any)
		if item["title"] != title {
			t.Fatalf("item %d title = %v, want %s (items=%+v)", i, item["title"], title, items)
		}
	}
}

// hostRerankOrder mimics a host model: it maps the requested target titles to
// the indices the reranker presented and returns them best-first as a
// comma-separated list.
func hostRerankOrder(prompt string, want []string) string {
	indexByTitle := map[string]string{}
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		dot := strings.IndexByte(line, '.')
		if dot <= 0 {
			continue
		}
		if _, err := strconv.Atoi(line[:dot]); err != nil {
			continue
		}
		for _, title := range want {
			if strings.Contains(line, title) {
				indexByTitle[title] = line[:dot]
			}
		}
	}
	order := make([]string, 0, len(want))
	for _, title := range want {
		if index, ok := indexByTitle[title]; ok {
			order = append(order, index)
		}
	}
	return strings.Join(order, ", ")
}
