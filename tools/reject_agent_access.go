package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// RejectAgentAccess is the owner-side rejection path for ACR-50 §§2, 7. Sets
// status='rejected' and records the operator-supplied reason; no
// covenant_members row is created. Owner-only admin tool.
type RejectAgentAccess struct{}

func (t *RejectAgentAccess) ToolName() string { return "reject_agent_access" }
func (t *RejectAgentAccess) ToolType() string { return "admin" }

func (t *RejectAgentAccess) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"request_id", "reason"},
	}
}

func (t *RejectAgentAccess) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can reject access requests")
	}
	requestID, _ := params["request_id"].(string)
	if requestID == "" {
		return fmt.Errorf("request_id is required")
	}
	req, err := ctx.CovenantSvc.GetAccessRequest(ctx.Covenant.CovenantID, requestID)
	if err != nil {
		return err
	}
	if req.Status != "pending" {
		return fmt.Errorf("access request %q is not pending (current status: %s)", requestID, req.Status)
	}
	return nil
}

func (t *RejectAgentAccess) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	requestID, _ := params["request_id"].(string)
	reason, _ := params["reason"].(string)
	return map[string]any{
		"request_id": requestID,
		"status":     "rejected",
		"reason":     reason,
		"detail":     fmt.Sprintf("Access request %s rejected.", requestID),
		"is_final":   true,
	}, nil
}

func (t *RejectAgentAccess) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: ctx.Covenant.State}
}

func (t *RejectAgentAccess) ApplySideEffects(ctx *execution.Context, log *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	requestID, _ := result["request_id"].(string)
	reason, _ := result["reason"].(string)
	return ctx.CovenantSvc.RejectAccessRequest(ctx.Covenant.CovenantID, requestID, reason, log.LogID)
}

func (t *RejectAgentAccess) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	receipt.Extra["reason"] = result["reason"]
}
