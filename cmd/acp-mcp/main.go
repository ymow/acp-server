// cmd/acp-mcp — MCP JSON-RPC 2.0 over stdio transport for acp-server.
//
// Wire protocol: newline-delimited JSON-RPC 2.0 on stdin/stdout.
//
// Supported methods:
//
//	initialize       → serverInfo + capabilities
//	initialized      → notification (no response)
//	tools/list       → all ACP tool definitions
//	tools/call       → proxy to ACP HTTP API
//
// Environment:
//
//	ACP_SERVER_URL    — default http://localhost:8080
//	ACP_SESSION_TOKEN — agent session token
//	ACP_OWNER_TOKEN   — owner token (admin tools)
//	ACP_COVENANT_ID   — default covenant ID
//	ACP_AGENT_ID      — default agent ID
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ── JSON-RPC 2.0 types ────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── MCP tool definition ───────────────────────────────────────────────────────

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ── Tool catalogue ─────────────────────────────────────────────────────────────

// ownerTools is the set of tools that require X-Owner-Token auth.
var ownerTools = map[string]bool{
	"reject_agent":  true,
	"reject_draft":  true,
	"list_members":  true,
}

// routingProps are shared fields present on every tool's inputSchema.
var routingProps = map[string]any{
	"covenant_id": map[string]any{
		"type":        "string",
		"description": "Covenant ID (overrides ACP_COVENANT_ID env var)",
	},
	"agent_id": map[string]any{
		"type":        "string",
		"description": "Agent ID (overrides ACP_AGENT_ID env var)",
	},
	"session_token": map[string]any{
		"type":        "string",
		"description": "Session token (overrides ACP_SESSION_TOKEN env var)",
	},
	"owner_token": map[string]any{
		"type":        "string",
		"description": "Owner token (overrides ACP_OWNER_TOKEN env var)",
	},
}

// makeSchema merges routing props with tool-specific props.
func makeSchema(toolProps map[string]any, required []string) map[string]any {
	props := map[string]any{}
	for k, v := range routingProps {
		props[k] = v
	}
	for k, v := range toolProps {
		props[k] = v
	}
	s := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var allTools = []toolDef{
	{
		Name:        "propose_passage",
		Description: "Propose a draft passage for token consideration. Creates a pending token entry that requires owner approval via approve_draft.",
		InputSchema: makeSchema(map[string]any{
			"word_count": map[string]any{
				"type":        "integer",
				"description": "Number of words in the passage (required, must be > 0)",
			},
			"chapter": map[string]any{
				"type":        "string",
				"description": "Chapter or section identifier",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "Short summary of the passage content",
			},
			"content_hash": map[string]any{
				"type":        "string",
				"description": "SHA-256 hash of the passage content for deduplication",
			},
		}, []string{"word_count"}),
	},
	{
		Name:        "approve_draft",
		Description: "Approve a pending draft and award tokens to the proposer. Owner-only. Requires log_id or draft_id plus word_count.",
		InputSchema: makeSchema(map[string]any{
			"log_id": map[string]any{
				"type":        "string",
				"description": "Audit log ID of the propose_passage call (one of log_id or draft_id required)",
			},
			"draft_id": map[string]any{
				"type":        "string",
				"description": "Draft ID returned by propose_passage (one of log_id or draft_id required)",
			},
			"word_count": map[string]any{
				"type":        "integer",
				"description": "Approved word count (required, must be > 0)",
			},
			"acceptance_ratio": map[string]any{
				"type":        "number",
				"description": "Fraction of tokens to award, 0.0–1.0 (default: 1.0)",
			},
		}, []string{"word_count"}),
	},
	{
		Name:        "approve_agent",
		Description: "Activate a covenant member whose status is 'pending'. Owner-only.",
		InputSchema: makeSchema(map[string]any{
			"agent_id": map[string]any{
				"type":        "string",
				"description": "ID of the pending agent to approve (required)",
			},
		}, []string{"agent_id"}),
	},
	{
		Name:        "reject_agent",
		Description: "Reject a pending covenant member. Owner-only. Uses X-Owner-Token auth.",
		InputSchema: makeSchema(map[string]any{
			"agent_id": map[string]any{
				"type":        "string",
				"description": "ID of the pending agent to reject (required)",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Optional reason for rejection",
			},
		}, []string{"agent_id"}),
	},
	{
		Name:        "reject_draft",
		Description: "Reverse a token ledger entry and refund budget cost. Owner-only. Uses X-Owner-Token auth.",
		InputSchema: makeSchema(map[string]any{
			"log_id": map[string]any{
				"type":        "string",
				"description": "Audit log ID of the entry to reverse (required)",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Optional reason for rejection",
			},
		}, []string{"log_id"}),
	},
	{
		Name:        "configure_token_rules",
		Description: "Store token allocation rules on a DRAFT covenant. Owner-only.",
		InputSchema: makeSchema(map[string]any{
			"rules": map[string]any{
				"type":        "object",
				"description": "Token rules object (required); schema is application-defined",
			},
		}, []string{"rules"}),
	},
	{
		Name:        "generate_settlement_output",
		Description: "Trigger settlement output generation for a LOCKED or ACTIVE covenant. Owner-only.",
		InputSchema: makeSchema(map[string]any{}, nil),
	},
	{
		Name:        "confirm_settlement_output",
		Description: "Confirm a pending settlement output and transition covenant to SETTLED. Owner-only.",
		InputSchema: makeSchema(map[string]any{
			"settlement_output_id": map[string]any{
				"type":        "string",
				"description": "ID of the settlement output to confirm (required)",
			},
		}, []string{"settlement_output_id"}),
	},
	{
		Name:        "get_token_balance",
		Description: "Return confirmed, pending, and rejected token totals for an agent. Requires session token.",
		InputSchema: makeSchema(map[string]any{}, nil),
	},
	{
		Name:        "list_members",
		Description: "Return all covenant members with their token totals. Uses X-Owner-Token auth.",
		InputSchema: makeSchema(map[string]any{}, nil),
	},
	{
		Name:        "get_token_history",
		Description: "Return token ledger entries for an agent (ACR-20 Part 7). Optional from/to (ISO 8601) and status filter. Requires session token.",
		InputSchema: makeSchema(map[string]any{
			"from": map[string]any{
				"type":        "string",
				"description": "Inclusive lower bound, ISO 8601 timestamp (optional)",
			},
			"to": map[string]any{
				"type":        "string",
				"description": "Inclusive upper bound, ISO 8601 timestamp (optional)",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "Filter by ledger status: confirmed | pending | rejected (optional)",
			},
		}, nil),
	},
	{
		Name:        "leave_covenant",
		Description: "Mark the calling agent as having left the covenant. Confirmed token entries are preserved. Owner cannot leave.",
		InputSchema: makeSchema(map[string]any{
			"reason": map[string]any{
				"type":        "string",
				"description": "Optional reason for leaving",
			},
		}, nil),
	},
}

// ── Config from environment ───────────────────────────────────────────────────

type config struct {
	serverURL    string
	sessionToken string
	ownerToken   string
	covenantID   string
	agentID      string
}

func loadConfig() config {
	c := config{
		serverURL:    os.Getenv("ACP_SERVER_URL"),
		sessionToken: os.Getenv("ACP_SESSION_TOKEN"),
		ownerToken:   os.Getenv("ACP_OWNER_TOKEN"),
		covenantID:   os.Getenv("ACP_COVENANT_ID"),
		agentID:      os.Getenv("ACP_AGENT_ID"),
	}
	if c.serverURL == "" {
		c.serverURL = "http://localhost:8080"
	}
	return c
}

// ── Main loop ─────────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeError(enc, nil, -32700, "parse error")
			continue
		}

		// Notifications have no ID and expect no response.
		if req.Method == "notifications/initialized" || req.Method == "initialized" {
			continue
		}

		resp := dispatch(cfg, &req)
		_ = enc.Encode(resp)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "acp-mcp: scanner error: %v\n", err)
		os.Exit(1)
	}
}

