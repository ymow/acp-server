# PHASE-7A-DECISIONS — Working-Group Ratification Queue

> Phase 7.A (Escrow + Auto-Settlement) cannot start coding until ten
> implementation-blocking decisions are ratified. This page is the
> **operator-facing tally sheet** — not the spec.
>
> Canonical spec text lives in
> [`inkmesh/Agent Covenant Protocol_ACP/ACR-500_Decisions_v0.1.md`](https://github.com/ymow/acp-server)
> (236 lines, includes "Impact if reversed" analysis per decision).
>
> Updated: 2026-05-09. Status snapshot from spec v0.1.

---

## How to use this page

1. **Working-group member?** Read the spec doc first. Don't ratify from
   this summary alone.
2. **For each decision:** record your vote in the table below — `accept`
   (default stands), `reject` (default does not stand; reasoning needed),
   or `defer` (more info required, blocking question listed).
3. **Threshold:** ≥80% of decisions ratified (accept *or* reject with
   resolution captured) unblocks Phase 7.A implementation. <50% means
   spec needs a v0.2 design pass first.
4. **Don't ship code while >2 decisions are still open** — half-ratified
   contracts produce architecture you'll regret.

---

## The 10 decisions

| # | Topic | v0.1 default | Why it matters | Status |
|---|---|---|---|---|
| **C-1** | Spec authority — ACR-100 §4 vs ACR-500 | Both stand; ACR-500 supersedes REVIEW-07 | If reversed, ACR-100 §4 needs major rewrite + Phase 7.A scope changes | 🟠 OPEN |
| **C-2** | Escrow ⇄ x402 Pull layering | Stacked (escrow source, x402 rail) | Reversal forces a `pull-only` rail profile + `escrow_id NULL` path in `withdrawal_offers` | 🟠 OPEN |
| **C-3** | Lock timing — when deposit gate takes effect | DRAFT → OPEN transition | Affects whether owners can configure budgets after participants join | 🟠 OPEN |
| **C-4** | Budget growth requires top-up? | Yes (`ErrEscrowTopUpRequired`) | If no, owners can over-promise; if yes, UX needs top-up flow | 🟠 OPEN |
| **C-5** | `reject_draft` escrow refund? | No | If yes, rejected drafts return funds to escrow pool; if no, drafted-but-rejected work consumes nothing | 🟠 OPEN |
| **C-6** | Single-asset escrow only? | Yes | Multi-asset escrow = exchange-rate complexity; single-asset = simpler invariants | 🟠 OPEN |
| **C-7** | Custody model | Profile B (multisig) testnet first | A: server custody; B: multisig; C: full self-custody. Determines compliance posture | 🟠 OPEN |
| **C-8** | Gas / fee allocation | Deduct from claimant share | Alternative: owner pays gas at settlement. Affects effective payout math | 🟠 OPEN |
| **C-9** | Settlement timeout days | 180 | After timeout, what happens? Refund-to-owner vs claim-window-extension vs forfeit | 🟠 OPEN |
| **C-10** | Departed Agent claim rights | Yes (Constitutional §5) | If no, agents who `leave_covenant` lose pre-settlement entitlement; political call | 🟠 OPEN |

---

## Ratification template (copy this section per WG member)

```
Reviewer: <name>
Date:     <YYYY-MM-DD>
Spec ref: ACR-500_Decisions_v0.1.md (commit-pin: <git-sha>)

C-1: [accept | reject | defer]   notes:
C-2: [accept | reject | defer]   notes:
C-3: [accept | reject | defer]   notes:
C-4: [accept | reject | defer]   notes:
C-5: [accept | reject | defer]   notes:
C-6: [accept | reject | defer]   notes:
C-7: [accept | reject | defer]   notes:
C-8: [accept | reject | defer]   notes:
C-9: [accept | reject | defer]   notes:
C-10: [accept | reject | defer]  notes:

Overall recommendation: [start v0.2 spec | promote to v0.2 with deltas | block]
```

Append completed reviews to the spec repo under
`Agent Covenant Protocol_ACP/ACR-500_Reviews/<reviewer>-<date>.md`.

---

## What ratification unblocks

Once ≥80% accepted (or rejected with documented resolution):

1. **Spec promotion.** ACR-500 v0.1 → v0.2 with the resolved deltas.
   Update `ACP_Index_v0.4.md` REVIEW-07 to reflect final state.
2. **Schema work.** New tables: `covenant_escrows`, `escrow_deposits`,
   `escrow_releases`. New columns on `covenant_members`:
   `claimant_wallet`, `gas_responsibility`. Schema PR per
   `internal/db/schema.sql`.
3. **Custody integration.** Per C-7 outcome — likely a multisig adapter
   (Safe wallet on Base testnet first) before mainnet.
4. **Tool surface.** New tools: `lock_escrow`, `release_to_claimants`,
   `refund_to_owner`, `top_up_escrow`. New HTTP routes in
   `internal/api/api.go`.
5. **Settlement engine wiring.** `internal/settlement/` gains an
   `EscrowSettler` that calls the custody adapter on
   `confirm_settlement_output`. The existing `LedgerSettler` stays —
   bookkeeping mode remains a first-class option for non-money
   covenants.
6. **Audit log.** New action types per ACR-300 for deposits, releases,
   refunds. Chain payloads must include `escrow_tx_hash`.

**Estimated implementation effort post-ratification:** 3–5 weeks of
focused engineering, single operator. Compresses to 2 weeks with two
operators (one on schema/api, one on custody adapter).

---

## What rejection of any single decision triggers

| Rejected | Cascading work |
|---|---|
| C-1 | ACR-100 §4 rewrite; ACP_Index_v0.4 REVIEW-07 reopen-and-resolve again |
| C-2 | New `pull-only` rail profile; `withdrawal_offers.escrow_id` becomes nullable |
| C-3 | Re-time deposit gate (e.g. ACTIVE rather than OPEN); UX impact for owners |
| C-4 | Over-allocation handling — circuit-breaker on `acceptance_ratio` overflow |
| C-5 | New refund accounting in `escrow_deposits`; reject_draft becomes ledger-aware |
| C-6 | Multi-asset accounting + exchange-rate snapshots in audit log |
| C-7 | Custody adapter completely different (server-custody vs full self-custody) |
| C-8 | Settlement math changes; payout previews need rework |
| C-9 | Different post-timeout state (extend, forfeit, escalate) |
| C-10 | leave_covenant gains an `unclaimed_at` field; departed agents lose silently |

The pattern: **rejecting a default doesn't stop the project — it
re-scopes a slice.** Knowing the cascade up front means the working
group can vote with eyes open.

---

## Anti-patterns we're avoiding

- **Don't ship "v0.1 defaults all the way."** That's not ratification,
  that's authorial bias. Each decision deserves an explicit yes.
- **Don't merge code that depends on an OPEN decision.** Even
  speculatively. Branches piling up against unratified decisions create
  a rebase swamp.
- **Don't try to ratify all ten in one meeting.** Three or four per
  session, recorded, signed. Decision fatigue is a real failure mode.
