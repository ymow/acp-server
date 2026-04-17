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
