package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// helper: send one JSON-RPC request and return the parsed response.
func call(t *testing.T, s *Server, request string) jsonRPCResponse {
	t.Helper()
	in := strings.NewReader(request + "\n")
	var out bytes.Buffer
	if err := s.RunWith(in, &out); err != nil {
		t.Fatalf("RunWith error: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("no response received")
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, line)
	}
	return resp
}

func TestMCPInitialize(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.ID == nil {
		t.Fatal("response ID is nil")
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v, want 2024-11-05", result["protocolVersion"])
	}

	serverInfo, ok := result["serverInfo"].(map[string]interface{})
	if !ok {
		t.Fatal("serverInfo missing")
	}
	if serverInfo["name"] != "prowl" {
		t.Errorf("serverInfo.name = %v, want prowl", serverInfo["name"])
	}
}

func TestMCPToolsList(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}

	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) == 0 {
		t.Fatal("no tools returned")
	}

	if len(tools) != 5 {
		t.Errorf("expected 5 tools, got %d", len(tools))
	}

	expectedTools := map[string]bool{
		"prowl_overview":        false,
		"prowl_file_context":    false,
		"prowl_scope":           false,
		"prowl_impact":          false,
		"prowl_semantic_search": false,
	}

	for _, raw := range tools {
		tool := raw.(map[string]interface{})
		name := tool["name"].(string)
		if _, ok := expectedTools[name]; ok {
			expectedTools[name] = true
		} else {
			t.Errorf("unexpected tool: %s", name)
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestMCPToolsCallMissingQuery(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"prowl_semantic_search","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for missing query, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "query") {
		t.Errorf("error message should mention query: %s", resp.Error.Message)
	}
}

func TestMCPToolsCallNilEmbedder(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"prowl_semantic_search","arguments":{"query":"auth logic"}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for nil embedder, got nil")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "Embedder not available") {
		t.Errorf("error message = %s, want 'Embedder not available'", resp.Error.Message)
	}
}

func TestMCPToolsCallUnknownTool(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`)

	if resp.Error == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}

func TestMCPMethodNotFound(t *testing.T) {
	s := New(nil, nil, "")
	resp := call(t, s, `{"jsonrpc":"2.0","id":6,"method":"nonexistent"}`)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method, got nil")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

func TestMCPNotificationNoResponse(t *testing.T) {
	s := New(nil, nil, "")
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	if err := s.RunWith(in, &out); err != nil {
		t.Fatalf("RunWith error: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for notification, got: %s", out.String())
	}
}

func TestHeatTracking(t *testing.T) {
	s := New(nil, nil, "")

	// Unknown file should have zero heat
	score := s.heatScore("unknown.go")
	if score != 0.0 {
		t.Errorf("heat for unknown file = %f, want 0.0", score)
	}

	// Record access
	s.recordAccess("src/auth.go")
	score = s.heatScore("src/auth.go")
	if score <= 0.0 {
		t.Errorf("heat after access should be > 0, got %f", score)
	}

	// Multiple accesses should increase heat
	s.recordAccess("src/auth.go")
	s.recordAccess("src/auth.go")
	score2 := s.heatScore("src/auth.go")
	if score2 <= score {
		t.Errorf("heat should increase with more accesses: %f <= %f", score2, score)
	}
}

func TestMCPParseError(t *testing.T) {
	s := New(nil, nil, "")
	in := strings.NewReader("not json\n")
	var out bytes.Buffer
	if err := s.RunWith(in, &out); err != nil {
		t.Fatalf("RunWith error: %v", err)
	}

	line := strings.TrimSpace(out.String())
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700", resp.Error.Code)
	}
}
