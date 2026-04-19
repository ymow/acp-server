package covenant

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acpcrypto "github.com/inkmesh/acp-server/internal/crypto"
	acpdb "github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/keys"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acp.db")
	conn, err := acpdb.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newTestSealer(t *testing.T) *acpcrypto.Sealer {
	t.Helper()
	p, err := keys.NewLocalKeyfileProvider(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return acpcrypto.NewSealer(p)
}

func TestHashPlatformIDDeterministic(t *testing.T) {
	a := HashPlatformID("github:octocat")
	b := HashPlatformID("github:octocat")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(a))
	}
	if HashPlatformID("github:alice") == HashPlatformID("github:bob") {
		t.Fatal("expected different hashes for different inputs")
	}
}

func TestCreatePopulatesHashAndEnc(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	sealer := newTestSealer(t)
	svc.SetSealer(sealer)

	_, _, err := svc.Create("Test Covenant", "code", "github:alice")
	if err != nil {
		t.Fatalf("create covenant: %v", err)
	}

	var (
		hash string
		enc  []byte
	)
	row := db.QueryRow(`SELECT platform_id_hash, platform_id_enc FROM platform_identities WHERE platform_id = ?`, "github:alice")
	if err := row.Scan(&hash, &enc); err != nil {
		t.Fatalf("scan row: %v", err)
	}

	wantHash := HashPlatformID("github:alice")
	if hash != wantHash {
		t.Errorf("hash mismatch: got %q, want %q", hash, wantHash)
	}
	if len(enc) == 0 {
		t.Fatal("platform_id_enc is empty; expected sealed blob")
	}

	// Decrypting with the same sealer (AAD = hash) must yield the plaintext.
	plaintext, err := sealer.Open(hash, "platform_id", enc)
	if err != nil {
		t.Fatalf("open enc: %v", err)
	}
	if !bytes.Equal(plaintext, []byte("github:alice")) {
		t.Errorf("decrypted mismatch: got %q", plaintext)
	}
}

func TestCreateWithoutSealerLeavesEncNull(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	// No SetSealer call.

	_, _, err := svc.Create("Test Covenant", "code", "github:nobody")
	if err != nil {
		t.Fatalf("create covenant: %v", err)
	}

	var (
		hash string
		enc  sql.NullString
	)
	row := db.QueryRow(`SELECT platform_id_hash, platform_id_enc FROM platform_identities WHERE platform_id = ?`, "github:nobody")
	if err := row.Scan(&hash, &enc); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if hash != HashPlatformID("github:nobody") {
		t.Errorf("hash not written without sealer: %q", hash)
	}
	if enc.Valid {
		t.Errorf("expected platform_id_enc NULL without sealer, got %v", enc.String)
	}
}

func TestBackfillHydratesLegacyRows(t *testing.T) {
	db := newTestDB(t)
	// Seed as if Phase 1/2/3 wrote the row: no hash, no enc.
	_, err := db.Exec(`INSERT INTO platform_identities (platform_id, created_at) VALUES (?, ?)`,
		"github:legacy", "2025-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	sealer := newTestSealer(t)
	n, err := BackfillPlatformIdentities(db, sealer)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("backfill updated %d rows, want 1", n)
	}

	var (
		hash string
		enc  []byte
	)
	row := db.QueryRow(`SELECT platform_id_hash, platform_id_enc FROM platform_identities WHERE platform_id = ?`, "github:legacy")
	if err := row.Scan(&hash, &enc); err != nil {
		t.Fatalf("post-backfill scan: %v", err)
	}
	if hash != HashPlatformID("github:legacy") {
		t.Errorf("post-backfill hash mismatch: %q", hash)
	}
	if len(enc) == 0 {
		t.Fatal("post-backfill platform_id_enc empty")
	}

	// Second run must be a no-op (idempotency).
	n2, err := BackfillPlatformIdentities(db, sealer)
	if err != nil {
		t.Fatalf("backfill second run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second backfill updated %d rows, want 0 (idempotency)", n2)
	}
}

// ACR-700 §4: Member JSON must not surface plaintext platform_id. The field
// stays on the struct for internal joins but is tagged json:"-". Any future
// change that reintroduces the key (typo, rename, struct copy) must fail here.
func TestMemberJSONOmitsPlatformID(t *testing.T) {
	m := Member{
		CovenantID: "cov_1",
		PlatformID: "github:should-never-appear",
		AgentID:    "agt_1",
		TierID:     "tier_x",
		Status:     "active",
		JoinedAt:   time.Unix(0, 0).UTC(),
	}
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	if strings.Contains(s, "github:should-never-appear") {
		t.Errorf("plaintext platform_id leaked into Member JSON: %s", s)
	}
	if strings.Contains(s, `"platform_id"`) {
		t.Errorf("platform_id key present in Member JSON: %s", s)
	}
}

func TestBackfillSkipsAlreadyHydrated(t *testing.T) {
	db := newTestDB(t)
	svc := New(db)
	sealer := newTestSealer(t)
	svc.SetSealer(sealer)

	// Fresh insert through the sealer: hash + enc already populated.
	if _, _, err := svc.Create("Test", "code", "github:fresh"); err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := BackfillPlatformIdentities(db, sealer)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 0 {
		t.Errorf("backfill updated %d rows on already-hydrated DB, want 0", n)
	}
}
