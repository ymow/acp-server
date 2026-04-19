package crypto

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/inkmesh/acp-server/internal/keys"
)

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	p, err := keys.NewLocalKeyfileProvider(path)
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return NewSealer(p)
}

// T1: Seal then Open returns the original plaintext.
func TestSealOpenRoundTrip(t *testing.T) {
	s := newTestSealer(t)
	plaintext := []byte("gh:octocat")

	blob, err := s.Seal("cov-abc", "platform_id", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(blob) < HeaderSize+AuthTagSize {
		t.Fatalf("blob too short: %d", len(blob))
	}
	if blob[0] != VersionByte {
		t.Errorf("version byte = 0x%02x, want 0x%02x", blob[0], VersionByte)
	}

	got, err := s.Open("cov-abc", "platform_id", blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestSealEmptyPlaintext(t *testing.T) {
	s := newTestSealer(t)

	blob, err := s.Seal("cov-abc", "platform_id", nil)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := s.Open("cov-abc", "platform_id", blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty plaintext, got %q", got)
	}
}

// T2: Flipping any ciphertext byte causes Open to fail; no plaintext returned.
func TestOpenTamperDetection(t *testing.T) {
	s := newTestSealer(t)
	blob, err := s.Seal("cov-abc", "platform_id", []byte("payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	tampered := append([]byte(nil), blob...)
	// Flip a byte inside the ciphertext payload (past the header).
	tampered[HeaderSize] ^= 0xFF
	if got, err := s.Open("cov-abc", "platform_id", tampered); err == nil {
		t.Fatalf("expected auth error, got plaintext %q", got)
	}

	// Flip a byte inside the nonce.
	tampered = append([]byte(nil), blob...)
	tampered[VersionSize+KeyVersionSize] ^= 0x01
	if _, err := s.Open("cov-abc", "platform_id", tampered); err == nil {
		t.Fatal("expected auth error on nonce flip, got nil")
	}

	// Flip a byte inside the auth tag (last AuthTagSize bytes of the blob).
	tampered = append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := s.Open("cov-abc", "platform_id", tampered); err == nil {
		t.Fatal("expected auth error on tag flip, got nil")
	}
}

// T3: Swapping ciphertext between two rows causes Open to error (AAD mismatch).
func TestOpenCrossRowBinding(t *testing.T) {
	s := newTestSealer(t)

	blobA, err := s.Seal("cov-A", "platform_id", []byte("row-A-secret"))
	if err != nil {
		t.Fatalf("seal A: %v", err)
	}

	// Same sealer, same column, different covenant: ciphertext must not
	// authenticate when opened against the wrong AAD.
	if _, err := s.Open("cov-B", "platform_id", blobA); err == nil {
		t.Fatal("expected auth error when opening cov-A blob as cov-B")
	}

	// Same sealer, same covenant, different column: also must fail.
	if _, err := s.Open("cov-A", "other_column", blobA); err == nil {
		t.Fatal("expected auth error when opening platform_id blob as other_column")
	}
}

func TestSealUniqueNonces(t *testing.T) {
	s := newTestSealer(t)
	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		blob, err := s.Seal("cov-abc", "platform_id", []byte("same plaintext"))
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		nonce := string(blob[VersionSize+KeyVersionSize : HeaderSize])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce collision at iteration %d", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestOpenRejectsShortBlob(t *testing.T) {
	s := newTestSealer(t)
	if _, err := s.Open("cov", "col", make([]byte, HeaderSize+AuthTagSize-1)); !errors.Is(err, ErrFormat) {
		t.Errorf("short blob err = %v, want ErrFormat", err)
	}
	if _, err := s.Open("cov", "col", nil); !errors.Is(err, ErrFormat) {
		t.Errorf("nil blob err = %v, want ErrFormat", err)
	}
}

func TestOpenRejectsUnsupportedVersion(t *testing.T) {
	s := newTestSealer(t)
	blob, err := s.Seal("cov", "col", []byte("x"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	blob[0] = 0x02
	if _, err := s.Open("cov", "col", blob); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("version err = %v, want ErrUnsupportedVersion", err)
	}
}

// T6: Open of a row whose key_version is unavailable returns
// keys.ErrKeyVersionUnavailable (not a panic, not a silent fallback).
func TestOpenUnknownKeyVersion(t *testing.T) {
	s := newTestSealer(t)
	blob, err := s.Seal("cov", "col", []byte("x"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Rewrite header's key_version to something the provider doesn't have.
	blob[1] = 0x00
	blob[2] = 0x00
	blob[3] = 0x09

	_, err = s.Open("cov", "col", blob)
	if !errors.Is(err, keys.ErrKeyVersionUnavailable) {
		t.Errorf("err = %v, want keys.ErrKeyVersionUnavailable", err)
	}
}

func TestSealRejectsEmptyAADInputs(t *testing.T) {
	s := newTestSealer(t)
	if _, err := s.Seal("", "col", []byte("x")); err == nil {
		t.Error("expected error on empty covenant_id")
	}
	if _, err := s.Seal("cov", "", []byte("x")); err == nil {
		t.Error("expected error on empty column")
	}
	if _, err := s.Open("", "col", make([]byte, HeaderSize+AuthTagSize)); err == nil {
		t.Error("expected error on empty covenant_id (open)")
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, v := range []uint32{1, 2, 255, 256, 0xFFFFFE, MaxKeyVersion} {
		nonce := bytes.Repeat([]byte{byte(v)}, NonceSize)
		buf := make([]byte, HeaderSize)
		writeHeader(buf, v, nonce)

		version, keyVersion, gotNonce, payload, err := parseHeader(append(buf, bytes.Repeat([]byte{0}, AuthTagSize)...))
		if err != nil {
			t.Fatalf("parse v=%d: %v", v, err)
		}
		if version != VersionByte {
			t.Errorf("v=%d version=0x%02x, want 0x%02x", v, version, VersionByte)
		}
		if keyVersion != v {
			t.Errorf("key_version round-trip: got %d, want %d", keyVersion, v)
		}
		if !bytes.Equal(gotNonce, nonce) {
			t.Errorf("nonce round-trip failed for v=%d", v)
		}
		if len(payload) != AuthTagSize {
			t.Errorf("v=%d payload len = %d, want %d", v, len(payload), AuthTagSize)
		}
	}
}
