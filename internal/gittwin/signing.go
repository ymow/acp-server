package gittwin

// ACR-400 v0.2 anchor signing. Each pending anchor gets an ed25519 signature
// over the canonical note JSON (with signature field nil) before it is
// written. Verifiers read the signature back, re-serialize the note with
// signature nil, and confirm the bytes match the pubkey — any tampering
// between server and git land flips the check to false.
//
// Key material flow:
//
//	ACP_ANCHOR_SIGNING_KEY  base64(seed32||pubkey32), 88 chars standard base64
//	                        unset → unsigned anchors (opt-in signing)
//
// We do not auto-persist a generated key; auto-rotating the signer every
// restart would invalidate every historical anchor's pubkey. Operators run
// `GenerateSigningKey` once, paste the base64 into env, and leave it.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// AnchorSigningKeyEnv is the env var the server reads at startup.
const AnchorSigningKeyEnv = "ACP_ANCHOR_SIGNING_KEY"

// AlgEd25519 is the only algorithm supported in v0.2. Signature payloads
// carry the algorithm so future keys can roll without breaking old notes.
const AlgEd25519 = "ed25519"

// Signer signs anchor note bytes with a single algorithm+key pair. A nil
// Signer means anchors go out unsigned — ACR-400 v0.2 permits this for
// v0.1-compat deployments but warns that verifiers cannot attribute them.
type Signer interface {
	Algorithm() string
	PublicKey() []byte
	Sign(msg []byte) ([]byte, error)
}

// ed25519Signer is the stdlib-backed Signer.
type ed25519Signer struct {
	priv ed25519.PrivateKey
}

func (s *ed25519Signer) Algorithm() string  { return AlgEd25519 }
func (s *ed25519Signer) PublicKey() []byte  { return s.priv.Public().(ed25519.PublicKey) }
func (s *ed25519Signer) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, msg), nil
}

// LoadSignerFromEnv parses AnchorSigningKeyEnv. Returns (nil, nil) when the
// env is unset so callers can treat "no signer" as a valid configuration.
// Returns an error only when the env is set but malformed — a bad key in
// env should block startup, not silently degrade to unsigned.
func LoadSignerFromEnv() (Signer, error) {
	raw := os.Getenv(AnchorSigningKeyEnv)
	if raw == "" {
		return nil, nil
	}
	return ParseSigningKey(raw)
}

// ParseSigningKey decodes a base64-encoded 64-byte ed25519 private key
// (seed32 || pubkey32, the format ed25519.NewKeyFromSeed produces).
func ParseSigningKey(b64 string) (Signer, error) {
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: base64: %w", AnchorSigningKeyEnv, err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("parse %s: want %d bytes (seed32+pub32), got %d",
			AnchorSigningKeyEnv, ed25519.PrivateKeySize, len(decoded))
	}
	priv := ed25519.PrivateKey(decoded)
	// Sanity check: private[32:] must match Public() derived from seed[:32].
	// Catches operators who pasted seed-only or swapped halves.
	derived := ed25519.NewKeyFromSeed(decoded[:32])
	if !bytesEqual(derived[32:], decoded[32:]) {
		return nil, errors.New("ed25519 key seed and pubkey halves do not match")
	}
	return &ed25519Signer{priv: priv}, nil
}

// GenerateSigningKey returns a brand-new Signer plus its base64 form,
// suitable for dropping into ACP_ANCHOR_SIGNING_KEY. Operators call this
// once (e.g., via a bootstrap CLI) then never again — persistence is
// their responsibility.
func GenerateSigningKey() (Signer, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ed25519: %w", err)
	}
	return &ed25519Signer{priv: priv}, base64.StdEncoding.EncodeToString(priv), nil
}

// VerifyAnchorSignature re-serializes noteJSON with the signature field set
// to nil and checks the stored signature against the stored pubkey. Returns
// (true, nil) on a clean verify; (false, nil) on tamper/wrong-key; or
// (false, err) on malformed JSON.
//
// We verify against the pubkey embedded in the note so a rotated server key
// does not invalidate historical anchors. Trust-on-first-use applies —
// external verifiers should pin the pubkey(s) they trust.
func VerifyAnchorSignature(noteJSON []byte) (bool, error) {
	note, err := parseAnchorNote(noteJSON)
	if err != nil {
		return false, err
	}
	if note.Signature == nil {
		return false, errors.New("note is unsigned")
	}
	if note.Signature.Algorithm != AlgEd25519 {
		return false, fmt.Errorf("unsupported algorithm %q", note.Signature.Algorithm)
	}
	pub, err := base64.StdEncoding.DecodeString(note.Signature.PublicKey)
	if err != nil {
		return false, fmt.Errorf("decode pubkey: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("pubkey size: want %d got %d", ed25519.PublicKeySize, len(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(note.Signature.Value)
	if err != nil {
		return false, fmt.Errorf("decode signature: %w", err)
	}

	// Re-serialize with signature nulled out. Use the shared canonicalizer
	// so signer and verifier agree on field ordering byte-for-byte.
	canonical, err := canonicalUnsignedAnchor(note)
	if err != nil {
		return false, err
	}
	return ed25519.Verify(pub, canonical, sig), nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
