package mcp

import (
	"testing"
)

func TestProwlScopeWithoutEmbedder(t *testing.T) {
	st := setupTestStore(t)
	defer st.Close()

	s := New(st, nil, t.TempDir())
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_scope","arguments":{"task":"fix auth"}}}`)

	if resp.Error == nil {
		t.Fatal("expected error when embedder is nil")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603", resp.Error.Code)
	}
}

func TestProwlScopeMissingTask(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prowl_scope","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing task")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}
