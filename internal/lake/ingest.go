package lake

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IngestSource enumerates the source formats lake.ingest understands.
type IngestSource string

const (
	SourceCSV     IngestSource = "csv"
	SourceJSON    IngestSource = "json"
	SourceJSONL   IngestSource = "jsonl"
	SourceParquet IngestSource = "parquet"
	SourceSQLite  IngestSource = "sqlite"
	SourceHTTP    IngestSource = "http"
)

// IngestMode says how to combine new rows with the target table.
type IngestMode string

const (
	ModeAppend  IngestMode = "append"  // INSERT INTO target ...
	ModeReplace IngestMode = "replace" // DROP + CREATE TABLE ... AS source
)

// IngestRequest is what lake.ingest takes. Options is format-specific (CSV
// delimiter, header, etc.) and is currently consulted only for csv (key
// "delimiter") and json/jsonl (key "no_header" is accepted but unused). Keys
// must be ASCII identifiers; values are validated before use.
type IngestRequest struct {
	Type        IngestSource
	URI         string // file path or URL
	TargetTable string // schema.table; auto-creates schema if needed
	Mode        IngestMode
	Options     map[string]any
	// SQLiteTable is preserved for surface compatibility; sqlite ingest is
	// not implemented on CH in Phase 2.
	SQLiteTable string
}

// IngestReport is the structured result of one Ingest call.
type IngestReport struct {
	TargetTable string `json:"target_table"`
	RowsLoaded  int64  `json:"rows_loaded"`
	ElapsedMs   int64  `json:"elapsed_ms"`
}

// ErrSourceNotImplemented is returned by Ingest when the caller asks for a
// source type that the ClickHouse-backed lake doesn't support yet (parquet,
// sqlite). Phase 5 picks these up.
var ErrSourceNotImplemented = errors.New("lake: ingest source not implemented on ClickHouse yet")

// Ingest loads data from a single source into a target table. Sandboxing of
// source URIs is the caller's responsibility — the gateway tool layer
// enforces an allowlist before reaching here.
func (s *Service) Ingest(ctx context.Context, req IngestRequest) (*IngestReport, error) {
	if err := validateQualifiedName(req.TargetTable); err != nil {
		return nil, fmt.Errorf("Ingest: target_table: %w", err)
	}
	if req.Mode == "" {
		req.Mode = ModeAppend
	}
	if req.Mode != ModeAppend && req.Mode != ModeReplace {
		return nil, fmt.Errorf("Ingest: unsupported mode %q", req.Mode)
	}
	schema, _ := splitQualified(req.TargetTable, s.cfg.Database)
	if schema != "" {
		if err := s.conn.Exec(ctx,
			fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, quoteIdent(schema))); err != nil {
			return nil, fmt.Errorf("Ingest: ensure database: %w", err)
		}
	}
	started := time.Now()

	switch req.Type {
	case SourceCSV:
		n, err := s.ingestCSV(ctx, req)
		if err != nil {
			return nil, err
		}
		return &IngestReport{TargetTable: req.TargetTable, RowsLoaded: n, ElapsedMs: time.Since(started).Milliseconds()}, nil
	case SourceJSONL:
		n, err := s.ingestJSONL(ctx, req)
		if err != nil {
			return nil, err
		}
		return &IngestReport{TargetTable: req.TargetTable, RowsLoaded: n, ElapsedMs: time.Since(started).Milliseconds()}, nil
	case SourceJSON:
		// Single-document JSON files: treat the top-level value as an
		// array of objects; fall through to the JSONL path with the
		// array materialised into individual rows.
		n, err := s.ingestJSONArray(ctx, req)
		if err != nil {
			return nil, err
		}
		return &IngestReport{TargetTable: req.TargetTable, RowsLoaded: n, ElapsedMs: time.Since(started).Milliseconds()}, nil
	case SourceHTTP:
		n, err := s.ingestHTTP(ctx, req)
		if err != nil {
			return nil, err
		}
		return &IngestReport{TargetTable: req.TargetTable, RowsLoaded: n, ElapsedMs: time.Since(started).Milliseconds()}, nil
	case SourceParquet, SourceSQLite:
		return nil, fmt.Errorf("%w: %s", ErrSourceNotImplemented, req.Type)
	default:
		return nil, fmt.Errorf("Ingest: unknown source type %q", req.Type)
	}
}

// ingestCSV parses the CSV at req.URI in Go and bulk-loads it via the native
// PrepareBatch API. Header row is required (column names come from row 0).
func (s *Service) ingestCSV(ctx context.Context, req IngestRequest) (int64, error) {
	f, err := os.Open(req.URI)
	if err != nil {
		return 0, fmt.Errorf("Ingest CSV: open: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	if d, ok := req.Options["delimiter"].(string); ok && len(d) == 1 {
		r.Comma = rune(d[0])
	}
	r.ReuseRecord = false
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		return 0, fmt.Errorf("Ingest CSV: read header: %w", err)
	}
	for _, h := range header {
		if !isValidIdent(h) {
			return 0, fmt.Errorf("Ingest CSV: invalid column name %q in header", h)
		}
	}

	rows := make([][]string, 0, 64)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("Ingest CSV: read row: %w", err)
		}
		rows = append(rows, rec)
	}

	return s.bulkLoadStringRows(ctx, req, header, rows)
}

