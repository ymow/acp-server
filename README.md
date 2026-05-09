# acp-server

> **Git tracks what changed. ACP tracks who contributed, how much it was worth, and how the reward is distributed.**

[![X (formerly Twitter) Thread](https://img.shields.io/badge/X-Thread-000000?style=flat&logo=x)](https://x.com/ACPSERVER/status/2045899600498942367)

**Agent Covenant Protocol (ACP)** — an open protocol for multi-participant collaboration between humans and AI agents, with tamper-evident contribution tracking and proportional token settlement.

This is NOT IBM's Agent Communication Protocol. ACP is a new protocol for a new problem:

> *How do multiple participants — human or AI — collaborate on a shared project, where every contribution is provably recorded, and everyone receives fair compensation automatically?*

ACP is a protocol, not a service. Anyone can run their own acp-server. Any MCP-compatible agent — Claude, GPT-4o, Gemini, Qwen, Ollama, or a human — can join any Covenant.

## What is ACP?

```
Owner creates Covenant → Participants join → Participants contribute
→ Owner approves → Covenant locks → Settlement generated → SETTLED ✓
```

---

## Status

**Phases 1 → 4 complete.** Phase 7.A (Escrow + Auto-Settlement) spec drafted (ACR-500 v0.1), implementation gated on working-group decisions. See [ACP_Roadmap.md](https://github.com/ymow/acp-server) for the full plan.

| Phase | Scope | Status |
|---|---|---|
| 1 | MVP core, 8 ACs | ✅ |
| 2 | Reject paths, queries, MCP transport | ✅ |
| 2.5 | Infra hardening (ParamsPolicy, audit rebuild, int64 minor units) | ✅ |
| 3.0 | Housekeeping (`unit_count`, `owner_id`, `budget_currency`) | ✅ |
| 3.B | Token lifecycle (rules engine, snapshots, leave_covenant) | ✅ |
| 3.A | Git Covenant Twin (ACR-400) | ✅ |
| 4.1 | `rate_limit_per_hour` (ACR-20 Part 4 Layer 2) | ✅ |
| 4.3 | `concentration_warn_pct` (ACR-20 Part 4 Layer 5) | ✅ |
| 4.5 | `platform_id` at-rest encryption + key rotation (ACR-700) | ✅ |
| 4.6 | Full ACR-50 access flow (`apply_to_covenant` + entry fees) | ✅ |
| 7.A | Escrow + Auto-Settlement (USDC on Base) | 📝 spec drafting |

**First real Covenant SETTLED: 2026-04-15**
```
Covenant: acp-server Protocol Development
Tokens:   4,475 ink total
Audit:    hash chain valid ✓
```

| | |
|---|---|
| Go version | 1.25 |
| Dependencies | stdlib only (zero external deps) |
| DB | SQLite |
| Auth | `X-Owner-Token` / `X-Session-Token` |
| Audit | ACR-300 v0.2 hash chain (`spec_version=ACR-300@2.2`, integer `cost_delta` + ISO-4217 `cost_currency`) |
| MCP Transport | JSON-RPC 2.0 over stdio (`cmd/acp-mcp`) |

---

## What is an Ink Token?

**Ink is a contribution unit, not a cryptocurrency.**

```
tokens = unit_count × tier_multiplier × acceptance_ratio
```

| Variable | Meaning |
|----------|---------|
| `unit_count` | Size of contribution (code: lines, prose: words) |
| `tier_multiplier` | Value tier (core=3x, feature=2x, review=1.5x, docs=1x) |
| `acceptance_ratio` | Quality factor set by maintainer (0.0–1.0) |

Tokens are Covenant-scoped, non-transferable, and non-tradeable. They represent contribution share — at settlement, each participant receives a proportional payout from the contributor pool.

---

## Architecture

```
Any MCP Client                    HTTP Client
(Claude Code, Cursor,             (curl, SDK, custom agent)
 GPT-4o, Gemini, Qwen, Ollama)
        ↓ JSON-RPC 2.0 / stdio          ↓ HTTP
    cmd/acp-mcp  ──────────────→  internal/api/api.go
    (MCP adapter)                       ↓
                               internal/execution/
                               (8-step engine)
                                        ↓
                        internal/audit/    internal/budget/
                        (hash chain)       (atomic gate)
                                        ↓
                               SQLite (schema.sql)
```

### Covenant State Machine

```
DRAFT → OPEN → ACTIVE → LOCKED → SETTLED
```

| State | What can happen |
|-------|----------------|
| DRAFT | Configure token rules, tiers, budget |
| OPEN | Participants apply to join |
| ACTIVE | Submit contributions, approve/reject drafts |
| LOCKED | Generate settlement output |
| SETTLED | Final state, immutable |

### Interface Catalog (17 tools + lifecycle endpoints)

| Interface | Type | Phase |
|-----------|------|-------|
| `configure_token_rules` | admin | 1 |
| `approve_agent` | admin | 1 |
| `reject_agent` | admin | 2 |
| `propose_passage` | contribution | 1 |
| `approve_draft` | admin | 1 |
| `reject_draft` | admin | 2 |
| `get_token_balance` | query | 2 |
| `list_members` | query | 2 |
| `generate_settlement_output` | settlement | 1 |
| `confirm_settlement_output` | admin | 1 |
| `leave_covenant` | admin | 3.B |
| `get_token_history` | query | 3.B |
| `configure_anti_gaming` | admin | 4.1 |
| `get_concentration_status` | query | 4.3 |
| `apply_to_covenant` | lifecycle | 4.6 |
| `approve_agent_access` | admin | 4.6 |
| `reject_agent_access` | admin | 4.6 |
| `get_agent_access_status` | query | 4.6 |

All interfaces run through the execution engine — every action is recorded in the audit log hash chain.

---

## Verification Architecture

ACP uses a blockchain-like append-only hash chain, with three trust layers you choose from based on your needs:

```
Layer 1  Hash Chain (implemented)
         SHA-256 chain on every action
         Proves: tamper-evidence
         Trust model: trust the server owner
         Verify: GET /covenants/{id}/audit/verify
         Chain survives cold cache: budget.RebuildFromAuditLog()
         reconstructs budget_spent from audit_logs ⟗ token_ledger,
         refund-aware (per ACP_Implementation_Spec_MVP Part 8).

Layer 2  Git Anchor (implemented, Phase 3.A · ACR-400 v0.2)
         ed25519-signed settlement hash written to refs/notes/acp-anchors
         Proves: public, permanent record tied to code history
         Trust model: trust git history + signing key
         Verify: GET /git-twin/pubkey + git log refs/notes/acp-anchors

Layer 3  On-chain Merkle Proof (Phase 7.D, future)
         Merkle root published on-chain
         Proves: trustless, permissionless verification
         Trust model: trustless
```

Most collaborations only need Layer 1. Open source projects use Layer 2. High-value cross-org work uses Layer 3.

---

## Quick Start

### Run the server

```bash
go build ./...
ACP_ADDR=:8080 ACP_DB_PATH=./acp.db ./acp-server
```

### Connect via MCP (Claude Code / Cursor / any MCP client)

```bash
go build -o acp-mcp ./cmd/acp-mcp

export ACP_SERVER_URL=http://localhost:8080
export ACP_SESSION_TOKEN=sess_xxxxx
export ACP_COVENANT_ID=cvnt_xxxxx
export ACP_AGENT_ID=agent_xxxxx
```

Add to `~/.claude/mcp.json`:

```json
{
  "mcpServers": {
    "acp": {
      "command": "/path/to/acp-mcp",
      "env": {
        "ACP_SERVER_URL": "http://localhost:8080",
        "ACP_SESSION_TOKEN": "${ACP_SESSION_TOKEN}",
        "ACP_COVENANT_ID": "${ACP_COVENANT_ID}",
        "ACP_AGENT_ID": "${ACP_AGENT_ID}"
      }
    }
  }
}
```

### Connect via other frameworks

**OpenAI Agents SDK**
```python
from agents import Agent, MCPServerStdio
acp = MCPServerStdio(command="./acp-mcp", env={...})
agent = Agent(name="contributor", model="o3", mcp_servers=[acp])
```

**Google ADK (Gemini)**
```python
from google.adk.tools.mcp_tool import MCPToolset, StdioServerParameters
acp_tools = MCPToolset(
    connection_params=StdioServerParameters(command="./acp-mcp", env={...})
)
```

**LangChain (Ollama / Qwen)**
```python
from langchain_mcp_adapters.client import MultiServerMCPClient
async with MultiServerMCPClient({"acp": {"command": "./acp-mcp", "transport": "stdio"}}) as client:
    tools = client.get_tools()
    agent = create_react_agent(ChatOllama(model="qwen3"), tools)
```

---

## Environment Variables

### acp-server
| Variable | Default | Description |
|----------|---------|-------------|
| `ACP_ADDR` | `:8080` | HTTP listen address |
| `ACP_DB_PATH` | `./acp.db` | SQLite database path |

### acp-mcp
| Variable | Default | Description |
|----------|---------|-------------|
| `ACP_SERVER_URL` | `http://localhost:8080` | acp-server address |
| `ACP_SESSION_TOKEN` | — | Participant session token |
| `ACP_OWNER_TOKEN` | — | Owner token (admin interfaces) |
| `ACP_COVENANT_ID` | — | Default covenant ID |
| `ACP_AGENT_ID` | — | Default agent ID |

---

## REST API

### Covenant Lifecycle

```
POST   /covenants                        — create covenant
POST   /covenants/{id}/tiers             — add contribution tiers
POST   /covenants/{id}/transition        — state transition (Owner only)
POST   /covenants/{id}/join              — participant applies to join
GET    /covenants/{id}                   — get covenant details + members
GET    /covenants/{id}/state             — get current state
POST   /covenants/{id}/budget            — set global budget
GET    /covenants/{id}/budget            — get budget status
GET    /covenants/{id}/audit             — get audit log
GET    /covenants/{id}/audit/verify      — verify hash chain integrity
```

### Session Tokens

```
POST   /sessions/issue                   — issue session token (Owner only)
POST   /sessions/rotate                  — rotate session token
```

### Interface Execution

```
POST   /tools/{interface_name}
Headers: X-Covenant-ID, X-Agent-ID
         X-Session-Token  (participant interfaces)
         X-Owner-Token    (admin interfaces)
Body:    {"params": {...}}
→ 200: {"receipt": {...}}
→ 4xx: {"error": "..."}
```

---

## Phase 1 Acceptance Criteria

| AC | Description | Status |
|----|-------------|--------|
| AC-1 | Owner creates Covenant, configures token rules, activates to OPEN | ✅ |
| AC-2 | Two participants apply (pending) and are approved (with audit log) | ✅ |
| AC-3 | Participant A submits a contribution (`propose_passage`) | ✅ |
| AC-4 | Owner approves the contribution (`approve_draft` via `log_id`) | ✅ |
| AC-5 | Participant B submits and is approved independently | ✅ |
| AC-6 | Global budget is correctly decremented | ✅ |
| AC-7 | Audit log hash chain is intact (`verifyChain` returns valid) | ✅ |
| AC-8 | Settlement generated, confirmed, Covenant reaches SETTLED | ✅ |

Phase 2–4 acceptance is verified by `acp_test.go`, `scenario_test.go`, `integration_test.go`, and the per-phase test files under `internal/*/`.

---

## Roadmap

See [ACP_Roadmap.md](https://github.com/ymow/acp-server) for the full Phase 0–7 plan (v0.4.3, last updated 2026-05-06).

For an operator-facing snapshot of what's actually queued, see:
- [`docs/PHASE-NEXT.md`](docs/PHASE-NEXT.md) — one-page status surface
- [`docs/PHASE-7A-DECISIONS.md`](docs/PHASE-7A-DECISIONS.md) — the 10 ACR-500 decisions queued for working-group ratification
- [`docs/PHASE-4.2-LAUNCH.md`](docs/PHASE-4.2-LAUNCH.md) — public-launch + observation-week checklist

**Next: Phase 7.A — Escrow + Auto-Settlement** (規格中 / spec drafting)

Escrow-first ordering (per Roadmap v0.4.3): Phase 7.A is now ahead of Phase 5 (cross-Covenant reputation). Reputation is a *product* of completed real transactions, not a precondition — Escrow is the minimal precondition for two strangers to safely complete their first transaction.

What's drafted:
- **ACR-500 Escrow Standard v0.1** — lock / release / refund semantics above ACR-100 §4 withdrawal
- **10 open decisions** (custody model, lock timing, gas allocation, etc.) blocking ratification — see [`docs/PHASE-7A-DECISIONS.md`](docs/PHASE-7A-DECISIONS.md)

What's deferred:
- Phase 4.2 public release + inbound observation (1-week milestone, not code) — checklist at [`docs/PHASE-4.2-LAUNCH.md`](docs/PHASE-4.2-LAUNCH.md)
- Phase 5 (cross-Covenant reputation) — gated on Phase 7.A real-transaction data
- Phase 6 (Genesis migration) — gated on a mature OSS project asking to onboard
- KMS / Vault adapters, Redis budget counter, similarity_threshold — gated on demand

---

## Security

- **Owner operations**: `X-Owner-Token` header required
- **Participant operations**: `X-Session-Token` header required
- Session tokens stored as SHA-256 hashes — raw tokens never persisted
- Audit log is append-only with hash chain (ACR-300@2.2) — any tampering breaks the chain. `cost_currency` is part of the hash payload, so USD/EUR/etc. charges cannot collide at the same numeric cost
- Budget gate uses atomic SQLite `UPDATE WHERE remaining >= cost` — no double-spend
- Budget counter can be rebuilt from the audit log (`budget.RebuildFromAuditLog`) — runtime cache drift is recoverable from durable storage
- **ParamsPolicy** (ACP Spec v0.2 Part 6): each interface declares
  `PreviewFields` / `SensitiveFields` / `HashPreviewFields` / `StoreHashOnly`.
  Raw user content (e.g. draft prose) never lands in `audit_logs.params_preview`;
  only whitelisted bookkeeping fields do. Mask lengths are rune-aware.
- **At-rest encryption** (ACR-700): `platform_id` is sealed with AES-256-GCM
  under a versioned keyring. The reference build keeps keys on local disk
  (`LocalKeyfileProvider`); pluggable `KeyProvider` interface lets operators
  point at AWS KMS / HashiCorp Vault / GCP KMS / their HSM of choice. See
  [`docs/key-provider.md`](docs/key-provider.md) for the contract and an
  adapter skeleton.

---

## Spec

ACP is an open protocol. The full specification lives in the [inkmesh/acp-spec](https://github.com/ymow/acp-server) repository:

- `ACP_Implementation_Spec_MVP.md` — canonical implementation spec
- `ACR-20` v0.2 — Token Standard (Ink)
- `ACR-50` v0.1 — Access Gate
- `ACR-60` v0.1 — Budget Gate
- `ACR-100` v0.3 — Settlement Standard (x402 Pull withdrawal)
- `ACR-300` v0.2 — Audit Log Standard (hash chain @2.2)
- `ACR-400` v0.2 — Git Covenant Twin *(implemented, Phase 3.A)*
- `ACR-500` v0.1 — Covenant Escrow Standard *(draft, Phase 7.A blocker)*
- `ACR-700` v0.1 — Key Management & At-Rest Encryption *(implemented, Phase 4.5)*

---

## License

MIT
