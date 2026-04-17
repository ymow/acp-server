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
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return err
		}
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
