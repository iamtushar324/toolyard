-- 0044: passkey ceremonies in the database (TEC-1630, v0.1 slice A).
--
-- A WebAuthn ceremony (registration or assertion) starts with one request
-- and finishes with another, which may reach another replica. The started
-- ceremony was kept in process memory; it is now one row here, read and
-- deleted in one statement when it finishes (single use).
--
-- id is the random ceremony ID the browser carries; kind is register or
-- assert; user_id the owner it is for; purpose what an assertion confirms
-- ('' for a registration); session the go-webauthn session data as JSON;
-- rp_id and origin the relying party the ceremony started from. Times are
-- Unix milliseconds; expired rows are deleted when the next one starts.
--
-- Same table as migrations/postgres/0044_passkey_ceremonies.sql.
-- Rollback: an older binary ignores the table.
CREATE TABLE IF NOT EXISTS passkey_ceremonies (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    purpose    TEXT NOT NULL,
    session    TEXT NOT NULL,
    rp_id      TEXT NOT NULL,
    origin     TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_passkey_ceremonies_expires ON passkey_ceremonies (expires_at);
