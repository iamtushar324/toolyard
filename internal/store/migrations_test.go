package store

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMigrationPairs: after the Postgres baseline, every SQLite migration
// has a Postgres twin with the same file name and the other way round, so
// both engines record the same names in schema_migrations.
func TestMigrationPairs(t *testing.T) {
	sqliteNames, err := migrationNames("migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	pgNames, err := migrationNames("migrations/postgres")
	if err != nil {
		t.Fatal(err)
	}
	var after []string
	for _, n := range sqliteNames {
		if n > pgBaselineCovers {
			after = append(after, n)
		}
	}
	var twins []string
	hasBaseline := false
	for _, n := range pgNames {
		if n == pgBaseline {
			hasBaseline = true
			continue
		}
		if n <= pgBaselineCovers {
			t.Errorf("Postgres migration %s sorts before the baseline's last SQLite migration %s", n, pgBaselineCovers)
		}
		twins = append(twins, n)
	}
	if !hasBaseline {
		t.Fatalf("migrations/postgres/%s is missing", pgBaseline)
	}
	sort.Strings(after)
	sort.Strings(twins)
	if strings.Join(after, "\n") != strings.Join(twins, "\n") {
		t.Fatalf("SQLite migrations after %s and Postgres migrations differ:\n sqlite:   %v\n postgres: %v", pgBaselineCovers, after, twins)
	}
}

// TestBaselineCoversEverySQLiteMigrationThrough0039: the baseline stands for
// the 41 SQLite files up to and including 0039_key_canaries.sql.
func TestBaselineCoversEverySQLiteMigrationThrough0039(t *testing.T) {
	covered, err := sqliteNamesThrough(pgBaselineCovers)
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 41 || covered[0] != "0001_init.sql" || covered[len(covered)-1] != pgBaselineCovers {
		t.Fatalf("baseline covers %d names (%v ... %v), want 41 from 0001_init.sql to %s", len(covered), covered[0], covered[len(covered)-1], pgBaselineCovers)
	}
}

// TestPostgresMigrationsHaveNoPlaceholders: migrations run through the
// placeholder-rewriting connector, so a ? outside strings and comments would
// be rewritten to $n and break the file.
func TestPostgresMigrationsHaveNoPlaceholders(t *testing.T) {
	names, err := migrationNames("migrations/postgres")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body, err := migrations.ReadFile("migrations/postgres/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if Rebind(string(body)) != string(body) {
			t.Errorf("%s holds a ? outside strings and comments", name)
		}
	}
}

// TestSQLiteMigrationsRecordUnixSeconds: applied_at is written from Go as
// Unix seconds (it was strftime('%s','now') before).
func TestSQLiteMigrationsRecordUnixSeconds(t *testing.T) {
	before := time.Now().Unix()
	db, err := Open(filepath.Join(t.TempDir(), "toolyard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Dialect != SQLite || db.Dialect.IsPostgres() || db.Path == "" {
		t.Fatalf("Open: Dialect %v, Path %q", db.Dialect, db.Path)
	}
	var n int
	var lo, hi int64
	if err := db.QueryRow(`SELECT COUNT(*), MIN(applied_at), MAX(applied_at) FROM schema_migrations`).Scan(&n, &lo, &hi); err != nil {
		t.Fatal(err)
	}
	names, err := migrationNames("migrations/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if n != len(names) {
		t.Fatalf("recorded %d migrations, want %d", n, len(names))
	}
	if lo < before-5 || hi > time.Now().Unix()+5 {
		t.Fatalf("applied_at between %d and %d, want Unix seconds near %d", lo, hi, before)
	}
}

func TestDialectSQL(t *testing.T) {
	if SQLite.String() != "sqlite" || Postgres.String() != "postgres" {
		t.Fatalf("String: %q %q", SQLite.String(), Postgres.String())
	}
	if SQLite.IsPostgres() || !Postgres.IsPostgres() {
		t.Fatal("IsPostgres")
	}
	if got := SQLite.JSONText("doc", "client_request_id"); got != "json_extract(doc, '$.client_request_id')" {
		t.Fatalf("SQLite JSONText = %q", got)
	}
	if got := Postgres.JSONText("r.doc", "status"); got != `(replace(r.doc, '\u0000', '\ufffd')::jsonb ->> 'status')` {
		t.Fatalf("Postgres JSONText = %q", got)
	}
	if SQLite.ILike() != "LIKE" || Postgres.ILike() != "ILIKE" {
		t.Fatal("ILike")
	}
	if SQLite.ForUpdate() != "" || Postgres.ForUpdate() != " FOR UPDATE" {
		t.Fatal("ForUpdate")
	}
	var zero Dialect
	if zero != SQLite {
		t.Fatal("the zero Dialect must be SQLite")
	}
}

func TestJSONTextRejectsNonIdentifierKeys(t *testing.T) {
	for _, key := range []string{"", "a.b", "a'b", "a b", "$.x", "k-1"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("JSONText accepted key %q", key)
				}
			}()
			_ = SQLite.JSONText("doc", key)
		}()
	}
}

func TestIsPostgresURL(t *testing.T) {
	for url, want := range map[string]bool{
		"postgres://u:p@h:5432/db":          true,
		"postgresql://h/db?sslmode=require": true,
		" POSTGRES://h/db ":                 true,
		"/var/lib/toolyard/toolyard.db":     false,
		"file:toolyard.db":                  false,
		"":                                  false,
	} {
		if got := IsPostgresURL(url); got != want {
			t.Errorf("IsPostgresURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestPostgresTargetNeverReturnsTheSecret(t *testing.T) {
	host, database, err := PostgresTarget("postgres://toolyard:s3cret-pw@db.example.invalid:5432/toolyard?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	if host != "db.example.invalid" || database != "toolyard" {
		t.Fatalf("PostgresTarget = %q %q", host, database)
	}
	if _, _, err := PostgresTarget("postgres://toolyard:s3cret-pw@[bad"); err == nil || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("a bad URL gave error %v; want an error that does not quote the password", err)
	}
}

// TestPostgresTargetRefusesMalformedURLs: every URL pgx would reject, or
// would read with part of the password in the host name, is an ErrConfig
// error that quotes nothing; an upper-case scheme is accepted.
func TestPostgresTargetRefusesMalformedURLs(t *testing.T) {
	for name, url := range map[string]string{
		"unescaped @ in the password": "postgres://toolyard:s3cret@pw@db.example.invalid:5432/toolyard",
		"unescaped / in the password": "postgres://toolyard:s3cret/pw@db.example.invalid:5432/toolyard",
		"bad port":                    "postgres://toolyard:s3cret-pw@db.example.invalid:54x2/toolyard",
		"not a postgres URL":          "mysql://toolyard:s3cret-pw@db.example.invalid/toolyard",
	} {
		_, _, err := PostgresTarget(url)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err %v, want ErrConfig", name, err)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "db.example.invalid") {
			t.Errorf("%s: the error quotes the URL: %v", name, err)
		}
	}
	host, database, err := PostgresTarget(" POSTGRES://toolyard:s3cret-pw@db.example.invalid:5432/toolyard ")
	if err != nil || host != "db.example.invalid" || database != "toolyard" {
		t.Fatalf("upper-case scheme: %q %q %v", host, database, err)
	}
	if _, _, err := PostgresTarget("PostgreSQL://toolyard:pw%40x@db.example.invalid/toolyard"); err != nil {
		t.Fatalf("mixed-case postgresql:// with an escaped @: %v", err)
	}
}
