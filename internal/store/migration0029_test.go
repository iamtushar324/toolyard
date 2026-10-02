package store

import (
	"path/filepath"
	"testing"
)

// TestMigration0029AppliesToExistingDB: a database with users, servers and
// a pending flow from before connect links upgrades in place. Existing
// pending flows come out as not ticket-started, and the new table cascades
// with both the server and the user and refuses a bad purpose.
func TestMigration0029AppliesToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := openAt(t, path, "0029_connect_tickets.sql")
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_1','ada','x',1,1)`,
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_2','bob','x',1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, created_at, updated_at) VALUES('linear','http','https://mcp.linear.app/mcp',1,1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, created_at, updated_at) VALUES('bk','http','https://bk.example/mcp',1,1,1)`,
		`INSERT INTO oauth_pending(state, upstream_name, user_id, mode, expires_at, created_at) VALUES('st1','linear','u_1','callback',9999999999999,1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open with 0029 pending: %v", err)
	}
	defer db.Close()

	var via int
	if err := db.QueryRow(`SELECT via_ticket FROM oauth_pending WHERE state = 'st1'`).Scan(&via); err != nil || via != 0 {
		t.Fatalf("existing pending via_ticket = %d (err %v), want 0", via, err)
	}
	for _, q := range []string{
		`INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, created_at, expires_at) VALUES('h1','u_1','linear','per_user',1,2)`,
		`INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, agent_id, created_at, expires_at) VALUES('h2','u_2','linear','shared','ag_1',1,2)`,
		`INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, created_at, expires_at) VALUES('h3','u_1','bk','per_user',1,2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("insert ticket: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, created_at, expires_at) VALUES('h4','u_1','linear','bogus',1,2)`); err == nil {
		t.Fatal("purpose outside per_user|shared accepted")
	}
	if _, err := db.Exec(`INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, created_at, expires_at) VALUES('h5','u_1','nope','shared',1,2)`); err == nil {
		t.Fatal("ticket for an unknown server accepted")
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM connect_tickets`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = 'u_2'`); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("tickets after deleting a user = %d, want 2", n)
	}
	if _, err := db.Exec(`DELETE FROM upstream_servers WHERE name = 'linear'`); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("tickets after deleting a server = %d, want 1", n)
	}
}
