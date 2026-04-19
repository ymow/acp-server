// Package api wires the ACP MCP tools into a JSON-over-HTTP server.
//
// All tool endpoints follow the same envelope:
//
//	POST /tools/{tool_name}
//	Headers: X-Covenant-ID, X-Agent-ID, X-Session-Token
//	Body:    {"params": {...}}
//
//	200 OK   → {"receipt": {...}}
//	4xx/5xx  → {"error": "..."}
package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
	acpcrypto "github.com/inkmesh/acp-server/internal/crypto"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/gittwin"
	"github.com/inkmesh/acp-server/internal/keys"
	"github.com/inkmesh/acp-server/internal/ratelimit"
	"github.com/inkmesh/acp-server/internal/sessions"
	"github.com/inkmesh/acp-server/tools"
)

type Server struct {
	db           *sql.DB
	covSvc       *covenant.Service
	engine       *execution.Engine
	mux          *http.ServeMux
	anchorSigner gittwin.Signer
}

func New(db *sql.DB) *Server {
	covSvc := covenant.New(db)

	// ACR-700 Phase 4.5: wire the LocalKeyfileProvider so platform_id writes
	// populate platform_id_enc and the startup backfill pass hydrates any
	// legacy rows inserted before Phase 4.5 landed. A missing key file is
	// auto-generated with a one-shot fingerprint warning (§3.2); a malformed
	// key or loose permissions aborts startup.
	keyProvider, err := keys.NewLocalKeyfileProvider("")
	if err != nil {
		log.Fatalf("load acp master key: %v", err)
	}
	if keyProvider.WasFirstStart() {
		log.Printf("═══════════════════════════════════════════════════════════════")
		log.Printf("  ACP: new master key generated")
		log.Printf("  path:        %s", keyProvider.Path())
		log.Printf("  fingerprint: %s", keyProvider.Fingerprint())
		log.Printf("  ACTION:      Back up this file offline. Loss is permanent")
		log.Printf("               and every platform_id_enc in this database")
		log.Printf("               becomes unreadable.")
		log.Printf("═══════════════════════════════════════════════════════════════")
	} else {
		log.Printf("acp master key loaded (fingerprint: %s)", keyProvider.Fingerprint())
	}
	sealer := acpcrypto.NewSealer(keyProvider)
	covSvc.SetSealer(sealer)
	if n, err := covenant.BackfillPlatformIdentities(db, sealer); err != nil {
		log.Fatalf("backfill platform_identities: %v", err)
	} else if n > 0 {
		log.Printf("acp backfill: hydrated platform_id_hash/enc for %d legacy row(s)", n)
	}

	signer, err := gittwin.LoadSignerFromEnv()
	if err != nil {
		// A malformed signing key in env is an operator misconfiguration —
		// bail loud rather than silently fall back to unsigned anchors.
		log.Fatalf("load anchor signer: %v", err)
	}
	s := &Server{
		db:           db,
		covSvc:       covSvc,
		engine:       execution.New(db, covSvc),
		mux:          http.NewServeMux(),
		anchorSigner: signer,
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// ── Covenant management ──────────────────────────────────────────────────
	s.mux.HandleFunc("POST /covenants", s.handleCreateCovenant)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/tiers", s.handleAddTier)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/transition", s.handleTransition)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/join", s.handleJoin)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/apply", s.handleApplyToCovenant)
	s.mux.HandleFunc("GET /covenants/{covenant_id}", s.handleGetCovenant)
	s.mux.HandleFunc("GET /covenants/{covenant_id}/state", s.handleGetState)

	// ── MCP Tool execution ───────────────────────────────────────────────────
	s.mux.HandleFunc("POST /tools/propose_passage", s.toolHandler(&tools.ProposePassage{}))
	s.mux.HandleFunc("POST /tools/approve_draft", s.toolHandler(&tools.ApproveDraft{}))
	s.mux.HandleFunc("POST /tools/generate_settlement_output", s.toolHandler(&tools.GenerateSettlement{}))
	s.mux.HandleFunc("POST /tools/configure_token_rules", s.toolHandler(&tools.ConfigureTokenRules{}))
	s.mux.HandleFunc("POST /tools/configure_anti_gaming", s.toolHandler(&tools.ConfigureAntiGaming{}))
	s.mux.HandleFunc("POST /tools/approve_agent", s.toolHandler(&tools.ApproveAgent{}))
	s.mux.HandleFunc("POST /tools/confirm_settlement_output", s.toolHandler(&tools.ConfirmSettlementOutput{AnchorSigner: s.anchorSigner}))
	s.mux.HandleFunc("POST /tools/leave_covenant", s.toolHandler(&tools.LeaveCovenant{}))

	// ── Phase 2 admin tools (X-Owner-Token auth) ─────────────────────────────
	s.mux.HandleFunc("POST /tools/reject_agent", s.ownerToolHandler(&tools.RejectAgent{}))
	s.mux.HandleFunc("POST /tools/reject_draft", s.ownerToolHandler(&tools.RejectDraft{}))

	// ── Phase 4.6 ACR-50 access gate (owner admin) ───────────────────────────
	s.mux.HandleFunc("POST /tools/approve_agent_access", s.ownerToolHandler(&tools.ApproveAgentAccess{}))
	s.mux.HandleFunc("POST /tools/reject_agent_access", s.ownerToolHandler(&tools.RejectAgentAccess{}))

	// ── Phase 2 query tools ───────────────────────────────────────────────────
	s.mux.HandleFunc("POST /tools/get_token_balance", s.handleGetTokenBalance)
	s.mux.HandleFunc("POST /tools/list_members", s.handleListMembers)
	s.mux.HandleFunc("POST /tools/get_token_history", s.handleGetTokenHistory)
	s.mux.HandleFunc("POST /tools/get_concentration_status", s.handleGetConcentrationStatus)

	// ── Audit & verification ─────────────────────────────────────────────────
	s.mux.HandleFunc("GET /covenants/{covenant_id}/audit/verify", s.handleVerifyChain)
	s.mux.HandleFunc("GET /covenants/{covenant_id}/audit", s.handleGetAuditLog)

	// ── Budget ────────────────────────────────────────────────────────────────
	s.mux.HandleFunc("GET /covenants/{covenant_id}/budget", s.handleGetBudget)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/budget", s.handleSetBudget)

	// ── Session tokens (REVIEW-14) ────────────────────────────────────────────
	s.mux.HandleFunc("POST /sessions/issue", s.handleIssueToken)
	s.mux.HandleFunc("POST /sessions/rotate", s.handleRotateToken)

	// ── Git Twin (ACR-400) ────────────────────────────────────────────────────
	s.mux.HandleFunc("POST /covenants/{covenant_id}/git-twin", s.handleSetGitTwin)
	s.mux.HandleFunc("POST /git-twin/merge", s.handleGitTwinMerge)
	s.mux.HandleFunc("GET /git-twin/covenants", s.handleGitTwinFindCovenants)
	s.mux.HandleFunc("POST /git-twin/event", s.handleGitTwinRecordEvent)
	s.mux.HandleFunc("GET /git-twin/anchors/pending", s.handleGitTwinListPendingAnchors)
	s.mux.HandleFunc("POST /git-twin/anchors/{anchor_id}/ack", s.handleGitTwinAckAnchor)
	s.mux.HandleFunc("GET /git-twin/pubkey", s.handleGitTwinPubkey)
}

