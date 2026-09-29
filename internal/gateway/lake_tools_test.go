package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// newTestGatewayWithLake spins up a minimal Gateway backed by a temp SQLite
// control plane and a ClickHouse connection. Each test gets its own ephemeral
// CH database (`test_<rand>`) which is dropped at cleanup, so the suite can
// run concurrently against a single CH instance without state bleed.
//
// Tests skip when TOOLYARD_CH_TEST_ADDR is unset, matching the policy in
// internal/lake/lake_test.go: CI doesn't carry a running CH, but the
// deploy box always has one.
func newTestGatewayWithLake(t *testing.T) *Gateway {
	t.Helper()
	addr := os.Getenv("TOOLYARD_CH_TEST_ADDR")
	if addr == "" {
		t.Skip("TOOLYARD_CH_TEST_ADDR not set; skipping gateway-lake integration test")
	}

	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "tld.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}

	dbName := "test_" + randHex(t, 6)
	lk, err := lake.Open(lake.Config{
		Addr:     addr,
		User:     envOrDefault("TOOLYARD_CH_TEST_USER", "default"),
		Password: os.Getenv("TOOLYARD_CH_TEST_PASSWORD"),
		Database: dbName,
	})
	if err != nil {
		t.Fatalf("lake.Open: %v", err)
	}
	// Create the ephemeral database explicitly — lake.Open creates the
	// standard raw/mart/app databases but not the per-test one we want
	// to scope writes to. The lake.* tools used by the tests below all
	// land in `app` anyway; the per-test DB is just for paranoid
	// isolation (and a fast `DROP DATABASE` cleanup path).
	if _, err := lk.Exec(ctx, fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS `+"`%s`", dbName)); err != nil {
		_ = lk.Close()
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lk.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS `+"`%s`", dbName))
		_ = lk.Close()
	})

	gw := New(Options{
		Policy:   policy.New(nil),
		Approval: bus,
		Audit:    audit.New(db),
		Hub:      realtime.NewHub(),
		Lake:     lk,
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	return gw
}

func envOrDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// TestLakeApprovalMatrix is the canonical "what's the policy for each lake.*
// tool" assertion. If the matrix ever changes (e.g., we decide UPDATE goes
// free, or insert needs approval), this test will fail with a clear pointer
// at the offending tool.
func TestLakeApprovalMatrix(t *testing.T) {
	gw := newTestGatewayWithLake(t)
	cases := []struct {
		tool           string
		wantFreeForced bool // true if forcedAction must be ActionAllow
		wantApprove    bool // true if forcedAction is nil AND name heuristic returns approve
	}{
		// Free without approval (forcedAction = Allow).
		{"lake.query", true, false},
		{"lake.list_tables", true, false},
		{"lake.describe_table", true, false},
		{"lake.insert", true, false},
		{"lake.create_table", true, false},
		{"lake.ingest", true, false},
		// Approval-gated (forcedAction = nil; name heuristic → approve).
		{"lake.update", false, true},
		{"lake.delete", false, true},
		{"lake.alter", false, true},
		{"lake.drop", false, true},
	}
	for _, c := range cases {
		gw.mu.RLock()
		entry, ok := gw.tools[c.tool]
		gw.mu.RUnlock()
		if !ok {
			t.Errorf("%s: not registered", c.tool)
			continue
		}
		if c.wantFreeForced {
			if entry.forcedAction == nil || *entry.forcedAction != policy.ActionAllow {
				t.Errorf("%s: want forcedAction=Allow; got %v", c.tool, entry.forcedAction)
			}
		} else {
			if entry.forcedAction != nil {
				t.Errorf("%s: want no forcedAction; got %v", c.tool, *entry.forcedAction)
			}
		}
	}
}

// TestLakeToolsArePinned asserts that the additive lake.* tools live in
// PinnedTools (so an agent always sees them regardless of surface_mode)
// while the destructive ones (update/delete/alter/drop) are NOT pinned —
// agents must reach them via tools.search. Drift in either direction
// would silently broaden or narrow the default agent toolbelt.
func TestLakeToolsArePinned(t *testing.T) {
	pinned := []string{
		"lake.query", "lake.list_tables", "lake.describe_table",
		"lake.insert", "lake.create_table", "lake.ingest",
	}
	notPinned := []string{
		"lake.update", "lake.delete", "lake.alter", "lake.drop",
	}
	for _, name := range pinned {
		if !IsPinned(name) {
			t.Errorf("%s: expected pinned", name)
		}
	}
	for _, name := range notPinned {
		if IsPinned(name) {
			t.Errorf("%s: expected NOT pinned", name)
		}
	}
}

// TestLakeQueryThroughGateway exercises the lake.query handler end-to-end:
// build args with a _reason, run via routeEntry, expect a successful read.
func TestLakeQueryThroughGateway(t *testing.T) {
	gw := newTestGatewayWithLake(t)
	// Seed a tiny table so we have something to query. CH dialect:
	// `numbers()` replaces DuckDB's `range()`, and MergeTree needs an
	// explicit engine + ORDER BY clause.
	if _, err := gw.lake.Exec(context.Background(),
		`CREATE TABLE app.t ENGINE = MergeTree() ORDER BY x AS SELECT number AS x FROM numbers(10)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = gw.lake.Exec(context.Background(), `DROP TABLE IF EXISTS app.t`)
	})
	gw.mu.RLock()
	entry := gw.tools["lake.query"]
	gw.mu.RUnlock()

	args := map[string]any{
		ReasonField: "smoke test for lake.query through the gateway",
		"sql":       `SELECT COUNT(*) AS n FROM app.t`,
	}
	res, err := gw.routeEntry(context.Background(), entry, "test", args)
	if err != nil {
		t.Fatalf("routeEntry: %v", err)
	}
	if res.IsError {
		t.Fatalf("routeEntry returned error result: %s", debugContent(res))
	}
}

// TestLakeQueryRejectsWrites ensures lake.query refuses non-SELECT — the
// parser-based read-only guarantee at the lake level is intentional and
// should not silently slip away under refactor.
func TestLakeQueryRejectsWrites(t *testing.T) {
	gw := newTestGatewayWithLake(t)
	gw.mu.RLock()
	entry := gw.tools["lake.query"]
	gw.mu.RUnlock()
	args := map[string]any{
		ReasonField: "deliberately attempt write through query path",
		"sql":       `INSERT INTO app.bad VALUES (1)`,
	}
	res, err := gw.routeEntry(context.Background(), entry, "test", args)
	if err != nil {
		t.Fatalf("routeEntry: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError; got success: %s", debugContent(res))
	}
}

func debugContent(res any) string {
	type ct interface{ String() string }
	if r, ok := res.(ct); ok {
		return r.String()
	}
	return ""
}

// TestLakeInsertNoApproval confirms that lake.insert runs without ever
// touching the approval bus. This is the core "additive operations are
// free" guarantee from the user's spec — if it ever regresses, lake.insert
// will start prompting for approval and the test will fail.
func TestLakeInsertNoApproval(t *testing.T) {
	gw := newTestGatewayWithLake(t)
	if _, err := gw.lake.Exec(context.Background(),
		`CREATE TABLE app.k (name String, qty Int32) ENGINE = MergeTree() ORDER BY name`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = gw.lake.Exec(context.Background(), `DROP TABLE IF EXISTS app.k`)
	})
	gw.mu.RLock()
	entry := gw.tools["lake.insert"]
	gw.mu.RUnlock()
	args := map[string]any{
		ReasonField: "smoke test for lake.insert through the gateway",
		"table":     "app.k",
		"rows": []any{
			map[string]any{"name": "alpha", "qty": int32(1)},
			map[string]any{"name": "beta", "qty": int32(2)},
		},
	}
	res, err := gw.routeEntry(context.Background(), entry, "test", args)
	if err != nil {
		t.Fatalf("routeEntry: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got error: %s", debugContent(res))
	}
	// And the rows actually landed.
	rs, err := gw.lake.Query(context.Background(),
		`SELECT COUNT(*) AS n FROM app.k`, lake.QueryOpts{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rs.RowCount != 1 {
		t.Fatalf("verify: want 1 row in result, got %d", rs.RowCount)
	}
}

// TestLakeCreateTableNoApproval is the same guarantee for CREATE TABLE.
func TestLakeCreateTableNoApproval(t *testing.T) {
	gw := newTestGatewayWithLake(t)
	t.Cleanup(func() {
		_, _ = gw.lake.Exec(context.Background(), `DROP TABLE IF EXISTS app.zz`)
	})
	gw.mu.RLock()
	entry := gw.tools["lake.create_table"]
	gw.mu.RUnlock()
	args := map[string]any{
		ReasonField: "smoke test for lake.create_table",
		"sql":       `CREATE TABLE app.zz (id Int32, name String) ENGINE = MergeTree() ORDER BY id`,
	}
	res, err := gw.routeEntry(context.Background(), entry, "test", args)
	if err != nil {
		t.Fatalf("routeEntry: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_table failed: %s", debugContent(res))
	}
}
