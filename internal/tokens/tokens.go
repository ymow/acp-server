// Package tokens implements ACR-20: Ink Token ledger.
package tokens

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

const baseTokensPerUnit = 1 // 1 token per unit (word/line/bar per space_type), MVP baseline

// Calculate derives confirmed token count from unit_count (space_type-aware:
// words for book, lines for code, bars for music, etc.), tier multiplier,
// and acceptance ratio.
func Calculate(unitCount int, tierMultiplier, acceptanceRatio float64) int {
	raw := float64(unitCount) * tierMultiplier * acceptanceRatio
	return int(math.Round(raw))
}

// ConfirmContribution writes a confirmed token entry to the ledger.
// Called in Step 6 (apply side effects) after the audit log is committed.
func ConfirmContribution(db *sql.DB, covenantID, agentID, logID, sourceRef string, delta int) error {
	// Current balance for this agent
	var current int
	row := db.QueryRow(`
		SELECT COALESCE(SUM(delta), 0) FROM token_ledger
		WHERE covenant_id=? AND agent_id=? AND status='confirmed'`,
		covenantID, agentID)
	if err := row.Scan(&current); err != nil {
		return err
	}

	_, err := db.Exec(`
		INSERT INTO token_ledger (id, covenant_id, agent_id, delta, balance_after,
		                          source_type, source_ref, log_id, status)
		VALUES (?, ?, ?, ?, ?, 'passage', ?, ?, 'confirmed')`,
		id.LedgerID(), covenantID, agentID, delta, current+delta, sourceRef, logID,
	)
	return err
}

// Balance returns the confirmed token balance for an agent.
func Balance(db *sql.DB, covenantID, agentID string) (int, error) {
	var total int
	err := db.QueryRow(`
		SELECT COALESCE(SUM(delta), 0) FROM token_ledger
		WHERE covenant_id=? AND agent_id=? AND status='confirmed'`,
		covenantID, agentID).Scan(&total)
	return total, err
}

// TotalByAgent returns a map of agentID → confirmed token balance for all agents.
func TotalByAgent(db *sql.DB, covenantID string) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT agent_id, COALESCE(SUM(delta), 0)
		FROM token_ledger WHERE covenant_id=? AND status='confirmed'
		GROUP BY agent_id`, covenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var agentID string
		var total int
		if err := rows.Scan(&agentID, &total); err != nil {
			return nil, err
		}
		out[agentID] = total
	}
	return out, rows.Err()
}

// CreatePending registers a draft pending approval.
func CreatePending(db *sql.DB, covenantID, agentID, draftID string) error {
	now := time.Now().UTC()
	_, err := db.Exec(`
		INSERT INTO pending_tokens (draft_id, covenant_id, agent_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		draftID, covenantID, agentID,
		now.Format(time.RFC3339Nano),
		now.Add(30*24*time.Hour).Format(time.RFC3339Nano),
	)
	return err
}

// ClaimPending returns the agentID for a draft and deletes the pending record.
func ClaimPending(db *sql.DB, covenantID, draftID string) (string, error) {
	var agentID string
	err := db.QueryRow(`SELECT agent_id FROM pending_tokens WHERE covenant_id=? AND draft_id=?`,
		covenantID, draftID).Scan(&agentID)
	if err != nil {
		return "", fmt.Errorf("draft %q not found: %w", draftID, err)
	}
	_, err = db.Exec(`DELETE FROM pending_tokens WHERE draft_id=?`, draftID)
	return agentID, err
}
