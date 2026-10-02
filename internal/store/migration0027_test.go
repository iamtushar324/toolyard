package store

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// openAt opens a fresh database and applies every migration up to, but not
// including, stop: the shape an installed toolyard has before a release.
func openAt(t *testing.T, path, stop string) *sql.DB {
	t.Helper()
	sdb, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name >= stop {
			break
		}
		body, _ := migrations.ReadFile("migrations/" + name)
		if _, err := sdb.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := sdb.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, 1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	return sdb
}

// TestMigration0027AppliesToExistingDB: a database with servers, a shared
// token and a pending flow from before per-user sign-in upgrades in place.
// Existing servers come out as shared, existing pending flows as not
// per-user, and the new table cascades with both the server and the user.
func TestMigration0027AppliesToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old := openAt(t, path, "0027_per_user_oauth.sql")
	seed := []string{
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_1','ada','x',1,1)`,
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_2','bob','x',1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, created_at, updated_at) VALUES('linear','http','https://mcp.linear.app/mcp',1,1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, created_at, updated_at) VALUES('bk','http','https://bk.example/mcp',1,1,1)`,
		`INSERT INTO oauth_clients(upstream_name, issuer, authorization_endpoint, token_endpoint, client_id, redirect_uri, scopes, token_endpoint_auth_method, registered_at, updated_at)
            VALUES('linear','https://linear.app','https://linear.app/oauth/authorize','https://api.linear.app/oauth/token','cid','https://ty/cb','read','none',1,1)`,
		`INSERT INTO oauth_tokens(upstream_name, access_token_enc, state) VALUES('linear','sealed','active')`,
		`INSERT INTO oauth_pending(state, upstream_name, user_id, mode, expires_at, created_at) VALUES('st1','linear','u_1','callback',9999999999999,1)`,
	}
	for _, q := range seed {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open with 0027 pending: %v", err)
	}
	defer db.Close()

	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = '0027_per_user_oauth.sql'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("0027 recorded %d times (err %v)", applied, err)
	}
	var mode string
	if err := db.QueryRow(`SELECT auth_mode FROM upstream_servers WHERE name = 'linear'`).Scan(&mode); err != nil || mode != "shared" {
		t.Fatalf("existing server auth_mode = %q (err %v), want shared", mode, err)
	}
	var perUser, bound int
	if err := db.QueryRow(`SELECT per_user, browser_bound FROM oauth_pending WHERE state = 'st1'`).Scan(&perUser, &bound); err != nil || perUser != 0 || bound != 0 {
		t.Fatalf("existing pending per_user = %d browser_bound = %d (err %v), want 0 0", perUser, bound, err)
	}
	var label sql.NullString
	if err := db.QueryRow(`SELECT account_label FROM oauth_tokens WHERE upstream_name = 'linear'`).Scan(&label); err != nil || label.Valid {
		t.Fatalf("existing shared token account_label = %v (err %v), want NULL", label, err)
	}
	if _, err := db.Exec(`UPDATE upstream_servers SET auth_mode = 'bogus' WHERE name = 'bk'`); err == nil {
		t.Fatal("auth_mode accepted a value outside shared|per_user")
	}

	// New rows: one per (server, user); both foreign keys cascade.
	for _, q := range []string{
		`INSERT INTO oauth_user_tokens(upstream_name, user_id, access_token_enc, state) VALUES('linear','u_1','s1','active')`,
		`INSERT INTO oauth_user_tokens(upstream_name, user_id, access_token_enc, state) VALUES('linear','u_2','s2','active')`,
		`INSERT INTO oauth_user_tokens(upstream_name, user_id, access_token_enc, state) VALUES('bk','u_1','s3','active')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("insert user token: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO oauth_user_tokens(upstream_name, user_id, state) VALUES('linear','u_1','active')`); err == nil {
		t.Fatal("second row for the same (server, user) was accepted")
	}
	if _, err := db.Exec(`INSERT INTO oauth_user_tokens(upstream_name, user_id, state) VALUES('nope','u_1','active')`); err == nil {
		t.Fatal("row for an unknown server was accepted")
	}
	count := func(where string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM oauth_user_tokens WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = 'u_2'`); err != nil {
		t.Fatal(err)
	}
	if n := count(`user_id = 'u_2'`); n != 0 {
		t.Fatalf("deleting the user left %d token rows", n)
	}
	if _, err := db.Exec(`DELETE FROM upstream_servers WHERE name = 'linear'`); err != nil {
		t.Fatal(err)
	}
	if n := count(`upstream_name = 'linear'`); n != 0 {
		t.Fatalf("deleting the server left %d token rows", n)
	}
	if n := count(`upstream_name = 'bk' AND user_id = 'u_1'`); n != 1 {
		t.Fatalf("unrelated row count = %d, want 1", n)
	}
}
