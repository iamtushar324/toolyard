package store_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// These tests run on the engine the CI job selects (TOOLYARD_TEST_DB).

func TestOpenSetsTheEngine(t *testing.T) {
	db := storetest.Open(t)
	if db.Dialect != storetest.Engine() {
		t.Fatalf("Dialect = %v, want %v", db.Dialect, storetest.Engine())
	}
	if db.Dialect.IsPostgres() != (db.Path == "") {
		t.Fatalf("Dialect %v with Path %q", db.Dialect, db.Path)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	insertUser := `INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(?, ?, 'x', 1, 1)`
	if _, err := db.ExecContext(ctx, insertUser, "u_1", "ada"); err != nil {
		t.Fatal(err)
	}
	// UNIQUE(username)
	_, err := db.ExecContext(ctx, insertUser, "u_2", "ada")
	if err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate username: err %v, IsUniqueViolation false", err)
	}
	if !store.IsUniqueViolation(fmt.Errorf("create user: %w", err)) {
		t.Fatal("IsUniqueViolation does not unwrap")
	}
	// PRIMARY KEY(id)
	if _, err := db.ExecContext(ctx, insertUser, "u_1", "bob"); err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate primary key: err %v, IsUniqueViolation false", err)
	}
	// Composite primary key.
	setting := `INSERT INTO memory_entries(scope, key, value, updated_at) VALUES('s', 'k', ?, 1)`
	if _, err := db.ExecContext(ctx, setting, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, setting, "v2"); err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate composite key: err %v, IsUniqueViolation false", err)
	}
	// Partial unique index.
	insertAgent := `INSERT INTO agents(id, name, owner_user, token_hash, enroll_code, created_at) VALUES(?, 'n', 'u_1', 'h', ?, 1)`
	if _, err := db.ExecContext(ctx, insertAgent, "ag_1", "code"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insertAgent, "ag_2", "code"); err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate in a partial unique index: err %v, IsUniqueViolation false", err)
	}
	// Other errors are not unique violations.
	if _, err := db.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_3', NULL, 'x', 1, 1)`); err == nil || store.IsUniqueViolation(err) {
		t.Fatalf("NOT NULL violation: err %v, want an error that is not a unique violation", err)
	}
	if store.IsUniqueViolation(nil) || store.IsUniqueViolation(errors.New("UNIQUE constraint failed: users.username")) {
		t.Fatal("IsUniqueViolation matched nil or error text")
	}
}

func TestTableExists(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	for table, want := range map[string]bool{
		"users":             true,
		"schema_migrations": true,
		"key_canaries":      true,
		"policies":          false, // dropped by 0013
		"lake_saved_queries": func() bool {
			return !db.Dialect.IsPostgres() // dormant: SQLite still has it, the Postgres baseline leaves it out
		}(),
		"no_such_table": false,
	} {
		got, err := store.TableExists(ctx, db, db.Dialect, table)
		if err != nil {
			t.Fatalf("TableExists(%s): %v", table, err)
		}
		if got != want {
			t.Errorf("TableExists(%s) = %v, want %v", table, got, want)
		}
	}
	// Works through a transaction too (keyguard reads inside one).
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if ok, err := store.TableExists(ctx, tx, db.Dialect, "users"); err != nil || !ok {
		t.Fatalf("TableExists in a transaction = %v, %v", ok, err)
	}
}

