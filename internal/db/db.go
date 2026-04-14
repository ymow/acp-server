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
