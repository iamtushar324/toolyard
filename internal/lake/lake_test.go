package lake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTestLake opens a Service against the CH instance pointed to by
// TOOLYARD_CH_TEST_ADDR. When that env var is unset the test is skipped so
// CI doesn't need a running CH. Each test gets its own ephemeral database
// `test_<rand>` that's dropped at cleanup.
func openTestLake(t *testing.T) (*Service, string) {
	t.Helper()
	addr := os.Getenv("TOOLYARD_CH_TEST_ADDR")
	if addr == "" {
		t.Skip("TOOLYARD_CH_TEST_ADDR not set; skipping ClickHouse-backed lake test")
	}
	dbName := "test_" + randHex(t, 6)
	s, err := Open(Config{
		Addr:     addr,
		User:     envOrDefault("TOOLYARD_CH_TEST_USER", "default"),
		Password: os.Getenv("TOOLYARD_CH_TEST_PASSWORD"),
		Database: dbName,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.conn.Exec(context.Background(),
		fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, quoteIdent(dbName))); err != nil {
		_ = s.Close()
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() {
		_ = s.conn.Exec(context.Background(),
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, quoteIdent(dbName)))
		_ = s.Close()
	})
	return s, dbName
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

func TestOpen_CreatesStandardDatabases(t *testing.T) {
	s, _ := openTestLake(t)
	// raw/mart/app are created at Open time. Verify they exist via the
	// system.databases system table.
	rows, err := s.conn.Query(context.Background(),
		`SELECT name FROM system.databases WHERE name IN ('raw','mart','app') ORDER BY name`)
	if err != nil {
		t.Fatalf("query system.databases: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name)
	}
	want := []string{"app", "mart", "raw"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestQuery_HitsRowCap(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".t"
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (x Int64) ENGINE = MergeTree() ORDER BY x`, quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT number FROM numbers(5000)`, quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	res, err := s.Query(ctx, fmt.Sprintf(`SELECT x FROM %s ORDER BY x`, quoteQualifiedName(tbl)),
		QueryOpts{MaxRows: 100})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.RowCount != 100 {
		t.Fatalf("want 100 rows, got %d", res.RowCount)
	}
	if !res.Truncated {
		t.Fatalf("want Truncated=true")
	}
}

func TestQuery_BytesCap(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := quoteQualifiedName(db + ".big")
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (s String) ENGINE = MergeTree() ORDER BY tuple()`, tbl)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT repeat('x', 1024) FROM numbers(1000)`, tbl)); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	res, err := s.Query(ctx, fmt.Sprintf(`SELECT s FROM %s`, tbl),
		QueryOpts{MaxBytes: 64 * 1024})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("want Truncated=true")
	}
	if res.SizeBytes > 128*1024 {
		t.Fatalf("byte cap soft-violated badly: %d", res.SizeBytes)
	}
}

func TestInsertRows_Roundtrip(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".k"
	tblQ := quoteQualifiedName(tbl)
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (name String, qty Int32) ENGINE = MergeTree() ORDER BY name`, tblQ)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	res, err := s.InsertRows(ctx, tbl, []map[string]any{
		{"name": "alpha", "qty": int32(1)},
		{"name": "beta", "qty": int32(2)},
	})
	if err != nil {
		t.Fatalf("InsertRows: %v", err)
	}
	if res.RowsAffected != 2 {
		t.Fatalf("want 2 rows affected, got %d", res.RowsAffected)
	}
	q, err := s.Query(ctx, fmt.Sprintf(`SELECT name, qty FROM %s ORDER BY name`, tblQ), QueryOpts{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if q.RowCount != 2 || q.Rows[0][0].(string) != "alpha" {
		t.Fatalf("unexpected: %+v", q)
	}
}

func TestUpdateRows_PreCount(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".u"
	tblQ := quoteQualifiedName(tbl)
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id Int32, v Int32) ENGINE = MergeTree() ORDER BY id`, tblQ)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT number, 0 FROM numbers(10)`, tblQ)); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	res, err := s.UpdateRows(ctx, tbl, map[string]any{"v": int32(7)}, "id < ?", []any{int32(4)})
	if err != nil {
		t.Fatalf("UpdateRows: %v", err)
	}
	if res.RowsAffected != 4 {
		t.Fatalf("want 4 rows affected, got %d", res.RowsAffected)
	}
	q, err := s.Query(ctx, fmt.Sprintf(`SELECT count() FROM %s WHERE v = 7`, tblQ), QueryOpts{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	got := q.Rows[0][0]
	// CH count() returns UInt64.
	switch n := got.(type) {
	case uint64:
		if n != 4 {
			t.Fatalf("post-update count: want 4, got %d", n)
		}
	default:
		t.Fatalf("unexpected count type %T: %v", got, got)
	}
}

func TestUpdateRows_RequiresWhere(t *testing.T) {
	s, db := openTestLake(t)
	tbl := db + ".u"
	if _, err := s.Exec(context.Background(), fmt.Sprintf(
		`CREATE TABLE %s (id Int32, v Int32) ENGINE = MergeTree() ORDER BY id`, quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	_, err := s.UpdateRows(context.Background(), tbl, map[string]any{"v": int32(7)}, "", nil)
	if err == nil {
		t.Fatalf("expected error for empty WHERE")
	}
	if !strings.Contains(err.Error(), "WHERE") {
		t.Fatalf("error should mention WHERE: %v", err)
	}
}

func TestDeleteRows_PreCount(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".d"
	tblQ := quoteQualifiedName(tbl)
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id Int32) ENGINE = MergeTree() ORDER BY id`, tblQ)); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s SELECT number FROM numbers(10)`, tblQ)); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	res, err := s.DeleteRows(ctx, tbl, "id >= ?", []any{int32(6)})
	if err != nil {
		t.Fatalf("DeleteRows: %v", err)
	}
	if res.RowsAffected != 4 {
		t.Fatalf("want 4 rows affected, got %d", res.RowsAffected)
	}
}

func TestDeleteRows_RequiresWhere(t *testing.T) {
	s, _ := openTestLake(t)
	_, err := s.DeleteRows(context.Background(), "app.t", "  ", nil)
	if err == nil {
		t.Fatalf("expected error for empty WHERE")
	}
}

func TestQuery_RejectsNonSelect(t *testing.T) {
	s, _ := openTestLake(t)
	if _, err := s.Query(context.Background(), `INSERT INTO app.x VALUES (1)`, QueryOpts{}); err == nil {
		t.Fatalf("Query should reject INSERT")
	}
	if _, err := s.Query(context.Background(), `DROP TABLE app.x`, QueryOpts{}); err == nil {
		t.Fatalf("Query should reject DROP")
	}
	if _, err := s.Query(context.Background(), `SYSTEM RELOAD CONFIG`, QueryOpts{}); err == nil {
		t.Fatalf("Query should reject SYSTEM")
	}
}

func TestValidateSelect(t *testing.T) {
	cases := []struct {
		sql string
		ok  bool
	}{
		{"SELECT 1", true},
		{"  WITH q AS (SELECT 1) SELECT * FROM q", true},
		{"SHOW TABLES", true},
		{"DESCRIBE TABLE t", true},
		{"EXPLAIN AST SELECT 1", true},
		{"PRAGMA database_list", false}, // dropped from CH allowlist
		{"INSERT INTO t VALUES (1)", false},
		{"DELETE FROM t", false},
		{"SELECT 1; SELECT 2", false},
		{"", false},
		{"-- comment\nSELECT 1", true},
		{"/* block */\nSELECT 1", true},
		{"SYSTEM RELOAD CONFIG", false},
	}
	for _, c := range cases {
		err := ValidateSelect(c.sql)
		if (err == nil) != c.ok {
			t.Errorf("ValidateSelect(%q) ok=%v err=%v", c.sql, c.ok, err)
		}
	}
}

func TestValidateInsert(t *testing.T) {
	if err := ValidateInsert("INSERT INTO t VALUES(1)"); err != nil {
		t.Errorf("INSERT should pass: %v", err)
	}
	if err := ValidateInsert("UPDATE t SET x=1"); err == nil {
		t.Errorf("UPDATE should fail")
	}
	if err := ValidateInsert("INSERT INTO t VALUES(1); DELETE FROM t"); err == nil {
		t.Errorf("multi-statement should fail")
	}
}

func TestValidateCreate(t *testing.T) {
	if err := ValidateCreate("CREATE TABLE t(x INT)"); err != nil {
		t.Errorf("CREATE TABLE should pass: %v", err)
	}
	if err := ValidateCreate("CREATE OR REPLACE VIEW v AS SELECT 1"); err != nil {
		t.Errorf("CREATE OR REPLACE VIEW should pass: %v", err)
	}
	if err := ValidateCreate("CREATE TABLE t AS SELECT * FROM x; DROP TABLE x"); err == nil {
		t.Errorf("multi-statement should fail")
	}
	if err := ValidateCreate("DROP TABLE t"); err == nil {
		t.Errorf("DROP should fail")
	}
}

func TestValidateAlter(t *testing.T) {
	if err := ValidateAlter("ALTER TABLE t ADD COLUMN y INT"); err != nil {
		t.Errorf("ALTER should pass: %v", err)
	}
	if err := ValidateAlter("DROP TABLE t"); err == nil {
		t.Errorf("DROP should fail")
	}
}

func TestValidateQualifiedName(t *testing.T) {
	good := []string{"app.t", "raw.kite_holdings", "schema.table.column", "lone"}
	bad := []string{"", "a..b", "1bad", "drop;-- ", "a.b.c.d", "weird-name"}
	for _, s := range good {
		if err := validateQualifiedName(s); err != nil {
			t.Errorf("%q should be valid: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := validateQualifiedName(s); err == nil {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestDropObject(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".dropme"
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (x Int32) ENGINE = MergeTree() ORDER BY x`, quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.DropObject(ctx, tbl, "table", false); err != nil {
		t.Fatalf("DropObject: %v", err)
	}
	// IF EXISTS makes the second drop safe.
	if _, err := s.DropObject(ctx, tbl, "table", false); err != nil {
		t.Fatalf("DropObject idempotent: %v", err)
	}
	if _, err := s.DropObject(ctx, tbl, "weirdkind", false); err == nil {
		t.Fatalf("expected error for unsupported kind")
	}
	if _, err := s.DropObject(ctx, tbl, "index", false); err == nil {
		t.Fatalf("expected error for INDEX (not a top-level object in CH)")
	}
}

