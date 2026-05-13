# ADR 0004 — Personal data lake (ClickHouse)

Status: accepted (TUS-104, superseded engine choice in TUS-141 cutover)
Date: 2026-05-09 (DuckDB landing), 2026-05-12 (ClickHouse cutover)

## Context

toolyard exposes action tools (push approvals, MCP integrations) but has no
landing surface for *data* — Kite holdings, bank/CC JSONs, daily memory
markdowns, interactions logs, and the various Nova-side SQLite files. The
user wants a single place to ingest, query, and visualise all of it without
standing up Postgres or Snowflake.

TUS-104 originally scoped the feature to a DuckDB-backed warehouse for
simplicity (single embedded file, no separate server process). That landed
on 2026-05-09 and worked well for the first few weeks. Two pressures pushed
us to ClickHouse:

1. **Grafana**: panel queries had to round-trip through a custom
   `/v1/lake/exec` endpoint on the toolyard gateway because Grafana has no
   first-party DuckDB datasource. The Infinity plugin worked but required
   JSONata projection mapping per panel and was brittle. With ClickHouse,
   Grafana's official `grafana-clickhouse-datasource` talks to the server
   directly — no gateway hop, no projection mapping.

2. **Concurrency**: DuckDB takes a process-level write lock on its file.
   This blocks any second consumer (Grafana via host-mode, backup tools,
   debug shells) and meant operational sequences like "rotate the toolyard
   binary while keeping the lake queryable" required stopping the lake too.

ClickHouse runs as a sibling container, has a real client/server protocol,
and is queryable concurrently by the gateway, Grafana, and ad-hoc clients.
The cutover (Phase 1-5) ran 2026-05-10..2026-05-12.

## Decisions

1. **Engine**: ClickHouse 24.8, deployed via `deploy/clickhouse/docker-compose.yaml`.
   Host bindings `127.0.0.1:18123` (HTTP) and `127.0.0.1:19000` (native).
   Loopback-only on the host; Grafana reaches it via the shared
   `toolyard_lake_net` docker network at `clickhouse:9000`. Single `default`
   user; password lives in the toolyard settings DB (`clickhouse_password`),
   rendered to `/var/lib/toolyard/clickhouse-runtime.env` on every gateway
   boot. The gateway opens the lake with `lake.Open(lake.Config{Addr,
   User, Password})` against the host bindings.

2. **Read-only enforcement**: CH has no per-connection read-only mode
   equivalent to DuckDB's. `lake.query` continues to call
   `ValidateSelect()` before reaching the driver — defense in depth
   equivalent to a separate read pool.

3. **Tool prefix**: unchanged — `lake.*`, dashboard at `/lake/`,
   REST under `/v1/lake/*`. The MCP tool surface is byte-compatible with
   the DuckDB era; agents using `lake.query`/`lake.insert`/etc. didn't
   need any code changes.

4. **Approval matrix**: unchanged.

   | Tool                | Forced action | How enforced                      |
   | ------------------- | ------------- | --------------------------------- |
   | `lake.query`        | allow         | `forcedAction = ActionAllow`      |
   | `lake.list_tables`  | allow         | `forcedAction = ActionAllow`      |
   | `lake.describe_table`| allow        | `forcedAction = ActionAllow`      |
   | `lake.insert`       | allow         | `forcedAction = ActionAllow`      |
   | `lake.create_table` | allow         | `forcedAction = ActionAllow`      |
   | `lake.ingest`       | allow         | `forcedAction = ActionAllow`      |
   | `lake.update`       | approve       | default policy (name heuristic)   |
   | `lake.delete`       | approve       | default policy (name heuristic)   |
   | `lake.alter`        | approve       | default policy (name heuristic)   |
   | `lake.drop`         | approve       | default policy (name heuristic)   |

5. **Schema layout**: unchanged — `raw`, `mart`, `app` databases (CH
   "database" ≡ DuckDB "schema"; identifier syntax `db.table` is
   identical). `mart.*` views ported to CH dialect and applied
   idempotently on install from `deploy/clickhouse/mart-views.sql`.

