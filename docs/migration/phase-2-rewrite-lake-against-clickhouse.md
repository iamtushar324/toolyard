# Phase 2 — Rewrite `internal/lake/` against ClickHouse

**Status:** ready to start
**Pre-reqs:** Phase 1 (data migration tool) is done and merged.
**Scope:** replace the DuckDB-backed `lake.Service` with a ClickHouse-backed
implementation while keeping the public Go API of `internal/lake/` and the
`lake.*` MCP tool / `/v1/lake/*` HTTP surface byte-compatible. Callers
(`internal/gateway/lake_tools.go`, `internal/api/lake_routes.go`,
`cmd/gateway/lake_cmd.go`, `cmd/gateway/main.go`) **must keep building**
against the same package surface — they get rewritten in Phase 3.

This is a pure-Go-package rewrite. **Do not** touch the gateway tool
layer, the HTTP routes, the dashboards, the Grafana datasource, or
`go.mod`-level removal of go-duckdb in this phase. Phase 3 ports the
callers, Phase 4 does the Grafana cutover, Phase 5 deletes go-duckdb.

---

## 1. Background you need before touching code

### 1.1 What's already in place from Phase 1

- `clickhouse/clickhouse-server:24.8` runs in docker via
  `deploy/clickhouse/docker-compose.yaml`. Bound to **`127.0.0.1:9000`
  (native)** and **`127.0.0.1:8123` (HTTP)** on the host.
- Resource caps: 2 GiB memory, 0.5 CPU, `max_concurrent_queries=4`,
  `background_pool_size=4` with `concurrency_ratio=8` (clears MergeTree
  sanity checks).
- Auth: single `default` user with a password sourced from
  `TOOLYARD_CH_PASSWORD` env var. The password lives in toolyard's
  settings DB (`settings.ClickhousePassword`); toolyard renders it into
  `/var/lib/toolyard/clickhouse-runtime.env` on every boot via
  `api.WriteClickhouseRuntimeEnv` (`internal/api/lake_routes.go`).
  Rotatable from the dashboard's Settings tab; rotation requires
  `docker compose restart clickhouse` to reload.
- `cmd/lake-migrate/main.go` is the one-shot DuckDB → CH data
  migration tool. **Don't delete it in this phase** — Phase 5 does
  that. Read it for the type-mapping logic; reuse the same mappings.
- `clickhouse-go/v2` is already in `go.mod` (added by Phase 1).
- All 22 base tables in the source `raw` schema have been verified to
  migrate cleanly to CH (idempotent re-run with row-count parity).
  `mart.*` are 14 views that need manual SQL translation to CH dialect
  — the migration tool dumps their DDLs to `mart_views_to_port.sql`.
  Translating views is **Phase 5**, not Phase 2.

### 1.2 What `internal/lake/` looks like today (the surface to preserve)

```
internal/lake/
├── lake.go        (371 lines) — Service struct, Open/Close, Query, Exec,
│                                InsertRows, UpdateRows, DeleteRows,
│                                DropObject + Result/ExecResult/Column types
├── safety.go      (130 lines) — ValidateSelect / ValidateInsert /
│                                ValidateCreate / ValidateAlter
├── catalog.go     (162 lines) — TableInfo, ColumnInfo, TableDescription,
│                                ListTables, DescribeTable
├── ingest.go      (230 lines) — IngestSource, IngestMode, IngestRequest,
│                                IngestReport, Ingest
├── util.go        (71 lines)  — isValidIdent, validateQualifiedName,
│                                quoteIdent, quoteQualifiedName, sortedKeys
└── lake_test.go   (200 lines) — table-driven tests
```

**Public types and methods that callers depend on** (see them in use in
`internal/gateway/lake_tools.go` and `internal/api/lake_routes.go`):

```go
// lake.go
func Open(path string) (*Service, error)
func (s *Service) Close() error
func (s *Service) DB() *sql.DB
func (s *Service) Query(ctx, sqlStr, opts QueryOpts) (*Result, error)
func (s *Service) Exec(ctx, sqlStr, params...) (*ExecResult, error)
func (s *Service) InsertRows(ctx, table, rows []map[string]any) (*ExecResult, error)
func (s *Service) UpdateRows(ctx, table, set, where, params) (*ExecResult, error)
func (s *Service) DeleteRows(ctx, table, where, params) (*ExecResult, error)
func (s *Service) DropObject(ctx, name, kind, cascade) (*ExecResult, error)

// catalog.go
func (s *Service) ListTables(ctx, schema) ([]TableInfo, error)
func (s *Service) DescribeTable(ctx, table) (*TableDescription, error)

// ingest.go
func (s *Service) Ingest(ctx, req IngestRequest) (*IngestReport, error)

// util.go (used by gateway/api code)
quoteIdent(s) string
quoteQualifiedName(s) string
isValidIdent(s) bool
validateQualifiedName(s) error
```

