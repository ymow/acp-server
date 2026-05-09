# X (Twitter) Launch Thread — Draft

> Phase 4.2 launch artefact. Replace `<...>` placeholders, post one tweet
> at a time (don't auto-thread the entire draft — pace allows replies to
> tweet 1 to land before tweet 2 publishes). Total: 8 tweets.

---

## Tweet 1 — hook (the headline)

```
Git tracks what changed.

ACP tracks who contributed, how much it was worth,
and how the reward gets distributed.

Open protocol. Self-hosted. Works with any MCP-compatible agent
(Claude, GPT, Gemini, Qwen, you).

🧵
```

## Tweet 2 — the formula

```
The whole protocol fits in one line:

  tokens = unit_count × tier_multiplier × acceptance_ratio

unit_count: how big the contribution was
tier_multiplier: how much that tier is worth (core 3×, feature 2×, …)
acceptance_ratio: 0–1 quality factor set by maintainer
```

## Tweet 3 — what it is (and isn't)

```
ACP is NOT a blockchain.
ACP is NOT a token to trade.
ACP is NOT a SaaS hosting your work.

ACP IS a protocol that records contribution claims, lets a maintainer
accept/reject them, and produces a tamper-evident settlement.

Self-hosted Go server. Single binary. SQLite. Zero external Go deps.
```

## Tweet 4 — what's shipped

```
Phases 1 → 4 are live as of 2026-05.

✓ Hash chain audit log (Phase 1, ACR-300)
✓ Full passage flow (Phase 2)
✓ Git Twin anchor — settlements signed into git notes (Phase 3.A, ACR-400)
✓ Per-hour rate limiting (Phase 4.1)
✓ At-rest encryption with versioned keyring (Phase 4.5, ACR-700)
✓ ACR-50 access gate + entry fees (Phase 4.6)
```

## Tweet 5 — proof of life

```
First real Covenant settled 2026-04-15:

  Covenant: acp-server Protocol Development
  Tokens:   4,475 ink total
  Audit:    hash chain valid ✓

This is the protocol eating its own dogfood — we used ACP to
record the contributions that built ACP.
```

## Tweet 6 — what's next (honest)

```
Phase 7.A — Escrow + Auto-Settlement — is in spec drafting.

ACR-500 v0.1 is queued for working-group ratification: 10 implementation-
blocking decisions (custody model, lock timing, gas allocation, …)
need explicit yes/no before code lands.

We don't pretend this part is shipped. It isn't.
```

## Tweet 7 — try it

```
Five-minute quickstart:

  $ git clone https://github.com/ymow/acp-server
  $ cd acp-server && go build ./...
  $ ./acp-server

Create a Covenant, add tiers, submit a passage, settle.
Connect any MCP client (Claude Code, Cursor, Codex, …).

Docs: <link to acp-frontend.vercel.app/docs>
```

## Tweet 8 — links + ask

```
What I'd love:

→ Run it once. Tell me what broke.
→ Tag a project where you wished this existed (open source, agent
   collaboration, anywhere reward distribution is awkward).
→ Read ACR-500 if you have escrow-design experience —
   we could use the eyes.

Repo: github.com/ymow/acp-server
```

---

## Posting checklist

```
[ ] Replace <link to acp-frontend.vercel.app/docs> with the live URL
[ ] Tweet 1 has zero replies → wait 90 seconds before Tweet 2
[ ] Pin Tweet 1 to your profile after thread completes
[ ] Add the X thread URL to acp-server README badge (already wired)
[ ] After 24h: log the engagement signal in observation table per
    docs/PHASE-4.2-LAUNCH.md
```

## Things to NOT do in replies

- Don't promise features in response to single asks. Log them.
- Don't argue with "this is just JIRA" — log as `objection`, read in batch.
- Don't link to your other projects unless directly asked.
- Don't reply to the thread with feature additions — those become tweets
  in their own threads, weeks later, when ratified.
