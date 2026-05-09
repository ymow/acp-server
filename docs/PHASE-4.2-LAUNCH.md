# PHASE-4.2-LAUNCH — Public Launch + Observation Week

> Phase 4.2 is **not code**. It's a one-week observation window: open the
> server up to the public, watch who shows up and what their first issue
> is, then make a data-driven call about whether to pivot scope or stay
> on roadmap.
>
> This page is the checklist so when launch day comes, there's no
> bikeshed.

---

## Trigger condition

Phase 4.2 starts when:

- Phases 1, 2, 2.5, 3.0, 3.B, 3.A, 4.1, 4.3, 4.5, 4.6 are all ✅ shipped
  ✓ (true as of 2026-05-09)
- A signed-anchor settlement has happened on a real Covenant ✓ (true:
  `cvnt_a54e1c43`, 2026-04-15, 4,475 ink)
- The frontend docs surface matches server state ✓ (true post-2026-05-09)
- You have ≥1 hour each weekday for the observation week to read inbound
  signal

If any of those is false, **don't launch yet** — you'll waste the
inbound signal because you can't react to it.

---

## Pre-launch checklist (T-1 to T-0)

```
[ ] All status callouts on acp-frontend match acp-server README's status table
[ ] /docs pages reflect Phase 4.5 + 4.6 surface (verify QuickstartPage uses ACR-50)
[ ] First real settlement is referenced on landing as proof point
[ ] GitHub repo:
    [ ] README ✅ shows Phase 1 → 4 complete
    [ ] LICENSE = MIT ✓
    [ ] Issues template includes the three observation categories (deploy / feature / compliance)
    [ ] Discussions enabled (or accept-issues-only call decided)
    [ ] CODEOWNERS or maintainer-handle is reachable
[ ] Demo Covenant is reproducible from QuickstartPage in <5 min on a clean machine (test once)
[ ] `acp-doctor` runs clean against the demo Covenant DB
[ ] Vercel deployment of acp-frontend is green and crawlable (curl the prerendered HTML)
[ ] Outreach payload prepared:
    [ ] X/Twitter thread drafted (badge already in README points to one)
    [ ] HN "Show HN" post drafted (don't post both same day)
    [ ] AI-agent dev community channels identified (3 max)
[ ] Internal observation rubric written down (next section)
```

---

## Observation rubric (the entire point of Phase 4.2)

The roadmap (`ACP_Roadmap.md` §4.2) defines the question:

```
1. 誰部署了？        → GitHub star profile 分類：個人 / 組織
2. 第一個 issue 是什麼？ → 部署 / feature / 合規
3. 三週後決策分流：
   - 多數個人 OSS → 保持本 roadmap 路線
   - 多數企業    → KMS adapter 從延後項提前到 Phase 4
   - 沒 inbound  → 問題在 distribution，roadmap 不動、focus 切 outreach
```

In practice, log every inbound signal in a flat table:

| Date | Channel | Identifier | Profile | First action | Category |
|---|---|---|---|---|---|
| 2026-05-?? | star | @user | individual / org / unknown | starred | — |
| 2026-05-?? | issue | #N | individual / org | "deploy on Railway?" | **deploy** |
| 2026-05-?? | issue | #N | org | "KMS adapter for Vault" | **feature** (paid signal) |
| 2026-05-?? | DM | source | — | "is x402 supported?" | **feature** |
| 2026-05-?? | issue | #N | individual | "GDPR compliance?" | **compliance** |

Three weeks in, the histogram of `Category × Profile` decides:

- **Heavy "individual + deploy"** — keep the roadmap; emphasise
  documentation and onboarding polish.
- **Heavy "org + feature" with KMS / Vault asks** — promote KMS adapter
  out of the deferred list and into Phase 4.7 explicitly.
- **Heavy "compliance" from anyone** — surface a public security/compliance
  posture (SOC2 stance, GDPR posture, audit-log access controls). Don't
  build features on speculation, but don't be silent.
- **No inbound** — distribution problem, not protocol problem. Spend the
  next two weeks on outreach (talks, blog posts, demo videos), then
  re-evaluate.

---

## Things to NOT do during observation

- **Don't add features in response to single requests.** One person
  asking for x402 is noise; three independent asks for x402 in two
  weeks is signal.
- **Don't open new Phase 7.A code branches.** Phase 7.A is gated on
  ACR-500 working-group ratification (see PHASE-7A-DECISIONS.md), not
  inbound demand. Conflating them produces an architecture you'll regret.
- **Don't argue with negative comments publicly.** "ACP duplicates Git"
  / "this is just JIRA" / etc. are surface objections — log them in the
  observation table under category `objection` and read in batch at end
  of week. The pattern matters; individual instances don't.
- **Don't extend the observation window past 3 weeks.** Decision fatigue
  + late-arrivals dilute signal. If you don't have enough at week 3,
  the answer isn't "wait" — it's "go back to outreach."

---

## Launch-day mechanics

```
T-0 morning:
  [ ] Tag the release: git tag acp-server-v0.4.6
  [ ] Push tag, create GitHub release with concise notes
  [ ] Verify /health endpoint responds 200 from a fresh machine
  [ ] Open issues/discussions
  [ ] Bring up acp-frontend.vercel.app in incognito and re-verify nothing
      is broken from cold cache

T-0 noon (one channel only — pick X or HN, not both):
  [ ] Post outreach thread
  [ ] Subscribe yourself to repo notifications (issues + discussions + watching)

T-0 evening:
  [ ] Read every inbound message even if you don't reply yet
  [ ] Log to observation table

T+1 onward (daily):
  [ ] 30-min triage: reply to inbound, log category, **resist the urge to
      promise features**
  [ ] Add to observation table

T+7:
  [ ] Mid-window check: are categories distributing? If 100% deploy
      questions, you may want to pre-write deployment guides
      (Railway / Fly / Zeabur) to compress future load.

T+21:
  [ ] Histogram day. Decide: stay roadmap, promote KMS, or pivot to
      outreach.
  [ ] Mark Phase 4.2 ✅ in README + PHASE-NEXT.md.
  [ ] Write a short retro: "what we learned in 3 weeks" — 500 words
      max, append to BACKLOG / decisions log.
```

---

## What ships out of Phase 4.2

Nothing in code. The output of Phase 4.2 is a **decision document**
saying which of these three branches the project is on:

1. *Stay roadmap* (individual OSS dominant) — Phase 7.A becomes the
   focus once decisions ratify.
2. *Promote KMS adapter* (org/enterprise dominant) — open a Phase 4.7
   specifically for KMS / Vault adapters; Phase 7.A still ratifies in
   parallel.
3. *Distribution rework* (no inbound) — pause feature work; produce
   2–3 outreach artefacts (long-form blog, demo video, conference talk
   submission). Re-attempt launch in 4 weeks.

That decision goes back into the canonical roadmap doc as a v0.4.4
update.

---

## When NOT to start Phase 4.2

- If **Phase 7.A decisions are simultaneously up for ratification this
  week.** Don't split attention. Ratify Phase 7.A first (1–2 weeks),
  then launch into a project that has clear next-phase momentum.
- If you're personally not available for daily 30-min triage. Empty
  observation = wasted launch.
- If the frontend deploy is broken or stale (run the pre-launch
  checklist above, no shortcuts).
