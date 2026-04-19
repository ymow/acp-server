package covenant

import (
	"testing"
)

// accessTestSvc spins up a covenant in OPEN state with one tier so the
// happy-path CreateAccessRequest tests have somewhere to land. Mirrors
// the harness in platform_id_test.go to keep setup cost low.
func accessTestSvc(t *testing.T, withSealer bool) (*Service, string) {
	t.Helper()
	db := newTestDB(t)
	svc := New(db)
	if withSealer {
		svc.SetSealer(newTestSealer(t))
	}
	cov, _, err := svc.Create("Test", "code", "github:owner")
	if err != nil {
		t.Fatalf("create covenant: %v", err)
	}
	if err := svc.AddTier(cov.CovenantID, "tier_a", "Tier A", 1.0, nil, 0); err != nil {
		t.Fatalf("add tier: %v", err)
	}
	if _, err := svc.Transition(cov.CovenantID, "OPEN"); err != nil {
		t.Fatalf("transition to OPEN: %v", err)
	}
	return svc, cov.CovenantID
}

func TestCreateAccessRequestHappyPath(t *testing.T) {
	svc, covID := accessTestSvc(t, true)

	ar, err := svc.CreateAccessRequest(covID, "github:applicant", "tier_a", "stripe:pi_123", "I agree to the terms.")
	if err != nil {
		t.Fatalf("create access request: %v", err)
	}
	if ar.Status != "pending" {
		t.Errorf("status = %q, want pending", ar.Status)
	}
	if ar.RequestID == "" || ar.PlatformIDHash == "" {
		t.Errorf("missing ids: request_id=%q hash=%q", ar.RequestID, ar.PlatformIDHash)
	}
	if ar.PlatformIDHash != HashPlatformID("github:applicant") {
		t.Errorf("hash mismatch")
	}

	// Verify platform_id_enc was populated by the sealer.
	var enc []byte
	err = svc.db.QueryRow(`SELECT platform_id_enc FROM agent_access_requests WHERE request_id=?`, ar.RequestID).Scan(&enc)
	if err != nil {
		t.Fatalf("scan enc: %v", err)
	}
	if len(enc) == 0 {
		t.Error("platform_id_enc empty — sealer did not write ciphertext")
	}
}

func TestCreateAccessRequestRejectsNonOpen(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	svc.SetSealer(newTestSealer(t))

	cov, _, err := svc.Create("Test", "code", "github:owner")
	if err != nil {
		t.Fatalf("create covenant: %v", err)
	}
	// DRAFT — no tier, no transition. Apply must be rejected.
	if _, err := svc.CreateAccessRequest(cov.CovenantID, "github:x", "tier_a", "", ""); err == nil {
		t.Error("expected error applying to DRAFT covenant")
	}
}

func TestCreateAccessRequestRejectsUnknownTier(t *testing.T) {
	svc, covID := accessTestSvc(t, true)
	if _, err := svc.CreateAccessRequest(covID, "github:x", "tier_does_not_exist", "", ""); err == nil {
		t.Error("expected error applying with unknown tier")
	}
}

func TestCreateAccessRequestWithoutSealerLeavesEncNull(t *testing.T) {
	svc, covID := accessTestSvc(t, false)
	ar, err := svc.CreateAccessRequest(covID, "github:x", "tier_a", "", "")
	if err != nil {
		t.Fatalf("create access request: %v", err)
	}
	var enc []byte
	if err := svc.db.QueryRow(
		`SELECT platform_id_enc FROM agent_access_requests WHERE request_id=?`,
		ar.RequestID,
	).Scan(&enc); err != nil {
		t.Fatalf("scan enc: %v", err)
	}
	if enc != nil {
		t.Errorf("expected NULL platform_id_enc without sealer, got %d bytes", len(enc))
	}
}

