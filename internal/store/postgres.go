package store

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// DefaultPostgresMaxConns is the pool size OpenPostgres uses when it is given
// none.
const DefaultPostgresMaxConns = 10

// IsPostgresURL reports whether url names a Postgres database
// (postgres:// or postgresql://, the scheme in any letter case).
func IsPostgresURL(url string) bool {
	u := strings.ToLower(strings.TrimSpace(url))
	return strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://")
}

// PostgresTarget returns the host and database name of a Postgres URL, for
// logs. It never returns the user or the password. Its error matches
// ErrConfig and never quotes the URL.
func PostgresTarget(url string) (host, database string, err error) {
	cfg, err := parsePostgresURL(url)
	if err != nil {
		return "", "", err
	}
	return cfg.Host, cfg.Database, nil
}

// ErrConfig marks a database setting a restart cannot fix: a Postgres URL
// that cannot be parsed, or a database whose collation is not C. The
// gateway maps it to exit code 78.
var ErrConfig = errors.New("store: database configuration error")

// configError is an error matching ErrConfig. Its message is fixed text:
// never the URL or the password.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

func (e *configError) Is(target error) bool { return target == ErrConfig }

// ErrConnect marks a failure to reach the Postgres server or to open a
// session on it when a database is opened that a retry may cure: a refused,
// reset or timed-out connection, a host name that does not resolve, or a
// server error of class 08 (connection exception) or 53 (insufficient
// resources, such as too many connections) or 57P01-57P03 (the server is
// shutting down, crashed or starting up, as during an Aurora failover). A
// caller may retry it (serve does). It never marks an ErrConfig error, any
// other server error (a refused login, a database that does not exist, no
// CONNECT privilege, a bad parameter) or a TLS certificate that does not
// verify: a retry cannot fix those.
var ErrConnect = errors.New("store: cannot connect to the database")

// connectError is an error matching ErrConnect. Its message is the wrapped
// error's, which never quotes the URL.
type connectError struct{ err error }

func (e *connectError) Error() string { return e.err.Error() }

func (e *connectError) Unwrap() error { return e.err }

func (e *connectError) Is(target error) bool { return target == ErrConnect }

// connectFailure marks err, a failure while opening a session, as
// ErrConnect, unless a retry cannot fix it (see ErrConnect).
func connectFailure(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && !retryableServerError(pe.Code) {
		return err
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return err
	}
	return &connectError{err: err}
}

// retryableServerError reports whether a retry may cure a server error
// (SQLSTATE code) met while opening a session.
func retryableServerError(code string) bool {
	if strings.HasPrefix(code, "08") || strings.HasPrefix(code, "53") {
		return true
	}
	switch code {
	case "57P01", "57P02", "57P03": // admin shutdown, crash shutdown, cannot connect now
		return true
	}
	return false
}

// errBadPostgresURL never quotes the URL: it holds the password.
var errBadPostgresURL error = &configError{msg: "store: the database URL cannot be parsed; the form is postgres://user:password@host:5432/database?sslmode=require " +
	"(percent-encode @ / : # ? and % in the password)"}

// errAtInHost is a URL whose password holds an unescaped @: the parser
// takes the first @ as the end of the password, so the rest of the password
// would become part of the host name (and reach the logs).
var errAtInHost error = &configError{msg: "store: the database URL does not parse into user, password, host and database; percent-encode @ (%40), / (%2F), : (%3A) and # (%23) in the password"}

func parsePostgresURL(url string) (*pgx.ConnConfig, error) {
	url = strings.TrimSpace(url)
	if !IsPostgresURL(url) {
		return nil, &configError{msg: "store: a Postgres database URL starts with postgres:// or postgresql://"}
	}
	// URL schemes are case-insensitive, but pgx only knows the lower-case
	// spelling: POSTGRES:// would otherwise fail to parse.
	if i := strings.Index(url, "://"); i > 0 {
		url = strings.ToLower(url[:i]) + url[i:]
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		// pgx redacts the password in its message only as best it can.
		return nil, errBadPostgresURL
	}
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	for _, h := range hosts {
		if strings.Contains(h, "@") {
			return nil, errAtInHost
		}
	}
	// A '/' in the password ends the host part early: the rest, '@' and
	// all, becomes the database name, which the logs show.
	if strings.Contains(cfg.Database, "@") {
		return nil, errAtInHost
	}
	return cfg, nil
}

// PostgresOptions tune OpenPostgresWith.
type PostgresOptions struct {
	// MaxConns is the pool size; <= 0 means DefaultPostgresMaxConns.
	MaxConns int
	// ReadOnly makes every session read-only
	// (default_transaction_read_only=on) and applies no migrations.
	ReadOnly bool
	// NoMigrate applies no migrations: the database is used as it is. A
	// caller that has checked the database may then call MigratePostgres.
	NoMigrate bool
}

