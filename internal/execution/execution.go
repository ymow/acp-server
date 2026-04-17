// Package execution implements the ACP Execution Layer v0.2: eight-step flow.
// Steps are immutable and must not be reordered.
package execution

import (
	"database/sql"
	"fmt"

	"github.com/inkmesh/acp-server/internal/audit"
	"github.com/inkmesh/acp-server/internal/budget"
	"github.com/inkmesh/acp-server/internal/covenant"
)

// SideEffects carries the computed outputs of Step 4.
// CostDelta is USD cents; NetDelta stays float because cost_weight is a
// real-number multiplier.
type SideEffects struct {
	TokensDelta int
	CostDelta   int64
	NetDelta    float64
	StateAfter  string
}

// Receipt is returned from Step 8.
type Receipt struct {
	ReceiptID     string         `json:"receipt_id"`
	CovenantID    string         `json:"covenant_id"`
	AgentID       string         `json:"agent_id"`
	ToolName      string         `json:"tool_name"`
	Status        string         `json:"status"`
	TokensAwarded int            `json:"tokens_awarded"`
	CostDelta     int64          `json:"cost_delta"` // USD cents
	NetDelta      float64        `json:"net_delta"`
	Timestamp     string         `json:"timestamp"`
	LogHash       string         `json:"log_hash"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// Context holds the verified identity and covenant, populated by Step 1.
type Context struct {
	Covenant    *covenant.Covenant
	Member      *covenant.Member
	DB          *sql.DB
	CovenantSvc *covenant.Service
}

// Tool is the core interface every ACP clause/admin tool must satisfy.
// The engine calls these methods on every Run; optional capabilities are
// declared via the separate mixin interfaces below (CostEstimator,
// ReceiptEnricher, PolicyAware) so that adding a new cross-cutting
// concern does not force every tool file to change.
type Tool interface {
	ToolName() string
	ToolType() string

	// Step 2: additional precondition checks (identity already verified by engine)
	CheckPreconditions(ctx *Context, params map[string]any) error

	// Step 3: execute core logic, return result data
	ExecuteLogic(ctx *Context, params map[string]any) (map[string]any, error)

	// Step 4: derive side effects from result data (no DB writes)
	CalculateSideEffects(ctx *Context, result map[string]any, params map[string]any) SideEffects

	// Step 6: apply side effects (token writes, state changes, etc.)
	ApplySideEffects(ctx *Context, log *audit.Entry, effects SideEffects, result map[string]any, params map[string]any) error
}

// ── Optional capability interfaces ────────────────────────────────────────
//
// A tool implements zero or more of these. Adding a new capability should
// mean adding a new interface here, not modifying Tool — this is the whole
// point of the split. Engine resolution is via type assertion.

// CostEstimator is implemented by tools that charge an external cost (x402,
// API fees, etc.). Tools that do not implement it contribute 0 to the
// budget gate and produce cost_delta=0 in the audit log.
type CostEstimator interface {
	EstimateCost(ctx *Context, params map[string]any) int64 // USD cents
}

// ReceiptEnricher is implemented by tools that want to append extra fields
// to the Step 8 receipt (status tags, output IDs, confirmation timestamps).
// Tools that do not implement it return a bare receipt.
type ReceiptEnricher interface {
	EnrichReceipt(receipt *Receipt, result map[string]any)
}

// PolicyAware is implemented by tools that want to override the default
// params-preview masking. The engine falls back to DefaultParamsPolicy()
// when a tool does not implement it.
type PolicyAware interface {
	ParamsPolicy() ParamsPolicy
}

// resolveCost returns the tool's cost estimate (0 if no CostEstimator).
func resolveCost(tool Tool, ctx *Context, params map[string]any) int64 {
	if c, ok := tool.(CostEstimator); ok {
		return c.EstimateCost(ctx, params)
	}
	return 0
}

// resolveEnrich applies any ReceiptEnricher the tool declares. No-op if absent.
func resolveEnrich(tool Tool, receipt *Receipt, result map[string]any) {
	if e, ok := tool.(ReceiptEnricher); ok {
		e.EnrichReceipt(receipt, result)
	}
}

// resolvePolicy returns the tool's declared ParamsPolicy or the default.
func resolvePolicy(tool Tool) ParamsPolicy {
	if p, ok := tool.(PolicyAware); ok {
		return p.ParamsPolicy()
	}
	return DefaultParamsPolicy()
}

// Engine runs the eight-step ACP execution flow.
type Engine struct {
	db  *sql.DB
	cov *covenant.Service
}

func New(db *sql.DB, covSvc *covenant.Service) *Engine {
	return &Engine{db: db, cov: covSvc}
}

// Run executes a tool against the given covenant/agent/session.
func (e *Engine) Run(covenantID, agentID, sessionID string, tool Tool, params map[string]any) (*Receipt, error) {
	// ── Step 1: Identity verification ─────────────────────────────────────
	cov, err := e.cov.Get(covenantID)
	if err != nil {
		return nil, fmt.Errorf("step1: covenant not found: %w", err)
	}
	mem, err := e.cov.GetMember(covenantID, agentID)
	if err != nil {
		return nil, fmt.Errorf("step1: agent not in covenant: %w", err)
	}
	if mem.Status != "active" {
		return nil, fmt.Errorf("step1.forbidden: agent status %q does not permit execution", mem.Status)
	}

	ctx := &Context{Covenant: cov, Member: mem, DB: e.db, CovenantSvc: e.cov}

	// ── Step 2: Pre-condition check ────────────────────────────────────────
	if err := tool.CheckPreconditions(ctx, params); err != nil {
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step2: %w", err)
	}

	// ── Step 2.5: Budget gate (WI6: check-only, create reservation) ───────
	estimated := resolveCost(tool, ctx, params)
	reservationID, err := budget.CheckAndReserve(e.db, covenantID, estimated)
	if err != nil {
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step2.5: %w", err)
	}

	// ── Step 3: Execute core logic ─────────────────────────────────────────
	result, err := tool.ExecuteLogic(ctx, params)
	if err != nil {
		budget.ReleaseReservation(e.db, reservationID)
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step3: %w", err)
	}

	// ── Step 4: Calculate side effects ────────────────────────────────────
	effects := tool.CalculateSideEffects(ctx, result, params)
	if effects.StateAfter == "" {
		effects.StateAfter = cov.State
	}
	// ACR-20 §6 / Execution Layer §Step 4c: if the tool did not set its own
	// cost_delta, fall back to the Step 2.5 estimate — otherwise audit_logs
	// records cost_delta=0 while budget_counters actually deducts the cost,
	// breaking reject_draft refunds and net-contribution accounting.
	if effects.CostDelta == 0 && estimated > 0 {
		effects.CostDelta = estimated
	}
	effects.NetDelta = float64(effects.TokensDelta) - cov.CostWeight*float64(effects.CostDelta)

	// ── Step 5: Write Audit Log  ← MUST precede Step 6 ───────────────────
	maskedParams := ApplyParamsPolicy(params, resolvePolicy(tool))
	logEntry, err := audit.LogEvent(e.db, audit.Entry{
		CovenantID:    covenantID,
		AgentID:       agentID,
		SessionID:     sessionID,
		ToolName:      tool.ToolName(),
		ToolType:      tool.ToolType(),
		ParamsPreview: maskedParams,
		Result:        "success",
		ResultDetail:  strVal(result, "detail"),
		TokensDelta:   effects.TokensDelta,
		CostDelta:     effects.CostDelta,
		NetDelta:      effects.NetDelta,
		StateBefore:   cov.State,
		StateAfter:    effects.StateAfter,
	})
	if err != nil {
		budget.ReleaseReservation(e.db, reservationID)
		return nil, fmt.Errorf("step5: audit log: %w", err)
	}

	// ── Step 6: Apply side effects ─────────────────────────────────────────
	if err := tool.ApplySideEffects(ctx, logEntry, effects, result, params); err != nil {
		budget.ReleaseReservation(e.db, reservationID)
		return nil, fmt.Errorf("step6: %w", err)
	}

	// ── Step 7: Settle budget (WI6: actual deduction happens here) ────────
	if err := budget.RecordSpend(e.db, covenantID, estimated, reservationID, logEntry.LogID); err != nil {
		return nil, fmt.Errorf("step7: budget update: %w", err)
	}

	// ── Step 8: Return receipt ─────────────────────────────────────────────
	status := "accepted"
	if b, ok := result["is_final"].(bool); ok && !b {
		status = "pending"
	}
	receipt := &Receipt{
		ReceiptID:     logEntry.LogID,
		CovenantID:    covenantID,
		AgentID:       agentID,
		ToolName:      tool.ToolName(),
		Status:        status,
		TokensAwarded: effects.TokensDelta,
		CostDelta:     effects.CostDelta,
		NetDelta:      effects.NetDelta,
		Timestamp:     logEntry.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		LogHash:       logEntry.Hash,
		Extra:         map[string]any{},
	}
	resolveEnrich(tool, receipt, result)
	return receipt, nil
}

func (e *Engine) logRejection(covenantID, agentID, sessionID string, tool Tool, params map[string]any, state, reason string) {
	audit.LogEvent(e.db, audit.Entry{
		CovenantID:    covenantID,
		AgentID:       agentID,
		SessionID:     sessionID,
		ToolName:      tool.ToolName(),
		ToolType:      tool.ToolType(),
		ParamsPreview: ApplyParamsPolicy(params, resolvePolicy(tool)),
		Result:        "rejected",
		ResultDetail:  reason,
		StateBefore:   state,
		StateAfter:    state,
	})
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}
