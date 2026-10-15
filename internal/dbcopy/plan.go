package dbcopy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// excludedTables are never copied or compared, with the reason the report
// gives. Every name starting with sqlite_ is excluded too.
var excludedTables = map[string]string{
	"schema_migrations":  "migration bookkeeping: each database records its own, compared by name instead",
	"lake_ingest_runs":   "dormant ClickHouse lake table, left out of the Postgres schema",
	"lake_saved_queries": "dormant ClickHouse lake table, left out of the Postgres schema",
	"inbox_blob_objects": "blob store table: filled from the blob files and checked blob by blob",
	"inbox_blob_chunks":  "blob store table: filled from the blob files and checked blob by blob",
}

// blobTables hold the Postgres blob store. A fresh target has them empty;
// -truncate-target empties them with the copied tables.
var blobTables = []string{"inbox_blob_objects", "inbox_blob_chunks"}

// expectedDroppedColumns are source columns the Postgres schema leaves out
// on purpose. Any other source column the target lacks stops a copy: its
// data would be lost.
var expectedDroppedColumns = map[string]bool{
	"events.lake_synced": true,
}

// skipRule leaves out the source rows whose column holds value (as text):
// leftovers of the removed ClickHouse lake. Copy does not write them and
// Verify leaves them out on both sides.
type skipRule struct {
	Table, Column, Value string
}

func (r skipRule) String() string {
	return fmt.Sprintf("%s rows where %s = '%s'", r.Table, r.Column, r.Value)
}

var skipRules = []skipRule{
	{Table: "system_settings", Column: "key", Value: "clickhouse_password"},
	{Table: "user_server_access", Column: "server", Value: "lake"},
}

func excludedWhy(name string) (string, bool) {
	if strings.HasPrefix(name, "sqlite_") {
		return "SQLite internal table", true
	}
	why, ok := excludedTables[name]
	return why, ok
}

// table is one table copied or compared.
type table struct {
	Name string
	// Cols are the columns both databases have, in source order, typed as
	// the target holds them.
	Cols []column
	// PK indexes Cols: the source primary key in key order, or every
	// column when the table has none.
	PK []int
	// Identity names the target's identity (or serial) columns.
	Identity []string
	skips    []skipAt
}

type skipAt struct {
	col   int
	value string
}

func (t *table) colNames() []string {
	out := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		out[i] = c.Name
	}
	return out
}

// selectSQL reads t's columns, unordered (rows are sorted in Go).
func (t *table) selectSQL() string {
	q := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		q[i] = quoteIdent(c.Name)
	}
	return "SELECT " + strings.Join(q, ", ") + " FROM " + quoteIdent(t.Name)
}

// skipped reports whether a skip rule leaves out the row vals.
func (t *table) skipped(vals []any) bool {
	for _, s := range t.skips {
		switch x := vals[s.col].(type) {
		case string:
			if x == s.value {
				return true
			}
		case []byte:
			if string(x) == s.value {
				return true
			}
		}
	}
	return false
}

// applySkips attaches the skip rules for t's table to its columns.
func (t *table) applySkips() {
	t.skips = nil
	for _, r := range skipRules {
		if r.Table != t.Name {
			continue
		}
		for i, c := range t.Cols {
			if c.Name == r.Column {
				t.skips = append(t.skips, skipAt{col: i, value: r.Value})
			}
		}
	}
}

// rowRef names the row vals for an error: its primary key, or its row
// number when the failing column is part of the key (the key would then
// show the value).
func (t *table) rowRef(vals []any, failing int, rowNum int64) string {
	parts := make([]string, 0, len(t.PK))
	for _, i := range t.PK {
		if i == failing {
			return fmt.Sprintf("row %d of the table scan (the value is part of the primary key, so the key is not shown)", rowNum)
		}
		parts = append(parts, t.Cols[i].Name+"="+keyText(vals[i]))
	}
	return "primary key " + strings.Join(parts, ", ")
}

// keyText shows a primary-key value in an error. Keys are ids and names,
// never secrets; long ones are shortened and blobs show only their size.
func keyText(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		if len(x) > 64 {
			x = x[:64] + "..."
		}
		return strconv.Quote(x)
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	}
	return fmt.Sprintf("<%T>", v)
}

