package ratelimit

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inkmesh/acp-server/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(t.TempDir() + "/ratelimit_test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// The schema declares anti_gaming_policies.covenant_id as a FK to
	// covenants(covenant_id); insert stub rows so UpsertPolicy doesn't trip
	// the constraint. Empty owner_id is tolerated by Phase 3.0 backfill guard.
	t.Cleanup(func() { conn.Close() })
	return conn
}

func seedCovenant(t *testing.T, conn *sql.DB, covenantID string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := conn.Exec(`
		INSERT INTO covenants (covenant_id, title, state, created_at, updated_at)
		VALUES (?, 'test', 'ACTIVE', ?, ?)`, covenantID, now, now)
	if err != nil {
		t.Fatalf("seed covenant %s: %v", covenantID, err)
	}
}

func TestLoadPolicyReturnsZeroWhenAbsent(t *testing.T) {
	conn := openTestDB(t)
	p, err := LoadPolicy(conn, "cov_missing")
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.RateLimitPerHour != 0 {
		t.Errorf("want zero limit for absent policy, got %d", p.RateLimitPerHour)
	}
	if p.CovenantID != "cov_missing" {
		t.Errorf("CovenantID not propagated: %q", p.CovenantID)
	}
}

func TestUpsertPolicyRequiresCovenantID(t *testing.T) {
	conn := openTestDB(t)
	if err := UpsertPolicy(conn, Policy{}); err == nil {
		t.Fatal("expected error on empty CovenantID")
	}
}

func TestUpsertPolicyRoundtrip(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_rt")
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_rt", RateLimitPerHour: 5}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Update on same key should overwrite, not duplicate.
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_rt", RateLimitPerHour: 9}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	p, err := LoadPolicy(conn, "cov_rt")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.RateLimitPerHour != 9 {
		t.Errorf("limit after update: got %d want 9", p.RateLimitPerHour)
	}
}

func TestCheckAndIncrementUnlimitedWhenZero(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_zero")
	// No policy row → default zero → unlimited.
	for i := 0; i < 10; i++ {
		if err := CheckAndIncrement(conn, "cov_zero", "agent_a", "propose_passage"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	// No counter row should have been written.
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM rate_limit_counters`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("counter rows without policy: got %d want 0", n)
	}
}

func TestCheckAndIncrementEnforcesLimit(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_lim")
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_lim", RateLimitPerHour: 3}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := CheckAndIncrement(conn, "cov_lim", "agent_a", "propose_passage"); err != nil {
			t.Fatalf("call %d within limit: %v", i+1, err)
		}
	}
	err := CheckAndIncrement(conn, "cov_lim", "agent_a", "propose_passage")
	if err == nil {
		t.Fatal("expected ErrRateLimitExceeded on 4th call")
	}
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("want ErrRateLimitExceeded, got %v", err)
	}
}

func TestCheckAndIncrementIsolatesAgents(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_iso_ag")
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_iso_ag", RateLimitPerHour: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// agent_a burns its quota.
	if err := CheckAndIncrement(conn, "cov_iso_ag", "agent_a", "propose_passage"); err != nil {
		t.Fatalf("agent_a call 1: %v", err)
	}
	if err := CheckAndIncrement(conn, "cov_iso_ag", "agent_a", "propose_passage"); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("agent_a should be exhausted: %v", err)
	}
	// agent_b must still have its own allowance.
	if err := CheckAndIncrement(conn, "cov_iso_ag", "agent_b", "propose_passage"); err != nil {
		t.Fatalf("agent_b call 1 should pass: %v", err)
	}
}

func TestCheckAndIncrementIsolatesCovenants(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_x")
	seedCovenant(t, conn, "cov_y")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_x", RateLimitPerHour: 1})
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_y", RateLimitPerHour: 1})

	if err := CheckAndIncrement(conn, "cov_x", "agent_a", "propose_passage"); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndIncrement(conn, "cov_x", "agent_a", "propose_passage"); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("cov_x exhausted expected: %v", err)
	}
	// Same agent in cov_y must have an independent bucket.
	if err := CheckAndIncrement(conn, "cov_y", "agent_a", "propose_passage"); err != nil {
		t.Fatalf("cov_y should still allow: %v", err)
	}
}

func TestCheckAndIncrementHourRollover(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_roll")
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_roll", RateLimitPerHour: 2}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Simulate an exhausted prior hour by inserting directly with a stale window.
	prior := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	_, err := conn.Exec(`
		INSERT INTO rate_limit_counters (covenant_id, agent_id, tool_name, window_start, call_count)
		VALUES (?, ?, ?, ?, ?)`,
		"cov_roll", "agent_a", GlobalBucket, prior, 2)
	if err != nil {
		t.Fatalf("seed prior window: %v", err)
	}

	// Current window should still have 2 fresh slots regardless of the prior row.
	for i := 0; i < 2; i++ {
		if err := CheckAndIncrement(conn, "cov_roll", "agent_a", "propose_passage"); err != nil {
			t.Fatalf("fresh window call %d: %v", i+1, err)
		}
	}
	if err := CheckAndIncrement(conn, "cov_roll", "agent_a", "propose_passage"); !errors.Is(err, ErrRateLimitExceeded) {
		t.Fatalf("fresh window 3rd call expected ErrRateLimitExceeded, got %v", err)
	}
}

func TestCheckAndIncrementConcurrentRespectsLimit(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_cc")
	limit := 10
	workers := 50
	if err := UpsertPolicy(conn, Policy{CovenantID: "cov_cc", RateLimitPerHour: limit}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var ok, rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := CheckAndIncrement(conn, "cov_cc", "agent_a", "propose_passage")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrRateLimitExceeded):
				rejected.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != int32(limit) {
		t.Errorf("accepted calls: got %d want %d", ok.Load(), limit)
	}
	if rejected.Load() != int32(workers-limit) {
		t.Errorf("rejected calls: got %d want %d", rejected.Load(), workers-limit)
	}

	// Verify the persisted counter equals the limit exactly.
	var count int
	err := conn.QueryRow(`
		SELECT call_count FROM rate_limit_counters
		WHERE covenant_id=? AND agent_id=? AND tool_name=?`,
		"cov_cc", "agent_a", GlobalBucket).Scan(&count)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if count != limit {
		t.Errorf("persisted counter: got %d want %d", count, limit)
	}
}

func TestErrRateLimitExceededCarriesContext(t *testing.T) {
	conn := openTestDB(t)
	seedCovenant(t, conn, "cov_ctx")
	_ = UpsertPolicy(conn, Policy{CovenantID: "cov_ctx", RateLimitPerHour: 1})
	_ = CheckAndIncrement(conn, "cov_ctx", "agent_a", "propose_passage")
	err := CheckAndIncrement(conn, "cov_ctx", "agent_a", "propose_passage")
	if err == nil {
		t.Fatal("expected rejection")
	}
	msg := fmt.Sprintf("%v", err)
	if !errors.Is(err, ErrRateLimitExceeded) || msg == ErrRateLimitExceeded.Error() {
		t.Errorf("error should wrap sentinel with context, got %q", msg)
	}
}
