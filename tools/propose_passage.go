package tools

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/id"
	"github.com/inkmesh/acp-server/internal/tokens"
)

// ProposePassage creates a pending draft entry.
// Tokens are not awarded until approve_draft is called.
type ProposePassage struct{}

func (t *ProposePassage) ToolName() string { return "propose_passage" }
func (t *ProposePassage) ToolType() string { return "clause" }

func (t *ProposePassage) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if ctx.Covenant.State != "ACTIVE" {
		return fmt.Errorf("covenant must be ACTIVE, currently %s", ctx.Covenant.State)
	}
	wc, _ := intParam(params, "word_count")
	if wc <= 0 {
		return fmt.Errorf("word_count must be > 0")
	}
	return nil
}

func (t *ProposePassage) EstimateCost(_ *execution.Context, _ map[string]any) float64 { return 10 }

func (t *ProposePassage) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	wc, _ := intParam(params, "word_count")
	draftID := "draft_" + id.Agent()[6:] // reuse random8
	return map[string]any{
		"draft_id":   draftID,
		"word_count": wc,
		"detail":     fmt.Sprintf("Draft %s proposed with %d words.", draftID, wc),
		"is_final":   false,
	}, nil
}

func (t *ProposePassage) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *ProposePassage) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	return tokens.CreatePending(ctx.DB, ctx.Covenant.CovenantID, ctx.Member.AgentID, result["draft_id"].(string))
}

func (t *ProposePassage) EnrichReceipt(_ *execution.Receipt, _ map[string]any) {}
