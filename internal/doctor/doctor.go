// Package doctor runs offline integrity checks against an acp-server
// database. Phase 4.5.7 ships platform_id residual scans that back ACR-700 §4:
// every audit log preview and every stored platform_identities row must be
// consistent with the redaction and encryption guarantees that land in 4.5.3
// through 4.5.5. CI or an operator can run this periodically and fail the
// build on any Error finding.
package doctor

import (
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
)

type Severity int

const (
	Info Severity = iota
	Warn
	Error
)

func (s Severity) String() string {
	switch s {
	case Info:
		return "OK"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	}
	return "?"
}

// Finding is one check's verdict. Check is a stable dotted ID so operators
// can filter or suppress specific checks without text matching. Details
// hold per-row specifics (audit log IDs, platform_id prefixes) and are
// truncated upstream to keep logs usable.
type Finding struct {
	Check    string
	Severity Severity
	Message  string
	Details  []string
}

type Report struct {
	Findings []Finding
}

// ExitCode returns 1 if any finding has Error severity, 0 otherwise. Warn
// and Info findings are informational and do not fail CI.
func (r Report) ExitCode() int {
	for _, f := range r.Findings {
		if f.Severity == Error {
			return 1
		}
	}
	return 0
}

func (r Report) Print(w io.Writer) {
	for _, f := range r.Findings {
		fmt.Fprintf(w, "[%s] %s: %s\n", f.Severity, f.Check, f.Message)
		for _, d := range f.Details {
			fmt.Fprintf(w, "       %s\n", d)
		}
	}
}

// maxDetails caps per-finding detail lines so a pathological leak across
// thousands of audit rows does not drown the output. The count is still
// reported in the Message.
const maxDetails = 20

// Run executes every registered check against db and returns an aggregated
// Report. Checks are independent; a failure in one does not short-circuit
// the rest so an operator sees the full picture in a single pass.
func Run(db *sql.DB) Report {
	checks := []func(*sql.DB) Finding{
		checkPlatformIDHashCoverage,
		checkPlatformIDEncCoverage,
		checkPlatformIDPreviewResidual,
		checkPlatformIDResultDetailResidual,
	}
	out := Report{}
	for _, c := range checks {
		out.Findings = append(out.Findings, c(db))
	}
	return out
}

// checkPlatformIDHashCoverage verifies every platform_identities row has
// platform_id_hash populated. An empty hash means the 4.5.4 backfill never
// ran (or failed partway), which would leave 4.5.5 lookups unable to find
// the row. This is always an Error: the fix is to restart acp-server so
// the startup backfill runs, or invoke the backfill directly.
func checkPlatformIDHashCoverage(db *sql.DB) Finding {
	const name = "platform_id.hash_coverage"
	var total, missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_identities`).Scan(&total); err != nil {
		return Finding{Check: name, Severity: Error, Message: "count total: " + err.Error()}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_identities WHERE platform_id_hash = ''`).Scan(&missing); err != nil {
		return Finding{Check: name, Severity: Error, Message: "count missing hash: " + err.Error()}
	}
	if missing > 0 {
		return Finding{
			Check:    name,
			Severity: Error,
			Message:  fmt.Sprintf("%d of %d rows missing platform_id_hash (run BackfillPlatformIdentities)", missing, total),
		}
	}
	return Finding{
		Check:    name,
		Severity: Info,
		Message:  fmt.Sprintf("%d rows, all hashed", total),
	}
}

// checkPlatformIDEncCoverage reports rows with NULL platform_id_enc. Severity
// is Warn, not Error: rows inserted when the server has no KeyProvider (dev
// setups, tests without a sealer) legitimately land with NULL. An operator
// who expects encryption everywhere should treat Warn as a follow-up, not
// a CI failure — plaintext has not leaked, the blob is simply absent.
func checkPlatformIDEncCoverage(db *sql.DB) Finding {
	const name = "platform_id.enc_coverage"
	var total, missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_identities`).Scan(&total); err != nil {
		return Finding{Check: name, Severity: Error, Message: "count total: " + err.Error()}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM platform_identities WHERE platform_id_enc IS NULL`).Scan(&missing); err != nil {
		return Finding{Check: name, Severity: Error, Message: "count missing enc: " + err.Error()}
	}
	if missing > 0 {
		return Finding{
			Check:    name,
			Severity: Warn,
			Message:  fmt.Sprintf("%d of %d rows missing platform_id_enc (sealer offline when row was written)", missing, total),
		}
	}
	return Finding{
		Check:    name,
		Severity: Info,
		Message:  fmt.Sprintf("%d rows, all sealed", total),
	}
}

