package tokens

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

// Snapshot is the in-memory representation of a token_snapshots row.
// Populated at ACTIVE → LOCKED transition; immutable afterwards.
type Snapshot struct {
	ID           string    `json:"id"`
	CovenantID   string    `json:"covenant_id"`
	AgentID      string    `json:"agent_id"`
	AgentTokens  int       `json:"agent_tokens"`
	CostTokens   int64     `json:"cost_tokens"` // minor units of covenant.budget_currency
	SnappedAt    time.Time `json:"snapped_at"`
	SnapshotHash string    `json:"snapshot_hash"`
}

// CaptureSnapshot writes one token_snapshots row per distinct agent that has
// any confirmed ledger entry or logged cost on the covenant. Safe to call
// more than once per covenant (each run creates fresh rows; the DB does not
// enforce uniqueness here). Returns the rows it wrote so callers can surface
// the snapshot in settlement receipts.
//
// ACR-20 Part 5: each row carries SHA-256(covenant_id|agent_id|agent_tokens|
// cost_tokens|snapped_at) so a tampered snapshot can be detected against the
// recomputed hash.
func CaptureSnapshot(db *sql.DB, covenantID string) ([]Snapshot, error) {
	rows, err := db.Query(`
		SELECT m.agent_id,
		       COALESCE(SUM(CASE WHEN l.status='confirmed' THEN l.delta ELSE 0 END), 0) AS tokens,
		       COALESCE((SELECT SUM(cost_delta) FROM audit_logs
		                 WHERE covenant_id = m.covenant_id
		                   AND agent_id = m.agent_id
		                   AND result='success'), 0) AS cost
		FROM covenant_members m
		LEFT JOIN token_ledger l
		  ON l.covenant_id = m.covenant_id AND l.agent_id = m.agent_id
		WHERE m.covenant_id = ?
		GROUP BY m.agent_id`, covenantID)
	if err != nil {
		return nil, fmt.Errorf("snapshot query: %w", err)
	}
	defer rows.Close()

	type row struct {
		AgentID string
		Tokens  int
		Cost    int64
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.AgentID, &r.Tokens, &r.Cost); err != nil {
			return nil, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	snapped := now.Format(time.RFC3339Nano)
	out := make([]Snapshot, 0, len(pending))
	for _, r := range pending {
		if r.Tokens == 0 && r.Cost == 0 {
			continue
		}
		s := Snapshot{
			ID:          id.LedgerID(),
			CovenantID:  covenantID,
			AgentID:     r.AgentID,
			AgentTokens: r.Tokens,
			CostTokens:  r.Cost,
			SnappedAt:   now,
		}
		s.SnapshotHash = snapshotHash(s.CovenantID, s.AgentID, s.AgentTokens, s.CostTokens, snapped)

		_, err := db.Exec(`
			INSERT INTO token_snapshots (id, covenant_id, agent_id, agent_tokens, cost_tokens, snapped_at, snapshot_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.CovenantID, s.AgentID, s.AgentTokens, s.CostTokens, snapped, s.SnapshotHash,
		)
		if err != nil {
			return nil, fmt.Errorf("snapshot insert: %w", err)
		}
		out = append(out, s)
	}
	return out, nil
}

// VerifySnapshot recomputes the hash from the stored fields and reports any
// mismatch. Used by audit verifiers and by tests to prove the hash function
// is load-bearing.
func VerifySnapshot(s Snapshot) bool {
	want := snapshotHash(s.CovenantID, s.AgentID, s.AgentTokens, s.CostTokens,
		s.SnappedAt.Format(time.RFC3339Nano))
	return want == s.SnapshotHash
}

func snapshotHash(covenantID, agentID string, tokens int, cost int64, snappedAt string) string {
	payload := fmt.Sprintf("%s|%s|%d|%d|%s", covenantID, agentID, tokens, cost, snappedAt)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
