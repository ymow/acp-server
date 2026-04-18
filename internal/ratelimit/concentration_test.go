package ratelimit

import (
	"database/sql"
	"testing"
)

// seedLedger inserts one confirmed ledger entry. The helper fabricates unique
// ids/log_ids so each call stays legal under the UNIQUE(log_id) constraint.
func seedLedger(t *testing.T, conn *sql.DB, covenantID, agentID string, delta int, nonce string) {
	t.Helper()
	_, err := conn.Exec(`
		INSERT INTO token_ledger
			(id, covenant_id, agent_id, delta, balance_after, source_type, source_ref, log_id, status)
		VALUES (?, ?, ?, ?, ?, 'passage', ?, ?, 'confirmed')`,
		"lg_"+nonce, covenantID, agentID, delta, delta, "draft_"+nonce, "log_"+nonce,
	)
	if err != nil {
		t.Fatalf("seed ledger (%s, %s, %d): %v", covenantID, agentID, delta, err)
	}
}

func TestCheckConcentrationNoPolicyDisablesWarnings(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_np")
	seedLedger(t, conn, "cov_np", "agent_a", 900, "a1")
	seedLedger(t, conn, "cov_np", "agent_b", 100, "b1")

	report, err := CheckConcentration(conn, "cov_np")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if report.Threshold != 0 {
		t.Errorf("threshold: got %v want 0", report.Threshold)
	}
	if report.Total != 1000 {
		t.Errorf("total: got %d want 1000", report.Total)
	}
	if report.Warnings != nil {
		t.Errorf("warnings must be nil when threshold is 0, got %+v", report.Warnings)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries: got %d want 2", len(report.Entries))
	}
	if report.Entries[0].AgentID != "agent_a" || report.Entries[0].Tokens != 900 {
		t.Errorf("entry[0]: got %+v want agent_a/900", report.Entries[0])
	}
}

func TestCheckConcentrationBelowThreshold(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_bt")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_bt", ConcentrationWarnPct: 60})
	seedLedger(t, conn, "cov_bt", "agent_a", 50, "a1")
	seedLedger(t, conn, "cov_bt", "agent_b", 50, "b1")

	report, err := CheckConcentration(conn, "cov_bt")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if report.Warnings != nil {
		t.Errorf("want no warnings at 50/50 vs 60%% threshold, got %+v", report.Warnings)
	}
	if len(report.Entries) != 2 {
		t.Errorf("entries: got %d want 2", len(report.Entries))
	}
}

func TestCheckConcentrationSingleAgentOverThreshold(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_so")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_so", ConcentrationWarnPct: 40})
	seedLedger(t, conn, "cov_so", "agent_a", 800, "a1")
	seedLedger(t, conn, "cov_so", "agent_b", 200, "b1")

	report, err := CheckConcentration(conn, "cov_so")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("warnings: got %d want 1", len(report.Warnings))
	}
	w := report.Warnings[0]
	if w.AgentID != "agent_a" {
		t.Errorf("warning agent: got %q want agent_a", w.AgentID)
	}
	if w.Share != 80 {
		t.Errorf("share: got %v want 80", w.Share)
	}
}

func TestCheckConcentrationMultipleWarnings(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_mw")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_mw", ConcentrationWarnPct: 30})
	seedLedger(t, conn, "cov_mw", "agent_a", 400, "a1")
	seedLedger(t, conn, "cov_mw", "agent_b", 400, "b1")
	seedLedger(t, conn, "cov_mw", "agent_c", 200, "c1")

	report, err := CheckConcentration(conn, "cov_mw")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if len(report.Warnings) != 2 {
		t.Fatalf("warnings: got %d want 2 (a,b each hold 40%%)", len(report.Warnings))
	}
	// Deterministic sort: equal tokens → agent_id ascending.
	if report.Warnings[0].AgentID != "agent_a" || report.Warnings[1].AgentID != "agent_b" {
		t.Errorf("warning order: got [%s, %s] want [agent_a, agent_b]",
			report.Warnings[0].AgentID, report.Warnings[1].AgentID)
	}
}

func TestCheckConcentrationZeroTotal(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_zt")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_zt", ConcentrationWarnPct: 40})

	report, err := CheckConcentration(conn, "cov_zt")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if report.Total != 0 {
		t.Errorf("total: got %d want 0", report.Total)
	}
	if report.Entries != nil || report.Warnings != nil {
		t.Errorf("empty covenant should yield empty entries/warnings, got %+v / %+v",
			report.Entries, report.Warnings)
	}
	if report.Threshold != 40 {
		t.Errorf("threshold still propagated: got %v want 40", report.Threshold)
	}
}

func TestCheckConcentrationExcludesNonPositiveAgents(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_ex")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_ex", ConcentrationWarnPct: 60})
	seedLedger(t, conn, "cov_ex", "agent_a", 900, "a1")
	// agent_b has a net zero (earn then reversal) — should not appear in entries.
	seedLedger(t, conn, "cov_ex", "agent_b", 100, "b1")
	seedLedger(t, conn, "cov_ex", "agent_b", -100, "b2")
	// agent_c earned only 100 — appears, below threshold.
	seedLedger(t, conn, "cov_ex", "agent_c", 100, "c1")

	report, err := CheckConcentration(conn, "cov_ex")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if report.Total != 1000 {
		t.Errorf("total must ignore zero-net agents: got %d want 1000", report.Total)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("entries: got %d want 2 (agent_b excluded)", len(report.Entries))
	}
	for _, e := range report.Entries {
		if e.AgentID == "agent_b" {
			t.Errorf("agent_b with net 0 must not appear: %+v", e)
		}
	}
	if len(report.Warnings) != 1 || report.Warnings[0].AgentID != "agent_a" {
		t.Errorf("warnings: got %+v want single agent_a", report.Warnings)
	}
}

func TestCheckConcentrationIgnoresNonConfirmedLedger(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_nc")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_nc", ConcentrationWarnPct: 60})
	seedLedger(t, conn, "cov_nc", "agent_a", 100, "a1")
	// Insert a pending ledger row directly — must not affect totals.
	_, err := conn.Exec(`
		INSERT INTO token_ledger
			(id, covenant_id, agent_id, delta, balance_after, source_type, source_ref, log_id, status)
		VALUES ('lg_pend', 'cov_nc', 'agent_b', 10000, 10000, 'passage', 'd_pend', 'log_pend', 'pending')`)
	if err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	report, err := CheckConcentration(conn, "cov_nc")
	if err != nil {
		t.Fatalf("CheckConcentration: %v", err)
	}
	if report.Total != 100 {
		t.Errorf("pending rows must be excluded: got total %d want 100", report.Total)
	}
	if len(report.Entries) != 1 || report.Entries[0].AgentID != "agent_a" {
		t.Errorf("entries should contain only agent_a: %+v", report.Entries)
	}
}