// checkPlatformIDPreviewResidual scans audit_logs.params_preview for plaintext
// platform_id occurrences. Every known identity in platform_identities is a
// candidate substring; a hit means a tool's ParamsPolicy did not mark the
// plaintext field as sensitive and the plaintext landed in the durable log.
// This violates ACR-700 §4 and the fix is to add the offending param name
// to ParamsPolicy.SensitiveFields (see record_git_twin_event for the pattern).
func checkPlatformIDPreviewResidual(db *sql.DB) Finding {
	return scanAuditForPlatformIDs(db, "platform_id.preview_residual", "params_preview")
}

// checkPlatformIDResultDetailResidual scans audit_logs.result_detail. Most
// tools only write short status strings there, but any tool that echoes
// an error with the raw platform_id in it would leak. Covered here rather
// than left to manual review.
func checkPlatformIDResultDetailResidual(db *sql.DB) Finding {
	return scanAuditForPlatformIDs(db, "platform_id.result_detail_residual", "result_detail")
}

// likeEscape escapes the two SQLite LIKE metacharacters so a platform_id
// like "foo_bar" matches literally, not as "foo<any>bar". We use '\' as
// the escape character and must declare it with ESCAPE on the LIKE clause.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likeEscape(s string) string { return likeEscaper.Replace(s) }

func scanAuditForPlatformIDs(db *sql.DB, checkName, column string) Finding {
	// Platform_identities.platform_id is still plaintext in 4.5.x; once a
	// later phase removes it, this query shifts to decrypting platform_id_enc.
	// Ordering stabilises test output.
	rows, err := db.Query(`SELECT platform_id FROM platform_identities ORDER BY platform_id`)
	if err != nil {
		return Finding{Check: checkName, Severity: Error, Message: "load identities: " + err.Error()}
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return Finding{Check: checkName, Severity: Error, Message: "scan identity: " + err.Error()}
		}
		if pid == "" {
			continue
		}
		ids = append(ids, pid)
	}
	if err := rows.Err(); err != nil {
		return Finding{Check: checkName, Severity: Error, Message: "iterate identities: " + err.Error()}
	}
	if len(ids) == 0 {
		return Finding{Check: checkName, Severity: Info, Message: "no identities to scan"}
	}

	// One LIKE per identity. For the sizes this tool targets (thousands of
	// rows, hundreds of identities), this is cheaper than building a regex
	// CTE and keeps the code readable.
	q := fmt.Sprintf(`SELECT log_id FROM audit_logs WHERE %s LIKE ? ESCAPE '\'`, column)
	hits := map[string][]string{} // pid → log_ids
	totalHits := 0
	for _, pid := range ids {
		r, err := db.Query(q, "%"+likeEscape(pid)+"%")
		if err != nil {
			return Finding{Check: checkName, Severity: Error, Message: "scan " + column + ": " + err.Error()}
		}
		for r.Next() {
			var logID string
			if err := r.Scan(&logID); err != nil {
				r.Close()
				return Finding{Check: checkName, Severity: Error, Message: "scan row: " + err.Error()}
			}
			hits[pid] = append(hits[pid], logID)
			totalHits++
		}
		r.Close()
	}
	if totalHits == 0 {
		return Finding{
			Check:    checkName,
			Severity: Info,
			Message:  fmt.Sprintf("scanned %d identities across audit_logs.%s, no residuals", len(ids), column),
		}
	}

	// Sort pids for deterministic output; cap the per-finding detail list.
	pids := make([]string, 0, len(hits))
	for pid := range hits {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	var details []string
	for _, pid := range pids {
		for _, logID := range hits[pid] {
			if len(details) >= maxDetails {
				break
			}
			details = append(details, fmt.Sprintf("log_id=%s pid_prefix=%s", logID, pidPrefix(pid)))
		}
		if len(details) >= maxDetails {
			break
		}
	}
	return Finding{
		Check:    checkName,
		Severity: Error,
		Message:  fmt.Sprintf("%d audit_logs.%s rows leak plaintext platform_id across %d identities", totalHits, column, len(hits)),
		Details:  details,
	}
}

// pidPrefix keeps the Details output from re-publishing the very plaintext
// the check was raised to catch. Operators can cross-reference via the
// log_id without the doctor echoing the identity itself.
func pidPrefix(pid string) string {
	if len(pid) <= 8 {
		return pid[:len(pid)/2] + "…"
	}
	return pid[:8] + "…"
}
