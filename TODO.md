# TODO — Known Gaps

Spec gaps surfaced by the 2026-04-17 ACP spec ⇔ implementation review.
Kept out of code so the repo stays clean; tracked here until promoted to
tickets or fixed.

---

## Security

### platform_id at-rest encryption — DONE (Phase 4.5 / ACR-700)

**STATUS: ✅ DONE (Phase 4.5)** — 加密已完成；剩餘工作是移除 plaintext 欄位（Migration 006），計畫 Sprint 6 / Q3 2026-09-30 前完成。

- **Spec:** `ACP_Covenant_Spec_v0.2_EN.md:159` — "platform_id stored encrypted;
  only InkMesh internal systems can decrypt."
- **Resolved (2026-04-xx, Phase 4.5):** AES-256-GCM encryption fully implemented.
  - `platform_identities.platform_id_hash` (SHA-256 hex, indexable) +
    `platform_id_enc` (sealed blob) written on every upsert.
  - `internal/crypto/seal.go`: AES-256-GCM with version+key_version+nonce header.
  - `internal/keys/local.go`: versioned keyring (v{N}.key), 0600 perms, UID check.
  - `internal/covenant/backfill.go`: `BackfillLegacyRows()` idempotent migration.
  - `covenant.Member.PlatformID` tagged `json:"-"`; HTTP layer never leaks it.
  - Tests: `internal/covenant/platform_id_test.go` (6 tests including `TestMemberJSONOmitsPlatformID`).
- **Remaining work (Migration 006):** The `platform_id` plaintext column still exists as PK in
  `platform_identities` and FK in `covenant_members` (32 call sites). Dropping
  it requires Migration 006 to promote `platform_id_hash` as the new key.
  Scheduled for Sprint 6 / Q3 2026-09-30 (ACR-700 compliance deadline). Until then, the
  plaintext column remains but is never returned in HTTP responses.

---

## Accounting

### cost_delta values are placeholder constants, not real external spend
- **Spec:** `ACR-300_Audit_Log_v0.2.md:85` — "呼叫外部 API 花費 USD 0.05 →
  cost_delta = 5" (INTEGER cents, represents actual x402 / API payment).
- **Partially resolved (schema + types + currency):** cost is now INTEGER
  minor-units end-to-end plus an ISO-4217 `cost_currency` column. Schema
  columns `audit_logs.cost_delta`, `covenants.budget_limit`,
  `budget_counters.budget_limit/spent`, and `budget_reservations.amount`
  are all `INTEGER`. Go types: `execution.SideEffects.CostDelta`,
  `execution.Receipt.CostDelta`, `audit.Entry.CostDelta`,
  `budget.State.Budget{Limit,Spent}`, `covenant.Covenant.BudgetLimit`
  are `int64`. `EstimateCost` returns `int64`. `NetDelta` stays `float64`
  because `cost_weight × cost_delta` can be fractional.
- **Currency (Path A):** `audit_logs.cost_currency TEXT NOT NULL DEFAULT 'USD'`
  added to the schema + migration. `audit.Entry.CostCurrency`,
  `execution.SideEffects.CostCurrency`, `execution.Receipt.CostCurrency`
  default to `"USD"` when unset.
- **Currency (Phase 3.0 follow-up):** propagated to the budget layer.
  `covenants.budget_currency` is the authoritative single-currency contract
  per covenant; `budget_counters.currency` mirrors it. `execution.Run` Step 4
  rejects any charge whose `cost_currency` does not match the covenant's
  `budget_currency`, releasing the Step 2.5 reservation and logging a rejection
  — so budget package internals can assume uniform currency and never mix
  USD cents with EUR cents. Multi-currency per covenant is still out of scope
  (would need a currency-aware ledger, deferred to Phase 7 with x402).
- **Hash chain impact:** `audit.computeHash` branches on `spec_version`
  — 2.0 rows keep `%.8f` cost, 2.1 rows use `%d` cost with no currency
  in the payload, 2.2 rows include `cost_currency` as a hash component
  so 10 USD-cents cannot collide with 10 EUR-cents. `VerifyChain` handles
  all three. Schema default bumped to 2.2.
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