6. **Ingest paths**:
   * **csv / jsonl / json** — parsed in Go, bulk-loaded via the CH
     native protocol's `PrepareBatch` API.
   * **http(s)** — delegated to CH's `url()` table function for
     server-side streaming.
   * **parquet / sqlite** — return `lake.ErrSourceNotImplemented`
     pending a follow-up. The original DuckDB ingest had these via
     `read_parquet` and the `sqlite_scanner` extension; the CH-side
     equivalent (`file()` and `sqlite()` table functions) needs the
     source file mounted into the container's `user_files/` dir, which
     isn't an automatic path for agent-supplied URIs.

7. **Mutations**: `lake.update` and `lake.delete` translate to CH's
   `ALTER TABLE … UPDATE/DELETE` with `mutations_sync=2` for synchronous
   semantics. `rows_affected` is computed by a pre-mutation `count()`
   over the WHERE predicate — CH's mutation API doesn't return a row
   count, but the count is exact.

8. **Defense in depth**: unchanged in spirit, except for the `PRAGMA`
   keyword being dropped from the read-only allowlist (CH has no useful
   PRAGMAs; `SYSTEM` is deliberately not allowed because it mutates
   server state).

9. **Discipline**: unchanged. 1000-row default cap, 10K max,
   2 MiB byte cap, 30 s timeout. `result.Truncated` communicated back.

10. **Backup**: `toolyard lake backup` no longer emits a SQL dump
    (DuckDB's `EXPORT DATABASE` had no clean CH equivalent without
    server-side BACKUP-engine config). Instead it prints the filesystem
    snapshot recipe — `/var/lib/toolyard/clickhouse/data/` is captured by
    the existing `tar -czf /var/lib/toolyard.tgz` pattern.

11. **Out of scope (v0.1)**: scheduling, encryption at rest (FDE),
    multi-user. The `parquet`/`sqlite` ingest paths re-enter scope as
    follow-up work.

## Alternatives considered

* **Stay on DuckDB** — kept things simple but Grafana integration was
  brittle and the file-lock model blocked concurrent operations.
* **chDB / DataFusion** — in-process CH-compatible engines; rejected
  because the operational pain we were trying to solve was Grafana
  needing a separate process to talk to, not the engine's compute model.
* **Postgres** — too heavy for a single-user analytical workload; OLAP
  query plans would be a regression.
* **Hosted ClickHouse Cloud** — adds an internet hop and a recurring
  bill; we have a single host that can run a 2 GiB container fine.

## Migration history

The cutover landed in five phases:

1. **Phase 1** (2026-05-10) — one-shot DuckDB → CH data migration tool
   at `cmd/lake-migrate/`. Reads DuckDB read-only, writes to CH with
   row-count parity verification. Deleted in Phase 5.
2. **Phase 2** (2026-05-11) — `internal/lake/` rewritten against CH via
   `clickhouse-go/v2`. Public Service API preserved byte-compatible.
3. **Phase 3** (2026-05-12) — gateway callers ported from
   `lake.Open(path)` to `lake.Open(lake.Config{...})`; CH password
   plumbed through `settings.EnsureClickhousePassword()`.
4. **Phase 4** (2026-05-11) — Grafana datasource swapped from Infinity
   to `grafana-clickhouse-datasource`; dashboards regenerated against
   the CH plugin's rawSql target shape.
5. **Phase 5** (2026-05-12) — `cmd/lake-migrate/` deleted, go-duckdb
   removed from `go.mod`, the 2 remaining mart views translated to CH
   and pinned in `deploy/clickhouse/mart-views.sql`, this ADR updated.

The DuckDB snapshot at `/var/lib/toolyard/lake.duckdb` is retained as
`lake.duckdb.snapshot-<date>` for ~30 days post-cutover as a diff target
in case CH ever returns unexpected numbers. Reclaim the ~28 MiB when
confidence is high.

## Follow-ups

* `parquet` and `sqlite` ingest paths re-enable, probably by
  copying agent-supplied URIs into the CH container's `user_files/`
  mount and delegating to CH's table functions.
* Marimo notebook (`notebooks/`) — parallel surface for ad-hoc
  exploration; the dashboard is the canonical surface.
