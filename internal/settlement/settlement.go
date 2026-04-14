// Package settlement implements ACR-100 MVP: SettlementOutput generation.
package settlement

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/id"
	"github.com/inkmesh/acp-server/internal/tokens"
)

// Distribution holds one agent's settlement share.
type Distribution struct {
	AgentID       string  `json:"agent_id"`
	InkTokens     int     `json:"ink_tokens"`
	ShareOfPool   float64 `json:"share_of_pool"`   // fraction of contributor pool
	FinalSharePct float64 `json:"final_share_pct"` // final % of total revenue
}

// Output is the full settlement report.
type Output struct {
	OutputID          string
	CovenantID        string
	TriggerLogID      string
	TriggerLogHash    string
	TotalTokens       int
	OwnerSharePct     float64
	PlatformSharePct  float64
	ContributorPoolPct float64
	Distributions     []Distribution
	GeneratedAt       time.Time
}

// Generate computes the SettlementOutput for a LOCKED covenant.
func Generate(db *sql.DB, covSvc *covenant.Service, covenantID, triggerLogID, triggerLogHash string) (*Output, error) {
	cov, err := covSvc.Get(covenantID)
	if err != nil {
		return nil, err
	}
	if cov.State != "LOCKED" && cov.State != "ACTIVE" {
		return nil, fmt.Errorf("settlement requires LOCKED or ACTIVE state, got %s", cov.State)
	}

	balances, err := tokens.TotalByAgent(db, covenantID)
	if err != nil {
		return nil, err
	}

	total := 0
	for _, v := range balances {
		total += v
	}

	var dists []Distribution
	for agentID, bal := range balances {
		shareOfPool := 0.0
		if total > 0 {
			shareOfPool = float64(bal) / float64(total)
		}
		finalSharePct := shareOfPool * cov.ContributorPoolPct
		dists = append(dists, Distribution{
			AgentID:       agentID,
			InkTokens:     bal,
			ShareOfPool:   shareOfPool,
			FinalSharePct: finalSharePct,
		})
	}

	out := &Output{
		OutputID:           id.SettlementID(),
		CovenantID:         covenantID,
		TriggerLogID:       triggerLogID,
		TriggerLogHash:     triggerLogHash,
		TotalTokens:        total,
		OwnerSharePct:      cov.OwnerSharePct,
		PlatformSharePct:   cov.PlatformSharePct,
		ContributorPoolPct: cov.ContributorPoolPct,
		Distributions:      dists,
		GeneratedAt:        time.Now().UTC(),
	}

	distsJSON, _ := json.Marshal(dists)
	_, err = db.Exec(`
		INSERT INTO settlement_outputs
		  (output_id, covenant_id, trigger_log_id, trigger_log_hash,
		   total_tokens, owner_share_pct, platform_share_pct, contributor_pool_pct,
		   distributions, generated_at, status)
		VALUES (?,?,?,?,?,?,?,?,?,?,'pending_confirmation')`,
		out.OutputID, covenantID, triggerLogID, triggerLogHash,
		total, cov.OwnerSharePct, cov.PlatformSharePct, cov.ContributorPoolPct,
		string(distsJSON), out.GeneratedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("save settlement: %w", err)
	}
	return out, nil
}
