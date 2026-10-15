-- 0024: per-person identity keys forwarded to Beknown services.
--
-- Each user may hold one identity key. It is the token of that user's
-- dedicated identity agent (agents.kind = 'identity'), so it authenticates
-- at /mcp like any agent token (Authorization: Bearer or x-bf-vk). toolyard
-- keeps the raw key sealed because upstreams such as BkCoreServices expect
-- the raw value in x-bk-bifrost-vk and resolve sha256(key) in prime-service's
-- bifrost_virtual_key_actors registry. agents.token_hash and
-- user_identity_keys.key_hash are the same sha256 hex.
ALTER TABLE agents ADD COLUMN kind TEXT NOT NULL DEFAULT 'agent' CHECK (kind IN ('agent', 'identity'));
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_identity_owner ON agents(owner_user) WHERE kind = 'identity';

CREATE TABLE IF NOT EXISTS user_identity_keys (
    user_id     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    agent_id    TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    key_hash    TEXT NOT NULL UNIQUE,  -- sha256 hex of the raw key: the registry fingerprint
    key_sealed  TEXT NOT NULL,         -- sealbox(raw key), AAD = user id
    revealed_at INTEGER,               -- when the owner saw it (shown once)
    created_by  TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- Where each key's fingerprint was registered: one row per registry
-- upstream (a server whose identity setting has register = true).
CREATE TABLE IF NOT EXISTS identity_key_registrations (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    upstream   TEXT NOT NULL,
    key_hash   TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('registered', 'error', 'revoked')),
    error      TEXT,
    by_user    TEXT,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, upstream)
);

-- Fingerprints that must leave a registry: superseded by a rotate or
-- revoked, but not yet deleted there. A row leaves this table only once
-- delete-bifrost-virtual-key-actor succeeded (or the registry's list no
-- longer shows the hash); Register and the background retry keep trying as
-- the retrying admin or the recorded by_user. No cascade on user_id: a
-- removed user's fingerprint still has to go.
CREATE TABLE IF NOT EXISTS identity_key_retired (
    upstream   TEXT NOT NULL,
    key_hash   TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    by_user    TEXT,
    error      TEXT,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (upstream, key_hash)
);
CREATE INDEX IF NOT EXISTS idx_identity_key_retired_user ON identity_key_retired(user_id);

-- Per-server identity forwarding: {"header": "x-bk-bifrost-vk", "register": true}.
ALTER TABLE upstream_servers ADD COLUMN identity_json TEXT;
