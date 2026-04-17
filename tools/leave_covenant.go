package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// LeaveCovenant marks the calling member's status as 'left'. The member's
// confirmed token_ledger entries stay intact so their contribution history
// is preserved (Constitutional Principle: confirmed contributions are
// never deleted, only the member's active-participation flag is cleared).
// The covenant owner cannot leave — they must transition to SETTLED first.
type LeaveCovenant struct{}

func (t *LeaveCovenant) ToolName() string { return "leave_covenant" }
func (t *LeaveCovenant) ToolType() string { return "admin" }

func (t *LeaveCovenant) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{PreviewFields: []string{"reason"}}
}

func (t *LeaveCovenant) CheckPreconditions(ctx *execution.Context, _ map[string]any) error {
	if ctx.Member.IsOwner {
		return fmt.Errorf("covenant owner cannot leave; transition to SETTLED instead")
	}
	if ctx.Member.Status == "left" {
		return fmt.Errorf("agent has already left this covenant")
	}
	return nil
}

func (t *LeaveCovenant) ExecuteLogic(ctx *execution.Context, params map[string]any) (map[string]any, error) {
	reason, _ := params["reason"].(string)
	return map[string]any{
		"agent_id": ctx.Member.AgentID,
		"reason":   reason,
		"detail":   fmt.Sprintf("Agent %s left covenant %s", ctx.Member.AgentID, ctx.Covenant.CovenantID),
		"is_final": true,
	}, nil
}

func (t *LeaveCovenant) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *LeaveCovenant) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, _ map[string]any, _ map[string]any) error {
	_, err := ctx.DB.Exec(
		`UPDATE covenant_members SET status='left' WHERE covenant_id=? AND agent_id=?`,
		ctx.Covenant.CovenantID, ctx.Member.AgentID,
	)
	return err
}

func (t *LeaveCovenant) EnrichReceipt(receipt *execution.Receipt, _ map[string]any) {
	receipt.Extra["status"] = "left"
}
