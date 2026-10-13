-- Browser consent delegates one account to one server key and authenticated profile.
-- Friendly labels are self-reported metadata, never a hardware identity proof.
CREATE TABLE host_connection_requests (
 request_id TEXT PRIMARY KEY, authorization_ref TEXT NOT NULL UNIQUE,
 environment_id TEXT NOT NULL, local_user_id TEXT NOT NULL, public_key TEXT NOT NULL,
 host_name TEXT NOT NULL, platform TEXT NOT NULL, content_hash TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', owner_user_id TEXT, agent_id TEXT,
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, decided_at INTEGER, claimed_at INTEGER, connection_generation INTEGER
);
CREATE INDEX host_requests_binding ON host_connection_requests(environment_id,local_user_id,status);
CREATE TABLE host_connections (
 environment_id TEXT NOT NULL, local_user_id TEXT NOT NULL, public_key TEXT NOT NULL,
 host_name TEXT NOT NULL, platform TEXT NOT NULL, user_id TEXT NOT NULL, agent_id TEXT NOT NULL UNIQUE,
 credential_enc TEXT NOT NULL, credential_hash TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1,
 connection_generation INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'active',
 expires_at INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(environment_id,local_user_id)
);
CREATE INDEX host_connections_owner ON host_connections(user_id);
CREATE TABLE host_proof_replays (environment_id TEXT NOT NULL, public_key TEXT NOT NULL, jti TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(environment_id,public_key,jti));
CREATE TABLE host_handoffs(code_hash TEXT PRIMARY KEY,agent_id TEXT NOT NULL,user_id TEXT NOT NULL,expires_at INTEGER NOT NULL,consumed_at INTEGER);