// ── Auth helpers ─────────────────────────────────────────────────────────────

// validateSession requires a valid X-Session-Token for the given agent+covenant.
// A-1: called at every /tools/* entry point.
func (s *Server) validateSession(r *http.Request, covenantID, agentID string) error {
	token := r.Header.Get("X-Session-Token")
	if token == "" {
		return errors.New("X-Session-Token header required")
	}
	valid, _ := sessions.Validate(s.db, token, agentID, covenantID)
	if !valid {
		return errors.New("invalid or expired session token")
	}
	return nil
}

// validateOwnerToken requires a valid X-Owner-Token matching the covenant's owner_token.
// A-2/A-4: called on /transition, /budget, /sessions/issue.
func (s *Server) validateOwnerToken(r *http.Request, covenantID string) error {
	token := r.Header.Get("X-Owner-Token")
	if token == "" {
		return errors.New("X-Owner-Token header required")
	}
	stored, err := s.covSvc.GetOwnerToken(covenantID)
	if err != nil || stored == "" || stored != token {
		return errors.New("invalid owner token")
	}
	return nil
}

// ── Covenant handlers ────────────────────────────────────────────────────────

func (s *Server) handleCreateCovenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title           string `json:"title"`
		SpaceType       string `json:"space_type"`
		OwnerPlatformID string `json:"owner_platform_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Title == "" || req.OwnerPlatformID == "" {
		jsonError(w, "title and owner_platform_id are required", http.StatusBadRequest)
		return
	}
	if req.SpaceType == "" {
		req.SpaceType = "book"
	}
	cov, owner, err := s.covSvc.Create(req.Title, req.SpaceType, req.OwnerPlatformID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"covenant": cov, "owner_agent_id": owner.AgentID})
}

func (s *Server) handleAddTier(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	var req struct {
		TierID          string  `json:"tier_id"`
		DisplayName     string  `json:"display_name"`
		TokenMultiplier float64 `json:"token_multiplier"`
		MaxSlots        *int    `json:"max_slots"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.TierID == "" {
		jsonError(w, "tier_id is required", http.StatusBadRequest)
		return
	}
	if req.TokenMultiplier == 0 {
		req.TokenMultiplier = 1.0
	}
	if err := s.covSvc.AddTier(covenantID, req.TierID, req.DisplayName, req.TokenMultiplier, req.MaxSlots); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	// A-4: only the covenant owner may trigger state transitions.
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		TargetState string `json:"target_state"`
	}
	if !decode(w, r, &req) {
		return
	}
	cov, err := s.covSvc.Transition(covenantID, req.TargetState)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"covenant": cov})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	var req struct {
		PlatformID string `json:"platform_id"`
		TierID     string `json:"tier_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	member, err := s.covSvc.Join(covenantID, req.PlatformID, req.TierID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"member": member})
}

// handleApplyToCovenant is the ACR-50 §2.2 request_agent_access surface. It
// differs from /join in three ways: (1) no covenant_members row is created —
// the request sits in agent_access_requests until the owner runs approve or
// reject; (2) the applicant's platform_id is sealed via the 4.5 Sealer and
// never echoed back in the response; (3) payment_ref + self_declaration are
// captured for the owner's review. Public endpoint: the applicant has no
// session yet, and cannot have one until approval.
func (s *Server) handleApplyToCovenant(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	var req struct {
		PlatformID      string `json:"platform_id"`
		TierID          string `json:"tier_id"`
		PaymentRef      string `json:"payment_ref"`
		SelfDeclaration string `json:"self_declaration"`
	}
	if !decode(w, r, &req) {
		return
	}
	ar, err := s.covSvc.CreateAccessRequest(
		covenantID, req.PlatformID, req.TierID, req.PaymentRef, req.SelfDeclaration,
	)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// ACR-50 §2.3 AgentAccessReceipt shape. platform_id is represented only by
	// the 12-char hash prefix so the applicant can verify "the server heard me"
	// without the server re-transmitting plaintext.
	hashPrefix := ""
	if len(ar.PlatformIDHash) >= 12 {
		hashPrefix = ar.PlatformIDHash[:12]
	}
	jsonOK(w, map[string]any{
		"request_id":              ar.RequestID,
		"covenant_id":             ar.CovenantID,
		"tier_id":                 ar.TierID,
		"payment_ref":             ar.PaymentRef,
		"status":                  ar.Status,
		"platform_id_hash_prefix": hashPrefix,
		"created_at":              ar.CreatedAt,
	})
}

func (s *Server) handleGetCovenant(w http.ResponseWriter, r *http.Request) {
	cov, err := s.covSvc.Get(r.PathValue("covenant_id"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]any{"covenant": cov})
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	agentID := r.URL.Query().Get("agent_id")

	cov, err := s.covSvc.Get(covenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	state := map[string]any{
		"covenant_id":      covenantID,
		"lifecycle_status": cov.State,
	}

	if agentID != "" {
		mem, err := s.covSvc.GetMember(covenantID, agentID)
		if err == nil {
			state["agent_id"] = agentID
			state["agent_status"] = mem.Status
		}
	}

	budState, _ := budget.GetState(s.db, covenantID)
	state["budget"] = map[string]any{
		"limit":     budState.BudgetLimit,
		"spent":     budState.BudgetSpent,
		"remaining": budState.Remaining(),
		"currency":  budState.Currency,
	}

	jsonOK(w, state)
}

// ── MCP Tool handler ─────────────────────────────────────────────────────────

func (s *Server) toolHandler(tool execution.Tool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		covenantID := r.Header.Get("X-Covenant-ID")
		agentID := r.Header.Get("X-Agent-ID")
		sessionToken := r.Header.Get("X-Session-Token")

		if covenantID == "" || agentID == "" {
			jsonError(w, "X-Covenant-ID and X-Agent-ID headers are required", http.StatusBadRequest)
			return
		}

		// A-1: session token is mandatory for all tool endpoints.
		if sessionToken == "" {
			jsonError(w, "X-Session-Token header required", http.StatusUnauthorized)
			return
		}
		valid, inGrace := sessions.Validate(s.db, sessionToken, agentID, covenantID)
		if !valid {
			jsonError(w, "invalid or expired session token", http.StatusUnauthorized)
			return
		}
		if inGrace {
			w.Header().Set("Acp-Token-Warning",
				"Token in grace period. Rotate immediately.")
		}

		var body struct {
			Params map[string]any `json:"params"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Params == nil {
			body.Params = map[string]any{}
		}

		receipt, err := s.engine.Run(covenantID, agentID, sha256Hex(sessionToken), tool, body.Params)
		if err != nil {
			// Distinguish auth/validation errors (4xx) from internal errors (5xx)
			status := http.StatusInternalServerError
			msg := err.Error()
			if strings.HasPrefix(msg, "step1.forbidden:") {
				status = http.StatusForbidden
			} else if strings.HasPrefix(msg, "step1:") || strings.HasPrefix(msg, "step2:") ||
				strings.HasPrefix(msg, "step2.5:") || strings.HasPrefix(msg, "step3:") {
				status = http.StatusBadRequest
			}
			jsonError(w, msg, status)
			return
		}
		jsonOK(w, map[string]any{"receipt": receipt})
	}
}

