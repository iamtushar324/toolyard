-- toolyard v0.1 schema. SQLite, written Postgres-portable (avoid TEXT-vs-VARCHAR
-- distinctions, no AUTOINCREMENT, no SQLite-specific affinity tricks).

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    owner_user      TEXT NOT NULL,
    token_hash      TEXT NOT NULL,
    enroll_code     TEXT,
    enroll_expires  INTEGER,
    last_seen       INTEGER,
    created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_owner ON agents(owner_user);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_enroll_code ON agents(enroll_code) WHERE enroll_code IS NOT NULL;

CREATE TABLE IF NOT EXISTS tool_registrations (
    id              TEXT PRIMARY KEY,
    upstream_name   TEXT NOT NULL,
    tool_name       TEXT NOT NULL,
    description     TEXT,
    input_schema    TEXT NOT NULL,
    annotations     TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE(upstream_name, tool_name)
);

CREATE TABLE IF NOT EXISTS policies (
    id              TEXT PRIMARY KEY,
    priority        INTEGER NOT NULL DEFAULT 0,
    expression      TEXT NOT NULL,
    action          TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_policies_priority ON policies(priority);

CREATE TABLE IF NOT EXISTS approval_requests (
    id              TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    upstream_name   TEXT NOT NULL,
    tool_name       TEXT NOT NULL,
    arguments       TEXT NOT NULL,
    reason          TEXT NOT NULL,
    intent_category TEXT,
    status          TEXT NOT NULL,
    decision_token  TEXT,
    decided_by      TEXT,
    decided_at      INTEGER,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approvals_pending
    ON approval_requests(status, expires_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_approvals_created
    ON approval_requests(created_at DESC);

CREATE TABLE IF NOT EXISTS audit_events (
    id              TEXT PRIMARY KEY,
    ts              INTEGER NOT NULL,
    agent_id        TEXT,
    upstream_name   TEXT,
    tool_name       TEXT,
    event_type      TEXT NOT NULL,
    decision        TEXT,
    reason          TEXT,
    arguments       TEXT,
    result_summary  TEXT,
    approval_id     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts        ON audit_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_agent_ts  ON audit_events(agent_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_approval  ON audit_events(approval_id);

CREATE TABLE IF NOT EXISTS memory_entries (
    scope           TEXT NOT NULL,
    key             TEXT NOT NULL,
    value           TEXT NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY(scope, key)
);
CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_entries(scope);

CREATE TABLE IF NOT EXISTS push_subscriptions (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    endpoint        TEXT NOT NULL UNIQUE,
    p256dh          TEXT NOT NULL,
    auth            TEXT NOT NULL,
    user_agent      TEXT,
    created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_push_user ON push_subscriptions(user_id);

CREATE TABLE IF NOT EXISTS server_keys (
    id              TEXT PRIMARY KEY,
    purpose         TEXT NOT NULL UNIQUE,
    private_key     BLOB NOT NULL,
    public_key      BLOB NOT NULL,
    created_at      INTEGER NOT NULL
);
