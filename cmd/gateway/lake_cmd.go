package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// runLake dispatches `toolyard lake <subcmd>`. Subcommands:
//
//	tables   print every table in the lake (for quick sanity checks)
//	backup   prints the filesystem-snapshot recipe — see the body for why
//	         we no longer emit a SQL-level dump.
func runLake(argv []string) error {
	if len(argv) == 0 {
		return errors.New("lake: missing subcommand. Try: backup | tables")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "backup":
		return runLakeBackup(rest)
	case "tables":
		return runLakeTables(rest)
	default:
		return fmt.Errorf("lake: unknown subcommand %q", sub)
	}
}

// openLakeForCLI opens the lake against the local ClickHouse stack. We
// load the password from the toolyard settings DB at <dataDir>/toolyard.db
// — the same source of truth the long-running gateway uses, so the CLI
// and the server always agree on credentials. CH host:port is hardcoded
// to 127.0.0.1:19000 to match deploy/clickhouse/docker-compose.yaml.
func openLakeForCLI(dataDir string) (*lake.Service, error) {
	db, err := store.Open(filepath.Join(dataDir, "toolyard.db"))
	if err != nil {
		return nil, fmt.Errorf("lake CLI: open settings db: %w", err)
	}
	// The settings service caches in memory; closing the underlying
	// store right after we've read the password is fine because we
	// only need the one value. The lake.Service holds its own
	// CH connection independently.
	defer db.Close()

	ctx := context.Background()
	s, err := settings.New(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("lake CLI: init settings: %w", err)
	}
	pw, _, err := s.EnsureClickhousePassword(ctx)
	if err != nil {
		return nil, fmt.Errorf("lake CLI: load clickhouse password: %w", err)
	}
	return lake.Open(lake.Config{
		Addr:     "127.0.0.1:19000",
		User:     "default",
		Password: pw,
	})
}

// runLakeBackup is intentionally a no-op informational subcommand on
// ClickHouse: the lake's storage is a docker volume mounted at
// /var/lib/toolyard/clickhouse/data, which is already captured by the
// `tar -czf /var/lib/toolyard.tgz /var/lib/toolyard/` pattern the
// install README documents. A SQL-level dump (DuckDB-era
// `EXPORT DATABASE ... FORMAT PARQUET`) has no clean CH equivalent
// without server-side BACKUP-engine config, and would duplicate state
// that already lives in the filesystem snapshot.
func runLakeBackup(argv []string) error {
	fs := flag.NewFlagSet("lake backup", flag.ExitOnError)
	_ = fs.String("data", defaultDataDir(), "toolyard data dir (unused, kept for back-compat)")
	_ = fs.String("out", "", "output directory (unused, kept for back-compat)")
	_ = fs.Parse(argv)
	fmt.Println(`toolyard lake backup: the lake is now ClickHouse-backed.
Take a filesystem snapshot instead — CH state lives under
/var/lib/toolyard/clickhouse/data and is captured by:

    sudo systemctl stop toolyard
    sudo docker compose -f deploy/clickhouse/docker-compose.yaml stop
    sudo tar -czf /tmp/toolyard-$(date -I).tgz /var/lib/toolyard/
    sudo docker compose -f deploy/clickhouse/docker-compose.yaml start
    sudo systemctl start toolyard

Restore is the reverse: stop both, extract, start both. The settings
DB (toolyard.db) and the CH password (clickhouse-runtime.env) are in
the same tar, so the restored stack comes up with matching creds.`)
	return nil
}

func runLakeTables(argv []string) error {
	fs := flag.NewFlagSet("lake tables", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "toolyard data dir")
	schema := fs.String("schema", "", "filter to one schema (raw|mart|app)")
	_ = fs.Parse(argv)
	s, err := openLakeForCLI(*dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	tabs, err := s.ListTables(context.Background(), *schema)
	if err != nil {
		return err
	}
	body, _ := json.MarshalIndent(tabs, "", "  ")
	fmt.Println(string(body))
	return nil
}