// ownerToolHandler wraps admin tools that authenticate via X-Owner-Token instead of X-Session-Token.
// It finds the covenant owner's agentID and runs the tool through the execution engine.
func (s *Server) ownerToolHandler(tool execution.Tool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		covenantID := r.Header.Get("X-Covenant-ID")
		if covenantID == "" {
			jsonError(w, "X-Covenant-ID header required", http.StatusBadRequest)
			return
		}
		ownerToken := r.Header.Get("X-Owner-Token")
		if ownerToken == "" {
			jsonError(w, "X-Owner-Token header required", http.StatusUnauthorized)
			return
		}
		if err := s.validateOwnerToken(r, covenantID); err != nil {
			jsonError(w, err.Error(), http.StatusUnauthorized)
			return
		}
		agentID, err := s.covSvc.GetOwnerAgentID(covenantID)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		var body struct {
			Params map[string]any `json:"params"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Params == nil {
			body.Params = map[string]any{}
		}

		receipt, err := s.engine.Run(covenantID, agentID, sha256Hex(ownerToken), tool, body.Params)
		if err != nil {
			status := http.StatusInternalServerError
			msg := err.Error()
			if strings.HasPrefix(msg, "step1.forbidden:") {
				status = http.StatusForbidden
			} else if strings.HasPrefix(msg, "step1:") || strings.HasPrefix(msg, "step2:") ||
				strings.HasPrefix(msg, "step2.5:") || strings.HasPrefix(msg, "step3:") {
				status = http.StatusBadRequest
			}
			jsonError(w, msg, status)
			return
		}
		jsonOK(w, map[string]any{"receipt": receipt})
	}
}

// handleGetTokenBalance returns confirmed/pending/rejected token totals for an agent.
// Requires valid X-Session-Token.
func (s *Server) handleGetTokenBalance(w http.ResponseWriter, r *http.Request) {
	sessionToken := r.Header.Get("X-Session-Token")
	if sessionToken == "" {
		jsonError(w, "X-Session-Token header required", http.StatusUnauthorized)
		return
	}

	var body struct {
		Params struct {
			CovenantID string `json:"covenant_id"`
			AgentID    string `json:"agent_id"`
		} `json:"params"`
	}
	if !decode(w, r, &body) {
		return
	}
	covenantID := body.Params.CovenantID
	agentID := body.Params.AgentID
	if covenantID == "" || agentID == "" {
		jsonError(w, "covenant_id and agent_id are required", http.StatusBadRequest)
		return
	}

	valid, _ := sessions.Validate(s.db, sessionToken, agentID, covenantID)
	if !valid {
		jsonError(w, "invalid or expired session token", http.StatusUnauthorized)
		return
	}

	type balRow struct {
		status string
		total  int
	}
	rows, err := s.db.Query(`
		SELECT status, COALESCE(SUM(delta), 0)
		FROM token_ledger
		WHERE covenant_id=? AND agent_id=?
		GROUP BY status`,
		covenantID, agentID,
	)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var confirmed, pending, rejected int
	for rows.Next() {
		var st string
		var total int
		if err := rows.Scan(&st, &total); err != nil {
			continue
		}
		switch st {
		case "confirmed":
			confirmed = total
		case "pending":
			pending = total
		case "rejected":
			rejected = total
		}
	}
	// ACR-20 Part 7: covenant-wide rank by confirmed tokens. Dense rank
	// (ties share a rank); returned as optional so a zero-balance agent
	// gets rank=0 rather than pretending to be below the field.
	var rank int
	if confirmed > 0 {
		_ = s.db.QueryRow(`
			SELECT 1 + COUNT(DISTINCT other.total)
			FROM (SELECT agent_id, SUM(delta) AS total
			      FROM token_ledger
			      WHERE covenant_id=? AND status='confirmed'
			      GROUP BY agent_id) AS other
			WHERE other.total > ?`,
			covenantID, confirmed,
		).Scan(&rank)
	}

	jsonOK(w, map[string]any{
		"confirmed_tokens": confirmed,
		"pending_tokens":   pending,
		"rejected_tokens":  rejected,
		"total_tokens":     confirmed + pending,
		"rank":             rank,
	})
}

// handleGetTokenHistory returns ledger entries for an agent in a covenant.
// ACR-20 Part 7: optional from/to (ISO 8601) and status filter. Requires a
// valid X-Session-Token bound to the same agent.
func (s *Server) handleGetTokenHistory(w http.ResponseWriter, r *http.Request) {
	sessionToken := r.Header.Get("X-Session-Token")
	if sessionToken == "" {
		jsonError(w, "X-Session-Token header required", http.StatusUnauthorized)
		return
	}
	var body struct {
		Params struct {
			CovenantID string `json:"covenant_id"`
			AgentID    string `json:"agent_id"`
			From       string `json:"from"`
			To         string `json:"to"`
			Status     string `json:"status"`
		} `json:"params"`
	}
	if !decode(w, r, &body) {
		return
	}
	p := body.Params
	if p.CovenantID == "" || p.AgentID == "" {
		jsonError(w, "covenant_id and agent_id are required", http.StatusBadRequest)
		return
	}
	if valid, _ := sessions.Validate(s.db, sessionToken, p.AgentID, p.CovenantID); !valid {
		jsonError(w, "invalid or expired session token", http.StatusUnauthorized)
		return
	}

	// Join audit_logs for timestamp; token_ledger has no timestamp column.
	query := `
		SELECT l.id, l.log_id, l.delta, l.balance_after, l.source_type, l.source_ref, l.status, a.timestamp
		FROM token_ledger l
		JOIN audit_logs a ON a.log_id = l.log_id
		WHERE l.covenant_id=? AND l.agent_id=?`
	args := []any{p.CovenantID, p.AgentID}
	if p.Status != "" {
		query += ` AND l.status=?`
		args = append(args, p.Status)
	}
	if p.From != "" {
		query += ` AND a.timestamp >= ?`
		args = append(args, p.From)
	}
	if p.To != "" {
		query += ` AND a.timestamp <= ?`
		args = append(args, p.To)
	}
	query += ` ORDER BY a.timestamp ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type entry struct {
		EntryID      string `json:"entry_id"`
		LogID        string `json:"log_id"`
		Delta        int    `json:"tokens_delta"`
		BalanceAfter int    `json:"balance_after"`
		SourceType   string `json:"source_type"`
		SourceRef    string `json:"source_ref"`
		Status       string `json:"status"`
		Timestamp    string `json:"timestamp"`
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.EntryID, &e.LogID, &e.Delta, &e.BalanceAfter,
			&e.SourceType, &e.SourceRef, &e.Status, &e.Timestamp); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		entries = append(entries, e)
	}
	if entries == nil {
		entries = []entry{}
	}
	jsonOK(w, map[string]any{"entries": entries, "count": len(entries)})
}

