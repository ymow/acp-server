// Package sessions implements REVIEW-14: session token rotation with 30s grace period.
package sessions

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

const GracePeriod = 30 * time.Second

// Issue creates a new active session token for an agent.
// Returns the raw token (shown once; never stored).
func Issue(db *sql.DB, agentID, covenantID string) (string, error) {
	raw := randomHex(32)
	hash := hashToken(raw)
	now := time.Now().UTC()

	_, err := db.Exec(`
		INSERT INTO session_tokens (token_id, agent_id, covenant_id, token_hash, status, created_at)
		VALUES (?, ?, ?, ?, 'active', ?)`,
		id.SessionID(), agentID, covenantID, hash, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return "", err
	}
	return raw, nil
}

// Rotate moves the current active token to grace state and issues a new one.
// Returns (newRawToken, warningMessage).
func Rotate(db *sql.DB, agentID, covenantID string) (string, string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	_, err = tx.Exec(`
		UPDATE session_tokens SET status='grace', rotated_at=?
		WHERE agent_id=? AND covenant_id=? AND status='active'`,
		now.Format(time.RFC3339Nano), agentID, covenantID,
	)
	if err != nil {
		return "", "", err
	}

	raw := randomHex(32)
	hash := hashToken(raw)
	_, err = tx.Exec(`
		INSERT INTO session_tokens (token_id, agent_id, covenant_id, token_hash, status, created_at)
		VALUES (?, ?, ?, ?, 'active', ?)`,
		id.SessionID(), agentID, covenantID, hash, now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return "", "", err
	}

	if err := tx.Commit(); err != nil {
		return "", "", err
	}

	warning := fmt.Sprintf("Token rotated. Old token valid for %ds. Update your session token immediately.",
		int(GracePeriod.Seconds()))
	return raw, warning, nil
}

// Validate checks if a raw token is valid for the given agent/covenant.
// Returns (isValid, inGrace).
func Validate(db *sql.DB, rawToken, agentID, covenantID string) (bool, bool) {
	hash := hashToken(rawToken)

	var status string
	var rotatedStr sql.NullString
	err := db.QueryRow(`
		SELECT status, rotated_at FROM session_tokens
		WHERE token_hash=? AND agent_id=? AND covenant_id=?`,
		hash, agentID, covenantID,
	).Scan(&status, &rotatedStr)
	if err != nil {
		return false, false
	}

	switch status {
	case "expired":
		return false, false
	case "active":
		return true, false
	case "grace":
		if !rotatedStr.Valid {
			return false, false
		}
		rotated, err := time.Parse(time.RFC3339Nano, rotatedStr.String)
		if err != nil {
			return false, false
		}
		if time.Since(rotated) > GracePeriod {
			// Lazily mark expired
			db.Exec(`UPDATE session_tokens SET status='expired' WHERE token_hash=?`, hash)
			return false, false
		}
		return true, true
	default:
		return false, false
	}
}

// ValidateForCovenant checks if a raw token is valid for any agent in the given covenant.
// Used for endpoints that require any covenant participant (e.g. /audit).
func ValidateForCovenant(db *sql.DB, rawToken, covenantID string) bool {
	hash := hashToken(rawToken)
	var status string
	var rotatedStr sql.NullString
	err := db.QueryRow(`
		SELECT status, rotated_at FROM session_tokens
		WHERE token_hash=? AND covenant_id=?`,
		hash, covenantID,
	).Scan(&status, &rotatedStr)
	if err != nil {
		return false
	}
	switch status {
	case "active":
		return true
	case "grace":
		if !rotatedStr.Valid {
			return false
		}
		rotated, err := time.Parse(time.RFC3339Nano, rotatedStr.String)
		if err != nil {
			return false
		}
		return time.Since(rotated) <= GracePeriod
	default:
		return false
	}
}

// ExpireGraceTokens sweeps grace tokens past their window. Safe to call periodically.
func ExpireGraceTokens(db *sql.DB) (int64, error) {
	cutoff := time.Now().UTC().Add(-GracePeriod).Format(time.RFC3339Nano)
	res, err := db.Exec(`
		UPDATE session_tokens SET status='expired'
		WHERE status='grace' AND rotated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
