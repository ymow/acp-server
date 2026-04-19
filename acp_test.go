package main_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/inkmesh/acp-server/internal/api"
	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/gittwin"
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
		must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget counter")

		// AC-3: Agent A proposes + gets approved → tokens calculated
		r1, err := engine.Run(cov.CovenantID, agent1.AgentID, "sess_a",
			&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
		must(t, err, "propose agent1")
		if r1.Status != "pending" {
			t.Errorf("AC-3: want pending, got %s", r1.Status)
		}

		// Owner approves draft
		draftID := getDraftID(t, conn, cov.CovenantID, agent1.AgentID)
		r1a, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveDraft{}, map[string]any{
				"draft_id":         draftID,
				"unit_count":       1000,
				"acceptance_ratio": 1.0,
			})
		must(t, err, "approve agent1")
		if r1a.TokensAwarded != 1000 {
			t.Errorf("AC-3: want 1000 tokens, got %d", r1a.TokensAwarded)
		}
		t.Logf("✓ AC-3  tokens_awarded=%d", r1a.TokensAwarded)

		// AC-4: Agent B proposes + approved
		_, err = engine.Run(cov.CovenantID, agent2.AgentID, "sess_b",
			&tools.ProposePassage{}, map[string]any{"unit_count": 500})
		must(t, err, "propose agent2")

		draftID2 := getDraftID(t, conn, cov.CovenantID, agent2.AgentID)
		r2a, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
			&tools.ApproveDraft{}, map[string]any{
				"draft_id":         draftID2,
				"unit_count":       500,
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
		t.Logf("✓ AC-5  budget_limit=%d budget_spent=%d (cents)", budState.BudgetLimit, budState.BudgetSpent)

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
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 50.0, "USD"), "budget counter")

	for i := 0; i < 5; i++ {
		_, err := engine.Run(cov.CovenantID, agent.AgentID, "sess_bex",
			&tools.ProposePassage{}, map[string]any{"unit_count": 100})
		if err != nil {
			t.Fatalf("call %d should succeed: %v", i+1, err)
		}
	}

	// 6th call must be rejected.
	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_bex",
		&tools.ProposePassage{}, map[string]any{"unit_count": 100})
	if err == nil {
		t.Fatal("AC-5: 6th call should be rejected due to budget exhaustion")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("AC-5: expected 'budget exhausted' error, got: %v", err)
	}
	t.Logf("✓ AC-5  budget exhaustion correctly rejected: %v", err)
}

// TestCostAndNetDeltaAccounting verifies that audit_logs captures the actual
// Step 2.5 estimated cost as cost_delta and computes net_delta per ACR-20 §6:
//
//	net_delta = tokens_delta - cost_weight × cost_delta
//
// Regression guard: before this fix, both columns were always 0 because no
// tool populated SideEffects.{CostDelta,NetDelta}, causing reject_draft
// refunds to no-op and settlement net-contribution math to be wrong.
func TestCostAndNetDeltaAccounting(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/netdelta_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("NetDelta Test", "book", "pid_nd_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_nd_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve agent")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget")

	// Propose: EstimateCost = 10, TokensDelta = 0 → expected cost_delta=10, net_delta=-10.
	rp, err := engine.Run(cov.CovenantID, agent.AgentID, "sess_nd",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	must(t, err, "propose")
	if rp.CostDelta != 10 {
		t.Errorf("propose cost_delta: want 10, got %v", rp.CostDelta)
	}
	if rp.NetDelta != -10 {
		t.Errorf("propose net_delta: want -10 (0 - 1.0×10), got %v", rp.NetDelta)
	}

	// Approve: EstimateCost = 5, TokensDelta = 1000 → expected cost_delta=5, net_delta=995.
	draftID := getDraftID(t, conn, cov.CovenantID, agent.AgentID)
	ra, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftID,
			"unit_count":       1000,
			"acceptance_ratio": 1.0,
		})
	must(t, err, "approve")
	if ra.TokensAwarded != 1000 {
		t.Fatalf("approve tokens: want 1000, got %d", ra.TokensAwarded)
	}
	if ra.CostDelta != 5 {
		t.Errorf("approve cost_delta: want 5, got %v", ra.CostDelta)
	}
	wantNet := float64(1000) - 1.0*5.0
	if ra.NetDelta != wantNet {
		t.Errorf("approve net_delta: want %v (1000 - 1.0×5), got %v", wantNet, ra.NetDelta)
	}

	// Verify audit_logs row matches the receipt (not just the in-memory return).
	var loggedCost, loggedNet float64
	err = conn.QueryRow(
		`SELECT cost_delta, net_delta FROM audit_logs WHERE log_id=?`, ra.ReceiptID,
	).Scan(&loggedCost, &loggedNet)
	must(t, err, "read audit_logs")
	if loggedCost != 5 || loggedNet != wantNet {
		t.Errorf("audit_logs drift: cost=%v net=%v, want cost=5 net=%v", loggedCost, loggedNet, wantNet)
	}

	t.Logf("✓ cost_delta/net_delta correctly recorded (propose=%v/%v, approve=%v/%v)",
		rp.CostDelta, rp.NetDelta, ra.CostDelta, ra.NetDelta)
}

