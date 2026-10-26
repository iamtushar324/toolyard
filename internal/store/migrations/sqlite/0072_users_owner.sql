-- 0072: the owner, the one person in charge of an install.
--
-- is_owner = 1 marks the owner: always an active admin, never demoted or
-- blocked, and the only one who can hand the role to another active user
-- (POST /v1/users/{id}/make-owner). The partial unique index keeps it to
-- one row, so two first sign-ins racing for an empty install cannot both
-- become the owner. Who becomes the owner: the first account
-- /v1/auth/setup creates, else the first verified Clerk sign-in (with
-- -owner-email set, only that person). Existing installs get theirs at
-- startup (identity.EnsureOwner): the active user with the configured
-- -owner-email, else the oldest active admin. No backfill here: the owner
-- email is configuration the migration cannot see.
--
-- Same column and index as migrations/postgres/0072_users_owner.sql.
-- Rollback: an older binary ignores the column and the index, and treats
-- the oldest account as the owner again; drop the index, then the column,
-- to remove them.
ALTER TABLE users ADD COLUMN is_owner INTEGER NOT NULL DEFAULT 0 CHECK (is_owner IN (0, 1));
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_owner ON users(is_owner) WHERE is_owner = 1;
