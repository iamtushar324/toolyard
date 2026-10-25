package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// migration0062 stores each push receiver's destination host.
const migration0062 = "0062_callback_receiver_host.sql"

// hosts0062 are receivers before 0062 and the host 0062 must derive (the
// value callbacks.destinationHost gives).
var hosts0062 = []struct{ id, destination, transport, host string }{
	{"cb_query", "https://Hooks.Example.com/a?x=1", "push", "hooks.example.com"},
	{"cb_port", "https://receiver.example:443/hook", "push", "receiver.example"},
	{"cb_v6", "https://[2001:DB8::1]/hook", "push", "2001:db8::1"},
	{"cb_bare", "https://q.example?only", "push", "q.example"},
	{"cb_plain", "https://plain.example", "push", "plain.example"},
	{"cb_pull", "", "pull", ""},
}

func seed0062(t *testing.T, exec func(string, ...any) error) {
	t.Helper()
	for _, r := range hosts0062 {
		if err := exec(`INSERT INTO callback_receivers(id,agent_id,environment_id,client_receiver_id,destination,secret_enc,created_at,transport) VALUES(?,'ag_1','',?,?,'enc',1,?)`, r.id, r.id, r.destination, r.transport); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

func check0062(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, r := range hosts0062 {
		var host string
		if err := db.QueryRow(`SELECT host FROM callback_receivers WHERE id=?`, r.id).Scan(&host); err != nil || host != r.host {
			t.Errorf("%s (%q): host %q %v, want %q", r.id, r.destination, host, err, r.host)
		}
	}
}

// TestMigration0062DerivesReceiverHosts (SQLite).
func TestMigration0062DerivesReceiverHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	sdb, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetMaxOpenConns(1)
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := store.MigrationFilesIn("sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= migration0062 {
			break
		}
		body, err := store.MigrationFile("sqlite", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sdb.Exec(body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := sdb.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, 1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	seed0062(t, func(q string, args ...any) error { _, err := sdb.Exec(q, args...); return err })
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open with 0062 pending: %v", err)
	}
	defer db.Close()
	check0062(t, db.DB)
}

// TestMigration0062DerivesReceiverHostsPostgres: the same on Postgres.
func TestMigration0062DerivesReceiverHostsPostgres(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 2, NoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := store.MigrationFilesIn("postgres")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(name string) {
		t.Helper()
		body, err := store.MigrationFile("postgres", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, body); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	apply(store.PostgresBaseline)
	for _, name := range names {
		if name != store.PostgresBaseline && name < migration0062 {
			apply(name)
		}
	}
	seed0062(t, func(q string, args ...any) error { _, err := db.ExecContext(ctx, q, args...); return err })
	apply(migration0062)
	check0062(t, db.DB)
}