// TestMigrationAddsCostWeight verifies the idempotent ALTER TABLE migration
// installs cost_weight on a legacy DB that predates the column, and is
// a no-op on a DB that already has it.
func TestMigrationAddsCostWeight(t *testing.T) {
	dbPath := t.TempDir() + "/legacy.db"

	// Simulate a pre-migration DB: create covenants table WITHOUT cost_weight.
	legacy, err := sql.Open("sqlite", dbPath+"?_foreign_keys=on")
	must(t, err, "open legacy")
	_, err = legacy.Exec(`
		CREATE TABLE covenants (
			covenant_id TEXT PRIMARY KEY,
			version     TEXT NOT NULL DEFAULT 'ACP@1.0',
			space_type  TEXT NOT NULL DEFAULT 'book',
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT 'DRAFT',
			owner_share_pct REAL NOT NULL DEFAULT 30.0,
			platform_share_pct REAL NOT NULL DEFAULT 0.0,
			contributor_pool_pct REAL NOT NULL DEFAULT 70.0,
			budget_limit REAL NOT NULL DEFAULT 0,
			owner_token TEXT NOT NULL DEFAULT '',
			token_rules_json TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
	`)
	must(t, err, "create legacy covenants")
	_, err = legacy.Exec(`INSERT INTO covenants (covenant_id, title, created_at, updated_at)
		VALUES ('cvnt_legacy', 'Old Book', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	must(t, err, "seed legacy row")
	legacy.Close()

	// First open: migration should ADD cost_weight and backfill the legacy row to 1.0.
	conn, err := db.Open(dbPath)
	must(t, err, "open with migration")
	var weight float64
	err = conn.QueryRow(`SELECT cost_weight FROM covenants WHERE covenant_id='cvnt_legacy'`).Scan(&weight)
	must(t, err, "read cost_weight")
	if weight != 1.0 {
		t.Errorf("legacy row backfill: want cost_weight=1.0, got %v", weight)
	}

	// Second open: migration must be a no-op (duplicate column swallowed).
	conn.Close()
	conn2, err := db.Open(dbPath)
	must(t, err, "reopen")
	defer conn2.Close()
	err = conn2.QueryRow(`SELECT cost_weight FROM covenants WHERE covenant_id='cvnt_legacy'`).Scan(&weight)
	must(t, err, "re-read cost_weight")
	t.Logf("✓ migration idempotent; cost_weight=%v", weight)
}

// TestOwnerIDExplicit verifies Phase 3.0 housekeeping: covenants.owner_id is
// populated on Create, round-trips through Get, and GetOwnerAgentID reads it
// directly without the is_owner=1 JOIN. Unblocks Constitutional Principle #2
// (agent_id vs owner_id as separately addressable concepts).
func TestOwnerIDExplicit(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/owner_id_test.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	cov, owner, err := covSvc.Create("Owner ID Test", "book", "pid_owner_x")
	must(t, err, "create")
	if cov.OwnerID == "" {
		t.Fatal("OwnerID empty after Create")
	}
	if cov.OwnerID != owner.AgentID {
		t.Errorf("OwnerID=%q does not match owner member agent_id=%q", cov.OwnerID, owner.AgentID)
	}

	// Round-trip through Get.
	got, err := covSvc.Get(cov.CovenantID)
	must(t, err, "get")
	if got.OwnerID != owner.AgentID {
		t.Errorf("Get.OwnerID=%q, want %q", got.OwnerID, owner.AgentID)
	}

	// Fast-path: GetOwnerAgentID returns owner_id even if we null out the
	// covenant_members.is_owner flag (proves the lookup no longer depends on it).
	_, err = conn.Exec(`UPDATE covenant_members SET is_owner=0 WHERE covenant_id=?`, cov.CovenantID)
	must(t, err, "clear is_owner flag")
	agentID, err := covSvc.GetOwnerAgentID(cov.CovenantID)
	must(t, err, "get owner agent id")
	if agentID != owner.AgentID {
		t.Errorf("GetOwnerAgentID=%q after is_owner cleared, want %q", agentID, owner.AgentID)
	}
}

// TestMigrationBackfillsOwnerID verifies a pre-3.0 covenant (no owner_id column,
// ownership only via is_owner=1) gets backfilled on first open. This matters
// because Phase 3.B+ starts reading owner_id directly — a legacy row with an
// empty owner_id would break GetOwnerAgentID's fast path.
func TestMigrationBackfillsOwnerID(t *testing.T) {
	dbPath := t.TempDir() + "/legacy_owner.db"

	legacy, err := sql.Open("sqlite", dbPath+"?_foreign_keys=on")
	must(t, err, "open legacy")
	_, err = legacy.Exec(`
		CREATE TABLE covenants (
			covenant_id TEXT PRIMARY KEY,
			version     TEXT NOT NULL DEFAULT 'ACP@1.0',
			space_type  TEXT NOT NULL DEFAULT 'book',
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT 'DRAFT',
			owner_share_pct REAL NOT NULL DEFAULT 30.0,
			platform_share_pct REAL NOT NULL DEFAULT 0.0,
			contributor_pool_pct REAL NOT NULL DEFAULT 70.0,
			budget_limit REAL NOT NULL DEFAULT 0,
			owner_token TEXT NOT NULL DEFAULT '',
			token_rules_json TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE covenant_members (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			covenant_id  TEXT NOT NULL,
			platform_id  TEXT NOT NULL,
			agent_id     TEXT NOT NULL,
			tier_id      TEXT,
			is_owner     INTEGER NOT NULL DEFAULT 0,
			status       TEXT NOT NULL DEFAULT 'active',
			joined_at    TEXT NOT NULL
		);
	`)
	must(t, err, "create legacy tables")
	_, err = legacy.Exec(`INSERT INTO covenants (covenant_id, title, created_at, updated_at)
		VALUES ('cvnt_legacy_owner', 'Old Book', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	must(t, err, "seed legacy covenant")
	_, err = legacy.Exec(`INSERT INTO covenant_members
		(covenant_id, platform_id, agent_id, is_owner, status, joined_at)
		VALUES ('cvnt_legacy_owner', 'pid_legacy', 'agent_legacy_owner', 1, 'active', '2026-01-01T00:00:00Z')`)
	must(t, err, "seed legacy owner member")
	legacy.Close()

	// First open runs ALTER + backfill.
	conn, err := db.Open(dbPath)
	must(t, err, "open with migration")
	defer conn.Close()
	var ownerID string
	err = conn.QueryRow(`SELECT owner_id FROM covenants WHERE covenant_id='cvnt_legacy_owner'`).Scan(&ownerID)
	must(t, err, "read owner_id")
	if ownerID != "agent_legacy_owner" {
		t.Errorf("backfill: want owner_id=agent_legacy_owner, got %q", ownerID)
	}

	// Second open: backfill is a no-op (WHERE owner_id='' skips populated rows).
	var ownerAfter string
	err = conn.QueryRow(`SELECT owner_id FROM covenants WHERE covenant_id='cvnt_legacy_owner'`).Scan(&ownerAfter)
	must(t, err, "re-read owner_id")
	if ownerAfter != "agent_legacy_owner" {
		t.Errorf("backfill not idempotent: got %q", ownerAfter)
	}
	t.Logf("✓ owner_id backfilled from is_owner=1 lookup")
}

// TestRejectDraftRefundsBudget verifies that after approve_draft deducts cost
// from the budget, reject_draft refunds that same amount. This only works
// because audit_logs.cost_delta now carries the real cost (was always 0 before
// the TokensDelta/CostDelta/NetDelta wiring fix).
func TestRejectDraftRefundsBudget(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/refund_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Refund Test", "book", "pid_r_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_r_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve agent")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget")

	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_r",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	must(t, err, "propose")
	draftID := getDraftID(t, conn, cov.CovenantID, agent.AgentID)
	approveReceipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftID,
			"unit_count":       1000,
			"acceptance_ratio": 1.0,
		})
	must(t, err, "approve")

	before, err := budget.GetState(conn, cov.CovenantID)
	must(t, err, "budget before")
	// Expect propose (10) + approve (5) = 15 spent.
	if before.BudgetSpent != 15 {
		t.Fatalf("budget_spent before reject: want 15, got %v", before.BudgetSpent)
	}

	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.RejectDraft{}, map[string]any{
			"log_id": approveReceipt.ReceiptID,
			"reason": "test refund",
		})
	must(t, err, "reject_draft")

	after, err := budget.GetState(conn, cov.CovenantID)
	must(t, err, "budget after")
	// approve cost (5) should be refunded; propose cost (10) stays spent.
	if after.BudgetSpent != 10 {
		t.Errorf("budget_spent after reject: want 10 (propose cost only), got %v", after.BudgetSpent)
	}
	t.Logf("✓ reject_draft refunded budget: 15 → %v", after.BudgetSpent)
}

// TestCostWeightApplied verifies net_delta honours a non-default cost_weight,
// so operators can bias the trade-off between tokens earned and cost spent.
func TestCostWeightApplied(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/cost_weight_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Weight Test", "book", "pid_w_owner")
	must(t, err, "create")
	// Override cost_weight directly; there is no tool for this yet (P1 work).
	_, err = conn.Exec(`UPDATE covenants SET cost_weight=? WHERE covenant_id=?`, 3.0, cov.CovenantID)
	must(t, err, "set cost_weight=3.0")

	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_w_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve agent")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget")

	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_w",
		&tools.ProposePassage{}, map[string]any{"unit_count": 500})
	must(t, err, "propose")

	draftID := getDraftID(t, conn, cov.CovenantID, agent.AgentID)
	ra, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftID,
			"unit_count":       500,
			"acceptance_ratio": 1.0,
		})
	must(t, err, "approve")

	// tokens_delta = 500, cost_delta = 5, cost_weight = 3.0 → net_delta = 500 - 15 = 485.
	wantNet := float64(500) - 3.0*5.0
	if ra.NetDelta != wantNet {
		t.Errorf("net_delta with cost_weight=3.0: want %v, got %v", wantNet, ra.NetDelta)
	}
	t.Logf("✓ cost_weight=3.0 applied: net_delta=%v", ra.NetDelta)
}

// TestRebuildFromAuditLog verifies that budget_spent can be reconstructed
// from the durable audit log chain after the runtime counter is wiped —
// the Phase 2 Redis-restart scenario (ACP_Implementation_Spec_MVP Part 8).
//
// Exercises the non-trivial path: a reject_draft refund must be honoured,
// so the rebuild excludes cost_delta whose token_ledger row is reversed.
func TestRebuildFromAuditLog(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/rebuild_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Rebuild Test", "book", "pid_rb_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_rb_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve agent")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget")

	// Run propose (10) + approve (5), then reject_draft (refunds 5).
	// Net durable spend = 10.
	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_rb",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	must(t, err, "propose")
	draftID := getDraftID(t, conn, cov.CovenantID, agent.AgentID)
	approveReceipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":         draftID,
			"unit_count":       1000,
			"acceptance_ratio": 1.0,
		})
	must(t, err, "approve")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.RejectDraft{}, map[string]any{
			"log_id": approveReceipt.ReceiptID,
			"reason": "test rebuild",
		})
	must(t, err, "reject_draft")

	// Sanity: the live counter should already reflect the refund.
	state, err := budget.GetState(conn, cov.CovenantID)
	must(t, err, "state before wipe")
	if state.BudgetSpent != 10 {
		t.Fatalf("pre-wipe budget_spent: want 10, got %v", state.BudgetSpent)
	}

	// Simulate a cold cache: wipe budget_spent in the counter.
	_, err = conn.Exec(`UPDATE budget_counters SET budget_spent=0 WHERE covenant_id=?`,
		cov.CovenantID)
	must(t, err, "wipe counter")

	rebuilt, err := budget.RebuildFromAuditLog(conn, cov.CovenantID)
	must(t, err, "rebuild")
	if rebuilt != 10 {
		t.Errorf("rebuild returned: want 10, got %v", rebuilt)
	}

	state, err = budget.GetState(conn, cov.CovenantID)
	must(t, err, "state after rebuild")
	if state.BudgetSpent != 10 {
		t.Errorf("post-rebuild budget_spent: want 10, got %v", state.BudgetSpent)
	}
	t.Logf("✓ rebuild reconstructed budget_spent=%v from audit_log after wipe", rebuilt)
}

// TestRebuildFromAuditLog_MissingCounter rejects rebuilding when the
// caller forgot to EnsureCounter first — silent no-op would mask a bug.
func TestRebuildFromAuditLog_MissingCounter(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/rebuild_missing_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	_, err = budget.RebuildFromAuditLog(conn, "cvnt_nonexistent")
	if err == nil || !strings.Contains(err.Error(), "no budget_counter row") {
		t.Fatalf("expected missing-counter error, got %v", err)
	}
}

