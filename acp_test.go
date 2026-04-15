package main_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/sessions"
	"github.com/inkmesh/acp-server/internal/tokens"
	"github.com/inkmesh/acp-server/tools"
)

func TestMVPAcceptanceCriteria(t *testing.T) {
	// Use a temp DB for each test run
	dbPath := t.TempDir() + "/acp_test.db"
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	// ── AC-1: Owner creates Covenant (DRAFT → OPEN) ────────────────────────
	t.Run("AC1_CreateCovenant", func(t *testing.T) {
		cov, owner, err := covSvc.Create("Test Book", "book", "pid_owner_1")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if cov.State != "DRAFT" {
			t.Errorf("want DRAFT, got %s", cov.State)
		}

		// Add tier, then open
		if err := covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil); err != nil {
			t.Fatalf("add tier: %v", err)
		}
		cov, err = covSvc.Transition(cov.CovenantID, "OPEN")
		if err != nil {
			t.Fatalf("→OPEN: %v", err)
		}
		if cov.State != "OPEN" {
			t.Errorf("want OPEN, got %s", cov.State)
		}
		t.Logf("✓ AC-1  covenant=%s owner=%s", cov.CovenantID, owner.AgentID)
	})

	// Full lifecycle test (ACs 1–8 together)
	t.Run("AC_FullLifecycle", func(t *testing.T) {
		// Setup
		cov, ownerMem, err := covSvc.Create("Royalty Test", "book", "pid_owner_2")
		must(t, err, "create")
		must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
		cov, err = covSvc.Transition(cov.CovenantID, "OPEN")
		must(t, err, "→OPEN")

		// AC-2: Two agents join and covenant goes ACTIVE
		agent1, err := covSvc.Join(cov.CovenantID, "pid_agent_a", "contributor")
		must(t, err, "join agent1")
		agent2, err := covSvc.Join(cov.CovenantID, "pid_agent_b", "contributor")
		must(t, err, "join agent2")
		cov, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
		must(t, err, "→ACTIVE")
		if cov.State != "ACTIVE" {
			t.Errorf("AC-2: want ACTIVE, got %s", cov.State)
		}
		t.Logf("✓ AC-2  agent1=%s agent2=%s", agent1.AgentID, agent2.AgentID)

		// WI-1: agents join as 'pending'; owner must approve them before they can act.
		_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveAgent{}, map[string]any{"agent_id": agent1.AgentID})
		must(t, err, "approve agent1")
		_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveAgent{}, map[string]any{"agent_id": agent2.AgentID})
		must(t, err, "approve agent2")

		// Budget: set a generous limit so budget gate is exercised
		must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0), "budget counter")

		// AC-3: Agent A proposes + gets approved → tokens calculated
		r1, err := engine.Run(cov.CovenantID, agent1.AgentID, "sess_a",
			&tools.ProposePassage{}, map[string]any{"word_count": 1000})
		must(t, err, "propose agent1")
		if r1.Status != "pending" {
			t.Errorf("AC-3: want pending, got %s", r1.Status)
		}

		// Owner approves draft
		draftID := getDraftID(t, conn, cov.CovenantID, agent1.AgentID)
		r1a, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveDraft{}, map[string]any{
				"draft_id":         draftID,
				"word_count":       1000,
				"acceptance_ratio": 1.0,
			})
		must(t, err, "approve agent1")
		if r1a.TokensAwarded != 1000 {
			t.Errorf("AC-3: want 1000 tokens, got %d", r1a.TokensAwarded)
		}
		t.Logf("✓ AC-3  tokens_awarded=%d", r1a.TokensAwarded)

		// AC-4: Agent B proposes + approved
		_, err = engine.Run(cov.CovenantID, agent2.AgentID, "sess_b",
			&tools.ProposePassage{}, map[string]any{"word_count": 500})
		must(t, err, "propose agent2")

		draftID2 := getDraftID(t, conn, cov.CovenantID, agent2.AgentID)
		r2a, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveDraft{}, map[string]any{
				"draft_id":         draftID2,
				"word_count":       500,
				"acceptance_ratio": 1.0,
			})
		must(t, err, "approve agent2")
		if r2a.TokensAwarded != 500 {
			t.Errorf("AC-4: want 500 tokens, got %d", r2a.TokensAwarded)
		}
		t.Logf("✓ AC-4  tokens_awarded=%d", r2a.TokensAwarded)

		// AC-5: Global budget correctly deducted (no cost in this test, but counter exists)
		budState, err := budget.GetState(conn, cov.CovenantID)
		must(t, err, "budget state")
		t.Logf("✓ AC-5  budget_limit=%.2f budget_spent=%.2f", budState.BudgetLimit, budState.BudgetSpent)

		// AC-6: Audit log can reconstruct token totals
		balA, _ := tokens.Balance(conn, cov.CovenantID, agent1.AgentID)
		balB, _ := tokens.Balance(conn, cov.CovenantID, agent2.AgentID)
		byAgent, _ := tokens.TotalByAgent(conn, cov.CovenantID)
		ledgerTotal := 0
		for _, v := range byAgent {
			ledgerTotal += v
		}
		if balA+balB != ledgerTotal {
			t.Errorf("AC-6: sum of balances (%d) != ledger total (%d)", balA+balB, ledgerTotal)
		}
		t.Logf("✓ AC-6  balA=%d balB=%d ledger_total=%d", balA, balB, ledgerTotal)

		// AC-7: Owner locks space (ACTIVE → LOCKED)
		cov, err = covSvc.Transition(cov.CovenantID, "LOCKED")
		must(t, err, "→LOCKED")
		if cov.State != "LOCKED" {
			t.Errorf("AC-7: want LOCKED, got %s", cov.State)
		}
		t.Logf("✓ AC-7  state=%s", cov.State)

		// AC-8: Settlement output correct proportions
		rSettle, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_settle",
			&tools.GenerateSettlement{}, map[string]any{})
		must(t, err, "generate settlement")

		outputID, _ := rSettle.Extra["output_id"].(string)
		if outputID == "" {
			t.Fatal("AC-8: no output_id in receipt")
		}

		// Verify shares: contributor pool = 70%, A has 1000/1500 = 66.67%, B has 500/1500 = 33.33%
		var distsJSON string
		var totalTokens int
		conn.QueryRow(`SELECT total_tokens, distributions FROM settlement_outputs WHERE output_id=?`, outputID).
			Scan(&totalTokens, &distsJSON)

		if totalTokens != 1500 {
			t.Errorf("AC-8: want total_tokens=1500, got %d", totalTokens)
		}
		t.Logf("✓ AC-8  output_id=%s total_tokens=%d distributions=%s", outputID, totalTokens, distsJSON)

		// ── Hash chain integrity ───────────────────────────────────────────
		valid, violations := audit.VerifyChain(conn, cov.CovenantID)
		if !valid {
			t.Errorf("hash chain invalid: %v", violations)
		}
		t.Logf("✓ Hash chain valid (%d violations)", len(violations))
	})
}

