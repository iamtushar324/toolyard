-- 0028: which browser may finish an OAuth flow.
--
-- browser_bound marks a callback-mode flow started from a dashboard session.
-- The provider sends the browser back cross-site, so the Strict session
-- cookie does not come along; instead the begin response sets a short-lived
-- SameSite=Lax cookie bound to (state, user), and the callback accepts a
-- bound flow only from a browser holding it (or a session for that user).
-- A flow started without a browser (an operator token) stays unbound and
-- keeps today's behaviour.
ALTER TABLE oauth_pending ADD COLUMN browser_bound INTEGER NOT NULL DEFAULT 0;
