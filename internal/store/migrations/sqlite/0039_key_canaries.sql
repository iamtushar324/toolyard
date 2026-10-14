-- 0039: key canaries, so a wrong or missing master key stops the start.
--
-- One row per master key file (identity.key, oauth.key, secrets.key,
-- session.key). check_blob is a fixed constant sealed with that key
-- (internal/keyguard); at startup each key must open its row, or toolyard
-- refuses to start instead of failing to decrypt later. kid is a short,
-- non-secret fingerprint of the key, shown in errors so an operator can
-- tell key files apart. created_at is Unix milliseconds.
--
-- The table starts empty: the first start with this version writes a row
-- for each key it finds (adopting an existing installation's keys). While
-- any row exists, a missing key is never created again. Rollback: an older
-- binary ignores this table.
CREATE TABLE IF NOT EXISTS key_canaries (
    name       TEXT PRIMARY KEY,
    kid        TEXT NOT NULL,
    check_blob BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