The struct types `Result`, `ExecResult`, `Column`, `QueryOpts`,
`TableInfo`, `ColumnInfo`, `TableDescription`, `IngestRequest`,
`IngestReport`, `IngestSource`, `IngestMode` and their fields **must
keep their JSON tags identical** — they're returned over `/v1/lake/*` and
the Grafana Infinity datasource and the `web/lake/` UI parse them.

### 1.3 Schemas in DuckDB → Databases in CH

| DuckDB concept                  | CH equivalent                          |
|---------------------------------|----------------------------------------|
| `CREATE SCHEMA raw`             | `CREATE DATABASE raw`                  |
| `raw.account_balances`          | `raw.account_balances` (same syntax)   |
| `information_schema.tables`     | `system.tables`                        |
| `information_schema.columns`    | `system.columns`                       |
| `SELECT * FROM duckdb_views()`  | `SELECT … FROM system.tables WHERE engine='View'` |

ClickHouse uses **databases** where DuckDB uses **schemas**. The
qualified-name syntax `db.table` is identical; the bootstrap is the
only place this matters in code (`Open()`).

The standard databases stay the same names — `raw`, `mart`, `app` —
because the migration tool already created them and every existing
caller and dashboard uses those literals. Don't rename.

---

## 2. Design decisions baked in

### 2.1 Connection: native protocol, single pool

Use `clickhouse-go/v2` via the `clickhouse.Open` factory (returns a
`driver.Conn`, not `*sql.DB`). The native protocol on TCP/9000 is
faster than HTTP/8123 and supports streaming inserts via `PrepareBatch`.

Pool sizing: `MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: 10m`.
The CH server's `max_concurrent_queries=4`, so anything above 4 just
queues server-side. Keep pool ≤ server budget.

DSN comes from new fields on a config struct passed to `Open` (see
2.5 below).

### 2.2 Read-only enforcement stays parser-based

`safety.ValidateSelect` is the read-only guarantee for `lake.Query` —
the gateway tool layer calls it before invoking `Service.Query`. CH
**does not** have an equivalent of DuckDB's read-only access mode that
we can use as a separate pool. Keep the parser-based validation; just
update the keyword allowlist to match what CH supports (see 2.6).

### 2.3 MergeTree everywhere; updates are async ALTER

CH tables created via `lake.create_table` and `lake.ingest` get
`ENGINE = MergeTree() ORDER BY <key>` where `<key>` is:

- `id` if the source has an `Int64`/`Int32` `id` column
- `tuple()` otherwise

This matches the migration tool's logic in
`cmd/lake-migrate/main.go:migrateTable`.

`UPDATE` and `DELETE` on a MergeTree table go through CH **mutations**:

```sql
ALTER TABLE foo UPDATE col=val WHERE pred         -- async by default
ALTER TABLE foo DELETE WHERE pred                  -- async by default
```

Mutations are asynchronous and progress is visible in
`system.mutations`. For the `lake.update` and `lake.delete` tools we
want **synchronous** semantics so the agent's reported `rows_affected`
is meaningful. Set the per-query setting `mutations_sync = 2` to make
the call block until all replicas finish (we're single-node, so
effectively "block until done"):

```go
ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
    "mutations_sync": uint64(2),
}))
conn.Exec(ctx, "ALTER TABLE foo UPDATE …")
```

`UpdateRows` and `DeleteRows` need to:
1. Translate the standard SQL `UPDATE table SET … WHERE …` /
   `DELETE FROM table WHERE …` into the `ALTER TABLE … UPDATE/DELETE`
   form (or accept the `ALTER TABLE …` form directly).
2. Apply the `mutations_sync=2` context setting.
3. Report `rows_affected` from a separate `SELECT count() FROM table
   WHERE pred_before` or by inspecting `system.mutations` — CH does
   **not** return rowcount from `ALTER TABLE … UPDATE`. Easiest: count
   the matching rows pre-mutation, return that as `rows_affected`.

