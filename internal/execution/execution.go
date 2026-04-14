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
type SideEffects struct {
	TokensDelta int
	CostDelta   float64
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
	CostDelta     float64        `json:"cost_delta"`
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

// Tool is the interface every ACP clause/admin tool must satisfy.
type Tool interface {
	ToolName() string
	ToolType() string

	// Step 2: additional precondition checks (identity already verified by engine)
	CheckPreconditions(ctx *Context, params map[string]any) error

	// Step 2.5: estimated cost for budget gate; return 0 to skip
	EstimateCost(ctx *Context, params map[string]any) float64

	// Step 3: execute core logic, return result data
	ExecuteLogic(ctx *Context, params map[string]any) (map[string]any, error)

	// Step 4: derive side effects from result data (no DB writes)
	CalculateSideEffects(ctx *Context, result map[string]any, params map[string]any) SideEffects

	// Step 6: apply side effects (token writes, state changes, etc.)
	ApplySideEffects(ctx *Context, log *audit.Entry, effects SideEffects, result map[string]any, params map[string]any) error

	// Step 8: enrich the receipt (optional extra fields)
	EnrichReceipt(receipt *Receipt, result map[string]any)
}

// Engine runs the eight-step ACP execution flow.
type Engine struct {
	db      *sql.DB
	cov     *covenant.Service
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
		return nil, fmt.Errorf("step1: agent status %q does not permit execution", mem.Status)
	}

	ctx := &Context{Covenant: cov, Member: mem, DB: e.db, CovenantSvc: e.cov}

	// ── Step 2: Pre-condition check ────────────────────────────────────────
	if err := tool.CheckPreconditions(ctx, params); err != nil {
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step2: %w", err)
	}

	// ── Step 2.5: Budget gate ──────────────────────────────────────────────
	estimated := tool.EstimateCost(ctx, params)
	if err := budget.CheckAndReserve(e.db, covenantID, estimated); err != nil {
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step2.5: %w", err)
	}

	// ── Step 3: Execute core logic ─────────────────────────────────────────
	result, err := tool.ExecuteLogic(ctx, params)
	if err != nil {
		e.logRejection(covenantID, agentID, sessionID, tool, params, cov.State, err.Error())
		return nil, fmt.Errorf("step3: %w", err)
	}

	// ── Step 4: Calculate side effects ────────────────────────────────────
	effects := tool.CalculateSideEffects(ctx, result, params)
	if effects.StateAfter == "" {
		effects.StateAfter = cov.State
	}

	// ── Step 5: Write Audit Log  ← MUST precede Step 6 ───────────────────
	maskedParams := maskSensitive(params)
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
		return nil, fmt.Errorf("step5: audit log: %w", err)
	}

	// ── Step 6: Apply side effects ─────────────────────────────────────────
	if err := tool.ApplySideEffects(ctx, logEntry, effects, result, params); err != nil {
		return nil, fmt.Errorf("step6: %w", err)
	}

	// ── Step 7: Update budget counter ─────────────────────────────────────
	if err := budget.RecordSpend(e.db, covenantID, effects.CostDelta); err != nil {
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
	tool.EnrichReceipt(receipt, result)
	return receipt, nil
}

func (e *Engine) logRejection(covenantID, agentID, sessionID string, tool Tool, params map[string]any, state, reason string) {
	audit.LogEvent(e.db, audit.Entry{
		CovenantID:    covenantID,
		AgentID:       agentID,
		SessionID:     sessionID,
		ToolName:      tool.ToolName(),
		ToolType:      tool.ToolType(),
		ParamsPreview: maskSensitive(params),
		Result:        "rejected",
		ResultDetail:  reason,
		StateBefore:   state,
		StateAfter:    state,
	})
}

func maskSensitive(params map[string]any) map[string]any {
	sensitive := map[string]bool{"content": true, "text": true, "draft": true, "password": true}
	out := map[string]any{}
	for k, v := range params {
		if sensitive[k] {
			out[k] = fmt.Sprintf("*** (length: %d)", len(fmt.Sprint(v)))
		} else {
			out[k] = v
		}
	}
	return out
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}
