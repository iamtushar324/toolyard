-- 0038: sign every browser out once.
--
-- Dashboard handoff links now open the Inbox and never create a session;
-- only password, Clerk or first-run sign-in does. This one-time, data-only
-- step revokes every dashboard session that exists before the change, so
-- each browser signs in again through one of those methods.
--
-- It uses the same representation as logout and "revoke sessions"
-- (identity.RevokeSession / RevokeAllForUser): revoked_at is set, in Unix
-- milliseconds, and the row stays. IsSessionValid then rejects the cookie.
-- No schema change: an older binary reads the same table and treats the
-- rows as revoked too. Rows already revoked keep their original time. An
-- empty table is a no-op.
UPDATE sessions
   SET revoked_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE revoked_at IS NULL;
