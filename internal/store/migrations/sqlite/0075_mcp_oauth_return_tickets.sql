-- 0075: return links for toolyard's OAuth server for MCP clients (TEC-1808,
-- docs/self-hosting.md "OAuth for MCP clients").
--
-- When the person allows an app on the consent screen, toolyard no longer
-- sends the browser straight back to the client with a code. It stores a
-- return ticket and shows a return link, so the person can allow the app
-- in one browser (or Chrome profile) and finish in the one where the
-- client waits.
--
-- mcp_oauth_return_tickets is one return link: ticket_hash is the SHA-256
-- hex of the ticket (the raw ticket is never stored). It keeps what the
-- authorization code needs (the grant, client, redirect URI, state, PKCE
-- challenge, resource and scope of the approved request) and the person
-- who approved it. It works once (used_at) until expires_at, ten minutes
-- after created_at (the consent). The code is minted only when the link is
-- used, so its 60 seconds start then. A newer consent for the same grant
-- ends its unused tickets. Times are Unix milliseconds.
--
-- Same table as migrations/postgres/0075_mcp_oauth_return_tickets.sql.
-- Rollback: an older binary ignores the table (consent then redirects with
-- a code again); drop it by hand after the rollback.
CREATE TABLE IF NOT EXISTS mcp_oauth_return_tickets (
    ticket_hash    TEXT PRIMARY KEY,
    grant_id       TEXT NOT NULL REFERENCES mcp_oauth_grants(id) ON DELETE CASCADE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id      TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL,
    state          TEXT NOT NULL,
    code_challenge TEXT NOT NULL,
    resource       TEXT NOT NULL,
    scope          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    used_at        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_return_tickets_expires ON mcp_oauth_return_tickets (expires_at);
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_return_tickets_grant ON mcp_oauth_return_tickets (grant_id);
