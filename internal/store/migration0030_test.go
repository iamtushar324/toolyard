package store

import (
	"path/filepath"
	"testing"
)

// TestMigration0030AppliesToExistingDB: pending flows from before the
// opener mark come out as not opener-verified.
func TestMigration0030AppliesToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := openAt(t, path, "0030_connect_opener.sql")
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_1','ada','x',1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, created_at, updated_at) VALUES('linear','http','https://mcp.linear.app/mcp',1,1,1)`,
		`INSERT INTO oauth_pending(state, upstream_name, user_id, mode, expires_at, created_at, via_ticket) VALUES('st1','linear','u_1','callback',9999999999999,1,1)`,
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
		t.Fatalf("open with 0030 pending: %v", err)
	}
	defer db.Close()
	var via, opener int
	if err := db.QueryRow(`SELECT via_ticket, opener_verified FROM oauth_pending WHERE state = 'st1'`).Scan(&via, &opener); err != nil || via != 1 || opener != 0 {
		t.Fatalf("existing pending via_ticket = %d opener_verified = %d (err %v), want 1 0", via, opener, err)
	}
}
