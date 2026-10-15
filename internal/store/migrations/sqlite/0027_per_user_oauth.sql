-- 0027: per-user sign-in for upstream servers.
--
-- auth_mode says who signs in to an upstream:
--   shared   one account for everyone (today's oauth_tokens row); every
--            change made through the server goes as that account.
--   per_user each toolyard user connects their own account; an agent's
--            calls run with the token of the person who owns the agent,
--            and never with a shared credential.
--
-- The OAuth client (oauth_clients) stays one per server in both modes:
-- everyone shares the app registration, each person has their own token.
ALTER TABLE upstream_servers ADD COLUMN auth_mode TEXT NOT NULL DEFAULT 'shared' CHECK (auth_mode IN ('shared', 'per_user'));

-- One row per (server, person). Same token columns as oauth_tokens, plus
-- the account label the provider reported (id_token email when present).
-- Rows go with the server and with the user, as user_server_access does.
CREATE TABLE IF NOT EXISTS oauth_user_tokens (
    upstream_name        TEXT NOT NULL REFERENCES upstream_servers(name) ON DELETE CASCADE,
    user_id              TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access_token_enc     TEXT,
    refresh_token_enc    TEXT,
    token_type           TEXT,
    scope                TEXT,
    account_label        TEXT,
    obtained_at          INTEGER,
    access_expires_at    INTEGER,
    refresh_expires_at   INTEGER,
    last_refresh_at      INTEGER,
    refresh_failures     INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL DEFAULT 'unauthorized',
    last_error           TEXT,
    PRIMARY KEY (upstream_name, user_id)
);
CREATE INDEX IF NOT EXISTS idx_oauth_user_tokens_user ON oauth_user_tokens(user_id);

-- The shared token gets the same best-effort label so the dashboard can
-- say which account a shared server acts as.
ALTER TABLE oauth_tokens ADD COLUMN account_label TEXT;

-- A pending flow started from "My connections" stores its token on the
-- oauth_user_tokens row of oauth_pending.user_id, and only when the browser
-- that returns to the callback is signed in as that user.
ALTER TABLE oauth_pending ADD COLUMN per_user INTEGER NOT NULL DEFAULT 0;
