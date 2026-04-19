package covenant

import (
	"database/sql"
	"fmt"

	"github.com/inkmesh/acp-server/internal/crypto"
)

// BackfillPlatformIdentities populates platform_id_hash (and when sealer is
// non-nil, platform_id_enc) for legacy platform_identities rows that predate
// ACR-700. Callers are expected to invoke this once at server startup after
// the Sealer is wired, so Phase 4.5 lookups on platform_id_hash never miss a
// row that was inserted during Phase 1/2/3.
//
// The pass is idempotent: rows whose hash is already set are skipped via the
// SELECT filter, and the UPDATE's WHERE guard prevents a racing writer from
// clobbering a fresh value.
//
// Returns the number of rows updated on this invocation.
func BackfillPlatformIdentities(db *sql.DB, sealer *crypto.Sealer) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("backfill: nil db")
	}
	rows, err := db.Query(`SELECT platform_id FROM platform_identities WHERE platform_id_hash = ''`)
	if err != nil {
		return 0, fmt.Errorf("backfill: select legacy rows: %w", err)
	}
	var legacy []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return 0, fmt.Errorf("backfill: scan: %w", err)
		}
		legacy = append(legacy, pid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("backfill: rows: %w", err)
	}
	rows.Close()

	updated := 0
	for _, pid := range legacy {
		hash := HashPlatformID(pid)
		var enc []byte
		if sealer != nil {
			blob, err := sealer.Seal(hash, "platform_id", []byte(pid))
			if err != nil {
				return updated, fmt.Errorf("backfill: seal %q: %w", pid, err)
			}
			enc = blob
		}
		res, err := db.Exec(`
			UPDATE platform_identities
			SET platform_id_hash = ?, platform_id_enc = ?
			WHERE platform_id = ? AND platform_id_hash = ''`,
			hash, enc, pid,
		)
		if err != nil {
			return updated, fmt.Errorf("backfill: update %q: %w", pid, err)
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	return updated, nil
}
