-- 0076: the waiting stop page of toolyard's OAuth server for MCP clients
-- (TEC-1808, docs/self-hosting.md "OAuth for MCP clients").
--
-- The page /oauth/authorize stops on also waits: after the person allows
-- the app in another browser (the copied consent link), it shows
-- "Approved in another browser" and its Continue finishes in the window
-- where the app started, with the same approval as the return link (0075):
-- whichever of the two is used first works, the other is refused as used.
--
-- mcp_oauth_requests gains:
--   waiter_hash  SHA-256 hex of the waiter secret that only the stop page
--                carries (the raw value is never stored and never in a
--                URL). NULL for requests an older binary stored: their stop
--                page does not wait.
--   verdict      'allow' or 'deny' once the person answered; NULL before,
--                and for answers an older binary recorded.
-- mcp_oauth_return_tickets gains request_id, the authorization request the
-- consent answered, so the waiting page finds its approval. NULL for
-- tickets an older binary stored.
--
-- Same columns as migrations/postgres/0076_mcp_oauth_waiting_page.sql.
-- Rollback: an older binary ignores the columns (its stop page does not
-- wait; the return link still works). Drop them by hand after the rollback.
ALTER TABLE mcp_oauth_requests ADD COLUMN waiter_hash TEXT;
ALTER TABLE mcp_oauth_requests ADD COLUMN verdict TEXT;
ALTER TABLE mcp_oauth_return_tickets ADD COLUMN request_id TEXT;
CREATE INDEX IF NOT EXISTS idx_mcp_oauth_return_tickets_request ON mcp_oauth_return_tickets (request_id);
