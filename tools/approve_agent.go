package tools

import (
	"fmt"
	"time"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// ApproveAgent activates a covenant member whose status is 'pending'.
// Owner-only admin tool. TokensDelta = 0.
type ApproveAgent struct{}

func (t *ApproveAgent) ToolName() string { return "approve_agent" }
func (t *ApproveAgent) ToolType() string { return "admin" }

// ParamsPolicy: only agent_id is relevant for audit.
func (t *ApproveAgent) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"agent_id"},
	}
}

func (t *ApproveAgent) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can approve agents")
	}
	agentID, _ := params["agent_id"].(string)
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	// Verify the target agent exists and is pending.
	var status string
	err := ctx.DB.QueryRow(
		`SELECT status FROM covenant_members WHERE covenant_id=? AND agent_id=?`,
		ctx.Covenant.CovenantID, agentID,
	).Scan(&status)
	if err != nil {
		return fmt.Errorf("agent %q not found in this covenant: %w", agentID, err)
	}
	if status != "pending" {
		return fmt.Errorf("agent %q is not pending (current status: %s)", agentID, status)
	}
	return nil
}

func (t *ApproveAgent) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	agentID, _ := params["agent_id"].(string)
	approvedAt := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"agent_id":    agentID,
		"status":      "active",
		"approved_at": approvedAt,
		"detail":      fmt.Sprintf("Agent %s approved and activated.", agentID),
		"is_final":    true,
	}, nil
}

func (t *ApproveAgent) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: ctx.Covenant.State}
}

func (t *ApproveAgent) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	agentID, _ := result["agent_id"].(string)
	approvedAt, _ := result["approved_at"].(string)
	_, err := ctx.DB.Exec(
		`UPDATE covenant_members SET status='active', joined_at=? WHERE covenant_id=? AND agent_id=?`,
		approvedAt, ctx.Covenant.CovenantID, agentID,
	)
	return err
}

func (t *ApproveAgent) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	receipt.Extra["approved_at"] = result["approved_at"]
}
