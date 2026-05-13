// Package lake is toolyard's personal data lake, backed by ClickHouse.
//
// Was DuckDB until phase 2; see ADR 0004 + the Phase 2 brief
// (docs/migration/phase-2-rewrite-lake-against-clickhouse.md) for the rewrite
// rationale. The public surface (Service methods, Result/ExecResult/Column
// structs, IngestRequest/IngestReport, etc.) is preserved byte-compatible so
// that the gateway tool layer and HTTP routes in Phase 3 keep working with
// the same shapes.
//
// The lake holds analytical, append-mostly data — finances, health, ops — as
// distinct from the SQLite control plane (which holds approvals, audit, agents).
// A single CH connection (clickhouse-go/v2 native protocol) fronts the engine;
// read-only on lake.query is enforced by ValidateSelect at the API layer
// since CH has no per-connection read-only mode equivalent to DuckDB's.
package lake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Database names. Bootstrap and ingest write here; the dashboard reads from
// mart; agents write to app. In ClickHouse these are databases; we keep the
// DuckDB-era "Schema" naming on the constants so callers don't have to change.
const (
	SchemaRaw  = "raw"
	SchemaMart = "mart"
	SchemaApp  = "app"
)

// DefaultMaxRows is the row cap for a single Query call when the caller did
// not pass max_rows. AbsoluteMaxRows is the hard ceiling we never exceed even
// when the caller asks for more — keeps a runaway agent from streaming a
// million rows into context.
const (
	DefaultMaxRows  = 1000
	AbsoluteMaxRows = 10_000
	// MaxResponseBytes is a soft cap on the total serialized JSON size of a
	// query result. We stop appending rows once we cross it and set
	// truncated=true.
	MaxResponseBytes = 2 << 20 // 2 MiB
	// DefaultQueryTimeout caps Query/Exec calls so an analytical query
	// can't pin a connection forever.
	DefaultQueryTimeout = 30 * time.Second
)

// Config is the open-time configuration for a ClickHouse-backed lake.
type Config struct {
	// Addr is the host:port of the CH native protocol endpoint
	// (e.g. "127.0.0.1:19000" against the deploy/clickhouse compose,
	// or "clickhouse:9000" when reaching CH over the shared docker
	// network). Required.
	Addr string
	// User and Password authenticate to CH. User defaults to "default".
	User     string
	Password string
	// Database is the baseline database for unqualified names. Defaults to
	// "default". The standard raw/mart/app databases are created at Open
	// time regardless.
	Database string
	// DialTimeout caps the initial TCP+handshake. Defaults to 5s.
	DialTimeout time.Duration
}

// Service wraps a single ClickHouse connection. The native protocol's batch
// inserts (PrepareBatch) need a driver.Conn, not a *sql.DB, so we keep the
// raw driver handle.
type Service struct {
	conn     chdriver.Conn
	cfg      Config
	queryTTL time.Duration
}

// Open dials ClickHouse using cfg and returns a ready Service. The standard
// databases (raw, mart, app) are created idempotently. Returns an error if
// the connection or the bootstrap ping/exec fails.
func Open(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("lake.Open: Addr is required")
	}
	if cfg.User == "" {
		cfg.User = "default"
	}
	if cfg.Database == "" {
		cfg.Database = "default"
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.User,
			Password: cfg.Password,
		},
		DialTimeout: cfg.DialTimeout,
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		// CH server caps max_concurrent_queries=4 in our deployed config,
		// so keep the pool at or below that to avoid server-side queueing.
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: 10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("lake: open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("lake: ping: %w", err)
	}

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bootCancel()
	for _, db := range []string{SchemaRaw, SchemaMart, SchemaApp} {
		stmt := `CREATE DATABASE IF NOT EXISTS ` + quoteIdent(db)
		if err := conn.Exec(bootCtx, stmt); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("lake: ensure database %s: %w", db, err)
		}
	}

	return &Service{
		conn:     conn,
		cfg:      cfg,
		queryTTL: DefaultQueryTimeout,
	}, nil
}

