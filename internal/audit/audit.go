// Package audit implements ACR-300 v0.2: INSERT-only, hash-chained audit log.
package audit

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

// Entry is a single audit log record.
type Entry struct {
	LogID         string
	CovenantID    string
	Sequence      int
	AgentID       string
	SessionID     string
	ToolName      string
	ToolType      string
	ParamsHash    string
	ParamsPreview map[string]any
	Result        string
	ResultDetail  string
	TokensDelta   int
	CostDelta     int64   // minor units of CostCurrency (e.g. USD cents)
	CostCurrency  string  // ISO 4217 code; defaults to "USD" (ACR-300@2.2)
	NetDelta      float64 // tokens_delta - cost_weight × cost_delta
	StateBefore   string
	StateAfter    string
	Timestamp     time.Time
	PrevLogID     string // empty for genesis
	Hash          string
	SpecVersion   string // ACR-300 spec version, e.g. "ACR-300@2.0"
}

// LogEvent records a single ACP event, auto-computing sequence and hash chain.
func LogEvent(db *sql.DB, e Entry) (*Entry, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Determine sequence and prev_log_id (locked read within transaction)
	var seq int
	var prevID string
	row := tx.QueryRow(`
		SELECT sequence, log_id FROM audit_logs
		WHERE covenant_id=? ORDER BY sequence DESC LIMIT 1`, e.CovenantID)
	switch err := row.Scan(&seq, &prevID); err {
	case nil:
		seq++
	case sql.ErrNoRows:
		seq = 1
		prevID = ""
	default:
		return nil, err
	}

	e.LogID = id.LogID()
	e.Sequence = seq
	e.PrevLogID = prevID
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if e.SpecVersion == "" {
		e.SpecVersion = "ACR-300@2.2"
	}
	if e.CostCurrency == "" {
		e.CostCurrency = "USD"
	}

	// Compute params hash + preview
	paramsJSON, _ := json.Marshal(e.ParamsPreview)
	e.ParamsHash = fmt.Sprintf("%x", sha256.Sum256(paramsJSON))

	// ACR-300 v0.2 hash
	e.Hash = computeHash(e)

	preview, _ := json.Marshal(e.ParamsPreview)

	_, err = tx.Exec(`
		INSERT INTO audit_logs
		  (log_id, covenant_id, sequence, agent_id, session_id,
		   tool_name, tool_type, params_hash, params_preview, result,
		   result_detail, tokens_delta, cost_delta, cost_currency, net_delta,
		   state_before, state_after, timestamp, prev_log_id, hash, spec_version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.LogID, e.CovenantID, e.Sequence, e.AgentID, e.SessionID,
		e.ToolName, e.ToolType, e.ParamsHash, string(preview), e.Result,
		e.ResultDetail, e.TokensDelta, e.CostDelta, e.CostCurrency, e.NetDelta,
		e.StateBefore, e.StateAfter,
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		nullableStr(e.PrevLogID), e.Hash, e.SpecVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("insert audit log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("audit commit: %w", err)
	}
	return &e, nil
}

// VerifyChain re-derives every hash in the chain and checks prev_log_id links.
// Returns (valid, violations).
func VerifyChain(db *sql.DB, covenantID string) (bool, []string) {
	rows, err := db.Query(`
		SELECT log_id, sequence, agent_id, session_id, tool_name, tool_type,
		       params_hash, result, result_detail, tokens_delta, cost_delta, cost_currency, net_delta,
		       state_before, state_after, timestamp, prev_log_id, hash, spec_version
		FROM audit_logs WHERE covenant_id=? ORDER BY sequence`, covenantID)
	if err != nil {
		return false, []string{err.Error()}
	}
	defer rows.Close()

	var violations []string
	var prevID string

	for rows.Next() {
		var e Entry
		var tsStr string
		var prevIDNull sql.NullString
		e.CovenantID = covenantID

		err := rows.Scan(
			&e.LogID, &e.Sequence, &e.AgentID, &e.SessionID, &e.ToolName, &e.ToolType,
			&e.ParamsHash, &e.Result, &e.ResultDetail, &e.TokensDelta, &e.CostDelta, &e.CostCurrency, &e.NetDelta,
			&e.StateBefore, &e.StateAfter, &tsStr, &prevIDNull, &e.Hash, &e.SpecVersion,
		)
		if err != nil {
			violations = append(violations, fmt.Sprintf("seq %d: scan error: %v", e.Sequence, err))
			continue
		}
		if prevIDNull.Valid {
			e.PrevLogID = prevIDNull.String
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)

		// Check chain link
		if e.PrevLogID != prevID {
			violations = append(violations,
				fmt.Sprintf("seq %d: prev_log_id mismatch (got %q, want %q)", e.Sequence, e.PrevLogID, prevID))
		}

		// Re-derive hash
		if want := computeHash(e); want != e.Hash {
			violations = append(violations,
				fmt.Sprintf("seq %d: hash mismatch (stored %s…, computed %s…)", e.Sequence, e.Hash[:8], want[:8]))
		}

		prevID = e.LogID
	}
	return len(violations) == 0, violations
}

// computeHash implements the ACR-300 audit-chain hash. Format depends on
// spec_version so that a rewritten chain can still verify older rows:
//
//   - ACR-300@2.0 — CostDelta formatted %.8f (pre-integer-cents schema).
//   - ACR-300@2.1 — CostDelta formatted %d (INTEGER minor units).
//   - ACR-300@2.2 — adds CostCurrency (ISO 4217) as a hash component so a
//     10-cent USD charge cannot collide with a 10-minor-unit EUR charge.
//
// NetDelta continues to use %.8f: cost_weight × cost_delta can be fractional
// even when cost_delta itself is integer.
func computeHash(e Entry) string {
	prevPart := "GENESIS"
	if e.PrevLogID != "" {
		prevPart = e.PrevLogID
	}
	var costField string
	if e.SpecVersion == "ACR-300@2.0" {
		costField = fmt.Sprintf("%.8f", float64(e.CostDelta))
	} else {
		costField = fmt.Sprintf("%d", e.CostDelta)
	}
	components := []string{
		prevPart,
		e.LogID,
		e.CovenantID,
		fmt.Sprintf("%d", e.Sequence),
		e.AgentID,
		e.ToolName,
		e.Result,
		fmt.Sprintf("%d", e.TokensDelta),
		costField,
	}
	// Currency entered the hash payload at 2.2. New versions that keep the
	// field must be listed here explicitly (lexical >= breaks at 2.10).
	switch e.SpecVersion {
	case "ACR-300@2.2":
		components = append(components, e.CostCurrency)
	}
	components = append(components,
		fmt.Sprintf("%.8f", e.NetDelta),
		e.StateAfter,
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.ParamsHash,
		e.SpecVersion,
	)
	payload := strings.Join(components, "|")
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h)
}

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
