package gittwin

// ACR-400 Part 5: Git Anchor.
//
// An anchor binds one SettlementOutput to a cryptographic pointer that the
// bridge will write to refs/notes/acp-anchors on the twinned repo. v0.1 is
// an opt-in "server enqueues, bridge writes" split: the server records
// settlement_hash + snapshot_hash + a pre-rendered JSON payload; the bridge
// polls /git-twin/anchors/pending, writes a git note, then acks with the
// resulting commit SHA. We deliberately do not sign here — ed25519 signing
// is v0.2 material (chunk 7).

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/inkmesh/acp-server/internal/id"
)

// Anchor is the in-memory shape of a git_twin_anchors row.
type Anchor struct {
	AnchorID           string `json:"anchor_id"`
	CovenantID         string `json:"covenant_id"`
	SettlementOutputID string `json:"settlement_output_id"`
	RepoURL            string `json:"repo_url"`
	SettlementHash     string `json:"settlement_hash"`
	SnapshotHash       string `json:"snapshot_hash"`
	NoteBody           string `json:"note_body"`
	Status             string `json:"status"`
	EnqueuedAt         string `json:"enqueued_at"`
	WrittenAt          string `json:"written_at,omitempty"`
	WrittenCommitSHA   string `json:"written_commit_sha,omitempty"`
}

// AnchorNoteV1 is the JSON payload the bridge writes as a git note body.
// Field order must stay stable — bridge and downstream verifiers re-hash
// this struct (with Signature nulled) to verify ed25519 signatures.
type AnchorNoteV1 struct {
	Type               string           `json:"type"`
	CovenantID         string           `json:"covenant_id"`
	SettlementOutputID string           `json:"settlement_output_id"`
	SettlementHash     string           `json:"settlement_hash"`
	SnapshotHash       string           `json:"snapshot_hash"`
	TotalTokens        int              `json:"total_tokens"`
	GeneratedAt        string           `json:"generated_at"`
	Signature          *AnchorSignature `json:"signature,omitempty"`
}

// AnchorSignature is the optional ed25519 attestation over a canonical
// rendering of AnchorNoteV1 with Signature=nil. Algorithm is explicit so
// future key rollovers (ed25519 → secp256k1, etc.) can coexist in the same
// refs/notes/acp-anchors history.
type AnchorSignature struct {
	Algorithm string `json:"alg"`
	PublicKey string `json:"public_key"` // base64-encoded raw pubkey bytes
	Value     string `json:"value"`      // base64-encoded signature
}

// canonicalUnsignedAnchor returns the exact bytes the signer signed and the
// verifier checks against. Signature is always nulled out before marshaling
// so the byte sequence is well-defined regardless of whether a signature is
// already attached.
func canonicalUnsignedAnchor(note *AnchorNoteV1) ([]byte, error) {
	dup := *note
	dup.Signature = nil
	return json.Marshal(&dup)
}

// parseAnchorNote is a small helper used by VerifyAnchorSignature. Kept
// package-private so external callers go through the Verify API rather
// than poking at raw JSON.
func parseAnchorNote(raw []byte) (*AnchorNoteV1, error) {
	var n AnchorNoteV1
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("parse anchor note: %w", err)
	}
	return &n, nil
}

var ErrAnchorNotFound = errors.New("anchor not found")

