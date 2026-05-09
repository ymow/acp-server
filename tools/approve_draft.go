package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/ratelimit"
	"github.com/inkmesh/acp-server/internal/tokens"
)

// ApproveDraft confirms a pending draft and awards tokens to the proposer.
type ApproveDraft struct{}

func (t *ApproveDraft) ToolName() string { return "approve_draft" }
func (t *ApproveDraft) ToolType() string { return "clause" }

// ParamsPolicy: approve_draft callers sometimes pass the full draft text for
// verification. Whitelist only the bookkeeping fields; drop anything else.
func (t *ApproveDraft) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"log_id", "draft_id", "unit_count", "acceptance_ratio"},
	}
}

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

func (t *ApproveDraft) EstimateCost(_ *execution.Context, params map[string]any) int64 {
	if v, ok := params["cost_cents"].(float64); ok && v >= 0 {
		return int64(v)
	}
	return 5 // x402 placeholder; replaced by receipt in Phase 7.A
}

func (t *ApproveDraft) ExecuteLogic(ctx *execution.Context, params map[string]any) (map[string]any, error) {
	logID, _ := params["log_id"].(string)
	draftID, _ := params["draft_id"].(string)
	uc, _ := intParam(params, "unit_count")
	ratio, _ := floatParam(params, "acceptance_ratio")
	if ratio == 0 {
		ratio = 1.0
	}
	if uc <= 0 {
		return nil, fmt.Errorf("unit_count must be > 0 for approval")
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
		"unit_count":        uc,
		"acceptance_ratio":  ratio,
		"detail":            fmt.Sprintf("Draft %s approved at %.0f%%", draftID, ratio*100),
		"is_final":          true,
	}, nil
}

func (t *ApproveDraft) CalculateSideEffects(ctx *execution.Context, result map[string]any, _ map[string]any) execution.SideEffects {
	uc, _ := result["unit_count"].(int)
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

	// ACR-20 Part 2: prefer a configured TokenRule for the originating clause
	// (propose_passage) when available. Falls back to the legacy fixed formula
	// so covenants created before configure_token_rules keep working.
	delta := tokens.Calculate(uc, multiplier, ratio)
	var rulesJSON string
	_ = ctx.DB.QueryRow(`SELECT token_rules_json FROM covenants WHERE covenant_id=?`,
		ctx.Covenant.CovenantID).Scan(&rulesJSON)
	if rules, err := tokens.ParseRules(rulesJSON); err == nil {
		if rule, ok := rules["propose_passage"]; ok {
			if v, err := rule.Evaluate(tokens.RuleVars{
				WordCount:       uc,
				AcceptanceRatio: ratio,
				TierMultiplier:  multiplier,
			}); err == nil {
				delta = v
			}
		}
	}
	return execution.SideEffects{
		TokensDelta: delta,
		StateAfter:  ctx.Covenant.State,
	}
}

func (t *ApproveDraft) ApplySideEffects(ctx *execution.Context, log *audit.Entry, effects execution.SideEffects, result map[string]any, _ map[string]any) error {
	proposerAgentID, _ := result["proposer_agent_id"].(string)
	draftID, _ := result["draft_id"].(string)
	if err := tokens.ConfirmContribution(ctx.DB, ctx.Covenant.CovenantID, proposerAgentID, log.LogID, draftID, effects.TokensDelta); err != nil {
		return err
	}
	// ACR-20 Part 4 Layer 5: concentration check runs *after* the ledger write
	// so the fresh award is reflected in the report. Failure to compute the
	// report is non-fatal — a transient warning miss must not block approval.
	if report, err := ratelimit.CheckConcentration(ctx.DB, ctx.Covenant.CovenantID); err == nil && len(report.Warnings) > 0 {
		result["concentration_warnings"] = report.Warnings
		result["concentration_total"] = report.Total
	}
	return nil
}

// EnrichReceipt surfaces concentration warnings on the receipt when the
// just-applied token award pushed an agent above concentration_warn_pct.
// Owners see this in their tool response without needing a separate query.
func (t *ApproveDraft) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	if w, ok := result["concentration_warnings"]; ok {
		receipt.Extra["concentration_warnings"] = w
	}
	if total, ok := result["concentration_total"]; ok {
		receipt.Extra["concentration_total"] = total
	}
}

