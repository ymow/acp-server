package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// ApproveAgentAccess is the owner-side of ACR-50 §§2, 7. It flips a pending
// agent_access_requests row to 'approved' and activates the applicant as a
// covenant member. Owner-only admin tool. TokensDelta = 0.
type ApproveAgentAccess struct{}

func (t *ApproveAgentAccess) ToolName() string { return "approve_agent_access" }
func (t *ApproveAgentAccess) ToolType() string { return "admin" }

// ParamsPolicy: request_id is a safe identifier for audit.
func (t *ApproveAgentAccess) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{"request_id"},
	}
}

func (t *ApproveAgentAccess) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can approve access requests")
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

func (t *ApproveAgentAccess) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	requestID, _ := params["request_id"].(string)
	return map[string]any{
		"request_id": requestID,
		"status":     "approved",
		"detail":     fmt.Sprintf("Access request %s approved.", requestID),
		"is_final":   true,
	}, nil
}

func (t *ApproveAgentAccess) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{TokensDelta: 0, StateAfter: ctx.Covenant.State}
}

func (t *ApproveAgentAccess) ApplySideEffects(ctx *execution.Context, log *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	requestID, _ := result["request_id"].(string)
	mem, err := ctx.CovenantSvc.ApproveAccessRequest(ctx.Covenant.CovenantID, requestID, log.LogID)
	if err != nil {
		return err
	}
	// Surface the freshly minted agent_id / tier_id on the receipt so the
	// applicant can start using their covenant-scoped handle immediately.
	result["agent_id"] = mem.AgentID
	result["tier_id"] = mem.TierID
	return nil
}

func (t *ApproveAgentAccess) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["status"] = result["status"]
	if v, ok := result["agent_id"]; ok {
		receipt.Extra["agent_id"] = v
	}
	if v, ok := result["tier_id"]; ok {
		receipt.Extra["tier_id"] = v
	}
}
