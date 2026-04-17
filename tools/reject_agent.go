package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// RejectAgent sets a pending covenant member's status to 'rejected'.
// Owner-only admin tool.
type RejectAgent struct{}

func (t *RejectAgent) ToolName() string { return "reject_agent" }
func (t *RejectAgent) ToolType() string { return "admin" }

// ParamsPolicy: agent_id + human reason are both safe for audit.
func (t *RejectAgent) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"agent_id", "reason"},
	}
}

func (t *RejectAgent) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can reject agents")
	}
	agentID, _ := params["agent_id"].(string)
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}
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

func (t *RejectAgent) EstimateCost(_ *execution.Context, _ map[string]any) int64 { return 0 }

func (t *RejectAgent) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	agentID, _ := params["agent_id"].(string)
	reason, _ := params["reason"].(string)
	return map[string]any{
		"agent_id": agentID,
		"status":   "rejected",
		"reason":   reason,
		"detail":   fmt.Sprintf("Agent %s rejected.", agentID),
		"is_final": true,
	}, nil
}

func (t *RejectAgent) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: ctx.Covenant.State}
}

func (t *RejectAgent) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	agentID, _ := result["agent_id"].(string)
	_, err := ctx.DB.Exec(
		`UPDATE covenant_members SET status='rejected' WHERE covenant_id=? AND agent_id=?`,
		ctx.Covenant.CovenantID, agentID,
	)
	return err
}

func (t *RejectAgent) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	receipt.Extra["reason"] = result["reason"]
}
