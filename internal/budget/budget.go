// Package budget implements ACR-60 MVP: global budget gate with authorize-then-settle.
// Phase 2 WI6 (Option A): CheckAndReserve checks only (no deduction); RecordSpend settles.
//
// All monetary values are minor units (int64) of State.Currency (ISO 4217).
// execution.Run enforces that a charge's cost_currency matches the covenant's
// budget_currency — this package never has to mix currencies internally.
package budget

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

type State struct {
	CovenantID  string
	BudgetLimit int64  // minor units of Currency
	BudgetSpent int64  // minor units of Currency
	Currency    string // ISO 4217, mirrors covenants.budget_currency
}

// Remaining returns minor units left in the budget, or -1 when unlimited.
func (s State) Remaining() int64 {
	if s.BudgetLimit == 0 {
		return -1
	}
	return s.BudgetLimit - s.BudgetSpent
}

// EnsureCounter creates (or idempotently resets) a budget_counter row.
// limit is in minor units of currency; 0 means unlimited. currency defaults
// to "USD" when empty. Calling EnsureCounter with a different currency on an
// existing row is a no-op — change the covenant's budget_currency instead.
func EnsureCounter(db *sql.DB, covenantID string, limit int64, currency string) error {
	if currency == "" {
		currency = "USD"
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO budget_counters (covenant_id, budget_limit, budget_spent, currency, updated_at)
		VALUES (?, ?, 0, ?, ?)`,
		covenantID, limit, currency, time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

// CheckAndReserve checks whether sufficient budget remains for estimatedCost.
// Phase 2 WI6 (Option A): does NOT deduct from budget_counters; creates a reservation record.
// Returns (reservationID, error). reservationID is "" when estimatedCost <= 0 or no counter exists.
func CheckAndReserve(db *sql.DB, covenantID string, estimatedCost int64) (string, error) {
	if estimatedCost <= 0 {
		return "", nil
	}

	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Check remaining budget
	var limit, spent int64
	err = tx.QueryRow(`SELECT budget_limit, budget_spent FROM budget_counters WHERE covenant_id = ?`,
		covenantID).Scan(&limit, &spent)
	if err == sql.ErrNoRows {
		// No counter = unlimited; still create a reservation for audit purposes
		if err2 := tx.Commit(); err2 != nil {
			return "", err2
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if limit > 0 && (limit-spent) < estimatedCost {
		return "", fmt.Errorf("budget exhausted: remaining=%d required=%d (cents)", limit-spent, estimatedCost)
	}

	// Create reservation record (audit_log_id filled in later by RecordSpend)
	reservationID := id.LogID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`
		INSERT INTO budget_reservations (id, covenant_id, audit_log_id, amount, status, created_at)
		VALUES (?, ?, '', ?, 'reserved', ?)`,
		reservationID, covenantID, estimatedCost, now,
	)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return reservationID, nil
}

// RecordSpend deducts estimatedCost from budget_counters and settles the reservation.
// Phase 2 WI6: this is the actual deduction point (replaces CheckAndReserve's old role).
// auditLogID is stored in the reservation for traceability.
func RecordSpend(db *sql.DB, covenantID string, estimatedCost int64, reservationID, auditLogID string) error {
	if estimatedCost <= 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		UPDATE budget_counters
		SET budget_spent = budget_spent + ?, updated_at = ?
		WHERE covenant_id = ?`,
		estimatedCost, now, covenantID,
	)
	if err != nil {
		return err
	}
	// Settle the reservation record if one was created.
	if reservationID != "" {
		db.Exec(`
			UPDATE budget_reservations SET status='settled', audit_log_id=?
			WHERE id=?`,
			auditLogID, reservationID,
		)
	}
	return nil
}

// Release decrements budget_spent by amount (used by reject_draft to refund a settled spend).
func Release(db *sql.DB, covenantID string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	_, err := db.Exec(`
		UPDATE budget_counters
		SET budget_spent = MAX(0, budget_spent - ?), updated_at = ?
		WHERE covenant_id = ?`,
		amount, time.Now().UTC().Format(time.RFC3339Nano), covenantID,
	)
	return err
}

// ReleaseReservation marks a reservation as released when tool execution fails.
// No change to budget_counters (CheckAndReserve never deducted them).
func ReleaseReservation(db *sql.DB, reservationID string) {
	if reservationID == "" {
		return
	}
	db.Exec(`UPDATE budget_reservations SET status='released' WHERE id=?`, reservationID)
}

// GetState returns the current budget state for a covenant.
func GetState(db *sql.DB, covenantID string) (State, error) {
	var s State
	s.CovenantID = covenantID
	err := db.QueryRow(`SELECT budget_limit, budget_spent, currency FROM budget_counters WHERE covenant_id=?`,
		covenantID).Scan(&s.BudgetLimit, &s.BudgetSpent, &s.Currency)
	if err == sql.ErrNoRows {
		return s, nil // no counter = unlimited
	}
	return s, err
}

// RebuildFromAuditLog reconstructs budget_counters.budget_spent from the
// durable audit log chain. Required by ACP_Implementation_Spec_MVP Part 8:
// when the runtime counter cache (e.g. Redis in Phase 2) is cold or has
// drifted, the ledger must be the source of truth.
//
// The reconstruction sums cost_delta from successful audit_log entries and
// subtracts refunds — entries whose corresponding token_ledger row has been
// reversed by reject_draft. A naive SUM(cost_delta WHERE result='success')
// would over-count in that case, since reject_draft records itself as its
// own cost_delta=0 success entry rather than as a negative cost_delta on
// the original row.
//
// Returns the reconstructed budget_spent total (USD cents). Errors if the
// covenant has no budget_counter row (caller must EnsureCounter first) — a
// missing row signals misuse, not zero spend.
func RebuildFromAuditLog(db *sql.DB, covenantID string) (int64, error) {
	var total int64
	err := db.QueryRow(`
		SELECT COALESCE(SUM(a.cost_delta), 0)
		FROM audit_logs a
		LEFT JOIN token_ledger t ON t.log_id = a.log_id
		WHERE a.covenant_id = ?
		  AND a.result = 'success'
		  AND (t.status IS NULL OR t.status NOT IN ('rejected', 'reversed'))`,
		covenantID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("rebuild sum: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(`
		UPDATE budget_counters SET budget_spent=?, updated_at=?
		WHERE covenant_id=?`,
		total, now, covenantID,
	)
	if err != nil {
		return 0, fmt.Errorf("rebuild write: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, fmt.Errorf("rebuild: no budget_counter row for covenant %q (call EnsureCounter first)", covenantID)
	}
	return total, nil
}