// ingestJSONL parses newline-delimited JSON objects at req.URI and loads
// them. Column set is the union of keys observed in row 0; rows missing a
// column get an empty string.
func (s *Service) ingestJSONL(ctx context.Context, req IngestRequest) (int64, error) {
	f, err := os.Open(req.URI)
	if err != nil {
		return 0, fmt.Errorf("Ingest JSONL: open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var (
		header []string
		rows   [][]string
	)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return 0, fmt.Errorf("Ingest JSONL: parse: %w", err)
		}
		if header == nil {
			header = sortedKeys(obj)
			for _, h := range header {
				if !isValidIdent(h) {
					return 0, fmt.Errorf("Ingest JSONL: invalid column name %q", h)
				}
			}
		}
		rec := make([]string, len(header))
		for i, k := range header {
			if v, ok := obj[k]; ok {
				rec[i] = stringifyJSONValue(v)
			}
		}
		rows = append(rows, rec)
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("Ingest JSONL: scan: %w", err)
	}
	return s.bulkLoadStringRows(ctx, req, header, rows)
}

// ingestJSONArray reads a single JSON document containing an array of
// objects at req.URI.
func (s *Service) ingestJSONArray(ctx context.Context, req IngestRequest) (int64, error) {
	f, err := os.Open(req.URI)
	if err != nil {
		return 0, fmt.Errorf("Ingest JSON: open: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var arr []map[string]any
	if err := dec.Decode(&arr); err != nil {
		return 0, fmt.Errorf("Ingest JSON: parse: %w", err)
	}
	if len(arr) == 0 {
		return 0, nil
	}
	header := sortedKeys(arr[0])
	for _, h := range header {
		if !isValidIdent(h) {
			return 0, fmt.Errorf("Ingest JSON: invalid column name %q", h)
		}
	}
	rows := make([][]string, 0, len(arr))
	for _, obj := range arr {
		rec := make([]string, len(header))
		for i, k := range header {
			if v, ok := obj[k]; ok {
				rec[i] = stringifyJSONValue(v)
			}
		}
		rows = append(rows, rec)
	}
	return s.bulkLoadStringRows(ctx, req, header, rows)
}

// ingestHTTP delegates to CH's url() table function — let the server stream
// the file directly. We dispatch format by extension, matching what the
// DuckDB-era code did.
func (s *Service) ingestHTTP(ctx context.Context, req IngestRequest) (int64, error) {
	format := "CSVWithNames"
	switch strings.ToLower(filepath.Ext(req.URI)) {
	case ".parquet":
		format = "Parquet"
	case ".json", ".ndjson", ".jsonl":
		format = "JSONEachRow"
	case ".tsv":
		format = "TSVWithNames"
	}
	// We don't want the URL inlined to a SQL string with naive escaping,
	// but CH's url() function takes its argument as a constant — there's
	// no parameter binding for table functions. Defense-in-depth: require
	// the URL to start with http(s):// and forbid any single-quote so the
	// SQL-literal can't be terminated.
	u := strings.TrimSpace(req.URI)
	if !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		return 0, fmt.Errorf("Ingest HTTP: only http(s):// URLs supported")
	}
	if strings.ContainsAny(u, "'\\\r\n") {
		return 0, fmt.Errorf("Ingest HTTP: URL contains forbidden characters")
	}
	target := quoteQualifiedName(req.TargetTable)
	srcClause := fmt.Sprintf(`SELECT * FROM url('%s', '%s')`, u, format)

	var stmt string
	switch req.Mode {
	case ModeReplace:
		stmt = fmt.Sprintf(`CREATE OR REPLACE TABLE %s ENGINE = MergeTree() ORDER BY tuple() AS %s`, target, srcClause)
	default:
		stmt = fmt.Sprintf(`INSERT INTO %s %s`, target, srcClause)
	}
	if err := s.conn.Exec(ctx, stmt); err != nil {
		return 0, fmt.Errorf("Ingest HTTP: %w", err)
	}
	// CH's INSERT/CREATE-AS doesn't return rowcount; query it back.
	var n uint64
	if err := s.conn.QueryRow(ctx, fmt.Sprintf(`SELECT count() FROM %s`, target)).Scan(&n); err != nil {
		return 0, fmt.Errorf("Ingest HTTP: post-count: %w", err)
	}
	return int64(n), nil
}

// bulkLoadStringRows is the shared landing path for csv/json/jsonl. It
// ensures the target table exists with a column shape matching `header`:
//   - replace mode: drops and recreates with Nullable(String) columns.
//   - append mode: requires the table to exist; values are coerced to the
//     target column types via convertStringToCH.
//
// Then it streams rows via PrepareBatch.
func (s *Service) bulkLoadStringRows(ctx context.Context, req IngestRequest, header []string, rows [][]string) (int64, error) {
	if len(header) == 0 {
		return 0, errors.New("Ingest: no columns in source")
	}
	target := quoteQualifiedName(req.TargetTable)

	if req.Mode == ModeReplace {
		if err := s.conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, target)); err != nil {
			return 0, fmt.Errorf("Ingest: drop existing: %w", err)
		}
		colDefs := make([]string, len(header))
		for i, h := range header {
			colDefs[i] = fmt.Sprintf("%s Nullable(String)", quoteIdent(h))
		}
		ddl := fmt.Sprintf(`CREATE TABLE %s (%s) ENGINE = MergeTree() ORDER BY tuple()`,
			target, strings.Join(colDefs, ", "))
		if err := s.conn.Exec(ctx, ddl); err != nil {
			return 0, fmt.Errorf("Ingest: create target: %w", err)
		}
	}

	// Look up the actual column types we're inserting into. This handles
	// both replace (we just created all-String) and append (we need to
	// know how to coerce each value).
	desc, err := s.DescribeTable(ctx, req.TargetTable)
	if err != nil {
		return 0, fmt.Errorf("Ingest: describe target: %w", err)
	}
	typesByName := make(map[string]string, len(desc.Columns))
	nullableByName := make(map[string]bool, len(desc.Columns))
	for _, c := range desc.Columns {
		typesByName[c.Name] = c.Type
		nullableByName[c.Name] = c.Nullable
	}
	for _, h := range header {
		if _, ok := typesByName[h]; !ok {
			return 0, fmt.Errorf("Ingest: target column %q missing in %s", h, req.TargetTable)
		}
	}

	colsQuoted := make([]string, len(header))
	for i, h := range header {
		colsQuoted[i] = quoteIdent(h)
	}
	insertSQL := fmt.Sprintf(`INSERT INTO %s (%s)`, target, strings.Join(colsQuoted, ","))
	batch, err := s.conn.PrepareBatch(ctx, insertSQL)
	if err != nil {
		return 0, fmt.Errorf("Ingest: prepare batch: %w", err)
	}
	defer batch.Close()
	for i, rec := range rows {
		vals := make([]any, len(header))
		for j, h := range header {
			vals[j], err = convertStringToCH(rec[j], typesByName[h], nullableByName[h])
			if err != nil {
				return 0, fmt.Errorf("Ingest: row %d col %q: %w", i, h, err)
			}
		}
		if err := batch.Append(vals...); err != nil {
			return 0, fmt.Errorf("Ingest: append row %d: %w", i, err)
		}
	}
	if err := batch.Send(); err != nil {
		return 0, fmt.Errorf("Ingest: send batch: %w", err)
	}
	return int64(len(rows)), nil
}

