-- 0006: per-call metrics fact table + auto-approval rules.
--
-- call_events is the fat fact row written by internal/metrics.Sink for every
-- tool call that hits routeEntry. We deliberately avoid storing argument
-- values (only their canonical fingerprint and top-level key names) so this
-- table stays free of caller-supplied secrets.
--
-- Rollups (mv_*) are computed lazily by the analytics package — schema
-- exists here so we can index them up front.

CREATE TABLE IF NOT EXISTS call_events (
    event_id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                    INTEGER NOT NULL,            -- unix millis
    request_id            TEXT,
    session_id            TEXT,

    agent_id              TEXT,
    agent_name            TEXT,                         -- snapshot for cheap reads

    upstream              TEXT NOT NULL,
    short_name            TEXT NOT NULL,                -- tool name without upstream prefix
    tool_name             TEXT NOT NULL,                -- "github.create_issue"
    is_write              INTEGER NOT NULL DEFAULT 0,
    is_destructive        INTEGER NOT NULL DEFAULT 0,
    pinned_tool           INTEGER NOT NULL DEFAULT 0,

    via                   TEXT,                         -- direct | tools.execute | dashboard
    surface_mode          TEXT,
    in_top_n              INTEGER,

    fingerprint           TEXT,
    args_size_bytes       INTEGER,
    args_top_keys         TEXT,                         -- JSON array

    reason_text           TEXT,
    reason_len            INTEGER,
    intent_category       TEXT,
    reason_quality        REAL,                         -- filled by offline scorer

    approval_id           TEXT,
    approval_outcome      TEXT,                         -- none|auto|approved|denied|expired
    approval_latency_ms   INTEGER,
    approval_via          TEXT,                         -- dashboard|push|cli|batch|auto
    approval_decider      TEXT,
    coalesced_into        TEXT,

    outcome               TEXT NOT NULL,                -- ok|error|timeout|denied|expired|deferred
    error_class           TEXT,
    queue_latency_ms      INTEGER,
    upstream_latency_ms   INTEGER,
    total_latency_ms      INTEGER,
    result_size_bytes     INTEGER,
    token_estimate_in     INTEGER,
    token_estimate_out    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_call_events_ts            ON call_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_call_events_agent_ts      ON call_events(agent_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_call_events_tool_ts       ON call_events(tool_name, ts DESC);
CREATE INDEX IF NOT EXISTS idx_call_events_fp_ts         ON call_events(fingerprint, ts DESC);
CREATE INDEX IF NOT EXISTS idx_call_events_outcome_ts    ON call_events(outcome, ts DESC);

-- approval_events is the per-approval lifecycle row, decoupled so we can
-- track latency independent of whether the call ever resumed.
CREATE TABLE IF NOT EXISTS approval_events (
    approval_id      TEXT PRIMARY KEY,
    fingerprint      TEXT,
    agent_id         TEXT,
    tool_name        TEXT,
    is_destructive   INTEGER NOT NULL DEFAULT 0,
    created_ts       INTEGER NOT NULL,
    decided_ts       INTEGER,
    ttl_seconds      INTEGER,
    outcome          TEXT NOT NULL,                    -- pending|approved|denied|expired|auto
    decided_via      TEXT,
    decided_by       TEXT,
    reason_text      TEXT,
    coalesced_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_approval_events_fp_ts ON approval_events(fingerprint, created_ts DESC);
CREATE INDEX IF NOT EXISTS idx_approval_events_agent ON approval_events(agent_id, created_ts DESC);
CREATE INDEX IF NOT EXISTS idx_approval_events_tool  ON approval_events(tool_name, created_ts DESC);

-- tool_dim carries metadata that doesn't belong on every call row: whether a
-- tool is destructive (vetoes auto-approval), when we last saw it, etc.
CREATE TABLE IF NOT EXISTS tool_dim (
    tool_name        TEXT PRIMARY KEY,
    upstream         TEXT NOT NULL,
    is_destructive   INTEGER NOT NULL DEFAULT 0,
    is_write         INTEGER NOT NULL DEFAULT 0,
    auto_approve_ok  INTEGER NOT NULL DEFAULT 0,        -- operator-set opt-in for tool-wide auto
    last_seen_ts     INTEGER
);

-- mv_fingerprint_state is the auto-approval feature store. One row per
-- (fingerprint, agent_id) covering "all-time" stats; the analytics package
-- also keeps a separate 30d view derived from call_events on the fly.
CREATE TABLE IF NOT EXISTS mv_fingerprint_state (
    fingerprint       TEXT NOT NULL,
    agent_id          TEXT NOT NULL,
    first_seen        INTEGER,
    last_seen         INTEGER,
    last_decided_ts   INTEGER,
    total_seen        INTEGER NOT NULL DEFAULT 0,
    approved          INTEGER NOT NULL DEFAULT 0,
    denied            INTEGER NOT NULL DEFAULT 0,
    expired           INTEGER NOT NULL DEFAULT 0,
    cooloff_until     INTEGER,
    PRIMARY KEY (fingerprint, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_fp_state_last_seen ON mv_fingerprint_state(last_seen DESC);

-- auto_approval_rules: source of truth for "skip the human tap because we
-- have learned this is safe". Reads happen on every approval bus create, so
-- the autoapproval package keeps a memo'd snapshot.
CREATE TABLE IF NOT EXISTS auto_approval_rules (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,                       -- static | pattern | tool
    agent_id        TEXT,                                -- null = any agent
    fingerprint     TEXT,                                -- null = any args
    tool_name       TEXT,                                -- null = any tool
    enabled         INTEGER NOT NULL DEFAULT 1,
    source          TEXT NOT NULL,                       -- user | proposer
    rationale_json  TEXT,
    created_at      INTEGER NOT NULL,
    cooloff_until   INTEGER,
    hit_count       INTEGER NOT NULL DEFAULT 0,
    last_hit_ts     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_auto_rules_lookup ON auto_approval_rules(enabled, kind);
CREATE INDEX IF NOT EXISTS idx_auto_rules_fp     ON auto_approval_rules(fingerprint) WHERE fingerprint IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_auto_rules_tool   ON auto_approval_rules(tool_name)   WHERE tool_name IS NOT NULL;

-- anomaly_events: append-only log of detected anomalies. Surfaced in the
-- dashboard's notifications tab. Human can dismiss (sets dismissed_at).
CREATE TABLE IF NOT EXISTS anomaly_events (
    id              TEXT PRIMARY KEY,
    ts              INTEGER NOT NULL,
    kind            TEXT NOT NULL,                       -- rate_spike | error_spike | args_outlier | reason_repeat
    severity        TEXT NOT NULL,                       -- info | warn | crit
    agent_id        TEXT,
    tool_name       TEXT,
    summary         TEXT NOT NULL,
    detail_json     TEXT,
    dismissed_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_anomaly_ts ON anomaly_events(ts DESC);

-- metrics_state: small key/value table for watermarks and rollup-refresh
-- timestamps. Kept generic so we can park future bookkeeping here without
-- another migration.
CREATE TABLE IF NOT EXISTS metrics_state (
    name        TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  INTEGER NOT NULL
);
