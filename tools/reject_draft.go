package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/execution"
)

// RejectDraft reverses a token_ledger entry (by its audit log_id) and refunds the budget cost.
// Owner-only admin tool.
type RejectDraft struct{}

func (t *RejectDraft) ToolName() string { return "reject_draft" }
func (t *RejectDraft) ToolType() string { return "admin" }

func (t *RejectDraft) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can reject drafts")
	}
	logID, _ := params["log_id"].(string)
	if logID == "" {
		return fmt.Errorf("log_id is required")
	}
	// Confirm the ledger entry exists and belongs to this covenant.
	var status string
	err := ctx.DB.QueryRow(
		`SELECT status FROM token_ledger WHERE log_id=? AND covenant_id=?`,
		logID, ctx.Covenant.CovenantID,
	).Scan(&status)
	if err != nil {
		return fmt.Errorf("token_ledger entry for log_id %q not found: %w", logID, err)
	}
	if status == "rejected" || status == "reversed" {
		return fmt.Errorf("token_ledger entry for log_id %q is already %s", logID, status)
	}
	return nil
}

func (t *RejectDraft) EstimateCost(_ *execution.Context, _ map[string]any) float64 { return 0 }

func (t *RejectDraft) ExecuteLogic(ctx *execution.Context, params map[string]any) (map[string]any, error) {
	logID, _ := params["log_id"].(string)
	reason, _ := params["reason"].(string)

	// Read delta and the cost charged for this log entry.
	var delta int
	err := ctx.DB.QueryRow(
		`SELECT delta FROM token_ledger WHERE log_id=? AND covenant_id=?`,
		logID, ctx.Covenant.CovenantID,
	).Scan(&delta)
	if err != nil {
		return nil, fmt.Errorf("reading token_ledger: %w", err)
	}

	var costDelta float64
	_ = ctx.DB.QueryRow(
		`SELECT cost_delta FROM audit_logs WHERE log_id=?`, logID,
	).Scan(&costDelta)

	return map[string]any{
		"log_id":          logID,
		"tokens_returned": delta,
		"cost_returned":   costDelta,
		"reason":          reason,
		"status":          "rejected",
		"detail":          fmt.Sprintf("Draft (log %s) rejected, %d tokens reversed.", logID, delta),
		"is_final":        true,
	}, nil
}

func (t *RejectDraft) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: ctx.Covenant.State}
}

func (t *RejectDraft) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	logID, _ := result["log_id"].(string)
	costDelta, _ := result["cost_returned"].(float64)

	_, err := ctx.DB.Exec(
		`UPDATE token_ledger SET status='rejected' WHERE log_id=? AND covenant_id=?`,
		logID, ctx.Covenant.CovenantID,
	)
	if err != nil {
		return fmt.Errorf("updating token_ledger status: %w", err)
	}

	// Refund the budget cost that was charged when the draft was originally settled.
	if costDelta > 0 {
		if err := budget.Release(ctx.DB, ctx.Covenant.CovenantID, costDelta); err != nil {
			return fmt.Errorf("releasing budget: %w", err)
		}
	}
	return nil
}

func (t *RejectDraft) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	receipt.Extra["tokens_returned"] = result["tokens_returned"]
	receipt.Extra["reason"] = result["reason"]
}