### 2.4 Multi-row `InsertRows` uses native batch

`PrepareBatch` opens a streaming insert; iterate rows calling
`Append`, then `Send` once. This is what `cmd/lake-migrate` does —
copy that pattern. Don't try to build a single `INSERT INTO … VALUES (…),(…),…`
string; it works but throws away the native protocol's compression
and is slower for batches over a few dozen rows.

### 2.5 `Open()` signature change is OK; callers will be updated in Phase 3

The current `Open(path string)` opens a DuckDB file. The new `Open`
takes a config struct since CH needs host/port/user/password/database.
**This is a breaking change to the package API**, but `cmd/gateway/main.go`
and `cmd/gateway/lake_cmd.go` only get rewritten in Phase 3 anyway —
just leave them broken at the end of Phase 2 with a compile error
that documents what Phase 3 needs to call instead.

Suggested shape:

```go
type Config struct {
    Addr     string        // e.g. "127.0.0.1:9000"
    User     string        // e.g. "default"
    Password string        // from settings; never logged
    Database string        // baseline database name; defaults to "default"
    DialTimeout time.Duration
}

func Open(cfg Config) (*Service, error)
```

Plus a convenience `OpenFromEnv()` that reads `TOOLYARD_CH_*` env vars
for tests.

### 2.6 SQL dialect differences vs DuckDB

These are the ones you'll hit. Update `safety.go` and the catalog
queries accordingly.

| Feature              | DuckDB                            | ClickHouse                                 |
|----------------------|-----------------------------------|--------------------------------------------|
| `CREATE SCHEMA`      | `CREATE SCHEMA IF NOT EXISTS x`   | `CREATE DATABASE IF NOT EXISTS x`          |
| Catalog query        | `information_schema.tables`       | `system.tables` (also has IS, but use sys) |
| Column metadata      | `information_schema.columns`      | `system.columns`                           |
| View detection       | `table_type = 'VIEW'`             | `engine = 'View'` in `system.tables`       |
| Row count of table   | `SELECT count(*) FROM t`          | `SELECT total_rows FROM system.tables`     |
| Show DDL             | `SELECT view_definition FROM IS`  | `SHOW CREATE TABLE db.t`                   |
| `count_star()`       | `count_star()`                    | `count()`                                  |
| `TRY_CAST(x AS T)`   | `TRY_CAST(x AS T)`                | `accurateCastOrNull(x, 'T')` or `toTOrNull(x)` |
| `date_trunc`         | `date_trunc('day', x)`            | `date_trunc('day', x)` (works) or `toStartOfDay` |
| `COLUMNS('regex')`   | works                             | no equivalent — must be expanded by hand   |
| `EXPORT DATABASE`    | works                             | no — use `SELECT … INTO OUTFILE` or `BACKUP` |
| `PRAGMA …`           | many                              | very limited; CH uses `system.settings` queries |
| `DESCRIBE table`     | works                             | `DESCRIBE TABLE table` (note the TABLE)    |
| `ATTACH 'file' AS x` | sqlite_scanner                    | no in-process attach; use `sqlite()` table function |
| Identifier quoting   | `"name"` or `\`name\``            | both work; prefer backticks                |

`ValidateSelect` in `safety.go` allows `SELECT|WITH|SHOW|PRAGMA|DESCRIBE|EXPLAIN|VALUES|TABLE`.
For CH:

- Drop `PRAGMA` from the allowlist (CH has no useful PRAGMAs).
- Keep `SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN`, `VALUES`,
  `TABLE`.
- Add `SYSTEM` for read-only `SYSTEM RELOAD CONFIG`-style commands?
  **No** — those mutate server state. Don't allow.

### 2.7 Type mapping (carry over from Phase 1 verbatim)

