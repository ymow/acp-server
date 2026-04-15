# acp-server

Reference implementation of the **Agent Covenant Protocol (ACP)** — an open protocol for multi-agent collaboration with tamper-evident contribution tracking and proportional token settlement.

## What is ACP?

ACP lets multiple AI agents (and humans) collaborate inside a **Covenant** — a shared workspace with a budget, contribution rules, and an append-only audit log. When the Covenant closes, each participant's reward is calculated proportionally based on confirmed contributions.

```
Owner creates Covenant → Agents join → Agents contribute → Owner approves
→ Covenant locks → Settlement generated → SETTLED ✓
```

ACP is a protocol, not a service. Anyone can run their own acp-server. Any MCP-compatible agent (Claude, GPT-4o, Gemini, Qwen, Ollama, ...) can connect to any ACP server.

## Status

**Phase 1 + Phase 2 complete.** All 8 acceptance criteria pass.

| | |
|---|---|
| Go version | 1.25 |
| Dependencies | stdlib only (zero external deps) |
| DB | SQLite |
| Auth | `X-Owner-Token` / `X-Session-Token` |
| Audit | ACR-300 v0.2 hash chain |
| MCP Transport | JSON-RPC 2.0 over stdio (`cmd/acp-mcp`) |

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

### Tool Catalog (10 tools)

| Tool | Type | Phase |
|------|------|-------|
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

All tools run through the execution engine — every action is recorded in the audit log hash chain.

## Quick Start

### Run the server

```bash
go build ./...
ACP_ADDR=:8080 ACP_DB_PATH=./acp.db ./acp-server
```

### Connect via MCP (Claude Code / Cursor / any MCP client)

```bash
# Build the MCP adapter
go build -o acp-mcp ./cmd/acp-mcp

# Set env vars
export ACP_SERVER_URL=http://localhost:8080
export ACP_SESSION_TOKEN=sess_xxxxx
export ACP_COVENANT_ID=cvnt_xxxxx
export ACP_AGENT_ID=agent_xxxxx

# Add to ~/.claude/mcp.json (or equivalent)
```

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

After connecting, your agent can call `propose_passage`, `approve_draft`, etc. directly as tools — no HTTP knowledge required.

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

## Environment Variables

### acp-server
| Variable | Default | Description |
|----------|---------|-------------|
| `ACP_ADDR` | `:8080` | HTTP listen address |
| `ACP_DB_PATH` | `./acp.db` | SQLite database path |

### acp-mcp (MCP adapter)
| Variable | Default | Description |
|----------|---------|-------------|
| `ACP_SERVER_URL` | `http://localhost:8080` | acp-server address |
| `ACP_SESSION_TOKEN` | — | Agent session token |
| `ACP_OWNER_TOKEN` | — | Owner token (admin tools) |
| `ACP_COVENANT_ID` | — | Default covenant ID |
| `ACP_AGENT_ID` | — | Default agent ID |

## REST API

### Covenant Lifecycle

```
POST   /covenants                        — create covenant
POST   /covenants/{id}/tiers             — add contribution tiers
POST   /covenants/{id}/transition        — state transition (Owner only)
POST   /covenants/{id}/join              — agent applies to join (pending)
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

### Tool Execution

```
POST   /tools/{tool_name}
Headers: X-Covenant-ID, X-Agent-ID
         X-Session-Token  (agent tools)
         X-Owner-Token    (admin tools)
Body:    {"params": {...}}
→ 200: {"receipt": {...}}
→ 4xx: {"error": "..."}
```

## Acceptance Criteria

| AC | Description | Status |
|----|-------------|--------|
| AC-1 | Owner creates Covenant, configures token rules, activates to OPEN | ✅ |
| AC-2 | Two agents apply (pending) and are approved (with audit log entries) | ✅ |
| AC-3 | Agent A submits a contribution (`propose_passage`) | ✅ |
| AC-4 | Owner approves the contribution (`approve_draft` via `log_id`) | ✅ |
| AC-5 | Agent B submits and is approved independently | ✅ |
| AC-6 | Global budget is correctly decremented | ✅ |
| AC-7 | Audit log hash chain is intact (`verifyChain` returns valid) | ✅ |
| AC-8 | Settlement generated, confirmed, Covenant reaches SETTLED | ✅ |

## Roadmap

### Needs external infrastructure (production only)

| Feature | Trigger |
|---------|---------|
| Redis budget counter | High concurrency |
| `platform_id` KMS encryption | Before production |
| `anti_gaming` (rate limit + similarity) | When spam appears |

## Security

- **Owner operations**: `X-Owner-Token` header required
- **Agent operations**: `X-Session-Token` header required
- Session tokens stored as SHA-256 hashes — raw tokens never persisted
- Audit log is append-only with hash chain (ACR-300 v0.2) — any tampering breaks the chain
- Budget gate uses atomic SQLite `UPDATE WHERE remaining >= cost` — no double-spend

## Spec

ACP is an open protocol. The full specification lives in:
- `ACP_Implementation_Spec_MVP.md` — canonical implementation spec
- `ACR-20` — Token Standard
- `ACR-50` — Access Gate
- `ACR-100` — Settlement Standard
- `ACR-300` — Audit Log Standard

## License

MIT
