-- 0002: persist upstream MCP server configs so the dashboard can manage them
-- and they survive restarts without an external config file.

CREATE TABLE IF NOT EXISTS upstream_servers (
    name        TEXT PRIMARY KEY,
    transport   TEXT NOT NULL,                -- 'stdio' | 'http'
    command     TEXT,                         -- stdio: program path
    args_json   TEXT,                         -- stdio: JSON-encoded []string
    url         TEXT,                         -- http: endpoint URL
    env_json    TEXT,                         -- JSON-encoded map[string]string
    enabled     INTEGER NOT NULL DEFAULT 1,
    last_status TEXT,                         -- 'ok' | error message
    last_error  TEXT,
    tool_count  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
