# ACP v0.4.6 — Phase 4.6 ACR-50 Access Gate

> Git tracks what changed. ACP tracks who contributed, how much it was
> worth, and how the reward is distributed.

This release closes Phase 4.6 (ACR-50 access flow), making ACP ready
for Phase 4.2 (public observation week — see
[`docs/PHASE-4.2-LAUNCH.md`](../PHASE-4.2-LAUNCH.md)).

## Status as of this release

| Phase | Scope | Status |
|---|---|---|
| 1 | MVP core (8 acceptance criteria) | ✅ |
| 2 | Reject paths, queries, MCP transport | ✅ |
| 2.5 | Infra hardening | ✅ |
| 3.0 | Housekeeping | ✅ |
| 3.A | Git Covenant Twin (ACR-400) | ✅ |
| 3.B | Token lifecycle | ✅ |
| 4.1 | Rate limiting (ACR-20 Part 4 Layer 2) | ✅ |
| 4.3 | Concentration warn (ACR-20 Part 4 Layer 5) | ✅ |
| 4.5 | At-rest encryption + key rotation (ACR-700) | ✅ |
| 4.6 | ACR-50 access flow + entry fees | ✅ |
| 4.2 | Public release + observation | 🟡 ready (this release) |
| 7.A | Escrow + Auto-Settlement (ACR-500) | 📝 spec drafting |

## What shipped in this release

### Phase 4.6 — ACR-50 access flow

- New endpoint: `POST /covenants/{id}/apply` (public, no session yet).
  Applicants submit `platform_id`, `tier_id`, optional `payment_ref`
  and `self_declaration`. Server seals the platform_id at rest under
  ACR-700 encryption, returns a 12-char hash prefix as receipt.
- New tools (owner): `approve_agent_access`, `reject_agent_access`.
- New tool (applicant): `get_agent_access_status` (unauthenticated;
  protected by 64-bit random `request_id` + `covenant_id` scoping).
- `list_members` now surfaces `pending_access_requests` for triage.
- Tier configuration accepts optional `entry_fee_tokens`. When
  non-zero, an applicant joining at that tier consumes the fee from
  their balance and the deduction lands in the audit log via the
  entry-fee ledger entry.

### Phase 4.5 — Encryption + key rotation

- AES-256-GCM at-rest sealing for `platform_id` (ACR-700 v0.1).
- Versioned keyring (`v{N}.key`) under the keyring directory; mode
  0600 enforced; UID check on load.
- `acp-server rotate-key` is O(1) — generates a new keyring version
  without touching encrypted rows.
- `acp-server reencrypt` is O(rows), idempotent — safely re-runs and
  re-encrypts everything under the latest key.
- `acp-doctor` includes `platform_id` residual scanner — verifies no
  plaintext platform_ids leaked into logs / params previews.
- Pluggable `KeyProvider` interface for KMS / Vault adapters — see
  [`docs/key-provider.md`](../key-provider.md). Reference build uses
  `LocalKeyfileProvider`; production deployers can write their own
  adapters when needed.

### Phase 4.3 — Concentration warning

- `concentration_warn_pct` (ACR-20 Part 4 Layer 5) — owners can set a
  threshold above which a single agent's share triggers a warning in
  `list_members`.

### Phase 4.1 — Rate limiting

- Per-(covenant, agent, hour) rate limiting via `rate_limit_per_hour`
  in `configure_anti_gaming`. Returns structured HTTP 429 envelope
  on exceedance. Counters in SQLite — rebuildable from audit log.

### Operational improvements (this release)

- **Boot-time keyring validation.** `acp-server` now opens the
  keyring at startup and refuses to start with an actionable error
  message if the keyring is missing/corrupted. Ends the
  lazy-load-misconfig pathology that bit ixdd-engine on 2026-05-08.
- **CI workflow.** `.github/workflows/ci.yml` runs go vet, build,
  test, and per-cmd binary builds on every push and PR. Visible on
  the README badge.
- **Reserved internal packages.** `internal/escrow/`,
  `internal/reputation/`, `internal/genesis/` exist as doc-only
  scaffolds with explicit "DO NOT IMPLEMENT" markers pointing to
  ACR-500/200/600 ratification gates respectively. Reserves the
  namespace; signals intent.
- **EstimateCost x402 hook.** Tool `EstimateCost` methods now accept
  an optional `cost_cents` param, letting x402-aware callers supply
  the cost at request time rather than relying on hardcoded
  placeholders. Forward-looking infrastructure for ACR-510 §x402.
- **LICENSE file added.** README always claimed MIT; the file now
  exists at the repo root.

### Documentation surface

- [`docs/PHASE-NEXT.md`](../PHASE-NEXT.md) — operator-facing one-page
  status snapshot
- [`docs/PHASE-7A-DECISIONS.md`](../PHASE-7A-DECISIONS.md) — the 10
  ACR-500 working-group decisions queued for ratification
- [`docs/PHASE-4.2-LAUNCH.md`](../PHASE-4.2-LAUNCH.md) — public
  launch + observation checklist
- [`docs/launch/`](.) — launch artefacts (X thread, HN post, deploy
  guides for Railway and Fly.io, this release notes file)

## Breaking changes

None. The legacy `/join` + `approve_agent` path still works alongside
the new ACR-50 flow for backwards compatibility.