| DuckDB type                        | ClickHouse type        | Notes                                  |
|------------------------------------|------------------------|----------------------------------------|
| `VARCHAR`, `TEXT`, `STRING`        | `Nullable(String)`     |                                        |
| `DOUBLE`, `REAL`, `FLOAT…`         | `Nullable(Float64)`    |                                        |
| `BIGINT`, `INT8`                   | `Nullable(Int64)`      |                                        |
| `INTEGER`, `INT`, `INT4`           | `Nullable(Int32)`      |                                        |
| `SMALLINT`, `INT2`                 | `Nullable(Int16)`      |                                        |
| `TINYINT`, `INT1`                  | `Nullable(Int8)`       |                                        |
| `HUGEINT`                          | `Nullable(Int128)`     |                                        |
| `UBIGINT`                          | `Nullable(UInt64)`     |                                        |
| `TIMESTAMP[…]`                     | `Nullable(DateTime64(3))` |                                     |
| `DATE`                             | `Nullable(Date)`       |                                        |
| `BOOLEAN`, `BOOL`                  | `Nullable(Bool)`       |                                        |
| `DECIMAL(p,s)` / `NUMERIC(p,s)`    | `Nullable(Decimal(p,s))` | preserve precision when parseable    |
| `STRUCT(…)`, `MAP(…,…)`, `T[]`, `JSON` | `Nullable(String)` | JSON-encoded for losslessness         |

Reuse `cmd/lake-migrate/main.go:toCHType` — copy it (or extract to a
shared helper in `internal/lake/util.go`). When the column is the
MergeTree `ORDER BY` key, narrow to non-Nullable.

### 2.8 Ingest paths — what survives, what changes

Today's `IngestSource` values:

| Source           | DuckDB approach                | CH replacement                           |
|------------------|--------------------------------|------------------------------------------|
| `csv`            | `read_csv_auto('file')`        | `INSERT INTO t FROM INFILE 'file' FORMAT CSVWithNames` (clickhouse-client) — but we're a server connection, not the CLI. For server-side ingest of host files, we have to either: (a) stage rows through Go (parse CSV, batch insert), or (b) use the `file()` table function with the file mounted into the CH container's `/var/lib/clickhouse/user_files/`. **Recommendation:** Go-side parse + batch insert — keeps the server stateless and works with any host path the gateway can read. |
| `json`, `jsonl`  | `read_json_auto`               | Same trade-off. Go-side parse + insert.  |
| `parquet`        | `read_parquet`                 | CH has `file()` table function with `Parquet` format. Requires file in container's `user_files/`. **Recommendation:** Go-side parse via `apache/arrow/go/v17/parquet`, or document that parquet ingest needs the file copied to the container's user_files dir first. For Phase 2, keep this constant but document "not implemented yet" if scope creeps. |
| `http`           | DuckDB httpfs extension        | CH supports `url('https://…', 'CSVWithNames')` natively. Use it. |
| `sqlite`         | DuckDB `ATTACH … TYPE SQLITE`  | CH has the `sqlite()` table function: `INSERT INTO target SELECT * FROM sqlite('/path/to/db', 'table')`. The file must be readable by the CH process — same caveat as `file()`. **Recommendation for Phase 2:** keep the surface, document that sqlite ingest from arbitrary host paths requires copying into a known mount point; defer the full automation to a follow-up. |

For Phase 2, prioritise getting `csv`, `jsonl`, and `http` working
correctly. `parquet` and `sqlite` can return a clear "not yet
supported on CH" error — Phase 5 (or a follow-up) tackles those.

### 2.9 Result row coercion

`Service.Query` returns `Result.Rows` as `[][]any` so callers can JSON-
encode it. CH's `driver.Rows.Scan` works the same as `sql.Rows.Scan`:
pass a slice of `any` pointers. Most types come back as Go primitives
(`int64`, `float64`, `string`, `time.Time`, `bool`). For the
JSON-encoded `Nullable(String)` columns that hold STRUCT/MAP data, the
caller gets a `*string` back — dereference and pass through to JSON
encoding.

One CH-specific quirk: `Nullable(T)` columns scan into `**T`. Plan for
this: dereference one level when collecting `holders[i]` so the JSON
output matches what DuckDB used to produce (a `null` JSON value when
SQL `NULL`).

---

## 3. Implementation order

Do these in sequence. Each step compiles cleanly and passes the
existing tests for the prior step before moving on.

### Step 1 — Spike: minimal `lake.Service` open + ping

