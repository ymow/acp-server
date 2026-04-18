package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

// Open returns a *sql.DB connected to the given SQLite file path,
// with WAL mode and the full schema applied.
func Open(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	// SQLite is single-writer; cap write concurrency to 1.
	conn.SetMaxOpenConns(1)

	if err := applySchema(conn); err != nil {
		conn.Close()
		return nil, err
	}
	if err := applyMigrations(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func applySchema(db *sql.DB) error {
	_, file, _, _ := runtime.Caller(0)
	schemaPath := filepath.Join(filepath.Dir(file), "schema.sql")
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return err
	}
	_, err = db.Exec(string(data))
	return err
}

// applyMigrations runs ALTER TABLE statements that CREATE TABLE IF NOT EXISTS cannot handle.
// Each migration is idempotent: duplicate-column errors are swallowed so pre-existing DBs upgrade cleanly.
func applyMigrations(db *sql.DB) error {
	migrations := []string{
		`ALTER TABLE covenants ADD COLUMN cost_weight REAL NOT NULL DEFAULT 1.0`,
		// ACR-300@2.2: cost_currency column on audit_logs. Default 'USD' keeps
		// legacy rows verifiable (2.0/2.1 hash payloads never included currency).
		`ALTER TABLE audit_logs ADD COLUMN cost_currency TEXT NOT NULL DEFAULT 'USD'`,
		// Phase 3.0: propagate currency to budget layer. execution.Run rejects
		// a charge whose CostCurrency doesn't match covenants.budget_currency.
		`ALTER TABLE covenants ADD COLUMN budget_currency TEXT NOT NULL DEFAULT 'USD'`,
		`ALTER TABLE budget_counters ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD'`,
		// Phase 3.0: make covenant ownership load-bearing in the schema
		// instead of derived via is_owner=1 lookups. Enables Constitutional
		// Principle #2 (agent_id vs owner_id separation) and Git Twin mapping.
		`ALTER TABLE covenants ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
		// Phase 3.B: snapshot tamper-evidence per ACR-20 Part 5. The hash
		// covers covenant_id|agent_id|agent_tokens|cost_tokens|snapped_at so
		// any after-the-fact edit to the token_snapshots row is detectable.
		`ALTER TABLE token_snapshots ADD COLUMN snapshot_hash TEXT NOT NULL DEFAULT ''`,
		// Phase 3.A: ACR-400 Git Covenant Twin binding. Empty defaults mean
		// the covenant has no git twin attached; SetGitTwin populates them
		// and only while the covenant is DRAFT.
		`ALTER TABLE covenants ADD COLUMN git_twin_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE covenants ADD COLUMN git_twin_provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE covenants ADD COLUMN git_twin_config_json TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return err
		}
	}
	// Backfill owner_id for pre-migration rows. Idempotent: WHERE owner_id=''
	// skips rows already populated; EXISTS guard avoids NULL → NOT NULL violation
	// on legacy covenants that somehow have no owner member.
	if _, err := db.Exec(`
		UPDATE covenants
		SET owner_id = (
			SELECT agent_id FROM covenant_members
			WHERE covenant_id = covenants.covenant_id AND is_owner = 1
			LIMIT 1
		)
		WHERE owner_id = ''
		  AND EXISTS (
		    SELECT 1 FROM covenant_members
		    WHERE covenant_id = covenants.covenant_id AND is_owner = 1
		  )`); err != nil {
		return err
	}
	return nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && (contains(err.Error(), "duplicate column") || contains(err.Error(), "already exists"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
