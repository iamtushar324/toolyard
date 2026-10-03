package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigration0031PreservesExistingRequestsAndSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := openAt(t, path, "0031_inbox_idempotency.sql")
	for _, q := range []string{
		`INSERT INTO inbox_requests(id, agent_id, kind, status, urgency, doc, created_at, updated_at, expires_at) VALUES('rq_old','ag_1','question','answered','soon','{"answer":"Keep it"}',1,2,3)`,
		`INSERT INTO agent_sessions(id, agent_id, title, status, started_at, last_heartbeat_at) VALUES('ses_old','ag_1','Original work','idle',1,2)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, doc string
	var key, hash sql.NullString
	if err := db.QueryRow(`SELECT status, doc, idempotency_key, submission_hash FROM inbox_requests WHERE id = 'rq_old'`).Scan(&status, &doc, &key, &hash); err != nil {
		t.Fatal(err)
	}
	if status != "answered" || doc != `{"answer":"Keep it"}` || key.Valid || hash.Valid {
		t.Fatalf("existing request changed: %q %q %+v %+v", status, doc, key, hash)
	}
	if err := db.QueryRow(`SELECT status, idempotency_key, submission_hash FROM agent_sessions WHERE id = 'ses_old'`).Scan(&status, &key, &hash); err != nil {
		t.Fatal(err)
	}
	if status != "idle" || key.Valid || hash.Valid {
		t.Fatalf("existing session changed: %q %+v %+v", status, key, hash)
	}
}
