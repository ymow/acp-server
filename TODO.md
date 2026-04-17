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
- **Partially resolved (schema + types):** cost is now INTEGER cents
  end-to-end. Schema columns `audit_logs.cost_delta`,
  `covenants.budget_limit`, `budget_counters.budget_limit/spent`, and
  `budget_reservations.amount` are all `INTEGER`. Go types:
  `execution.SideEffects.CostDelta`, `execution.Receipt.CostDelta`,
  `audit.Entry.CostDelta`, `budget.State.Budget{Limit,Spent}`,
  `covenant.Covenant.BudgetLimit` are `int64`. `EstimateCost` returns
  `int64`. `NetDelta` stays `float64` because `cost_weight × cost_delta`
  can be fractional.
- **Hash chain impact:** `audit.computeHash` branches on `spec_version`
  — rows stamped `ACR-300@2.0` still format cost as `%.8f` (historical
  compatibility); new rows stamp `ACR-300@2.1` and format cost as `%d`.
  `VerifyChain` handles both. Schema default bumped to 2.1.
- **Still placeholder-valued:** `EstimateCost` returns hardcoded cents
  (`propose_passage`=10, `approve_draft`=5, `generate_settlement`=20);
  no real external spend yet. Unlocking real values needs x402 (below).
- **Remaining fix sketch:**
  1. Make `EstimateCost` param-aware: accept `cost_cents` in params
     (declared external spend) defaulting to 0.
  2. For local-only tools, hard-code `return 0` and cover the budget gate
     in tests via a synthetic "paid_tool" fixture.
  3. When x402 lands, populate `cost_cents` from the actual payment
     receipt in Step 3.

### No budget counter rebuild from audit_log — DONE
- **Spec:** `ACP_Implementation_Spec_MVP.md` Part 8 — Redis restart must be
  able to reconstruct `budget_spent` from durable storage.
- **Resolved:** `budget.RebuildFromAuditLog(db, covenantID)` in
  `internal/budget/budget.go`. The naive `SUM(cost_delta WHERE result='success')`
  from the original sketch would have over-counted refunds, because
  `reject_draft` logs itself as its own success row with cost_delta=0 rather
  than a negative cost_delta on the original entry. The implementation LEFT
  JOINs `token_ledger` and excludes entries whose ledger status is
  `rejected` or `reversed`, matching what the live counter accumulates via
  `RecordSpend` / `Release`.
- **Tests:** `TestRebuildFromAuditLog` (propose+approve+reject, wipe,
  rebuild → 10), `_MissingCounter` (errors when EnsureCounter not called),
  `_EmptyLedger` (zeroes drift when no audit rows exist).

---

## Identity & Policy

### ParamsPolicy is ad-hoc masking — DONE
- **Spec:** `ACP_Covenant_Spec_v0.2_EN.md` Part 6 — `ParamsPolicy` struct
  with `store_hash_only` / `preview_fields` / `sensitive_fields`.
- **Resolved:** `internal/execution/policy.go` defines `ParamsPolicy` +
  `ApplyParamsPolicy`. Tools optionally implement `PolicyAwareTool` to
  declare their policy; the engine resolves per-call via `resolvePolicy()`.
  `maskSensitive()` has been removed; rejection-path logging now uses the
  same policy as success-path.
- **Semantics:** whitelist (`PreviewFields`) drops non-listed keys;
  blacklist (`SensitiveFields`) masks values as `*** (length: N)` with
  rune-aware length (fixes prior byte-length bug for CJK text); hash
  fields (`HashPreviewFields`) truncate to first 8 runes + `...`;
  `StoreHashOnly` emits a single deterministic sha256 of canonical JSON.
- **Per-tool policies:** propose_passage (whitelist bookkeeping + hash
  preview), approve_draft (whitelist log/draft ids + metrics), reject_draft
  (log_id + reason), approve_agent / reject_agent (agent_id + reason),
  confirm_settlement_output (output_id), generate_settlement /
  configure_token_rules (Default policy — rules tree is audit-worthy).
- **Tests:** `policy_test.go` covers whitelist, sensitive mask, rune
  length, hash preview, stacked policies, StoreHashOnly determinism,
  input immutability, default legacy behaviour.

---

## Next-phase items (already on ACP_Roadmap)

- x402 HTTP 402 integration — real external cost tracking (unblocks
  `cost_delta` correctness above)
- Per-agent budget ceilings (only global today)
- ACR-50 automated access-tier approval
- Reactor / Blueprint aggregation layer
- On-chain Git anchoring
