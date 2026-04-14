// Package budget implements ACR-60 MVP: global budget gate with atomic check-and-spend.
package budget

import (
	"database/sql"
	"fmt"
	"time"
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

// CheckAndReserve atomically checks and reserves budget for a single execution.
// It uses a single UPDATE statement so two concurrent requests cannot both pass
// when only one unit of budget remains (no SELECT-then-UPDATE race).
// SetMaxOpenConns(1) in db.Open ensures SQLite single-writer ordering.
func CheckAndReserve(db *sql.DB, covenantID string, estimatedCost float64) error {
	if estimatedCost <= 0 {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`
		UPDATE budget_counters
		SET budget_spent = budget_spent + ?,
		    updated_at   = ?
		WHERE covenant_id = ?
		  AND (budget_limit = 0 OR (budget_limit - budget_spent) >= ?)`,
		estimatedCost, now, covenantID, estimatedCost,
	)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return nil // reserved
	}

	// rows affected == 0: either no counter row (unlimited) or budget exhausted
	var limit, spent float64
	err = db.QueryRow(`SELECT budget_limit, budget_spent FROM budget_counters WHERE covenant_id = ?`,
		covenantID).Scan(&limit, &spent)
	if err == sql.ErrNoRows {
		return nil // no counter = unlimited
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("budget exhausted: remaining=%.8f required=%.8f", limit-spent, estimatedCost)
}

// RecordSpend decrements the remaining budget after a successful execution (Step 7).
func RecordSpend(db *sql.DB, covenantID string, actualCost float64) error {
	if actualCost <= 0 {
		return nil
	}
	_, err := db.Exec(`
		UPDATE budget_counters
		SET budget_spent = budget_spent + ?, updated_at = ?
		WHERE covenant_id = ?`,
		actualCost, time.Now().UTC().Format(time.RFC3339Nano), covenantID,
	)
	return err
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
