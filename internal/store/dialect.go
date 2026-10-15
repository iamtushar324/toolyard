package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// Dialect is the SQL engine behind a DB. The zero value is SQLite.
type Dialect int

const (
	SQLite Dialect = iota
	Postgres
)

// String returns "sqlite" or "postgres".
func (d Dialect) String() string {
	if d == Postgres {
		return "postgres"
	}
	return "sqlite"
}

// IsPostgres reports whether d is Postgres.
func (d Dialect) IsPostgres() bool { return d == Postgres }

// JSONText returns an SQL expression for the text value at the top-level key
// of the JSON text column col. SQLite: json_extract(col, '$.key'). Postgres:
// (replace(col, '\u0000', '\ufffd')::jsonb ->> 'key'): jsonb refuses the
// \u0000 escape json.Marshal writes for a NUL character, and the replace
// keeps the text valid JSON, so no stored doc makes the query fail (the
// inbox_client_request_key index uses the same expression). key must be a
// plain identifier ([A-Za-z0-9_]+), or JSONText panics: it is pasted into
// SQL. col is trusted SQL.
func (d Dialect) JSONText(col, key string) string {
	if !plainIdent(key) {
		panic(fmt.Sprintf("store: JSONText key %q is not a plain identifier", key))
	}
	if d == Postgres {
		return "(replace(" + col + `, '\u0000', '\ufffd')::jsonb ->> '` + key + "')"
	}
	return "json_extract(" + col + ", '$." + key + "')"
}

// ILike returns the case-insensitive LIKE operator: "LIKE" on SQLite, whose
// LIKE already ignores ASCII case, and "ILIKE" on Postgres.
func (d Dialect) ILike() string {
	if d == Postgres {
		return "ILIKE"
	}
	return "LIKE"
}

// ForUpdate returns the row-lock suffix for a SELECT inside a transaction:
// "" on SQLite (one connection, so the transaction already excludes other
// writers) and " FOR UPDATE" on Postgres. Note the leading space.
func (d Dialect) ForUpdate() string {
	if d == Postgres {
		return " FOR UPDATE"
	}
	return ""
}

func plainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// IsUniqueViolation reports a UNIQUE or PRIMARY KEY violation on either
// engine: sqlite3.ErrConstraintUnique or ErrConstraintPrimaryKey, or a
// Postgres error with SQLSTATE 23505. It unwraps err.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.ExtendedCode == sqlite3.ErrConstraintUnique || se.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return false
}

// Querier is satisfied by *sql.DB, *sql.Tx, *sql.Conn and *store.DB.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
	_ Querier = (*sql.Conn)(nil)
	_ Querier = (*DB)(nil)
)

// TableExists reports whether table exists: in sqlite_master on SQLite, and
// in information_schema.tables of current_schema() on Postgres. Views do
// not count.
func TableExists(ctx context.Context, q Querier, d Dialect, table string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`
	if d == Postgres {
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name = CAST(? AS TEXT))`
	}
	var ok bool
	if err := q.QueryRowContext(ctx, query, table).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