// EnqueueAnchor records a pending anchor for the given settlement. Callers
// (tools.ConfirmSettlementOutput) should invoke this only when the covenant
// has a git twin bound. The returned AnchorID is surfaced in the tool receipt
// so the bridge can trace the enqueue → note write correlation.
//
// When signer is non-nil, the serialized note carries an ed25519 signature
// over the canonical (unsigned) rendering. A nil signer produces an unsigned
// note — ACR-400 v0.2 permits this for v0.1-compat deployments.
func EnqueueAnchor(db *sql.DB, covenantID, repoURL, settlementOutputID string, signer Signer) (*Anchor, error) {
	if repoURL == "" {
		return nil, errors.New("enqueue anchor: repo_url required")
	}
	// Pull settlement facts so the server, not the bridge, is authoritative
	// about what goes into the note payload. Bridge compromise should not
	// let an attacker rewrite the anchor contents.
	var settlementHash string
	var totalTokens int
	var generatedAt string
	err := db.QueryRow(`
		SELECT trigger_log_hash, total_tokens, generated_at
		FROM settlement_outputs WHERE output_id=? AND covenant_id=?`,
		settlementOutputID, covenantID,
	).Scan(&settlementHash, &totalTokens, &generatedAt)
	if err != nil {
		return nil, fmt.Errorf("load settlement %q: %w", settlementOutputID, err)
	}

	snapshotHash, err := rollupSnapshotHash(db, covenantID)
	if err != nil {
		return nil, err
	}

	note := AnchorNoteV1{
		Type:               "acp.anchor.settlement.v1",
		CovenantID:         covenantID,
		SettlementOutputID: settlementOutputID,
		SettlementHash:     settlementHash,
		SnapshotHash:       snapshotHash,
		TotalTokens:        totalTokens,
		GeneratedAt:        generatedAt,
	}

	if signer != nil {
		canonical, err := canonicalUnsignedAnchor(&note)
		if err != nil {
			return nil, fmt.Errorf("canonicalize anchor note: %w", err)
		}
		sig, err := signer.Sign(canonical)
		if err != nil {
			return nil, fmt.Errorf("sign anchor note: %w", err)
		}
		note.Signature = &AnchorSignature{
			Algorithm: signer.Algorithm(),
			PublicKey: base64.StdEncoding.EncodeToString(signer.PublicKey()),
			Value:     base64.StdEncoding.EncodeToString(sig),
		}
	}

	bodyBytes, err := json.Marshal(note)
	if err != nil {
		return nil, fmt.Errorf("marshal anchor note: %w", err)
	}

	a := &Anchor{
		AnchorID:           id.AnchorID(),
		CovenantID:         covenantID,
		SettlementOutputID: settlementOutputID,
		RepoURL:            repoURL,
		SettlementHash:     settlementHash,
		SnapshotHash:       snapshotHash,
		NoteBody:           string(bodyBytes),
		Status:             "pending",
		EnqueuedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	_, err = db.Exec(`
		INSERT INTO git_twin_anchors
		  (anchor_id, covenant_id, settlement_output_id, repo_url,
		   settlement_hash, snapshot_hash, note_body, status, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		a.AnchorID, a.CovenantID, a.SettlementOutputID, a.RepoURL,
		a.SettlementHash, a.SnapshotHash, a.NoteBody, a.EnqueuedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert anchor: %w", err)
	}
	return a, nil
}

// ListPendingAnchors returns pending anchors ordered by enqueued_at asc. When
// repoURL is non-empty, only anchors for that repo are returned — lets a
// bridge that fronts multiple repos filter to the one it's about to write.
// limit<=0 is treated as "no cap"; the bridge already paginates via enqueue
// order so an unbounded read is safe at v0.1 volumes.
func ListPendingAnchors(db *sql.DB, repoURL string, limit int) ([]Anchor, error) {
	query := `
		SELECT anchor_id, covenant_id, settlement_output_id, repo_url,
		       settlement_hash, snapshot_hash, note_body, status, enqueued_at,
		       COALESCE(written_at,''), COALESCE(written_commit_sha,'')
		FROM git_twin_anchors WHERE status='pending'`
	args := []any{}
	if repoURL != "" {
		query += ` AND repo_url=?`
		args = append(args, repoURL)
	}
	query += ` ORDER BY enqueued_at ASC`
	if limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Anchor
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.AnchorID, &a.CovenantID, &a.SettlementOutputID, &a.RepoURL,
			&a.SettlementHash, &a.SnapshotHash, &a.NoteBody, &a.Status, &a.EnqueuedAt,
			&a.WrittenAt, &a.WrittenCommitSHA); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AckAnchor marks a pending anchor as written and records the resulting commit
// SHA. Idempotent on "already written": a second ack with the same SHA is a
// no-op; a second ack with a different SHA is rejected so split-brain bridges
// cannot silently overwrite each other's anchor references.
func AckAnchor(db *sql.DB, anchorID, commitSHA string) error {
	if commitSHA == "" {
		return errors.New("written_commit_sha required")
	}
	var status, existingSHA string
	err := db.QueryRow(
		`SELECT status, COALESCE(written_commit_sha,'') FROM git_twin_anchors WHERE anchor_id=?`,
		anchorID,
	).Scan(&status, &existingSHA)
	if err == sql.ErrNoRows {
		return ErrAnchorNotFound
	}
	if err != nil {
		return err
	}
	if status == "written" {
		if existingSHA == commitSHA {
			return nil
		}
		return fmt.Errorf("anchor %s already written at %s; refusing to overwrite with %s",
			anchorID, existingSHA, commitSHA)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(
		`UPDATE git_twin_anchors SET status='written', written_at=?, written_commit_sha=?
		 WHERE anchor_id=?`,
		now, commitSHA, anchorID,
	)
	return err
}

// rollupSnapshotHash hashes the sorted per-agent snapshot_hash values for the
// covenant into one commitment, so the anchor can be verified against the
// token_snapshots rows captured at LOCKED without listing every agent in the
// note. Returns "" when the covenant has no snapshots (settle-from-ACTIVE or
// empty ledger); callers should still allow the anchor in that case because
// the settlement_hash already binds the audit chain.
func rollupSnapshotHash(db *sql.DB, covenantID string) (string, error) {
	rows, err := db.Query(
		`SELECT snapshot_hash FROM token_snapshots WHERE covenant_id=?`, covenantID,
	)
	if err != nil {
		return "", fmt.Errorf("load snapshots: %w", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return "", err
		}
		if h != "" {
			hashes = append(hashes, h)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(hashes) == 0 {
		return "", nil
	}
	sort.Strings(hashes)
	h := sha256.New()
	for _, v := range hashes {
		h.Write([]byte(v))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
