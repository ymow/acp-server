// Package reencrypt walks every at-rest ciphertext column and re-seals rows
// that were encrypted under a retired key_version into the current one. It
// backs the `acp-server reencrypt` subcommand (ACR-700 §3.3 rotation job).
//
// The operation is intentionally idempotent: a row already sealed under the
// current version is skipped without a rewrite. That makes the command safe
// to re-run after a partial failure, during a scheduled window, or simply
// "in case we forgot whether it finished."
package reencrypt

import (
	"database/sql"
	"encoding/binary"
	"fmt"

	"github.com/inkmesh/acp-server/internal/crypto"
)

// Target names one at-rest sealed column and the scanning shape needed to
// walk it. Adding a new *_enc column to the codebase means appending one
// entry here — the reencrypt loop is column-agnostic otherwise.
type Target struct {
	// Table is the SQL table name.
	Table string
	// PKColumn is the primary key column the UPDATE uses to pin the row.
	PKColumn string
	// AADRowColumn holds the value the encryption call passed as row_id at
	// Seal time. For platform_id columns that's the platform_id_hash column
	// (ACR-700 §2.3 row anchor).
	AADRowColumn string
	// EncColumn is the sealed ciphertext blob column.
	EncColumn string
	// AADColumnName is the literal string the Seal call passed as the
	// column field of the AAD. It is NOT necessarily equal to EncColumn —
	// Seal binds to the logical name ("platform_id") while storage lives in
	// "platform_id_enc". Binding the literal here keeps Seal/Open symmetric.
	AADColumnName string
}

// platform_id columns are the only at-rest sealed columns as of 4.5.8. The
// list is ordered by table creation order so the command's progress output
// is stable.
var DefaultTargets = []Target{
	{
		Table:         "platform_identities",
		PKColumn:      "platform_id",
		AADRowColumn:  "platform_id_hash",
		EncColumn:     "platform_id_enc",
		AADColumnName: "platform_id",
	},
	{
		Table:         "agent_access_requests",
		PKColumn:      "request_id",
		AADRowColumn:  "platform_id_hash",
		EncColumn:     "platform_id_enc",
		AADColumnName: "platform_id",
	},
}

// Stats reports the outcome of a reencrypt pass across all targets. Zero
// values are meaningful (nothing to do is a valid success).
type Stats struct {
	Scanned     int
	Reencrypted int
	Skipped     int
	NullEnc     int
	PerTable    map[string]TableStats
}

// TableStats is the same shape as Stats, scoped to one target.
type TableStats struct {
	Scanned     int
	Reencrypted int
	Skipped     int
	NullEnc     int
}

// Run walks every row in every target. For each row whose ciphertext
// key_version is older than the sealer's current version it does
// Open(old) → Seal(current) → UPDATE in a single transaction. Rows already
// on the current version or with NULL ciphertext are left alone.
//
// If any row fails to re-seal, Run returns that error without touching
// subsequent rows. Partial progress up to the failure is already persisted
// (each row is its own transaction), so a re-run resumes from where the
// failure was.
func Run(db *sql.DB, sealer *crypto.Sealer) (Stats, error) {
	return RunTargets(db, sealer, DefaultTargets)
}

// RunTargets is the testable form of Run with a caller-supplied target list.
// Tests feed a synthetic target so they don't have to couple to the default
// list.
func RunTargets(db *sql.DB, sealer *crypto.Sealer, targets []Target) (Stats, error) {
	_, currentVersion, err := sealer.Provider().Current()
	if err != nil {
		return Stats{}, fmt.Errorf("reencrypt: resolve current key_version: %w", err)
	}
	stats := Stats{PerTable: map[string]TableStats{}}

	for _, tgt := range targets {
		ts, err := runOne(db, sealer, tgt, currentVersion)
		// runOne always returns partial stats, so merge them before surfacing
		// the error — otherwise the operator loses the "how far did we get"
		// information when a late row fails.
		stats.Scanned += ts.Scanned
		stats.Reencrypted += ts.Reencrypted
		stats.Skipped += ts.Skipped
		stats.NullEnc += ts.NullEnc
		stats.PerTable[tgt.Table] = ts
		if err != nil {
			return stats, fmt.Errorf("reencrypt %s: %w", tgt.Table, err)
		}
	}
	return stats, nil
}

func runOne(db *sql.DB, sealer *crypto.Sealer, tgt Target, currentVersion uint32) (TableStats, error) {
	var ts TableStats

	query := fmt.Sprintf(
		`SELECT %s, %s, %s FROM %s`,
		tgt.PKColumn, tgt.AADRowColumn, tgt.EncColumn, tgt.Table,
	)
	rows, err := db.Query(query)
	if err != nil {
		return ts, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type pending struct {
		pk    string
		aadID string
		blob  []byte
	}
	var toRewrite []pending
	for rows.Next() {
		ts.Scanned++
		var (
			pk    string
			aadID string
			blob  []byte
		)
		if err := rows.Scan(&pk, &aadID, &blob); err != nil {
			return ts, fmt.Errorf("scan: %w", err)
		}
		if blob == nil {
			ts.NullEnc++
			continue
		}
		v, err := parseKeyVersion(blob)
		if err != nil {
			return ts, fmt.Errorf("row %s=%q: %w", tgt.PKColumn, pk, err)
		}
		if v == currentVersion {
			ts.Skipped++
			continue
		}
		// Buffer rewrites so we release the read cursor before issuing UPDATEs.
		// SQLite with max-open-conns=1 cannot run an UPDATE while a query
		// cursor is still open on the same connection.
		toRewrite = append(toRewrite, pending{pk: pk, aadID: aadID, blob: blob})
	}
	if err := rows.Err(); err != nil {
		return ts, fmt.Errorf("iterate: %w", err)
	}
	if err := rows.Close(); err != nil {
		return ts, fmt.Errorf("close: %w", err)
	}

	updateStmt := fmt.Sprintf(
		`UPDATE %s SET %s = ? WHERE %s = ?`,
		tgt.Table, tgt.EncColumn, tgt.PKColumn,
	)
	for _, p := range toRewrite {
		plaintext, err := sealer.Open(p.aadID, tgt.AADColumnName, p.blob)
		if err != nil {
			return ts, fmt.Errorf("open row %s=%q: %w", tgt.PKColumn, p.pk, err)
		}
		fresh, err := sealer.Seal(p.aadID, tgt.AADColumnName, plaintext)
		if err != nil {
			return ts, fmt.Errorf("seal row %s=%q: %w", tgt.PKColumn, p.pk, err)
		}
		if _, err := db.Exec(updateStmt, fresh, p.pk); err != nil {
			return ts, fmt.Errorf("update row %s=%q: %w", tgt.PKColumn, p.pk, err)
		}
		ts.Reencrypted++
	}
	return ts, nil
}

// parseKeyVersion peels the key_version out of a §2.3 header without the
// overhead of a full Open. Used to decide whether a row needs rewriting
// before we ask the sealer to touch key material.
func parseKeyVersion(blob []byte) (uint32, error) {
	if len(blob) < 4 {
		return 0, fmt.Errorf("ciphertext truncated: %d bytes", len(blob))
	}
	if blob[0] != crypto.VersionByte {
		return 0, fmt.Errorf("unsupported ciphertext version 0x%02x", blob[0])
	}
	// Big-endian u24 in bytes [1..4).
	return binary.BigEndian.Uint32([]byte{0, blob[1], blob[2], blob[3]}), nil
}