// handleListMembers returns all covenant members with their token totals.
// Requires valid X-Owner-Token.
func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Params struct {
			CovenantID string `json:"covenant_id"`
		} `json:"params"`
	}
	if !decode(w, r, &body) {
		return
	}
	covenantID := body.Params.CovenantID
	if covenantID == "" {
		jsonError(w, "covenant_id is required", http.StatusBadRequest)
		return
	}
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	rows, err := s.db.Query(`
		SELECT m.agent_id, m.status, COALESCE(m.tier_id,''), m.joined_at,
		       COALESCE(SUM(CASE WHEN l.status='confirmed' THEN l.delta ELSE 0 END), 0),
		       COALESCE(pi.platform_id_hash, '')
		FROM covenant_members m
		LEFT JOIN token_ledger l ON l.covenant_id=m.covenant_id AND l.agent_id=m.agent_id
		LEFT JOIN platform_identities pi ON pi.platform_id = m.platform_id
		WHERE m.covenant_id=?
		GROUP BY m.agent_id, m.status, m.tier_id, m.joined_at, pi.platform_id_hash`,
		covenantID,
	)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// ACR-700 §4: surface a 12-char platform_id_hash prefix instead of the
	// plaintext platform_id. The owner can correlate across covenants without
	// the server ever returning the full identifier.
	type memberRow struct {
		AgentID              string `json:"agent_id"`
		Status               string `json:"status"`
		TierID               string `json:"tier_id,omitempty"`
		JoinedAt             string `json:"joined_at"`
		ConfirmedTokens      int    `json:"confirmed_tokens"`
		PlatformIDHashPrefix string `json:"platform_id_hash_prefix,omitempty"`
	}
	var members []memberRow
	for rows.Next() {
		var (
			m    memberRow
			hash string
		)
		if err := rows.Scan(&m.AgentID, &m.Status, &m.TierID, &m.JoinedAt, &m.ConfirmedTokens, &hash); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(hash) >= 12 {
			m.PlatformIDHashPrefix = hash[:12]
		}
		members = append(members, m)
	}
	if members == nil {
		members = []memberRow{}
	}

	// ACR-50 §7: owners need the pending access queue alongside current
	// members to run the approve/reject flow without a second request.
	// Same 12-char hash-prefix redaction rule — plaintext platform_id never
	// leaves the DB.
	type pendingRow struct {
		RequestID            string `json:"request_id"`
		PlatformIDHashPrefix string `json:"platform_id_hash_prefix"`
		TierID               string `json:"tier_id"`
		PaymentRef           string `json:"payment_ref,omitempty"`
		SelfDeclaration      string `json:"self_declaration,omitempty"`
		CreatedAt            string `json:"created_at"`
	}
	pendingReqs, err := s.covSvc.ListPendingAccessRequests(covenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending := make([]pendingRow, 0, len(pendingReqs))
	for _, r := range pendingReqs {
		row := pendingRow{
			RequestID:       r.RequestID,
			TierID:          r.TierID,
			PaymentRef:      r.PaymentRef,
			SelfDeclaration: r.SelfDeclaration,
			CreatedAt:       r.CreatedAt.Format(time.RFC3339Nano),
		}
		if len(r.PlatformIDHash) >= 12 {
			row.PlatformIDHashPrefix = r.PlatformIDHash[:12]
		}
		pending = append(pending, row)
	}

	jsonOK(w, map[string]any{
		"members":                 members,
		"pending_access_requests": pending,
	})
}

