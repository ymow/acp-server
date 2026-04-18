package tools

import (
	"fmt"
	"log"
	"time"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/gittwin"
)

// ConfirmSettlementOutput confirms a pending settlement output and transitions
// the Covenant from LOCKED to SETTLED. Owner-only admin tool.
//
// AnchorSigner is optional: when set, every anchor enqueued here is signed
// with that key before the bridge writes it. Leaving it nil produces
// unsigned anchors, which ACR-400 v0.2 permits for v0.1-compat deployments.
type ConfirmSettlementOutput struct {
	AnchorSigner gittwin.Signer
}

func (t *ConfirmSettlementOutput) ToolName() string { return "confirm_settlement_output" }
func (t *ConfirmSettlementOutput) ToolType() string { return "admin" }

// ParamsPolicy: only the output ID is relevant for audit.
func (t *ConfirmSettlementOutput) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"settlement_output_id"},
	}
}

func (t *ConfirmSettlementOutput) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can confirm settlement output")
	}
	if ctx.Covenant.State != "LOCKED" {
		return fmt.Errorf("covenant must be LOCKED to confirm settlement, currently %s", ctx.Covenant.State)
	}
	outputID, _ := params["settlement_output_id"].(string)
	if outputID == "" {
		return fmt.Errorf("settlement_output_id is required")
	}
	// Verify the output exists and is awaiting confirmation.
	var status string
	err := ctx.DB.QueryRow(
		`SELECT status FROM settlement_outputs WHERE output_id=? AND covenant_id=?`,
		outputID, ctx.Covenant.CovenantID,
	).Scan(&status)
	if err != nil {
		return fmt.Errorf("settlement output %q not found: %w", outputID, err)
	}
	if status != "pending_confirmation" {
		return fmt.Errorf("settlement output %q is not pending confirmation (status: %s)", outputID, status)
	}
	return nil
}

func (t *ConfirmSettlementOutput) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	outputID, _ := params["settlement_output_id"].(string)
	confirmedAt := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"settlement_output_id": outputID,
		"status":               "SETTLED",
		"confirmed_at":         confirmedAt,
		"detail":               fmt.Sprintf("Settlement output %s confirmed. Covenant transitioning to SETTLED.", outputID),
		"is_final":             true,
	}, nil
}

func (t *ConfirmSettlementOutput) CalculateSideEffects(_ *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: "SETTLED"}
}

func (t *ConfirmSettlementOutput) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	outputID, _ := result["settlement_output_id"].(string)
	confirmedAt, _ := result["confirmed_at"].(string)

	_, err := ctx.DB.Exec(
		`UPDATE settlement_outputs SET status='confirmed', confirmed_at=? WHERE output_id=? AND covenant_id=?`,
		confirmedAt, outputID, ctx.Covenant.CovenantID,
	)
	if err != nil {
		return fmt.Errorf("update settlement output: %w", err)
	}

	// Transition covenant LOCKED → SETTLED.
	if _, err = ctx.CovenantSvc.Transition(ctx.Covenant.CovenantID, "SETTLED"); err != nil {
		return fmt.Errorf("covenant transition to SETTLED: %w", err)
	}

	// ACR-400 Part 5: enqueue a Git Anchor so the bridge can pin this settlement
	// into the twinned repo's refs/notes/acp-anchors. Failure to enqueue is a
	// soft error: settlement already landed in the ledger, and the anchor is a
	// best-effort amplifier rather than a correctness guarantee. Log + carry on.
	if ctx.Covenant.GitTwinURL != "" {
		anchor, aerr := gittwin.EnqueueAnchor(ctx.DB, ctx.Covenant.CovenantID, ctx.Covenant.GitTwinURL, outputID, t.AnchorSigner)
		if aerr != nil {
			log.Printf("enqueue git anchor for %s/%s: %v", ctx.Covenant.CovenantID, outputID, aerr)
		} else {
			result["anchor_id"] = anchor.AnchorID
		}
	}
	return nil
}

func (t *ConfirmSettlementOutput) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	receipt.Extra["confirmed_at"] = result["confirmed_at"]
	receipt.Extra["settlement_output_id"] = result["settlement_output_id"]
	if aid, ok := result["anchor_id"].(string); ok && aid != "" {
		receipt.Extra["anchor_id"] = aid
	}
}
