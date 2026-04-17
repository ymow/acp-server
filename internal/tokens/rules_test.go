package tokens

import "testing"

func TestParseRulesEmpty(t *testing.T) {
	m, err := ParseRules("")
	if err != nil || len(m) != 0 {
		t.Fatalf("empty: m=%v err=%v", m, err)
	}
}

func TestParseRulesLegacyObject(t *testing.T) {
	m, err := ParseRules(`{"base_rate": 1.0, "proposal_cost": 10}`)
	if err != nil {
		t.Fatalf("legacy object should not error, got %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("legacy object should produce no rules, got %v", m)
	}
}

func TestParseRulesArray(t *testing.T) {
	raw := `[{"tool_name":"propose_passage","formula":"floor(word_count / 100) * base_rate * tier_multiplier","base_rate":2,"pending":true}]`
	m, err := ParseRules(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, ok := m["propose_passage"]
	if !ok {
		t.Fatalf("expected propose_passage rule")
	}
	if r.BaseRate != 2 || !r.Pending {
		t.Fatalf("unexpected rule: %+v", r)
	}
}

func TestEvaluateAcceptanceFormula(t *testing.T) {
	r := &TokenRule{
		ToolName: "propose_passage",
		Formula:  "floor(word_count * acceptance_ratio / 100) * base_rate * tier_multiplier",
		BaseRate: 2,
	}
	// 850 words × 0.8 / 100 = 6.8 → floor = 6 × 2 × 1.5 = 18
	got, err := r.Evaluate(RuleVars{WordCount: 850, AcceptanceRatio: 0.8, TierMultiplier: 1.5})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got != 18 {
		t.Fatalf("want 18 got %d", got)
	}
}

func TestEvaluateFixedFormula(t *testing.T) {
	r := &TokenRule{Formula: "base_rate * tier_multiplier", BaseRate: 5}
	got, err := r.Evaluate(RuleVars{TierMultiplier: 2})
	if err != nil || got != 10 {
		t.Fatalf("want 10 got=%d err=%v", got, err)
	}
}

func TestValidateFormulaRejectsUnknownIdent(t *testing.T) {
	if err := ValidateFormula("word_count * SECRET"); err == nil {
		t.Fatal("expected error for unknown identifier SECRET")
	}
}

func TestValidateFormulaRejectsUnsupportedCall(t *testing.T) {
	if err := ValidateFormula("exec(word_count)"); err == nil {
		t.Fatal("expected error for unsupported function exec")
	}
}

func TestValidateFormulaRejectsDivisionByZero(t *testing.T) {
	r := &TokenRule{Formula: "word_count / 0"}
	if _, err := r.Evaluate(RuleVars{WordCount: 10}); err == nil {
		t.Fatal("expected division-by-zero error")
	}
}