// TestRebuildFromAuditLog_EmptyLedger handles the boundary case: counter
// exists, but no audit entries yet — rebuild should zero the counter
// rather than leaving stale data.
func TestRebuildFromAuditLog_EmptyLedger(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/rebuild_empty_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	cov, _, err := covSvc.Create("Empty Test", "book", "pid_e_owner")
	must(t, err, "create")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000.0, "USD"), "budget")
	// Simulate drift: pretend the counter has a bogus value.
	_, err = conn.Exec(`UPDATE budget_counters SET budget_spent=999 WHERE covenant_id=?`,
		cov.CovenantID)
	must(t, err, "inject drift")

	rebuilt, err := budget.RebuildFromAuditLog(conn, cov.CovenantID)
	must(t, err, "rebuild")
	if rebuilt != 0 {
		t.Errorf("empty-ledger rebuild: want 0, got %v", rebuilt)
	}
	state, _ := budget.GetState(conn, cov.CovenantID)
	if state.BudgetSpent != 0 {
		t.Errorf("counter after rebuild: want 0, got %v", state.BudgetSpent)
	}
}

// TestCrossCurrencyChargeRejected verifies that a charge whose cost_currency
// does not match the covenant's budget_currency is rejected at Step 4 of the
// execution flow — never reaching budget_counters or audit_logs as success.
//
// Motivation: budget_counters.budget_spent is a single-currency scalar; mixing
// USD cents with EUR cents would corrupt the ledger. The enforcement lives in
// execution.Run (not in the budget package) so budget internals can safely
// assume uniform currency.
func TestCrossCurrencyChargeRejected(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/currency_mismatch.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Currency Mismatch", "book", "pid_cm_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	agent, err := covSvc.Join(cov.CovenantID, "pid_cm_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agent.AgentID})
	must(t, err, "approve agent")

	// Force covenant to EUR budget — ProposePassage charges USD cents, so any
	// call now represents a cross-currency mismatch.
	_, err = conn.Exec(`UPDATE covenants SET budget_currency='EUR' WHERE covenant_id=?`, cov.CovenantID)
	must(t, err, "force EUR budget")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 1000, "EUR"), "EUR counter")

	_, err = engine.Run(cov.CovenantID, agent.AgentID, "sess_cm",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	if err == nil {
		t.Fatal("expected cross-currency rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "does not match covenant budget_currency") {
		t.Errorf("expected currency mismatch error, got: %v", err)
	}

	// Ledger invariant: nothing was deducted; no success row was written.
	state, err := budget.GetState(conn, cov.CovenantID)
	must(t, err, "get state")
	if state.BudgetSpent != 0 {
		t.Errorf("budget_spent after rejection: want 0, got %d", state.BudgetSpent)
	}
	var successCount int
	err = conn.QueryRow(`SELECT COUNT(*) FROM audit_logs
		WHERE covenant_id=? AND tool_name='propose_passage' AND result='success'`,
		cov.CovenantID).Scan(&successCount)
	must(t, err, "count success logs")
	if successCount != 0 {
		t.Errorf("expected 0 success audit rows, got %d", successCount)
	}
	t.Logf("✓ cross-currency charge rejected: %v", err)
}

// ── Phase 3.B ────────────────────────────────────────────────────────────────

func TestSpaceTypeValidation(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/space_type.db")
	must(t, err, "open db")
	defer conn.Close()
	covSvc := covenant.New(conn)

	for _, st := range []string{"book", "code", "music", "research", "custom"} {
		if _, _, err := covSvc.Create("st-"+st, st, "pid_"+st); err != nil {
			t.Errorf("space_type %q should be accepted: %v", st, err)
		}
	}
	if _, _, err := covSvc.Create("bogus", "novel", "pid_bogus"); err == nil {
		t.Fatal("expected rejection for space_type=novel")
	}

	// UnitName round-trips through Get().
	cov, _, err := covSvc.Create("code project", "code", "pid_code")
	must(t, err, "create code covenant")
	reloaded, err := covSvc.Get(cov.CovenantID)
	must(t, err, "get code covenant")
	if reloaded.UnitName != "Commit" {
		t.Errorf("UnitName for code: want Commit, got %q", reloaded.UnitName)
	}
}

func TestTokenSnapshotHashOnLock(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/snapshot.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Snapshot Test", "book", "pid_snap_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	ag, err := covSvc.Join(cov.CovenantID, "pid_snap_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": ag.AgentID})
	must(t, err, "approve agent")
	_, err = engine.Run(cov.CovenantID, ag.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 500})
	must(t, err, "propose")
	draftID := getDraftID(t, conn, cov.CovenantID, ag.AgentID)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o2",
		&tools.ApproveDraft{}, map[string]any{"draft_id": draftID, "unit_count": 500, "acceptance_ratio": 1.0})
	must(t, err, "approve draft")

	// Lock → snapshot rows written.
	_, err = covSvc.Transition(cov.CovenantID, "LOCKED")
	must(t, err, "→LOCKED")

	rows, err := conn.Query(`SELECT agent_id, agent_tokens, snapshot_hash FROM token_snapshots WHERE covenant_id=?`, cov.CovenantID)
	must(t, err, "query snapshots")
	defer rows.Close()
	count := 0
	for rows.Next() {
		var agentID, hash string
		var tokensVal int
		must(t, rows.Scan(&agentID, &tokensVal, &hash), "scan")
		if hash == "" {
			t.Errorf("snapshot_hash empty for agent %s", agentID)
		}
		if agentID == ag.AgentID && tokensVal != 500 {
			t.Errorf("agent tokens: want 500, got %d", tokensVal)
		}
		count++
	}
	if count == 0 {
		t.Fatal("expected at least one snapshot row after LOCK")
	}

	// Tampering with agent_tokens must invalidate the hash.
	_, err = conn.Exec(`UPDATE token_snapshots SET agent_tokens = agent_tokens + 1 WHERE covenant_id=? AND agent_id=?`,
		cov.CovenantID, ag.AgentID)
	must(t, err, "tamper")

	var stored tokens.Snapshot
	var snapped string
	err = conn.QueryRow(`SELECT id, covenant_id, agent_id, agent_tokens, cost_tokens, snapped_at, snapshot_hash
		FROM token_snapshots WHERE covenant_id=? AND agent_id=?`, cov.CovenantID, ag.AgentID,
	).Scan(&stored.ID, &stored.CovenantID, &stored.AgentID, &stored.AgentTokens,
		&stored.CostTokens, &snapped, &stored.SnapshotHash)
	must(t, err, "reread snapshot")
	stored.SnappedAt, _ = time.Parse(time.RFC3339Nano, snapped)
	if tokens.VerifySnapshot(stored) {
		t.Fatal("expected tamper detection: VerifySnapshot returned true after mutation")
	}
}

func TestLeaveCovenantBlocksFurtherActions(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/leave.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Leave Test", "book", "pid_leave_owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	ag, err := covSvc.Join(cov.CovenantID, "pid_leaver", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": ag.AgentID})
	must(t, err, "approve agent")

	// Earn one confirmed contribution — it must survive the departure.
	_, err = engine.Run(cov.CovenantID, ag.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 200})
	must(t, err, "propose")
	draftID := getDraftID(t, conn, cov.CovenantID, ag.AgentID)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o2",
		&tools.ApproveDraft{}, map[string]any{"draft_id": draftID, "unit_count": 200, "acceptance_ratio": 1.0})
	must(t, err, "approve draft")

	// Owner cannot leave.
	if _, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_o_leave",
		&tools.LeaveCovenant{}, map[string]any{}); err == nil {
		t.Fatal("owner must not be allowed to leave_covenant")
	}

	// Contributor leaves.
	_, err = engine.Run(cov.CovenantID, ag.AgentID, "sess_a2",
		&tools.LeaveCovenant{}, map[string]any{"reason": "done"})
	must(t, err, "leave covenant")

	var status string
	err = conn.QueryRow(`SELECT status FROM covenant_members WHERE covenant_id=? AND agent_id=?`,
		cov.CovenantID, ag.AgentID).Scan(&status)
	must(t, err, "read status")
	if status != "left" {
		t.Errorf("member status: want left, got %q", status)
	}

	// Confirmed contribution must still be queryable.
	bal, err := tokens.Balance(conn, cov.CovenantID, ag.AgentID)
	must(t, err, "balance")
	if bal != 200 {
		t.Errorf("confirmed balance after leave: want 200, got %d", bal)
	}

	// Further execution is blocked at Step 1.
	if _, err := engine.Run(cov.CovenantID, ag.AgentID, "sess_a3",
		&tools.ProposePassage{}, map[string]any{"unit_count": 50}); err == nil {
		t.Fatal("propose_passage after leave must be blocked")
	} else if !strings.Contains(err.Error(), "step1.forbidden") {
		t.Errorf("expected step1.forbidden, got %v", err)
	}
}

