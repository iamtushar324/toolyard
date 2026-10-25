-- 0057: the OAuth client id on audit rows (v0.1 slice C).
--
-- audit_events.oauth_client_id is the OAuth client (mcp_oauth_grants) whose
-- agent raised the row, set when the row is written by a trigger, so every
-- path that writes audit rows carries it without knowing about OAuth, and
-- the row keeps it after the agent or the grant changes. NULL for agents
-- that are not OAuth clients. An explicit value is kept.
--
-- Same column as migrations/postgres/0057_audit_oauth_client.sql (a BEFORE
-- INSERT trigger there, an AFTER INSERT trigger here).
-- Rollback: an older binary ignores the column; the trigger keeps filling it.

ALTER TABLE audit_events ADD COLUMN oauth_client_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_oauth_client ON audit_events (oauth_client_id)
    WHERE oauth_client_id IS NOT NULL;

CREATE TRIGGER IF NOT EXISTS audit_events_oauth_client AFTER INSERT ON audit_events
WHEN NEW.oauth_client_id IS NULL AND NEW.agent_id IS NOT NULL
BEGIN
    UPDATE audit_events
       SET oauth_client_id = (SELECT g.client_id FROM mcp_oauth_grants g WHERE g.agent_id = NEW.agent_id)
     WHERE id = NEW.id AND EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.agent_id = NEW.agent_id);
END;
