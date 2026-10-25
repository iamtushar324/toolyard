-- 0057: the OAuth client id on audit rows (v0.1 slice C).
--
-- audit_events.oauth_client_id is the OAuth client (mcp_oauth_grants) whose
-- agent raised the row, set when the row is written by a trigger, so every
-- path that writes audit rows carries it without knowing about OAuth, and
-- the row keeps it after the agent or the grant changes. NULL for agents
-- that are not OAuth clients. An explicit value is kept.
--
-- Same column as migrations/sqlite/0057_audit_oauth_client.sql.

ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS oauth_client_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_oauth_client ON audit_events (oauth_client_id)
    WHERE oauth_client_id IS NOT NULL;

CREATE OR REPLACE FUNCTION audit_events_oauth_client() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.oauth_client_id IS NULL AND NEW.agent_id IS NOT NULL THEN
        NEW.oauth_client_id := (SELECT g.client_id FROM mcp_oauth_grants g WHERE g.agent_id = NEW.agent_id);
    END IF;
    RETURN NEW;
END;
$fn$;

DROP TRIGGER IF EXISTS audit_events_oauth_client ON audit_events;
CREATE TRIGGER audit_events_oauth_client BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_oauth_client();
