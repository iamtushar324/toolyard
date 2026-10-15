package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// An unreachable server (nothing listens on 127.0.0.1:1) is tried again,
// 1 s then 2 s apart but never past -db-connect-timeout; each retry is
// logged without the URL; the final error is still store.ErrConnect (exit
// 1, not 78).
func TestOpenPostgresRetryGivesUpAfterTheTimeout(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	target := dbTarget{URL: dbCmdSecretURL, Source: databaseURLEnv}
	opts := store.PostgresOptions{MaxConns: 1, NoMigrate: true}

	start := time.Now()
	_, err := openPostgresRetry(context.Background(), target, opts, 2500*time.Millisecond, logf)
	elapsed := time.Since(start)
	if !errors.Is(err, store.ErrConnect) || errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("unreachable server: %v, want store.ErrConnect and no configuration error", err)
	}
	if !strings.Contains(err.Error(), "gave up after 3 attempts") {
		t.Fatalf("error %v, want it to say it gave up after 3 attempts (at 0, 1 and 2.5 s)", err)
	}
	if elapsed < 2*time.Second || elapsed > 20*time.Second {
		t.Fatalf("gave up after %s, want about 2.5 s", elapsed)
	}
	if len(logs) != 2 {
		t.Fatalf("%d retries logged, want 2: %q", len(logs), logs)
	}
	for _, l := range append(logs, err.Error()) {
		if strings.Contains(l, "s3cret-pw") || strings.Contains(l, dbCmdSecretURL) {
			t.Fatalf("the URL or its password was shown: %q", l)
		}
	}
	if !strings.Contains(logs[0], "postgres host=127.0.0.1 database=toolyard") || !strings.Contains(logs[0], "attempt 1") {
		t.Fatalf("retry line %q does not name the database and the attempt", logs[0])
	}

	// 0: one attempt, nothing logged.
	logs = nil
	if _, err := openPostgresRetry(context.Background(), target, opts, 0, logf); !errors.Is(err, store.ErrConnect) || len(logs) != 0 {
		t.Fatalf("-db-connect-timeout 0: %v, %d retries", err, len(logs))
	}
}

// A configuration error is never retried: it returns at once and exits 78.
func TestOpenPostgresRetryNeverRetriesAConfigError(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	start := time.Now()
	_, err := openPostgresRetry(context.Background(), dbTarget{URL: "postgres://toolyard:s3cret-pw@127.0.0.1:99999/toolyard"},
		store.PostgresOptions{MaxConns: 1, NoMigrate: true}, time.Minute, logf)
	if !errors.Is(err, keyguard.ErrConfig) || errors.Is(err, store.ErrConnect) {
		t.Fatalf("bad port: %v, want a configuration error", err)
	}
	if len(logs) != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("a configuration error was retried: %d retries in %s", len(logs), time.Since(start))
	}
}

// The caller's context ends the retries early (SIGTERM, or db migrate's
// -timeout).
func TestOpenPostgresRetryStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := openPostgresRetry(ctx, dbTarget{URL: dbCmdSecretURL}, store.PostgresOptions{MaxConns: 1, NoMigrate: true}, time.Minute, nil)
	if err == nil {
		t.Fatal("an unreachable server opened")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("retries went on for %s after the context ended", time.Since(start))
	}
}

// serve opens SQLite at once: the retry is for Postgres only.
func TestOpenServeDatabaseSQLite(t *testing.T) {
	storetest.SkipUnlessSQLite(t)
	target, _, _ := migratedTarget(t)
	for _, mode := range []migrateMode{migrateAuto, migrateOff} {
		db, unknown, err := openServeDatabase(context.Background(), target, 1, mode, time.Minute, nil)
		if err != nil || len(unknown) != 0 {
			t.Fatalf("-migrate=%s: %v %v", mode, unknown, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
