package dbcopy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// CopyOptions change how Copy treats the target.
type CopyOptions struct {
	// TruncateTarget empties every copied table and the blob store tables
	// of the target (TRUNCATE ... CASCADE) before the copy, inside the
	// copy's transaction. It is for repeating a rehearsal on a throwaway
	// database only: without it, Copy refuses a target that holds rows.
	TruncateTarget bool
}

// A table is loaded in COPY batches of at most batchRows rows or about
// batchBytes bytes, whichever comes first.
const (
	batchRows  = 5000
	batchBytes = 32 << 20
)

// Copy loads the SQLite database src (opened read-only; Copy only reads
// it) and the blob files in srcBlobDir into the Postgres database dst.
//
// It refuses, before writing anything, when dst is not Postgres, when the
// two databases record different migrations, when a source table or column
// is missing in the target (other than the dormant lake items), when the
// target holds rows (unless opts.TruncateTarget), or when a blob file is
// missing or does not hash to its name.
//
// Every table is then copied in one transaction on one connection: COPY in
// batches, parents before children, the identity sequences moved past the
// copied ids, then commit. A value that does not fit its target column
// stops the copy with a *CellError naming the table, column and primary
// key, never the value; nothing is committed. Last, every blob goes into
// the Postgres blob store (a blob already there is left alone).
//
// The returned report is filled as far as Copy got, also with an error.
func Copy(ctx context.Context, src *sql.DB, srcBlobDir string, dst *store.DB, opts CopyOptions) (*Report, error) {
	rep := newReport("copy")
	if src == nil || dst == nil {
		return rep, errors.New("db copy: no source or no target database")
	}
	if !dst.Dialect.IsPostgres() {
		return rep, errors.New("db copy: the target must be a Postgres database")
	}
	// One read transaction: every source read sees the same snapshot.
	stx, err := src.BeginTx(ctx, nil)
	if err != nil {
		return rep, fmt.Errorf("db copy: read the source: %w", err)
	}
	defer func() { _ = stx.Rollback() }()

	p, err := buildPlan(ctx, stx, dst)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	p.fill(rep)
	if err := p.refusal(); err != nil {
		return rep, err
	}

	full, err := nonEmptyTables(ctx, dst, append(p.tableNames(), p.BlobTables...))
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	rep.NonEmptyTargetTables = full
	if len(full) > 0 && !opts.TruncateTarget {
		rep.problemf("the target is not empty: %d tables hold rows (%s)", len(full), listNames(full))
		return rep, fmt.Errorf("db copy: refused, nothing was written: the target database is not empty (%d tables hold rows: %s). "+
			"Copy into a new, empty database; -truncate-target empties it first, for a rehearsal only", len(full), listNames(full))
	}

	blobs, err := scanSourceBlobs(ctx, stx, srcBlobDir, &rep.Blobs)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	if missing, bad := len(rep.Blobs.MissingFiles), len(rep.Blobs.MismatchedFiles); missing+bad > 0 {
		rep.problemf("%d inbox_blobs rows have no file, and %d blob files do not hash to their name", missing, bad)
		return rep, fmt.Errorf("db copy: refused, nothing was written: %d inbox_blobs rows have no file in %s, and %d files do not hash to their name (the report lists them)",
			missing, srcBlobDir, bad)
	}

	var tables []TableReport
	var reset []string
	err = dst.WithPgxConn(ctx, func(conn *pgx.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return loadError("begin the copy transaction", err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // after Commit: a no-op
		if opts.TruncateTarget {
			names := append(p.tableNames(), p.BlobTables...)
			quoted := make([]string, len(names))
			for i, n := range names {
				quoted[i] = quoteIdent(n)
			}
			if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+strings.Join(quoted, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
				return loadError("empty the target", err)
			}
		}
		for _, t := range p.Tables {
			tr, err := copyTable(ctx, stx, tx, t)
			tables = append(tables, tr)
			if err != nil {
				return err
			}
		}
		for _, t := range p.Tables {
			for _, col := range t.Identity {
				// Names are plain identifiers (plainIdent), so pasting them
				// is safe; the raw pgx connection takes no ? placeholders.
				q := `SELECT setval(pg_get_serial_sequence('` + t.Name + `', '` + col + `'), ` +
					`COALESCE((SELECT MAX(` + quoteIdent(col) + `) FROM ` + quoteIdent(t.Name) + `), 0) + 1, false)`
				if _, err := tx.Exec(ctx, q); err != nil {
					return loadError("reset the identity sequence of "+t.Name+"."+col, err)
				}
				reset = append(reset, t.Name+"."+col)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return loadError("commit the copy", err)
		}
		return nil
	})
	rep.Tables = append(rep.Tables, tables...)
	if err != nil {
		rep.problemf("the table copy failed and was rolled back: the target holds nothing from this run")
		return rep, err
	}
	rep.IdentityReset = reset
	rep.TruncatedTarget = opts.TruncateTarget
	for _, tr := range tables {
		if !tr.Match {
			rep.problemf("table %s: %d source rows, but Postgres took %d", tr.Name, tr.SourceRows, tr.CopiedRows)
		}
	}

	if err := copyBlobs(ctx, blobs, blobstore.NewPostgres(dst.DB), &rep.Blobs); err != nil {
		rep.problemf("the blob copy stopped after %d of %d blobs; the tables are committed", rep.Blobs.Copied+rep.Blobs.AlreadyPresent, len(blobs))
		return rep, err
	}
	return rep, nil
}

// copyTable streams t's rows from the source into COPY batches on tx.
func copyTable(ctx context.Context, src store.Querier, tx pgx.Tx, t *table) (TableReport, error) {
	tr := TableReport{Name: t.Name}
	rows, err := src.QueryContext(ctx, t.selectSQL())
	if err != nil {
		return tr, fmt.Errorf("db copy: read the source's %s: %w", t.Name, err)
	}
	defer rows.Close()
	names := t.colNames()
	vals, ptrs := scanTargets(len(t.Cols))
	batch := make([][]any, 0, batchRows)
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{t.Name}, names, pgx.CopyFromRows(batch))
		if err != nil {
			return loadError("load "+t.Name, err)
		}
		if n != int64(len(batch)) {
			return fmt.Errorf("db copy: load %s: Postgres took %d of %d rows; nothing was written", t.Name, n, len(batch))
		}
		tr.CopiedRows += n
		batch = batch[:0]
		size = 0
		return nil
	}
	var rowNum int64
	for rows.Next() {
		rowNum++
		if err := rows.Scan(ptrs...); err != nil {
			return tr, fmt.Errorf("db copy: read the source's %s: %w", t.Name, err)
		}
		if t.skipped(vals) {
			tr.SkippedRows++
			continue
		}
		tr.SourceRows++
		out := make([]any, len(vals))
		for i, c := range t.Cols {
			v, reason := coerce(vals[i], c)
			if reason != "" {
				return tr, &CellError{Table: t.Name, Column: c.Name, Row: t.rowRef(vals, i, rowNum), Reason: reason}
			}
			out[i] = v
			size += valueSize(v)
		}
		batch = append(batch, out)
		if len(batch) >= batchRows || size >= batchBytes {
			if err := flush(); err != nil {
				return tr, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return tr, fmt.Errorf("db copy: read the source's %s: %w", t.Name, err)
	}
	if err := flush(); err != nil {
		return tr, err
	}
	tr.Match = tr.CopiedRows == tr.SourceRows
	return tr, nil
}

func valueSize(v any) int {
	switch x := v.(type) {
	case string:
		return len(x) + 8
	case []byte:
		return len(x) + 8
	}
	return 8
}

// nonEmptyTables names the target tables in names that hold a row.
func nonEmptyTables(ctx context.Context, dst store.Querier, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		var has bool
		if err := dst.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+quoteIdent(n)+`)`).Scan(&has); err != nil {
			return nil, fmt.Errorf("read the target's %s: %w", n, err)
		}
		if has {
			out = append(out, n)
		}
	}
	return out, nil
}

// loadError describes a failure of the copy transaction without quoting a
// value: a Postgres data error (SQLSTATE class 22) can quote the value it
// rejected, and pgx's own encoding errors print the Go value, so those
// messages are withheld. Constraint and column names are kept.
func loadError(what string, err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		msg := pe.Message
		if strings.HasPrefix(pe.Code, "22") {
			msg = "Postgres rejected a value (its message is withheld: it can quote the value)"
		}
		var extra []string
		if pe.ConstraintName != "" {
			extra = append(extra, "constraint "+pe.ConstraintName)
		}
		if pe.ColumnName != "" {
			extra = append(extra, "column "+pe.ColumnName)
		}
		detail := ""
		if len(extra) > 0 {
			detail = ", " + strings.Join(extra, ", ")
		}
		return fmt.Errorf("db copy: %s: %s (SQLSTATE %s%s). Nothing was written.", what, msg, pe.Code, detail)
	}
	var ne net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) {
		return fmt.Errorf("db copy: %s: %w. Nothing was written.", what, err)
	}
	return fmt.Errorf("db copy: %s failed (%T; the message is withheld: it can quote a value). Nothing was written.", what, err)
}
