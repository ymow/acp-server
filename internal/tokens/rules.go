package tokens

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
)

// TokenRule is the per-clause-tool formula that converts contribution
// attributes (word/unit count, acceptance ratio, tier multiplier) into an
// integer token delta. Defined at covenant creation, frozen after ACTIVE.
// See ACR-20 Part 2.
type TokenRule struct {
	ToolName string  `json:"tool_name"`
	Formula  string  `json:"formula"`
	BaseRate float64 `json:"base_rate"`
	Pending  bool    `json:"pending"`
}

// RuleVars is the free-variable environment available to a TokenRule formula.
// base_rate is injected automatically from TokenRule.BaseRate; callers do not
// set it on RuleVars.
type RuleVars struct {
	WordCount       int
	AcceptanceRatio float64
	TierMultiplier  float64
	CallCount       int
}

// ParseRules unmarshals covenants.token_rules_json into a tool_name-indexed
// map. Empty input returns an empty map (not an error) so callers can treat
// "no rules configured" as a normal fallback case. Legacy object-shaped
// payloads from pre-3.B covenants ({"base_rate": 1, ...}) are silently
// treated as "no rules" rather than erroring, so approve_draft falls back
// to the default formula instead of rejecting the whole tool call.
func ParseRules(raw string) (map[string]*TokenRule, error) {
	out := map[string]*TokenRule{}
	if raw == "" {
		return out, nil
	}
	trimmed := skipSpaces(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		// Not an array — legacy free-form object. No parseable rules.
		return out, nil
	}
	var rules []TokenRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("token_rules_json: %w", err)
	}
	for i := range rules {
		r := rules[i]
		if r.ToolName == "" {
			return nil, fmt.Errorf("token_rules[%d]: tool_name is required", i)
		}
		out[r.ToolName] = &r
	}
	return out, nil
}

func skipSpaces(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return s[i:]
		}
	}
	return ""
}

// Evaluate parses and evaluates the formula in the given environment.
// Returns the rounded integer token delta (round-half-away-from-zero; the
// spec uses floor() explicitly inside the formula so trailing rounding is
// benign). Any non-whitelisted syntax produces an error rather than silently
// evaluating to zero, so a misconfigured covenant fails loudly at approve
// time instead of minting wrong token counts.
func (r *TokenRule) Evaluate(vars RuleVars) (int, error) {
	v, err := evalFormula(r.Formula, r.BaseRate, vars)
	if err != nil {
		return 0, fmt.Errorf("token rule %q: %w", r.ToolName, err)
	}
	return int(math.Round(v)), nil
}

// ValidateFormula parses the formula string without evaluating it. Used at
// configure_token_rules time so a malformed formula fails in DRAFT rather
// than at the first approve_draft call.
func ValidateFormula(formula string) error {
	if formula == "" {
		return fmt.Errorf("formula is required")
	}
	expr, err := parser.ParseExpr(formula)
	if err != nil {
		return fmt.Errorf("parse formula: %w", err)
	}
	// Dry-run with zero env to surface identifier / call errors now.
	_, err = evalExpr(expr, RuleVars{}, 0)
	return err
}

func evalFormula(formula string, baseRate float64, vars RuleVars) (float64, error) {
	if formula == "" {
		return 0, fmt.Errorf("formula is empty")
	}
	expr, err := parser.ParseExpr(formula)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	return evalExpr(expr, vars, baseRate)
}

func evalExpr(node ast.Expr, vars RuleVars, baseRate float64) (float64, error) {
	switch n := node.(type) {
	case *ast.BasicLit:
		if n.Kind != token.INT && n.Kind != token.FLOAT {
			return 0, fmt.Errorf("unsupported literal %s", n.Value)
		}
		return strconv.ParseFloat(n.Value, 64)
	case *ast.Ident:
		switch n.Name {
		case "word_count":
			return float64(vars.WordCount), nil
		case "acceptance_ratio":
			return vars.AcceptanceRatio, nil
		case "tier_multiplier":
			return vars.TierMultiplier, nil
		case "call_count":
			return float64(vars.CallCount), nil
		case "base_rate":
			return baseRate, nil
		default:
			return 0, fmt.Errorf("unknown identifier %q", n.Name)
		}
	case *ast.ParenExpr:
		return evalExpr(n.X, vars, baseRate)
	case *ast.UnaryExpr:
		v, err := evalExpr(n.X, vars, baseRate)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.SUB:
			return -v, nil
		case token.ADD:
			return v, nil
		}
		return 0, fmt.Errorf("unsupported unary operator %s", n.Op)
	case *ast.BinaryExpr:
		lhs, err := evalExpr(n.X, vars, baseRate)
		if err != nil {
			return 0, err
		}
		rhs, err := evalExpr(n.Y, vars, baseRate)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return lhs + rhs, nil
		case token.SUB:
			return lhs - rhs, nil
		case token.MUL:
			return lhs * rhs, nil
		case token.QUO:
			if rhs == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			return lhs / rhs, nil
		}
		return 0, fmt.Errorf("unsupported binary operator %s", n.Op)
	case *ast.CallExpr:
		ident, ok := n.Fun.(*ast.Ident)
		if !ok {
			return 0, fmt.Errorf("unsupported function expression")
		}
		if len(n.Args) != 1 {
			return 0, fmt.Errorf("function %s requires exactly 1 argument", ident.Name)
		}
		v, err := evalExpr(n.Args[0], vars, baseRate)
		if err != nil {
			return 0, err
		}
		switch ident.Name {
		case "floor":
			return math.Floor(v), nil
		case "ceil":
			return math.Ceil(v), nil
		case "round":
			return math.Round(v), nil
		}
		return 0, fmt.Errorf("unsupported function %q", ident.Name)
	}
	return 0, fmt.Errorf("unsupported expression type %T", node)
}
