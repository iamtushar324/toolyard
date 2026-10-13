package store

import (
	"path/filepath"
	"testing"
)

func TestMigration0032PreservesExistingInboxPermission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")
	old := openAt(t, path, "0032_inbox_batch_execution.sql")
	seeds := []string{
		`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES('rq_old','agent_owner','access','approved','soon','{"id":"rq_old","agent_id":"agent_owner","kind":"access","status":"approved","expires_at":123456,"tools":[{"tool":"files.write","decision":"allowed","params":{"path":{"eq":"one"}}},{"tool":"files.write","decision":"refused","params":{"path":{"eq":"two"}}}]}',1,2,123456)`,
		`INSERT INTO inbox_grants(id,request_id,tool_index,agent_id,tool,params,max_uses,uses,status,token_hash,created_at,expires_at,last_used_at) VALUES('gr_used','rq_old',0,'agent_owner','files.write','{"path":{"eq":"one"}}',1,1,'used','original_hash',1,555,9)`,
		`INSERT INTO inbox_grants(id,request_id,tool_index,agent_id,tool,params,max_uses,uses,status,token_hash,created_at,expires_at) VALUES('gr_unused','rq_old',0,'agent_owner','files.write','{"path":{"eq":"one"}}',1,0,'active','unused_hash',1,555)`,
		`INSERT INTO inbox_grant_uses(id,grant_id,arguments,used_at) VALUES('use_original','gr_used','{"path":"one"}',9)`,
	}
	for _, q := range seeds {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0032_inbox_batch_execution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	var owner, first, second, verdict, mode string
	var expiry, revision int
	err = old.QueryRow(`SELECT agent_id,expires_at,json_extract(doc,'$.tools[0].call_id'),json_extract(doc,'$.tools[1].call_id'),json_extract(doc,'$.tools[1].verdict'),json_extract(doc,'$.execution_mode'),json_extract(doc,'$.revision') FROM inbox_requests WHERE id='rq_old'`).Scan(&owner, &expiry, &first, &second, &verdict, &mode, &revision)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "agent_owner" || expiry != 123456 || first != "call_1" || second != "call_2" || verdict != "rejected" || mode != "agent" || revision != 1 {
		t.Fatalf("changed preserved request: %s %d %s %s %s %s %d", owner, expiry, first, second, verdict, mode, revision)
	}
	var state, args, hash, callID string
	var uses, deadline int
	err = old.QueryRow(`SELECT g.call_id,g.token_hash,g.uses,g.expires_at,e.state,e.arguments FROM inbox_grants g JOIN inbox_executions e ON e.grant_id=g.id WHERE g.id='gr_used'`).Scan(&callID, &hash, &uses, &deadline, &state, &args)
	if err != nil {
		t.Fatal(err)
	}
	if callID != "call_1" || hash != "original_hash" || uses != 1 || deadline != 555 || state != "outcome_unknown" || args != "{\"path\":\"one\"}" {
		t.Fatalf("permission changed: %s %s %d %d %s %s", callID, hash, uses, deadline, state, args)
	}
	var count int
	if err = old.QueryRow(`SELECT count(*) FROM inbox_executions WHERE grant_id='gr_unused'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unused grant got an execution: %d %v", count, err)
	}
	if err = old.QueryRow(`SELECT count(*) FROM inbox_grant_uses WHERE id='use_original'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("history lost: %d %v", count, err)
	}
	old.Close()
}
