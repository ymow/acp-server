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
	"net/http"
	"strings"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
	"github.com/inkmesh/acp-server/internal/execution"
	"github.com/inkmesh/acp-server/internal/sessions"
	"github.com/inkmesh/acp-server/tools"
)

type Server struct {
	db     *sql.DB
	covSvc *covenant.Service
	engine *execution.Engine
	mux    *http.ServeMux
}

func New(db *sql.DB) *Server {
	covSvc := covenant.New(db)
	s := &Server{
		db:     db,
		covSvc: covSvc,
		engine: execution.New(db, covSvc),
		mux:    http.NewServeMux(),
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
	s.mux.HandleFunc("GET /covenants/{covenant_id}", s.handleGetCovenant)
	s.mux.HandleFunc("GET /covenants/{covenant_id}/state", s.handleGetState)

	// ── MCP Tool execution ───────────────────────────────────────────────────
	s.mux.HandleFunc("POST /tools/propose_passage", s.toolHandler(&tools.ProposePassage{}))
	s.mux.HandleFunc("POST /tools/approve_draft", s.toolHandler(&tools.ApproveDraft{}))
	s.mux.HandleFunc("POST /tools/generate_settlement_output", s.toolHandler(&tools.GenerateSettlement{}))
	s.mux.HandleFunc("POST /tools/configure_token_rules", s.toolHandler(&tools.ConfigureTokenRules{}))
	s.mux.HandleFunc("POST /tools/approve_agent", s.toolHandler(&tools.ApproveAgent{}))
	s.mux.HandleFunc("POST /tools/confirm_settlement_output", s.toolHandler(&tools.ConfirmSettlementOutput{}))

	// ── Phase 2 admin tools (X-Owner-Token auth) ─────────────────────────────
	s.mux.HandleFunc("POST /tools/reject_agent", s.ownerToolHandler(&tools.RejectAgent{}))
	s.mux.HandleFunc("POST /tools/reject_draft", s.ownerToolHandler(&tools.RejectDraft{}))

	// ── Phase 2 query tools ───────────────────────────────────────────────────
	s.mux.HandleFunc("POST /tools/get_token_balance", s.handleGetTokenBalance)
	s.mux.HandleFunc("POST /tools/list_members", s.handleListMembers)

	// ── Audit & verification ─────────────────────────────────────────────────
	s.mux.HandleFunc("GET /covenants/{covenant_id}/audit/verify", s.handleVerifyChain)
	s.mux.HandleFunc("GET /covenants/{covenant_id}/audit", s.handleGetAuditLog)

	// ── Budget ────────────────────────────────────────────────────────────────
	s.mux.HandleFunc("GET /covenants/{covenant_id}/budget", s.handleGetBudget)
	s.mux.HandleFunc("POST /covenants/{covenant_id}/budget", s.handleSetBudget)

	// ── Session tokens (REVIEW-14) ────────────────────────────────────────────
	s.mux.HandleFunc("POST /sessions/issue", s.handleIssueToken)
	s.mux.HandleFunc("POST /sessions/rotate", s.handleRotateToken)
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
	jsonOK(w, map[string]any{
		"confirmed_tokens": confirmed,
		"pending_tokens":   pending,
		"rejected_tokens":  rejected,
	})
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
		       COALESCE(SUM(CASE WHEN l.status='confirmed' THEN l.delta ELSE 0 END), 0)
		FROM covenant_members m
		LEFT JOIN token_ledger l ON l.covenant_id=m.covenant_id AND l.agent_id=m.agent_id
		WHERE m.covenant_id=?
		GROUP BY m.agent_id, m.status, m.tier_id, m.joined_at`,
		covenantID,
	)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type memberRow struct {
		AgentID         string `json:"agent_id"`
		Status          string `json:"status"`
		TierID          string `json:"tier_id,omitempty"`
		JoinedAt        string `json:"joined_at"`
		ConfirmedTokens int    `json:"confirmed_tokens"`
	}
	var members []memberRow
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.AgentID, &m.Status, &m.TierID, &m.JoinedAt, &m.ConfirmedTokens); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		members = append(members, m)
	}
	if members == nil {
		members = []memberRow{}
	}
	jsonOK(w, map[string]any{"members": members})
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
		       cost_delta, net_delta, state_before, state_after, timestamp, hash
		FROM audit_logs WHERE covenant_id=? ORDER BY sequence DESC LIMIT ?`,
		covenantID, limit)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type row struct {
		LogID       string  `json:"log_id"`
		Sequence    int     `json:"sequence"`
		AgentID     string  `json:"agent_id"`
		ToolName    string  `json:"tool_name"`
		Result      string  `json:"result"`
		TokensDelta int     `json:"tokens_delta"`
		CostDelta   float64 `json:"cost_delta"`
		NetDelta    float64 `json:"net_delta"`
		StateBefore string  `json:"state_before"`
		StateAfter  string  `json:"state_after"`
		Timestamp   string  `json:"timestamp"`
		Hash        string  `json:"hash"`
	}
	var entries []row
	for rows.Next() {
		var e row
		rows.Scan(&e.LogID, &e.Sequence, &e.AgentID, &e.ToolName, &e.Result,
			&e.TokensDelta, &e.CostDelta, &e.NetDelta, &e.StateBefore, &e.StateAfter,
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
		BudgetLimit float64 `json:"budget_limit"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := budget.EnsureCounter(s.db, covenantID, req.BudgetLimit); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"ok": true, "budget_limit": req.BudgetLimit})
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