// CellError is a source value that does not fit its target column. Its
// message names the table, the column and the row's primary key, never the
// value.
type CellError struct {
	Table  string
	Column string
	Row    string // "primary key id=7", or a row number
	Reason string // what is wrong, without the value
}

func (e *CellError) Error() string {
	return fmt.Sprintf("db copy: %s.%s, %s: the value %s. Nothing was written.", e.Table, e.Column, e.Row, e.Reason)
}

// plan is what Copy and Verify work through: the tables both databases
// have, in an order that puts parents before children, and every
// difference between the two schemas.
type plan struct {
	Tables               []*table
	Migrations           int
	SourceOnlyMigrations []string
	TargetOnlyMigrations []string
	Excluded             []ExcludedTable
	MissingTables        []string
	TargetOnlyTables     []string
	DroppedColumns       []string
	UnexpectedDropped    []string
	TargetOnlyColumns    []string
	// BlobTables are the blob store tables the target has.
	BlobTables []string
}

type targetCol struct {
	name, dataType     string
	nullable, identity bool
}

// buildPlan reads both schemas. src is the SQLite source (read through its
// catalog: sqlite_master and the pragma table functions), dst the Postgres
// target (information_schema). It changes nothing.
func buildPlan(ctx context.Context, src, dst store.Querier) (*plan, error) {
	p := &plan{}
	srcMig, err := readNames(ctx, src, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read the source's migrations: %w", err)
	}
	dstMig, err := readNames(ctx, dst, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read the target's migrations: %w", err)
	}
	p.Migrations = len(srcMig)
	p.SourceOnlyMigrations, p.TargetOnlyMigrations = diffNames(srcMig, dstMig)

	srcTables, err := readNames(ctx, src, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("read the source's tables: %w", err)
	}
	dstTables, err := readNames(ctx, dst, `SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return nil, fmt.Errorf("read the target's tables: %w", err)
	}
	dstCols, err := readTargetColumns(ctx, dst)
	if err != nil {
		return nil, fmt.Errorf("read the target's columns: %w", err)
	}
	inSource := setOf(srcTables)
	inTarget := setOf(dstTables)
	for _, n := range dstTables {
		if _, ex := excludedWhy(n); ex {
			for _, b := range blobTables {
				if n == b {
					p.BlobTables = append(p.BlobTables, n)
				}
			}
			continue
		}
		if !inSource[n] {
			p.TargetOnlyTables = append(p.TargetOnlyTables, n)
		}
	}

	byName := map[string]*table{}
	var names []string
	for _, n := range srcTables {
		if why, ex := excludedWhy(n); ex {
			et := ExcludedTable{Name: n, SourceRows: -1, Why: why} // -1: not counted
			if plainIdent(n) {
				if err := src.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(n)).Scan(&et.SourceRows); err != nil {
					return nil, fmt.Errorf("count the source's %s: %w", n, err)
				}
			}
			p.Excluded = append(p.Excluded, et)
			continue
		}
		if !plainIdent(n) {
			return nil, fmt.Errorf("source table %q is not a plain lowercase name", n)
		}
		if !inTarget[n] {
			p.MissingTables = append(p.MissingTables, n)
			continue
		}
		t, err := planTable(ctx, src, n, dstCols[n], p)
		if err != nil {
			return nil, err
		}
		byName[n] = t
		names = append(names, n)
	}

	parents, err := readParents(ctx, src, names)
	if err != nil {
		return nil, err
	}
	order, err := orderByParents(names, parents)
	if err != nil {
		return nil, err
	}
	for _, n := range order {
		p.Tables = append(p.Tables, byName[n])
	}
	sort.Strings(p.DroppedColumns)
	sort.Strings(p.UnexpectedDropped)
	sort.Strings(p.TargetOnlyColumns)
	return p, nil
}

// planTable types the columns of one table both databases have.
func planTable(ctx context.Context, src store.Querier, name string, tcols []targetCol, p *plan) (*table, error) {
	type srcCol struct {
		name string
		pk   int
	}
	rows, err := src.QueryContext(ctx, `SELECT name, pk FROM pragma_table_info(?) ORDER BY cid`, name)
	if err != nil {
		return nil, fmt.Errorf("read the source's columns of %s: %w", name, err)
	}
	var scols []srcCol
	for rows.Next() {
		var c srcCol
		if err := rows.Scan(&c.name, &c.pk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the source's columns of %s: %w", name, err)
		}
		scols = append(scols, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read the source's columns of %s: %w", name, err)
	}
	rows.Close()

	byName := make(map[string]targetCol, len(tcols))
	for _, c := range tcols {
		byName[c.name] = c
	}
	t := &table{Name: name}
	inSource := map[string]bool{}
	pkAt := map[int]int{} // position in the source key -> index in t.Cols
	npk := 0
	for _, sc := range scols {
		inSource[sc.name] = true
		if sc.pk > 0 {
			npk++
		}
		tc, ok := byName[sc.name]
		if !ok {
			full := name + "." + sc.name
			p.DroppedColumns = append(p.DroppedColumns, full)
			if !expectedDroppedColumns[full] {
				p.UnexpectedDropped = append(p.UnexpectedDropped, full)
			}
			continue
		}
		if !plainIdent(sc.name) {
			return nil, fmt.Errorf("column %s.%q is not a plain lowercase name", name, sc.name)
		}
		col, err := columnOf(name, sc.name, tc.dataType, tc.nullable)
		if err != nil {
			return nil, err
		}
		if sc.pk > 0 {
			pkAt[sc.pk] = len(t.Cols)
		}
		t.Cols = append(t.Cols, col)
		if tc.identity {
			t.Identity = append(t.Identity, sc.name)
		}
	}
	for _, c := range tcols {
		if !inSource[c.name] {
			p.TargetOnlyColumns = append(p.TargetOnlyColumns, name+"."+c.name)
		}
	}
	if len(t.Cols) == 0 {
		return nil, fmt.Errorf("table %s has no column in both databases", name)
	}
	if npk > 0 && len(pkAt) == npk {
		for k := 1; k <= npk; k++ {
			i, ok := pkAt[k]
			if !ok {
				t.PK = nil
				break
			}
			t.PK = append(t.PK, i)
		}
	}
	if len(t.PK) == 0 {
		for i := range t.Cols {
			t.PK = append(t.PK, i)
		}
	}
	t.applySkips()
	return t, nil
}

func readTargetColumns(ctx context.Context, dst store.Querier) (map[string][]targetCol, error) {
	rows, err := dst.QueryContext(ctx, `SELECT table_name::text, column_name::text, data_type::text,
		is_nullable::text, is_identity::text, COALESCE(column_default::text, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]targetCol{}
	for rows.Next() {
		var tbl, nullable, identity, def string
		var c targetCol
		if err := rows.Scan(&tbl, &c.name, &c.dataType, &nullable, &identity, &def); err != nil {
			return nil, err
		}
		c.nullable = nullable == "YES"
		c.identity = identity == "YES" || strings.HasPrefix(def, "nextval(")
		out[tbl] = append(out[tbl], c)
	}
	return out, rows.Err()
}

// readParents reads, for each source table in names, the tables its
// foreign keys reference.
func readParents(ctx context.Context, src store.Querier, names []string) (map[string][]string, error) {
	parents := make(map[string][]string, len(names))
	for _, n := range names {
		ps, err := readNames(ctx, src, `SELECT "table" FROM pragma_foreign_key_list(?)`, n)
		if err != nil {
			return nil, fmt.Errorf("read the source's foreign keys of %s: %w", n, err)
		}
		parents[n] = ps
	}
	return parents, nil
}

// orderByParents orders names so every table comes after the tables its
// foreign keys reference (parents outside names are ignored), with ties in
// name order.
func orderByParents(names []string, parents map[string][]string) ([]string, error) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	want := setOf(sorted)
	done := map[string]bool{}
	out := make([]string, 0, len(sorted))
	for len(out) < len(sorted) {
		progressed := false
		for _, n := range sorted {
			if done[n] {
				continue
			}
			ready := true
			for _, par := range parents[n] {
				if par != n && want[par] && !done[par] {
					ready = false
					break
				}
			}
			if ready {
				done[n] = true
				out = append(out, n)
				progressed = true
			}
		}
		if !progressed {
			var rest []string
			for _, n := range sorted {
				if !done[n] {
					rest = append(rest, n)
				}
			}
			return nil, fmt.Errorf("the foreign keys between %s form a cycle", strings.Join(rest, ", "))
		}
	}
	return out, nil
}

