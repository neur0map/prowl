package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neur0map/prowl/internal/capability"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestCoreToolsAreStructuredAndProposalRemainsReviewOnly(t *testing.T) {
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
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	contextService := &contextpacket.Service{Store: database, Knowledge: repository, Root: root}
	server := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Context: contextService, Knowledge: repository, Capabilities: catalog, Root: root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "tool-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "propose_knowledge_change" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint) {
			t.Fatalf("read-only annotations missing for %s: %+v", tool.Name, tool.Annotations)
		}
	}
	capabilities, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "search_capabilities", Arguments: map[string]any{"query": "context"}})
	if err != nil || capabilities.IsError || capabilities.StructuredContent == nil {
		t.Fatalf("capability result = %+v err=%v", capabilities, err)
	}
	if len(capabilities.Content) == 0 {
		t.Fatal("core capability result omitted MCP resource links")
	}
	if _, ok := capabilities.Content[0].(*sdk.ResourceLink); !ok {
		t.Fatalf("capability content[0] = %T, want *mcp.ResourceLink", capabilities.Content[0])
	}
	candidate := "---\ntype: Decision\ntitle: Keep review human\nprowl:\n  id: review-human\n---\nNever accept from MCP.\n"
	denied, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "propose_knowledge_change", Arguments: map[string]any{"target": "decisions/review.md", "candidate": candidate}})
	if err != nil {
		t.Fatal(err)
	}
	if !denied.IsError {
		t.Fatal("non-elicitation client created a proposal")
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".prowl", "proposals")); err == nil && len(entries) > 0 {
		t.Fatalf("denied proposal wrote files: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(repository.Root, "decisions", "review.md")); !os.IsNotExist(err) {
		t.Fatal("MCP proposal accepted canonical knowledge")
	}
}

func TestContextLensParityMCP(t *testing.T) {
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

	contextService := &contextpacket.Service{Store: database, Knowledge: repository, Root: root}
	input := contextSearchIn{Query: "authentication boundary"}
	raw, err := contextService.Search(contextpacket.Request{Question: input.Query, Mode: contextpacket.ModeCompact, BudgetTokens: 1800})
	if err != nil {
		t.Fatal(err)
	}
	want, err := contextpacket.MarshalCanonicalProjection(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, packet, err := (&handlers{context: contextService}).searchContext(context.Background(), nil, input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	if packet.TraceID != "" || bytes.Contains(encoded, []byte(`"trace_id"`)) || !bytes.Equal(encoded, want) {
		t.Fatalf("MCP canonical packet differs:\n got: %s\nwant: %s", encoded, want)
	}
}

// The history MCP tool used to discard its context; it now forwards it, so an MCP
// client that drops the call cancels the underlying git line-history walk instead
// of leaving it running on a large repository. The control call proves the tool
// still returns commits under its new ctx-first signature; the cancellation call
// shadows git with a shim that blocks ~10s, so a handler that ignored ctx would
// take that long -- a prompt empty return proves the context reaches git.
func TestHistoryToolForwardsContextCancellation(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package p\n\nfunc Target() int {\n\treturn 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "--quiet", "-m", "feat: add Target")

	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := index.Index(db, root, nil); err != nil {
		t.Fatal(err)
	}
	h := &handlers{q: query.New(db), root: root}

	// Control: the tool returns commits with its new ctx-first signature.
	if _, out, err := h.history(context.Background(), nil, historyIn{Symbol: "Target"}); err != nil {
		t.Fatalf("history tool errored on a tracked symbol: %v", err)
	} else if len(out.Commits) == 0 {
		t.Fatal("history tool returned no commits for a tracked, committed symbol")
	}

	// Cancellation: shadow git with a blocking shim, then cancel a call in flight.
	// `exec sleep` is a single process the ctx SIGKILL can reap cleanly.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan historyOut, 1)
	go func() {
		_, o, _ := h.history(ctx, nil, historyIn{Symbol: "Target"})
		done <- o
	}()
	time.Sleep(200 * time.Millisecond) // let git actually start
	start := time.Now()
	cancel()
	select {
	case o := <-done:
		if len(o.Commits) != 0 {
			t.Fatalf("cancelled history tool = %d commits, want 0", len(o.Commits))
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("history tool took %v after cancellation; ctx is not reaching git", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("history tool did not return within 3s of cancellation; ctx is not reaching git")
	}
}
