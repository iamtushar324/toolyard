-- 0007: server-side session revocation table.
--
-- The dashboard's auth cookie is a JWT (HS256), but a stolen JWT with TTL
-- 24h is unrecoverable until expiry without a server-side check. This
-- table lets logout, password change, and admin-initiated revoke
-- invalidate a specific session id immediately.

CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,        -- jti claim from the JWT
    user_id         TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    revoked_at      INTEGER,
    user_agent      TEXT,
    client_ip       TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
