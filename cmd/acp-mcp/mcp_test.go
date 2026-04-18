package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolsList(t *testing.T) {
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	}

	cfg := loadConfig()
	resp := dispatch(cfg, &req)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	// Marshal result back to JSON so we can check the output string.
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	out := string(b)

	for _, name := range []string{"propose_passage", "approve_draft", "reject_agent"} {
		if !strings.Contains(out, name) {
			t.Errorf("tools/list response missing %q", name)
		}
	}
}

func TestToolsListAllTen(t *testing.T) {
	expected := []string{
		"propose_passage",
		"approve_draft",
		"approve_agent",
		"reject_agent",
		"reject_draft",
		"configure_token_rules",
		"configure_anti_gaming",
		"generate_settlement_output",
		"confirm_settlement_output",
		"get_token_balance",
		"list_members",
		"get_token_history",
		"leave_covenant",
	}

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	}

	cfg := loadConfig()
	resp := dispatch(cfg, &req)
	b, _ := json.Marshal(resp.Result)
	out := string(b)

	for _, name := range expected {
		if !strings.Contains(out, name) {
			t.Errorf("tools/list missing %q", name)
		}
	}
}

func TestInitialize(t *testing.T) {
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`3`),
		Method:  "initialize",
	}

	cfg := loadConfig()
	resp := dispatch(cfg, &req)

	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}

	b, _ := json.Marshal(resp.Result)
	out := string(b)

	if !strings.Contains(out, "acp-server") {
		t.Errorf("initialize response missing serverInfo name: %s", out)
	}
}

func TestUnknownMethod(t *testing.T) {
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`4`),
		Method:  "nonexistent/method",
	}

	cfg := loadConfig()
	resp := dispatch(cfg, &req)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method, got nil")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
}
