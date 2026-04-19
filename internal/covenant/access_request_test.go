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
	if err := svc.AddTier(cov.CovenantID, "tier_a", "Tier A", 1.0, nil); err != nil {
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
	svc.AddTier(covA.CovenantID, "tier_a", "A", 1.0, nil)
	svc.Transition(covA.CovenantID, "OPEN")

	covB, _, _ := svc.Create("B", "code", "github:ownerB")
	svc.AddTier(covB.CovenantID, "tier_a", "A", 1.0, nil)
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
	svc.AddTier(covB.CovenantID, "tier_a", "A", 1.0, nil)
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
