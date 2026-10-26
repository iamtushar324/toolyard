package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// migration0072 adds users.is_owner and its one-owner unique index.
const migration0072 = "0072_users_owner.sql"

// seed0072 is an install from before 0072: the password account that ran
// the setup (oldest), a Clerk admin whose email is the configured owner
// email, and a member.
func seed0072(t *testing.T, exec func(string, ...any) error) {
	t.Helper()
	for _, r := range []struct{ id, username, email, role string }{
		{"u_setup", "admin", "", "admin"},
		{"u_owner", "owner", "Owner@Example.com", "admin"},
		{"u_member", "member", "member@example.com", "member"},
	} {
		created := map[string]int{"u_setup": 1, "u_owner": 2, "u_member": 3}[r.id]
		if err := exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at, email, role, status)
			VALUES(?,?,'x',?,?,NULLIF(?,''),?,'active')`, r.id, r.username, created, created, r.email, r.role); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

// check0072: nobody is the owner straight after the migration, the
// startup backfill (identity.EnsureOwner) never trusts a stored email (a
// configured owner email leaves the role for that person's verified
// sign-in), picks the oldest active admin without one, and the index and
// CHECK allow one owner only.
func check0072(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE is_owner <> 0`).Scan(&owners); err != nil || owners != 0 {
		t.Fatalf("owners after 0072 = %d %v, want 0 (no SQL backfill)", owners, err)
	}
	id := identity.New(db)
	if _, err := id.EnsureOwner(ctx, "owner@example.com"); !errors.Is(err, identity.ErrNoUser) {
		t.Fatalf("backfill with an owner email: %v, want ErrNoUser (u_owner's stored email is not proof)", err)
	}
	u, err := id.EnsureOwner(ctx, "")
	if err != nil || u.ID != "u_setup" || !u.Owner || u.Role != identity.RoleAdmin {
		t.Fatalf("backfill without an owner email = %+v %v, want u_setup", u, err)
	}
	if _, err := db.Exec(`UPDATE users SET is_owner = 1 WHERE id = 'u_owner'`); err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("second owner: err %v, want a unique violation", err)
	}
	if _, err := db.Exec(`UPDATE users SET is_owner = 2 WHERE id = 'u_member'`); err == nil {
		t.Fatal("is_owner = 2 accepted: the CHECK is missing")
	}
	var actor string
	if err := db.QueryRow(`SELECT agent_id FROM audit_events WHERE event_type = 'user.owner'`).Scan(&actor); err != nil || actor != identity.ActorStartup {
		t.Fatalf("backfill audit actor = %q %v", actor, err)
	}
}

// TestMigration0072UsersOwner (SQLite).
func TestMigration0072UsersOwner(t *testing.T) {
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
		if name >= migration0072 {
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
	seed0072(t, func(q string, args ...any) error { _, err := sdb.Exec(q, args...); return err })
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open with 0072 pending: %v", err)
	}
	defer db.Close()
	check0072(t, db)
}

// TestMigration0072UsersOwnerPostgres: the same on Postgres.
func TestMigration0072UsersOwnerPostgres(t *testing.T) {
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
		if name != store.PostgresBaseline && name < migration0072 {
			apply(name)
		}
	}
	seed0072(t, func(q string, args ...any) error { _, err := db.ExecContext(ctx, q, args...); return err })
	apply(migration0072)
	check0072(t, db)
}
