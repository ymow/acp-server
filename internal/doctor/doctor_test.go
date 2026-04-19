package doctor

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	acpcrypto "github.com/inkmesh/acp-server/internal/crypto"
	"github.com/inkmesh/acp-server/internal/covenant"
	acpdb "github.com/inkmesh/acp-server/internal/db"
	"github.com/inkmesh/acp-server/internal/keys"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acp.db")
	conn, err := acpdb.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newSealer(t *testing.T) *acpcrypto.Sealer {
	t.Helper()
	p, err := keys.NewLocalKeyfileProvider(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("key provider: %v", err)
	}
	return acpcrypto.NewSealer(p)
}

// findByCheck returns the Finding with the matching dotted ID, or nil. The
// doctor's Report is order-stable (Run iterates a fixed slice) but looking
// up by name keeps tests robust against future insertions.
func findByCheck(t *testing.T, r Report, check string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("check %q missing from report: %+v", check, r.Findings)
	return Finding{}
}

// TestRunCleanDBAllOK is the happy path: a fresh covenant.Service writes
// hashed + sealed identities, the doctor should report Info across the board
// and exit 0.
func TestRunCleanDBAllOK(t *testing.T) {
	conn := openDB(t)
	svc := covenant.New(conn)
	svc.SetSealer(newSealer(t))

	if _, _, err := svc.Create("Test", "code", "github:alice"); err != nil {
		t.Fatalf("create: %v", err)
	}

	report := Run(conn)
	if code := report.ExitCode(); code != 0 {
		t.Errorf("ExitCode = %d on clean DB, want 0", code)
	}
	for _, f := range report.Findings {
		if f.Severity == Error {
			t.Errorf("unexpected Error finding on clean DB: %s: %s", f.Check, f.Message)
		}
	}
}

// TestRunDetectsMissingHash seeds a legacy-style row (no hash) directly and
// asserts hash_coverage fires Error. This mirrors the Phase 1/2/3 residual
// case the backfill is meant to cover.
func TestRunDetectsMissingHash(t *testing.T) {
	conn := openDB(t)
	_, err := conn.Exec(`INSERT INTO platform_identities (platform_id, created_at) VALUES (?, ?)`,
		"github:legacy", "2025-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	report := Run(conn)
	f := findByCheck(t, report, "platform_id.hash_coverage")
	if f.Severity != Error {
		t.Errorf("hash_coverage severity = %s, want Error", f.Severity)
	}
	if report.ExitCode() != 1 {
		t.Errorf("ExitCode = %d with missing hash, want 1", report.ExitCode())
	}
}

// TestRunDetectsPreviewResidual plants a known platform_id inside an
// audit_logs.params_preview and confirms the scanner flags it. Seeds a
// clean identity row first so the scan has a needle to search for.
func TestRunDetectsPreviewResidual(t *testing.T) {
	conn := openDB(t)
	svc := covenant.New(conn)
	svc.SetSealer(newSealer(t))
	if _, _, err := svc.Create("Test", "code", "github:leak-me"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Synthesise an audit row with the plaintext embedded in the preview —
	// this is what a buggy ParamsPolicy (missing SensitiveFields) would produce.
	_, err := conn.Exec(`
		INSERT INTO audit_logs (log_id, covenant_id, sequence, agent_id, session_id,
			tool_name, tool_type, params_hash, params_preview, result, state_before,
			state_after, timestamp, hash)
		VALUES ('log_test_1', 'cov_1', 1, 'agt_1', 'sess_1', 'some_tool', 'query',
			'deadbeef', '{"actor_platform_id":"github:leak-me"}', 'success',
			'DRAFT', 'DRAFT', '2026-04-19T00:00:00Z', 'hash1')`)
	if err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	report := Run(conn)
	f := findByCheck(t, report, "platform_id.preview_residual")
	if f.Severity != Error {
		t.Errorf("preview_residual severity = %s, want Error", f.Severity)
	}
	if !strings.Contains(f.Message, "leak plaintext") {
		t.Errorf("preview_residual message missing leak phrasing: %s", f.Message)
	}
	// Detail lines must reference the log_id but must NOT echo the full pid.
	joined := strings.Join(f.Details, "\n")
	if !strings.Contains(joined, "log_test_1") {
		t.Errorf("details missing log_id: %q", joined)
	}
	if strings.Contains(joined, "github:leak-me") {
		t.Errorf("details leaked full platform_id: %q", joined)
	}
	if report.ExitCode() != 1 {
		t.Errorf("ExitCode = %d with residual, want 1", report.ExitCode())
	}
}

// TestLikeEscape pins the escape behaviour so a platform_id containing a
// SQLite LIKE metacharacter matches literally rather than as a wildcard.
// Regression guard for the bug where `foo_bar` would match `fooXbar`.
func TestLikeEscape(t *testing.T) {
	cases := map[string]string{
		`plain`:   `plain`,
		`a_b`:     `a\_b`,
		`50%`:     `50\%`,
		`a\b`:     `a\\b`,
		`%_\`:     `\%\_\\`,
	}
	for in, want := range cases {
		if got := likeEscape(in); got != want {
			t.Errorf("likeEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
