package tools

import (
	"encoding/json"
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
)

// ConfigureTokenRules stores token allocation rules for a Covenant in DRAFT state.
// Rules are serialised as JSON into covenants.token_rules_json.
type ConfigureTokenRules struct{}

func (t *ConfigureTokenRules) ToolName() string { return "configure_token_rules" }
func (t *ConfigureTokenRules) ToolType() string { return "admin" }

// ParamsPolicy: the rules tree itself IS the audit-worthy content here.
// Pass it through verbatim (default masking handles the historical field
// names for defense-in-depth).
func (t *ConfigureTokenRules) ParamsPolicy() execution.ParamsPolicy {
	return execution.DefaultParamsPolicy()
}

func (t *ConfigureTokenRules) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can configure token rules")
	}
	if ctx.Covenant.State != "DRAFT" {
		return fmt.Errorf("covenant must be in DRAFT state to configure token rules, currently %s", ctx.Covenant.State)
	}
	if params["rules"] == nil {
		return fmt.Errorf("rules is required")
	}
	return nil
}

func (t *ConfigureTokenRules) EstimateCost(_ *execution.Context, _ map[string]any) int64 { return 0 }

func (t *ConfigureTokenRules) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	rulesJSON, err := json.Marshal(params["rules"])
	if err != nil {
		return nil, fmt.Errorf("invalid rules format: %w", err)
	}
	return map[string]any{
		"rules_json": string(rulesJSON),
		"detail":     "Token rules configured.",
		"is_final":   true,
	}, nil
}

func (t *ConfigureTokenRules) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *ConfigureTokenRules) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	rulesJSON, _ := result["rules_json"].(string)
	_, err := ctx.DB.Exec(
		`UPDATE covenants SET token_rules_json=?, updated_at=datetime('now','utc') WHERE covenant_id=?`,
		rulesJSON, ctx.Covenant.CovenantID,
	)
	return err
}

func (t *ConfigureTokenRules) EnrichReceipt(receipt *execution.Receipt, _ map[string]any) {
	receipt.Extra["ok"] = true
}
