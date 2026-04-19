package covenant

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

// AccessRequest is one row of agent_access_requests (ACR-50 §§2,7). The
// plaintext platform_id never lives on this struct: callers that need to
// correlate requests across covenants compare platform_id_hash.
type AccessRequest struct {
	RequestID        string    `json:"request_id"`
	CovenantID       string    `json:"covenant_id"`
	PlatformIDHash   string    `json:"platform_id_hash"`
	TierID           string    `json:"tier_id"`
	PaymentRef       string    `json:"payment_ref,omitempty"`
	SelfDeclaration  string    `json:"self_declaration,omitempty"`
	Status           string    `json:"status"`
	RejectReason     string    `json:"reject_reason,omitempty"`
	ApproveLogID     string    `json:"approve_log_id,omitempty"`
	RejectLogID      string    `json:"reject_log_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

var (
	// ErrNoAccessRequest is returned by GetAccessRequest / the approve+reject
	// paths when the request_id does not exist or does not belong to the
	// specified covenant.
	ErrNoAccessRequest = errors.New("access request not found")

	// ErrAccessRequestResolved is returned when the caller tries to approve
	// or reject a request that is already approved or rejected. Idempotency
	// is enforced at the service layer rather than with a DB-level trigger.
	ErrAccessRequestResolved = errors.New("access request already resolved")
)

// CreateAccessRequest drives the applicant side of ACR-50 Part 2. It validates
// that the covenant is accepting applications, seals the applicant's
// platform_id, and inserts one pending row. No covenant_members row is
// created here — approval flips that.
//
// The covenant's state must be OPEN. DRAFT rejects (tiers not finalized);
// ACTIVE and beyond reject (the space is in write-mode or sealed).
func (s *Service) CreateAccessRequest(covenantID, platformID, tierID, paymentRef, selfDeclaration string) (*AccessRequest, error) {
	if platformID == "" {
		return nil, errors.New("platform_id is required")
	}
	if tierID == "" {
		return nil, errors.New("tier_id is required")
	}

	cov, err := s.Get(covenantID)
	if err != nil {
		return nil, err
	}
	if cov.State != "OPEN" {
		return nil, fmt.Errorf("covenant is not accepting applications (state=%s)", cov.State)
	}

	var tierExists int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM access_tiers WHERE covenant_id=? AND tier_id=?`,
		covenantID, tierID,
	).Scan(&tierExists); err != nil {
		return nil, fmt.Errorf("lookup tier: %w", err)
	}
	if tierExists == 0 {
		return nil, fmt.Errorf("tier %q not found on covenant", tierID)
	}

	hash := HashPlatformID(platformID)
	var enc []byte
	if s.sealer != nil {
		blob, sealErr := s.sealer.Seal(hash, "platform_id", []byte(platformID))
		if sealErr != nil {
			return nil, fmt.Errorf("seal platform_id: %w", sealErr)
		}
		enc = blob
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	// Upsert platform_identities so that on approve we can resolve the
	// plaintext platform_id (needed for covenant_members FK) by joining on
	// platform_id_hash. INSERT OR IGNORE keeps this idempotent for repeat
	// applicants.
	if err := upsertPlatformIdentity(s.db, s.sealer, platformID, nowStr); err != nil {
		return nil, fmt.Errorf("upsert platform identity: %w", err)
	}

	requestID := id.AccessRequest()
	if _, err := s.db.Exec(`
		INSERT INTO agent_access_requests
			(request_id, covenant_id, platform_id_hash, platform_id_enc,
			 tier_id, payment_ref, self_declaration, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		requestID, covenantID, hash, enc,
		tierID, paymentRef, selfDeclaration, nowStr,
	); err != nil {
		return nil, fmt.Errorf("insert access request: %w", err)
	}

	return &AccessRequest{
		RequestID:       requestID,
		CovenantID:      covenantID,
		PlatformIDHash:  hash,
		TierID:          tierID,
		PaymentRef:      paymentRef,
		SelfDeclaration: selfDeclaration,
		Status:          "pending",
		CreatedAt:       now,
	}, nil
}

// GetAccessRequest loads a single request scoped to covenantID. Scoping
// prevents one covenant's owner from inspecting another's requests by
// guessing request IDs.
func (s *Service) GetAccessRequest(covenantID, requestID string) (*AccessRequest, error) {
	row := s.db.QueryRow(`
		SELECT request_id, covenant_id, platform_id_hash, tier_id,
		       payment_ref, self_declaration, status, reject_reason,
		       approve_log_id, reject_log_id, created_at, resolved_at
		FROM agent_access_requests
		WHERE covenant_id=? AND request_id=?`,
		covenantID, requestID,
	)
	return scanAccessRequest(row)
}

// ApproveAccessRequest flips a pending request to 'approved' and activates
// the applicant as a covenant_members row in one transaction. Plaintext
// platform_id is resolved via platform_identities (upserted at apply time),
// so approve never needs the applicant to retransmit it.
//
// Idempotency: a second call on an already-resolved request returns
// ErrAccessRequestResolved. approveLogID is the audit log entry that
// authorized this approval — stored so the member row is traceable back
// through the hash chain.
func (s *Service) ApproveAccessRequest(covenantID, requestID, approveLogID string) (*Member, error) {
	req, err := s.GetAccessRequest(covenantID, requestID)
	if err != nil {
		return nil, err
	}
	if req.Status != "pending" {
		return nil, ErrAccessRequestResolved
	}

	var plaintext string
	if err := s.db.QueryRow(
		`SELECT platform_id FROM platform_identities WHERE platform_id_hash=?`,
		req.PlatformIDHash,
	).Scan(&plaintext); err != nil {
		return nil, fmt.Errorf("resolve platform_id for approve: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	agentID := id.Agent()

	if _, err := tx.Exec(`
		INSERT INTO covenant_members (covenant_id, platform_id, agent_id, tier_id, is_owner, status, joined_at)
		VALUES (?, ?, ?, ?, 0, 'active', ?)`,
		covenantID, plaintext, agentID, req.TierID, nowStr,
	); err != nil {
		return nil, fmt.Errorf("insert covenant_member on approve: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE agent_access_requests
		SET status='approved', approve_log_id=?, resolved_at=?
		WHERE covenant_id=? AND request_id=?`,
		approveLogID, nowStr, covenantID, requestID,
	); err != nil {
		return nil, fmt.Errorf("flip access request to approved: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &Member{
		CovenantID: covenantID,
		PlatformID: plaintext,
		AgentID:    agentID,
		TierID:     req.TierID,
		Status:     "active",
		JoinedAt:   now,
	}, nil
}

// RejectAccessRequest marks a pending request as rejected with an
// operator-supplied reason. No covenant_members row is created. Idempotent
// by the same rule as approve.
func (s *Service) RejectAccessRequest(covenantID, requestID, reason, rejectLogID string) error {
	req, err := s.GetAccessRequest(covenantID, requestID)
	if err != nil {
		return err
	}
	if req.Status != "pending" {
		return ErrAccessRequestResolved
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.db.Exec(`
		UPDATE agent_access_requests
		SET status='rejected', reject_reason=?, reject_log_id=?, resolved_at=?
		WHERE covenant_id=? AND request_id=?`,
		reason, rejectLogID, now, covenantID, requestID,
	)
	return err
}

// ListPendingAccessRequests returns every pending request for an owner
// review queue, oldest first so the UX can offer FIFO processing.
func (s *Service) ListPendingAccessRequests(covenantID string) ([]AccessRequest, error) {
	rows, err := s.db.Query(`
		SELECT request_id, covenant_id, platform_id_hash, tier_id,
		       payment_ref, self_declaration, status, reject_reason,
		       approve_log_id, reject_log_id, created_at, resolved_at
		FROM agent_access_requests
		WHERE covenant_id=? AND status='pending'
		ORDER BY created_at ASC`,
		covenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccessRequest
	for rows.Next() {
		req, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanAccessRequest works
// on both single-row lookups and iteration.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccessRequest(row rowScanner) (*AccessRequest, error) {
	var (
		req        AccessRequest
		createdAt  string
		resolvedAt sql.NullString
	)
	if err := row.Scan(
		&req.RequestID,
		&req.CovenantID,
		&req.PlatformIDHash,
		&req.TierID,
		&req.PaymentRef,
		&req.SelfDeclaration,
		&req.Status,
		&req.RejectReason,
		&req.ApproveLogID,
		&req.RejectLogID,
		&createdAt,
		&resolvedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoAccessRequest
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		req.CreatedAt = t
	}
	if resolvedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, resolvedAt.String); err == nil {
			req.ResolvedAt = &t
		}
	}
	return &req, nil
}
