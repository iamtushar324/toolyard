-- 0021: owner inbox, scoped grants, agent sessions.
--
-- inbox_requests holds everything an agent sends its owner: access requests
-- (one or more restricted tools), questions, blockers and updates. The full
-- request lives in `doc` (JSON) so attachment and tool shapes can evolve
-- without a migration; the columns beside it are what we filter and sort on.
CREATE TABLE IF NOT EXISTS inbox_requests (
    id          TEXT PRIMARY KEY,          -- 'rq_' + uuid
    agent_id    TEXT NOT NULL,
    session_id  TEXT,
    kind        TEXT NOT NULL,             -- access | question | blocker | update
    status      TEXT NOT NULL,             -- pending | approved | denied | returned | answered | read | cancelled | expired
    urgency     TEXT NOT NULL,             -- now | soon | digest | fyi
    related_id  TEXT,                      -- an update's request, when it closes one
    doc         TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_status_created ON inbox_requests(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inbox_agent_status   ON inbox_requests(agent_id, status);

-- One grant per allowed tool in an approved access request. The plaintext
-- token is kept in pending_token only until the agent first collects it,
-- then cleared; afterwards only its sha256 remains.
CREATE TABLE IF NOT EXISTS inbox_grants (
    id             TEXT PRIMARY KEY,       -- 'gr_' + short uuid
    request_id     TEXT NOT NULL,
    tool_index     INTEGER NOT NULL,
    agent_id       TEXT NOT NULL,
    tool           TEXT NOT NULL,
    params         TEXT NOT NULL,          -- JSON constraints, as approved
    max_uses       INTEGER NOT NULL,
    uses           INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL,          -- active | used | expired | revoked
    token_hash     TEXT NOT NULL,
    pending_token  TEXT,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    revoked_at     INTEGER,
    last_used_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_inbox_grants_agent   ON inbox_grants(agent_id, status);
CREATE INDEX IF NOT EXISTS idx_inbox_grants_request ON inbox_grants(request_id);

CREATE TABLE IF NOT EXISTS inbox_grant_uses (
    id          TEXT PRIMARY KEY,
    grant_id    TEXT NOT NULL,
    arguments   TEXT,
    used_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_grant_uses_grant ON inbox_grant_uses(grant_id);

-- Dry runs are logged so the owner can see how many an agent ran before
-- sending, and which flags disappeared between drafts.
CREATE TABLE IF NOT EXISTS inbox_dry_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id    TEXT NOT NULL,
    title       TEXT NOT NULL,
    flags       TEXT NOT NULL,             -- JSON array of flag labels
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inbox_dry_runs_agent ON inbox_dry_runs(agent_id, created_at DESC);

-- Copies of media an agent linked to, fetched once when the request is sent.
CREATE TABLE IF NOT EXISTS inbox_blobs (
    sha256        TEXT PRIMARY KEY,
    content_type  TEXT NOT NULL,
    size          INTEGER NOT NULL,
    source_url    TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

-- A named unit of agent work, so the owner can see many parallel agents.
CREATE TABLE IF NOT EXISTS agent_sessions (
    id                 TEXT PRIMARY KEY,   -- 'ses_' + uuid
    agent_id           TEXT NOT NULL,
    title              TEXT NOT NULL,
    repo               TEXT,
    branch             TEXT,
    host               TEXT,
    status             TEXT NOT NULL,      -- working | waiting_on_owner | blocked_on_owner | idle | done
    note               TEXT,
    started_at         INTEGER NOT NULL,
    last_heartbeat_at  INTEGER NOT NULL,
    ended_at           INTEGER
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_agent ON agent_sessions(agent_id, last_heartbeat_at DESC);
