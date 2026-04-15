package main_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inkmesh/acp-server/internal/api"
	"github.com/inkmesh/acp-server/internal/db"
)

// TestMVPIntegration runs the complete ACP MVP lifecycle over real HTTP.
// This is the "跑完一整個範例" test.
func TestMVPIntegration(t *testing.T) {
	// ── Setup ─────────────────────────────────────────────────────────────────
	conn, err := db.Open(t.TempDir() + "/integration.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	srv := httptest.NewServer(api.New(conn))
	defer srv.Close()

	c := &client{base: srv.URL, t: t}

	t.Log("═══════════════════════════════════════════════════════════")
	t.Log(" ACP MVP Integration Test — full lifecycle over HTTP")
	t.Log("═══════════════════════════════════════════════════════════")

	// ── Step 1: Create Covenant ────────────────────────────────────────────
	t.Log("\n▶ Step 1: Create Covenant (DRAFT)")
	var createResp struct {
		Covenant     map[string]any `json:"covenant"`
		OwnerAgentID string         `json:"owner_agent_id"`
	}
	c.post("/covenants", map[string]any{
		"title":             "The Last Algorithm",
		"space_type":        "book",
		"owner_platform_id": "pid_owner_alice",
	}, &createResp)

	covenantID := createResp.Covenant["covenant_id"].(string)
	ownerAgentID := createResp.OwnerAgentID
	ownerToken := createResp.Covenant["owner_token"].(string) // A-2/A-4: shown once at create
	t.Logf("  covenant_id   = %s", covenantID)
	t.Logf("  owner_agent   = %s", ownerAgentID)
	t.Logf("  state         = %s", createResp.Covenant["state"])
	t.Logf("  owner_token   = %s…", ownerToken[:8])

	ownerH := map[string]string{"X-Owner-Token": ownerToken}

	// ── Step 2: Add tier + transition to OPEN ──────────────────────────────
	t.Log("\n▶ Step 2: Add tier → OPEN")
	c.post("/covenants/"+covenantID+"/tiers", map[string]any{
		"tier_id":          "contributor",
		"display_name":     "Contributor",
		"token_multiplier": 1.0,
	}, nil)

	var transResp struct{ Covenant map[string]any `json:"covenant"` }
	c.postH("/covenants/"+covenantID+"/transition", ownerH,
		map[string]any{"target_state": "OPEN"}, &transResp)
	t.Logf("  state = %s", transResp.Covenant["state"])

	// ── Step 3: Two agents join ────────────────────────────────────────────
	t.Log("\n▶ Step 3: Agent A and Agent B join")
	var joinA struct{ Member map[string]any `json:"member"` }
	c.post("/covenants/"+covenantID+"/join",
		map[string]any{"platform_id": "pid_agent_bob", "tier_id": "contributor"}, &joinA)
	agentA := joinA.Member["agent_id"].(string)
	t.Logf("  Agent A = %s", agentA)

	var joinB struct{ Member map[string]any `json:"member"` }
	c.post("/covenants/"+covenantID+"/join",
		map[string]any{"platform_id": "pid_agent_carol", "tier_id": "contributor"}, &joinB)
	agentB := joinB.Member["agent_id"].(string)
	t.Logf("  Agent B = %s", agentB)

	// → ACTIVE
	c.postH("/covenants/"+covenantID+"/transition", ownerH,
		map[string]any{"target_state": "ACTIVE"}, &transResp)
	t.Logf("  state = %s", transResp.Covenant["state"])

	// ── Step 4: Set budget ─────────────────────────────────────────────────
	t.Log("\n▶ Step 4: Set global budget = $100")
	c.postH("/covenants/"+covenantID+"/budget", ownerH,
		map[string]any{"budget_limit": 100.0}, nil)

	// ── Step 5: Issue session tokens ───────────────────────────────────────
	t.Log("\n▶ Step 5: Issue session tokens (REVIEW-14)")
	var tokA struct{ Token string `json:"token"` }
	c.postH("/sessions/issue", ownerH, map[string]any{
		"agent_id": agentA, "covenant_id": covenantID,
	}, &tokA)
	t.Logf("  Agent A token issued (len=%d)", len(tokA.Token))

	var tokOwner struct{ Token string `json:"token"` }
	c.postH("/sessions/issue", ownerH, map[string]any{
		"agent_id": ownerAgentID, "covenant_id": covenantID,
	}, &tokOwner)

	// A-1: Agent B also needs a session token to call tool endpoints.
	var tokB struct{ Token string `json:"token"` }
	c.postH("/sessions/issue", ownerH, map[string]any{
		"agent_id": agentB, "covenant_id": covenantID,
	}, &tokB)
	t.Logf("  Agent B token issued (len=%d)", len(tokB.Token))

	// ── Step 5b: Owner approves Agent A and Agent B (WI-1: join is now pending) ──
	t.Log("\n▶ Step 5b: Owner approves Agent A and Agent B")
	var apprAgentA struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, ownerAgentID, tokOwner.Token, "approve_agent",
		map[string]any{"agent_id": agentA}, &apprAgentA)
	t.Logf("  Agent A approved → status=%v", apprAgentA.Receipt["extra"])
	var apprAgentB struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, ownerAgentID, tokOwner.Token, "approve_agent",
		map[string]any{"agent_id": agentB}, &apprAgentB)
	t.Logf("  Agent B approved → status=%v", apprAgentB.Receipt["extra"])

	// ── Step 6: Agent A proposes 1000-word passage ─────────────────────────
	t.Log("\n▶ Step 6: Agent A proposes passage (1000 words)")
	var propA struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, agentA, tokA.Token, "propose_passage",
		map[string]any{"word_count": 1000}, &propA)
	t.Logf("  receipt_id = %s", propA.Receipt["receipt_id"])
	t.Logf("  status     = %s  (pending = tokens await approval)", propA.Receipt["status"])
	t.Logf("  log_hash   = %s…", propA.Receipt["log_hash"].(string)[:16])

	// ── Step 7: Owner approves Agent A's draft ─────────────────────────────
	t.Log("\n▶ Step 7: Owner approves Agent A draft (100% ratio)")
	draftA := c.getDraftID(conn, covenantID, agentA)
	var apprA struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, ownerAgentID, tokOwner.Token, "approve_draft", map[string]any{
		"draft_id":         draftA,
		"word_count":       1000,
		"acceptance_ratio": 1.0,
	}, &apprA)
	t.Logf("  tokens_awarded = %.0f", apprA.Receipt["tokens_awarded"])
	t.Logf("  log_hash       = %s…", apprA.Receipt["log_hash"].(string)[:16])

	// ── Step 8: Agent B proposes 500-word passage ──────────────────────────
	t.Log("\n▶ Step 8: Agent B proposes passage (500 words)")
	var propB struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, agentB, tokB.Token, "propose_passage",
		map[string]any{"word_count": 500}, &propB)
	t.Logf("  status = %s", propB.Receipt["status"])

	// ── Step 9: Owner approves Agent B's draft ─────────────────────────────
	t.Log("\n▶ Step 9: Owner approves Agent B draft (80% ratio)")
	draftB := c.getDraftID(conn, covenantID, agentB)
	var apprB struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, ownerAgentID, tokOwner.Token, "approve_draft", map[string]any{
		"draft_id":         draftB,
		"word_count":       500,
		"acceptance_ratio": 0.8,
	}, &apprB)
	t.Logf("  tokens_awarded = %.0f  (500 × 0.8 = 400)", apprB.Receipt["tokens_awarded"])

	// ── Step 10: Check live state ──────────────────────────────────────────
	t.Log("\n▶ Step 10: Query live covenant state (AC-6 Reactor MVP)")
	var stateResp map[string]any
	c.get(fmt.Sprintf("/covenants/%s/state?agent_id=%s", covenantID, agentA), &stateResp)
	t.Logf("  lifecycle_status = %s", stateResp["lifecycle_status"])
	if bud, ok := stateResp["budget"].(map[string]any); ok {
		t.Logf("  budget_limit     = %.2f", bud["limit"])
		t.Logf("  budget_spent     = %.2f", bud["spent"])
	}

	// ── Step 11: Rotate token for Agent A ─────────────────────────────────
	t.Log("\n▶ Step 11: Rotate session token (REVIEW-14 grace period)")
	var rotResp map[string]any
	// A-3: must present current valid token to rotate.
	hdr := c.postH("/sessions/rotate",
		map[string]string{"X-Session-Token": tokA.Token},
		map[string]any{"agent_id": agentA, "covenant_id": covenantID},
		&rotResp)
	t.Logf("  Acp-Token-Warning = %q", hdr.Get("Acp-Token-Warning"))
	newTokA := rotResp["token"].(string)

	// Old token still works (grace)
	var graceCheck struct{ Receipt map[string]any `json:"receipt"` }
	statusCode := c.toolWithStatus(covenantID, agentA, tokA.Token, "propose_passage",
		map[string]any{"word_count": 100}, &graceCheck)
	t.Logf("  Old token during grace: HTTP %d (200 = still valid)", statusCode)

	// New token works too
	c.tool(covenantID, agentA, newTokA, "propose_passage",
		map[string]any{"word_count": 100}, &graceCheck)
	t.Logf("  New token: HTTP 200 ✓")

	// ── Step 12: Lock + Settle ─────────────────────────────────────────────
	t.Log("\n▶ Step 12: Lock covenant")
	c.postH("/covenants/"+covenantID+"/transition", ownerH,
		map[string]any{"target_state": "LOCKED"}, &transResp)
	t.Logf("  state = %s", transResp.Covenant["state"])

	t.Log("\n▶ Step 13: Generate settlement output")
	var settleResp struct{ Receipt map[string]any `json:"receipt"` }
	c.tool(covenantID, ownerAgentID, tokOwner.Token, "generate_settlement_output",
		map[string]any{}, &settleResp)
	outputID := settleResp.Receipt["extra"].(map[string]any)["output_id"].(string)
	t.Logf("  output_id = %s", outputID)

	// Read settlement from DB directly for full detail
	var distsJSON string
	var totalTokens int
	conn.QueryRow(`SELECT total_tokens, distributions FROM settlement_outputs WHERE output_id=?`,
		outputID).Scan(&totalTokens, &distsJSON)

	var dists []map[string]any
	json.Unmarshal([]byte(distsJSON), &dists)
	t.Logf("  total_tokens = %d", totalTokens)
	for _, d := range dists {
		t.Logf("  Agent %-20s tokens=%.0f  final_share=%.4f%%",
			d["agent_id"], d["ink_tokens"], d["final_share_pct"])
	}

	// ── Step 14: Verify audit chain ────────────────────────────────────────
	t.Log("\n▶ Step 14: Verify audit hash chain")
	var verifyResp struct {
		Valid      bool     `json:"valid"`
		Violations []string `json:"violations"`
	}
	c.get("/covenants/"+covenantID+"/audit/verify", &verifyResp)
	if !verifyResp.Valid {
		t.Errorf("  ✗ chain INVALID: %v", verifyResp.Violations)
	} else {
		t.Log("  ✓ chain valid — 0 violations")
	}

	// ── Step 15: Dump audit log ────────────────────────────────────────────
	t.Log("\n▶ Step 15: Audit log (last entries)")
	var auditResp struct {
		Entries []map[string]any `json:"entries"`
		Count   int              `json:"count"`
	}
	// A-4: audit log requires a valid covenant participant session token.
	c.getH("/covenants/"+covenantID+"/audit",
		map[string]string{"X-Session-Token": tokOwner.Token},
		&auditResp)
	t.Logf("  total entries = %d", auditResp.Count)
	for i := len(auditResp.Entries) - 1; i >= 0; i-- {
		e := auditResp.Entries[i]
		t.Logf("  [seq %-2.0f] %-35s %s  tokens_delta=%.0f  hash=%s…",
			e["sequence"], e["tool_name"], e["result"], e["tokens_delta"],
			e["hash"].(string)[:16])
	}

	t.Log("\n═══════════════════════════════════════════════════════════")
	t.Log(" MVP PASS — full lifecycle verified end-to-end over HTTP")
	t.Log("═══════════════════════════════════════════════════════════")
}

