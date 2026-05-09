-- 0008: OAuth integration for remote MCP upstreams.
--
-- Many remote MCPs (Atlassian, Linear, Notion, GitHub-hosted MCPs) speak
-- OAuth 2.1 with PKCE over Streamable HTTP. We store one client per
-- upstream (Dynamic Client Registration when supported, manual fallback
-- otherwise) plus its current encrypted tokens so the refresher can keep
-- the connection alive without operator intervention.
--
-- Tokens live in their own row keyed by upstream_name so we can rotate
-- the schema (or wipe tokens) without touching client registration.

CREATE TABLE IF NOT EXISTS oauth_clients (
    upstream_name                  TEXT PRIMARY KEY,
    issuer                         TEXT NOT NULL,
    authorization_endpoint         TEXT NOT NULL,
    token_endpoint                 TEXT NOT NULL,
    registration_endpoint          TEXT,
    revocation_endpoint            TEXT,
    device_authorization_endpoint  TEXT,
    client_id                      TEXT NOT NULL,
    client_secret_enc              TEXT,
    redirect_uri                   TEXT NOT NULL,
    scopes                         TEXT NOT NULL,
    token_endpoint_auth_method     TEXT NOT NULL,
    metadata_json                  TEXT,
    registered_at                  INTEGER NOT NULL,
    updated_at                     INTEGER NOT NULL,
    FOREIGN KEY(upstream_name) REFERENCES upstream_servers(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS oauth_tokens (
    upstream_name        TEXT PRIMARY KEY,
    access_token_enc     TEXT,
    refresh_token_enc    TEXT,
    token_type           TEXT,
    scope                TEXT,
    obtained_at          INTEGER,
    access_expires_at    INTEGER,
    refresh_expires_at   INTEGER,
    last_refresh_at      INTEGER,
    refresh_failures     INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL DEFAULT 'unauthorized',
    last_error           TEXT,
    is_pat               INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY(upstream_name) REFERENCES upstream_servers(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS oauth_pending (
    state             TEXT PRIMARY KEY,
    upstream_name     TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    mode              TEXT NOT NULL,
    code_verifier     TEXT,
    device_code       TEXT,
    user_code         TEXT,
    interval_s        INTEGER,
    verification_uri  TEXT,
    expires_at        INTEGER NOT NULL,
    created_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_pending_user    ON oauth_pending(user_id);
CREATE INDEX IF NOT EXISTS idx_oauth_pending_expires ON oauth_pending(expires_at);
