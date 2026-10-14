package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// TestSchemaParity: a fully migrated Postgres database has the same schema
// as a fully migrated SQLite database: the same tables, columns (type class,
// nullability, default, identity), primary keys, UNIQUE constraints,
// foreign keys, CHECK counts and explicit indexes (name, uniqueness,
// columns, partial or not). Only the dormant lake items are SQLite-only.
func TestSchemaParity(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	lite, err := store.Open(filepath.Join(t.TempDir(), "parity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	pg := storetest.Open(t)

	ls := sqliteSchema(t, ctx, lite.DB)
	ps := postgresSchema(t, ctx, pg.DB)

	// Dormant lake items: in SQLite, left out of the Postgres baseline.
	for _, name := range []string{"lake_ingest_runs", "lake_saved_queries", "sqlite_sequence"} {
		delete(ls.tables, name)
	}
	if ev, ok := ls.tables["events"]; ok {
		delete(ev.columns, "lake_synced")
		delete(ev.indexes, "idx_events_unsynced")
	}

	var problems []string
	for _, name := range sortedKeys(ls.tables) {
		if _, ok := ps.tables[name]; !ok {
			problems = append(problems, "table "+name+" is missing on Postgres")
		}
	}
	for _, name := range sortedKeys(ps.tables) {
		lt, ok := ls.tables[name]
		if !ok {
			problems = append(problems, "table "+name+" exists only on Postgres")
			continue
		}
		problems = append(problems, compareTable(name, lt, ps.tables[name])...)
	}
	if len(problems) > 0 {
		t.Fatalf("schema differs between SQLite and Postgres:\n  %s", strings.Join(problems, "\n  "))
	}
	if len(ps.tables) < 70 {
		t.Fatalf("only %d tables on Postgres: the schema reader found too little", len(ps.tables))
	}
}

type schema struct{ tables map[string]*tableShape }

type tableShape struct {
	columns map[string]string // name -> "class null|notnull default=<d> [identity]"
	pk      string            // ordered columns, comma-separated
	uniques []string          // sorted; each is ordered columns, comma-separated
	fks     []string          // sorted; "col->table.col on delete ACTION"
	checks  int
	indexes map[string]string // explicit indexes: name -> "unique|plain partial|full cols"
}

func compareTable(name string, l, p *tableShape) []string {
	var out []string
	for _, c := range sortedKeys(l.columns) {
		pc, ok := p.columns[c]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s.%s is missing on Postgres", name, c))
		case pc != l.columns[c]:
			out = append(out, fmt.Sprintf("%s.%s: sqlite %q, postgres %q", name, c, l.columns[c], pc))
		}
	}
	for _, c := range sortedKeys(p.columns) {
		if _, ok := l.columns[c]; !ok {
			out = append(out, fmt.Sprintf("%s.%s exists only on Postgres", name, c))
		}
	}
	if l.pk != p.pk {
		out = append(out, fmt.Sprintf("%s primary key: sqlite (%s), postgres (%s)", name, l.pk, p.pk))
	}
	if a, b := strings.Join(l.uniques, " | "), strings.Join(p.uniques, " | "); a != b {
		out = append(out, fmt.Sprintf("%s UNIQUE constraints: sqlite [%s], postgres [%s]", name, a, b))
	}
	if a, b := strings.Join(l.fks, " | "), strings.Join(p.fks, " | "); a != b {
		out = append(out, fmt.Sprintf("%s foreign keys: sqlite [%s], postgres [%s]", name, a, b))
	}
	if l.checks != p.checks {
		out = append(out, fmt.Sprintf("%s CHECK constraints: sqlite %d, postgres %d", name, l.checks, p.checks))
	}
	for _, ix := range sortedKeys(l.indexes) {
		pi, ok := p.indexes[ix]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("index %s on %s is missing on Postgres", ix, name))
		case pi != l.indexes[ix]:
			out = append(out, fmt.Sprintf("index %s on %s: sqlite %q, postgres %q", ix, name, l.indexes[ix], pi))
		}
	}
	for _, ix := range sortedKeys(p.indexes) {
		if _, ok := l.indexes[ix]; !ok {
			out = append(out, fmt.Sprintf("index %s on %s exists only on Postgres", ix, name))
		}
	}
	return out
}

var (
	plainColumn  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	checkClause  = regexp.MustCompile(`(?i)\bCHECK\s*\(`) // not the check_blob column
	quotedNumber = regexp.MustCompile(`^'-?[0-9]+'$`)
)