// handleGetConcentrationStatus returns the full ACR-20 Part 4 Layer 5 report
// for a covenant: threshold, total confirmed tokens, per-agent shares, and the
// warnings subset. Owner-only — concentration data exposes each member's
// relative position in the pool and is not general-participant information.
func (s *Server) handleGetConcentrationStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Params struct {
			CovenantID string `json:"covenant_id"`
		} `json:"params"`
	}
	if !decode(w, r, &body) {
		return
	}
	covenantID := body.Params.CovenantID
	if covenantID == "" {
		jsonError(w, "covenant_id is required", http.StatusBadRequest)
		return
	}
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}

	report, err := ratelimit.CheckConcentration(s.db, covenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Normalise empty slices to JSON arrays so clients can iterate without
	// null-guarding. The domain report uses nil to signal "no data".
	entries := report.Entries
	if entries == nil {
		entries = []ratelimit.ConcentrationEntry{}
	}
	warnings := report.Warnings
	if warnings == nil {
		warnings = []ratelimit.ConcentrationEntry{}
	}
	jsonOK(w, map[string]any{
		"threshold_pct": report.Threshold,
		"total_tokens":  report.Total,
		"entries":       entries,
		"warnings":      warnings,
	})
}

// ── Audit handlers ───────────────────────────────────────────────────────────

