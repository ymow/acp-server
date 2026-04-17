package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/settlement"
)

// GenerateSettlement produces the SettlementOutput. Owner-only admin tool.
type GenerateSettlement struct{}

func (t *GenerateSettlement) ToolName() string { return "generate_settlement_output" }
func (t *GenerateSettlement) ToolType() string { return "admin" }

// ParamsPolicy: settlement takes no user-supplied params; default masking
// for the common sensitive field names is sufficient.
func (t *GenerateSettlement) ParamsPolicy() execution.ParamsPolicy {
	return execution.DefaultParamsPolicy()
}

func (t *GenerateSettlement) CheckPreconditions(ctx *execution.Context, _ map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can trigger settlement")
	}
	if ctx.Covenant.State != "LOCKED" && ctx.Covenant.State != "ACTIVE" {
		return fmt.Errorf("covenant must be LOCKED or ACTIVE to settle, currently %s", ctx.Covenant.State)
	}
	return nil
}

func (t *GenerateSettlement) EstimateCost(_ *execution.Context, _ map[string]any) int64 { return 20 }

func (t *GenerateSettlement) ExecuteLogic(_ *execution.Context, _ map[string]any) (map[string]any, error) {
	return map[string]any{
		"detail":   "Settlement output generation triggered.",
		"is_final": true,
	}, nil
}

func (t *GenerateSettlement) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *GenerateSettlement) ApplySideEffects(ctx *execution.Context, log *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	out, err := settlement.Generate(ctx.DB, ctx.CovenantSvc, ctx.Covenant.CovenantID, log.LogID, log.Hash)
	if err != nil {
		return err
	}
	result["output_id"] = out.OutputID
	return nil
}

func (t *GenerateSettlement) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	if oid, ok := result["output_id"].(string); ok {
		receipt.Extra["output_id"] = oid
	}
}
