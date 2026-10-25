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
	// SourceGuard, when set, runs after the last source read, just before
	// the target commit. An error (the source changed or came into use
	// during the run) stops the copy, and nothing is committed.
	SourceGuard func() error
	// JobResultDir is the folder of large async-job results
	// (<data>/job-results); "" leaves job results out.
	JobResultDir string
}

// A table is loaded in COPY batches of at most batchRows rows or about
// batchBytes bytes, whichever comes first.
const (
	batchRows  = 5000
	batchBytes = 32 << 20
)

// Copy loads the SQLite database src (opened read-only; Copy only reads
// it) and the blob files in srcBlobDir into the Postgres database dst.
// Open dst without migrations (store.PostgresOptions.NoMigrate): Copy
// decides whether the target may be migrated.
//
// Every refusal runs first (the target's in a read-only transaction), so a
// refused copy writes nothing at all. Copy refuses when dst is not
// Postgres; when the target records no toolyard migration but holds tables
// (another application's database, say); when the target records
// migrations other than the source's, or (for an empty target) the source
// is not at this toolyard version's migrations; when a source table or
// column is missing in the target (other than the dormant lake items);
// when the target holds rows (unless opts.TruncateTarget); or when a blob
// file is missing or does not hash to its name. Only an empty target, one
// that records no migration, is migrated, and only after every refusal
// passed. A target that records migrations is never migrated.
//
// Then everything is copied in one transaction on one connection: COPY in
// batches, parents before children, every blob into the blob store tables
// (hashed again on the way), the identity sequences moved past the copied
// ids, opts.SourceGuard, then commit. A value that does not fit its target
// column stops the copy with a *CellError naming the table, column and
// primary key, never the value; nothing is committed, and a rerun starts
// again from the beginning.
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

	// The source-only checks first: hashing every blob file can take a
	// while, and the target's check transaction below stays short.
	blobs, err := scanSourceBlobs(ctx, stx, srcBlobDir, &rep.Blobs)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	if missing, bad := len(rep.Blobs.MissingFiles), len(rep.Blobs.MismatchedFiles); missing+bad > 0 {
		rep.problemf("%d inbox_blobs rows have no file, and %d blob files do not hash to their name", missing, bad)
		return rep, fmt.Errorf("db copy: refused, nothing was written: %d inbox_blobs rows have no file in %s, and %d files do not hash to their name (the report lists them)",
			missing, srcBlobDir, bad)
	}
	jobBlobs, err := scanJobResults(ctx, stx, opts.JobResultDir, &rep.JobResults)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	seqs, err := readSQLiteSequences(ctx, stx)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}

	// Every check of the target runs in a read-only transaction: Postgres
	// itself refuses a write before the checks pass.
	rtx, err := dst.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return rep, fmt.Errorf("db copy: read the target: %w", err)
	}
	defer func() { _ = rtx.Rollback() }() // after the first Rollback: a no-op

	ts, err := readTargetState(ctx, rtx)
	if err != nil {
		return rep, fmt.Errorf("db copy: %w", err)
	}
	var p *plan
	if ts.fresh() {
		if err := refuseFreshTarget(ctx, stx, ts, rep); err != nil {
			return rep, err
		}
	} else {
		if p, err = buildPlan(ctx, stx, rtx); err != nil {
			return rep, fmt.Errorf("db copy: %w", err)
		}
		p.fill(rep)
		if err := p.refusal(); err != nil {
			return rep, err
		}
		if err := refuseNonEmpty(ctx, rtx, p, rep, opts); err != nil {
			return rep, err
		}
	}
	if err := rtx.Rollback(); err != nil {
		return rep, fmt.Errorf("db copy: end the target check: %w", err)
	}

	if ts.fresh() {
		// Every refusal passed: give the empty target this version's
		// schema, then plan against it.
		if err := dst.MigratePostgres(ctx); err != nil {
			return rep, fmt.Errorf("db copy: create the toolyard tables in the empty target: %w", err)
		}
		// From here on the target holds the empty schema, so a refusal must
		// not say that nothing was written. A source table or column that
		// this version's schema lacks is the only way to get here.
		if p, err = buildPlan(ctx, stx, dst); err != nil {
			rep.problemf("the empty toolyard schema was created in the target (no rows); fix the source and run db copy again")
			return rep, fmt.Errorf("db copy: the empty toolyard schema was created in the target, but no rows were copied: %w", err)
		}
		p.fill(rep)
		if err := p.refusal(); err != nil {
			rep.problemf("the empty toolyard schema was created in the target (no rows); fix the source and run db copy again")
			return rep, fmt.Errorf("db copy: the empty toolyard schema was created in the target, but no rows were copied: %s",
				strings.TrimPrefix(err.Error(), "db copy: refused, nothing was written: "))
		}
	}
	if len(blobs)+len(jobBlobs) > 0 && len(p.BlobTables) != len(blobTables) {
		rep.problemf("the target has no blob store tables (%s)", strings.Join(blobTables, ", "))
		return rep, fmt.Errorf("db copy: refused, nothing was written: the target has no blob store tables (%s)", strings.Join(blobTables, ", "))
	}

	var tables []TableReport
	var reset []string
	var copied, present, jobCopied, jobPresent int
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
		if copied, present, err = copyBlobs(ctx, blobs, pgxExecer(tx)); err != nil {
			return err
		}
		if jobCopied, jobPresent, err = copyBlobs(ctx, jobBlobs, pgxExecer(tx)); err != nil {
			return err
		}
		for _, t := range p.Tables {
			for _, col := range t.Identity {
				// The next id is past both the largest copied id and the
				// largest id SQLite ever handed out (sqlite_sequence), so
				// the ids of deleted rows are never used again, as on
				// SQLite. Names are plain identifiers (plainIdent), so
				// pasting them is safe; the raw pgx connection takes $1.
				var seq int64
				if len(t.Identity) == 1 {
					seq = seqs[t.Name]
				}
				q := `SELECT setval(pg_get_serial_sequence('` + t.Name + `', '` + col + `'), ` +
					`GREATEST(COALESCE((SELECT MAX(` + quoteIdent(col) + `) FROM ` + quoteIdent(t.Name) + `), 0), $1::bigint) + 1, false)`
				if _, err := tx.Exec(ctx, q, seq); err != nil {
					return loadError("reset the identity sequence of "+t.Name+"."+col, err)
				}
				reset = append(reset, t.Name+"."+col)
			}
		}
		if opts.SourceGuard != nil {
			if err := opts.SourceGuard(); err != nil {
				return fmt.Errorf("db copy: %w. Nothing was written.", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return loadError("commit the copy", err)
		}
		return nil
	})
	rep.Tables = append(rep.Tables, tables...)
	if err != nil {
		rep.problemf("the copy failed and was rolled back: the target holds no row and no blob from this run")
		return rep, err
	}
	rep.IdentityReset = reset
	rep.TruncatedTarget = opts.TruncateTarget
	rep.Blobs.Copied, rep.Blobs.AlreadyPresent = copied, present
	rep.JobResults.Copied, rep.JobResults.AlreadyPresent = jobCopied, jobPresent
	for _, tr := range tables {
		if !tr.Match {
			rep.problemf("table %s: %d source rows, but Postgres took %d", tr.Name, tr.SourceRows, tr.CopiedRows)
		}
	}
	return rep, nil
}

