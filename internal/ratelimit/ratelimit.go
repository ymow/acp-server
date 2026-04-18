// Package ratelimit implements ACR-20 Part 4 Layer 2: per-covenant / per-agent
// hourly call ceilings for clause tools. Phase 4.1 writes every increment to
// the global bucket (tool_name="*"); the tool_name column is kept in the
// schema so later per-tool policies slot in without migration.
//
// Trust-model caveat: rate limits apply to clause tools only. Admin and query
// tools are exempt by design — ACP Trust Layer 1 assumes the owner is
// trusted; limiting an actor who already controls the server would be
// theatre. See ACR-20 Part 4 notes.
package ratelimit

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// GlobalBucket is the tool_name sentinel used when rate limits apply across
// all clause tools for an (agent, covenant) pair. Per-tool rows can still be
// written directly by future policies; CheckAndIncrement only touches this
// sentinel in Phase 4.1.
const GlobalBucket = "*"

// cleanupProbability is the per-call chance of pruning rows older than 24h.
// 1% is a floor that bounds table growth without needing a background job.
const cleanupProbability = 0.01

// ErrRateLimitExceeded is returned when the caller has already consumed the
// hourly quota for their (covenant, agent) bucket. Errors returned from
// CheckAndIncrement wrap this sentinel — callers can errors.Is() to detect it.
var ErrRateLimitExceeded = errors.New("rate limit exceeded")

// Policy mirrors AntiGamingPolicy from ACR-20 Part 4. Zero value means no
// policy configured, which is equivalent to all gates disabled.
type Policy struct {
	CovenantID            string
	RateLimitPerHour      int
	SimilarityThreshold   float64
	MinWordCount          int
	ConcentrationWarnPct  float64
}

// LoadPolicy reads the anti_gaming_policies row for a covenant. A missing row
// is not an error: the zero-value Policy is returned, which disables every
// gate (rate_limit_per_hour=0 → unlimited).
func LoadPolicy(db *sql.DB, covenantID string) (Policy, error) {
	p := Policy{CovenantID: covenantID}
	err := db.QueryRow(`
		SELECT rate_limit_per_hour, similarity_threshold, min_word_count, concentration_warn_pct
		FROM anti_gaming_policies WHERE covenant_id = ?`, covenantID,
	).Scan(&p.RateLimitPerHour, &p.SimilarityThreshold, &p.MinWordCount, &p.ConcentrationWarnPct)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// UpsertPolicy writes the full policy row. Intended to be called from
// admin tools (configure_anti_gaming) that have already verified the caller
// is the covenant owner.
func UpsertPolicy(db *sql.DB, p Policy) error {
	if p.CovenantID == "" {
		return errors.New("ratelimit: policy missing covenant_id")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		INSERT INTO anti_gaming_policies
			(covenant_id, rate_limit_per_hour, similarity_threshold, min_word_count, concentration_warn_pct, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(covenant_id) DO UPDATE SET
			rate_limit_per_hour    = excluded.rate_limit_per_hour,
			similarity_threshold   = excluded.similarity_threshold,
			min_word_count         = excluded.min_word_count,
			concentration_warn_pct = excluded.concentration_warn_pct,
			updated_at             = excluded.updated_at`,
		p.CovenantID, p.RateLimitPerHour, p.SimilarityThreshold,
		p.MinWordCount, p.ConcentrationWarnPct, now,
	)
	return err
}

// CheckAndIncrement atomically enforces the rate-limit gate for a clause tool
// call. Behavior:
//
//   - limit == 0            → unlimited; returns nil without writing a row.
//   - call_count < limit    → counter incremented, returns nil.
//   - call_count >= limit   → returns ErrRateLimitExceeded (wrapped with
//     diagnostic context so the audit log captures the triggering window).
//
// toolName is accepted for API stability but always aggregated into
// GlobalBucket during Phase 4.1. Future per-tool policies will branch here.
func CheckAndIncrement(db *sql.DB, covenantID, agentID, toolName string) error {
	_ = toolName // reserved for per-tool buckets; not used in Phase 4.1

	policy, err := LoadPolicy(db, covenantID)
	if err != nil {
		return fmt.Errorf("ratelimit: load policy: %w", err)
	}
	if policy.RateLimitPerHour <= 0 {
		return nil // unlimited
	}

	window := currentWindow()

	// Atomic insert-or-increment-if-under. RETURNING call_count ensures we
	// only consider the operation successful when SQLite actually touched a
	// row; an empty result means the ON CONFLICT WHERE clause rejected the
	// update (bucket already at limit).
	row := db.QueryRow(`
		INSERT INTO rate_limit_counters (covenant_id, agent_id, tool_name, window_start, call_count)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(covenant_id, agent_id, tool_name, window_start)
		DO UPDATE SET call_count = call_count + 1
		WHERE call_count < ?
		RETURNING call_count`,
		covenantID, agentID, GlobalBucket, window, policy.RateLimitPerHour,
	)

	var count int
	if err := row.Scan(&count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %d calls/hour (window %s)",
				ErrRateLimitExceeded, policy.RateLimitPerHour, window)
		}
		return fmt.Errorf("ratelimit: increment: %w", err)
	}

	maybeCleanup(db)
	return nil
}

// currentWindow returns the RFC3339 UTC timestamp of the start of the current
// hour. Fixed-window semantics are intentional for MVP simplicity; sliding
// windows can be added without schema changes when needed.
func currentWindow() string {
	return time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
}

// maybeCleanup prunes counter rows older than 24 hours with small probability.
// Opportunistic cleanup avoids a separate daemon; worst-case table size stays
// bounded at roughly 24 × (distinct covenant × agent × tool) rows.
func maybeCleanup(db *sql.DB) {
	if rand.Float64() >= cleanupProbability {
		return
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	db.Exec(`DELETE FROM rate_limit_counters WHERE window_start < ?`, cutoff)
}