// indexColumn is a column name, or "<expr>" for an expression.
func indexColumn(name string, desc bool) string {
	name = strings.Trim(name, `"`)
	if !plainColumn.MatchString(name) {
		name = "<expr>"
	}
	if desc {
		name += " DESC"
	}
	return name
}

// normDefault makes SQLite and Postgres column defaults comparable: literals
// stay as written, and Postgres casts (::text, ::bigint) are dropped.
func normDefault(d sql.NullString) string {
	if !d.Valid {
		return ""
	}
	s := strings.TrimSpace(d.String)
	for {
		i := strings.LastIndex(s, "::")
		if i < 0 || strings.ContainsAny(s[i:], "'()") {
			break
		}
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if quotedNumber.MatchString(s) { // '0'::bigint
		s = strings.Trim(s, "'")
	}
	return s
}

func columnShape(class string, notNull bool, def sql.NullString, identity bool) string {
	null := "null"
	if notNull {
		null = "notnull"
	}
	s := class + " " + null
	if identity {
		return s + " identity"
	}
	if d := normDefault(def); d != "" {
		s += " default=" + d
	}
	return s
}

func sqliteClass(typ string) string {
	switch strings.ToUpper(typ) {
	case "INTEGER":
		return "int"
	case "TEXT":
		return "text"
	case "BLOB":
		return "bytes"
	case "REAL":
		return "float"
	}
	return "sqlite:" + typ
}

func postgresClass(typ string) string {
	switch typ {
	case "bigint":
		return "int"
	case "text":
		return "text"
	case "bytea":
		return "bytes"
	case "double precision":
		return "float"
	}
	return "postgres:" + typ // integer, boolean, jsonb, ... are mismatches
}

func sqliteSchema(t *testing.T, ctx context.Context, db *sql.DB) schema {
	t.Helper()
	s := schema{tables: map[string]*tableShape{}}
	type tbl struct{ name, sql string }
	var tables []tbl
	for _, row := range queryStrings(t, ctx, db, 2, `SELECT name, sql FROM sqlite_master WHERE type = 'table'`) {
		tables = append(tables, tbl{row[0], row[1]})
	}
	for _, tb := range tables {
		shape := &tableShape{columns: map[string]string{}, indexes: map[string]string{}}
		s.tables[tb.name] = shape
		autoinc := strings.Contains(strings.ToUpper(tb.sql), "AUTOINCREMENT")
		shape.checks = len(checkClause.FindAllString(tb.sql, -1))

		rows, err := db.QueryContext(ctx, `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, tb.name)
		if err != nil {
			t.Fatal(err)
		}
		type pkCol struct {
			name string
			pos  int
		}
		var pks []pkCol
		for rows.Next() {
			var name, typ string
			var notNull, pk int
			var def sql.NullString
			if err := rows.Scan(&name, &typ, &notNull, &def, &pk); err != nil {
				t.Fatal(err)
			}
			// A primary key column is NOT NULL on Postgres; SQLite only
			// says so when it was written out.
			shape.columns[name] = columnShape(sqliteClass(typ), notNull == 1 || pk > 0, def, autoinc && pk > 0)
			if pk > 0 {
				pks = append(pks, pkCol{name, pk})
			}
		}
		closeRows(t, rows)
		sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
		var pkNames []string
		for _, p := range pks {
			pkNames = append(pkNames, p.name)
		}
		shape.pk = strings.Join(pkNames, ",")

		for _, fk := range queryStrings(t, ctx, db, 4, `SELECT "from", "table", "to", on_delete FROM pragma_foreign_key_list(?)`, tb.name) {
			shape.fks = append(shape.fks, fmt.Sprintf("%s->%s.%s on delete %s", fk[0], fk[1], fk[2], fk[3]))
		}
		sort.Strings(shape.fks)

		for _, ix := range queryStrings(t, ctx, db, 4, `SELECT name, CAST("unique" AS TEXT), origin, CAST(partial AS TEXT) FROM pragma_index_list(?)`, tb.name) {
			name, unique, origin, partial := ix[0], ix[1] == "1", ix[2], ix[3] == "1"
			var cols []string
			xrows, err := db.QueryContext(ctx, `SELECT name, "desc" FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`, name)
			if err != nil {
				t.Fatal(err)
			}
			for xrows.Next() {
				var col sql.NullString
				var desc int
				if err := xrows.Scan(&col, &desc); err != nil {
					t.Fatal(err)
				}
				cols = append(cols, indexColumn(col.String, desc == 1))
			}
			closeRows(t, xrows)
			switch origin {
			case "u":
				shape.uniques = append(shape.uniques, strings.Join(cols, ","))
			case "c":
				shape.indexes[name] = indexShape(unique, partial, cols)
			}
		}
		sort.Strings(shape.uniques)
	}
	return s
}

func postgresSchema(t *testing.T, ctx context.Context, db *sql.DB) schema {
	t.Helper()
	s := schema{tables: map[string]*tableShape{}}
	tables := queryStrings(t, ctx, db, 1, `SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	for _, row := range tables {
		name := row[0]
		shape := &tableShape{columns: map[string]string{}, indexes: map[string]string{}}
		s.tables[name] = shape

		rows, err := db.QueryContext(ctx, `SELECT column_name::text, data_type::text, is_nullable::text, column_default::text, is_identity::text
			FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = CAST(? AS TEXT)`, name)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col, typ, nullable, identity string
			var def sql.NullString
			if err := rows.Scan(&col, &typ, &nullable, &def, &identity); err != nil {
				t.Fatal(err)
			}
			shape.columns[col] = columnShape(postgresClass(typ), nullable == "NO", def, identity == "YES")
		}
		closeRows(t, rows)

		cons := queryStrings(t, ctx, db, 2, `SELECT k.contype::text,
				array_to_string(ARRAY(SELECT a.attname::text FROM unnest(k.conkey) WITH ORDINALITY AS u(attnum, ord)
					JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = u.attnum ORDER BY u.ord), ',')
			FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = CAST(? AS TEXT) AND k.contype IN ('p', 'u', 'c')`, name)
		for _, c := range cons {
			switch c[0] {
			case "p":
				shape.pk = c[1]
			case "u":
				shape.uniques = append(shape.uniques, c[1])
			case "c":
				shape.checks++
			}
		}
		sort.Strings(shape.uniques)

		for _, fk := range queryStrings(t, ctx, db, 4, `SELECT a.attname::text, cf.relname::text, af.attname::text, k.confdeltype::text
			FROM pg_constraint k
			JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = k.conkey[1]
			JOIN pg_class cf ON cf.oid = k.confrelid
			JOIN pg_attribute af ON af.attrelid = k.confrelid AND af.attnum = k.confkey[1]
			WHERE k.contype = 'f' AND n.nspname = current_schema() AND c.relname = CAST(? AS TEXT)`, name) {
			action := map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[fk[3]]
			shape.fks = append(shape.fks, fmt.Sprintf("%s->%s.%s on delete %s", fk[0], fk[1], fk[2], action))
		}
		sort.Strings(shape.fks)

		// Explicit indexes only: not the ones behind a PRIMARY KEY or UNIQUE constraint.
		for _, ix := range queryStrings(t, ctx, db, 4, `SELECT ic.relname::text, CASE WHEN i.indisunique THEN '1' ELSE '0' END,
				CASE WHEN i.indpred IS NULL THEN '0' ELSE '1' END,
				array_to_string(ARRAY(SELECT pg_get_indexdef(i.indexrelid, k, true) ||
					CASE WHEN (i.indoption[k - 1] & 1) = 1 THEN ' DESC' ELSE '' END
					FROM generate_series(1, i.indnkeyatts) AS k ORDER BY k), E'\t')
			FROM pg_index i
			JOIN pg_class ic ON ic.oid = i.indexrelid
			JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = CAST(? AS TEXT)
			  AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid)`, name) {
			var cols []string
			for _, col := range strings.Split(ix[3], "\t") {
				desc := strings.HasSuffix(col, " DESC")
				cols = append(cols, indexColumn(strings.TrimSuffix(col, " DESC"), desc))
			}
			shape.indexes[ix[0]] = indexShape(ix[1] == "1", ix[2] == "1", cols)
		}
	}
	return s
}

func indexShape(unique, partial bool, cols []string) string {
	kind, where := "plain", "full"
	if unique {
		kind = "unique"
	}
	if partial {
		where = "partial"
	}
	return kind + " " + where + " (" + strings.Join(cols, ", ") + ")"
}

// queryStrings runs a query whose n columns are all text (NULL reads as "").
func queryStrings(t *testing.T, ctx context.Context, db *sql.DB, n int, query string, args ...any) [][]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, n)
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	closeRows(t, rows)
	return out
}

func closeRows(t *testing.T, rows *sql.Rows) {
	t.Helper()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
