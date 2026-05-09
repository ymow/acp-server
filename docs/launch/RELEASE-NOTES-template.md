# GitHub Release Notes — Template

> Copy this for each release. Replace bracketed placeholders. Tag format
> is `acp-server-vX.Y.Z` to match `docs/PHASE-4.2-LAUNCH.md`. The
> "Status as of this release" table mirrors README — keep them in sync.

---

## Tag

```
acp-server-v0.4.6
```

## Release title

```
ACP v0.4.6 — Phase 4.6 ACR-50 Access Gate
```

(Pattern: `ACP <version> — <one-line headline of the most-significant
slice>`. Don't list every commit in the title.)

## Release notes body

```markdown
# ACP v0.4.6

> Git tracks what changed. ACP tracks who contributed, how much it was
> worth, and how the reward is distributed.

This release closes Phase 4.6 — the ACR-50 access gate — making ACP
ready for Phase 4.2 (public observation week, see
[`docs/PHASE-4.2-LAUNCH.md`](docs/PHASE-4.2-LAUNCH.md)).

## Status as of this release

| Phase | Scope | Status |
|---|---|---|
| 1 | MVP core | ✅ |
| 2 | Reject paths, queries, MCP transport | ✅ |
| 2.5 | Infra hardening | ✅ |
| 3.0 | Housekeeping | ✅ |
| 3.A | Git Covenant Twin (ACR-400) | ✅ |
| 3.B | Token lifecycle | ✅ |
| 4.1 | Rate limiting | ✅ |
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

### Phase 4.5.x — Encryption + key rotation maintenance

- `acp-server rotate-key` is O(1) — generates a new keyring version
  without touching encrypted rows.
- `acp-server reencrypt` is O(rows), idempotent — safely re-runs and
  re-encrypts everything under the latest key.
- `acp-doctor` includes `platform_id` residual scanner — verifies no
  plaintext platform_ids leaked into logs / params previews.

### Other

- [List specific commits / PRs since last release here]

## Breaking changes

None in this release. The legacy `/join` + `approve_agent` path still
works alongside the new ACR-50 flow for backwards compatibility.

If you previously deployed acp-server before v0.4.5 and stored
encrypted platform_ids, run `acp-server reencrypt` once after upgrade
to migrate them to the latest key version.

## What's NOT shipped (deliberately)

- **Phase 7.A — Escrow + Auto-Settlement.** ACR-500 v0.1 is drafted;
  10 working-group decisions are queued for ratification. No code in
  this release. Tracking: [`docs/PHASE-7A-DECISIONS.md`](docs/PHASE-7A-DECISIONS.md).
- **Phase 5 — cross-Covenant reputation.** Gated on Phase 7.A
  real-transaction data. By design.
- **Phase 6 — Genesis migration.** Gated on a specific OSS project
  asking to onboard. By design.

## Verification

This release was tested with:

- `go test ./...` — full suite green
- `acp_test.go` + `scenario_test.go` + `integration_test.go` for
  acceptance criteria
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
the README.

## Feedback (Phase 4.2 observation week)

This release initiates a 3-week observation window. Open issues with
the **Observation** template — your feedback shapes whether KMS
adapters get promoted from deferred, whether we go to Phase 7.A
ratification next, or whether distribution is the actual problem.

## Full changelog

[Generate via: `git log $LAST_TAG..HEAD --pretty=format:"- %s (%h)"`]

## Co-authors

Contributors at this Covenant settlement:
- [pull from `settlements/<latest>.json`]

---

**Tag:** `acp-server-v0.4.6`
**Date:** [YYYY-MM-DD]
**License:** MIT
```

## Where to publish

```
[ ] gh release create acp-server-v0.4.6 \
      --title "ACP v0.4.6 — Phase 4.6 ACR-50 Access Gate" \
      --notes-file docs/launch/RELEASE-NOTES-v0.4.6.md \
      --discussion-category "Releases" \
      --latest

[ ] After publishing: copy the GitHub release URL into the X thread
    Tweet 1 (replace the bare github.com link with the release URL
    so the thread points at the specific tagged version)

[ ] After publishing: link the release in HN-POST.md "Repo:" line.
```

## Anti-patterns

- **Don't list 50 commits in the body.** Use the changelog section as a
  link, not a body dump. Lead with phase progress, end with the link.
- **Don't fabricate "what's coming next" timelines.** "Phase 7.A spec
  drafting" is what the README says; the release notes inherit, not
  invent.
- **Don't tag every release as `latest`.** If you ship a hotfix on top
  of v0.4.6 that's only relevant to one issue, mark it pre-release until
  it's the recommended download.