func (s *Server) handleVerifyChain(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	valid, violations := audit.VerifyChain(s.db, covenantID)
	jsonOK(w, map[string]any{
		"covenant_id": covenantID,
		"valid":       valid,
		"violations":  violations,
	})
}

func (s *Server) handleGetAuditLog(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	// A-4: any covenant participant may read the audit log.
	sessionToken := r.Header.Get("X-Session-Token")
	if sessionToken == "" || !sessions.ValidateForCovenant(s.db, sessionToken, covenantID) {
		jsonError(w, "valid X-Session-Token for this covenant required", http.StatusUnauthorized)
		return
	}
	limit := 50
	rows, err := s.db.Query(`
		SELECT log_id, sequence, agent_id, tool_name, result, tokens_delta,
		       cost_delta, cost_currency, net_delta, state_before, state_after, timestamp, hash
		FROM audit_logs WHERE covenant_id=? ORDER BY sequence DESC LIMIT ?`,
		covenantID, limit)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type row struct {
		LogID        string  `json:"log_id"`
		Sequence     int     `json:"sequence"`
		AgentID      string  `json:"agent_id"`
		ToolName     string  `json:"tool_name"`
		Result       string  `json:"result"`
		TokensDelta  int     `json:"tokens_delta"`
		CostDelta    int64   `json:"cost_delta"`    // minor units of CostCurrency
		CostCurrency string  `json:"cost_currency"` // ISO 4217 (ACR-300@2.2)
		NetDelta     float64 `json:"net_delta"`
		StateBefore  string  `json:"state_before"`
		StateAfter   string  `json:"state_after"`
		Timestamp    string  `json:"timestamp"`
		Hash         string  `json:"hash"`
	}
	var entries []row
	for rows.Next() {
		var e row
		rows.Scan(&e.LogID, &e.Sequence, &e.AgentID, &e.ToolName, &e.Result,
			&e.TokensDelta, &e.CostDelta, &e.CostCurrency, &e.NetDelta, &e.StateBefore, &e.StateAfter,
			&e.Timestamp, &e.Hash)
		entries = append(entries, e)
	}
	jsonOK(w, map[string]any{"entries": entries, "count": len(entries)})
}

