package ratelimit

import (
	"database/sql"
	"fmt"
	"sort"
)

// ConcentrationEntry describes one agent's share of the confirmed token pool
// on a covenant. Share is expressed as a percentage (0–100) of Total.
type ConcentrationEntry struct {
	AgentID string  `json:"agent_id"`
	Tokens  int     `json:"tokens"`
	Share   float64 `json:"share_pct"`
}

// ConcentrationReport is the output of CheckConcentration: a deterministic,
// sorted breakdown plus the subset of entries that breached the policy's
// concentration_warn_pct. Emptied lists are returned as nil for easy JSON
// handling by callers.
type ConcentrationReport struct {
	Threshold float64              `json:"threshold_pct"`
	Total     int                  `json:"total_tokens"`
	Entries   []ConcentrationEntry `json:"entries,omitempty"`
	Warnings  []ConcentrationEntry `json:"warnings,omitempty"`
}

// CheckConcentration computes, but does not write, the token concentration
// picture for one covenant. It is ACR-20 Part 4 Layer 5 — a *signal* surfaced
// in receipts and owner query tools, never a hard gate (Trust Layer 1 lets the
// owner decide what to do about it).
//
// Semantics:
//   - Sums confirmed ledger deltas per agent; agents with a net ≤ 0 are
//     excluded from both Total and Entries (they hold no share to concentrate).
//   - Share = agent_tokens / Total × 100. Total == 0 → empty report.
//   - Threshold 0 means "concentration warnings disabled" (matches the default
//     anti_gaming_policies row from LoadPolicy); Warnings is nil in that case.
//   - Entries are sorted by tokens descending, then by agent_id ascending for
//     stable output across calls.
func CheckConcentration(db *sql.DB, covenantID string) (ConcentrationReport, error) {
	policy, err := LoadPolicy(db, covenantID)
	if err != nil {
		return ConcentrationReport{}, fmt.Errorf("ratelimit: load policy: %w", err)
	}
	report := ConcentrationReport{Threshold: policy.ConcentrationWarnPct}

	rows, err := db.Query(`
		SELECT agent_id, COALESCE(SUM(delta), 0) AS tokens
		FROM token_ledger
		WHERE covenant_id = ? AND status = 'confirmed'
		GROUP BY agent_id`, covenantID)
	if err != nil {
		return report, fmt.Errorf("ratelimit: concentration query: %w", err)
	}
	defer rows.Close()

	var entries []ConcentrationEntry
	total := 0
	for rows.Next() {
		var agentID string
		var tokens int
		if err := rows.Scan(&agentID, &tokens); err != nil {
			return report, fmt.Errorf("ratelimit: concentration scan: %w", err)
		}
		if tokens <= 0 {
			continue
		}
		entries = append(entries, ConcentrationEntry{AgentID: agentID, Tokens: tokens})
		total += tokens
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("ratelimit: concentration rows: %w", err)
	}

	if total == 0 {
		return report, nil
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Tokens != entries[j].Tokens {
			return entries[i].Tokens > entries[j].Tokens
		}
		return entries[i].AgentID < entries[j].AgentID
	})
	for i := range entries {
		entries[i].Share = float64(entries[i].Tokens) / float64(total) * 100
	}
	report.Total = total
	report.Entries = entries

	if policy.ConcentrationWarnPct <= 0 {
		return report, nil
	}
	var warnings []ConcentrationEntry
	for _, e := range entries {
		if e.Share > policy.ConcentrationWarnPct {
			warnings = append(warnings, e)
		}
	}
	report.Warnings = warnings
	return report, nil
}
