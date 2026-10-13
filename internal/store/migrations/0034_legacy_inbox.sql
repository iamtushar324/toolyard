-- Preserve legacy execution expectations without re-dispatch after an uncertain crash.
CREATE TABLE legacy_execution_claims (
 request_id TEXT PRIMARY KEY, state TEXT NOT NULL,
 claimed_at INTEGER NOT NULL, finished_at INTEGER, reason TEXT NOT NULL DEFAULT ''
);
-- An old allowed row without a result may already have reached its upstream.
-- Migration cannot infer that it is safe to repeat.
INSERT INTO legacy_execution_claims(request_id,state,claimed_at,reason)
 SELECT id,'outcome_unknown',COALESCE(decided_at,created_at),'Historical dispatch has no durable receipt; do not repeat'
 FROM approval_requests WHERE status='allowed' AND result_executed_at IS NULL;

-- Snapshot the original owner before an agent can be deleted. An unknown
-- owner stays NULL; neither a later user nor another agent inherits that row.
CREATE TABLE legacy_inbox_owners (
 request_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL,
 owner_user_id TEXT, agent_name TEXT
);
CREATE INDEX legacy_inbox_owners_user ON legacy_inbox_owners(owner_user_id,agent_id);
INSERT INTO legacy_inbox_owners(request_id,agent_id,owner_user_id,agent_name)
SELECT r.id,COALESCE(r.agent_id,''),
 CASE WHEN COALESCE(r.agent_id,'')=''
      THEN (SELECT id FROM users ORDER BY created_at,id LIMIT 1)
      WHEN a.id IS NOT NULL THEN a.owner_user
      WHEN (SELECT count(DISTINCT owner_user_id) FROM audit_events
            WHERE approval_id=r.id AND owner_user_id IS NOT NULL AND owner_user_id<>'')=1
      THEN (SELECT min(owner_user_id) FROM audit_events
            WHERE approval_id=r.id AND owner_user_id IS NOT NULL AND owner_user_id<>'')
 END,a.name
FROM approval_requests r LEFT JOIN agents a ON a.id=r.agent_id;
