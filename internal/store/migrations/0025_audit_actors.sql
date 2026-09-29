-- 0025: who raised each tool call and who decided it.
--
-- Raiser columns snapshot the caller at the time of the event (agent name,
-- owner, client, sessions, IP, path in); decider columns record the person
-- and the instrument (dashboard, push tap, passkey, Telegram, auto-rule,
-- policy, inbox grant, cancel, expiry). All additive.
ALTER TABLE audit_events ADD COLUMN agent_name TEXT;
ALTER TABLE audit_events ADD COLUMN agent_kind TEXT;
ALTER TABLE audit_events ADD COLUMN owner_user_id TEXT;
ALTER TABLE audit_events ADD COLUMN owner_email TEXT;
ALTER TABLE audit_events ADD COLUMN owner_name TEXT;
ALTER TABLE audit_events ADD COLUMN mcp_session_id TEXT;
ALTER TABLE audit_events ADD COLUMN agent_session_id TEXT;
ALTER TABLE audit_events ADD COLUMN client_session_id TEXT;
ALTER TABLE audit_events ADD COLUMN client_session_claimed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE audit_events ADD COLUMN client_kind TEXT;
ALTER TABLE audit_events ADD COLUMN client_name TEXT;
ALTER TABLE audit_events ADD COLUMN client_ip TEXT;
ALTER TABLE audit_events ADD COLUMN via TEXT;
ALTER TABLE audit_events ADD COLUMN decided_by_user_id TEXT;
ALTER TABLE audit_events ADD COLUMN decided_by_email TEXT;
ALTER TABLE audit_events ADD COLUMN decided_by_name TEXT;
ALTER TABLE audit_events ADD COLUMN decided_via TEXT;
ALTER TABLE audit_events ADD COLUMN decider_ref TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_owner_ts ON audit_events(owner_user_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_decider_ts ON audit_events(decided_by_user_id, ts DESC) WHERE decided_by_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_agent_session ON audit_events(agent_session_id) WHERE agent_session_id IS NOT NULL;

-- decided_by keeps its historical free-text value (user id, or via:ref);
-- these say how, and who by name, and snapshot the raiser for calls that
-- run after approval on a background context.
ALTER TABLE approval_requests ADD COLUMN decided_via TEXT;
ALTER TABLE approval_requests ADD COLUMN decider_email TEXT;
ALTER TABLE approval_requests ADD COLUMN decider_name TEXT;
ALTER TABLE approval_requests ADD COLUMN decider_ref TEXT;
ALTER TABLE approval_requests ADD COLUMN raised_by TEXT; -- JSON actor.Raiser

ALTER TABLE inbox_grants ADD COLUMN issued_by TEXT;  -- user id of the owner who approved
ALTER TABLE inbox_grants ADD COLUMN revoked_by TEXT;

ALTER TABLE auto_approval_rules ADD COLUMN created_by TEXT;
ALTER TABLE auto_approval_rules ADD COLUMN enabled_by TEXT;

ALTER TABLE call_events ADD COLUMN owner_user_id TEXT;
ALTER TABLE call_events ADD COLUMN client_kind TEXT;

-- Backfill what the agents table can still tell us about old rows.
UPDATE audit_events
   SET agent_name    = (SELECT a.name FROM agents a WHERE a.id = audit_events.agent_id),
       owner_user_id = (SELECT a.owner_user FROM agents a WHERE a.id = audit_events.agent_id),
       agent_kind    = COALESCE((SELECT a.kind FROM agents a WHERE a.id = audit_events.agent_id), 'agent')
 WHERE agent_id LIKE 'ag\_%' ESCAPE '\';
UPDATE audit_events
   SET owner_user_id = substr(agent_id, instr(agent_id, ':') + 1),
       agent_kind    = substr(agent_id, 1, instr(agent_id, ':') - 1)
 WHERE (agent_id LIKE 'dashboard:%' OR agent_id LIKE 'voice:%');
UPDATE audit_events
   SET owner_email = (SELECT u.email FROM users u WHERE u.id = audit_events.owner_user_id),
       owner_name  = (SELECT COALESCE(u.display_name, u.username) FROM users u WHERE u.id = audit_events.owner_user_id)
 WHERE owner_user_id IS NOT NULL;
-- Only the rows that record the decision (call.allowed / call.denied) name
-- the person who made it, and only when a person did (decided_by is a
-- user id, not rule:<id>, agent:<id>, telegram:<id> or token).
UPDATE audit_events
   SET decided_by_user_id = (SELECT r.decided_by FROM approval_requests r
                              WHERE r.id = audit_events.approval_id AND r.decided_by LIKE 'u\_%' ESCAPE '\')
 WHERE approval_id IS NOT NULL AND approval_id <> ''
   AND event_type IN ('call.allowed', 'call.denied');