// ── Budget handlers ───────────────────────────────────────────────────────────

func (s *Server) handleGetBudget(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	// A-4: budget is owner-only information.
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	state, err := budget.GetState(s.db, covenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, state)
}

func (s *Server) handleSetBudget(w http.ResponseWriter, r *http.Request) {
	covenantID := r.PathValue("covenant_id")
	// A-4: only the covenant owner may set the budget.
	if err := s.validateOwnerToken(r, covenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var req struct {
		BudgetLimit int64  `json:"budget_limit"` // minor units of Currency
		Currency    string `json:"currency"`     // ISO 4217; defaults to "USD" when empty
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}
	if err := budget.EnsureCounter(s.db, covenantID, req.BudgetLimit, req.Currency); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "budget_limit": req.BudgetLimit, "currency": req.Currency})
}

// ── Session token handlers ────────────────────────────────────────────────────

func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID    string `json:"agent_id"`
		CovenantID string `json:"covenant_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	// A-2: only the covenant owner may issue session tokens.
	if err := s.validateOwnerToken(r, req.CovenantID); err != nil {
		jsonError(w, err.Error(), http.StatusUnauthorized)
		return
	}
	raw, err := sessions.Issue(s.db, req.AgentID, req.CovenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"token":   raw,
		"warning": "Store this token securely. It will not be shown again.",
	})
}

func (s *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID    string `json:"agent_id"`
		CovenantID string `json:"covenant_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	// A-3: only the token holder may rotate their own token.
	currentToken := r.Header.Get("X-Session-Token")
	if currentToken == "" {
		jsonError(w, "X-Session-Token header required", http.StatusUnauthorized)
		return
	}
	valid, _ := sessions.Validate(s.db, currentToken, req.AgentID, req.CovenantID)
	if !valid {
		jsonError(w, "invalid or expired session token", http.StatusUnauthorized)
		return
	}
	newRaw, warning, err := sessions.Rotate(s.db, req.AgentID, req.CovenantID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Acp-Token-Warning", warning)
	jsonOK(w, map[string]any{"token": newRaw, "warning": warning})
}

// ── JSON helpers ──────────────────────────────────────────────────────────────

// sha256Hex returns the hex-encoded SHA-256 digest of s.
// Used to store a token fingerprint in audit logs instead of the raw token.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
