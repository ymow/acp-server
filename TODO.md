# TODO — Known Gaps

Spec gaps surfaced by the 2026-04-17 ACP spec ⇔ implementation review.
Kept out of code so the repo stays clean; tracked here until promoted to
tickets or fixed.

---

## Security

### platform_id at-rest encryption
- **Spec:** `ACP_Covenant_Spec_v0.2_EN.md:159` — "platform_id stored encrypted;
  only InkMesh internal systems can decrypt. Decryptable under legal
  requirement; not decrypted under normal circumstances."
- **Roadmap status:** `ACP_Roadmap.md:241` lists "platform_id KMS 加密 | AWS KMS +
  AES-256-GCM" as future work.
- **Current state:** `internal/covenant/covenant.go:42-52` and
  `internal/db/schema.sql` store `platform_id` in plaintext in
  `platform_identities` and `covenant_members`.
- **Fix sketch:**
  1. Add `platform_id_hash TEXT` (SHA-256) as lookup key; keep `platform_id_enc
     TEXT` for the encrypted payload.
  2. AES-256-GCM wrapper in `internal/crypto/` reading key from
     `ACP_KMS_KEY` env (base64 32B) for MVP, swap for AWS KMS later.
  3. Migration: hash + encrypt existing rows, drop plaintext column.
  4. Update all 32 call sites across `covenant/`, `api/`, `schema.sql`,
     `scenario_test.go`, `integration_test.go`.
- **Trigger to do it:** before accepting real user PII / going to production.

---

## Accounting

### cost_delta values are placeholder constants, not real external spend
- **Spec:** `ACR-300_Audit_Log_v0.2.md:85` — "呼叫外部 API 花費 USD 0.05 →
  cost_delta = 5" (INTEGER cents, represents actual x402 / API payment).
- **Current state:** Every tool's `EstimateCost` returns a hardcoded constant
  (`propose_passage`=10, `approve_draft`=5, `generate_settlement`=20). Local
  SQLite operations with zero external spend are nonetheless charged.
  `cost_delta` is also `REAL` instead of the spec's `INTEGER cents`.
- **Why deferred:** real cost wiring requires the x402 integration
  (see below). Zeroing the placeholders would disable budget-gate coverage
  in tests. The Apr-17 commit (`ec5e6ca`) made sure whatever value
  `EstimateCost` returns now flows through `audit_logs.cost_delta` and
  `net_delta` correctly, which was the more urgent bug.
- **Fix sketch:**
  1. Change schema column type `cost_delta REAL` → `cost_delta INTEGER`
     (cents) — spec alignment + eliminates float rounding.
  2. Make `EstimateCost` param-aware: accept `cost_cents` in params
     (declared external spend) defaulting to 0.
  3. For local-only tools, hard-code `return 0` and cover the budget gate
     in tests via a synthetic "paid_tool" fixture.
  4. When x402 lands, populate `cost_cents` from the actual payment
     receipt in Step 3.

### No budget counter rebuild from audit_log
- **Spec:** `ACP_Implementation_Spec_MVP.md` Part 8 — Redis restart must be
  able to reconstruct `budget_spent` by summing `audit_log.cost_delta`.
- **Current state:** `internal/budget/budget.go` holds state in SQLite only;
  no rebuild routine exists. Not an issue today (SQLite is durable) but
  will be when Phase 2 swaps in Redis.
- **Fix sketch:** `budget.RebuildFromAuditLog(db, covenantID)` that runs
  `SELECT SUM(cost_delta) FROM audit_logs WHERE covenant_id=? AND result='success'`
  and writes the result to `budget_counters`.

---

## Identity & Policy

### ParamsPolicy is ad-hoc masking
- **Spec:** `ACP_Covenant_Spec_v0.2_EN.md` Part 6 — `ParamsPolicy` struct
  with `store_hash_only` / `preview_fields`.
- **Current state:** `internal/execution/execution.go:194-212` — inline
  `maskSensitive()` with hardcoded field list (`content`, `text`, `draft`,
  `password`).
- **Fix sketch:** per-tool `ParamsPolicy` declaration; engine reads
  policy and applies masking uniformly.

---

## Next-phase items (already on ACP_Roadmap)

- x402 HTTP 402 integration — real external cost tracking (unblocks
  `cost_delta` correctness above)
- Per-agent budget ceilings (only global today)
- ACR-50 automated access-tier approval
- Reactor / Blueprint aggregation layer
- On-chain Git anchoring
