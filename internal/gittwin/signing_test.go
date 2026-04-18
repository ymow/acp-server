package gittwin

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGenerateAndParseRoundtrip(t *testing.T) {
	signer, b64, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if signer.Algorithm() != AlgEd25519 {
		t.Fatalf("alg: want %s got %s", AlgEd25519, signer.Algorithm())
	}
	if len(signer.PublicKey()) != 32 {
		t.Fatalf("pubkey len: want 32 got %d", len(signer.PublicKey()))
	}

	parsed, err := ParseSigningKey(b64)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !bytesEqual(parsed.PublicKey(), signer.PublicKey()) {
		t.Fatal("round-tripped pubkey diverges from original")
	}

	msg := []byte("hello anchor")
	sigA, err := signer.Sign(msg)
	if err != nil {
		t.Fatalf("sign orig: %v", err)
	}
	sigB, err := parsed.Sign(msg)
	if err != nil {
		t.Fatalf("sign parsed: %v", err)
	}
	if !bytesEqual(sigA, sigB) {
		t.Fatal("parsed signer produces different signature for same message")
	}
}

func TestParseSigningKeyRejectsGarbage(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"not base64", "not_base64!"},
		{"too short", base64.StdEncoding.EncodeToString([]byte("short"))},
	}
	for _, c := range cases {
		if _, err := ParseSigningKey(c.in); err == nil {
			t.Fatalf("%s: expected error, got nil", c.name)
		}
	}
}

func TestParseSigningKeyDetectsMismatchedHalves(t *testing.T) {
	_, b64, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(b64)
	// Flip the last pubkey byte so priv[32:] no longer matches seed-derived pubkey.
	raw[len(raw)-1] ^= 0xff
	broken := base64.StdEncoding.EncodeToString(raw)

	if _, err := ParseSigningKey(broken); err == nil || !strings.Contains(err.Error(), "halves") {
		t.Fatalf("expected halves mismatch error, got %v", err)
	}
}

func TestLoadSignerFromEnvUnset(t *testing.T) {
	prev, had := os.LookupEnv(AnchorSigningKeyEnv)
	os.Unsetenv(AnchorSigningKeyEnv)
	defer func() {
		if had {
			os.Setenv(AnchorSigningKeyEnv, prev)
		}
	}()
	signer, err := LoadSignerFromEnv()
	if err != nil {
		t.Fatalf("unset env should not error: %v", err)
	}
	if signer != nil {
		t.Fatal("unset env should yield nil signer")
	}
}

func TestSignAndVerifyAnchorRoundtrip(t *testing.T) {
	signer, _, err := GenerateSigningKey()
	if err != nil {
		t.Fatalf("gen: %v", err)
	}

	note := AnchorNoteV1{
		Type:               "acp.anchor.settlement.v1",
		CovenantID:         "cov_a",
		SettlementOutputID: "sout_b",
		SettlementHash:     "deadbeef",
		SnapshotHash:       "cafe",
		TotalTokens:        1234,
		GeneratedAt:        "2026-04-18T10:00:00Z",
	}
	canonical, err := canonicalUnsignedAnchor(&note)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	sig, err := signer.Sign(canonical)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	note.Signature = &AnchorSignature{
		Algorithm: signer.Algorithm(),
		PublicKey: base64.StdEncoding.EncodeToString(signer.PublicKey()),
		Value:     base64.StdEncoding.EncodeToString(sig),
	}
	signedJSON, err := json.Marshal(&note)
	if err != nil {
		t.Fatalf("marshal signed: %v", err)
	}

	ok, err := VerifyAnchorSignature(signedJSON)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("verify: clean-signed anchor should validate")
	}

	// Tampering anywhere in the body flips verify to false.
	tampered := strings.Replace(string(signedJSON), "\"total_tokens\":1234", "\"total_tokens\":9999", 1)
	ok, err = VerifyAnchorSignature([]byte(tampered))
	if err != nil {
		t.Fatalf("verify tampered: unexpected error: %v", err)
	}
	if ok {
		t.Fatal("verify should fail when total_tokens is edited post-sign")
	}

	// Swapping the stored pubkey for another keypair's pubkey also fails.
	other, _, _ := GenerateSigningKey()
	var swapped AnchorNoteV1
	_ = json.Unmarshal(signedJSON, &swapped)
	swapped.Signature.PublicKey = base64.StdEncoding.EncodeToString(other.PublicKey())
	swappedJSON, _ := json.Marshal(&swapped)
	ok, err = VerifyAnchorSignature(swappedJSON)
	if err != nil {
		t.Fatalf("verify swapped: unexpected error: %v", err)
	}
	if ok {
		t.Fatal("verify should fail when pubkey is swapped out from under the sig")
	}
}

func TestVerifyAnchorSignatureErrorsOnUnsigned(t *testing.T) {
	note := AnchorNoteV1{
		Type: "acp.anchor.settlement.v1",
	}
	raw, _ := json.Marshal(&note)
	ok, err := VerifyAnchorSignature(raw)
	if ok {
		t.Fatal("unsigned note should not verify")
	}
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("expected unsigned error, got %v", err)
	}
}
