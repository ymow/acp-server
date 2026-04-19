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
