package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/dbcopy"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const dbUsageText = `usage:
  toolyard db migrate [-data DIR] [-credentials-dir DIR] [-dry-run] [-timeout 10m] [-db-connect-timeout 60s]
  toolyard db copy    -from-data DIR [-credentials-dir DIR] [-truncate-target] [-allow-live-source] [-json PATH]
  toolyard db verify  -from-data DIR [-credentials-dir DIR] [-allow-live-source] [-json PATH]

migrate applies the schema migrations this binary knows and the database
does not record yet, to the database serve would use: $TOOLYARD_DATABASE_URL,
else a database-url file in $CREDENTIALS_DIRECTORY or -credentials-dir (a
toolyard.env there is read first, as by serve), else SQLite at
DIR/toolyard.db (created when missing). On Postgres it holds the same
advisory lock as serve while it migrates. It prints each migration it
applied, or "up to date", and exits 0; a setting that is missing or wrong
exits 78. -dry-run lists the pending migrations and applies none; it opens
the database read-only. Run it before serve -migrate=off starts. As serve
does, it keeps trying a Postgres server it cannot reach for
-db-connect-timeout (an Aurora failover). TOOLYARD_DATA (or
TOOLYARD_DATA_DIR), TOOLYARD_CREDENTIALS_DIR and TOOLYARD_DB_CONNECT_TIMEOUT
set -data, -credentials-dir and -db-connect-timeout when they are not given.

copy loads a stopped SQLite installation (DIR/toolyard.db and the blob files
in DIR/inbox/blobs) into an empty Postgres database: every table and every
blob in one transaction, so a failed copy leaves nothing behind. It only
reads the source. Before it writes anything it refuses a target that holds
rows or another application's tables, a source at other migrations, and a
missing or damaged blob file. It creates the toolyard tables only in a
database that records no migration yet, and never migrates any other.

verify compares the two and exits non-zero on any difference: row counts and
a SHA-256 of every table, every sealed value opened with its master key on
both sides, the signing keys and key canaries, and every blob. It opens the
target read-only and never migrates it.

