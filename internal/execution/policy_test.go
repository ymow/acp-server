package execution

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestApplyParamsPolicy_Whitelist(t *testing.T) {
	got := ApplyParamsPolicy(
		map[string]any{"word_count": 120, "chapter": "Ch1", "draft": "top secret"},
		ParamsPolicy{PreviewFields: []string{"word_count", "chapter"}},
	)
	want := map[string]any{"word_count": 120, "chapter": "Ch1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("whitelist drop failed: got %v want %v", got, want)
	}
}

func TestApplyParamsPolicy_SensitiveMask(t *testing.T) {
	got := ApplyParamsPolicy(
		map[string]any{"content": "hello world", "id": "x"},
		ParamsPolicy{SensitiveFields: []string{"content"}},
	)
	if s, _ := got["content"].(string); !strings.Contains(s, "length: 11") {
		t.Errorf("content not masked with rune length: got %q", got["content"])
	}
	if got["id"] != "x" {
		t.Errorf("non-sensitive id got %v", got["id"])
	}
}

func TestApplyParamsPolicy_MaskUsesRuneLength(t *testing.T) {
	// 4 characters, 12 bytes — must report length: 4, not 12.
	got := ApplyParamsPolicy(
		map[string]any{"content": "你好世界"},
		ParamsPolicy{SensitiveFields: []string{"content"}},
	)
	if got["content"].(string) != fmt.Sprintf("*** (length: %d)", 4) {
		t.Errorf("rune length wrong: got %v", got["content"])
	}
}

func TestApplyParamsPolicy_HashPreview(t *testing.T) {
	got := ApplyParamsPolicy(
		map[string]any{"content_hash": "abcd1234efgh5678"},
		ParamsPolicy{HashPreviewFields: []string{"content_hash"}},
	)
	if got["content_hash"] != "abcd1234..." {
		t.Errorf("hash not truncated: got %v", got["content_hash"])
	}
}

func TestApplyParamsPolicy_HashPreviewShort(t *testing.T) {
	// Values shorter than 8 runes should pass through unchanged.
	got := ApplyParamsPolicy(
		map[string]any{"content_hash": "abcd"},
		ParamsPolicy{HashPreviewFields: []string{"content_hash"}},
	)
	if got["content_hash"] != "abcd" {
		t.Errorf("short hash changed: got %v", got["content_hash"])
	}
}

func TestApplyParamsPolicy_WhitelistAndSensitiveStack(t *testing.T) {
	// A field in both lists should be kept AND masked.
	got := ApplyParamsPolicy(
		map[string]any{"secret": "xyz", "dropped": "zzz"},
		ParamsPolicy{
			PreviewFields:   []string{"secret"},
			SensitiveFields: []string{"secret"},
		},
	)
	if _, ok := got["dropped"]; ok {
		t.Errorf("non-whitelisted field leaked: %v", got)
	}
	if !strings.HasPrefix(got["secret"].(string), "*** ") {
		t.Errorf("whitelisted+sensitive not masked: %v", got["secret"])
	}
}

func TestApplyParamsPolicy_StoreHashOnly(t *testing.T) {
	p := map[string]any{"a": 1, "b": "x"}
	got := ApplyParamsPolicy(p, ParamsPolicy{StoreHashOnly: true})
	if len(got) != 1 {
		t.Fatalf("StoreHashOnly should produce single key, got %v", got)
	}
	h, ok := got["params_hash"].(string)
	if !ok || len(h) != 64 {
		t.Errorf("params_hash should be 64-char sha256 hex, got %v", got["params_hash"])
	}

	// Same input → same hash (canonical JSON with sorted keys).
	got2 := ApplyParamsPolicy(map[string]any{"b": "x", "a": 1}, ParamsPolicy{StoreHashOnly: true})
	if got["params_hash"] != got2["params_hash"] {
		t.Errorf("hash not deterministic across map orderings: %v vs %v",
			got["params_hash"], got2["params_hash"])
	}
}

func TestApplyParamsPolicy_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{"content": "hello", "id": "x"}
	_ = ApplyParamsPolicy(in, ParamsPolicy{SensitiveFields: []string{"content"}})
	if in["content"] != "hello" {
		t.Errorf("input mutated: %v", in)
	}
}

func TestDefaultParamsPolicy_LegacyFields(t *testing.T) {
	// Preserves pre-refactor behaviour for tools that don't override.
	got := ApplyParamsPolicy(
		map[string]any{
			"content":      "hi",
			"text":         "bye",
			"draft":        "d",
			"password":     "pw",
			"content_hash": "abcd1234efghij",
			"other":        "visible",
		},
		DefaultParamsPolicy(),
	)
	for _, f := range []string{"content", "text", "draft", "password"} {
		if s, _ := got[f].(string); !strings.HasPrefix(s, "*** ") {
			t.Errorf("default policy should mask %q, got %v", f, got[f])
		}
	}
	if got["content_hash"] != "abcd1234..." {
		t.Errorf("default policy should truncate content_hash, got %v", got["content_hash"])
	}
	if got["other"] != "visible" {
		t.Errorf("unknown field should pass through, got %v", got["other"])
	}
}