func TestGetAccessRequestScopedToCovenant(t *testing.T) {
	// A request created under covenant A must NOT be retrievable by passing
	// the same request_id with a different covenant_id. Protects the owner
	// review queue from cross-covenant lookups.
	dbA := newTestDB(t)
	svc := New(dbA)
	svc.SetSealer(newTestSealer(t))

	covA, _, _ := svc.Create("A", "code", "github:ownerA")
	svc.AddTier(covA.CovenantID, "tier_a", "A", 1.0, nil, 0)
	svc.Transition(covA.CovenantID, "OPEN")

	covB, _, _ := svc.Create("B", "code", "github:ownerB")
	svc.AddTier(covB.CovenantID, "tier_a", "A", 1.0, nil, 0)
	svc.Transition(covB.CovenantID, "OPEN")

	arA, err := svc.CreateAccessRequest(covA.CovenantID, "github:applicant", "tier_a", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.GetAccessRequest(covA.CovenantID, arA.RequestID); err != nil {
		t.Errorf("same-covenant lookup failed: %v", err)
	}
	if _, err := svc.GetAccessRequest(covB.CovenantID, arA.RequestID); err == nil {
		t.Error("cross-covenant lookup succeeded — scoping broken")
	}
}

func TestApproveAccessRequestHappyPath(t *testing.T) {
	svc, covID := accessTestSvc(t, true)

	ar, err := svc.CreateAccessRequest(covID, "github:applicant", "tier_a", "stripe:pi_1", "I agree.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mem, err := svc.ApproveAccessRequest(covID, ar.RequestID, "log_approve_1")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if mem.Status != "active" {
		t.Errorf("member status = %q, want active", mem.Status)
	}
	if mem.AgentID == "" {
		t.Error("member agent_id empty")
	}
	if mem.TierID != "tier_a" {
		t.Errorf("tier_id = %q, want tier_a", mem.TierID)
	}
	if mem.PlatformID != "github:applicant" {
		t.Errorf("plaintext platform_id not resolved: got %q", mem.PlatformID)
	}

	// Verify request row is flipped and log_id recorded.
	reloaded, err := svc.GetAccessRequest(covID, ar.RequestID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if reloaded.Status != "approved" {
		t.Errorf("request status = %q, want approved", reloaded.Status)
	}
	if reloaded.ApproveLogID != "log_approve_1" {
		t.Errorf("approve_log_id = %q, want log_approve_1", reloaded.ApproveLogID)
	}
	if reloaded.ResolvedAt == nil {
		t.Error("resolved_at not set after approve")
	}
}

func TestApproveAccessRequestIdempotent(t *testing.T) {
	// Second approve on a resolved request must fail — ACR-50 §7 idempotency.
	// Enforced at the service layer, not via a DB trigger.
	svc, covID := accessTestSvc(t, true)
	ar, _ := svc.CreateAccessRequest(covID, "github:a", "tier_a", "", "")

	if _, err := svc.ApproveAccessRequest(covID, ar.RequestID, "log1"); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if _, err := svc.ApproveAccessRequest(covID, ar.RequestID, "log2"); err == nil {
		t.Error("second approve succeeded — idempotency broken")
	}
}

func TestApproveAccessRequestRejectsUnknown(t *testing.T) {
	svc, covID := accessTestSvc(t, true)
	if _, err := svc.ApproveAccessRequest(covID, "areq_does_not_exist", "log"); err == nil {
		t.Error("expected error approving unknown request")
	}
}

func TestRejectAccessRequestHappyPath(t *testing.T) {
	svc, covID := accessTestSvc(t, true)
	ar, _ := svc.CreateAccessRequest(covID, "github:applicant", "tier_a", "", "")

	if err := svc.RejectAccessRequest(covID, ar.RequestID, "no slots", "log_reject_1"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	reloaded, err := svc.GetAccessRequest(covID, ar.RequestID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != "rejected" {
		t.Errorf("status = %q, want rejected", reloaded.Status)
	}
	if reloaded.RejectReason != "no slots" {
		t.Errorf("reject_reason = %q, want 'no slots'", reloaded.RejectReason)
	}
	if reloaded.RejectLogID != "log_reject_1" {
		t.Errorf("reject_log_id = %q", reloaded.RejectLogID)
	}
	if reloaded.ResolvedAt == nil {
		t.Error("resolved_at not set after reject")
	}

	// No covenant_members row should exist for this applicant.
	var n int
	svc.db.QueryRow(
		`SELECT COUNT(*) FROM covenant_members WHERE covenant_id=? AND platform_id=?`,
		covID, "github:applicant",
	).Scan(&n)
	if n != 0 {
		t.Errorf("rejected applicant got %d covenant_members rows, want 0", n)
	}
}

func TestRejectAccessRequestIdempotent(t *testing.T) {
	svc, covID := accessTestSvc(t, true)
	ar, _ := svc.CreateAccessRequest(covID, "github:a", "tier_a", "", "")

	if err := svc.RejectAccessRequest(covID, ar.RequestID, "first", "log1"); err != nil {
		t.Fatalf("first reject: %v", err)
	}
	if err := svc.RejectAccessRequest(covID, ar.RequestID, "second", "log2"); err == nil {
		t.Error("second reject succeeded — idempotency broken")
	}
}

func TestApproveAccessRequestCrossCovenantScoped(t *testing.T) {
	// A request on covenant A must not be approvable via covenant B, even if
	// the request_id is known. Same protection as GetAccessRequest.
	svc, covA := accessTestSvc(t, true)
	covB, _, _ := svc.Create("B", "code", "github:ownerB")
	svc.AddTier(covB.CovenantID, "tier_a", "A", 1.0, nil, 0)
	svc.Transition(covB.CovenantID, "OPEN")

	ar, _ := svc.CreateAccessRequest(covA, "github:applicant", "tier_a", "", "")
	if _, err := svc.ApproveAccessRequest(covB.CovenantID, ar.RequestID, "log"); err == nil {
		t.Error("cross-covenant approve succeeded — scoping broken")
	}
}

func TestListPendingAccessRequests(t *testing.T) {
	svc, covID := accessTestSvc(t, true)

	if _, err := svc.CreateAccessRequest(covID, "github:a", "tier_a", "", ""); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := svc.CreateAccessRequest(covID, "github:b", "tier_a", "", ""); err != nil {
		t.Fatalf("create b: %v", err)
	}

	pending, err := svc.ListPendingAccessRequests(covID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("pending = %d, want 2", len(pending))
	}
	for _, p := range pending {
		if p.Status != "pending" {
			t.Errorf("unexpected status %q in pending list", p.Status)
		}
	}
}

// feeTestSvc mirrors accessTestSvc but takes an explicit entry_fee so the
// ACR-50 §7 ledger-integration tests can pin the fee value.
func feeTestSvc(t *testing.T, entryFee int64) (*Service, string) {
	t.Helper()
	db := newTestDB(t)
	svc := New(db)
	svc.SetSealer(newTestSealer(t))
	cov, _, err := svc.Create("Fee", "code", "github:owner")
	if err != nil {
		t.Fatalf("create covenant: %v", err)
	}
	if err := svc.AddTier(cov.CovenantID, "tier_fee", "Fee Tier", 1.0, nil, entryFee); err != nil {
		t.Fatalf("add tier: %v", err)
	}
	if _, err := svc.Transition(cov.CovenantID, "OPEN"); err != nil {
		t.Fatalf("transition OPEN: %v", err)
	}
	return svc, cov.CovenantID
}

func TestApproveBooksEntryFeeLedgerRow(t *testing.T) {
	svc, covID := feeTestSvc(t, 500)
	ar, err := svc.CreateAccessRequest(covID, "github:applicant", "tier_fee", "", "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	mem, err := svc.ApproveAccessRequest(covID, ar.RequestID, "log_approve_fee")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	var delta, balanceAfter int64
	var sourceType, sourceRef, logID, status string
	err = svc.db.QueryRow(`
		SELECT delta, balance_after, source_type, source_ref, log_id, status
		FROM token_ledger
		WHERE covenant_id=? AND agent_id=? AND source_type='entry_fee'`,
		covID, mem.AgentID,
	).Scan(&delta, &balanceAfter, &sourceType, &sourceRef, &logID, &status)
	if err != nil {
		t.Fatalf("ledger row not found: %v", err)
	}
	if delta != -500 {
		t.Errorf("delta = %d, want -500", delta)
	}
	if balanceAfter != -500 {
		t.Errorf("balance_after = %d, want -500 (debt model)", balanceAfter)
	}
	if sourceRef != ar.RequestID {
		t.Errorf("source_ref = %q, want request_id %q", sourceRef, ar.RequestID)
	}
	if logID != "log_approve_fee" {
		t.Errorf("log_id = %q, want approve_log_id", logID)
	}
	if status != "confirmed" {
		t.Errorf("status = %q, want confirmed", status)
	}
}

func TestApproveZeroFeeSkipsLedger(t *testing.T) {
	// The legacy path — tiers without a fee must not produce a ledger row.
	// Otherwise pre-4.6.C covenants would gain spurious 0-delta rows on every
	// approve, polluting settlement math.
	svc, covID := feeTestSvc(t, 0)
	ar, _ := svc.CreateAccessRequest(covID, "github:applicant", "tier_fee", "", "")
	mem, err := svc.ApproveAccessRequest(covID, ar.RequestID, "log_approve_nofee")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	var n int
	if err := svc.db.QueryRow(
		`SELECT COUNT(*) FROM token_ledger WHERE covenant_id=? AND agent_id=?`,
		covID, mem.AgentID,
	).Scan(&n); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if n != 0 {
		t.Errorf("zero-fee approve created %d ledger rows, want 0", n)
	}
}

func TestApproveEntryFeeTransactional(t *testing.T) {
	// If the ledger insert fails the whole approve must roll back — otherwise
	// a member could end up active without the fee on record, or vice versa.
	// Forcing a duplicate log_id is the cheapest way to trigger an insert
	// error inside the tx.
	svc, covID := feeTestSvc(t, 100)
	ar1, _ := svc.CreateAccessRequest(covID, "github:a", "tier_fee", "", "")
	if _, err := svc.ApproveAccessRequest(covID, ar1.RequestID, "log_dup"); err != nil {
		t.Fatalf("first approve: %v", err)
	}

	ar2, _ := svc.CreateAccessRequest(covID, "github:b", "tier_fee", "", "")
	if _, err := svc.ApproveAccessRequest(covID, ar2.RequestID, "log_dup"); err == nil {
		t.Fatal("expected approve to fail on duplicate log_id")
	}

	// Second applicant must not have a covenant_members row: the tx rolled back.
	var memCount int
	svc.db.QueryRow(
		`SELECT COUNT(*) FROM covenant_members WHERE covenant_id=? AND platform_id=?`,
		covID, "github:b",
	).Scan(&memCount)
	if memCount != 0 {
		t.Errorf("rollback broken: failed approve left %d member rows", memCount)
	}

	// Access request must still be 'pending' (update was inside the same tx).
	req, err := svc.GetAccessRequest(covID, ar2.RequestID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if req.Status != "pending" {
		t.Errorf("status = %q after rollback, want pending", req.Status)
	}
}