// convertStringToCH turns a raw CSV/JSONL string field into a Go value the
// CH driver will accept for a column of chType. nullable controls whether an
// empty input means SQL NULL (return nil) vs the zero value of the type.
func convertStringToCH(raw, chType string, nullable bool) (any, error) {
	if nullable && raw == "" {
		return nil, nil
	}
	t, _ := unwrapNullable(chType)
	switch {
	case t == "String", strings.HasPrefix(t, "FixedString"), t == "JSON":
		s := raw
		if nullable {
			return &s, nil
		}
		return s, nil
	case t == "Bool":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, err
		}
		if nullable {
			return &b, nil
		}
		return b, nil
	case strings.HasPrefix(t, "Int"), strings.HasPrefix(t, "UInt"):
		// Pick the widest signed/unsigned variant the driver accepts to
		// avoid having to special-case every (U)Int8/16/32/64/128.
		if strings.HasPrefix(t, "UInt") {
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return nil, err
			}
			if nullable {
				return &n, nil
			}
			return n, nil
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, err
		}
		if nullable {
			return &n, nil
		}
		return n, nil
	case strings.HasPrefix(t, "Float"):
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		if nullable {
			return &f, nil
		}
		return f, nil
	case strings.HasPrefix(t, "DateTime"):
		// Accept RFC3339 + the common variants.
		tm, err := parseTimeLoose(raw)
		if err != nil {
			return nil, err
		}
		if nullable {
			return &tm, nil
		}
		return tm, nil
	case t == "Date", t == "Date32":
		tm, err := parseTimeLoose(raw)
		if err != nil {
			return nil, err
		}
		if nullable {
			return &tm, nil
		}
		return tm, nil
	default:
		// Unknown type: fall back to string and let the driver complain
		// if it can't coerce. Better than silently dropping data.
		s := raw
		if nullable {
			return &s, nil
		}
		return s, nil
	}
}

// parseTimeLoose tries a few common layouts.
func parseTimeLoose(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time format %q", s)
}

// stringifyJSONValue renders a JSON-decoded value back as a string suitable
// for the all-String CSV-like path. Objects and arrays come out as JSON.
func stringifyJSONValue(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
