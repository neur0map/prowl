package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/store"
)

func TestProposalUsesElicitationWhenClientSupportsIt(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		action      string
		confirm     bool
		wantCreated bool
	}{
		{name: "decline blocks write", action: "decline"},
		{name: "accepted confirmation creates review proposal", action: "accept", confirm: true, wantCreated: true},
		{name: "accept without confirmation blocks write", action: "accept"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".prowl"), 0o755); err != nil {
				t.Fatal(err)
			}
			database, err := store.Open(filepath.Join(root, ".prowl", "index.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repository := knowledge.NewRepository(filepath.Join(root, ".prowl", "knowledge"), okfv01.Codec{})
			if err := repository.Init(); err != nil {
				t.Fatal(err)
			}
			server := NewServerWithOptions(nil, database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Knowledge: repository, Root: root})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverTransport, clientTransport := sdk.NewInMemoryTransports()
			go func() { _ = server.Run(ctx, serverTransport) }()
			elicited := 0
			client := sdk.NewClient(&sdk.Implementation{Name: "elicitation-test", Version: "1"}, &sdk.ClientOptions{
				ElicitationHandler: func(_ context.Context, request *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
					elicited++
					if request.Params.Message == "" || request.Params.RequestedSchema == nil {
						t.Fatalf("incomplete elicitation: %+v", request.Params)
					}
					return &sdk.ElicitResult{Action: testCase.action, Content: map[string]any{"confirm": testCase.confirm}}, nil
				},
			})
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			candidate := "---\ntype: Decision\ntitle: Elicitation\nprowl:\n  id: elicitation\n---\nRequire host approval.\n"
			result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "propose_knowledge_change", Arguments: map[string]any{"target": "decisions/elicitation.md", "candidate": candidate}})
			if err != nil {
				t.Fatal(err)
			}
			if elicited != 1 || result.IsError == testCase.wantCreated {
				t.Fatalf("result IsError=%v elicited=%d", result.IsError, elicited)
			}
			entries, readErr := os.ReadDir(filepath.Join(root, ".prowl", "proposals"))
			created := readErr == nil && len(entries) == 1
			if created != testCase.wantCreated {
				t.Fatalf("proposal created=%v entries=%v err=%v", created, entries, readErr)
			}
			if _, err := os.Stat(filepath.Join(repository.Root, "decisions", "elicitation.md")); !os.IsNotExist(err) {
				t.Fatal("elicitation path accepted canonical knowledge")
			}
		})
	}
}
