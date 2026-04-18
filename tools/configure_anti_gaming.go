package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/ratelimit"
)

// ConfigureAntiGaming writes the AntiGamingPolicy for a Covenant (ACR-20 Part 4).
// Phase 4.1 exposes rate_limit_per_hour only; the other fields are accepted
// and persisted but not enforced until their respective chunks land.
//
// Allowed in DRAFT or OPEN — owners must be able to raise the limit once an
// abuse pattern is observed, not only at Covenant creation.
type ConfigureAntiGaming struct{}

func (t *ConfigureAntiGaming) ToolName() string { return "configure_anti_gaming" }
func (t *ConfigureAntiGaming) ToolType() string { return "admin" }

func (t *ConfigureAntiGaming) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if !ctx.Member.IsOwner {
		return fmt.Errorf("only covenant owner can configure anti-gaming policy")
	}
	switch ctx.Covenant.State {
	case "DRAFT", "OPEN", "ACTIVE":
	default:
		return fmt.Errorf("anti-gaming policy can only be configured before LOCKED, currently %s", ctx.Covenant.State)
	}
	// rate_limit_per_hour is the only Phase 4.1 knob; require it explicitly so
	// operators know what they asked for. Other fields are optional reservations.
	n, ok := intParam(params, "rate_limit_per_hour")
	if !ok {
		return fmt.Errorf("rate_limit_per_hour is required")
	}
	if n < 0 {
		return fmt.Errorf("rate_limit_per_hour must be >= 0 (0 means unlimited)")
	}
	return nil
}

func (t *ConfigureAntiGaming) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	rate, _ := intParam(params, "rate_limit_per_hour")
	sim, _ := floatParam(params, "similarity_threshold")
	minWords, _ := intParam(params, "min_word_count")
	concWarn, _ := floatParam(params, "concentration_warn_pct")
	return map[string]any{
		"rate_limit_per_hour":    rate,
		"similarity_threshold":   sim,
		"min_word_count":         minWords,
		"concentration_warn_pct": concWarn,
		"detail":                 "Anti-gaming policy updated.",
		"is_final":               true,
	}, nil
}

func (t *ConfigureAntiGaming) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *ConfigureAntiGaming) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	return ratelimit.UpsertPolicy(ctx.DB, ratelimit.Policy{
		CovenantID:           ctx.Covenant.CovenantID,
		RateLimitPerHour:     asInt(result["rate_limit_per_hour"]),
		SimilarityThreshold:  asFloat(result["similarity_threshold"]),
		MinWordCount:         asInt(result["min_word_count"]),
		ConcentrationWarnPct: asFloat(result["concentration_warn_pct"]),
	})
}

func (t *ConfigureAntiGaming) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	receipt.Extra["rate_limit_per_hour"] = result["rate_limit_per_hour"]
}

// asInt / asFloat coerce values surfaced back from the result map in
// ApplySideEffects; ExecuteLogic stored them with their native Go types.
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}