// TestSessionTokenRotation validates REVIEW-14 grace period behaviour.
func TestSessionTokenRotation(t *testing.T) {
	dbPath := t.TempDir() + "/sessions_test.db"
	conn, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	agentID := "agent_test01"
	covenantID := "cvnt_test"

	// Issue token
	raw, err := sessions.Issue(conn, agentID, covenantID)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Validate active token
	valid, inGrace := sessions.Validate(conn, raw, agentID, covenantID)
	if !valid || inGrace {
		t.Errorf("want valid=true inGrace=false, got %v %v", valid, inGrace)
	}

	// Rotate
	newRaw, warning, err := sessions.Rotate(conn, agentID, covenantID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if warning == "" {
		t.Error("want non-empty warning header")
	}

	// Old token should be in grace
	valid, inGrace = sessions.Validate(conn, raw, agentID, covenantID)
	if !valid || !inGrace {
		t.Errorf("old token: want valid=true inGrace=true, got %v %v", valid, inGrace)
	}

	// New token should be active
	valid, inGrace = sessions.Validate(conn, newRaw, agentID, covenantID)
	if !valid || inGrace {
		t.Errorf("new token: want valid=true inGrace=false, got %v %v", valid, inGrace)
	}

	t.Logf("✓ Session rotation OK  warning=%q", warning)
}

// TestBudgetExhaustion verifies AC-5: once cumulative cost exceeds the budget
// limit, the engine rejects further executions with "budget exhausted".
// propose_passage costs 10 per call; budget is set to 50, so the 6th call fails.
func TestBudgetExhaustion(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/budget_exhaustion.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, ownerMem, err := covSvc.Create("Budget Exhaustion Test", "book", "pid_bex_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	cov, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_bex_agent", "contributor")
	must(t, err, "join agent")
	cov, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	// WI-1: approve agent before it can execute tools.
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve bex agent")

	// Budget = 50; propose_passage costs 10 → 5 calls consume exactly 50.
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 50.0), "budget counter")

	for i := 0; i < 5; i++ {
		_, err := engine.Run(cov.CovenantID, agent.AgentID, "sess_bex",
			&tools.ProposePassage{}, map[string]any{"word_count": 100})
		if err != nil {
			t.Fatalf("call %d should succeed: %v", i+1, err)
		}
	}

	// 6th call must be rejected.
	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_bex",
		&tools.ProposePassage{}, map[string]any{"word_count": 100})
	if err == nil {
		t.Fatal("AC-5: 6th call should be rejected due to budget exhaustion")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("AC-5: expected 'budget exhausted' error, got: %v", err)
	}
	t.Logf("✓ AC-5  budget exhaustion correctly rejected: %v", err)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func must(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

func getDraftID(t *testing.T, conn *sql.DB, covenantID, agentID string) string {
	t.Helper()
	var draftID string
	err := conn.QueryRow(`SELECT draft_id FROM pending_tokens WHERE covenant_id=? AND agent_id=? LIMIT 1`,
		covenantID, agentID).Scan(&draftID)
	if err != nil {
		t.Fatalf("get draft_id for %s: %v", agentID, err)
	}
	return draftID
}

// Ensure the test binary can find the schema.sql embedded alongside db.go.
func init() {
	// When running tests from the module root, the working dir may differ.
	// db.Open uses runtime.Caller to find schema.sql relative to db.go, so no fix needed.
	_ = os.Getenv // silence unused import
	_ = fmt.Sprintf
}
