package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// ParamsPolicy declares how a tool's params map is transformed into the
// audit_logs.params_preview column. It corresponds to ACP_Covenant_Spec_v0.2
// Part 6 — `ParamsPolicy` — and replaces the legacy hardcoded maskSensitive()
// field list.
//
// Semantics:
//   - StoreHashOnly: emit only {"params_hash": sha256(canonical_json(params))}
//     for tools whose raw params must never touch the durable log.
//   - PreviewFields: whitelist — if non-empty, only these keys survive into
//     the preview; any others are dropped. Empty = allow all keys.
//   - SensitiveFields: blacklist — these keys are replaced with
//     "*** (length: N)" (N = rune count of fmt.Sprint(value)).
//   - HashPreviewFields: these keys are truncated to first 8 runes + "..."
//     (used for content hashes where full value is uninteresting for audit).
//
// PreviewFields and SensitiveFields may overlap: a field listed in both is
// kept (passes the whitelist) and then masked.
type ParamsPolicy struct {
	StoreHashOnly     bool
	PreviewFields     []string
	SensitiveFields   []string
	HashPreviewFields []string
}

// ApplyParamsPolicy returns a new map suitable for audit_logs.params_preview.
// The input map is never mutated.
func ApplyParamsPolicy(params map[string]any, policy ParamsPolicy) map[string]any {
	if policy.StoreHashOnly {
		return map[string]any{"params_hash": canonicalHash(params)}
	}

	whitelist := toSet(policy.PreviewFields)
	sensitive := toSet(policy.SensitiveFields)
	hashPreview := toSet(policy.HashPreviewFields)

	out := make(map[string]any, len(params))
	for k, v := range params {
		if len(whitelist) > 0 && !whitelist[k] {
			continue
		}
		switch {
		case sensitive[k]:
			out[k] = fmt.Sprintf("*** (length: %d)", runeLen(v))
		case hashPreview[k]:
			out[k] = truncateHash(v)
		default:
			out[k] = v
		}
	}
	return out
}

// DefaultParamsPolicy is used when a tool does not override ParamsPolicy().
// It preserves pre-refactor behaviour: mask a fixed set of fields that
// historically carried user content, and truncate content_hash.
//
// New tools should declare an explicit policy instead of relying on this.
func DefaultParamsPolicy() ParamsPolicy {
	return ParamsPolicy{
		SensitiveFields:   []string{"content", "text", "draft", "password"},
		HashPreviewFields: []string{"content_hash"},
	}
}

func toSet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	s := make(map[string]bool, len(keys))
	for _, k := range keys {
		s[k] = true
	}
	return s
}

func runeLen(v any) int { return len([]rune(fmt.Sprint(v))) }

func truncateHash(v any) string {
	s := fmt.Sprint(v)
	r := []rune(s)
	if len(r) > 8 {
		return string(r[:8]) + "..."
	}
	return s
}

// canonicalHash emits a stable sha256 of the params map. Keys are sorted so
// the hash is independent of Go's map iteration order.
func canonicalHash(params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		ordered = append(ordered, k, params[k])
	}
	buf, _ := json.Marshal(ordered)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}
