// Package keys implements ACR-700 Key Management. It holds the KeyProvider
// interface that every at-rest encryption call site depends on, the local
// keyfile reference implementation, and the shared fingerprint helper.
//
// Only the interface and helpers live here. Encrypt/decrypt helpers
// (ACR-700 §5.2 Seal/Open) are a separate package that depends on this one.
package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// KeySize is the AEAD master-key length in bytes. ACR-700 §2.2 mandates 256-bit
// keys drawn from a CSPRNG.
const KeySize = 32

// FirstVersion is the key_version assigned to the initial master key generated
// by a fresh LocalKeyfileProvider. Rotation bumps this monotonically (Part 3.3),
// but 4.5.1 only ships the no-rotation baseline.
const FirstVersion uint32 = 1

// ErrKeyVersionUnavailable is returned by KeyProvider.At when the caller asks
// for a key_version that the provider cannot materialize — typically because
// the archived key file is beyond the provider's reach. Decrypt paths must
// surface this error; they MUST NOT fall back to a different version or to
// plaintext (ACR-700 §3.6).
var ErrKeyVersionUnavailable = errors.New("acp keys: key_version unavailable")

// KeyProvider is the contract every at-rest encryption call site depends on.
// Implementations MUST be safe for concurrent use.
type KeyProvider interface {
	// Current returns the active (key, key_version) pair for new writes.
	Current() (key [KeySize]byte, version uint32, err error)

	// At returns the historical key for decrypting an older row. It returns
	// ErrKeyVersionUnavailable if the requested version has been archived
	// beyond the provider's reach.
	At(version uint32) (key [KeySize]byte, err error)
}

// Fingerprint is the first 8 bytes of SHA-256(key) rendered as 16 lowercase
// hex characters. ACR-700 §3.2 uses it in the first-start warning and in
// subsequent startup info logs so operators can confirm they loaded the file
// they expect without logging the raw key.
func Fingerprint(key [KeySize]byte) string {
	sum := sha256.Sum256(key[:])
	return hex.EncodeToString(sum[:8])
}