Replace `Open()` with the new `Config`-taking signature. Implement just
enough for `Open` + `Close` + `DB()` (return `nil` for `DB()` for now;
remove the field if nothing in Phase 3 ends up needing it). Add an
integration test that opens against a docker-running CH (skip when
`TOOLYARD_CH_TEST_ADDR` env var is empty so CI doesn't need CH).

### Step 2 — Port `safety.go`

Update the keyword allowlist (drop `PRAGMA`). Add tests covering CH-
specific cases:
- `WITH RECURSIVE …` should be allowed (CH supports it with a setting).
- `EXPLAIN AST SELECT …` should be allowed.
- `SYSTEM RELOAD CONFIG` should be **rejected** (it mutates).

### Step 3 — Port `Query` and `Exec`

`Query`: keep the `QueryOpts` shape; map `MaxRows`, `MaxBytes`,
`Timeout`, `Params`. Map `Result.Columns[i].Type` to CH type names
returned by `driver.Rows.ColumnTypes()`.

`Exec`: straightforward `conn.Exec(ctx, sql, params…)`. CH doesn't
return `RowsAffected` for most statements — just leave that field at
0 except for the row-count workaround in `UpdateRows`/`DeleteRows`.

### Step 4 — Port `InsertRows` to native batch

Use `conn.PrepareBatch(ctx, "INSERT INTO ... (cols)")` then `Append`
per row, `Send` at the end. Validate that all rows have the same
column set as today.

### Step 5 — Port `UpdateRows` and `DeleteRows` via mutations

For each:
1. Pre-count rows matching the WHERE (this is the `rows_affected` we
   return — do this **before** the mutation so the count matches).
2. Run `ALTER TABLE t UPDATE col=val WHERE …` /
   `ALTER TABLE t DELETE WHERE …` with `mutations_sync=2`.
3. Return `ExecResult{RowsAffected: preCount, ElapsedMs: …}`.

### Step 6 — Port `DropObject`

Same shape as today: `DROP TABLE/VIEW/INDEX/SCHEMA name [CASCADE]`.
CH spelling for SCHEMA is **DATABASE** — translate `kind == "schema"`
to `DROP DATABASE`. CH doesn't have INDEX as a top-level object the
way DuckDB does (skip indexes are at the table level via ALTER) —
return an error for `kind=="index"` with a clear message.

### Step 7 — Port `catalog.go`

`ListTables(schema)`:
```sql
SELECT database, name,
       multiIf(engine='View', 'VIEW', 'BASE TABLE') AS kind,
       total_rows
  FROM system.tables
 WHERE database NOT IN ('system','INFORMATION_SCHEMA','information_schema')
   {{if schema}}AND database = ?{{end}}
 ORDER BY database, name
```

`DescribeTable(name)`:
```sql
SELECT name, type, default_expression, position
  FROM system.columns
 WHERE database = ? AND table = ?
 ORDER BY position
```

`Nullable` wrappers in `type` indicate the column is nullable — set
`ColumnInfo.Nullable = strings.HasPrefix(t, "Nullable(")` and strip
the wrapper for the type string the agent sees.

For row count, query `total_rows` from `system.tables` (cheap and
accurate for MergeTree). Fall back to `count()` if missing.

### Step 8 — Port `ingest.go`

Implement only `csv`, `jsonl`, `http` for now. For `csv`/`jsonl`:
parse with `encoding/csv` / `bufio.Scanner` + `encoding/json`, infer
or accept declared types, use `PrepareBatch` to insert. For `http`:
use CH's `url()` table function:

```sql
INSERT INTO target SELECT * FROM url('https://…', 'CSVWithNames')
```

`parquet` and `sqlite` return a typed error
`ErrSourceNotImplemented`; gateway code in Phase 3 surfaces that to
the agent as a clean failure.

### Step 9 — Update `lake_test.go`

The existing tests open a temp DuckDB file. Rewrite to use a CH
connection — add a `testdb.go` helper that:
- skips with `t.Skip` when `TOOLYARD_CH_TEST_ADDR` is unset, or
- opens a connection to it and uses a unique per-test database name
  (`test_<random>`) that's `DROP DATABASE`'d in `t.Cleanup`.

Cover:
- Open + Close
- Query: SELECT 1, capped row counts, byte cap, timeout
- Exec: CREATE/DROP roundtrip
- InsertRows: batch insert + read back
- UpdateRows / DeleteRows: pre-count = post-rows-affected
- DropObject: TABLE / DATABASE / unsupported kind
- ListTables / DescribeTable: round-trip a table
- Ingest: CSV from a temp file, http from a httptest.Server

### Step 10 — Don't fix Phase 3 callers yet

`cmd/gateway/main.go:160-166` (`lake.Open(lakePath)`) and
`cmd/gateway/lake_cmd.go:36` (`lake.Open(filepath.Join(...))`) and
`internal/gateway/lake_tools_test.go:32` will fail to compile after
step 1 because the `Open` signature changed. **Leave them broken.**
The build is allowed to fail at the binary level for the duration of
Phase 2; `go test ./internal/lake/...` must pass.

The Phase 3 brief will pick up these compile errors as its starting
work list.

---

## 4. Validation checklist

Phase 2 is done when **all** of these are true:

- [ ] `go build ./internal/lake/...` succeeds.
- [ ] `go test ./internal/lake/...` passes against a running CH
      (skips cleanly when `TOOLYARD_CH_TEST_ADDR` is unset).
- [ ] No reference to `marcboeker/go-duckdb` remains in
      `internal/lake/` (`grep -rn marcboeker internal/lake/` is empty).
- [ ] `internal/lake/` imports `github.com/ClickHouse/clickhouse-go/v2`.
- [ ] The exported types and methods listed in §1.2 still exist with
      the same names. Their JSON tags are unchanged.
- [ ] Adding new methods is fine; removing or renaming an existing one
      is not (Phase 3 will lean on what's there).
- [ ] A docstring at the top of `lake.go` says "ClickHouse-backed".
      The original DuckDB-history comment block can stay if it's
      moved into a `// Was DuckDB until phase 2; see ADR 0004 + the
      Phase 2 brief for the rewrite rationale.` paragraph.
- [ ] `cmd/gateway/main.go` is **left broken** — failing build is
      expected and documented in your PR description.

Optional but appreciated:

- [ ] Bench: insert 10k rows via `InsertRows`, query them back.
      Should complete in well under 1s on the dev box.
- [ ] Add a doc comment on `UpdateRows` / `DeleteRows` calling out the
      `mutations_sync=2` choice and the pre-count `rows_affected`
      compromise.

---

## 5. Out of scope (do not touch)

- `internal/gateway/lake_*.go` and `internal/gateway/lake_paths.go` —
  Phase 3.
- `internal/api/lake_routes.go` — Phase 3.
- `cmd/gateway/main.go`, `cmd/gateway/lake_cmd.go`,
  `cmd/gateway/lake_time.go` — Phase 3.
- `web/dashboard/`, `web/lake/`, `deploy/grafana/` — Phase 4.
- Removing `go-duckdb` from `go.mod` / `cmd/lake-migrate/` — Phase 5.
- Translating mart views — Phase 5.
- ADR 0004 update — do this in Phase 5 once the migration is end-to-end.

---

## 6. References & file paths the next agent will want

- The Phase 1 migration tool: `cmd/lake-migrate/main.go` — type
  mapping, batch-insert pattern, MergeTree DDL generation, and the
  reflection trick for `duckdb.Map`. Don't reinvent these.
- The current DuckDB lake: `internal/lake/{lake,safety,catalog,ingest,util}.go`
- Callers (read-only for Phase 2): `internal/gateway/lake_tools.go`,
  `internal/api/lake_routes.go`, `cmd/gateway/main.go`,
  `cmd/gateway/lake_cmd.go`.
- ADR 0004 (current state, will be updated in Phase 5):
  `docs/adr/0004-lake-engine.md`.
- CH docker stack: `deploy/clickhouse/docker-compose.yaml` plus
  `config.d/toolyard.xml` and `users.d/toolyard.xml`.
- CH password file (rendered by toolyard, read by docker-compose):
  `/var/lib/toolyard/clickhouse-runtime.env` (var name is
  `TOOLYARD_CH_PASSWORD`, **not** `CLICKHOUSE_PASSWORD` — the latter
  triggers the upstream entrypoint into a write that fights our
  read-only `users.d` mount).

### How to bring CH up locally for development

CH is bound to loopback only. From the host:

```bash
# Bring up:
sudo docker compose -f deploy/clickhouse/docker-compose.yaml up -d

# Get the password:
sudo cat /var/lib/toolyard/clickhouse-runtime.env

# Smoke-test:
PW=…
curl -sS -u "default:$PW" --data-binary 'SELECT version()' \
  http://127.0.0.1:8123/

# For Go tests, set:
export TOOLYARD_CH_TEST_ADDR=127.0.0.1:9000
export TOOLYARD_CH_TEST_PASSWORD=$PW
```

The Phase 1 migration tool's smoke harness in this conversation
brought up an ephemeral CH on ports `18923` (HTTP) / `19000`
(native) using `docker run` with the same config.d/users.d mounts —
look at `cmd/lake-migrate/main.go` block comments for the pattern if
you want a self-contained throwaway instance for tests instead of
sharing the deployed one.
