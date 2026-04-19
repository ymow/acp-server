package db

import (
	"path/filepath"
	"testing"
)

// TestAgentAccessRequestsSchema pins the column layout for the Phase 4.6.1
// ACR-50 request table. Any change to this test signals a schema drift that
// the apply/approve/reject flows must be updated against.
func TestAgentAccessRequestsSchema(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`PRAGMA table_info(agent_access_requests)`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = ctype
	}

	want := map[string]string{
		"request_id":       "TEXT",
		"covenant_id":      "TEXT",
		"platform_id_hash": "TEXT",
		"platform_id_enc":  "BLOB",
		"tier_id":          "TEXT",
		"payment_ref":      "TEXT",
		"self_declaration": "TEXT",
		"status":           "TEXT",
		"reject_reason":    "TEXT",
		"approve_log_id":   "TEXT",
		"reject_log_id":    "TEXT",
		"created_at":       "TEXT",
		"resolved_at":      "TEXT",
	}
	for col, ctype := range want {
		if got[col] != ctype {
			t.Errorf("column %q: got type %q, want %q", col, got[col], ctype)
		}
	}
	for col := range got {
		if _, ok := want[col]; !ok {
			t.Errorf("unexpected column %q (type %q) — update the test or revert the schema", col, got[col])
		}
	}
}
