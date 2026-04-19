package tools

// RecordGitTwinEvent writes an audit-only row for a non-merge git event that
// the bridge parsed out of a GitHub webhook: push.forced, push.protected,
// pull_request.opened, pull_request.rejected, tag.settlement.
//
// Merges (pull_request.merged) already drive the draft → approve flow via
// the existing bridge endpoint; this tool covers the "the twin should know
// this happened but nothing in the ledger mutates" cases.
//
// State-agnostic by design: these events can arrive in any covenant state
// (a force-push against a SETTLED covenant is still interesting history).
// The audit row written by the engine in Step 5 is the entire side effect —
// no tokens, no cost, no state transition.

import (
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/gittwin"
)

type RecordGitTwinEvent struct{}

func (t *RecordGitTwinEvent) ToolName() string { return "record_git_twin_event" }
func (t *RecordGitTwinEvent) ToolType() string { return "query" }

// Surface the semantic fields in the audit preview so verifiers can
// reconstruct the git context without loading the raw webhook payload.
// summary and source_ref are free-form annotations the bridge fills in.
//
// actor_platform_id is kept in the preview whitelist so the audit row
// signals "who did this" to a verifier, but ACR-700 §4 requires that
// plaintext never land in the durable log. SensitiveFields replaces the
// value with "*** (length: N)" — same shape as other sensitive fields.
func (t *RecordGitTwinEvent) ParamsPolicy() execution.ParamsPolicy {
	return execution.ParamsPolicy{
		PreviewFields: []string{
			"event_kind",
			"actor_platform_id",
			"ref",
			"commit_head",
			"summary",
			"source_ref",
		},
		SensitiveFields: []string{"actor_platform_id"},
	}
}

// validEventKinds are the bridge-normalized kinds this tool accepts. The
// merged kind is deliberately excluded — that flow goes through the twin
// merge endpoint which runs propose_passage + approve_draft.
var validEventKinds = map[string]struct{}{
	string(gittwin.EventPushForced):          {},
	string(gittwin.EventPushProtected):       {},
	string(gittwin.EventPullRequestOpened):   {},
	string(gittwin.EventPullRequestRejected): {},
	string(gittwin.EventTagSettlement):       {},
}

func (t *RecordGitTwinEvent) CheckPreconditions(_ *execution.Context, params map[string]any) error {
	kind, _ := params["event_kind"].(string)
	if kind == "" {
		return fmt.Errorf("event_kind is required")
	}
	if _, ok := validEventKinds[kind]; !ok {
		return fmt.Errorf("event_kind %q is not a recordable git twin event", kind)
	}
	return nil
}

func (t *RecordGitTwinEvent) ExecuteLogic(_ *execution.Context, params map[string]any) (map[string]any, error) {
	kind, _ := params["event_kind"].(string)
	ref, _ := params["ref"].(string)
	commit, _ := params["commit_head"].(string)
	summary, _ := params["summary"].(string)

	detail := summary
	if detail == "" {
		switch {
		case ref != "" && commit != "":
			detail = fmt.Sprintf("%s on %s @ %s", kind, ref, shortSHA(commit))
		case ref != "":
			detail = fmt.Sprintf("%s on %s", kind, ref)
		case commit != "":
			detail = fmt.Sprintf("%s @ %s", kind, shortSHA(commit))
		default:
			detail = kind
		}
	}

	return map[string]any{
		"event_kind": kind,
		"detail":     detail,
		"is_final":   true,
	}, nil
}

// No-op side effects: Covenant state unchanged, zero tokens, zero cost.
// The audit row written by the engine is the entire point of this tool.
func (t *RecordGitTwinEvent) CalculateSideEffects(ctx *execution.Context, _ map[string]any, _ map[string]any) execution.SideEffects {
	return execution.SideEffects{StateAfter: ctx.Covenant.State}
}

func (t *RecordGitTwinEvent) ApplySideEffects(_ *execution.Context, _ *audit.Entry, _ execution.SideEffects, _ map[string]any, _ map[string]any) error {
	return nil
}

func (t *RecordGitTwinEvent) EnrichReceipt(receipt *execution.Receipt, result map[string]any) {
	if kind, ok := result["event_kind"].(string); ok {
		receipt.Extra["event_kind"] = kind
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