// ── Dispatch ──────────────────────────────────────────────────────────────────

func dispatch(cfg config, req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return ok(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]any{
				"name":    "acp-server",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
		})

	case "tools/list":
		return ok(req.ID, map[string]any{
			"tools": allTools,
		})

	case "tools/call":
		result, err := handleToolCall(cfg, req.Params)
		if err != nil {
			return errResp(req.ID, -32603, err.Error())
		}
		return ok(req.ID, result)

	default:
		return errResp(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// ── Tool call proxy ───────────────────────────────────────────────────────────

// callArgs is the structure of tools/call params.
type callArgs struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// toolCallResult is the MCP content response.
type toolCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func handleToolCall(cfg config, raw json.RawMessage) (toolCallResult, error) {
	var args callArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return errContent("invalid tools/call params: " + err.Error()), nil
	}

	if args.Name == "" {
		return errContent("missing tool name"), nil
	}

	a := args.Arguments
	if a == nil {
		a = map[string]any{}
	}

	// Routing fields — arguments override env vars.
	covenantID := strArg(a, "covenant_id", cfg.covenantID)
	agentID := strArg(a, "agent_id", cfg.agentID)
	sessionToken := strArg(a, "session_token", cfg.sessionToken)
	ownerToken := strArg(a, "owner_token", cfg.ownerToken)

	// Extract tool-specific params (everything except routing keys).
	routingKeys := map[string]bool{
		"covenant_id":  true,
		"agent_id":     true,
		"session_token": true,
		"owner_token":  true,
	}
	params := map[string]any{}
	for k, v := range a {
		if !routingKeys[k] {
			params[k] = v
		}
	}

	// Query tools embed covenant_id / agent_id inside params.
	switch args.Name {
	case "get_token_balance":
		if _, ok := params["covenant_id"]; !ok {
			params["covenant_id"] = covenantID
		}
		if _, ok := params["agent_id"]; !ok {
			params["agent_id"] = agentID
		}
	case "list_members":
		if _, ok := params["covenant_id"]; !ok {
			params["covenant_id"] = covenantID
		}
	}

	body, err := json.Marshal(map[string]any{"params": params})
	if err != nil {
		return errContent("marshal params: " + err.Error()), nil
	}

	url := strings.TrimRight(cfg.serverURL, "/") + "/tools/" + args.Name
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return errContent("build request: " + err.Error()), nil
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Covenant-ID", covenantID)

	if ownerTools[args.Name] {
		httpReq.Header.Set("X-Owner-Token", ownerToken)
	} else {
		httpReq.Header.Set("X-Agent-ID", agentID)
		httpReq.Header.Set("X-Session-Token", sessionToken)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return errContent("HTTP request failed: " + err.Error()), nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errContent("read response: " + err.Error()), nil
	}

	isError := resp.StatusCode >= 400
	return toolCallResult{
		Content: []contentItem{{Type: "text", Text: string(respBody)}},
		IsError: isError,
	}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func ok(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func writeError(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	_ = enc.Encode(errResp(id, code, msg))
}

func errContent(msg string) toolCallResult {
	return toolCallResult{
		Content: []contentItem{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func strArg(args map[string]any, key, fallback string) string {
	if v, ok := args[key].(string); ok && v != "" {
		return v
	}
	return fallback
}
