package tools

import "testing"

// ACR-700 §4: actor_platform_id must be surfaced in the audit preview so
// verifiers can see "who did this", but the plaintext value is replaced with
// a masked length marker by the execution engine's sensitive-field handling.
// Removing it from SensitiveFields would silently leak plaintext into the
// durable audit log — regression guard lives here.
func TestRecordGitTwinEventActorPlatformIDIsSensitive(t *testing.T) {
	policy := (&RecordGitTwinEvent{}).ParamsPolicy()
	var inPreview, inSensitive bool
	for _, f := range policy.PreviewFields {
		if f == "actor_platform_id" {
			inPreview = true
		}
	}
	for _, f := range policy.SensitiveFields {
		if f == "actor_platform_id" {
			inSensitive = true
		}
	}
	if !inPreview {
		t.Error("actor_platform_id must remain in PreviewFields (audit needs the who)")
	}
	if !inSensitive {
		t.Error("actor_platform_id must remain in SensitiveFields (plaintext must be masked)")
	}
}