If you previously deployed acp-server before v0.4.5 and stored
encrypted platform_ids, run `acp-server reencrypt` once after
upgrade to migrate them to the latest key version.

## What's NOT shipped (deliberately)

- **Phase 7.A — Escrow + Auto-Settlement.** ACR-500 v0.1 is drafted;
  ACR-500 v0.2 PROVISIONAL consolidates the v0.1 defaults; **10
  working-group decisions are queued for ratification**. No
  implementation code in this release. Tracking:
  [`docs/PHASE-7A-DECISIONS.md`](../PHASE-7A-DECISIONS.md).
- **Phase 5 — cross-Covenant reputation.** Spec drafted as ACR-200
  v0.1. Gated on Phase 7.A real-transaction data, by design.
- **Phase 6 — Genesis migration.** Spec drafted as ACR-600 v0.1.
  Gated on a specific OSS project asking to onboard.
- **Phase 7.B/C/D** — ACR-510 (multi-rail), ACR-520 (autonomous
  payment), ACR-530 (on-chain Merkle) all drafted in v0.1 SPECULATIVE
  status. None implementable until 7.A lands.
- **Migration 006** (drop plaintext `platform_id` column). Scheduled
  for Sprint 6 / Q3 2026-09-30 per ACR-700 compliance deadline.

## Verification

This release was tested with:

- `go test ./...` — full suite green
- `go vet ./...` — clean
- `acp_test.go` + `scenario_test.go` + `integration_test.go` for
  phase 1–4 acceptance criteria
- One real Covenant settled (`cvnt_a54e1c43`, 2026-04-15, 4,475 ink,
  hash chain valid)

## Get started

```bash
git clone https://github.com/ymow/acp-server
cd acp-server
go build ./...
./acp-server
```

See [Quickstart](https://acp-frontend.vercel.app/docs/quickstart) or
[`README.md`](../../README.md). Deploy guides for Railway / Fly.io in
[`docs/launch/`](.).

## Feedback (Phase 4.2 observation week)

This release initiates a 3-week observation window. Open issues with
the [Observation template](../../.github/ISSUE_TEMPLATE/observation.yml)
— your feedback shapes whether KMS adapters get promoted from
deferred, whether we move to Phase 7.A ratification next, or whether
distribution is the actual problem.

## Full changelog

```
- feat(tools): EstimateCost accepts cost_cents override + TODO.md sync
- feat(server): boot-time keyring validation + CI workflow + reserved package scaffolds
- docs(launch): Phase 4.2 launch artefacts — issue template + X/HN drafts + deploy guides
- docs(roadmap): operator-facing phase surface — PHASE-NEXT + 7A decisions + 4.2 launch
- chore: remove single-run settlement artifact and de-personalize tests
- docs: add X (Twitter) launch thread badge to README
- docs: KeyProvider extension point + BYO-KMS guidance
- feat: Phase 4.5.8 key rotation + reencrypt (ACR-700 §3.3)
- feat: Phase 4.6.C entry_fee ledger integration (ACR-50 §7)
- feat: Phase 4.6.A get_agent_access_status applicant poll (ACR-50 §2.3)
- feat: Phase 4.6.B list_members surfaces pending access requests (ACR-50 §7)
- feat: Phase 4.6.3 approve/reject access request tools (ACR-50 §§2,7)
- feat: Phase 4.6.2 apply_to_covenant service + HTTP endpoint (ACR-50 §2)
- feat: Phase 4.6.1 agent_access_requests schema (ACR-50 §§2,7)
- fix(crypto): align AAD error message + test comments with row_id rename
- feat: Phase 4.5.7 acp-doctor platform_id residual scanner (ACR-700 §4)
- feat: Phase 4.5.5 platform_id read-path redaction (ACR-700 §4)
- feat: Phase 4.5.4 platform_id writer cutover + backfill
- feat: Phase 4.5.3 platform_identities ACR-700 schema (additive)
- feat: Phase 4.5.2 Seal/Open AEAD helpers (ACR-700 §§2.3, 5.2)
- feat: Phase 4.5.1 KeyProvider + LocalKeyfileProvider (ACR-700 §5.1)
- feat: Phase 4.3 concentration_warn_pct (ACR-20 Part 4 Layer 5)
- feat: Phase 4.1 rate_limit_per_hour (ACR-20 Part 4 Layer 2)
- feat: Phase 3.A Git Covenant Twin (ACR-400 v0.2 chunks 1-7)
- feat: Phase 3.B Token Lifecycle (ACR-20 Parts 1/2/5/7 + leave_covenant)
```

(Earlier history available via `git log` on the repo.)

## When to publish this release

Per [`docs/PHASE-4.2-LAUNCH.md`](../PHASE-4.2-LAUNCH.md), publish
this release as part of the launch checklist:

```
gh release create acp-server-v0.4.6 \
  --title "ACP v0.4.6 — Phase 4.6 ACR-50 Access Gate" \
  --notes-file docs/launch/RELEASE-NOTES-v0.4.6.md \
  --discussion-category "Releases" \
  --latest
```

Don't tag until ready for the public observation window — once the
release is live, the X thread + HN post should follow within the
same business day to compress the inbound peak.

---

**Tag (when ready):** `acp-server-v0.4.6`
**License:** MIT
**Repo:** https://github.com/ymow/acp-server