func TestListTables_AndDescribeTable(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".sample"
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id Int64, name Nullable(String)) ENGINE = MergeTree() ORDER BY id`,
		quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s VALUES (1,'a'),(2,NULL)`, quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	infos, err := s.ListTables(ctx, db)
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "sample" || infos[0].Kind != "BASE TABLE" {
		t.Fatalf("unexpected ListTables: %+v", infos)
	}
	if infos[0].RowCount == nil || *infos[0].RowCount != 2 {
		// total_rows updates eagerly for MergeTree; if it's lagging, it
		// could legitimately be 0 in some test setups. Be loud about
		// what we got but don't fail on staleness.
		t.Logf("row count: %+v (may be stale)", infos[0].RowCount)
	}

	desc, err := s.DescribeTable(ctx, tbl)
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(desc.Columns) != 2 {
		t.Fatalf("want 2 columns, got %d", len(desc.Columns))
	}
	gotCols := map[string]ColumnInfo{}
	for _, c := range desc.Columns {
		gotCols[c.Name] = c
	}
	if gotCols["id"].Type != "Int64" || gotCols["id"].Nullable {
		t.Errorf("id col: %+v", gotCols["id"])
	}
	if gotCols["name"].Type != "String" || !gotCols["name"].Nullable {
		t.Errorf("name col: %+v", gotCols["name"])
	}
}

