# HN "Show HN" Post — Draft

> Phase 4.2 launch artefact. HN audience reads sober prose, distrusts
> marketing voice. Lead with substance, not headlines. Single submission;
> don't post X thread + HN same day.

---

## Title (60-character target)

```
Show HN: ACP — Open protocol for tracking AI agent contribution shares
```

(Backups if HN ranking is harder than expected:)
- `Show HN: An open protocol for who-contributed-what in agent collaboration`
- `Show HN: Tamper-evident contribution tracking for human + AI projects`

## Post body

```
Git tracks what changed; it doesn't track who contributed, how much it
was worth, or how rewards are distributed when something settles.
That's the gap ACP (Agent Covenant Protocol) tries to fill.

ACP is a protocol, not a service. You self-host a Go binary
(acp-server), open a Covenant, and any MCP-compatible agent — Claude,
GPT-4o, Cursor, Gemini, a human running curl — can join, submit
"passages" (claimed contributions), and have them approved or rejected
by the owner. Each accepted passage produces ink tokens via:

  tokens = unit_count × tier_multiplier × acceptance_ratio

Ink is non-transferable, Covenant-scoped, not a cryptocurrency. It's a
distribution key: when revenue exists, the owner uses ink percentages
to decide how to split it. Any currency, any amount, any time.

Where it's at:

  ✓ Phase 1 — append-only SHA-256 hash chain (ACR-300)
  ✓ Phase 2 — full passage / approval / settlement flow
  ✓ Phase 3.A — Git Twin: settlement hash signed (ed25519) into
    git notes (ACR-400). Anyone with the repo can verify the
    settlement existed at that point in time, without trusting
    the server.
  ✓ Phase 4.1 — per-hour rate limiting
  ✓ Phase 4.5 — at-rest encryption (AES-256-GCM) with versioned
    keyring + key rotation (ACR-700). Pluggable KeyProvider
    interface for KMS / Vault adapters when someone needs them.
  ✓ Phase 4.6 — ACR-50 access gate. Applicants apply, owners
    approve, optional entry-fee ledger.

What's NOT shipped (and we're explicit about this):

  → Phase 7.A — Escrow + auto-settlement (ACR-500 v0.1 is in spec
    drafting; ten implementation-blocking working-group decisions
    are queued). Until 7.A ships, settlement is off-chain
    bookkeeping — the protocol enforces the record, not the
    payment.
  → Phase 5 — cross-Covenant reputation. Gated on Phase 7.A real-
    transaction data, by design. Reputation built on synthetic
    Covenants is noise.
  → Phase 6 — Genesis migration (importing existing git history
    into ACP). Gated on a specific OSS project asking to onboard.

Stack:

  - Go 1.25, stdlib only (zero external Go deps)
  - SQLite (atomic UPDATE for the budget gate, no Redis)
  - JSON-RPC 2.0 over stdio for the MCP transport
  - MIT licensed

The first real Covenant settled on 2026-04-15. We used ACP to record
the contributions that built ACP. 4,475 ink total, hash chain valid.

Repo: https://github.com/ymow/acp-server
Docs: <acp-frontend-vercel-url>/docs
ACR specs: https://github.com/ymow/acp-server (linked from README)

Honest about what this is for: small-to-medium open-source teams,
research collabs, multi-agent setups where a maintainer wants a
verifiable record of who shipped what — and a way to translate that
record into payouts when revenue arrives. Not for tradeable tokens.
Not for trustless smart-contract enforcement (that's Phase 7.A and
won't lie to you about its status).

Curious about pushback. Especially: where does this overlap with
something you already use? What would make you NOT adopt it?
```

## Things to expect in comments and prepared responses

(Don't argue. Log to observation table. These are the patterns to
recognise so you don't waste a Saturday relitigating each.)

| Comment shape | Don't argue. Log as. |
|---|---|
| "this is just CLA / contributor agreement with extra steps" | `objection` — read in batch |
| "looks like Project Zomboid's contribution tracker / GitHub Sponsors" | `objection` — note adjacency, don't position |
| "what about Solana / Ethereum / [specific chain]?" | `feature` (if specific use case stated) or `objection` (if "but blockchain") |
| "GDPR / data sovereignty / KYC?" | `compliance` — promote KMS adapter discussion if pattern repeats |
| "why not federated git?" | `objection` — Git Twin already does this for the settlement hash |
| "this is a token!" | `objection` — clarify Ink is non-transferable, not tradeable |
| "tried it, here's what broke" | `feature` or `deploy` — gold; reply same day |
| "would you support [my team's protocol]?" | `feature` — log; reply only if pattern repeats |

## Posting mechanics

```
[ ] Post weekday morning EU/US (HN traffic peak ~9am EDT)
[ ] Don't post on Friday
[ ] Have a stable internet connection for first 2h (you'll triage replies)
[ ] Pre-pin the issue template to the repo so newcomers see it
[ ] Don't reply with feature roadmaps in HN comments — link to PHASE-NEXT.md
[ ] Don't sock-puppet upvote — HN detects this trivially and shadow-banishes
```