// OpenPostgres opens url (postgres:// or postgresql://) through the
// placeholder-rewriting pgx connector, so the ? placeholders of toolyard's
// SQL work unchanged. It sets MaxOpenConns(maxConns) (maxConns <= 0 means
// DefaultPostgresMaxConns), pings, checks the collation, and applies pending
// migrations under pg_advisory_lock. It never logs the URL or the password,
// and its errors do not quote the URL.
func OpenPostgres(ctx context.Context, url string, maxConns int) (*DB, error) {
	return OpenPostgresWith(ctx, url, PostgresOptions{MaxConns: maxConns})
}

// OpenPostgresReadOnly opens url for checks that must change nothing
// (toolyard keys-check): one connection whose session has
// default_transaction_read_only=on, and no migrations.
func OpenPostgresReadOnly(ctx context.Context, url string) (*DB, error) {
	return OpenPostgresWith(ctx, url, PostgresOptions{MaxConns: 1, ReadOnly: true})
}

// OpenPostgresWith opens url as OpenPostgres does, tuned by opts. Every open
// checks that the database collation is C (an error matching ErrConfig
// otherwise); only a read-write open without NoMigrate migrates.
func OpenPostgresWith(ctx context.Context, url string, opts PostgresOptions) (*DB, error) {
	cfg, err := parsePostgresURL(url)
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "toolyard"
	}
	if opts.ReadOnly {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = DefaultPostgresMaxConns
	}
	sdb := sql.OpenDB(&rebindConnector{inner: stdlib.GetConnector(*cfg)})
	sdb.SetMaxOpenConns(maxConns)
	sdb.SetMaxIdleConns(maxConns)
	sdb.SetConnMaxIdleTime(5 * time.Minute)
	sdb.SetConnMaxLifetime(30 * time.Minute)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, connectFailure(fmt.Errorf("store: connect to Postgres at %s/%s: %w", cfg.Host, cfg.Database, err))
	}
	if err := checkCollation(ctx, sdb); err != nil {
		_ = sdb.Close()
		if !errors.Is(err, ErrConfig) {
			// The session ended between the ping and this query.
			err = connectFailure(err)
		}
		return nil, err
	}
	db := &DB{DB: sdb, Dialect: Postgres, pgConfig: cfg}
	if opts.ReadOnly || opts.NoMigrate {
		return db, nil
	}
	if err := db.migratePostgres(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// MigratePostgres applies the pending Postgres migrations, as OpenPostgres
// does, for a database opened with NoMigrate.
func (db *DB) MigratePostgres(ctx context.Context) error {
	if !db.Dialect.IsPostgres() {
		return errors.New("store: MigratePostgres needs a Postgres database")
	}
	if err := db.migratePostgres(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// checkCollation refuses a database whose text does not sort and compare
// byte by byte, as SQLite's does: datcollate and datctype must be C or
// POSIX, and the locale provider must not be ICU. Aurora creates databases
// with en_US.UTF-8 unless told otherwise. datlocprovider exists from
// Postgres 15 on; to_jsonb reads it as NULL on older servers.
func checkCollation(ctx context.Context, q Querier) error {
	var collate, ctype, provider, encoding string
	err := q.QueryRowContext(ctx, `SELECT datcollate::text, datctype::text, COALESCE(to_jsonb(d) ->> 'datlocprovider', 'c'), pg_encoding_to_char(encoding)::text
		FROM pg_database d WHERE datname = current_database()`).Scan(&collate, &ctype, &provider, &encoding)
	if err != nil {
		return fmt.Errorf("store: read the database collation: %w", err)
	}
	if encoding != "UTF8" {
		// jsonb in SQL_ASCII refuses every \u escape above U+007F, and
		// text must hold any UTF-8 a client sends.
		return &configError{msg: fmt.Sprintf("store: the database encoding is %q; toolyard needs UTF8. Create it with: "+
			"CREATE DATABASE <name> TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'", encoding)}
	}
	if cLocale(collate) && cLocale(ctype) && provider != "i" {
		return nil
	}
	return &configError{msg: fmt.Sprintf("store: the database has collation %q, character type %q and locale provider %q; toolyard needs C (or POSIX) "+
		"for both, and not the ICU provider, so text sorts and compares as on SQLite. Create it with: "+
		"CREATE DATABASE <name> TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'", collate, ctype, provider)}
}

func cLocale(s string) bool { return s == "C" || s == "POSIX" }

// WithPgxConn runs fn on one raw *pgx.Conn from the pool, for COPY FROM and
// other pgx-only features. fn must not keep the connection after it returns.
// SQL sent through the *pgx.Conn is not rewritten: use $1, $2 there. It is
// an error on SQLite.
func (db *DB) WithPgxConn(ctx context.Context, fn func(*pgx.Conn) error) error {
	if !db.Dialect.IsPostgres() {
		return errors.New("store: WithPgxConn needs a Postgres database")
	}
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		rc, ok := driverConn.(*rebindConn)
		if !ok {
			return fmt.Errorf("store: unexpected driver connection %T", driverConn)
		}
		return fn(rc.pg.Conn())
	})
}

// ConnectPostgres opens one new connection to the database, outside the
// pool and not counted in its size, for a caller that must keep a session
// of its own for a long time: session advisory locks and LISTEN
// (internal/cluster). The caller closes it. SQL sent through it is not
// rewritten: use $1, $2 there. It is an error on SQLite.
func (db *DB) ConnectPostgres(ctx context.Context) (*pgx.Conn, error) {
	if !db.Dialect.IsPostgres() || db.pgConfig == nil {
		return nil, errors.New("store: ConnectPostgres needs a database opened with OpenPostgres")
	}
	conn, err := pgx.ConnectConfig(ctx, db.pgConfig.Copy())
	if err != nil {
		return nil, connectFailure(fmt.Errorf("store: connect to Postgres at %s/%s: %w", db.pgConfig.Host, db.pgConfig.Database, err))
	}
	return conn, nil
}

// ---- placeholder-rewriting connector ----------------------------------------

// rebindConnector wraps pgx's database/sql connector so that every
// connection rewrites ? placeholders to $n (Rebind).
type rebindConnector struct {
	inner driver.Connector
}

func (c *rebindConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pg, ok := conn.(*stdlib.Conn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("store: unexpected pgx driver connection %T", conn)
	}
	return &rebindConn{pg: pg}, nil
}

func (c *rebindConnector) Driver() driver.Driver { return c.inner.Driver() }

// rebindConn is a *stdlib.Conn that rewrites the query text of Prepare,
// Exec and Query, and forwards every optional interface stdlib.Conn
// implements, so database/sql treats it exactly like the pgx connection.
type rebindConn struct {
	pg *stdlib.Conn
}

var (
	_ driver.Connector          = (*rebindConnector)(nil)
	_ driver.Conn               = (*rebindConn)(nil)
	_ driver.ConnPrepareContext = (*rebindConn)(nil)
	_ driver.ConnBeginTx        = (*rebindConn)(nil)
	_ driver.ExecerContext      = (*rebindConn)(nil)
	_ driver.QueryerContext     = (*rebindConn)(nil)
	_ driver.Pinger             = (*rebindConn)(nil)
	_ driver.SessionResetter    = (*rebindConn)(nil)
	_ driver.Validator          = (*rebindConn)(nil)
	_ driver.NamedValueChecker  = (*rebindConn)(nil)
)

func (c *rebindConn) Prepare(query string) (driver.Stmt, error) {
	return c.pg.Prepare(Rebind(query))
}

func (c *rebindConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.pg.PrepareContext(ctx, Rebind(query))
}

func (c *rebindConn) Close() error { return c.pg.Close() }

// Begin is required by driver.Conn; database/sql calls BeginTx.
func (c *rebindConn) Begin() (driver.Tx, error) { return c.pg.Begin() }

func (c *rebindConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.pg.BeginTx(ctx, opts)
}

func (c *rebindConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.pg.ExecContext(ctx, Rebind(query), args)
}

func (c *rebindConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.pg.QueryContext(ctx, Rebind(query), args)
}

func (c *rebindConn) Ping(ctx context.Context) error { return c.pg.Ping(ctx) }

func (c *rebindConn) CheckNamedValue(nv *driver.NamedValue) error { return c.pg.CheckNamedValue(nv) }

func (c *rebindConn) ResetSession(ctx context.Context) error { return c.pg.ResetSession(ctx) }

// IsValid lets database/sql drop a connection pgx has already closed (for
// example after a network error) instead of returning it to the pool.
func (c *rebindConn) IsValid() bool { return !c.pg.Conn().IsClosed() }

// ---- migrations --------------------------------------------------------------

const (
	// pgMigrationLock is the pg_advisory_lock key held while migrating:
	// "toolyard" in ASCII.
	pgMigrationLock int64 = 0x746f6f6c79617264
	// pgBaseline is the Postgres file that creates the whole schema as of
	// pgBaselineCovers, the last SQLite migration it stands for.
	pgBaseline       = "0039_baseline.sql"
	pgBaselineCovers = "0039_key_canaries.sql"
)

// migratePostgres applies the Postgres migrations on one connection that
// holds pg_advisory_lock for the whole run, so two processes starting at
// once do not both migrate. An empty database gets the baseline in one
// transaction, recorded as every SQLite migration up to pgBaselineCovers;
// then every later file of migrations/postgres that is not recorded yet
// runs in its own transaction.
func (db *DB) migratePostgres(ctx context.Context) error {
	_, err := db.migratePostgresNames(ctx)
	return err
}

// migratePostgresNames is migratePostgres, and returns the migration names
// this run recorded, read and written while it held the lock: a run that
// waited for another to finish returns only what it applied itself.
func (db *DB) migratePostgresNames(ctx context.Context) ([]string, error) {
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(?)`, pgMigrationLock); err != nil {
		return nil, fmt.Errorf("take the migration lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, uerr := conn.ExecContext(uctx, `SELECT pg_advisory_unlock(?)`, pgMigrationLock); uerr != nil {
			// The session lock ends with its connection: drop this one
			// rather than return it to the pool still holding the lock.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        name TEXT PRIMARY KEY,
        applied_at BIGINT NOT NULL
    )`); err != nil {
		return nil, err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return nil, err
	}

	var recorded []string
	if !applied[pgBaselineCovers] {
		covered, err := sqliteNamesThrough(pgBaselineCovers)
		if err != nil {
			return nil, err
		}
		body, err := migrations.ReadFile("migrations/postgres/" + pgBaseline)
		if err != nil {
			return nil, err
		}
		if err := applyPostgres(ctx, conn, pgBaseline, string(body), covered); err != nil {
			return nil, err
		}
		for _, name := range covered {
			if !applied[name] { // ON CONFLICT DO NOTHING kept the rest
				recorded = append(recorded, name)
			}
		}
	}

	names, err := migrationNames("migrations/postgres")
	if err != nil {
		return recorded, err
	}
	for _, name := range names {
		if name == pgBaseline || applied[name] {
			continue
		}
		body, err := migrations.ReadFile("migrations/postgres/" + name)
		if err != nil {
			return recorded, err
		}
		if err := applyPostgres(ctx, conn, name, string(body), []string{name}); err != nil {
			return recorded, err
		}
		recorded = append(recorded, name)
	}
	return recorded, nil
}