// ── HTTP client helpers ───────────────────────────────────────────────────────

type client struct {
	base string
	t    *testing.T
}

func (c *client) post(path string, body any, out any) {
	c.t.Helper()
	c.postH(path, nil, body, out)
}

// postWithHeaders is kept for backward compatibility with existing call sites.
func (c *client) postWithHeaders(path string, body any, out any) http.Header {
	c.t.Helper()
	return c.postH(path, nil, body, out)
}

// postH performs a POST with optional extra request headers and returns response headers.
func (c *client) postH(path string, headers map[string]string, body any, out any) http.Header {
	c.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.t.Fatalf("POST %s → %d: %s", path, resp.StatusCode, data)
	}
	if out != nil {
		json.Unmarshal(data, out)
	}
	return resp.Header
}

func (c *client) get(path string, out any) {
	c.t.Helper()
	c.getH(path, nil, out)
}

// getH performs a GET with optional extra request headers.
func (c *client) getH(path string, headers map[string]string, out any) {
	c.t.Helper()
	req, _ := http.NewRequest("GET", c.base+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.t.Fatalf("GET %s → %d: %s", path, resp.StatusCode, data)
	}
	if out != nil {
		json.Unmarshal(data, out)
	}
}

func (c *client) tool(covenantID, agentID, token, toolName string, params any, out any) {
	c.t.Helper()
	c.toolWithStatus(covenantID, agentID, token, toolName, params, out)
}

func (c *client) toolWithStatus(covenantID, agentID, token, toolName string, params any, out any) int {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"params": params})
	req, _ := http.NewRequest("POST", c.base+"/tools/"+toolName, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Covenant-ID", covenantID)
	req.Header.Set("X-Agent-ID", agentID)
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("POST /tools/%s: %v", toolName, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		c.t.Fatalf("POST /tools/%s → %d: %s", toolName, resp.StatusCode, data)
	}
	if out != nil {
		json.Unmarshal(data, out)
	}
	return resp.StatusCode
}

func (c *client) getDraftID(conn *sql.DB, covenantID, agentID string) string {
	c.t.Helper()
	var draftID string
	err := conn.QueryRow(
		`SELECT draft_id FROM pending_tokens WHERE covenant_id=? AND agent_id=? LIMIT 1`,
		covenantID, agentID,
	).Scan(&draftID)
	if err != nil {
		c.t.Fatalf("getDraftID(%s): %v", agentID, err)
	}
	return draftID
}

func init() { _ = fmt.Sprintf }