// targetState is what the target holds before a copy.
type targetState struct {
	// migrations are the names schema_migrations records (none when the
	// table is missing).
	migrations []string
	// tables are the base tables of the current schema, schema_migrations
	// included.
	tables []string
}

// fresh reports a target that records no migration: an empty database, or
// one where toolyard has never run.
func (s targetState) fresh() bool { return len(s.migrations) == 0 }

// readTargetState reads the target's tables and recorded migrations. It
// only reads.
func readTargetState(ctx context.Context, q store.Querier) (targetState, error) {
	var s targetState
	tables, err := readNames(ctx, q, `SELECT table_name::text FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return s, fmt.Errorf("read the target's tables: %w", err)
	}
	s.tables = tables
	if !setOf(tables)["schema_migrations"] {
		return s, nil
	}
	if s.migrations, err = readNames(ctx, q, `SELECT name FROM schema_migrations`); err != nil {
		return s, fmt.Errorf("read the target's migrations: %w", err)
	}
	return s, nil
}

// refuseFreshTarget runs the refusals for a target that records no
// migration, before Copy migrates it: the target must hold no table (but
// an empty schema_migrations), and the source must be at exactly this
// toolyard version's migrations, so the migrated target matches it.
func refuseFreshTarget(ctx context.Context, src store.Querier, ts targetState, rep *Report) error {
	var other []string
	for _, n := range ts.tables {
		if n != "schema_migrations" {
			other = append(other, n)
		}
	}
	if len(other) > 0 {
		rep.TargetOnlyTables = other
		rep.problemf("the target records no toolyard migration but holds %d tables (%s)", len(other), listNames(other))
		return fmt.Errorf("db copy: refused, nothing was written: the target records no toolyard migration but holds %d tables (%s), "+
			"so it is not an empty database (is the URL right?). Copy into a new, empty database", len(other), listNames(other))
	}
	srcMig, err := readNames(ctx, src, `SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("db copy: read the source's migrations: %w", err)
	}
	want, err := store.PostgresMigrationNames()
	if err != nil {
		return fmt.Errorf("db copy: %w", err)
	}
	rep.Migrations = len(srcMig)
	onlySrc, onlyThis := diffNames(srcMig, want)
	rep.MigrationsMatch = len(onlySrc)+len(onlyThis) == 0
	rep.SourceOnlyMigrations = onlySrc
	if rep.MigrationsMatch {
		return nil
	}
	rep.problemf("the source's migrations differ from this toolyard version's: %d only in the source (%s), %d only in this version (%s)",
		len(onlySrc), listNames(onlySrc), len(onlyThis), listNames(onlyThis))
	return fmt.Errorf("db copy: refused, nothing was written: the source and this toolyard version are at different migrations (%d only in the source: %s; %d only in this version: %s). "+
		"Run this toolyard version once on the SQLite data directory (start and stop serve) so both are at the same level",
		len(onlySrc), listNames(onlySrc), len(onlyThis), listNames(onlyThis))
}

