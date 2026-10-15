package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// runKeysCheck is `toolyard keys-check -data <dir> [-credentials-dir <dir>]`:
// a read-only pre-deploy check that every master key would load on a real
// start. It opens the database serve would use (TEC-1635: Postgres from
// $TOOLYARD_DATABASE_URL or a database-url credential, else toolyard.db)
// read-only (no migrations, no writes), runs
// the same search, decode and canary rules as `serve`, and prints one line
// per key: found where, and whether its canary matches, is absent (adopted
// on the next start) or does not match. It never creates a key, writes a
// canary or changes any file. It succeeds (exit 0) when every key would
// load; otherwise the error matches keyguard.ErrConfig (exit 78).
func runKeysCheck(argv []string) error {
	fs := flag.NewFlagSet("keys-check", flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory holding toolyard.db (and, on older installations, the key files)")
	credentialsDir := fs.String("credentials-dir", os.Getenv("TOOLYARD_CREDENTIALS_DIR"), "directory searched for key files after $CREDENTIALS_DIRECTORY and before -data, as for serve. Defaults to $TOOLYARD_CREDENTIALS_DIR.")
	keysFlag := fs.String("keys", string(keyguard.CreateIfFresh), "the -keys mode serve will run with: create-if-fresh | require")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("keys-check: unexpected arguments %q", fs.Args())
	}
	// As serve does: a toolyard.env in the credentials directories fills
	// unset variables first ($TOOLYARD_DATABASE_URL among them).
	if _, err := keyguard.LoadEnvFiles(*credentialsDir); err != nil {
		return err
	}
	return keysCheck(context.Background(), os.Stdout, *dataDir, *credentialsDir, *keysFlag, os.Getenv)
}

func keysCheck(ctx context.Context, out io.Writer, dataDir, credentialsDir, keysMode string, getenv func(string) string) error {
	mode, err := keyguard.ParseMode(keysMode)
	if err != nil {
		return err
	}
	target, err := resolveDatabase(dataDir, credentialsDir, getenv)
	if err != nil {
		return keysCheckFailed(err.Error())
	}
	var db *sql.DB
	dialect := store.SQLite
	if target.isPostgres() {
		// A read-only session (default_transaction_read_only=on) and no
		// migrations. An empty database reads as new, as a missing file does.
		pdb, err := store.OpenPostgresReadOnly(ctx, target.URL)
		if err != nil {
			return keysCheckFailed(fmt.Sprintf("cannot open %s read-only: %v", target.describe(), err))
		}
		defer pdb.Close()
		db, dialect = pdb.DB, store.Postgres
		fmt.Fprintf(out, "database     %s (opened read-only)\n", target.describe())
	} else {
		dbPath := target.Path
		switch sdb, err := store.OpenReadOnly(dbPath); {
		case err == nil:
			db = sdb
			defer db.Close()
			fmt.Fprintf(out, "database     %s (opened read-only)\n", dbPath)
		case errors.Is(err, iofs.ErrNotExist):
			fmt.Fprintf(out, "database     %s not found: the first start creates a new one\n", dbPath)
		default:
			return keysCheckFailed(fmt.Sprintf("cannot open %s read-only: %v", dbPath, err))
		}
	}
	reports, err := keyguard.Check(ctx, keyguard.Config{
		DB:             db,
		Dialect:        dialect,
		DataDir:        dataDir,
		CredentialsDir: credentialsDir,
		Mode:           mode,
		Getenv:         getenv,
	}, keyguard.ServeKeys())
	for _, r := range reports {
		fmt.Fprintf(out, "%-12s %-4s %s\n", r.Name, r.Status, r.Detail)
	}
	return err
}

// keysCheckFailed is a keys-check failure that exits with code 78, like a
// key error at startup.
func keysCheckFailed(msg string) error {
	return &keysCheckError{msg: "keys-check: " + msg}
}

type keysCheckError struct{ msg string }

func (e *keysCheckError) Error() string { return e.msg }

func (e *keysCheckError) Is(target error) bool { return target == keyguard.ErrConfig }
