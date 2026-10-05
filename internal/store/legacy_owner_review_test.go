package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestReviewMigration0034SnapshotsLegacyOwners(t *testing.T) {
	db := openAt(t, filepath.Join(t.TempDir(), "legacy.db"), "0034_legacy_inbox.sql")
	defer db.Close()
	for _, q := range []string{
		`INSERT INTO users(id,username,password_hash,created_at,updated_at) VALUES('u_primary','primary','x',1,1),('u_member','member','x',2,2)`,
		`INSERT INTO agents(id,owner_user,name,token_hash,created_at) VALUES('ag_live','u_member','Original name','hash',1)`,
		`INSERT INTO approval_requests(id,agent_id,upstream_name,tool_name,arguments,reason,status,created_at,expires_at) VALUES('rq_live','ag_live','up','write','{}','reason','pending',3,100),('rq_audit','ag_removed','up','write','{}','reason','pending',3,100),('rq_conflict','ag_removed','up','write','{}','reason','pending',3,100),('rq_unknown','ag_unknown','up','write','{}','reason','pending',3,100),('rq_anon','','up','write','{}','reason','pending',3,100)`,
		`INSERT INTO audit_events(id,ts,event_type,approval_id,owner_user_id) VALUES('ev_1',4,'call.start','rq_audit','u_member'),('ev_2',4,'call.start','rq_conflict','u_member'),('ev_3',4,'call.start','rq_conflict','u_primary')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	body, err := migrations.ReadFile("migrations/0034_legacy_inbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DELETE FROM agents WHERE id='ag_live'`); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"rq_live": "u_member", "rq_audit": "u_member", "rq_anon": "u_primary", "rq_conflict": "", "rq_unknown": ""} {
		var owner sql.NullString
		if err = db.QueryRow(`SELECT owner_user_id FROM legacy_inbox_owners WHERE request_id=?`, id).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner.String != want || owner.Valid != (want != "") {
			t.Fatalf("%s owner = %+v, want %q", id, owner, want)
		}
	}
	var name string
	if err = db.QueryRow(`SELECT agent_name FROM legacy_inbox_owners WHERE request_id='rq_live'`).Scan(&name); err != nil || name != "Original name" {
		t.Fatalf("original agent name lost: %q %v", name, err)
	}
}
