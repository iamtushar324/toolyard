CREATE TABLE IF NOT EXISTS hook_events (
    id                TEXT PRIMARY KEY,
    ts                INTEGER NOT NULL,
    created_at        INTEGER NOT NULL,
    agent_id          TEXT NOT NULL,
    source            TEXT NOT NULL,
    event_name        TEXT NOT NULL,
    session_id        TEXT,
    turn_id           TEXT,
    conversation_id   TEXT,
    tool_name         TEXT,
    cwd               TEXT,
    text              TEXT,
    payload           TEXT,
    metadata          TEXT,
    memory_ingested   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_hook_events_ts
    ON hook_events(ts DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_agent_ts
    ON hook_events(agent_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_session_ts
    ON hook_events(session_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_source_ts
    ON hook_events(source, ts DESC);
CREATE INDEX IF NOT EXISTS idx_hook_events_event_ts
    ON hook_events(event_name, ts DESC);
