ALTER TABLE inbox_grants ADD COLUMN call_id TEXT NOT NULL DEFAULT '';
-- Atomic permission decisions, independent of transport audit publication.
CREATE TABLE inbox_decision_audit (
 id TEXT PRIMARY KEY,
 request_id TEXT NOT NULL,
 revision INTEGER NOT NULL,
 submission_id TEXT,
 actor_user_id TEXT,
 decision TEXT NOT NULL,
 recorded_at INTEGER NOT NULL,
 UNIQUE(request_id, revision)
);
-- A claim consumes one grant before dispatch. A stopped process cannot safely
-- infer whether an upstream write committed, so running claims become unknown.
CREATE TABLE inbox_executions (
 grant_id TEXT PRIMARY KEY,
 request_id TEXT NOT NULL,
 call_id TEXT NOT NULL,
 agent_id TEXT NOT NULL,
 state TEXT NOT NULL,
 attempt_id TEXT NOT NULL UNIQUE,
 arguments TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 finished_at INTEGER,
 result TEXT,
 error TEXT
);
CREATE INDEX inbox_executions_request ON inbox_executions(request_id);
-- Preserve existing Inbox permissions. Stable IDs are structural identifiers;
-- no missing agent explanation is invented during migration.
UPDATE inbox_requests SET doc=json_set(doc,
 '$.execution_mode', COALESCE(json_extract(doc,'$.execution_mode'),'agent'),
 '$.revision', COALESCE(json_extract(doc,'$.revision'),1),
 '$.tools', json(COALESCE((SELECT json_group_array(json_set(value,
   '$.call_id',COALESCE(json_extract(value,'$.call_id'),'call_'||(CAST(key AS INTEGER)+1)),
   '$.verdict',CASE json_extract(value,'$.decision') WHEN 'allowed' THEN 'accepted' WHEN 'refused' THEN 'rejected' ELSE '' END))
   FROM json_each(inbox_requests.doc,'$.tools')),'[]')))
WHERE kind='access';
UPDATE inbox_grants SET call_id=COALESCE((SELECT json_extract(doc,'$.tools['||inbox_grants.tool_index||'].call_id') FROM inbox_requests WHERE id=inbox_grants.request_id),'call_'||(tool_index+1));
-- Historical consumption never becomes another usable grant. Old versions did
-- not preserve dispatch outcomes here, so mark that uncertainty explicitly.
INSERT INTO inbox_executions(grant_id,request_id,call_id,agent_id,state,attempt_id,arguments,started_at,finished_at,error)
SELECT g.id,g.request_id,g.call_id,g.agent_id,'outcome_unknown','legacy_'||g.id,
 COALESCE((SELECT arguments FROM inbox_grant_uses u WHERE u.grant_id=g.id ORDER BY used_at DESC LIMIT 1),'{}'),
 COALESCE(g.last_used_at,g.created_at),COALESCE(g.last_used_at,g.created_at),
 'historical grant was consumed; this version has no authoritative dispatch outcome'
FROM inbox_grants g WHERE g.uses>0 OR g.status='used';
