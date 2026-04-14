package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/tokens"
)

// ApproveDraft confirms a pending draft and awards tokens to the proposer.
type ApproveDraft struct{}

func (t *ApproveDraft) ToolName() string { return "approve_draft" }
func (t *ApproveDraft) ToolType() string { return "clause" }

func (t *ApproveDraft) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if ctx.Covenant.State != "ACTIVE" {
		return fmt.Errorf("covenant must be ACTIVE, currently %s", ctx.Covenant.State)
	}
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can approve drafts")
	}
	logID, _ := params["log_id"].(string)
	draftID, _ := params["draft_id"].(string)
	if logID == "" && draftID == "" {
		return fmt.Errorf("log_id or draft_id is required")
	}
	return nil
}

func (t *ApproveDraft) EstimateCost(_ *execution.Context, _ map[string]any) float64 { return 5 }

func (t *ApproveDraft) ExecuteLogic(ctx *execution.Context, params map[string]any) (map[string]any, error) {
	logID, _ := params["log_id"].(string)
	draftID, _ := params["draft_id"].(string)
	wc, _ := intParam(params, "word_count")
	ratio, _ := floatParam(params, "acceptance_ratio")
	if ratio == 0 {
		ratio = 1.0
	}
	if wc <= 0 {
		return nil, fmt.Errorf("word_count must be > 0 for approval")
	}

	// log_id path: look up proposer from audit_logs, then find their pending draft.
	if logID != "" && draftID == "" {
		var proposerAgentID string
		err := ctx.DB.QueryRow(
			`SELECT agent_id FROM audit_logs WHERE log_id=? AND covenant_id=?`,
			logID, ctx.Covenant.CovenantID,
		).Scan(&proposerAgentID)
		if err != nil {
			return nil, fmt.Errorf("log_id %q not found in this covenant: %w", logID, err)
		}
		err = ctx.DB.QueryRow(
			`SELECT draft_id FROM pending_tokens WHERE covenant_id=? AND agent_id=? LIMIT 1`,
			ctx.Covenant.CovenantID, proposerAgentID,
		).Scan(&draftID)
		if err != nil {
			return nil, fmt.Errorf("no pending draft for agent %q (log %s): %w", proposerAgentID, logID, err)
		}
	}

	proposerAgentID, err := tokens.ClaimPending(ctx.DB, ctx.Covenant.CovenantID, draftID)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"draft_id":          draftID,
		"log_id":            logID,
		"proposer_agent_id": proposerAgentID,
		"word_count":        wc,
		"acceptance_ratio":  ratio,
		"detail":            fmt.Sprintf("Draft %s approved at %.0f%%", draftID, ratio*100),
		"is_final":          true,
	}, nil
}

func (t *ApproveDraft) CalculateSideEffects(ctx *execution.Context, result map[string]any, _ map[string]any) execution.SideEffects {
	wc, _ := result["word_count"].(int)
	ratio, _ := result["acceptance_ratio"].(float64)
	proposerAgentID, _ := result["proposer_agent_id"].(string)

	// Look up proposer's tier multiplier
	var tierID string
	ctx.DB.QueryRow(`SELECT COALESCE(tier_id,'') FROM covenant_members WHERE covenant_id=? AND agent_id=?`,
		ctx.Covenant.CovenantID, proposerAgentID).Scan(&tierID)
	multiplier := 1.0
	if tierID != "" {
		ctx.DB.QueryRow(`SELECT token_multiplier FROM access_tiers WHERE covenant_id=? AND tier_id=?`,
			ctx.Covenant.CovenantID, tierID).Scan(&multiplier)
	}

	delta := tokens.Calculate(wc, multiplier, ratio)
	return execution.SideEffects{
		TokensDelta: delta,
		StateAfter:  ctx.Covenant.State,
	}
}

func (t *ApproveDraft) ApplySideEffects(ctx *execution.Context, log *audit.Entry, effects execution.SideEffects, result map[string]any, _ map[string]any) error {
	proposerAgentID, _ := result["proposer_agent_id"].(string)
	draftID, _ := result["draft_id"].(string)
	return tokens.ConfirmContribution(ctx.DB, ctx.Covenant.CovenantID, proposerAgentID, log.LogID, draftID, effects.TokensDelta)
}

func (t *ApproveDraft) EnrichReceipt(_ *execution.Receipt, _ map[string]any) {}