// OpenFromEnv is a test convenience: opens against
// TOOLYARD_CH_TEST_ADDR / TOOLYARD_CH_TEST_USER / TOOLYARD_CH_TEST_PASSWORD.
// Returns (nil, nil) when TOOLYARD_CH_TEST_ADDR is unset so tests can skip.
func OpenFromEnv() (*Service, error) {
	addr := os.Getenv("TOOLYARD_CH_TEST_ADDR")
	if addr == "" {
		return nil, nil
	}
	user := os.Getenv("TOOLYARD_CH_TEST_USER")
	if user == "" {
		user = "default"
	}
	return Open(Config{
		Addr:     addr,
		User:     user,
		Password: os.Getenv("TOOLYARD_CH_TEST_PASSWORD"),
	})
}

// Close shuts down the underlying CH connection pool.
func (s *Service) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

// DB returns nil. Preserved for source-compat with the DuckDB-era surface;
// CH uses a native driver.Conn rather than *sql.DB. Phase 3 callers that
// need direct access should grow a Conn() accessor rather than reviving this.
func (s *Service) DB() *sql.DB { return nil }

// Column is one column in a query result.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Result is what Query returns: a column header, a row matrix, and metadata
// the caller needs to know whether it got everything.
type Result struct {
	Columns   []Column `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated"`
	ElapsedMs int64    `json:"elapsed_ms"`
	Error     string   `json:"error,omitempty"`
	// SizeBytes is the approximate serialized size of the included rows.
	SizeBytes int `json:"size_bytes"`
}

// QueryOpts captures the dial-able knobs Query exposes. Zero values give the
// defaults documented at the top of this file.
type QueryOpts struct {
	MaxRows  int
	MaxBytes int
	Timeout  time.Duration
	Params   []any
}

