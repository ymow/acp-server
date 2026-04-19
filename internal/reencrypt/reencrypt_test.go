package reencrypt

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	acpcrypto "github.com/inkmesh/acp-server/internal/crypto"
	acpdb "github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/keys"
)

// reencryptFixture wires a fresh DB + keyring + sealer and seeds one
// platform_identities row sealed under the provider's first key version.
// Returned fields make individual tests short and force every assertion to
// stand on the shared invariant (one row, hash known, plaintext known).
type reencryptFixture struct {
	db        *sql.DB
	provider  *keys.LocalKeyfileProvider
	sealer    *acpcrypto.Sealer
	platform  string
	hash      string
	pid       string
	v1        uint32
	plaintext []byte
}

func newReencryptFixture(t *testing.T) *reencryptFixture {
	t.Helper()
	tmp := t.TempDir()

	conn, err := acpdb.Open(filepath.Join(tmp, "acp.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	p, err := keys.NewLocalKeyfileProvider(filepath.Join(tmp, "master.key"))
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	sealer := acpcrypto.NewSealer(p)

	platform := "github:reencrypt-test"
	hash := "h_" + platform
	pid := "pid_reencrypt_001"
	plaintext := []byte(platform)

	blob, err := sealer.Seal(hash, "platform_id", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := conn.Exec(
		`INSERT INTO platform_identities (platform_id, kyc_status, created_at, platform_id_hash, platform_id_enc)
		 VALUES (?, 'none', ?, ?, ?)`,
		pid, time.Now().UTC().Format(time.RFC3339), hash, blob,
	); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	_, v1, err := p.Current()
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	return &reencryptFixture{
		db:        conn,
		provider:  p,
		sealer:    sealer,
		platform:  platform,
		hash:      hash,
		pid:       pid,
		v1:        v1,
		plaintext: plaintext,
	}
}

// blobKeyVersion is the test-side mirror of parseKeyVersion. Calling the
// production helper would still be valid but the test prefers an independent
// implementation so a header-format regression cannot mask itself.
func blobKeyVersion(t *testing.T, blob []byte) uint32 {
	t.Helper()
	if len(blob) < 4 {
		t.Fatalf("blob too short: %d bytes", len(blob))
	}
	return uint32(blob[1])<<16 | uint32(blob[2])<<8 | uint32(blob[3])
}

// readEnc fetches the current ciphertext blob for the seeded row.
func readEnc(t *testing.T, db *sql.DB, pid string) []byte {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(
		`SELECT platform_id_enc FROM platform_identities WHERE platform_id = ?`,
		pid,
	).Scan(&blob); err != nil {
		t.Fatalf("read row: %v", err)
	}
	return blob
}

// T5 (primary): a seeded v1 row gets rewritten under v2 after Rotate.
// Plaintext must round-trip through the new blob — otherwise the rewrite
// is corrupt and silently broken.
func TestRunRewritesAfterRotate(t *testing.T) {
	f := newReencryptFixture(t)

	originalBlob := readEnc(t, f.db, f.pid)
	if got := blobKeyVersion(t, originalBlob); got != f.v1 {
		t.Fatalf("seeded blob version = %d, want %d", got, f.v1)
	}

	v2, _, err := f.provider.Rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	stats, err := Run(f.db, f.sealer)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if stats.Reencrypted != 1 {
		t.Errorf("reencrypted = %d, want 1", stats.Reencrypted)
	}
	if stats.Skipped != 0 {
		t.Errorf("skipped = %d, want 0", stats.Skipped)
	}
	if stats.NullEnc != 0 {
		t.Errorf("null_enc = %d, want 0", stats.NullEnc)
	}

	updatedBlob := readEnc(t, f.db, f.pid)
	if got := blobKeyVersion(t, updatedBlob); got != v2 {
		t.Errorf("post-rewrite version = %d, want %d", got, v2)
	}
	if bytes.Equal(updatedBlob, originalBlob) {
		t.Error("expected rewritten blob to differ from original")
	}

	got, err := f.sealer.Open(f.hash, "platform_id", updatedBlob)
	if err != nil {
		t.Fatalf("open after rewrite: %v", err)
	}
	if !bytes.Equal(got, f.plaintext) {
		t.Errorf("plaintext changed: got %q, want %q", got, f.plaintext)
	}
}

// T5 (idempotency): a second Run after rotation must be a no-op. The
// reencrypt command is documented as safe to re-run — pin that promise.
func TestRunIdempotentSecondPass(t *testing.T) {
	f := newReencryptFixture(t)
	if _, _, err := f.provider.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := Run(f.db, f.sealer); err != nil {
		t.Fatalf("first reencrypt: %v", err)
	}
	blobAfterFirst := readEnc(t, f.db, f.pid)

	stats, err := Run(f.db, f.sealer)
	if err != nil {
		t.Fatalf("second reencrypt: %v", err)
	}
	if stats.Reencrypted != 0 {
		t.Errorf("second pass reencrypted = %d, want 0", stats.Reencrypted)
	}
	if stats.Skipped != 1 {
		t.Errorf("second pass skipped = %d, want 1", stats.Skipped)
	}

	if !bytes.Equal(readEnc(t, f.db, f.pid), blobAfterFirst) {
		t.Error("idempotent pass should not have rewritten the blob")
	}
}

// Without a rotation, every row is already at the current version. Run must
// short-circuit on the version check and never call Open/Seal — no rewrites,
// no plaintext touched.
func TestRunNoRotateSkipsAll(t *testing.T) {
	f := newReencryptFixture(t)

	stats, err := Run(f.db, f.sealer)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if stats.Reencrypted != 0 {
		t.Errorf("reencrypted = %d, want 0", stats.Reencrypted)
	}
	if stats.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", stats.Skipped)
	}
}

// NULL platform_id_enc rows must be counted but not touched. They represent
// either pre-encryption legacy rows or rows scheduled for backfill — either
// way, attempting to Open them would crash the run.
func TestRunCountsNullBlobs(t *testing.T) {
	f := newReencryptFixture(t)

	if _, err := f.db.Exec(
		`INSERT INTO platform_identities (platform_id, kyc_status, created_at, platform_id_hash, platform_id_enc)
		 VALUES ('pid_null', 'none', ?, 'h_null', NULL)`,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed null row: %v", err)
	}
	if _, _, err := f.provider.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	stats, err := Run(f.db, f.sealer)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if stats.NullEnc != 1 {
		t.Errorf("null_enc = %d, want 1", stats.NullEnc)
	}
	if stats.Reencrypted != 1 {
		t.Errorf("reencrypted = %d, want 1", stats.Reencrypted)
	}
	if stats.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", stats.Scanned)
	}
}

// Per-table breakdown survives a multi-target run. Verifying the map shape
// guards against silent regression in stat aggregation.
func TestRunPerTableStats(t *testing.T) {
	f := newReencryptFixture(t)
	if _, _, err := f.provider.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	stats, err := Run(f.db, f.sealer)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}

	pi, ok := stats.PerTable["platform_identities"]
	if !ok {
		t.Fatal("missing platform_identities entry")
	}
	if pi.Reencrypted != 1 || pi.Scanned != 1 {
		t.Errorf("platform_identities stats = %+v, want Reencrypted=1 Scanned=1", pi)
	}
	// agent_access_requests is in DefaultTargets but empty in this DB; it
	// should still appear with zero counters so operators see it was checked.
	if _, ok := stats.PerTable["agent_access_requests"]; !ok {
		t.Error("missing agent_access_requests entry (default target should always be reported)")
	}
}

// parseKeyVersion is the cheap front-gate: it must reject anything that
// isn't a §2.3 v0x01 header before the row reaches Open. These cases are
// the failure modes the reencrypt loop relies on it to catch.
func TestParseKeyVersionRejectsGarbage(t *testing.T) {
	if _, err := parseKeyVersion(nil); err == nil {
		t.Error("expected error on nil blob")
	}
	if _, err := parseKeyVersion([]byte{0x01, 0x00}); err == nil {
		t.Error("expected error on truncated blob")
	}
	if _, err := parseKeyVersion([]byte{0x99, 0x00, 0x00, 0x01}); err == nil {
		t.Error("expected error on unsupported version byte")
	}
	v, err := parseKeyVersion([]byte{0x01, 0x00, 0x00, 0x07})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v != 7 {
		t.Errorf("parsed version = %d, want 7", v)
	}
}