// refusal is why Copy must not run on this plan, or nil.
func (p *plan) refusal() error {
	var why []string
	if len(p.SourceOnlyMigrations)+len(p.TargetOnlyMigrations) > 0 {
		why = append(why, fmt.Sprintf("the two databases are at different migrations (%d only in the source: %s; %d only in the target: %s). "+
			"Run this toolyard version once on the SQLite data directory (start and stop serve) so both are at the same level",
			len(p.SourceOnlyMigrations), listNames(p.SourceOnlyMigrations), len(p.TargetOnlyMigrations), listNames(p.TargetOnlyMigrations)))
	}
	if len(p.MissingTables) > 0 {
		why = append(why, fmt.Sprintf("the target has no table %s", listNames(p.MissingTables)))
	}
	if len(p.UnexpectedDropped) > 0 {
		why = append(why, fmt.Sprintf("the target has no column %s, so its data would be lost", listNames(p.UnexpectedDropped)))
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("db copy: refused, nothing was written: %s", strings.Join(why, "; "))
}

// fill copies the plan's findings into r and records its problems.
func (p *plan) fill(r *Report) {
	r.Migrations = p.Migrations
	r.MigrationsMatch = len(p.SourceOnlyMigrations)+len(p.TargetOnlyMigrations) == 0
	r.SourceOnlyMigrations = p.SourceOnlyMigrations
	r.TargetOnlyMigrations = p.TargetOnlyMigrations
	r.ExcludedTables = append(r.ExcludedTables, p.Excluded...)
	r.MissingTables = p.MissingTables
	r.TargetOnlyTables = p.TargetOnlyTables
	r.DroppedColumns = append(r.DroppedColumns, p.DroppedColumns...)
	r.TargetOnlyColumns = p.TargetOnlyColumns
	for _, s := range skipRules {
		r.SkipRules = append(r.SkipRules, s.String())
	}
	if !r.MigrationsMatch {
		r.problemf("migrations differ: %d only in the source (%s), %d only in the target (%s)",
			len(p.SourceOnlyMigrations), listNames(p.SourceOnlyMigrations), len(p.TargetOnlyMigrations), listNames(p.TargetOnlyMigrations))
	}
	if len(p.MissingTables) > 0 {
		r.problemf("%d source tables are missing in the target: %s", len(p.MissingTables), listNames(p.MissingTables))
	}
	if len(p.UnexpectedDropped) > 0 {
		r.problemf("%d source columns are missing in the target: %s", len(p.UnexpectedDropped), listNames(p.UnexpectedDropped))
	}
}

func (p *plan) tableNames() []string {
	out := make([]string, len(p.Tables))
	for i, t := range p.Tables {
		out[i] = t.Name
	}
	return out
}

// readNames reads the first column of every row as text, sorted.
func readNames(ctx context.Context, q store.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// diffNames returns the names only in a and only in b (both sorted).
func diffNames(a, b []string) (onlyA, onlyB []string) {
	inA, inB := setOf(a), setOf(b)
	for _, n := range a {
		if !inB[n] {
			onlyA = append(onlyA, n)
		}
	}
	for _, n := range b {
		if !inA[n] {
			onlyB = append(onlyB, n)
		}
	}
	return onlyA, onlyB
}

func setOf(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// plainIdent accepts the names toolyard's schema uses: lowercase letters,
// digits and _, not starting with a digit. Only such names are ever pasted
// into SQL here.
func plainIdent(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// quoteIdent quotes a plain identifier for SQLite and Postgres alike.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