// applyPostgres runs body (one or more statements) and records names in one
// transaction. body goes through ExecContext with no arguments, so pgx sends
// it with the simple protocol, which allows several statements.
func applyPostgres(ctx context.Context, conn *sql.Conn, file, body string, names []string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply %s: %w", file, err)
	}
	now := time.Now().Unix()
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?) ON CONFLICT (name) DO NOTHING`, name, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
	}
	return tx.Commit()
}

func appliedMigrations(ctx context.Context, q Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// migrationNames lists the .sql files of one embedded migrations directory,
// sorted by full file name.
func migrationNames(dir string) ([]string, error) {
	entries, err := migrations.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// MigrationsDigest is a hex sha256 over the names and contents of every
// embedded migration file of both engines. It changes whenever a migration
// is added or edited (storetest names its template database with it).
func MigrationsDigest() string {
	h := sha256.New()
	for _, dir := range []string{"migrations/sqlite", "migrations/postgres"} {
		names, err := migrationNames(dir)
		if err != nil {
			panic(fmt.Sprintf("store: read embedded %s: %v", dir, err))
		}
		for _, name := range names {
			body, err := migrations.ReadFile(dir + "/" + name)
			if err != nil {
				panic(fmt.Sprintf("store: read embedded %s/%s: %v", dir, name, err))
			}
			fmt.Fprintf(h, "%s/%s %d\n", dir, name, len(body))
			h.Write(body)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sqliteNamesThrough returns the sorted SQLite migration names up to and
// including last.
func sqliteNamesThrough(last string) ([]string, error) {
	names, err := migrationNames("migrations/sqlite")
	if err != nil {
		return nil, err
	}
	for i, name := range names {
		if name == last {
			return names[:i+1], nil
		}
	}
	return nil, fmt.Errorf("store: SQLite migration %s is missing", last)
}

// PostgresMigrationNames returns, sorted, every migration name a fully
// migrated Postgres database records: the SQLite names the baseline stands
// for, then every later Postgres file. A SQLite database of the same
// version records the same names.
func PostgresMigrationNames() ([]string, error) {
	covered, err := sqliteNamesThrough(pgBaselineCovers)
	if err != nil {
		return nil, err
	}
	names, err := migrationNames("migrations/postgres")
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), covered...)
	for _, n := range names {
		if n != pgBaseline {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}
