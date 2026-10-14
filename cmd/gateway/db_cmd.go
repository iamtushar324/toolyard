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

	"github.com/tusharbhardwaj/toolyard/internal/dbcopy"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const dbUsageText = `usage:
  toolyard db copy   -from-data DIR [-credentials-dir DIR] [-truncate-target] [-allow-live-source] [-json PATH]
  toolyard db verify -from-data DIR [-credentials-dir DIR] [-allow-live-source] [-json PATH]

copy loads a stopped SQLite installation (DIR/toolyard.db and the blob files
in DIR/inbox/blobs) into an empty Postgres database: every table in one
transaction, then every blob into the Postgres blob store. It only reads the
source, and refuses a target that holds rows, a source at other migrations,
and a missing or damaged blob file, before it writes anything.

verify compares the two and exits non-zero on any difference: row counts and
a SHA-256 of every table, every sealed value opened with its master key on
both sides, the signing keys and key canaries, and every blob.

The target is $TOOLYARD_DATABASE_URL, else a database-url file in
$CREDENTIALS_DIRECTORY or -credentials-dir (as for serve). verify reads the
master keys as serve finds them; it never creates one. Stop toolyard first:
the source must not change during a run.
`

// dbTargetConns is the target's pool size: copy holds one connection for
// the tables, and verify one for its snapshot while the blob store reads
// on another.
const dbTargetConns = 4

// runDB is `toolyard db copy|verify` (TEC-1637).
func runDB(argv []string) error {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, dbUsageText)
		return errors.New("toolyard db: name a subcommand: copy or verify")
	}
	switch argv[0] {
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
	return fmt.Errorf("toolyard db: unknown subcommand %q (want copy or verify)", argv[0])
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

	dst, err := store.OpenPostgres(ctx, target.URL, dbTargetConns)
	if err != nil {
		return fmt.Errorf("toolyard db %s: open the target: %w", sub, err)
	}
	defer dst.Close()

	var rep *dbcopy.Report
	if sub == "copy" {
		rep, err = dbcopy.Copy(ctx, src, blobDir, dst, dbcopy.CopyOptions{TruncateTarget: *truncate})
	} else {
		rep, err = dbcopy.Verify(ctx, src, blobDir, dst, keys)
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
// only.
func writeDBReport(path string, rep *dbcopy.Report) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the report: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}
