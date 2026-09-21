package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestTrackedRejectsCallWhenFreshnessFails(t *testing.T) {
	sentinel := errors.New("refresh failed")
	called := false
	h := &handlers{beforeCall: func(context.Context) error { return sentinel }}
	wrapped := tracked(h, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, struct{}, error) {
		called = true
		return nil, struct{}{}, nil
	})
	if _, _, err := wrapped(context.Background(), nil, struct{}{}); !errors.Is(err, sentinel) {
		t.Fatalf("tracked error = %v, want sentinel", err)
	}
	if called {
		t.Fatal("tool handler ran after freshness failure")
	}
}

func TestMCPIntegration(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := index.Index(s, filepath.Join("..", "..", "testdata", "sample-config"), nil); err != nil {
		t.Fatal(err)
	}

	var calls int32
	srv := NewServer(query.New(s), s, "test",
		func(context.Context) (string, error) { return "reindexed", nil },
		func(context.Context) (doctor.Report, error) { return doctor.Run(s, config.Rules{}, doctor.Options{}) },
		func() { atomic.AddInt32(&calls, 1) })

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

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("tool %s returned error: %+v", name, res.Content)
		}
		if len(res.Content) == 0 {
			t.Fatalf("tool %s returned no content", name)
		}
		tc, ok := res.Content[0].(*sdk.TextContent)
		if !ok {
			t.Fatalf("tool %s: content not text: %T", name, res.Content[0])
		}
		return tc.Text
	}

	if out := call("find_symbol", map[string]any{"name": "M.apply"}); !strings.Contains(out, "M.apply") || !strings.Contains(out, "nvim/lua/opts.lua") {
		t.Fatalf("find_symbol: %s", out)
	}
	if out := call("blast_radius", map[string]any{"path": "hypr/colors.conf"}); !strings.Contains(out, "hypr/hyprland.conf") {
		t.Fatalf("blast_radius: %s", out)
	}
	if out := call("find_callers", map[string]any{"path": "hypr/colors.conf"}); !strings.Contains(out, "hypr/hyprland.conf") {
		t.Fatalf("find_callers: %s", out)
	}
	if out := call("status", nil); !strings.Contains(out, "files: 11") {
		t.Fatalf("status: %s", out)
	}
	if out := call("reindex", nil); !strings.Contains(out, "reindexed") {
		t.Fatalf("reindex: %s", out)
	}
	if out := call("doctor", nil); !strings.Contains(out, "score") {
		t.Fatalf("doctor: %s", out)
	}
	if out := call("overview", nil); !strings.Contains(out, "roles") {
		t.Fatalf("overview: %s", out)
	}
	if out := call("clusters", nil); !strings.Contains(out, "clusters") {
		t.Fatalf("clusters: %s", out)
	}

	lt, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lt.Tools) != 20 {
		t.Fatalf("tool count = %d, want 20", len(lt.Tools))
	}

	// The tracked wrapper should have recorded usage for the calls above.
	if st, _ := s.Stats(); st.Queries == 0 || st.AnswerBytes == 0 {
		t.Fatalf("usage stats not recorded: %+v", st)
	}
	if atomic.LoadInt32(&calls) < 8 {
		t.Fatalf("onCall fired %d times, want one per tool call", calls)
	}
}

func TestLegacyAndAllSurfacesApplyBeforeCall(t *testing.T) {
	for _, surface := range []Surface{SurfaceLegacy, SurfaceAll} {
		t.Run(string(surface), func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			calls := 0
			srv := NewServerWithOptions(query.New(st), st, "test", nil, nil, nil, ServerOptions{
				Surface:    surface,
				BeforeCall: func(context.Context) error { calls++; return errors.New("refresh failed") },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverT, clientT := sdk.NewInMemoryTransports()
			go func() { _ = srv.Run(ctx, serverT) }()
			client := sdk.NewClient(&sdk.Implementation{Name: "freshness-test", Version: "1"}, nil)
			sess, err := client.Connect(ctx, clientT, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			result, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "status", Arguments: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !result.IsError {
				t.Fatalf("calls=%d result=%+v; want one blocked legacy call", calls, result)
			}
		})
	}
}
