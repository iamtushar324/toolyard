-- 0026: operator tokens and secret requests.
--
-- An operator token lets a CLI agent drive the same /v1 API the dashboard
-- uses, as the user who owns it, limited to its scopes (read, write, owner).
-- Only the sha256 of the token is stored.
CREATE TABLE IF NOT EXISTS operator_tokens (
  id           TEXT PRIMARY KEY,          -- op_<hex>; the token is tyop_<hex>.<secret>
  user_id      TEXT NOT NULL,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  scopes       TEXT NOT NULL,             -- space-separated, e.g. "read write"
  created_by   TEXT,                      -- "cli", "dashboard", or the parent op_ id
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  last_used_at INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_operator_tokens_user ON operator_tokens(user_id);

-- A requested secret is a named placeholder an agent created so the owner can
-- type the value in later. It can be referenced immediately; it resolves only
-- once the owner has set it. value_enc stays '' while pending.
ALTER TABLE secrets ADD COLUMN pending INTEGER NOT NULL DEFAULT 0;
ALTER TABLE secrets ADD COLUMN requested_by TEXT;
