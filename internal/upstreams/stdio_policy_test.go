package upstreams

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// A stdio row saved before -no-stdio-upstreams was turned on must not be
// started by LoadAll or by a dashboard Reconnect. The command here would
// create a marker file if it were ever spawned.
func TestNoStdioUpstreamsBlocksSavedRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "up.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, _ := approval.New(ctx, db)
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub()})

	marker := filepath.Join(dir, "spawned")
	args, _ := json.Marshal([]string{"-c", "touch " + marker})
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO upstream_servers(name, transport, command, args_json, env_json, headers_json, enabled, created_at, updated_at)
        VALUES('fs', 'stdio', '/bin/sh', ?, '{}', '{}', 1, ?, ?)`, string(args), now, now); err != nil {
		t.Fatal(err)
	}

	svc := New(db, gw)
	svc.SetPolicy(Policy{AllowStdio: false})
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reconnect(ctx, "fs"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Reconnect of a stdio row under -no-stdio-upstreams: err = %v, want ErrInvalid", err)
	}
	time.Sleep(200 * time.Millisecond) // give a wrongly spawned process time to run
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("stdio upstream was spawned despite -no-stdio-upstreams")
	}
	got, err := svc.Get(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus == "ok" || !strings.Contains(got.LastError, "no-stdio-upstreams") {
		t.Fatalf("status = %q, error = %q; want the disabled reason", got.LastStatus, got.LastError)
	}
}
