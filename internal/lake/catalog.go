package lake

import (
	"context"
	"fmt"
	"strings"
)

// TableInfo is one row in the catalog listing.
type TableInfo struct {
	Schema   string `json:"schema"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // BASE TABLE | VIEW
	RowCount *int64 `json:"row_count,omitempty"`
}

// ListTables returns every table+view in the lake. If schema is non-empty,
// the listing is filtered to that database. The CH system tables already
// exclude `information_schema` style entries; we also drop the built-in
// `system` and `INFORMATION_SCHEMA` databases so callers don't see CH-
// internal plumbing.
func (s *Service) ListTables(ctx context.Context, schema string) ([]TableInfo, error) {
	q := `
		SELECT database,
		       name,
		       multiIf(engine = 'View', 'VIEW',
		               engine = 'MaterializedView', 'VIEW',
		               'BASE TABLE') AS kind,
		       toInt64(total_rows) AS row_count
		  FROM system.tables
		 WHERE database NOT IN ('system','INFORMATION_SCHEMA','information_schema')`
	args := []any{}
	if schema != "" {
		q += ` AND database = ?`
		args = append(args, schema)
	}
	q += ` ORDER BY database, name`

	rows, err := s.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListTables: %w", err)
	}
	defer rows.Close()
	out := []TableInfo{}
	for rows.Next() {
		var (
			t TableInfo
			n int64
		)
		if err := rows.Scan(&t.Schema, &t.Name, &t.Kind, &n); err != nil {
			return nil, err
		}
		// `total_rows` is unreliable for views (CH returns 0); only
		// attach it for base tables.
		if t.Kind == "BASE TABLE" {
			c := n
			t.RowCount = &c
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ColumnInfo is one column in DescribeTable's response.
type ColumnInfo struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable bool    `json:"nullable"`
	Default  *string `json:"default,omitempty"`
	Position int     `json:"position"`
}

// TableDescription is what DescribeTable returns.
type TableDescription struct {
	Schema   string       `json:"schema"`
	Name     string       `json:"name"`
	Kind     string       `json:"kind"`
	Columns  []ColumnInfo `json:"columns"`
	RowCount *int64       `json:"row_count,omitempty"`
}

// DescribeTable returns column metadata + row count for a single table.
// table may be "database.name" or "name" (resolved against the open
// connection's default database).
func (s *Service) DescribeTable(ctx context.Context, table string) (*TableDescription, error) {
	if err := validateQualifiedName(table); err != nil {
		return nil, err
	}
	schema, name := splitQualified(table, s.cfg.Database)

	desc := &TableDescription{Schema: schema, Name: name}

	// Resolve kind + verify existence via system.tables.
	row := s.conn.QueryRow(ctx, `
		SELECT database, name,
		       multiIf(engine = 'View', 'VIEW',
		               engine = 'MaterializedView', 'VIEW',
		               'BASE TABLE') AS kind,
		       toInt64(total_rows) AS row_count
		  FROM system.tables
		 WHERE database = ? AND name = ?`, schema, name)
	var rowCount int64
	if err := row.Scan(&desc.Schema, &desc.Name, &desc.Kind, &rowCount); err != nil {
		return nil, fmt.Errorf("DescribeTable %q: %w", table, err)
	}
	if desc.Kind == "BASE TABLE" {
		rc := rowCount
		desc.RowCount = &rc
	}

	// Columns.
	rows, err := s.conn.Query(ctx, `
		SELECT name,
		       type,
		       default_expression,
		       toInt32(position) AS position
		  FROM system.columns
		 WHERE database = ? AND table = ?
		 ORDER BY position`, desc.Schema, desc.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			c        ColumnInfo
			rawType  string
			defaultE string
			pos      int32
		)
		if err := rows.Scan(&c.Name, &rawType, &defaultE, &pos); err != nil {
			return nil, err
		}
		c.Position = int(pos)
		c.Type, c.Nullable = unwrapNullable(rawType)
		if defaultE != "" {
			d := defaultE
			c.Default = &d
		}
		desc.Columns = append(desc.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return desc, nil
}

// unwrapNullable strips a `Nullable(T)` wrapper and reports the inner type
// + a bool indicating whether the original was nullable.
func unwrapNullable(t string) (string, bool) {
	const prefix = "Nullable("
	if strings.HasPrefix(t, prefix) && strings.HasSuffix(t, ")") {
		return t[len(prefix) : len(t)-1], true
	}
	return t, false
}

// splitQualified returns (database, name). When unqualified, database
// defaults to the supplied default (the Service's configured Database).
func splitQualified(s, defaultDB string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:]
		}
	}
	return defaultDB, s
}