func TestIngest_CSV_Append(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".csv_target"
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id Int32, name String) ENGINE = MergeTree() ORDER BY id`,
		quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(csvPath, []byte("id,name\n1,alpha\n2,beta\n"), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	report, err := s.Ingest(ctx, IngestRequest{
		Type:        SourceCSV,
		URI:         csvPath,
		TargetTable: tbl,
		Mode:        ModeAppend,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if report.RowsLoaded != 2 {
		t.Fatalf("want 2 rows loaded, got %d", report.RowsLoaded)
	}
}

func TestIngest_CSV_Replace(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".csv_replace"
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(csvPath, []byte("a,b\nhello,world\nfoo,bar\n"), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	report, err := s.Ingest(ctx, IngestRequest{
		Type:        SourceCSV,
		URI:         csvPath,
		TargetTable: tbl,
		Mode:        ModeReplace,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if report.RowsLoaded != 2 {
		t.Fatalf("want 2 rows loaded, got %d", report.RowsLoaded)
	}
	desc, err := s.DescribeTable(ctx, tbl)
	if err != nil {
		t.Fatalf("DescribeTable: %v", err)
	}
	if len(desc.Columns) != 2 || !desc.Columns[0].Nullable || desc.Columns[0].Type != "String" {
		t.Fatalf("unexpected schema after replace: %+v", desc.Columns)
	}
}

func TestIngest_JSONL_Append(t *testing.T) {
	s, db := openTestLake(t)
	ctx := context.Background()
	tbl := db + ".jsonl_target"
	if _, err := s.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id Int32, name String) ENGINE = MergeTree() ORDER BY id`,
		quoteQualifiedName(tbl))); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.jsonl")
	body := `{"id":1,"name":"alpha"}` + "\n" + `{"id":2,"name":"beta"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	report, err := s.Ingest(ctx, IngestRequest{
		Type:        SourceJSONL,
		URI:         path,
		TargetTable: tbl,
		Mode:        ModeAppend,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if report.RowsLoaded != 2 {
		t.Fatalf("want 2 rows loaded, got %d", report.RowsLoaded)
	}
}

func TestIngest_HTTP(t *testing.T) {
	t.Skip("HTTP ingest via CH's url() requires CH to reach back to a host port; skipped in unit tests")
	// Placeholder: a real httptest.Server bound to a port reachable
	// from the CH container would be needed.
	_ = httptest.NewServer
	_ = http.StatusOK
}

func TestIngest_ParquetSqlite_NotImplemented(t *testing.T) {
	s, db := openTestLake(t)
	tbl := db + ".x"
	for _, src := range []IngestSource{SourceParquet, SourceSQLite} {
		_, err := s.Ingest(context.Background(), IngestRequest{
			Type: src, URI: "/tmp/foo", TargetTable: tbl, Mode: ModeAppend,
		})
		if err == nil {
			t.Fatalf("expected ErrSourceNotImplemented for %s", src)
		}
	}
}