// Query runs sql and returns at most MaxRows rows (or DefaultMaxRows if zero)
// with at most MaxBytes (or MaxResponseBytes) of serialized content. Both
// caps clamp; the result.Truncated flag tells the caller they hit one.
// ValidateSelect runs first — anything other than the allowlist (SELECT,
// WITH, SHOW, DESCRIBE, EXPLAIN, VALUES, TABLE) is rejected. This is the
// read-only guarantee for the lake.query MCP tool and the dashboard
// endpoints.
func (s *Service) Query(ctx context.Context, sqlStr string, opts QueryOpts) (*Result, error) {
	if err := ValidateSelect(sqlStr); err != nil {
		return nil, fmt.Errorf("lake.Query: %w", err)
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	if maxRows > AbsoluteMaxRows {
		maxRows = AbsoluteMaxRows
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxResponseBytes
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = s.queryTTL
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	rows, err := s.conn.Query(qctx, sqlStr, opts.Params...)
	if err != nil {
		return nil, fmt.Errorf("lake.Query: %w", err)
	}
	defer rows.Close()

	colTypes := rows.ColumnTypes()
	cols := make([]Column, len(colTypes))
	scanTypes := make([]reflect.Type, len(colTypes))
	for i, ct := range colTypes {
		cols[i] = Column{Name: ct.Name(), Type: ct.DatabaseTypeName()}
		scanTypes[i] = ct.ScanType()
	}

	res := &Result{Columns: cols, Rows: make([][]any, 0, 64)}
	approxSize := 0
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		ptrs := make([]any, len(cols))
		holders := make([]reflect.Value, len(cols))
		for i, t := range scanTypes {
			holders[i] = reflect.New(t)
			ptrs[i] = holders[i].Interface()
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("lake.Query scan: %w", err)
		}
		row := make([]any, len(cols))
		for i, h := range holders {
			row[i] = coerceForJSON(h.Elem().Interface())
		}
		if rb, mErr := json.Marshal(row); mErr == nil {
			approxSize += len(rb) + 1
			if approxSize > maxBytes {
				res.Truncated = true
				break
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lake.Query iter: %w", err)
	}
	res.RowCount = len(res.Rows)
	res.SizeBytes = approxSize
	res.ElapsedMs = time.Since(started).Milliseconds()
	return res, nil
}

// coerceForJSON normalizes a value the CH driver scanned into a JSON-friendly
// shape. CH returns Nullable(T) as a *T pointer, []byte for raw bytes, etc.;
// we dereference one level and stringify []byte so downstream encoders don't
// base64-blob it.
func coerceForJSON(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		v = rv.Elem().Interface()
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// ExecResult is what Exec returns for a non-query statement.
type ExecResult struct {
	RowsAffected int64 `json:"rows_affected"`
	ElapsedMs    int64 `json:"elapsed_ms"`
}

// Exec runs sql against ClickHouse. Used by ingest, DDL, and the
// approval-gated tools. sql is treated as opaque; callers that need
// statement-type enforcement should run the matching safety.Validate* first.
//
// Note: CH's native protocol Exec does not return a row count. RowsAffected is
// always 0 here; UpdateRows / DeleteRows compute it explicitly.
func (s *Service) Exec(ctx context.Context, sqlStr string, params ...any) (*ExecResult, error) {
	qctx, cancel := context.WithTimeout(ctx, s.queryTTL)
	defer cancel()
	started := time.Now()
	if err := s.conn.Exec(qctx, sqlStr, params...); err != nil {
		return nil, fmt.Errorf("lake.Exec: %w", err)
	}
	return &ExecResult{ElapsedMs: time.Since(started).Milliseconds()}, nil
}

// InsertRows builds and executes an INSERT for a slice of map-shaped rows.
// All rows must share the same key set; mismatch is rejected up-front so we
// never write a half-typed row. Used by lake.insert when the agent passes
// structured rows instead of raw SQL.
//
// Uses the CH native PrepareBatch streaming API rather than building a
// VALUES (…),(…),… string — keeps wire compression effective and avoids
// reallocating large query bodies.
func (s *Service) InsertRows(ctx context.Context, table string, rows []map[string]any) (*ExecResult, error) {
	if err := validateQualifiedName(table); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &ExecResult{}, nil
	}
	cols := sortedKeys(rows[0])
	if len(cols) == 0 {
		return nil, errors.New("lake.InsertRows: first row has no columns")
	}
	for i, r := range rows {
		if len(r) != len(cols) {
			return nil, fmt.Errorf("lake.InsertRows: row %d has %d cols, want %d", i, len(r), len(cols))
		}
		for _, c := range cols {
			if _, ok := r[c]; !ok {
				return nil, fmt.Errorf("lake.InsertRows: row %d missing column %q", i, c)
			}
		}
	}

	colsQuoted := make([]string, len(cols))
	for i, c := range cols {
		colsQuoted[i] = quoteIdent(c)
	}
	insertSQL := fmt.Sprintf(`INSERT INTO %s (%s)`,
		quoteQualifiedName(table),
		strings.Join(colsQuoted, ","),
	)

	qctx, cancel := context.WithTimeout(ctx, s.queryTTL)
	defer cancel()
	started := time.Now()

	batch, err := s.conn.PrepareBatch(qctx, insertSQL)
	if err != nil {
		return nil, fmt.Errorf("lake.InsertRows: prepare: %w", err)
	}
	defer batch.Close()
	for i, r := range rows {
		vals := make([]any, len(cols))
		for j, c := range cols {
			vals[j] = r[c]
		}
		if err := batch.Append(vals...); err != nil {
			return nil, fmt.Errorf("lake.InsertRows: append row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return nil, fmt.Errorf("lake.InsertRows: send: %w", err)
	}
	return &ExecResult{
		RowsAffected: int64(len(rows)),
		ElapsedMs:    time.Since(started).Milliseconds(),
	}, nil
}

// UpdateRows runs a CH mutation as a synchronous ALTER TABLE ... UPDATE so
// the caller's reported rows_affected is meaningful. WHERE is required —
// protects against an unbounded UPDATE that would silently rewrite the
// entire table.
//
// CH's ALTER ... UPDATE is asynchronous by default and returns no row count.
// We set mutations_sync=2 so the call blocks until the mutation finishes and
// compute rows_affected with a pre-mutation count(*) WHERE pred.
func (s *Service) UpdateRows(ctx context.Context, table string, set map[string]any, where string, params []any) (*ExecResult, error) {
	if err := validateQualifiedName(table); err != nil {
		return nil, err
	}
	if strings.TrimSpace(where) == "" {
		return nil, errors.New("lake.UpdateRows: WHERE clause is required")
	}
	if len(set) == 0 {
		return nil, errors.New("lake.UpdateRows: SET map must not be empty")
	}
	qctx, cancel := context.WithTimeout(ctx, s.queryTTL)
	defer cancel()
	started := time.Now()

	preCount, err := s.countMatching(qctx, table, where, params)
	if err != nil {
		return nil, fmt.Errorf("lake.UpdateRows: pre-count: %w", err)
	}

	cols := sortedKeys(set)
	assignments := make([]string, len(cols))
	args := make([]any, 0, len(set)+len(params))
	for i, c := range cols {
		assignments[i] = quoteIdent(c) + " = ?"
		args = append(args, set[c])
	}
	args = append(args, params...)
	sqlStr := fmt.Sprintf(`ALTER TABLE %s UPDATE %s WHERE %s`,
		quoteQualifiedName(table),
		strings.Join(assignments, ","),
		where,
	)
	mctx := clickhouse.Context(qctx, clickhouse.WithSettings(clickhouse.Settings{
		"mutations_sync": uint64(2),
	}))
	if err := s.conn.Exec(mctx, sqlStr, args...); err != nil {
		return nil, fmt.Errorf("lake.UpdateRows: %w", err)
	}
	return &ExecResult{
		RowsAffected: preCount,
		ElapsedMs:    time.Since(started).Milliseconds(),
	}, nil
}

// DeleteRows runs a CH mutation as a synchronous ALTER TABLE ... DELETE.
// WHERE is required. Same mutations_sync=2 + pre-count trick as UpdateRows.
func (s *Service) DeleteRows(ctx context.Context, table string, where string, params []any) (*ExecResult, error) {
	if err := validateQualifiedName(table); err != nil {
		return nil, err
	}
	if strings.TrimSpace(where) == "" {
		return nil, errors.New("lake.DeleteRows: WHERE clause is required")
	}
	qctx, cancel := context.WithTimeout(ctx, s.queryTTL)
	defer cancel()
	started := time.Now()

	preCount, err := s.countMatching(qctx, table, where, params)
	if err != nil {
		return nil, fmt.Errorf("lake.DeleteRows: pre-count: %w", err)
	}

	sqlStr := fmt.Sprintf(`ALTER TABLE %s DELETE WHERE %s`,
		quoteQualifiedName(table),
		where,
	)
	mctx := clickhouse.Context(qctx, clickhouse.WithSettings(clickhouse.Settings{
		"mutations_sync": uint64(2),
	}))
	if err := s.conn.Exec(mctx, sqlStr, params...); err != nil {
		return nil, fmt.Errorf("lake.DeleteRows: %w", err)
	}
	return &ExecResult{
		RowsAffected: preCount,
		ElapsedMs:    time.Since(started).Milliseconds(),
	}, nil
}

// countMatching returns the number of rows in table matching the given WHERE
// clause with bound params. Used by UpdateRows / DeleteRows to surface a
// rows_affected count that CH's mutation API doesn't return.
func (s *Service) countMatching(ctx context.Context, table, where string, params []any) (int64, error) {
	q := fmt.Sprintf(`SELECT count() FROM %s WHERE %s`,
		quoteQualifiedName(table), where)
	var n uint64
	if err := s.conn.QueryRow(ctx, q, params...).Scan(&n); err != nil {
		return 0, err
	}
	return int64(n), nil
}

// DropObject runs a parameterised DROP. kind must be one of: table, view,
// schema. CH has no top-level INDEX object (skip indexes live inside table
// DDL); kind="index" returns a typed error.
func (s *Service) DropObject(ctx context.Context, name, kind string, cascade bool) (*ExecResult, error) {
	if err := validateQualifiedName(name); err != nil {
		return nil, err
	}
	upper := strings.ToUpper(kind)
	chKind := upper
	switch upper {
	case "TABLE", "VIEW":
		// pass-through
	case "SCHEMA":
		chKind = "DATABASE"
	case "INDEX":
		return nil, errors.New("lake.DropObject: ClickHouse has no top-level INDEX; drop the parent table or use ALTER TABLE ... DROP INDEX")
	default:
		return nil, fmt.Errorf("lake.DropObject: unsupported kind %q", kind)
	}
	// SCHEMA in CH (now DATABASE) takes only the database name, not a
	// qualified name. Validate that callers passed a bare identifier.
	target := quoteQualifiedName(name)
	if chKind == "DATABASE" && strings.Contains(name, ".") {
		return nil, fmt.Errorf("lake.DropObject: schema name %q must be unqualified", name)
	}
	tail := ""
	if cascade && (chKind == "TABLE" || chKind == "DATABASE") {
		// CH does not accept CASCADE; dropping a database in CH already
		// cascades to its tables, and dropping a table cascades to any
		// materialized views that target it. Keep the flag tolerated but
		// silent.
		_ = tail
	}
	stmt := fmt.Sprintf(`DROP %s IF EXISTS %s%s`, chKind, target, tail)
	return s.Exec(ctx, stmt)
}
