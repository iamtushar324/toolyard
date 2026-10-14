-- 0003: simple key/value store for system-wide settings (no scope, no ACLs).
-- Values are JSON so we can persist booleans, strings, or small structs.

CREATE TABLE IF NOT EXISTS system_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