The target is $TOOLYARD_DATABASE_URL, else a database-url file in
$CREDENTIALS_DIRECTORY or -credentials-dir (as for serve). verify reads the
master keys as serve finds them; it never creates one. Stop toolyard first:
the source must not change during a run.
`

// dbTargetConns is the target's pool size: copy holds one connection for
// its checks or its copy transaction, verify one for its snapshot.
const dbTargetConns = 4

// runDB is `toolyard db migrate|copy|verify` (TEC-1637, TEC-1638).
func runDB(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, dbUsageText)
		return errors.New("toolyard db: name a subcommand: migrate, copy or verify")
	}
	switch argv[0] {
	case "migrate":
		// Ctrl-C or SIGTERM cancels the run; the migration in progress
		// then rolls back (each runs in its own transaction).
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDBMigrate(ctx, argv[1:], os.Stdout, os.Stderr, processEnv())
	case "copy", "verify":
		// Ctrl-C or SIGTERM cancels the run; the copy transaction then
		// rolls back.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDBCommand(ctx, argv[0], argv[1:], os.Stdout, os.Stderr, os.Getenv)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(os.Stdout, dbUsageText)
		return nil
	}
	fmt.Fprint(os.Stderr, dbUsageText)
	return fmt.Errorf("toolyard db: unknown subcommand %q (want migrate, copy or verify)", argv[0])
}

// dbMigrateConns is db migrate's Postgres pool size: the migration holds
// one connection (with the advisory lock) and the status reads use another.
const dbMigrateConns = 2

// runDBMigrate is `toolyard db migrate` (TEC-1638). It never prints or logs
// the database URL: the database is named by engine, host and name only.
func runDBMigrate(ctx context.Context, argv []string, stdout, stderr io.Writer, env flagEnv) error {
	fs := flag.NewFlagSet("db migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", defaultDataDirFrom(env.getenv),
		"data directory: SQLite at DIR/toolyard.db when no Postgres URL is set, as for serve. Defaults to $TOOLYARD_DATA_DIR, then ~/.toolyard.")
	credentialsDir := fs.String("credentials-dir", env.getenv("TOOLYARD_CREDENTIALS_DIR"),
		"directory searched after $CREDENTIALS_DIRECTORY for database-url and toolyard.env, as for serve. Defaults to $TOOLYARD_CREDENTIALS_DIR.")
	dryRun := fs.Bool("dry-run", false, "list the pending migrations and apply none; the database is opened read-only")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up after this long, the wait for the migration lock included; 0 means no limit")
	connectTimeout := fs.Duration("db-connect-timeout", defaultDBConnectTimeout, dbConnectTimeoutUsage)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("toolyard db migrate: unexpected arguments %q", fs.Args())
	}
	rep, err := loadFlagEnv(fs, env, credentialsDir, "data", "credentials-dir", "db-connect-timeout")
	if err != nil {
		return err
	}
	for _, w := range rep.warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if *timeout < 0 {
		return configErrorf("toolyard db migrate: -timeout must not be negative")
	}
	if err := checkConnectTimeout(*connectTimeout); err != nil {
		return err
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	target, err := resolveDatabase(*dataDir, *credentialsDir, env.getenv)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) }
	return dbMigrate(ctx, target, *dataDir, *dryRun, *connectTimeout, logf, stdout)
}

// dbMigrate applies the pending migrations of target (or, with dryRun,
// lists them) and prints what it did on w. A Postgres server it cannot
// reach is tried again for connectTimeout (openPostgresRetry; logf gets the
// retries).
func dbMigrate(ctx context.Context, target dbTarget, dataDir string, dryRun bool, connectTimeout time.Duration, logf func(string, ...any), w io.Writer) error {
	line := func(label, text string) { fmt.Fprintf(w, "%-8s %s\n", label, text) }
	line("database", fmt.Sprintf("%s (from %s)", target.describe(), target.Source))

	var db *store.DB
	var before, unknown []string
	switch {
	case target.isPostgres():
		opts := store.PostgresOptions{MaxConns: dbMigrateConns, NoMigrate: true}
		if dryRun {
			opts = store.PostgresOptions{MaxConns: 1, ReadOnly: true}
		}
		pdb, err := openPostgresRetry(ctx, target, opts, connectTimeout, logf)
		if err != nil {
			return asDBConfigError(fmt.Errorf("toolyard db migrate: open %s: %w", target.describe(), err))
		}
		db = pdb
	default:
		var sdb *store.DB
		var err error
		if dryRun {
			// Read only: a dry run changes nothing, not even the journal
			// mode, and works on a read-only mount.
			ro, rerr := store.OpenReadOnly(target.Path)
			if err = rerr; err == nil {
				sdb = &store.DB{DB: ro, Path: target.Path, Dialect: store.SQLite}
			}
		} else {
			sdb, err = store.OpenNoMigrate(target.Path)
		}
		switch {
		case errors.Is(err, iofs.ErrNotExist):
			// A new installation: every migration is pending, and the
			// file is created below (not on a dry run).
			if before, err = store.KnownMigrations(store.SQLite); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("toolyard db migrate: open %s: %w", target.Path, err)
		default:
			db = sdb
		}
	}
	if db != nil {
		defer db.Close()
		var err error
		if before, unknown, err = store.MigrationStatus(ctx, db); err != nil {
			return fmt.Errorf("toolyard db migrate: read the schema level: %w", err)
		}
	}
	for _, n := range unknown {
		line("note", n+" is recorded but unknown to this binary (a newer toolyard migrated the database)")
	}
	known := len(before)
	if all, err := store.KnownMigrations(storeDialect(target)); err == nil {
		known = len(all)
	}

	if dryRun {
		for _, n := range before {
			line("pending", n)
		}
		switch {
		case len(before) == 0:
			line("result", fmt.Sprintf("up to date: the database records all %d migrations this binary knows", known))
		case db == nil:
			line("result", fmt.Sprintf("%d pending; %s does not exist yet and migrate creates it; nothing applied (-dry-run)", len(before), target.Path))
		default:
			line("result", fmt.Sprintf("%d pending; nothing applied (-dry-run)", len(before)))
		}
		return nil
	}
	if len(before) == 0 {
		line("result", fmt.Sprintf("up to date: the database records all %d migrations this binary knows", known))
		return nil
	}

	if db == nil {
		// SQLite that does not exist yet: an empty file (a new database)
		// only its owner can read, in a data directory only its owner can
		// read, as serve makes it; then it is migrated below.
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return fmt.Errorf("toolyard db migrate: %w", err)
		}
		f, err := os.OpenFile(target.Path, os.O_WRONLY|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("toolyard db migrate: create %s: %w", target.Path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("toolyard db migrate: create %s: %w", target.Path, err)
		}
		sdb, err := store.OpenNoMigrate(target.Path)
		if err != nil {
			return fmt.Errorf("toolyard db migrate: open %s: %w", target.Path, err)
		}
		db = sdb
		defer db.Close()
	}
	// The names Migrate returns are those this run applied, read and
	// written under the migration lock on Postgres: a run that waited for
	// another never claims that one's work.
	applied, err := db.Migrate(ctx)
	for _, n := range applied {
		line("applied", n)
	}
	if err != nil {
		return fmt.Errorf("toolyard db migrate: %w", err)
	}
	after, _, err := store.MigrationStatus(ctx, db)
	if err != nil {
		return fmt.Errorf("toolyard db migrate: read the schema level after migrating: %w", err)
	}
	if len(after) > 0 {
		return fmt.Errorf("toolyard db migrate: %d migration(s) still pending after migrating: %s", len(after), nameList(after, 5))
	}
	if len(applied) < len(before) {
		line("note", fmt.Sprintf("another run applied %d of the %d pending migrations while this one waited", len(before)-len(applied), len(before)))
	}
	line("result", fmt.Sprintf("applied %d; the database records all %d migrations this binary knows", len(applied), known))
	return nil
}

// storeDialect is the engine of target.
func storeDialect(t dbTarget) store.Dialect {
	if t.isPostgres() {
		return store.Postgres
	}
	return store.SQLite
}

// runDBCommand runs `db copy` or `db verify` (sub). It never prints or
// logs the target URL or any value: only names, counts and hashes.
func runDBCommand(ctx context.Context, sub string, argv []string, stdout, stderr io.Writer, getenv func(string) string) error {
	fs := flag.NewFlagSet("db "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromData := fs.String("from-data", "", "data directory of the SQLite installation: DIR/toolyard.db and the blob files in DIR/inbox/blobs (required)")
	credentialsDir := fs.String("credentials-dir", getenv("TOOLYARD_CREDENTIALS_DIR"),
		"directory searched after $CREDENTIALS_DIRECTORY for the target URL file database-url and the master key files, as for serve. Defaults to $TOOLYARD_CREDENTIALS_DIR.")
	jsonPath := fs.String("json", "", "also write the report as JSON to this file (names, counts and hashes only)")
	allowLive := fs.Bool("allow-live-source", false,
		"skip the check that no toolyard has the source database open. Unsafe: writes made during the run are missed, and the blob files can change under it. For a rehearsal only.")
	truncate := new(bool)
	if sub == "copy" {
		truncate = fs.Bool("truncate-target", false,
			"empty every copied table and the blob store of the target before copying. Only to repeat a rehearsal on a throwaway database, never on a database in use.")
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("toolyard db %s: unexpected arguments %q", sub, fs.Args())
	}
	dataDir := strings.TrimSpace(*fromData)
	if dataDir == "" {
		return fmt.Errorf("toolyard db %s: -from-data is required: the data directory of the SQLite installation", sub)
	}
	dbPath := filepath.Join(dataDir, "toolyard.db")
	blobDir := filepath.Join(dataDir, "inbox", "blobs")
	jobResultDir := filepath.Join(dataDir, "job-results")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("toolyard db %s: source database: %w", sub, err)
	}
	if why := dbSourceInUse(dbPath); why != "" {
		if !*allowLive {
			return fmt.Errorf("toolyard db %s: the source database looks in use (%s). Stop toolyard first: a run on a live database "+
				"can miss writes made during it, and blob files can change under it. -allow-live-source skips this check, for a rehearsal only", sub, why)
		}
		fmt.Fprintf(stderr, "WARNING: the source database looks in use (%s); -allow-live-source is set, so this run may miss writes. Use the result for a rehearsal only.\n", why)
	}
	// The source opens immutable (no locks), so nothing stops a toolyard
	// that starts during the run from changing it: the guard checks, just
	// before the target commit and at the end of verify, that the files did
	// not change and the database is still not in use.
	guard, err := newSourceGuard(dbPath, *allowLive, stderr)
	if err != nil {
		return fmt.Errorf("toolyard db %s: %w", sub, err)
	}

	target, err := resolveDatabase(dataDir, *credentialsDir, getenv)
	if err != nil {
		return err
	}
	if !target.isPostgres() {
		return dbConfigErrorf("toolyard db %s needs a Postgres target: set %s, or put the URL in a %s file in $CREDENTIALS_DIRECTORY or -credentials-dir",
			sub, databaseURLEnv, databaseURLFile)
	}

	var keys map[string][]byte
	if sub == "verify" {
		keys, err = keyguard.ReadKeys(keyguard.Config{
			DataDir:        dataDir,
			CredentialsDir: *credentialsDir,
			Getenv:         getenv,
			Logf:           func(format string, args ...any) { fmt.Fprintln(stderr, fmt.Sprintf(format, args...)) },
		}, keyguard.ServeKeys())
		if err != nil {
			return err
		}
	}

	src, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("toolyard db %s: open the source read-only: %w", sub, err)
	}
	defer src.Close()
	fmt.Fprintf(stdout, "source   sqlite %s (read-only), blobs in %s\n", dbPath, blobDir)
	fmt.Fprintf(stdout, "target   %s (from %s)\n", target.describe(), target.Source)
	if *truncate {
		fmt.Fprintln(stderr, "WARNING: -truncate-target empties every table toolyard copies, and the blob store, in the target database before copying. "+
			"Use it only to repeat a rehearsal on a throwaway database, never on a database in use.")
	}

	// Neither command migrates on open. verify's sessions are read-only;
	// copy checks everything in a read-only transaction, and only then
	// creates the toolyard tables, and only in a database that records no
	// migration yet.
	opts := store.PostgresOptions{MaxConns: dbTargetConns, NoMigrate: true}
	if sub == "verify" {
		opts = store.PostgresOptions{MaxConns: dbTargetConns, ReadOnly: true}
	}
	dst, err := store.OpenPostgresWith(ctx, target.URL, opts)
	if err != nil {
		return asDBConfigError(fmt.Errorf("toolyard db %s: open the target: %w", sub, err))
	}
	defer dst.Close()

	var rep *dbcopy.Report
	if sub == "copy" {
		rep, err = dbcopy.Copy(ctx, src, blobDir, dst, dbcopy.CopyOptions{TruncateTarget: *truncate, SourceGuard: guard, JobResultDir: jobResultDir})
	} else {
		rep, err = dbcopy.Verify(ctx, src, blobDir, dst, keys, dbcopy.VerifyOptions{SourceGuard: guard, JobResultDir: jobResultDir})
	}
	if rep != nil {
		printDBReport(stdout, rep)
		if *jsonPath != "" {
			if werr := writeDBReport(*jsonPath, rep); werr != nil {
				if err == nil {
					return fmt.Errorf("toolyard db %s: %w", sub, werr)
				}
				fmt.Fprintf(stderr, "toolyard db %s: %v\n", sub, werr)
			} else {
				fmt.Fprintf(stdout, "report   %s\n", *jsonPath)
			}
		}
	}
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("toolyard db %s: %d problems (listed above)", sub, len(rep.Problems))
	}
	if sub == "copy" {
		fmt.Fprintln(stdout, "next     run toolyard db verify with the same flags")
	}
	return nil
}

// dbSourceInUse says why the SQLite database at dbPath looks open by a
// process ("" when it does not): a -shm file exists, or the -wal file is
// not empty. A closed database has neither (Close checkpoints the WAL).
func dbSourceInUse(dbPath string) string {
	base := filepath.Base(dbPath)
	if _, err := os.Stat(dbPath + "-shm"); !errors.Is(err, iofs.ErrNotExist) {
		return "a " + base + "-shm file exists"
	}
	fi, err := os.Stat(dbPath + "-wal")
	if errors.Is(err, iofs.ErrNotExist) {
		return ""
	}
	if err != nil || fi.Size() > 0 {
		return "its " + base + "-wal file is not empty"
	}
	return ""
}

// sourceFile is what newSourceGuard records of one source file.
type sourceFile struct {
	name   string // base name, for messages
	exists bool
	size   int64
	mod    time.Time
}

// statSource records the size and modification time of the SQLite database
// at dbPath and of its -wal and -shm files (a missing sidecar is recorded
// as missing).
func statSource(dbPath string) ([]sourceFile, error) {
	var out []sourceFile
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		f := sourceFile{name: filepath.Base(p)}
		fi, err := os.Stat(p)
		switch {
		case err == nil:
			f.exists, f.size, f.mod = true, fi.Size(), fi.ModTime()
		case errors.Is(err, iofs.ErrNotExist):
		default:
			return nil, fmt.Errorf("check the source file %s: %w", f.name, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// sourceChanges names the files whose existence, size or modification time
// differ between two statSource results.
func sourceChanges(before, after []sourceFile) []string {
	var out []string
	for i := range before {
		if i >= len(after) {
			break
		}
		b, a := before[i], after[i]
		switch {
		case b.exists != a.exists && a.exists:
			out = append(out, a.name+" appeared")
		case b.exists != a.exists:
			out = append(out, b.name+" disappeared")
		case b.exists && (b.size != a.size || !b.mod.Equal(a.mod)):
			out = append(out, a.name+" changed")
		}
	}
	return out
}

// newSourceGuard records the source files now and returns a check for the
// end of the run: it fails when a file changed since, or the database now
// looks in use. With allowLive it only warns on stderr (the run is a
// rehearsal on a live database anyway).
func newSourceGuard(dbPath string, allowLive bool, stderr io.Writer) (func() error, error) {
	before, err := statSource(dbPath)
	if err != nil {
		return nil, err
	}
	return func() error {
		after, err := statSource(dbPath)
		if err != nil {
			return err
		}
		why := sourceChanges(before, after)
		if inUse := dbSourceInUse(dbPath); inUse != "" {
			why = append(why, "the database looks in use: "+inUse)
		}
		if len(why) == 0 {
			return nil
		}
		if allowLive {
			fmt.Fprintf(stderr, "WARNING: the source changed during the run (%s); -allow-live-source is set. Use the result for a rehearsal only.\n", strings.Join(why, "; "))
			return nil
		}
		return fmt.Errorf("the source database changed during the run (%s): a toolyard may have started on it. "+
			"Stop it, and run the command again", strings.Join(why, "; "))
	}, nil
}

// printDBReport prints a short summary: counts, names and problems.
func printDBReport(w io.Writer, rep *dbcopy.Report) {
	switch rep.Command {
	case "copy":
		var rows, skipped int64
		for _, t := range rep.Tables {
			rows += t.CopiedRows
			skipped += t.SkippedRows
		}
		if rep.TruncatedTarget {
			fmt.Fprintln(w, "target   emptied first (-truncate-target)")
		}
		fmt.Fprintf(w, "tables   %d copied, %d rows, %d rows skipped\n", len(rep.Tables), rows, skipped)
		if len(rep.IdentityReset) > 0 {
			fmt.Fprintf(w, "ids      sequences moved past the copied ids: %s\n", strings.Join(rep.IdentityReset, ", "))
		}
		fmt.Fprintf(w, "blobs    %d rows: %d copied, %d already there, %d bytes, %d orphan files not copied\n",
			rep.Blobs.Rows, rep.Blobs.Copied, rep.Blobs.AlreadyPresent, rep.Blobs.Bytes, len(rep.Blobs.OrphanFiles))
		if jr := rep.JobResults; jr.Rows > 0 {
			fmt.Fprintf(w, "results  %d job results: %d copied, %d already there, %d bytes; %d missing and %d damaged source files not copied\n",
				jr.Rows, jr.Copied, jr.AlreadyPresent, jr.Bytes, len(jr.MissingFiles), len(jr.MismatchedFiles))
		}
	case "verify":
		var match int
		var rows int64
		for _, t := range rep.Tables {
			rows += t.SourceRows
			if t.Match {
				match++
			}
		}
		fmt.Fprintf(w, "tables   %d of %d match (%d source rows compared)\n", match, len(rep.Tables), rows)
		for _, t := range rep.Tables {
			if !t.Match {
				fmt.Fprintf(w, "  DIFF   %s: %d source rows, %d target rows\n", t.Name, t.SourceRows, t.TargetRows)
			}
		}
		var opened, values int
		for _, s := range rep.Sealed {
			values += s.SourceValues
			if s.Match {
				opened += s.SourceValues
			}
		}
		fmt.Fprintf(w, "sealed   %d of %d source values open on both sides to the same plaintext\n", opened, values)
		for _, s := range rep.Sealed {
			if !s.Match {
				fmt.Fprintf(w, "  DIFF   %s.%s (%s): %d/%d source and %d/%d target values do not open\n",
					s.Table, s.Column, s.Key, s.SourceUnopened, s.SourceValues, s.TargetUnopened, s.TargetValues)
			}
		}
		for _, n := range rep.NamedRows {
			state := "same"
			switch {
			case !n.Match:
				state = "DIFF"
			case !n.SourcePresent:
				state = "absent"
			}
			if n.KeyMatches != nil && !*n.KeyMatches {
				state = "WRONG KEY"
			}
			fmt.Fprintf(w, "  %-6s %s\n", state, n.Name)
		}
		fmt.Fprintf(w, "blobs    %d of %d read back from the target with the right sha256\n", rep.Blobs.Verified, rep.Blobs.Rows)
		if jr := rep.JobResults; jr.Rows > 0 {
			fmt.Fprintf(w, "results  %d of %d job results read back from the target with the right key (%d missing and %d damaged source files)\n",
				jr.Verified, jr.Rows, len(jr.MissingFiles), len(jr.MismatchedFiles))
		}
	}
	if len(rep.DroppedColumns) > 0 {
		fmt.Fprintf(w, "dropped  columns not in the target: %s\n", strings.Join(rep.DroppedColumns, ", "))
	}
	if len(rep.SkipRules) > 0 {
		fmt.Fprintf(w, "skipped  %s\n", strings.Join(rep.SkipRules, "; "))
	}
	if rep.OK() {
		fmt.Fprintln(w, "result   OK")
		return
	}
	fmt.Fprintf(w, "result   %d problems:\n", len(rep.Problems))
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
}

// writeDBReport writes the report as indented JSON, readable by its owner
// only. It writes a new file (O_CREATE|O_EXCL, mode 0600) in the same
// directory and renames it over path, so an existing file keeps neither its
// mode nor its content, and a symlink at path is replaced, not followed.
func writeDBReport(path string, rep *dbcopy.Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the report: %w", err)
	}
	// os.CreateTemp opens with O_RDWR|O_CREATE|O_EXCL and mode 0600.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		ok = true // closed and removed
		return fmt.Errorf("write the report: %w", err)
	}
	ok = true
	return nil
}
