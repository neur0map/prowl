package mcp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

func TestResourcesListReadAndRejectTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := "src/main #%.go"
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(sourcePath)), []byte("package main\n\n// RoundTripResource proves generated URI handling.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository := knowledge.NewRepository(filepath.Join(root, ".prowl", "knowledge"), okfv01.Codec{})
	if err := repository.Init(); err != nil {
		t.Fatal(err)
	}
	doc, err := (okfv01.Codec{}).Parse("concepts/storage.md", []byte("---\ntype: Concept\ntitle: Storage\nprowl:\n  id: storage\n---\nCanonical storage notes.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Write(doc); err != nil {
		t.Fatal(err)
	}
	fallback, err := (okfv01.Codec{}).Parse("concepts/reserved #?.md", []byte("---\ntype: Concept\ntitle: ReservedRoundTrip\n---\nFallback concept identifier.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Write(fallback); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(filepath.Join(root, ".prowl", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := index.IndexWithOptions(database, root, index.Options{Languages: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	contextService := &contextpacket.Service{Store: database, Knowledge: repository, Root: root}
	server := NewServerWithOptions(query.New(database), database, "test", nil, nil, nil, ServerOptions{Surface: SurfaceCore, Context: contextService, Knowledge: repository, Root: root})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "resource-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	listed, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, resource := range listed.Resources {
		uris = append(uris, resource.URI)
		if resource.MIMEType == "" || resource.Annotations == nil || len(resource.Annotations.Audience) == 0 || resource.Annotations.LastModified == "" {
			t.Fatalf("resource annotations incomplete: %+v", resource)
		}
	}
	sort.Strings(uris)
	wantURIs := []string{"prowl://workspace/current/changes", "prowl://workspace/current/knowledge/index", "prowl://workspace/current/overview", "prowl://workspaces"}
	if !equalStrings(uris, wantURIs) {
		t.Fatalf("resource URIs = %v", uris)
	}
	templates, err := session.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates.ResourceTemplates) != 2 {
		t.Fatalf("template count = %d", len(templates.ResourceTemplates))
	}

	concept, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "prowl://workspace/current/concept/storage"})
	if err != nil || len(concept.Contents) != 1 || !strings.Contains(concept.Contents[0].Text, "Canonical storage notes") {
		t.Fatalf("concept resource = %+v err=%v", concept, err)
	}
	source, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "prowl://workspace/current/source/src%2Fmain%20%23%25.go"})
	if err != nil || len(source.Contents) != 1 || !strings.Contains(source.Contents[0].Text, "RoundTripResource") {
		t.Fatalf("source resource = %+v err=%v", source, err)
	}
	packet, err := contextService.Search(contextpacket.Request{Question: "RoundTripResource", Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	if err != nil || len(packet.Items) == 0 {
		t.Fatalf("source packet = %+v err=%v", packet, err)
	}
	fetched, err := contextService.Get(contextpacket.Request{IDs: []string{packet.Items[0].ID}, Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	if err != nil || len(fetched.Items) != 1 {
		t.Fatalf("source get packet = %+v err=%v", fetched, err)
	}
	generatedSource, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: fetched.Items[0].DetailResource})
	if err != nil || len(generatedSource.Contents) != 1 || !strings.Contains(generatedSource.Contents[0].Text, "RoundTripResource") {
		t.Fatalf("generated source URI %q failed: %+v err=%v", fetched.Items[0].DetailResource, generatedSource, err)
	}
	conceptPacket, err := contextService.Search(contextpacket.Request{Question: "ReservedRoundTrip", Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	if err != nil || len(conceptPacket.Items) == 0 {
		t.Fatalf("concept packet = %+v err=%v", conceptPacket, err)
	}
	generatedConcept, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: conceptPacket.Items[0].DetailResource})
	if err != nil || len(generatedConcept.Contents) != 1 || !strings.Contains(generatedConcept.Contents[0].Text, "Fallback concept identifier") {
		t.Fatalf("generated concept URI %q failed: %+v err=%v", conceptPacket.Items[0].DetailResource, generatedConcept, err)
	}
	if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "prowl://workspace/current/source/%2e%2e/%2e%2e/etc/passwd"}); err == nil {
		t.Fatal("source traversal was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "prowl://workspace/current/source/leak.txt"}); err == nil {
		t.Fatal("source resource followed a symlink outside the workspace")
	}
	if err := os.WriteFile(filepath.Join(root, "oversized.txt"), bytes.Repeat([]byte("x"), maxSourceResourceBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "prowl://workspace/current/source/oversized.txt"}); err == nil {
		t.Fatal("oversized source resource was accepted")
	}
}