func TestJSONTextQueriesTheDocument(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	insert := `INSERT INTO inbox_requests(id, agent_id, kind, status, urgency, doc, created_at, updated_at, expires_at)
		VALUES(?, 'ag', 'question', 'pending', 'soon', ?, 1, 1, 9)`
	for id, doc := range map[string]string{
		"rq_1": `{"id":"rq_1","client_request_id":"c-1","status":"pending"}`,
		"rq_2": `{"id":"rq_2","status":"answered"}`,
	} {
		if _, err := db.ExecContext(ctx, insert, id, doc); err != nil {
			t.Fatal(err)
		}
	}
	var id string
	q := `SELECT id FROM inbox_requests WHERE agent_id = ? AND ` + db.Dialect.JSONText("doc", "client_request_id") + ` = ?`
	if err := db.QueryRowContext(ctx, q, "ag", "c-1").Scan(&id); err != nil || id != "rq_1" {
		t.Fatalf("JSONText lookup: %q %v", id, err)
	}
	var n int
	q = `SELECT COUNT(*) FROM inbox_requests WHERE ` + db.Dialect.JSONText("doc", "client_request_id") + ` IS NULL`
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != 1 {
		t.Fatalf("missing key reads as NULL: %d %v", n, err)
	}
	// The expression index from 0031 keeps a client_request_id unique per agent.
	_, err := db.ExecContext(ctx, insert, "rq_3", `{"id":"rq_3","client_request_id":"c-1"}`)
	if err == nil || !store.IsUniqueViolation(err) {
		t.Fatalf("second request with the same client_request_id: %v", err)
	}
}

// TestPlaceholdersInsideLiteralsSurvive: the driver rewrites only real
// placeholders, on both engines.
func TestPlaceholdersInsideLiteralsSurvive(t *testing.T) {
	db := storetest.Open(t)
	var a, b string
	if err := db.QueryRowContext(context.Background(), `SELECT 'what?' /* why? */, CAST(? AS TEXT) -- really?
		`, "yes").Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != "what?" || b != "yes" {
		t.Fatalf("got %q %q", a, b)
	}
}

func TestWithPgxConn(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	err := db.WithPgxConn(ctx, func(c *pgx.Conn) error {
		_, err := c.CopyFrom(ctx, pgx.Identifier{"system_settings"}, []string{"key", "value", "updated_at"},
			pgx.CopyFromRows([][]any{{"copy_a", `"1"`, int64(1)}, {"copy_b", `"2"`, int64(2)}}))
		return err
	})
	if !db.Dialect.IsPostgres() {
		if err == nil {
			t.Fatal("WithPgxConn ran on SQLite")
		}
		return
	}
	if err != nil {
		t.Fatalf("COPY through WithPgxConn: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM system_settings WHERE key LIKE 'copy_%'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("copied rows: %d %v", n, err)
	}
}

// TestPostgresMigrateIsIdempotent: running the migrations again, also from
// two goroutines at once, changes nothing, and Postgres records the same
// migration names as SQLite.
func TestPostgresMigrateIsIdempotent(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	db := storetest.Open(t)
	before := migrationRows(t, db)

	if err := store.MigratePostgresAgain(ctx, db); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.MigratePostgresAgain(ctx, db)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate %d: %v", i, err)
		}
	}
	if after := migrationRows(t, db); after != before {
		t.Fatalf("schema_migrations changed:\nbefore %s\nafter  %s", before, after)
	}

	want, err := store.SQLiteMigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Postgres recorded %v\nwant the SQLite names %v", got, want)
	}
}

func migrationRows(t *testing.T, db *store.DB) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT name, applied_at FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s=%d;", name, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestOpenPostgresReadOnly: keys-check's open reads, refuses every write,
// and does not migrate.
func TestOpenPostgresReadOnly(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dbURL := storetest.NewPostgresDatabase(t)
	ro, err := store.OpenPostgresReadOnly(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Dialect != store.Postgres || ro.Path != "" {
		t.Fatalf("Dialect %v Path %q", ro.Dialect, ro.Path)
	}
	var n int
	if err := ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("read: %d %v", n, err)
	}
	if _, err := ro.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES(?, '1', 1)`, "written"); err == nil {
		t.Fatal("a read-only session accepted an INSERT")
	}
	if _, err := ro.ExecContext(ctx, `CREATE TABLE ro_probe (v TEXT)`); err == nil {
		t.Fatal("a read-only session accepted a schema change")
	}
	if _, err := store.OpenPostgresReadOnly(ctx, "postgres://u:s3cret-pw@[bad"); err == nil || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("bad URL: %v", err)
	}
}
