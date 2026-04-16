# acp-server

> **Git tracks what changed. ACP tracks who contributed, how much it was worth, and how the reward is distributed.**

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

**Phase 1 + Phase 2 complete.** All 8 acceptance criteria pass.

**First real Covenant SETTLED: 2026-04-15**
```
Covenant: acp-server Protocol Development
Tokens:   4,475 ink total
Audit:    hash chain valid ✓
Anchor:   settlements/2026-04-15-acp-server-phase1-2.json
```

| | |
|---|---|
| Go version | 1.25 |
| Dependencies | stdlib only (zero external deps) |
| DB | SQLite |
| Auth | `X-Owner-Token` / `X-Session-Token` |
| Audit | ACR-300 v0.2 hash chain |
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

### Interface Catalog (10 interfaces)

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

Layer 2  Git Anchor (Phase 3)
         Settlement hash committed to the repo
         Proves: public, permanent record tied to code history
         Trust model: trust git history
         See: settlements/*.json

Layer 3  On-chain Merkle Proof (Phase 7)
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

## Acceptance Criteria

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

---

## Roadmap

See [ACP_Roadmap.md](https://github.com/ymow/acp-server) for the full Phase 0–7 plan.

**Next: Phase 3**
- ACR-400 Git Covenant Twin — git push/merge auto-triggers ACP interfaces
- Layer 2 Git Anchor — settlement hash in repo as permanent proof
- `unit_count` replacing `word_count` (space_type-aware)
- Constitutional Principles — formalized participant rights

---

## Security

- **Owner operations**: `X-Owner-Token` header required
- **Participant operations**: `X-Session-Token` header required
- Session tokens stored as SHA-256 hashes — raw tokens never persisted
- Audit log is append-only with hash chain (ACR-300 v0.2) — any tampering breaks the chain
- Budget gate uses atomic SQLite `UPDATE WHERE remaining >= cost` — no double-spend

---

## Spec

ACP is an open protocol. The full specification lives in the [inkmesh/acp-spec](https://github.com/ymow/acp-server) repository:

- `ACP_Implementation_Spec_MVP.md` — canonical implementation spec
- `ACR-20` — Token Standard (Ink)
- `ACR-50` — Access Gate
- `ACR-60` — Budget Gate
- `ACR-100` — Settlement Standard
- `ACR-300` — Audit Log Standard
- `ACR-400` — Git Covenant Twin *(Phase 3, draft)*

---

## License

MIT
