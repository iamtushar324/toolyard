-- 0055: toolyard as an OAuth 2.1 authorization server for its own /mcp
-- (v0.1 slice C, docs/design/v0.1.md section C).
--
-- MCP clients (ChatGPT, Claude.ai, Claude Code, Codex, Cursor) add toolyard
-- by URL and sign in through OAuth. These tables are separate from the
-- upstream OAuth client tables (oauth_clients, oauth_tokens, oauth_pending,
-- oauth_user_tokens), which hold toolyard's own sign-ins to other servers.
-- Every piece of server state lives here, never only in memory, so several
-- replicas on one Postgres database share it.
--
-- mcp_oauth_clients is one client: registered through RFC 7591 dynamic
-- registration (kind dcr, client_id mcpc_...) or read from a client ID
-- metadata document (kind cimd, client_id is the document's https URL and
-- expires_at ends the cached copy). redirect_uris is a JSON array.
-- secret_hash is the SHA-256 hex of a confidential DCR client's secret.
--
-- mcp_oauth_grants is one person's authorization of one client. Its agent
-- (agents.id) is who the client's calls run as; the nickname the person
-- chose is that agent's name. agent_id has no foreign key on purpose: the
-- grant is the tombstone that keeps the agent-to-client mapping when the
-- agent row is deleted (connecting the client again recreates the agent
-- under the same id). A revoked grant keeps its row too. may_manage is
-- the person's explicit choice that the client may manage toolyard
-- (policies, servers, shared connections) when its owner is an admin;
-- without it the agent never gets admin control (access.Scope).
--
-- mcp_oauth_requests is an authorization request waiting for consent (10
-- minutes); used_at is set once, when the person answers.
--
-- mcp_oauth_codes is an authorization code (SHA-256 hex of the code): single
-- use, 60 seconds. family_id names the tokens issued from the code, so a
-- replayed code revokes them.
--
-- mcp_oauth_tokens is an access or refresh token (SHA-256 hex). A refresh
-- token is used once (used_at); a second use revokes the whole grant,
-- except within 30 seconds while its successor (parent_hash names the token
-- it replaced) is still unused. auth_time is when the person consented;
-- no token outlives auth_time plus the absolute session limit.
--
-- mcp_oauth_rate_limits holds fixed-window counters per route and client IP.
--
-- Times are Unix milliseconds.
--
-- Same tables as migrations/sqlite/0055_mcp_oauth_server.sql.
-- Rollback: an older binary ignores the tables.

CREATE TABLE IF NOT EXISTS mcp_oauth_clients (
    client_id                  TEXT PRIMARY KEY,
    kind                       TEXT NOT NULL CHECK (kind IN ('dcr', 'cimd')),
    client_name                TEXT NOT NULL DEFAULT '',
    client_uri                 TEXT,
    logo_uri                   TEXT,
    redirect_uris              TEXT NOT NULL,
    token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',
    secret_hash                TEXT,
    metadata                   TEXT,
    registered_ip              TEXT,
    created_at                 BIGINT NOT NULL,
    updated_at                 BIGINT NOT NULL,
    expires_at                 BIGINT
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_clients_kind_created ON mcp_oauth_clients (kind, created_at);

CREATE TABLE IF NOT EXISTS mcp_oauth_grants (
    id             TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL REFERENCES mcp_oauth_clients(client_id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    agent_id       TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    resource       TEXT NOT NULL,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL,
    last_used_at   BIGINT,
    revoked_at     BIGINT,
    revoked_reason TEXT,
    may_manage     BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_oauth_grants_user_client ON mcp_oauth_grants (user_id, client_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_oauth_grants_agent ON mcp_oauth_grants (agent_id);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_grants_client ON mcp_oauth_grants (client_id);

CREATE TABLE IF NOT EXISTS mcp_oauth_requests (
    id             TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    state          TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    resource       TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    created_at     BIGINT NOT NULL,
    expires_at     BIGINT NOT NULL,
    used_at        BIGINT,
    user_id        TEXT
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_requests_expires ON mcp_oauth_requests (expires_at);

CREATE TABLE IF NOT EXISTS mcp_oauth_codes (
    code_hash      TEXT PRIMARY KEY,
    grant_id       TEXT NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    client_id      TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    resource       TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    family_id      TEXT NOT NULL,
    created_at     BIGINT NOT NULL,
    expires_at     BIGINT NOT NULL,
    used_at        BIGINT
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_codes_expires ON mcp_oauth_codes (expires_at);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_codes_grant ON mcp_oauth_codes (grant_id);

CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
    token_hash  TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
    grant_id    TEXT NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    client_id   TEXT NOT NULL,
    family_id   TEXT NOT NULL,
    parent_hash TEXT,
    scope       TEXT NOT NULL DEFAULT '',
    resource    TEXT NOT NULL,
    auth_time   BIGINT NOT NULL,
    created_at  BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL,
    used_at     BIGINT,
    revoked_at  BIGINT
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_tokens_grant ON mcp_oauth_tokens (grant_id);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_tokens_family ON mcp_oauth_tokens (family_id);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_tokens_parent ON mcp_oauth_tokens (parent_hash);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_tokens_expires ON mcp_oauth_tokens (expires_at);

CREATE TABLE IF NOT EXISTS mcp_oauth_rate_limits (
    bucket   TEXT PRIMARY KEY,
    hits     BIGINT NOT NULL,
    reset_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_rate_limits_reset ON mcp_oauth_rate_limits (reset_at);
