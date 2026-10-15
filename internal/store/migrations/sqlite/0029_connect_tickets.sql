-- 0029: one-time connect links.
--
-- An agent whose owner has not signed in to a server (their own account on
-- a per_user server; the shared account of a shared OAuth server, when the
-- owner is an admin) gets a link it shows the person in chat. The link
-- carries a random ticket; only the ticket's sha256 is stored here. Opening
-- the link redeems the ticket once (used_at), starts the OAuth flow for
-- that user and server in that browser, and sends the browser on to the
-- provider. Rows go with the server and with the user.
CREATE TABLE IF NOT EXISTS connect_tickets (
    id_hash    TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    upstream   TEXT NOT NULL REFERENCES upstream_servers(name) ON DELETE CASCADE,
    purpose    TEXT NOT NULL CHECK (purpose IN ('per_user', 'shared')),
    agent_id   TEXT,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_connect_tickets_expires ON connect_tickets(expires_at);

-- A pending flow a connect link started. On a per_user flow the callback
-- checks the account the provider reports against the person's email, and
-- the success page sends the person back to their agent instead of the
-- dashboard.
ALTER TABLE oauth_pending ADD COLUMN via_ticket INTEGER NOT NULL DEFAULT 0;
