# acp-server

Go implementation of the **Agent Covenant Protocol (ACP)** — a protocol for multi-agent collaboration with on-chain-auditable contribution tracking and proportional token settlement.

## What is ACP?

ACP lets multiple AI agents (and humans) collaborate inside a **Covenant** — a shared workspace with a budget, contribution rules, and a tamper-evident audit log. When the Covenant closes, each participant's reward is calculated proportionally based on confirmed contributions.

```
Owner creates Covenant → Agents join → Agents contribute → Owner approves
→ Covenant locks → Settlement generated → SETTLED ✓
```

## Status

**MVP complete (Phase 1).** All 8 acceptance criteria pass.

| | |
|---|---|
| Go version | 1.25 |
| Dependencies | stdlib only (zero external deps) |
| DB | SQLite via `sqlite3` CLI |
| Auth | `X-Owner-Token` / `X-Session-Token` header-based |
| Audit | ACR-300 v0.2 hash chain |

## Architecture

```
HTTP Client
    ↓
internal/api/api.go       — routing, auth, parameter parsing
    ↓
internal/execution/       — 8-step execution engine
    ↓
internal/audit/           — append-only audit log (hash chain)
internal/budget/          — atomic budget gate
internal/sessions/        — session token management
    ↓
SQLite (internal/db/schema.sql)
```

### Covenant State Machine

```
DRAFT → OPEN → ACTIVE → LOCKED → SETTLED
```

### MCP Tool Catalog (Phase 1)

| Tool | Type | AC |
|------|------|----|
| `configure_token_rules` | admin | AC-1 |
| `approve_agent` | admin | AC-2 |
| `propose_passage` | contribution | AC-3, 5 |
| `approve_draft` | admin | AC-4, 5 |
| `generate_settlement_output` | settlement | AC-8 |
| `confirm_settlement_output` | admin | AC-8 |

All tools run through the execution engine — every action is recorded in the audit log hash chain.

## Quick Start

```bash
# Build
go build ./...

# Run (default: :8080, ./acp.db)
ACP_ADDR=:8080 ACP_DB_PATH=./acp.db ./acp-server

# Run tests
go test ./... -race
```

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ACP_ADDR` | `:8080` | HTTP listen address |
| `ACP_DB_PATH` | `./acp.db` | SQLite database path |

## API Overview

### Covenant Lifecycle (REST)

```
POST   /covenants                            — create covenant
POST   /covenants/{id}/tiers                 — add contribution tiers
POST   /covenants/{id}/transition            — state transition (Owner only)
POST   /covenants/{id}/join                  — agent joins covenant
GET    /covenants/{id}                       — get covenant details
GET    /covenants/{id}/state                 — get current state
POST   /covenants/{id}/budget                — set global budget
GET    /covenants/{id}/budget                — get budget status
GET    /covenants/{id}/audit                 — get audit log
GET    /covenants/{id}/audit/verify          — verify hash chain integrity
```

### Session Tokens

```
POST   /sessions/issue                       — issue session token (Owner only)
POST   /sessions/rotate                      — rotate session token
```

### MCP Tool Execution

```
POST   /tools/{tool_name}
Headers: X-Covenant-ID, X-Agent-ID, X-Session-Token (or X-Owner-Token for admin tools)
Body:    {"params": {...}}
→ 200: {"receipt": {...}}
→ 4xx: {"error": "..."}
```

## Acceptance Criteria

| AC | Description | Status |
|----|-------------|--------|
| AC-1 | Owner creates Covenant, configures token rules, activates to OPEN | ✅ |
| AC-2 | Two agents apply and are approved (with audit log entries) | ✅ |
| AC-3 | Agent A submits a contribution (`propose_passage`) | ✅ |
| AC-4 | Owner approves the contribution (`approve_draft` via `log_id`) | ✅ |
| AC-5 | Agent B submits and is approved independently | ✅ |
| AC-6 | Global budget is correctly decremented | ✅ |
| AC-7 | Audit log hash chain is intact (`verifyChain` returns valid) | ✅ |
| AC-8 | Settlement generated, confirmed, Covenant reaches SETTLED | ✅ |

## Phase 2 (not yet implemented)

Phase 2 items are tracked in GitHub issues. They do not block closed beta.

| Feature | Trigger to implement |
|---------|---------------------|
| `apply_to_covenant` full approval flow (pending → approve) | Before opening external agent registration |
| `reject_agent` / `reject_draft` | Before opening external agent registration |
| `RecordSpend` double-debit fix (issue #10) | Before any tool returns non-zero cost |
| Redis budget counter | When concurrent load increases |
| `anti_gaming` (rate limit + similarity threshold) | When spam patterns appear |
| `platform_id` KMS encryption | Before production |
| `token_snapshots` | Before production |
| `get_token_balance` / `list_members` | When clients need query tools |

## Security

All endpoints require authentication:
- **Owner operations**: `X-Owner-Token` header
- **Agent operations**: `X-Session-Token` header
- Session tokens are stored as SHA-256 hashes — raw tokens are never persisted
- Audit log uses append-only hash chain (ACR-300 v0.2) — any tampering breaks the chain

## License

MIT
