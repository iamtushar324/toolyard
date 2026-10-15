package dbcopy

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// These tests use SQLite only (the source engine), so they run in both CI
// jobs.

const notShown = "hunter2-do-not-print"

func openSQLite(t *testing.T, name string) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func encode(t *testing.T, c column, vals ...any) string {
	t.Helper()
	var b []byte
	for _, v := range vals {
		var err error
		if b, err = appendCanonical(b, v, c); err != nil {
			t.Fatalf("encode %T: %v", v, err)
		}
	}
	return string(b)
}

// Equal values encode equally across engines; values Copy would refuse
// never equal a value of the column's type.
func TestCanonicalEncodingFollowsTheTargetType(t *testing.T) {
	text := column{Name: "c", Kind: kindText, Nullable: true}
	integer := column{Name: "c", Kind: kindInt, Bits: 64, Nullable: true}
	bytea := column{Name: "c", Kind: kindBytes, Nullable: true}
	double := column{Name: "c", Kind: kindFloat, Nullable: true}
	cases := []struct {
		name  string
		a, b  any
		col   column
		equal bool
	}{
		{"text equals the same bytes stored as a blob", "abc", []byte("abc"), text, true},
		{"an integer in a text column is not its text", int64(1), "1", text, false},
		{"a real in a text column is not its text", 1.5, "1.5", text, false},
		{"NULL is not empty text", nil, "", text, false},
		{"a clean integer string is the integer", "12", int64(12), integer, true},
		{"a negative one too", "-7", int64(-7), integer, true},
		{"padded text is not an integer", " 12", int64(12), integer, false},
		{"a real is not an integer", float64(12), int64(12), integer, false},
		{"an exact integer is the double", int64(3), float64(3), double, true},
		{"negative zero is not zero", math.Copysign(0, -1), 0.0, double, false},
		{"text is not bytea", "\x01\x02", []byte{1, 2}, bytea, false},
		{"bytea equals bytea", []byte{1, 2}, []byte{1, 2}, bytea, true},
	}
	for _, c := range cases {
		if got := encode(t, c.col, c.a) == encode(t, c.col, c.b); got != c.equal {
			t.Errorf("%s: equal = %v", c.name, got)
		}
	}
	// Values are length-delimited: ("ab", "c") is not ("a", "bc").
	if encode(t, text, "ab", "c") == encode(t, text, "a", "bc") {
		t.Fatal("two rows with shifted text encode equally")
	}
}

// coerce refuses anything that does not fit, with a reason that never
// quotes the value, and accepts the documented conversions.
func TestCoerceIsStrictAndNeverQuotesTheValue(t *testing.T) {
	refused := []struct {
		v   any
		col column
	}{
		{notShown, column{Kind: kindInt, Bits: 64}},
		{[]byte(notShown), column{Kind: kindInt, Bits: 64}},
		{notShown, column{Kind: kindBytes}},
		{notShown + "\x00", column{Kind: kindText}},
		{"\xff" + notShown, column{Kind: kindText}},
		{[]byte("\xfe" + notShown), column{Kind: kindText}},
		{int64(42), column{Kind: kindText}},
		{1.5, column{Kind: kindInt, Bits: 64}},
		{int64(1) << 40, column{Kind: kindInt, Bits: 32}},
		{int64(1)<<53 + 1, column{Kind: kindFloat}},
		{nil, column{Kind: kindText}},
	}
	for i, c := range refused {
		v, reason := coerce(c.v, c.col)
		if reason == "" || v != nil {
			t.Errorf("case %d (%T into %s) accepted", i, c.v, c.col.Kind)
		}
		if strings.Contains(reason, notShown) {
			t.Errorf("case %d: the reason quotes the value: %s", i, reason)
		}
	}
	accepted := []struct {
		v    any
		col  column
		want any
	}{
		{[]byte("héllo"), column{Kind: kindText}, "héllo"},
		{"123", column{Kind: kindInt, Bits: 64}, int64(123)},
		{int64(2), column{Kind: kindFloat}, float64(2)},
		{int64(-5), column{Kind: kindInt, Bits: 32}, int64(-5)},
		{nil, column{Kind: kindText, Nullable: true}, nil},
	}
	for i, c := range accepted {
		v, reason := coerce(c.v, c.col)
		if reason != "" || v != c.want {
			t.Errorf("case %d: %v %q, want %v", i, v, reason, c.want)
		}
	}
	if v, reason := coerce([]byte{1, 2}, column{Kind: kindBytes}); reason != "" || string(v.([]byte)) != "\x01\x02" {
		t.Errorf("bytea: %v %q", v, reason)
	}
}

// A type error names the table, the column and the primary key, never the
// value; when the value is part of the key, a row number stands in.
func TestCellErrorNamesTheRowNotTheValue(t *testing.T) {
	tb := &table{Name: "secrets", PK: []int{0}, Cols: []column{
		{Name: "name", Kind: kindText},
		{Name: "value_enc", Kind: kindText},
		{Name: "updated_at", Kind: kindInt, Bits: 64},
	}}
	vals := []any{"API_KEY", "sealed", notShown}
	_, reason := coerce(vals[2], tb.Cols[2])
	msg := (&CellError{Table: tb.Name, Column: "updated_at", Row: tb.rowRef(vals, 2, 4), Reason: reason}).Error()
	for _, want := range []string{"secrets.updated_at", `name="API_KEY"`, "is text"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, notShown) {
		t.Fatalf("the error shows the value: %s", msg)
	}
	ref := tb.rowRef([]any{notShown, "x", int64(1)}, 0, 4)
	if !strings.Contains(ref, "row 4") || strings.Contains(ref, notShown) {
		t.Fatalf("key column failing: %s", ref)
	}
}