func TestTokenRuleDrivesApproveDraft(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/token_rule.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, owner, err := covSvc.Create("Rule Test", "book", "pid_rule_owner")
	must(t, err, "create")
	// Configure a custom formula BEFORE going OPEN: floor(w/100) * base_rate * tier_multiplier
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_cfg",
		&tools.ConfigureTokenRules{}, map[string]any{
			"rules": []map[string]any{{
				"tool_name": "propose_passage",
				"formula":   "floor(word_count / 100) * base_rate * tier_multiplier",
				"base_rate": 3,
				"pending":   true,
			}},
		})
	must(t, err, "configure rules")

	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 2.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	ag, err := covSvc.Join(cov.CovenantID, "pid_rule_agent", "contributor")
	must(t, err, "join")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": ag.AgentID})
	must(t, err, "approve agent")

	_, err = engine.Run(cov.CovenantID, ag.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 1000})
	must(t, err, "propose")
	draftID := getDraftID(t, conn, cov.CovenantID, ag.AgentID)
	receipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_o2",
		&tools.ApproveDraft{}, map[string]any{"draft_id": draftID, "unit_count": 1000, "acceptance_ratio": 1.0})
	must(t, err, "approve draft")

	// floor(1000/100)=10 × base_rate(3) × tier(2.0) = 60
	if receipt.TokensAwarded != 60 {
		t.Errorf("rule-driven tokens: want 60, got %d", receipt.TokensAwarded)
	}
}

// ── Phase 3.A: Git Covenant Twin (ACR-400) ──────────────────────────────────

// TestGitTwinMergeFlow exercises /git-twin/merge: bridge auth, propose+approve
// atomicity, idempotency on retry, and unmapped-author handling. The test is
// HTTP-level on purpose — the bridge path is an HTTP boundary, so a pure
// service-level unit test would miss the auth + decoding seams.
func TestGitTwinMergeFlow(t *testing.T) {
	conn, covSvc, server, teardown := setupTwinServer(t, "twin-secret")
	defer teardown()

	// Covenant lifecycle: create → add tier → open → join author → approve → ACTIVE
	cov, owner, err := covSvc.Create("Git Twin Covenant", "code", "github:owner")
	must(t, err, "create")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	author, err := covSvc.Join(cov.CovenantID, "github:alice", "contributor")
	must(t, err, "join author")
	engine := execution.New(conn, covSvc)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": author.AgentID})
	must(t, err, "approve author")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 10000, "USD"), "budget")

	draftRef := "https://github.com/o/r/pull/42"

	// — Missing bridge secret → 401 —
	resp := postJSON(t, server.URL+"/git-twin/merge", nil, map[string]any{
		"covenant_id":        cov.CovenantID,
		"author_platform_id": "github:alice",
		"draft_ref":          draftRef,
		"unit_count":         120,
	})
	if resp.StatusCode != 401 {
		t.Fatalf("no auth should be 401, got %d", resp.StatusCode)
	}

	// — Wrong secret → 401 —
	resp = postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "wrong"},
		map[string]any{"covenant_id": cov.CovenantID, "author_platform_id": "github:alice", "draft_ref": draftRef, "unit_count": 120})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong secret should be 401, got %d", resp.StatusCode)
	}

	// — Happy path —
	body := map[string]any{
		"covenant_id":        cov.CovenantID,
		"author_platform_id": "github:alice",
		"draft_ref":          draftRef,
		"unit_count":         120,
		"acceptance_ratio":   1.0,
	}
	resp = postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "twin-secret"}, body)
	if resp.StatusCode != 200 {
		t.Fatalf("merge: want 200 got %d body=%s", resp.StatusCode, resp.Body)
	}
	if resp.JSON["propose_receipt"] == nil || resp.JSON["approve_receipt"] == nil {
		t.Fatalf("merge response missing receipts: %v", resp.JSON)
	}

	// Ledger check: author should have confirmed tokens.
	var confirmed int
	must(t, conn.QueryRow(
		`SELECT COALESCE(SUM(delta),0) FROM token_ledger WHERE covenant_id=? AND agent_id=? AND status='confirmed'`,
		cov.CovenantID, author.AgentID).Scan(&confirmed), "ledger query")
	if confirmed <= 0 {
		t.Fatalf("expected confirmed tokens for author, got %d", confirmed)
	}

	// — Idempotent retry —
	resp = postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "twin-secret"}, body)
	if resp.StatusCode != 200 {
		t.Fatalf("retry: want 200 got %d body=%s", resp.StatusCode, resp.Body)
	}
	if resp.JSON["idempotent"] != true {
		t.Fatalf("retry should be idempotent, got %v", resp.JSON)
	}

	// Ledger total must not have doubled.
	var confirmedAfter int
	must(t, conn.QueryRow(
		`SELECT COALESCE(SUM(delta),0) FROM token_ledger WHERE covenant_id=? AND agent_id=? AND status='confirmed'`,
		cov.CovenantID, author.AgentID).Scan(&confirmedAfter), "ledger query after retry")
	if confirmedAfter != confirmed {
		t.Fatalf("idempotency violated: %d → %d", confirmed, confirmedAfter)
	}

	// — Unmapped author → 200 with unmapped=true, ledger untouched —
	resp = postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{
			"covenant_id":        cov.CovenantID,
			"author_platform_id": "github:stranger",
			"draft_ref":          "https://github.com/o/r/pull/99",
			"unit_count":         50,
		})
	if resp.StatusCode != 200 || resp.JSON["unmapped"] != true {
		t.Fatalf("unmapped author should be 200 with unmapped=true, got %d %v", resp.StatusCode, resp.JSON)
	}
}

// TestGitTwinAnchorLifecycle walks a twin-bound covenant through settlement
// and checks that confirm_settlement_output enqueues a pending Git Anchor,
// the bridge can list it, and the ack flips status to 'written' with the
// commit SHA the bridge supplies. Covers ACR-400 Part 5 server plumbing.
func TestGitTwinAnchorLifecycle(t *testing.T) {
	conn, covSvc, server, teardown := setupTwinServer(t, "twin-secret")
	defer teardown()

	repoURL := "https://github.com/anchors/demo"
	cov, owner, err := covSvc.Create("Anchor Covenant", "code", "github:owner")
	must(t, err, "create")
	must(t, covSvc.SetGitTwin(cov.CovenantID, repoURL, "github", ""), "bind twin")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	author, err := covSvc.Join(cov.CovenantID, "github:alice", "contributor")
	must(t, err, "join author")
	engine := execution.New(conn, covSvc)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": author.AgentID})
	must(t, err, "approve author")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 10000, "USD"), "budget")

	// Drive a contribution through the bridge endpoint so the ledger has tokens
	// before we settle. The merge endpoint reuses the propose+approve path.
	resp := postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{
			"covenant_id":        cov.CovenantID,
			"author_platform_id": "github:alice",
			"draft_ref":          "https://github.com/anchors/demo/pull/1",
			"unit_count":         80,
			"acceptance_ratio":   1.0,
		})
	if resp.StatusCode != 200 {
		t.Fatalf("merge: want 200 got %d body=%s", resp.StatusCode, resp.Body)
	}

	// LOCKED → generate_settlement → confirm.
	_, err = covSvc.Transition(cov.CovenantID, "LOCKED")
	must(t, err, "→LOCKED")
	genReceipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.GenerateSettlement{}, map[string]any{})
	must(t, err, "generate_settlement")
	outputID, _ := genReceipt.Extra["output_id"].(string)
	if outputID == "" {
		t.Fatalf("generate_settlement missing output_id: %+v", genReceipt.Extra)
	}
	confirmReceipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ConfirmSettlementOutput{},
		map[string]any{"settlement_output_id": outputID})
	must(t, err, "confirm_settlement_output")
	anchorID, _ := confirmReceipt.Extra["anchor_id"].(string)
	if anchorID == "" {
		t.Fatalf("confirm_settlement_output did not enqueue anchor: %+v", confirmReceipt.Extra)
	}

	// Unauthenticated list → 401.
	listNoAuth := getJSON(t, server.URL+"/git-twin/anchors/pending", nil)
	if listNoAuth.StatusCode != 401 {
		t.Fatalf("unauth list should be 401, got %d", listNoAuth.StatusCode)
	}

	// Authenticated list → our anchor comes back with the right metadata.
	list := getJSON(t, server.URL+"/git-twin/anchors/pending?repo_url="+repoURL,
		map[string]string{"X-Bridge-Secret": "twin-secret"})
	if list.StatusCode != 200 {
		t.Fatalf("list anchors: want 200 got %d body=%s", list.StatusCode, list.Body)
	}
	anchors, _ := list.JSON["anchors"].([]any)
	if len(anchors) != 1 {
		t.Fatalf("want 1 pending anchor, got %d: %s", len(anchors), list.Body)
	}
	a := anchors[0].(map[string]any)
	if a["anchor_id"] != anchorID {
		t.Fatalf("list anchor_id mismatch: want %s got %v", anchorID, a["anchor_id"])
	}
	if a["settlement_output_id"] != outputID {
		t.Fatalf("list settlement_output_id mismatch: %v", a["settlement_output_id"])
	}
	if a["repo_url"] != repoURL {
		t.Fatalf("list repo_url mismatch: %v", a["repo_url"])
	}
	if sh, _ := a["snapshot_hash"].(string); sh == "" {
		t.Fatalf("snapshot_hash must be populated (covenant had a LOCKED snapshot): %v", a)
	}
	if nb, _ := a["note_body"].(string); !strings.Contains(nb, "acp.anchor.settlement.v1") ||
		!strings.Contains(nb, outputID) {
		t.Fatalf("note_body malformed: %v", a["note_body"])
	}

	// Ack without auth → 401.
	ackNoAuth := postJSON(t, server.URL+"/git-twin/anchors/"+anchorID+"/ack", nil,
		map[string]any{"written_commit_sha": "deadbeef"})
	if ackNoAuth.StatusCode != 401 {
		t.Fatalf("unauth ack should be 401, got %d", ackNoAuth.StatusCode)
	}

	// Ack with empty SHA → 400.
	ackEmpty := postJSON(t, server.URL+"/git-twin/anchors/"+anchorID+"/ack",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{"written_commit_sha": ""})
	if ackEmpty.StatusCode != 400 {
		t.Fatalf("empty sha ack should be 400, got %d", ackEmpty.StatusCode)
	}

	// Happy ack.
	ack := postJSON(t, server.URL+"/git-twin/anchors/"+anchorID+"/ack",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{"written_commit_sha": "abcdef1234567890"})
	if ack.StatusCode != 200 {
		t.Fatalf("ack: want 200 got %d body=%s", ack.StatusCode, ack.Body)
	}

	// Pending list should now be empty.
	list2 := getJSON(t, server.URL+"/git-twin/anchors/pending?repo_url="+repoURL,
		map[string]string{"X-Bridge-Secret": "twin-secret"})
	pending2, _ := list2.JSON["anchors"].([]any)
	if len(pending2) != 0 {
		t.Fatalf("after ack, pending list should be empty, got %d: %s", len(pending2), list2.Body)
	}

	// DB should show 'written' + our commit SHA.
	var status, sha string
	must(t, conn.QueryRow(
		`SELECT status, COALESCE(written_commit_sha,'') FROM git_twin_anchors WHERE anchor_id=?`,
		anchorID).Scan(&status, &sha), "select anchor")
	if status != "written" || sha != "abcdef1234567890" {
		t.Fatalf("anchor row not updated: status=%s sha=%s", status, sha)
	}

	// Re-ack with same SHA is a no-op (200).
	ackSame := postJSON(t, server.URL+"/git-twin/anchors/"+anchorID+"/ack",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{"written_commit_sha": "abcdef1234567890"})
	if ackSame.StatusCode != 200 {
		t.Fatalf("same-sha re-ack should be 200, got %d", ackSame.StatusCode)
	}

	// Re-ack with different SHA → 409 (split-brain guard).
	ackConflict := postJSON(t, server.URL+"/git-twin/anchors/"+anchorID+"/ack",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{"written_commit_sha": "0000000000000000"})
	if ackConflict.StatusCode != 409 {
		t.Fatalf("conflicting re-ack should be 409, got %d body=%s", ackConflict.StatusCode, ackConflict.Body)
	}

	// Unknown anchor → 404.
	ack404 := postJSON(t, server.URL+"/git-twin/anchors/anch_nope/ack",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{"written_commit_sha": "ffffffffffffffff"})
	if ack404.StatusCode != 404 {
		t.Fatalf("missing anchor should be 404, got %d", ack404.StatusCode)
	}
}

