-- ACP Server MVP Schema
-- ACR-300 v0.2 + ACR-20 + ACR-60 + ACR-100 + REVIEW-14

PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

-- ── Covenants ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS covenants (
    covenant_id     TEXT PRIMARY KEY,         -- cvnt_{uuid}
    version         TEXT NOT NULL DEFAULT 'ACP@1.0',
    space_type      TEXT NOT NULL DEFAULT 'book',
    title           TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'DRAFT',
    owner_id        TEXT NOT NULL DEFAULT '', -- covenant_members.agent_id of the owner role (explicit, not derived)
    owner_share_pct REAL NOT NULL DEFAULT 30.0,
    platform_share_pct REAL NOT NULL DEFAULT 0.0,
    contributor_pool_pct REAL NOT NULL DEFAULT 70.0,
    budget_limit    INTEGER NOT NULL DEFAULT 0,  -- minor units of budget_currency; 0 = unlimited
    budget_currency TEXT NOT NULL DEFAULT 'USD', -- ISO 4217; all cost_delta on this covenant must match
    cost_weight     REAL NOT NULL DEFAULT 1.0, -- ACR-20 §6: net_delta = tokens_delta - cost_weight × cost_delta
    owner_token     TEXT NOT NULL DEFAULT '', -- A-2: bearer token for owner-only operations
    token_rules_json TEXT NOT NULL DEFAULT '', -- JSON array of TokenRule objects
    -- ACR-400 Part 1: optional binding to an external git repo Digital Twin.
    -- Configurable only while DRAFT (enforced in covenant.SetGitTwin).
    git_twin_url         TEXT NOT NULL DEFAULT '', -- HTTPS clone URL; '' = no binding
    git_twin_provider    TEXT NOT NULL DEFAULT '', -- github | gitlab | gitea | local-hook
    git_twin_config_json TEXT NOT NULL DEFAULT '', -- JSON: branch allowlist, webhook_secret_hash, unit_mapper
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS access_tiers (
    tier_id          TEXT NOT NULL,
    covenant_id      TEXT NOT NULL REFERENCES covenants(covenant_id),
    display_name     TEXT NOT NULL,
    price_usd        REAL NOT NULL DEFAULT 0,
    token_multiplier REAL NOT NULL DEFAULT 1.0,
    max_slots        INTEGER,                  -- NULL = unlimited
    PRIMARY KEY (covenant_id, tier_id)
);

CREATE TABLE IF NOT EXISTS platform_identities (
    platform_id      TEXT PRIMARY KEY,             -- pid_{uuid}
    kyc_status       TEXT NOT NULL DEFAULT 'none',
    created_at       TEXT NOT NULL,
    platform_id_hash TEXT NOT NULL DEFAULT '',     -- ACR-700 §4: SHA-256(plaintext) hex; indexable lookup key
    platform_id_enc  BLOB                          -- ACR-700 §2.3 ciphertext blob; NULL until 4.5.4 writer cutover
);
CREATE INDEX IF NOT EXISTS idx_platform_identities_hash
    ON platform_identities(platform_id_hash) WHERE platform_id_hash != '';

CREATE TABLE IF NOT EXISTS covenant_members (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    covenant_id  TEXT NOT NULL REFERENCES covenants(covenant_id),
    platform_id  TEXT NOT NULL REFERENCES platform_identities(platform_id),
    agent_id     TEXT NOT NULL,               -- agent_{random8}
    tier_id      TEXT,
    is_owner     INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'active',
    joined_at    TEXT NOT NULL,
    UNIQUE(covenant_id, agent_id),
    UNIQUE(covenant_id, platform_id)
);

-- ── Audit Log (ACR-300 v0.2) ───────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS audit_logs (
    log_id       TEXT PRIMARY KEY,            -- uuid
    covenant_id  TEXT NOT NULL,
    sequence     INTEGER NOT NULL,
    agent_id     TEXT NOT NULL,
    session_id   TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    tool_type    TEXT NOT NULL,               -- clause | query | admin
    params_hash  TEXT NOT NULL,               -- SHA-256(params JSON)
    params_preview TEXT NOT NULL,             -- JSON, sensitive fields masked
    result       TEXT NOT NULL,               -- success | rejected | error
    result_detail TEXT NOT NULL DEFAULT '',
    tokens_delta INTEGER NOT NULL DEFAULT 0,
    cost_delta   INTEGER NOT NULL DEFAULT 0,  -- minor units of cost_currency (e.g. USD cents)
    cost_currency TEXT NOT NULL DEFAULT 'USD', -- ISO 4217 (ACR-300@2.2)
    net_delta    REAL NOT NULL DEFAULT 0,     -- cost_weight × cost_delta may be fractional
    state_before TEXT NOT NULL,
    state_after  TEXT NOT NULL,
    timestamp    TEXT NOT NULL,               -- RFC3339 UTC
    prev_log_id  TEXT,                        -- NULL for genesis
    hash         TEXT NOT NULL,               -- SHA-256 chain hash
    spec_version TEXT NOT NULL DEFAULT 'ACR-300@2.2',
    UNIQUE(covenant_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_audit_covenant ON audit_logs(covenant_id, sequence);

-- ── Token Ledger (ACR-20) ──────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS token_ledger (
    id           TEXT PRIMARY KEY,            -- uuid
    covenant_id  TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    delta        INTEGER NOT NULL,            -- positive = earn, negative = reversal
    balance_after INTEGER NOT NULL,
    source_type  TEXT NOT NULL,               -- passage | edit | bible | outline | reversal
    source_ref   TEXT NOT NULL,               -- draft_id or similar
    log_id       TEXT NOT NULL UNIQUE,        -- ref to audit_logs.log_id
    status       TEXT NOT NULL DEFAULT 'confirmed'  -- confirmed | pending | reversed
);
CREATE INDEX IF NOT EXISTS idx_ledger_agent ON token_ledger(covenant_id, agent_id);

CREATE TABLE IF NOT EXISTS pending_tokens (
    draft_id     TEXT PRIMARY KEY,
    covenant_id  TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);

-- ── Budget Counter (ACR-60 MVP) ────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS budget_counters (
    covenant_id    TEXT PRIMARY KEY,
    budget_limit   INTEGER NOT NULL DEFAULT 0,  -- minor units of currency; 0 = unlimited
    budget_spent   INTEGER NOT NULL DEFAULT 0,  -- minor units of currency
    currency       TEXT NOT NULL DEFAULT 'USD', -- ISO 4217; mirrors covenants.budget_currency
    updated_at     TEXT NOT NULL
);

-- ── Settlement (ACR-100 MVP) ───────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS settlement_outputs (
    output_id       TEXT PRIMARY KEY,          -- sout_{random8}
    covenant_id     TEXT NOT NULL,
    trigger_log_id  TEXT NOT NULL,
    trigger_log_hash TEXT NOT NULL,
    total_tokens    INTEGER NOT NULL,
    owner_share_pct REAL NOT NULL,
    platform_share_pct REAL NOT NULL,
    contributor_pool_pct REAL NOT NULL,
    distributions  TEXT NOT NULL,             -- JSON array
    generated_at    TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending_confirmation', -- pending_confirmation | confirmed
    confirmed_at    TEXT                       -- NULL until confirmed
);

-- ── Session Tokens (REVIEW-14) ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS session_tokens (
    token_id    TEXT PRIMARY KEY,             -- uuid
    agent_id    TEXT NOT NULL,
    covenant_id TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,         -- SHA-256; raw never stored
    status      TEXT NOT NULL DEFAULT 'active', -- active | grace | expired
    created_at  TEXT NOT NULL,
    rotated_at  TEXT,                         -- when entered grace
    expires_at  TEXT                          -- absolute expiry, NULL = rotate-only
);
CREATE INDEX IF NOT EXISTS idx_session_agent ON session_tokens(agent_id, covenant_id, status);

-- ── Token Snapshots (Phase 2 WI5: lock-time snapshot) ─────────────────────

CREATE TABLE IF NOT EXISTS token_snapshots (
  id            TEXT PRIMARY KEY,
  covenant_id   TEXT NOT NULL,
  agent_id      TEXT NOT NULL,
  agent_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_tokens   INTEGER NOT NULL DEFAULT 0,
  snapped_at    TEXT NOT NULL,
  snapshot_hash TEXT NOT NULL DEFAULT '', -- ACR-20 Part 5: SHA-256 tamper-evidence
  FOREIGN KEY (covenant_id) REFERENCES covenants(covenant_id)
);
CREATE INDEX IF NOT EXISTS idx_snapshots_covenant ON token_snapshots(covenant_id, agent_id);

-- ── Budget Reservations (Phase 2 WI6: authorize-then-settle) ──────────────

CREATE TABLE IF NOT EXISTS budget_reservations (
  id           TEXT PRIMARY KEY,
  covenant_id  TEXT NOT NULL,
  audit_log_id TEXT NOT NULL DEFAULT '',
  amount       INTEGER NOT NULL,  -- USD cents
  status       TEXT NOT NULL DEFAULT 'reserved',  -- reserved | settled | released
  created_at   TEXT NOT NULL
);

-- ── Git Twin Events (ACR-400 Part 7.2 idempotency) ────────────────────────

CREATE TABLE IF NOT EXISTS git_twin_events (
  draft_ref      TEXT PRIMARY KEY,            -- PR URL or commit SHA; bridge idempotency key
  covenant_id    TEXT NOT NULL,
  agent_id       TEXT NOT NULL,
  draft_id       TEXT NOT NULL DEFAULT '',    -- pending_tokens.draft_id after propose
  propose_log_id TEXT NOT NULL DEFAULT '',
  approve_log_id TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'proposed',  -- proposed | approved | rejected
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_git_twin_events_covenant ON git_twin_events(covenant_id);

-- ── Git Twin Anchors (ACR-400 Part 5) ─────────────────────────────────────
-- Server enqueues a pending row at confirm_settlement; the bridge polls, writes
-- a git note to refs/notes/acp-anchors, then acks with the resulting commit SHA.
CREATE TABLE IF NOT EXISTS git_twin_anchors (
  anchor_id            TEXT PRIMARY KEY,            -- anch_<random8>
  covenant_id          TEXT NOT NULL,
  settlement_output_id TEXT NOT NULL,
  repo_url             TEXT NOT NULL,
  settlement_hash      TEXT NOT NULL,               -- settlement_outputs.trigger_log_hash
  snapshot_hash        TEXT NOT NULL,               -- roll-up over token_snapshots for this covenant
  note_body            TEXT NOT NULL,               -- JSON payload the bridge writes as a git note
  status               TEXT NOT NULL DEFAULT 'pending',  -- pending | written
  enqueued_at          TEXT NOT NULL,
  written_at           TEXT,
  written_commit_sha   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_git_twin_anchors_status ON git_twin_anchors(status, enqueued_at);
CREATE INDEX IF NOT EXISTS idx_git_twin_anchors_covenant ON git_twin_anchors(covenant_id);

-- ── Anti-Gaming Policy (ACR-20 Part 4) ────────────────────────────────────
-- One row per covenant. Owner-configured via configure_anti_gaming tool.
-- Phase 4.1 populates rate_limit_per_hour only; later chunks add the rest.
CREATE TABLE IF NOT EXISTS anti_gaming_policies (
  covenant_id             TEXT PRIMARY KEY REFERENCES covenants(covenant_id),
  rate_limit_per_hour     INTEGER NOT NULL DEFAULT 0,   -- 0 = unlimited (Layer 2)
  similarity_threshold    REAL    NOT NULL DEFAULT 0.0, -- Layer 3, Phase 4+
  min_word_count          INTEGER NOT NULL DEFAULT 0,   -- Layer 4, Phase 4+
  concentration_warn_pct  REAL    NOT NULL DEFAULT 0.0, -- Layer 5, Phase 4.3
  updated_at              TEXT NOT NULL
);

-- ── Rate-Limit Counters (ACR-20 Part 4 Layer 2) ───────────────────────────
-- Per (covenant, agent, tool, hour-window) bucket. Phase 4.1 writes every
-- clause-tool call to tool_name='*' (global bucket); tool_name column kept
-- for future per-tool policies without migration.
CREATE TABLE IF NOT EXISTS rate_limit_counters (
  covenant_id   TEXT NOT NULL,
  agent_id      TEXT NOT NULL,
  tool_name     TEXT NOT NULL,   -- '*' = global bucket (Phase 4.1 default)
  window_start  TEXT NOT NULL,   -- RFC3339 UTC, floored to the hour
  call_count    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (covenant_id, agent_id, tool_name, window_start)
);
CREATE INDEX IF NOT EXISTS idx_ratelimit_cleanup ON rate_limit_counters(window_start);
