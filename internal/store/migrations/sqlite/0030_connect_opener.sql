-- 0030: who opened a connect link.
--
-- A connect link is redeemed by a same-origin POST from its confirm page,
-- and that request carries the browser's toolyard session when it has one.
-- opener_verified records that the session belonged to the flow's user, so
-- the callback (a cross-site return that never carries the session) may
-- store a per-user token whose account the provider did not name. Without
-- it, such a token is stored only when the provider's account email is the
-- person's own.
ALTER TABLE oauth_pending ADD COLUMN opener_verified INTEGER NOT NULL DEFAULT 0;
