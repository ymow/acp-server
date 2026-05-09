# PHASE-NEXT — What's Actually Queued

> Operator-facing snapshot of where the implementation is, what's *next*,
> and what's blocked. Pulls from the canonical living roadmap at
> [`inkmesh/Agent Covenant Protocol_ACP/ACP_Roadmap.md`](https://github.com/ymow/acp-server)
> (v0.4.3, last spec update 2026-05-06) — does not fork it.
>
> Updated: 2026-05-09. If this page disagrees with the roadmap doc, the
> roadmap doc wins; this page is a surface for operators who don't want
> to read 605 lines.

---

## TL;DR

| Phase | Status | Action |
|---|---|---|
| 1, 2, 2.5, 3.0, 3.B, 3.A | ✅ shipped | none |
| 4.1, 4.3, 4.5, 4.6 | ✅ shipped | none |
| **4.2 — Public launch + observation** | 🟡 ready, 1-week milestone | **Do** — see [PHASE-4.2-LAUNCH.md](PHASE-4.2-LAUNCH.md) |
| **7.A — Escrow + Auto-Settlement** | 🟠 spec drafting (ACR-500 v0.1) | **Decide** 10 working-group questions — see [PHASE-7A-DECISIONS.md](PHASE-7A-DECISIONS.md) |
| 7.B / 7.C / 7.D | 🔒 gated on 7.A landing + first real escrow tx | wait |
| 5 — Cross-Covenant reputation | 🔒 gated on 7.A real-transaction data | wait — by design |
| 6 — Genesis migration | 🔒 gated on first mature OSS adopter requesting onboarding | wait |
| Deferred items (KMS adapters, Redis budget, similarity_threshold) | 🔒 gated on demand | wait |

---

## What "next" means in practice

Two tracks run in parallel right now. They're independent — neither blocks
the other.

### Track A — Phase 4.2 launch (operator-driven)

Phase 4.2 is **not code**. It's a one-week observation window: open the
server up, watch who shows up, see what the first inbound issues look
like, decide whether to pivot scope based on actual demand vs.
hypothetical demand.

The roadmap defines this as:

```
1. 誰部署了？        → GitHub star profile 分類：個人 / 組織
2. 第一個 issue 是什麼？ → 部署 / feature / 合規
3. 三週後決策分流：
   - 多數個人 OSS → 保持本 roadmap 路線
   - 多數企業    → KMS adapter 從延後項提前到 Phase 4
   - 沒 inbound  → 問題在 distribution，roadmap 不動、focus 切 outreach
```

Concrete steps: see [PHASE-4.2-LAUNCH.md](PHASE-4.2-LAUNCH.md).

### Track B — Phase 7.A escrow (working-group-driven)

Phase 7.A is the protocol's biggest jump: from "tracks contribution" to
"holds and releases real money." That requires ten implementation-level
decisions before any code lands. They sit in the spec repo as
`ACR-500_Decisions_v0.1.md`, surfaced for review in
[PHASE-7A-DECISIONS.md](PHASE-7A-DECISIONS.md).

**Ratifying ≥80% of these decisions unblocks coding.** Ratifying <50%
means we're still in design space; don't start an implementation that
will need to be reworked.

---

## What's NOT next (deliberately)

The roadmap is clear about milestones that wait on real signal:

- **Phase 5 (cross-Covenant reputation)** waits on Phase 7.A real
  transaction data. Reputation built without real money behind it is
  noise — the roadmap calls this out explicitly. Skip ahead at your peril.
- **Phase 6 (Genesis migration)** waits on a *specific* trigger: a mature
  open-source project (OpenClaw is the named candidate) asking to
  on-board their git history. Building this without an adopter ready is
  a museum piece.
- **KMS / Vault adapters** wait on a paying enterprise asking. The
  `KeyProvider` interface is already in place (see
  [`docs/key-provider.md`](key-provider.md)) — the contract is stable;
  what's missing is concrete adapters, and we won't build those on
  speculation.
- **Redis budget counter** waits on observed contention in the SQLite
  atomic UPDATE path. Today there is none.
- **`similarity_threshold` (ACR-20 Part 4 Layer 3)** waits on (a)
  embedding provider decision and (b) duplicate-content emerging as a
  measured problem.

---

## Working-agreement on this doc

- **Update on shipped phases.** When a phase moves from `🟡 ready` /
  `🟠 spec drafting` to `✅ shipped`, mark it here and link to the
  README's status table.
- **Don't add new phases here.** New phases are added to the canonical
  roadmap doc first; this page reflects them after.
- **Don't restate the roadmap's reasoning.** Link, don't quote.
- **One page rule.** If this gets longer than this version, it's drifting
  into roadmap-mirror territory — refactor.