// refuseNonEmpty refuses a target whose copied tables or blob store tables
// hold a row, unless opts.TruncateTarget.
func refuseNonEmpty(ctx context.Context, q store.Querier, p *plan, rep *Report, opts CopyOptions) error {
	full, err := nonEmptyTables(ctx, q, append(p.tableNames(), p.BlobTables...))
	if err != nil {
		return fmt.Errorf("db copy: %w", err)
	}
	rep.NonEmptyTargetTables = full
	if len(full) == 0 || opts.TruncateTarget {
		return nil
	}
	rep.problemf("the target is not empty: %d tables hold rows (%s)", len(full), listNames(full))
	return fmt.Errorf("db copy: refused, nothing was written: the target database is not empty (%d tables hold rows: %s). "+
		"Copy into a new, empty database; -truncate-target empties it first, for a rehearsal only", len(full), listNames(full))
}

// readSQLiteSequences reads sqlite_sequence: for each AUTOINCREMENT table,
// the largest id SQLite has handed out, deleted rows included. It is empty
// when no table uses AUTOINCREMENT.
func readSQLiteSequences(ctx context.Context, src store.Querier) (map[string]int64, error) {
	out := map[string]int64{}
	ok, err := store.TableExists(ctx, src, store.SQLite, "sqlite_sequence")
	if err != nil || !ok {
		if err != nil {
			err = fmt.Errorf("read the source's sqlite_sequence: %w", err)
		}
		return out, err
	}
	rows, err := src.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, fmt.Errorf("read the source's sqlite_sequence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var seq sql.NullInt64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, fmt.Errorf("read the source's sqlite_sequence: %w", err)
		}
		if seq.Valid && seq.Int64 > out[name] {
			out[name] = seq.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the source's sqlite_sequence: %w", err)
	}
	return out, nil
}

// pgxExecer runs blob store statements (? placeholders) on the copy's raw
// pgx transaction, which does not rewrite them itself.
func pgxExecer(tx pgx.Tx) blobstore.Execer {
	return func(ctx context.Context, query string, args ...any) (int64, error) {
		tag, err := tx.Exec(ctx, store.Rebind(query), args...)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}
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
