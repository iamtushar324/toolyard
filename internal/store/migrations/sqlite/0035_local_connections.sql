-- Local servers prove existing Toolyard account control with an agent API key.
-- Persist tombstones and parent hashes so revocation cannot silently reconnect.
CREATE TABLE local_connections (
 environment_id TEXT NOT NULL, local_user_id TEXT NOT NULL, user_id TEXT NOT NULL,
 parent_agent_id TEXT NOT NULL, parent_token_hash TEXT NOT NULL,
 agent_id TEXT NOT NULL UNIQUE, credential_enc TEXT NOT NULL, credential_hash TEXT NOT NULL, version INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'active', expires_at INTEGER NOT NULL, connection_generation INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(environment_id,local_user_id), UNIQUE(environment_id,parent_agent_id)
);
CREATE TABLE local_handoffs (
 code_hash TEXT PRIMARY KEY, agent_id TEXT NOT NULL, user_id TEXT NOT NULL,
 expires_at INTEGER NOT NULL, consumed_at INTEGER
);
ALTER TABLE callback_receivers ADD COLUMN transport TEXT NOT NULL DEFAULT 'push';