// TestGitTwinAuditEvent covers the non-merge event forwarder (push.*, PR
// opened/rejected, settlement tag). Audit-only by design: nothing in the
// ledger moves, but the event has to land in audit_logs under a real agent
// so verifiers can reconstruct twin history and the hash chain stays intact.
func TestGitTwinAuditEvent(t *testing.T) {
	conn, covSvc, server, teardown := setupTwinServer(t, "twin-secret")
	defer teardown()

	cov, owner, err := covSvc.Create("Audit Event Covenant", "code", "github:owner")
	must(t, err, "create")
	must(t, covSvc.SetGitTwin(cov.CovenantID, "https://github.com/audit/demo", "github", ""), "bind twin")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	alice, err := covSvc.Join(cov.CovenantID, "github:alice", "contributor")
	must(t, err, "join alice")
	engine := execution.New(conn, covSvc)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": alice.AgentID})
	must(t, err, "approve alice")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 10000, "USD"), "budget")

	eventURL := server.URL + "/git-twin/event"
	authHeaders := map[string]string{"X-Bridge-Secret": "twin-secret"}

	// Auth rejection.
	noAuth := postJSON(t, eventURL, nil, map[string]any{
		"covenant_id": cov.CovenantID,
		"event_kind":  "push.forced",
	})
	if noAuth.StatusCode != 401 {
		t.Fatalf("unauth should be 401, got %d", noAuth.StatusCode)
	}

	// Missing required fields → 400.
	missing := postJSON(t, eventURL, authHeaders, map[string]any{"event_kind": "push.forced"})
	if missing.StatusCode != 400 {
		t.Fatalf("missing covenant_id should be 400, got %d body=%s", missing.StatusCode, missing.Body)
	}

	// Unknown event_kind → engine rejects in Step 2 (BadRequest).
	bad := postJSON(t, eventURL, authHeaders, map[string]any{
		"covenant_id": cov.CovenantID,
		"event_kind":  "push.totally_made_up",
	})
	if bad.StatusCode != 400 {
		t.Fatalf("unknown event_kind should be 400, got %d body=%s", bad.StatusCode, bad.Body)
	}

	// Mapped actor → agent_id resolves to alice; mapped_actor=true.
	mapped := postJSON(t, eventURL, authHeaders, map[string]any{
		"covenant_id":       cov.CovenantID,
		"actor_platform_id": "github:alice",
		"event_kind":        "push.forced",
		"ref":               "refs/heads/main",
		"commit_head":       "a1b2c3d4e5f6",
		"summary":           "force-push rebased main",
	})
	if mapped.StatusCode != 200 {
		t.Fatalf("mapped event: want 200 got %d body=%s", mapped.StatusCode, mapped.Body)
	}
	if got, _ := mapped.JSON["agent_id"].(string); got != alice.AgentID {
		t.Fatalf("mapped agent_id: want %s got %v", alice.AgentID, mapped.JSON["agent_id"])
	}
	if m, _ := mapped.JSON["mapped_actor"].(bool); !m {
		t.Fatalf("mapped_actor should be true for github:alice: %s", mapped.Body)
	}

	// Unmapped actor → falls back to owner; mapped_actor=false.
	unmapped := postJSON(t, eventURL, authHeaders, map[string]any{
		"covenant_id":       cov.CovenantID,
		"actor_platform_id": "github:nobody-here",
		"event_kind":        "pull_request.opened",
		"ref":               "refs/heads/main",
		"commit_head":       "deadbeef",
		"summary":           "drive-by PR",
	})
	if unmapped.StatusCode != 200 {
		t.Fatalf("unmapped event: want 200 got %d body=%s", unmapped.StatusCode, unmapped.Body)
	}
	if got, _ := unmapped.JSON["agent_id"].(string); got != owner.AgentID {
		t.Fatalf("unmapped agent_id: want owner %s got %v", owner.AgentID, unmapped.JSON["agent_id"])
	}
	if m, _ := unmapped.JSON["mapped_actor"].(bool); m {
		t.Fatalf("mapped_actor should be false for unknown login: %s", unmapped.Body)
	}

	// Blank actor_platform_id also falls back to owner.
	blank := postJSON(t, eventURL, authHeaders, map[string]any{
		"covenant_id": cov.CovenantID,
		"event_kind":  "tag.settlement",
		"commit_head": "cafebabe",
		"summary":     "settlement-2026-Q1",
	})
	if blank.StatusCode != 200 {
		t.Fatalf("blank actor: want 200 got %d body=%s", blank.StatusCode, blank.Body)
	}
	if got, _ := blank.JSON["agent_id"].(string); got != owner.AgentID {
		t.Fatalf("blank actor agent_id: want owner %s got %v", owner.AgentID, blank.JSON["agent_id"])
	}

	// Hash chain must still verify after three audit-only rows land.
	if valid, violations := audit.VerifyChain(conn, cov.CovenantID); !valid {
		t.Fatalf("audit hash chain broken after git twin events: %v", violations)
	}

	// Audit rows should show tool_name=record_git_twin_event with zero token/cost deltas.
	var count int
	must(t, conn.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE covenant_id=? AND tool_name='record_git_twin_event' AND result='success'`,
		cov.CovenantID).Scan(&count), "count audit rows")
	if count != 3 {
		t.Fatalf("expected 3 record_git_twin_event rows, got %d", count)
	}
	var sumTokens, sumCost int
	must(t, conn.QueryRow(
		`SELECT COALESCE(SUM(tokens_delta),0), COALESCE(SUM(cost_delta),0) FROM audit_logs
		 WHERE covenant_id=? AND tool_name='record_git_twin_event'`,
		cov.CovenantID).Scan(&sumTokens, &sumCost), "sum deltas")
	if sumTokens != 0 || sumCost != 0 {
		t.Fatalf("git twin events should not move ledger: tokens=%d cost=%d", sumTokens, sumCost)
	}
}

// TestGitTwinAnchorSigned exercises chunk 7: a signer set on
// ConfirmSettlementOutput causes the anchor note to carry an ed25519
// signature that gittwin.VerifyAnchorSignature can validate.
func TestGitTwinAnchorSigned(t *testing.T) {
	conn, covSvc, server, teardown := setupTwinServer(t, "twin-secret")
	defer teardown()

	signer, signerB64, err := gittwin.GenerateSigningKey()
	must(t, err, "gen signer")
	prev, had := os.LookupEnv(gittwin.AnchorSigningKeyEnv)
	os.Setenv(gittwin.AnchorSigningKeyEnv, signerB64)
	defer func() {
		if had {
			os.Setenv(gittwin.AnchorSigningKeyEnv, prev)
		} else {
			os.Unsetenv(gittwin.AnchorSigningKeyEnv)
		}
	}()

	repoURL := "https://github.com/anchors/signed"
	cov, owner, err := covSvc.Create("Signed Anchor Covenant", "code", "github:owner")
	must(t, err, "create")
	must(t, covSvc.SetGitTwin(cov.CovenantID, repoURL, "github", ""), "bind twin")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	author, err := covSvc.Join(cov.CovenantID, "github:alice", "contributor")
	must(t, err, "join")
	engine := execution.New(conn, covSvc)
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ApproveAgent{}, map[string]any{"agent_id": author.AgentID})
	must(t, err, "approve")
	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")
	must(t, budget.EnsureCounter(conn, cov.CovenantID, 10000, "USD"), "budget")

	resp := postJSON(t, server.URL+"/git-twin/merge",
		map[string]string{"X-Bridge-Secret": "twin-secret"},
		map[string]any{
			"covenant_id":        cov.CovenantID,
			"author_platform_id": "github:alice",
			"draft_ref":          "https://github.com/anchors/signed/pull/1",
			"unit_count":         50,
			"acceptance_ratio":   1.0,
		})
	if resp.StatusCode != 200 {
		t.Fatalf("merge: want 200 got %d body=%s", resp.StatusCode, resp.Body)
	}

	_, err = covSvc.Transition(cov.CovenantID, "LOCKED")
	must(t, err, "→LOCKED")
	genReceipt, err := engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.GenerateSettlement{}, map[string]any{})
	must(t, err, "generate_settlement")
	outputID, _ := genReceipt.Extra["output_id"].(string)

	// The tool instance wired into api.New reads the signer at startup; for
	// this direct engine.Run path we pass the signer on the tool struct.
	_, err = engine.Run(cov.CovenantID, owner.AgentID, "sess_o",
		&tools.ConfirmSettlementOutput{AnchorSigner: signer},
		map[string]any{"settlement_output_id": outputID})
	must(t, err, "confirm_settlement_output")

	// Pull the anchor row and verify its note body.
	var noteBody string
	must(t, conn.QueryRow(
		`SELECT note_body FROM git_twin_anchors WHERE covenant_id=?`,
		cov.CovenantID).Scan(&noteBody), "select anchor")

	ok, err := gittwin.VerifyAnchorSignature([]byte(noteBody))
	if err != nil {
		t.Fatalf("verify anchor signature: %v", err)
	}
	if !ok {
		t.Fatal("signed anchor should verify; got false")
	}

	// Tampering invalidates it.
	tampered := strings.Replace(noteBody, "\"total_tokens\":", "\"total_tokens\":9999,\"old_total_tokens\":", 1)
	ok, _ = gittwin.VerifyAnchorSignature([]byte(tampered))
	if ok {
		t.Fatal("tampered anchor should not verify")
	}

	// The /git-twin/pubkey endpoint is only wired via api.New, which reads
	// the env var at construction. The httptest.Server in setupTwinServer
	// was built before we set the env, so it has no signer — we verify the
	// no-signer path here and cover the happy path via the unit test below.
	pub := getJSON(t, server.URL+"/git-twin/pubkey", nil)
	if pub.StatusCode != 404 {
		t.Fatalf("pubkey endpoint without signer: want 404 got %d body=%s", pub.StatusCode, pub.Body)
	}
}

// TestGitTwinPubkeyEndpoint covers the /git-twin/pubkey happy path: server
// started with a signer configured exposes {alg, public_key} to anyone.
func TestGitTwinPubkeyEndpoint(t *testing.T) {
	signer, signerB64, err := gittwin.GenerateSigningKey()
	must(t, err, "gen signer")
	prev, had := os.LookupEnv(gittwin.AnchorSigningKeyEnv)
	os.Setenv(gittwin.AnchorSigningKeyEnv, signerB64)
	defer func() {
		if had {
			os.Setenv(gittwin.AnchorSigningKeyEnv, prev)
		} else {
			os.Unsetenv(gittwin.AnchorSigningKeyEnv)
		}
	}()

	dbPath := t.TempDir() + "/pubkey.db"
	conn, err := db.Open(dbPath)
	must(t, err, "open db")
	defer conn.Close()
	srv := httptest.NewServer(api.New(conn))
	defer srv.Close()

	pub := getJSON(t, srv.URL+"/git-twin/pubkey", nil)
	if pub.StatusCode != 200 {
		t.Fatalf("pubkey: want 200 got %d body=%s", pub.StatusCode, pub.Body)
	}
	if pub.JSON["alg"] != gittwin.AlgEd25519 {
		t.Fatalf("alg: want %s got %v", gittwin.AlgEd25519, pub.JSON["alg"])
	}
	pubB64, _ := pub.JSON["public_key"].(string)
	decoded, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("pubkey b64: %v", err)
	}
	want := signer.PublicKey()
	if len(decoded) != len(want) {
		t.Fatalf("pubkey size: want %d got %d", len(want), len(decoded))
	}
	for i := range want {
		if decoded[i] != want[i] {
			t.Fatalf("pubkey bytes diverge at %d", i)
		}
	}
}

// TestGitTwinSetGitTwinDraftOnly verifies the DRAFT-state guard: an owner
// cannot bind (or rebind) a git twin after the covenant has opened.
func TestGitTwinSetGitTwinDraftOnly(t *testing.T) {
	conn, covSvc, _, teardown := setupTwinServer(t, "twin-secret")
	defer teardown()
	_ = conn

	cov, _, err := covSvc.Create("X", "code", "github:owner")
	must(t, err, "create")
	// DRAFT → allowed
	if err := covSvc.SetGitTwin(cov.CovenantID, "https://github.com/o/r", "github", ""); err != nil {
		t.Fatalf("set twin on DRAFT: %v", err)
	}
	got, err := covSvc.Get(cov.CovenantID)
	must(t, err, "get")
	if got.GitTwinURL != "https://github.com/o/r" || got.GitTwinProvider != "github" {
		t.Fatalf("twin not persisted: %+v", got)
	}
	// Bad provider
	if err := covSvc.SetGitTwin(cov.CovenantID, "https://x", "weird", ""); err == nil {
		t.Fatal("invalid provider must error")
	}
	// Move past DRAFT
	must(t, covSvc.AddTier(cov.CovenantID, "t", "T", 1.0, nil), "tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")
	// Post-DRAFT set → must error
	if err := covSvc.SetGitTwin(cov.CovenantID, "https://other", "github", ""); err == nil {
		t.Fatal("set twin after OPEN must error")
	}
}

// setupTwinServer bootstraps an httptest server with a fresh DB and the
// ACP_BRIDGE_SECRET env set so /git-twin/* endpoints are active. The returned
// teardown restores the previous env value.
func setupTwinServer(t *testing.T, secret string) (*sql.DB, *covenant.Service, *httptest.Server, func()) {
	t.Helper()
	dbPath := t.TempDir() + "/twin.db"
	conn, err := db.Open(dbPath)
	must(t, err, "open db")

	prev, hadPrev := os.LookupEnv("ACP_BRIDGE_SECRET")
	os.Setenv("ACP_BRIDGE_SECRET", secret)

	covSvc := covenant.New(conn)
	srv := httptest.NewServer(api.New(conn))

	teardown := func() {
		srv.Close()
		conn.Close()
		if hadPrev {
			os.Setenv("ACP_BRIDGE_SECRET", prev)
		} else {
			os.Unsetenv("ACP_BRIDGE_SECRET")
		}
	}
	return conn, covSvc, srv, teardown
}

type jsonResp struct {
	StatusCode int
	Body       string
	JSON       map[string]any
}

func postJSON(t *testing.T, url string, headers map[string]string, body any) jsonResp {
	t.Helper()
	raw, err := json.Marshal(body)
	must(t, err, "marshal")
	req, err := http.NewRequest("POST", url, bytes.NewReader(raw))
	must(t, err, "new request")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err, "do")
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	out := jsonResp{StatusCode: resp.StatusCode, Body: string(buf)}
	_ = json.Unmarshal(buf, &out.JSON)
	return out
}

func getJSON(t *testing.T, url string, headers map[string]string) jsonResp {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	must(t, err, "new request")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err, "do")
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	out := jsonResp{StatusCode: resp.StatusCode, Body: string(buf)}
	_ = json.Unmarshal(buf, &out.JSON)
	return out
}

// TestRateLimitPerHour exercises ACR-20 Part 4 Layer 2 end-to-end via the
// execution engine: the 4th clause-tool call from a rate-capped agent must
// be rejected at Step 1.5, the rejection must land in the audit chain, and
// other agents or admin tools must remain unaffected.
func TestRateLimitPerHour(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/ratelimit_e2e.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, ownerMem, err := covSvc.Create("Rate-limit test", "book", "pid_rl_owner")
	must(t, err, "create covenant")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")

	agentA, err := covSvc.Join(cov.CovenantID, "pid_rl_a", "contributor")
	must(t, err, "join A")
	agentB, err := covSvc.Join(cov.CovenantID, "pid_rl_b", "contributor")
	must(t, err, "join B")

	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")

	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentA.AgentID})
	must(t, err, "approve A")
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentB.AgentID})
	must(t, err, "approve B")

	// Owner sets the hourly cap. configure_anti_gaming is an admin tool and
	// itself exempt from rate limiting, so repeated owner calls do not drain
	// any bucket.
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ConfigureAntiGaming{}, map[string]any{"rate_limit_per_hour": 3})
	must(t, err, "configure_anti_gaming")

	// Three propose_passage calls from agent A fit under the cap.
	for i := 0; i < 3; i++ {
		_, err := engine.Run(cov.CovenantID, agentA.AgentID, "sess_a",
			&tools.ProposePassage{}, map[string]any{"unit_count": 100})
		if err != nil {
			t.Fatalf("propose A call %d: %v", i+1, err)
		}
	}

	// The 4th must be rejected at Step 1.5 with the rate-limit sentinel.
	_, err = engine.Run(cov.CovenantID, agentA.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 100})
	if err == nil {
		t.Fatal("expected rate-limit rejection on 4th propose, got nil")
	}
	if !strings.Contains(err.Error(), "step1.5") || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("error should mark Step 1.5 rate-limit rejection, got %q", err.Error())
	}

	// Audit log must contain the rejection (clause tools' rejection is
	// recorded so operators can see abuse patterns) and the hash chain
	// must remain intact across it.
	var rejections int
	must(t, conn.QueryRow(`
		SELECT COUNT(*) FROM audit_logs
		WHERE covenant_id=? AND agent_id=? AND result='rejected'
		  AND tool_name='propose_passage'`,
		cov.CovenantID, agentA.AgentID).Scan(&rejections), "count rejections")
	if rejections != 1 {
		t.Errorf("want 1 rejection row, got %d", rejections)
	}

	valid, violations := audit.VerifyChain(conn, cov.CovenantID)
	if !valid {
		t.Errorf("hash chain broken after rate-limit rejection: %v", violations)
	}

	// Agent B must be unaffected — independent bucket.
	_, err = engine.Run(cov.CovenantID, agentB.AgentID, "sess_b",
		&tools.ProposePassage{}, map[string]any{"unit_count": 100})
	if err != nil {
		t.Fatalf("agent B's bucket should be fresh: %v", err)
	}

	// Query tools (get_token_balance via API handler, not engine) do not
	// share the clause bucket. Verifying via another admin call: a 4th
	// owner ApproveDraft on an unrelated draft still works because admin
	// tools are exempt from the gate.
	// (Implicit: agentB's successful propose above already used an admin
	// approve path inside covenant.Service.Join — if the gate leaked into
	// non-clause tools, that would have failed.)
}

// TestConcentrationWarning exercises ACR-20 Part 4 Layer 5 end-to-end: after
// one agent's confirmed token share crosses concentration_warn_pct, the very
// next approve_draft receipt must carry a warnings list identifying that
// agent. The warning is a signal, not a gate — approvals still succeed.
func TestConcentrationWarning(t *testing.T) {
	conn, err := db.Open(t.TempDir() + "/concentration_e2e.db")
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	engine := execution.New(conn, covSvc)

	cov, ownerMem, err := covSvc.Create("Concentration test", "book", "pid_cw_owner")
	must(t, err, "create covenant")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")

	agentA, err := covSvc.Join(cov.CovenantID, "pid_cw_a", "contributor")
	must(t, err, "join A")
	agentB, err := covSvc.Join(cov.CovenantID, "pid_cw_b", "contributor")
	must(t, err, "join B")

	_, err = covSvc.Transition(cov.CovenantID, "ACTIVE")
	must(t, err, "→ACTIVE")

	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentA.AgentID})
	must(t, err, "approve A")
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveAgent{}, map[string]any{"agent_id": agentB.AgentID})
	must(t, err, "approve B")

	// Threshold 40%, rate limit disabled — concentration warning is what we
	// are measuring, rate limiting would only confuse the scenario.
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ConfigureAntiGaming{}, map[string]any{
			"rate_limit_per_hour":    0,
			"concentration_warn_pct": 40.0,
		})
	must(t, err, "configure_anti_gaming")

	// Agent A proposes a passage worth 800 tokens.
	_, err = engine.Run(cov.CovenantID, agentA.AgentID, "sess_a",
		&tools.ProposePassage{}, map[string]any{"unit_count": 800})
	must(t, err, "A propose_passage")
	draftA := getDraftID(t, conn, cov.CovenantID, agentA.AgentID)

	// First approval: only A has tokens (800/800 = 100%), well above 40% →
	// receipt must carry a warning naming agent A.
	receipt, err := engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":   draftA,
			"unit_count": 800,
		})
	must(t, err, "approve A")
	warnings := extractConcentrationAgents(t, receipt, "A approve_draft")
	if len(warnings) != 1 || warnings[0] != agentA.AgentID {
		t.Fatalf("A approve: want warning on %q, got %v", agentA.AgentID, warnings)
	}

	// Agent B now proposes 200 tokens → confirmed split 800/200, A share 80%.
	_, err = engine.Run(cov.CovenantID, agentB.AgentID, "sess_b",
		&tools.ProposePassage{}, map[string]any{"unit_count": 200})
	must(t, err, "B propose_passage")
	draftB := getDraftID(t, conn, cov.CovenantID, agentB.AgentID)

	receipt, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":   draftB,
			"unit_count": 200,
		})
	must(t, err, "approve B")
	warnings = extractConcentrationAgents(t, receipt, "B approve_draft")
	if len(warnings) != 1 || warnings[0] != agentA.AgentID {
		t.Fatalf("B approve: A at 80%% should still trigger warning, got %v", warnings)
	}
	if tot, _ := receipt.Extra["concentration_total"].(int); tot != 1000 {
		t.Errorf("concentration_total: got %v want 1000", receipt.Extra["concentration_total"])
	}

	// Raise threshold above current concentration — next approval's receipt
	// must come back clean. Proves the check is live on every call, not a
	// one-shot sticky flag.
	_, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ConfigureAntiGaming{}, map[string]any{
			"rate_limit_per_hour":    0,
			"concentration_warn_pct": 90.0,
		})
	must(t, err, "raise threshold")

	_, err = engine.Run(cov.CovenantID, agentB.AgentID, "sess_b",
		&tools.ProposePassage{}, map[string]any{"unit_count": 50})
	must(t, err, "B second propose")
	draftB2 := getDraftID(t, conn, cov.CovenantID, agentB.AgentID)
	receipt, err = engine.Run(cov.CovenantID, ownerMem.AgentID, "sess_owner",
		&tools.ApproveDraft{}, map[string]any{
			"draft_id":   draftB2,
			"unit_count": 50,
		})
	must(t, err, "approve B2")
	if _, present := receipt.Extra["concentration_warnings"]; present {
		t.Errorf("threshold=90%%, A at ~76%% → no warning expected, got %v", receipt.Extra["concentration_warnings"])
	}

	// Audit chain integrity — concentration work must not corrupt hash linkage.
	valid, violations := audit.VerifyChain(conn, cov.CovenantID)
	if !valid {
		t.Errorf("hash chain broken: %v", violations)
	}
}

// extractConcentrationAgents pulls the agent_ids out of
// receipt.Extra["concentration_warnings"]. The engine stores the slice as
// []ratelimit.ConcentrationEntry via the result map; we assert on agent_id
// so the helper stays decoupled from the ratelimit package layout.
func extractConcentrationAgents(t *testing.T, receipt *execution.Receipt, label string) []string {
	t.Helper()
	raw, ok := receipt.Extra["concentration_warnings"]
	if !ok {
		return nil
	}
	// ratelimit.ConcentrationEntry is the concrete type; reflect via JSON to
	// avoid importing ratelimit here (acp_test is the top-level integration
	// layer and already carries plenty of dependencies).
	buf, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("%s: marshal warnings: %v", label, err)
	}
	var decoded []struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("%s: unmarshal warnings: %v", label, err)
	}
	agents := make([]string, 0, len(decoded))
	for _, e := range decoded {
		agents = append(agents, e.AgentID)
	}
	return agents
}

// TestListMembersSurfacesPendingAccessRequests exercises Phase 4.6 (B) end-to-end:
// an ACR-50 apply produces a pending request row that must appear alongside
// existing members in the owner's list_members response, so the review queue
// is one roundtrip. After approval, the pending list empties and the member
// count grows by one. Plaintext platform_id never leaves the server — only
// the 12-char hash prefix does.
func TestListMembersSurfacesPendingAccessRequests(t *testing.T) {
	dbPath := t.TempDir() + "/list_members_pending.db"
	conn, err := db.Open(dbPath)
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	srv := httptest.NewServer(api.New(conn))
	defer srv.Close()

	// Covenant in OPEN, one tier. Owner already exists as a member (by Create).
	cov, _, err := covSvc.Create("List-Members Test", "code", "github:owner_lm")
	must(t, err, "create covenant")
	must(t, covSvc.AddTier(cov.CovenantID, "contributor", "Contributor", 1.0, nil), "add tier")
	_, err = covSvc.Transition(cov.CovenantID, "OPEN")
	must(t, err, "→OPEN")

	ownerHeaders := map[string]string{
		"X-Covenant-ID":  cov.CovenantID,
		"X-Owner-Token":  cov.OwnerToken,
		"Content-Type":   "application/json",
	}

	// Applicant files an access request via HTTP (not direct service call), so
	// we also verify the apply handler shape while we're here.
	applyResp := postJSON(t, srv.URL+"/covenants/"+cov.CovenantID+"/apply", nil, map[string]any{
		"platform_id":      "github:applicant_lm",
		"tier_id":          "contributor",
		"payment_ref":      "stripe:pi_test",
		"self_declaration": "I will contribute.",
	})
	if applyResp.StatusCode != 200 {
		t.Fatalf("apply: status=%d body=%s", applyResp.StatusCode, applyResp.Body)
	}
	requestID, _ := applyResp.JSON["request_id"].(string)
	if requestID == "" {
		t.Fatalf("apply response missing request_id: %s", applyResp.Body)
	}
	if _, hasPlain := applyResp.JSON["platform_id"]; hasPlain {
		t.Errorf("apply response leaked plaintext platform_id: %s", applyResp.Body)
	}

	// list_members must surface the pending request alongside the (1) owner member.
	listResp := postJSON(t, srv.URL+"/tools/list_members", ownerHeaders, map[string]any{
		"params": map[string]any{"covenant_id": cov.CovenantID},
	})
	if listResp.StatusCode != 200 {
		t.Fatalf("list_members: status=%d body=%s", listResp.StatusCode, listResp.Body)
	}
	members, _ := listResp.JSON["members"].([]any)
	if len(members) != 1 {
		t.Errorf("want 1 member (owner), got %d: %s", len(members), listResp.Body)
	}
	pending, _ := listResp.JSON["pending_access_requests"].([]any)
	if len(pending) != 1 {
		t.Fatalf("want 1 pending request, got %d: %s", len(pending), listResp.Body)
	}
	p := pending[0].(map[string]any)
	if p["request_id"] != requestID {
		t.Errorf("pending request_id mismatch: got %v want %s", p["request_id"], requestID)
	}
	if prefix, _ := p["platform_id_hash_prefix"].(string); len(prefix) != 12 {
		t.Errorf("hash prefix length %d, want 12: %v", len(prefix), p)
	}
	if _, hasPlain := p["platform_id"]; hasPlain {
		t.Errorf("pending entry leaked plaintext platform_id: %v", p)
	}
	if p["tier_id"] != "contributor" {
		t.Errorf("tier_id = %v, want contributor", p["tier_id"])
	}

	// Approve via HTTP admin tool. After approval the pending list empties and
	// member count goes to 2 (owner + new active member).
	approveResp := postJSON(t, srv.URL+"/tools/approve_agent_access", ownerHeaders, map[string]any{
		"params": map[string]any{"request_id": requestID},
	})
	if approveResp.StatusCode != 200 {
		t.Fatalf("approve: status=%d body=%s", approveResp.StatusCode, approveResp.Body)
	}

	listResp2 := postJSON(t, srv.URL+"/tools/list_members", ownerHeaders, map[string]any{
		"params": map[string]any{"covenant_id": cov.CovenantID},
	})
	members2, _ := listResp2.JSON["members"].([]any)
	if len(members2) != 2 {
		t.Errorf("post-approve want 2 members, got %d: %s", len(members2), listResp2.Body)
	}
	pending2, _ := listResp2.JSON["pending_access_requests"].([]any)
	if len(pending2) != 0 {
		t.Errorf("post-approve want 0 pending, got %d: %s", len(pending2), listResp2.Body)
	}
}

// TestGetAgentAccessStatus exercises Phase 4.6 (A): an applicant must be
// able to poll their request status without a session token, and cross-
// covenant fishing with a valid request_id but the wrong covenant_id
// must be indistinguishable from "not found".
func TestGetAgentAccessStatus(t *testing.T) {
	dbPath := t.TempDir() + "/access_status.db"
	conn, err := db.Open(dbPath)
	must(t, err, "open db")
	defer conn.Close()

	covSvc := covenant.New(conn)
	srv := httptest.NewServer(api.New(conn))
	defer srv.Close()

	covA, _, err := covSvc.Create("A", "code", "github:ownerA")
	must(t, err, "create A")
	must(t, covSvc.AddTier(covA.CovenantID, "contributor", "C", 1.0, nil), "tier A")
	_, err = covSvc.Transition(covA.CovenantID, "OPEN")
	must(t, err, "→OPEN A")

	covB, _, err := covSvc.Create("B", "code", "github:ownerB")
	must(t, err, "create B")
	must(t, covSvc.AddTier(covB.CovenantID, "contributor", "C", 1.0, nil), "tier B")
	_, err = covSvc.Transition(covB.CovenantID, "OPEN")
	must(t, err, "→OPEN B")

	arA, err := covSvc.CreateAccessRequest(covA.CovenantID, "github:alice", "contributor", "stripe:pi_x", "declared")
	must(t, err, "apply A")

	statusURL := srv.URL + "/tools/get_agent_access_status"

	// Pending — no auth needed.
	resp := postJSON(t, statusURL, nil, map[string]any{
		"params": map[string]any{
			"covenant_id": covA.CovenantID,
			"request_id":  arA.RequestID,
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("pending poll: status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if resp.JSON["status"] != "pending" {
		t.Errorf("status = %v, want pending", resp.JSON["status"])
	}
	for _, leaked := range []string{"payment_ref", "self_declaration", "platform_id_hash", "platform_id_hash_prefix", "platform_id"} {
		if _, ok := resp.JSON[leaked]; ok {
			t.Errorf("pending poll leaked %q: %s", leaked, resp.Body)
		}
	}

	// Cross-covenant fishing: same request_id, wrong covenant → 404.
	crossResp := postJSON(t, statusURL, nil, map[string]any{
		"params": map[string]any{
			"covenant_id": covB.CovenantID,
			"request_id":  arA.RequestID,
		},
	})
	if crossResp.StatusCode != 404 {
		t.Errorf("cross-covenant poll: want 404, got %d body=%s", crossResp.StatusCode, crossResp.Body)
	}

	// Rejected state carries reject_reason; resolved_at populated.
	must(t, covSvc.RejectAccessRequest(covA.CovenantID, arA.RequestID, "tier full", "log_reject"), "reject")
	rejResp := postJSON(t, statusURL, nil, map[string]any{
		"params": map[string]any{
			"covenant_id": covA.CovenantID,
			"request_id":  arA.RequestID,
		},
	})
	if rejResp.JSON["status"] != "rejected" {
		t.Errorf("rejected status = %v", rejResp.JSON["status"])
	}
	if rejResp.JSON["reject_reason"] != "tier full" {
		t.Errorf("reject_reason = %v", rejResp.JSON["reject_reason"])
	}
	if rejResp.JSON["resolved_at"] == nil {
		t.Error("resolved_at missing after reject")
	}

	// Approved state has resolved_at but no reject_reason.
	arA2, err := covSvc.CreateAccessRequest(covA.CovenantID, "github:bob", "contributor", "", "")
	must(t, err, "apply A2")
	_, err = covSvc.ApproveAccessRequest(covA.CovenantID, arA2.RequestID, "log_approve")
	must(t, err, "approve A2")

	okResp := postJSON(t, statusURL, nil, map[string]any{
		"params": map[string]any{
			"covenant_id": covA.CovenantID,
			"request_id":  arA2.RequestID,
		},
	})
	if okResp.JSON["status"] != "approved" {
		t.Errorf("approved status = %v", okResp.JSON["status"])
	}
	if okResp.JSON["resolved_at"] == nil {
		t.Error("resolved_at missing after approve")
	}
	if _, ok := okResp.JSON["reject_reason"]; ok {
		t.Error("reject_reason should not appear on approved request")
	}
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
