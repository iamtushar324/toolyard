-- 0056: per-agent scope (v0.1 slice C, docs/design/v0.1.md section C).
--
-- agent_scopes narrows one agent below its owner's scope: mode 'all' is
-- everything the owner may use, 'listed' only the allowed servers and tools
-- (the always-on groups stay), and in both a deny wins. rules is JSON:
-- {"allow_servers": [...], "allow_tools": [{"server": ..., "tool": ...}],
-- "deny_servers": [...], "deny_tools": [...]}. An agent without a row keeps
-- exactly its owner's scope, so existing agents are unchanged. Applies to
-- OAuth clients (set on the consent screen) and to ordinary agents.
--
-- Same table as migrations/sqlite/0056_agent_scopes.sql.
-- Rollback: an older binary ignores the table (and every agent's limits).

CREATE TABLE IF NOT EXISTS agent_scopes (
    agent_id   TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    mode       TEXT NOT NULL DEFAULT 'all' CHECK (mode IN ('all', 'listed')),
    rules      TEXT NOT NULL DEFAULT '{}',
    updated_at BIGINT NOT NULL,
    updated_by TEXT
);
