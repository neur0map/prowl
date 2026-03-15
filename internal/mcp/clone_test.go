package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestStoreForPrimary(t *testing.T) {
	primary, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	s := New(primary, nil, "/ctx/primary", "dev")

	// Default (empty project) should return primary store
	st, ctxDir, err := s.storeFor(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("storeFor error: %v", err)
	}
	if st != primary {
		t.Error("expected primary store")
	}
	if ctxDir != "/ctx/primary" {
		t.Errorf("contextDir = %q, want /ctx/primary", ctxDir)
	}

	// Explicit "primary"
	st, _, err = s.storeFor(json.RawMessage(`{"project":"primary"}`))
	if err != nil {
		t.Fatalf("storeFor error: %v", err)
	}
	if st != primary {
		t.Error("expected primary store")
	}
}

func TestStoreForComparison(t *testing.T) {
	primary, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	comparison, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer comparison.Close()

	s := New(primary, nil, "/ctx/primary", "dev")
	s.compareStore = comparison
	s.compareContextDir = "/ctx/comparison"
	s.compareRepo = "owner/repo"

	st, ctxDir, err := s.storeFor(json.RawMessage(`{"project":"comparison"}`))
	if err != nil {
		t.Fatalf("storeFor error: %v", err)
	}
	if st != comparison {
		t.Error("expected comparison store")
	}
	if ctxDir != "/ctx/comparison" {
		t.Errorf("contextDir = %q, want /ctx/comparison", ctxDir)
	}
}

func TestStoreForNoComparison(t *testing.T) {
	primary, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	s := New(primary, nil, "/ctx/primary", "dev")

	_, _, err = s.storeFor(json.RawMessage(`{"project":"comparison"}`))
	if err == nil {
		t.Fatal("expected error when no comparison loaded")
	}
	if !strings.Contains(err.Error(), "no comparison repo loaded") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCloneStatusEmpty(t *testing.T) {
	s := New(nil, nil, "", "dev")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_clone_status","arguments":{}}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "no comparison repo loaded") {
		t.Errorf("expected 'no comparison repo loaded', got: %s", text)
	}
}

func TestCloneClose(t *testing.T) {
	primary, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	comparison, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't defer close — handleCloneClose will close it

	s := New(primary, nil, "", "dev")
	s.compareStore = comparison
	s.compareContextDir = "/ctx/comparison"
	s.compareRepo = "owner/repo"

	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_clone_close","arguments":{}}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	if s.compareStore != nil {
		t.Error("compareStore should be nil after close")
	}
	if s.compareRepo != "" {
		t.Error("compareRepo should be empty after close")
	}

	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "closed") {
		t.Errorf("expected 'closed' in response, got: %s", text)
	}
}

func TestCloneCloseIdempotent(t *testing.T) {
	s := New(nil, nil, "", "dev")

	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_clone_close","arguments":{}}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result := resp.Result.(map[string]interface{})
	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "no comparison repo to close") {
		t.Errorf("expected 'no comparison repo to close', got: %s", text)
	}
}
