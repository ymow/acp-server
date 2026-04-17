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

// ParamsPolicy: user-supplied prose fields must never hit the durable audit
// log verbatim. word_count / chapter / summary remain visible for audit.
// content_hash is truncated — the full hash lives in token_ledger already.
func (t *ProposePassage) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields:     []string{"word_count", "chapter", "summary", "content_hash"},
		HashPreviewFields: []string{"content_hash"},
	}
}

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

func (t *ProposePassage) EstimateCost(_ *execution.Context, _ map[string]any) int64 { return 10 }

func (t *ProposePassage) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	wc, _ := intParam(params, "word_count")
	draftID := "draft_" + id.Agent()[6:] // reuse random8
	result := map[string]any{
		"draft_id":   draftID,
		"word_count": wc,
		"detail":     fmt.Sprintf("Draft %s proposed with %d words.", draftID, wc),
		"is_final":   false,
	}
	// Optional anti-gaming fields: stored in audit log params_preview (content_hash masked to 8 chars).
	if chapter, ok := params["chapter"].(string); ok && chapter != "" {
		result["chapter"] = chapter
	}
	if summary, ok := params["summary"].(string); ok && summary != "" {
		result["summary"] = summary
	}
	if contentHash, ok := params["content_hash"].(string); ok && contentHash != "" {
		result["content_hash"] = contentHash
	}
	return result, nil
}

func (t *ProposePassage) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *ProposePassage) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	return tokens.CreatePending(ctx.DB, ctx.Covenant.CovenantID, ctx.Member.AgentID, result["draft_id"].(string))
}

