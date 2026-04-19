// Package crypto implements ACR-700 §2.3 ciphertext packaging and the
// §5.2 Seal/Open helpers.
//
// Cipher choice: AES-256-GCM (stdlib crypto/cipher). ACR-700 §2.1 lists both
// ChaCha20-Poly1305 and AES-256-GCM as compliant AEADs. The reference
// implementation listed in the spec is ChaCha20-Poly1305 (RFC 8439); this
// implementation picks AES-256-GCM to keep acp-server on the Go standard
// library (no golang.org/x/crypto dependency). Both ciphers provide the
// same 16-byte authentication tag and the same 96-bit nonce surface, so the
// ciphertext header format in §2.3 applies unchanged.
//
// The §2.3 blob layout every *_enc column stores:
//
//	[version: 1 byte] [key_version: 3 bytes, big-endian u24]
//	[nonce: 12 bytes] [ciphertext || tag: variable]
//
// AAD passed to the cipher is the string literal:
//
//	"acp-server|" + row_id + "|" + column_name
//
// Binding the row identity into the AAD prevents cut-and-paste of one row's
// ciphertext into another row — AEAD authentication fails on mismatch.
//
// ACR-700 §2.3 names the middle field covenant_id because the motivating
// table (covenant_members) is per-covenant. This implementation generalizes
// the name to row_id: callers pick whatever string uniquely identifies
// the row within its table. For global tables such as platform_identities
// (keyed by platform_id), callers pass platform_id_hash. To be folded
// back into ACR-700 v0.2 as a wording clarification.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/inkmesh/acp-server/internal/keys"
)

// Header field sizes and the current format version, per ACR-700 §2.3.
const (
	VersionByte     = 0x01
	VersionSize     = 1
	KeyVersionSize  = 3
	NonceSize       = 12
	HeaderSize      = VersionSize + KeyVersionSize + NonceSize
	AuthTagSize     = 16
	MaxKeyVersion   = (1 << 24) - 1
	aadPrefix       = "acp-server|"
	aadSeparator    = "|"
	aadMinParamsErr = "acp crypto: row_id and column are required for AAD"
)

// ErrFormat is returned when a ciphertext blob cannot be parsed: too short,
// wrong version byte, or missing auth tag room.
var ErrFormat = errors.New("acp crypto: malformed ciphertext")

// ErrUnsupportedVersion is returned when the header's format version byte is
// not one this build recognizes. This is distinct from KeyProvider version
// mismatches, which flow through keys.ErrKeyVersionUnavailable.
var ErrUnsupportedVersion = errors.New("acp crypto: unsupported ciphertext version")

// Sealer binds a KeyProvider to the Seal/Open operations. It is safe for
// concurrent use provided the underlying KeyProvider is.
type Sealer struct {
	provider keys.KeyProvider
}

// NewSealer wires a KeyProvider into the Seal/Open helpers.
func NewSealer(provider keys.KeyProvider) *Sealer {
	return &Sealer{provider: provider}
}

// Seal encrypts plaintext under the provider's current key, binding the result
// to (rowID, column) via the AAD. The returned blob carries the ACR-700
// §2.3 header so Open is self-describing.
func (s *Sealer) Seal(rowID, column string, plaintext []byte) ([]byte, error) {
	if rowID == "" || column == "" {
		return nil, errors.New(aadMinParamsErr)
	}
	key, version, err := s.provider.Current()
	if err != nil {
		return nil, fmt.Errorf("acp crypto: provider current: %w", err)
	}
	if version == 0 || version > MaxKeyVersion {
		return nil, fmt.Errorf("acp crypto: key_version %d out of range (1..%d)", version, MaxKeyVersion)
	}

	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("acp crypto: generate nonce: %w", err)
	}

	aad := buildAAD(rowID, column)

	// Allocate the final blob up front: header + plaintext + tag.
	out := make([]byte, HeaderSize, HeaderSize+len(plaintext)+aead.Overhead())
	writeHeader(out, version, nonce)

	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// Open authenticates and decrypts a blob produced by Seal. The caller must
// pass the same (rowID, column) that was used at seal time; any
// mismatch surfaces as an AEAD authentication failure.
//
// The blob's header determines which key_version the provider looks up. A
// version the provider cannot resolve is reported via keys.ErrKeyVersionUnavailable
// (propagated unchanged), so callers can distinguish "bad ciphertext" from
// "archived key gone".
func (s *Sealer) Open(rowID, column string, blob []byte) ([]byte, error) {
	if rowID == "" || column == "" {
		return nil, errors.New(aadMinParamsErr)
	}
	version, keyVersion, nonce, payload, err := parseHeader(blob)
	if err != nil {
		return nil, err
	}
	if version != VersionByte {
		return nil, fmt.Errorf("%w: got 0x%02x", ErrUnsupportedVersion, version)
	}

	key, err := s.provider.At(keyVersion)
	if err != nil {
		// Bubble up ErrKeyVersionUnavailable unchanged so callers can
		// errors.Is on it.
		return nil, err
	}

	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	aad := buildAAD(rowID, column)

	plaintext, err := aead.Open(nil, nonce, payload, aad)
	if err != nil {
		return nil, fmt.Errorf("acp crypto: authenticate: %w", err)
	}
	return plaintext, nil
}

// buildAAD packs (rowID, column) into the §2.3 binding string.
func buildAAD(rowID, column string) []byte {
	total := len(aadPrefix) + len(rowID) + len(aadSeparator) + len(column)
	out := make([]byte, 0, total)
	out = append(out, aadPrefix...)
	out = append(out, rowID...)
	out = append(out, aadSeparator...)
	out = append(out, column...)
	return out
}

// writeHeader serializes (VersionByte, keyVersion, nonce) into the first
// HeaderSize bytes of dst. dst must have at least HeaderSize capacity.
func writeHeader(dst []byte, keyVersion uint32, nonce []byte) {
	dst[0] = VersionByte
	// Big-endian u24: pack the low 3 bytes of keyVersion into dst[1..4].
	dst[1] = byte(keyVersion >> 16)
	dst[2] = byte(keyVersion >> 8)
	dst[3] = byte(keyVersion)
	copy(dst[VersionSize+KeyVersionSize:HeaderSize], nonce)
}

// parseHeader splits a blob into (version, keyVersion, nonce, payload). The
// payload includes the AEAD tag; the cipher strips it during Open.
func parseHeader(blob []byte) (byte, uint32, []byte, []byte, error) {
	if len(blob) < HeaderSize+AuthTagSize {
		return 0, 0, nil, nil, fmt.Errorf("%w: blob has %d bytes, need ≥%d", ErrFormat, len(blob), HeaderSize+AuthTagSize)
	}
	version := blob[0]
	// Expand the u24 to u32 via a zero high byte.
	keyVersion := binary.BigEndian.Uint32([]byte{0, blob[1], blob[2], blob[3]})
	nonce := blob[VersionSize+KeyVersionSize : HeaderSize]
	payload := blob[HeaderSize:]
	return version, keyVersion, nonce, payload, nil
}

// newAEAD constructs a fresh AES-256-GCM cipher for one Seal/Open call.
// Callers should not reuse instances across goroutines; construction is
// cheap and allocation-free after the first call per key (Go's crypto/aes
// caches the expanded key schedule internally on modern CPUs).
func newAEAD(key [keys.KeySize]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("acp crypto: aes cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("acp crypto: gcm wrap: %w", err)
	}
	return aead, nil
}
