// Package budget implements ACR-60 MVP: global budget gate with authorize-then-settle.
// Phase 2 WI6 (Option A): CheckAndReserve checks only (no deduction); RecordSpend settles.
package budget

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

type State struct {
	CovenantID  string
	BudgetLimit float64
	BudgetSpent float64
}

func (s State) Remaining() float64 {
	if s.BudgetLimit == 0 {
		return -1 // unlimited
	}
	return s.BudgetLimit - s.BudgetSpent
}

// EnsureCounter creates a budget_counter row if one doesn't exist yet.
func EnsureCounter(db *sql.DB, covenantID string, limit float64) error {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO budget_counters (covenant_id, budget_limit, budget_spent, updated_at)
		VALUES (?, ?, 0, ?)`,
		covenantID, limit, time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

// CheckAndReserve checks whether sufficient budget remains for estimatedCost.
// Phase 2 WI6 (Option A): does NOT deduct from budget_counters; creates a reservation record.
// Returns (reservationID, error). reservationID is "" when estimatedCost <= 0 or no counter exists.
func CheckAndReserve(db *sql.DB, covenantID string, estimatedCost float64) (string, error) {
	if estimatedCost <= 0 {
		return "", nil
	}

	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Check remaining budget
	var limit, spent float64
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
		return "", fmt.Errorf("budget exhausted: remaining=%.8f required=%.8f", limit-spent, estimatedCost)
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
func RecordSpend(db *sql.DB, covenantID string, estimatedCost float64, reservationID, auditLogID string) error {
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
func Release(db *sql.DB, covenantID string, amount float64) error {
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
	err := db.QueryRow(`SELECT budget_limit, budget_spent FROM budget_counters WHERE covenant_id=?`,
		covenantID).Scan(&s.BudgetLimit, &s.BudgetSpent)
	if err == sql.ErrNoRows {
		return s, nil // no counter = unlimited
	}
	return s, err
}
