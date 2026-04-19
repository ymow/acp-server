package api

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/gittwin"
	"github.com/inkmesh/acp-server/tools"
)

// validateBridgeSecret authenticates a request from cmd/acp-git-bridge.
// The bridge has no session token; instead both sides share ACP_BRIDGE_SECRET
// via env and the bridge passes it in X-Bridge-Secret. Empty env → disabled
// (returns 503 so ops notices rather than silently letting anyone through).
func (s *Server) validateBridgeSecret(r *http.Request) error {
	expected := os.Getenv("ACP_BRIDGE_SECRET")
	if expected == "" {
		return errors.New("git-twin endpoints disabled: ACP_BRIDGE_SECRET not configured")
	}
	got := r.Header.Get("X-Bridge-Secret")
	if got == "" || got != expected {
		return errors.New("invalid bridge secret")
	}
	return nil
}

// handleSetGitTwin binds (or unbinds) a covenant to a git repo Digital Twin.
// Owner-auth; covenant must be DRAFT (enforced in covenant.SetGitTwin).
func (s *Server) handleSetGitTwin(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		URL        string `json:"git_twin_url"`
		Provider   string `json:"git_twin_provider"`
		ConfigJSON string `json:"git_twin_config_json"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.covSvc.SetGitTwin(covenantID, req.URL, req.Provider, req.ConfigJSON); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// handleGitTwinFindCovenants returns covenants bound to a given repo URL.
// Bridge-auth; used by the bridge to route webhooks (ACR-400 Part 7).
func (s *Server) handleGitTwinFindCovenants(w http.ResponseWriter, r *http.Request) {
	if err := s.validateBridgeSecret(r); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	url := r.URL.Query().Get("repo_url")
	if url == "" {
		jsonError(w, "repo_url query parameter required", http.StatusBadRequest)
		return
	}
	ids, err := s.covSvc.FindByGitTwinURL(url)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ids == nil {
		ids = []string{}
	}
	jsonOK(w, map[string]any{"covenant_ids": ids})
}

// handleGitTwinMerge collapses "PR opened + PR merged" into a single atomic
// propose_passage + approve_draft sequence, keyed by draft_ref for idempotency.
//
// MVP simplification vs ACR-400 Part 2.1: the spec maps PR-open→propose and
// PR-merge→approve as separate events; v0.1 only fires on PR-merge because
// GitHub's pull_request.opened webhook does not carry additions/deletions
// needed for a meaningful unit_count.
func (s *Server) handleGitTwinMerge(w http.ResponseWriter, r *http.Request) {
	if err := s.validateBridgeSecret(r); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		CovenantID       string  `json:"covenant_id"`
		AuthorPlatformID string  `json:"author_platform_id"`
		DraftRef         string  `json:"draft_ref"`
		UnitCount        int     `json:"unit_count"`
		AcceptanceRatio  float64 `json:"acceptance_ratio"`
		ContentHash      string  `json:"content_hash"`
		Summary          string  `json:"summary"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.CovenantID == "" || req.AuthorPlatformID == "" || req.DraftRef == "" {
		jsonError(w, "covenant_id, author_platform_id, and draft_ref are required", http.StatusBadRequest)
		return
	}
	if req.UnitCount <= 0 {
		jsonError(w, "unit_count must be > 0", http.StatusBadRequest)
		return
	}
	if req.AcceptanceRatio == 0 {
		req.AcceptanceRatio = 1.0
	}

	// Idempotency: if this draft_ref already landed an approval, short-circuit.
	var existingStatus, existingProposeLog, existingApproveLog string
	err := s.db.QueryRow(`SELECT status, propose_log_id, approve_log_id FROM git_twin_events WHERE draft_ref=?`,
		req.DraftRef).Scan(&existingStatus, &existingProposeLog, &existingApproveLog)
	if err == nil && existingStatus == "approved" {
		jsonOK(w, map[string]any{
			"idempotent":     true,
			"status":         existingStatus,
			"propose_log_id": existingProposeLog,
			"approve_log_id": existingApproveLog,
		})
		return
	}

	// Author mapping (ACR-400 Part 3). Unmapped → audit-only, no ledger effect.
	author, err := s.covSvc.FindMemberByPlatformID(req.CovenantID, req.AuthorPlatformID)
	if errors.Is(err, covenant.ErrNoMember) {
		// ACR-700 §4: do not echo the plaintext author back to the bridge.
		// A 12-char hash prefix is enough for the bridge to correlate this
		// response with the webhook it forwarded.
		hash := covenant.HashPlatformID(req.AuthorPlatformID)
		jsonOK(w, map[string]any{
			"unmapped":                 true,
			"author_hash_prefix":       hash[:12],
			"covenant_id":              req.CovenantID,
			"draft_ref":                req.DraftRef,
			"detail":                   "contributor is not an approved covenant member; ledger untouched",
		})
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Step 1: propose_passage as the author.
	proposeParams := map[string]any{
		"unit_count":    req.UnitCount,
		"content_hash":  req.ContentHash,
		"summary":       truncate(req.Summary, 200),
		"chapter":       req.DraftRef,
	}
	proposeReceipt, err := s.engine.Run(req.CovenantID, author.AgentID,
		bridgeSessionFingerprint(req.DraftRef, "propose"),
		&tools.ProposePassage{}, proposeParams)
	if err != nil {
		jsonError(w, fmt.Sprintf("propose_passage failed: %v", err), mapEngineErrStatus(err))
		return
	}
	draftID, _ := proposeReceipt.Extra["draft_id"].(string)
	if draftID == "" {
		jsonError(w, "propose_passage did not return draft_id", http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.Exec(`
		INSERT INTO git_twin_events (draft_ref, covenant_id, agent_id, draft_id, propose_log_id, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'proposed', ?, ?)
		ON CONFLICT(draft_ref) DO UPDATE SET
		  draft_id=excluded.draft_id, propose_log_id=excluded.propose_log_id,
		  status='proposed', updated_at=excluded.updated_at`,
		req.DraftRef, req.CovenantID, author.AgentID, draftID, proposeReceipt.LogHash, now, now)

	// Step 2: approve_draft as the covenant owner (bridge impersonation).
	ownerAgentID, err := s.covSvc.GetOwnerAgentID(req.CovenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	approveParams := map[string]any{
		"draft_id":         draftID,
		"unit_count":       req.UnitCount,
		"acceptance_ratio": req.AcceptanceRatio,
	}
	approveReceipt, err := s.engine.Run(req.CovenantID, ownerAgentID,
		bridgeSessionFingerprint(req.DraftRef, "approve"),
		&tools.ApproveDraft{}, approveParams)
	if err != nil {
		jsonError(w, fmt.Sprintf("approve_draft failed: %v", err), mapEngineErrStatus(err))
		return
	}

	_, _ = s.db.Exec(`
		UPDATE git_twin_events SET approve_log_id=?, status='approved', updated_at=?
		WHERE draft_ref=?`,
		approveReceipt.LogHash, time.Now().UTC().Format(time.RFC3339Nano), req.DraftRef)

	jsonOK(w, map[string]any{
		"propose_receipt": proposeReceipt,
		"approve_receipt": approveReceipt,
		"agent_id":        author.AgentID,
	})
}

// bridgeSessionFingerprint is the session_id we stamp in audit_logs for
// bridge-originated calls. It is not a real session token hash; the prefix
// "bridge:" makes that obvious to anyone inspecting the chain.
func bridgeSessionFingerprint(draftRef, phase string) string {
	return "bridge:" + phase + ":" + draftRef
}

// mapEngineErrStatus mirrors the toolHandler's error → HTTP status mapping
// so bridge callers get sensible codes without routing through the full engine path.
func mapEngineErrStatus(err error) int {
	msg := err.Error()
	if strings.HasPrefix(msg, "step1.forbidden:") {
		return http.StatusForbidden
	}
	if strings.HasPrefix(msg, "step1:") || strings.HasPrefix(msg, "step2:") ||
		strings.HasPrefix(msg, "step2.5:") || strings.HasPrefix(msg, "step3:") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// handleGitTwinRecordEvent writes an audit-only row for a non-merge git event
// that the bridge parsed out of a GitHub webhook (push.forced, push.protected,
// pull_request.opened, pull_request.rejected, tag.settlement). Bridge-auth.
//
// Actor resolution (ACR-400 Part 3 extended for non-ledger events):
//   - actor_platform_id maps to a covenant member → audit row under that agent
//   - unmapped (external contributor, bot, deleted account) → audit row under
//     the covenant owner with actor_platform_id surfaced in ParamsPreview, so
//     verifiers still see who did what while the ledger stays clean
//
// No state gate: these events can arrive in any covenant state; the engine's
// Step 1 still requires the executing member to be active, which the owner
// always is.
func (s *Server) handleGitTwinRecordEvent(w http.ResponseWriter, r *http.Request) {
	if err := s.validateBridgeSecret(r); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		CovenantID      string `json:"covenant_id"`
		ActorPlatformID string `json:"actor_platform_id"`
		EventKind       string `json:"event_kind"`
		Ref             string `json:"ref"`
		CommitHead      string `json:"commit_head"`
		Summary         string `json:"summary"`
		SourceRef       string `json:"source_ref"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.CovenantID == "" || req.EventKind == "" {
		jsonError(w, "covenant_id and event_kind are required", http.StatusBadRequest)
		return
	}

	// Resolve the acting agent. Unmapped → fall back to owner so the audit
	// chain always has a real agent_id; actor_platform_id remains in the
	// params preview for attribution.
	var agentID string
	var mappedActor bool
	if req.ActorPlatformID != "" {
		member, err := s.covSvc.FindMemberByPlatformID(req.CovenantID, req.ActorPlatformID)
		switch {
		case err == nil:
			agentID = member.AgentID
			mappedActor = true
		case errors.Is(err, covenant.ErrNoMember):
			// fall through to owner fallback
		default:
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if agentID == "" {
		ownerAgentID, err := s.covSvc.GetOwnerAgentID(req.CovenantID)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		agentID = ownerAgentID
	}

	params := map[string]any{
		"event_kind":        req.EventKind,
		"actor_platform_id": req.ActorPlatformID,
		"ref":               req.Ref,
		"commit_head":       req.CommitHead,
		"summary":           truncate(req.Summary, 200),
		"source_ref":        req.SourceRef,
	}
	session := bridgeSessionFingerprint(req.EventKind+":"+req.CommitHead, "event")
	receipt, err := s.engine.Run(req.CovenantID, agentID, session, &tools.RecordGitTwinEvent{}, params)
	if err != nil {
		jsonError(w, err.Error(), mapEngineErrStatus(err))
		return
	}
	jsonOK(w, map[string]any{
		"receipt":      receipt,
		"agent_id":     agentID,
		"mapped_actor": mappedActor,
	})
}

// handleGitTwinPubkey exposes the server's anchor signing pubkey so external
// verifiers can pin it. No auth: the pubkey is public by design, and we don't
// want a verifier to need a bridge secret just to read it. 404 when signing
// is disabled (ACP_ANCHOR_SIGNING_KEY unset) so clients distinguish "no key"
// from "wrong URL".
func (s *Server) handleGitTwinPubkey(w http.ResponseWriter, _ *http.Request) {
	if s.anchorSigner == nil {
		jsonError(w, "anchor signing disabled", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{
		"alg":        s.anchorSigner.Algorithm(),
		"public_key": base64.StdEncoding.EncodeToString(s.anchorSigner.PublicKey()),
	})
}

// handleGitTwinListPendingAnchors returns unwritten anchors the bridge still
// needs to commit to refs/notes/acp-anchors. Bridge-auth. Optional filters:
//
//	?repo_url=...  limit to one twinned repo
//	?limit=N       cap row count (defaults to 100)
func (s *Server) handleGitTwinListPendingAnchors(w http.ResponseWriter, r *http.Request) {
	if err := s.validateBridgeSecret(r); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	repoURL := r.URL.Query().Get("repo_url")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	anchors, err := gittwin.ListPendingAnchors(s.db, repoURL, limit)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if anchors == nil {
		anchors = []gittwin.Anchor{}
	}
	jsonOK(w, map[string]any{"anchors": anchors, "count": len(anchors)})
}

// handleGitTwinAckAnchor records that the bridge wrote the git note for this
// anchor. Bridge-auth. Body: {"written_commit_sha": "<40-hex>"}. Idempotent
// when the same SHA is re-acked; mismatched re-acks 409 to flag split-brain.
func (s *Server) handleGitTwinAckAnchor(w http.ResponseWriter, r *http.Request) {
	if err := s.validateBridgeSecret(r); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	anchorID := r.PathValue("anchor_id")
	if anchorID == "" {
		jsonError(w, "anchor_id path segment required", http.StatusBadRequest)
		return
	}
	var req struct {
		WrittenCommitSHA string `json:"written_commit_sha"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := gittwin.AckAnchor(s.db, anchorID, req.WrittenCommitSHA); err != nil {
		switch {
		case errors.Is(err, gittwin.ErrAnchorNotFound):
			jsonError(w, err.Error(), http.StatusNotFound)
		case strings.Contains(err.Error(), "already written"):
			jsonError(w, err.Error(), http.StatusConflict)
		default:
			jsonError(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	jsonOK(w, map[string]any{"ok": true, "anchor_id": anchorID})
}
