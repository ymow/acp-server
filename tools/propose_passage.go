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
// log verbatim. unit_count / chapter / summary remain visible for audit.
// content_hash is truncated — the full hash lives in token_ledger already.
func (t *ProposePassage) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields:     []string{"unit_count", "chapter", "summary", "content_hash"},
		HashPreviewFields: []string{"content_hash"},
	}
}

func (t *ProposePassage) CheckPreconditions(ctx *execution.Context, params map[string]any) error {
	if ctx.Covenant.State != "ACTIVE" {
		return fmt.Errorf("covenant must be ACTIVE, currently %s", ctx.Covenant.State)
	}
	uc, _ := intParam(params, "unit_count")
	if uc <= 0 {
		return fmt.Errorf("unit_count must be > 0")
	}
	return nil
}

func (t *ProposePassage) EstimateCost(_ *execution.Context, _ map[string]any) int64 { return 10 }

func (t *ProposePassage) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	uc, _ := intParam(params, "unit_count")
	draftID := "draft_" + id.Agent()[6:] // reuse random8
	result := map[string]any{
		"draft_id":   draftID,
		"unit_count": uc,
		"detail":     fmt.Sprintf("Draft %s proposed with %d units.", draftID, uc),
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

// EnrichReceipt surfaces draft_id to callers. The engine's Step 8 receipt
// otherwise omits it, which forces the git bridge (ACR-400) to probe
// pending_tokens for the row it just wrote — racy and DB-tied.
func (t *ProposePassage) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	if d, ok := result["draft_id"].(string); ok {
		receipt.Extra["draft_id"] = d
	}
}

func (t *ProposePassage) ApplySideEffects(ctx *execution.Context, _ *audit.Entry, _ execution.SideEffects, result map[string]any, _ map[string]any) error {
	return tokens.CreatePending(ctx.DB, ctx.Covenant.CovenantID, ctx.Member.AgentID, result["draft_id"].(string))
}