func systemSettingsTable() *table {
	tb := &table{Name: "system_settings", PK: []int{0}, Cols: []column{
		{Name: "key", Kind: kindText},
		{Name: "value", Kind: kindText},
		{Name: "updated_at", Kind: kindInt, Bits: 64},
	}}
	tb.applySkips()
	return tb
}

// The table hash does not depend on insert order or on SQLite keeping a
// value as a blob, leaves out skipped rows, and changes with any value.
func TestTableDigestIgnoresOrderAndFollowsSkipRules(t *testing.T) {
	ctx := context.Background()
	a, b := openSQLite(t, "a.db"), openSQLite(t, "b.db")
	put := func(db *store.DB, key string, value any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO system_settings(key, value, updated_at) VALUES(?, ?, 1)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	put(a, "alpha", `"1"`)
	put(a, "beta", `"2"`)
	put(a, "gamma", `"3"`)
	put(a, "clickhouse_password", `"`+notShown+`"`) // skipped
	put(b, "gamma", `"3"`)
	put(b, "beta", []byte(`"2"`)) // SQLite keeps a []byte argument as a blob
	put(b, "alpha", `"1"`)

	tb := systemSettingsTable()
	da, err := digestTable(ctx, a, tb)
	if err != nil {
		t.Fatal(err)
	}
	db, err := digestTable(ctx, b, tb)
	if err != nil {
		t.Fatal(err)
	}
	if da.SHA256 != db.SHA256 || da.Rows != 3 || db.Rows != 3 || da.Skipped != 1 || db.Skipped != 0 {
		t.Fatalf("digests: %+v vs %+v", da, db)
	}
	if _, err := b.ExecContext(ctx, `UPDATE system_settings SET value = '"4"' WHERE key = 'gamma'`); err != nil {
		t.Fatal(err)
	}
	if d2, err := digestTable(ctx, b, tb); err != nil || d2.SHA256 == da.SHA256 {
		t.Fatalf("a changed value kept the digest: %v", err)
	}
}

// Source tables come parents first; the excluded tables never appear.
func TestSourceTablesOrderParentsFirst(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t, "src.db")
	all, err := readNames(ctx, src, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range all {
		if _, ex := excludedWhy(n); !ex {
			names = append(names, n)
		}
	}
	parents, err := readParents(ctx, src, names)
	if err != nil {
		t.Fatal(err)
	}
	order, err := orderByParents(names, parents)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, n := range order {
		at[n] = i
	}
	for _, n := range []string{"schema_migrations", "sqlite_sequence", "lake_ingest_runs", "lake_saved_queries", "inbox_blob_objects", "inbox_blob_chunks"} {
		if _, ok := at[n]; ok {
			t.Errorf("excluded table %s is in the copy order", n)
		}
	}
	for _, pc := range [][2]string{
		{"users", "user_server_access"},
		{"users", "user_identity_keys"},
		{"agents", "user_identity_keys"},
		{"upstream_servers", "oauth_tokens"},
		{"event_sources", "events"},
		{"inbox_requests", "callback_subscriptions"},
		{"callback_outbox", "callback_attempts"},
	} {
		p, okp := at[pc[0]]
		c, okc := at[pc[1]]
		if !okp || !okc || p > c {
			t.Errorf("%s (%d, %v) must come before %s (%d, %v)", pc[0], p, okp, pc[1], c, okc)
		}
	}
	if _, err := orderByParents([]string{"a", "b"}, map[string][]string{"a": {"b"}, "b": {"a"}}); err == nil {
		t.Fatal("a foreign-key cycle was ordered")
	}
}

// Copy and Verify refuse a target that is not Postgres before reading
// anything.
func TestCopyAndVerifyNeedAPostgresTarget(t *testing.T) {
	ctx := context.Background()
	srcDB := openSQLite(t, "src.db")
	dst := openSQLite(t, "dst.db")
	if _, err := Copy(ctx, srcDB.DB, t.TempDir(), dst, CopyOptions{}); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("copy into SQLite: %v", err)
	}
	if _, err := Verify(ctx, srcDB.DB, t.TempDir(), dst, nil, VerifyOptions{}); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("verify against SQLite: %v", err)
	}
}

// The JSON report carries "ok" and the problems.
func TestReportJSON(t *testing.T) {
	r := newReport("verify")
	b, err := r.MarshalJSON()
	if err != nil || !strings.Contains(string(b), `"ok":true`) || !strings.Contains(string(b), `"command":"verify"`) {
		t.Fatalf("%s %v", b, err)
	}
	r.problemf("table %s differs", "users")
	if b, _ = r.MarshalJSON(); !strings.Contains(string(b), `"ok":false`) || !strings.Contains(string(b), "table users differs") || r.OK() {
		t.Fatalf("%s", b)
	}
}
