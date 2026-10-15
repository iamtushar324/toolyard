-- 0004: per-(agent, tool) call counters that drive top-N agent surfaces.
-- agent_id = '' for unauthenticated/stdio sessions; we lump those into a
-- single shared bucket. count is incremented only on call.succeeded.

CREATE TABLE IF NOT EXISTS tool_usage (
    agent_id     TEXT NOT NULL DEFAULT '',
    tool_name    TEXT NOT NULL,
    count        INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL,
    PRIMARY KEY (agent_id, tool_name)
);
CREATE INDEX IF NOT EXISTS idx_tool_usage_agent_count
    ON tool_usage(agent_id, count DESC, last_used_at DESC);
CREATE INDEX IF NOT EXISTS idx_tool_usage_tool
    ON tool_usage(tool_name);
