# toolyard

Self-hosted MCP gateway with **per-tool-call human approval pushed to your
phone** and built-in shared memory. One Go binary + SQLite + a PWA dashboard.

Every coding agent today (Claude Code, Cursor, Codex CLI, web Claude)
reimplements its own MCP config, approval UX, and audit log. `toolyard` sits
between any number of agents and any number of upstream MCP servers, gives
you a single place to enroll servers and approve writes, and pushes approval
cards to your phone with the model's reasoning visible.

## Status

**v0.1 — works end-to-end.** Reads pass, writes hold for human approval,
audit log streams live to the dashboard, Web Push lights up your phone, and
the Claude Code hook lets long approvals resume cleanly. See
[`docs/architecture.md`](docs/architecture.md).

## Quickstart

```bash
go build -o toolyard ./cmd/gateway
./toolyard serve -addr :8787 -data ~/.toolyard
```

Open <http://localhost:8787>, create the local admin account, and:

1. **Agents → Generate code**, get a one-time enrollment code.
2. From your terminal:
   ```bash
   ./toolyard exchange -url http://localhost:8787 -code <code>
   ```
   Save the returned `token`.
3. Point your MCP-aware agent at `http://localhost:8787/mcp` with
   `Authorization: Bearer <token>`. Or run the gateway with `-stdio` for
   stdio-only hosts.
4. (Optional) **Settings → Enable push** in the dashboard to get notifications
   on this device.

The full self-hosting guide is in [`docs/self-hosting.md`](docs/self-hosting.md).

## What's in v0.1

- Stdio + streamable HTTP MCP server, agent enrollment via short-lived
  codes, sha256-hashed long-lived tokens.
- Schema-wrap that injects a required `_reason` field (20–2000 chars) and
  optional `_intent_category` enum into every upstream tool's input schema.
- Hardcoded policy: `read|get|list|search|...` pass through, everything
  else holds for human approval.
- Approval bus with Ed25519-signed decision tokens, durable to SQLite,
  with hybrid in-line wait + deferred-response degradation per the
  architecture plan.
- Built-in memory MCP (`memory.set`, `memory.get`, `memory.list`,
  `memory.delete`) backed by the same SQLite DB.
- Live SSE dashboard: pending approvals, audit feed, agent enrollment,
  memory browser, push setup.
- Web Push (VAPID) with one-tap allow/deny actions in the notification.
- `scripts/claude-code-hook.sh` reference implementation of the
  PostToolUse hook that resumes deferred approvals.
- Multi-stage `deploy/Dockerfile` + `deploy/docker-compose.yml`.

## Inbox and scoped permissions

Agents ask for restricted tools the way a colleague would: one request per
task, in their own words, with a voice note and evidence. You review it on
your phone and allow some or all of the tools; each allowed tool gets a
single-use permission (a grant) tied to the exact parameters you saw.

- **Agents** call `inbox.check` to see which planned calls are restricted,
  then one `inbox.request` with a first-person message, a ≤75-word voice-note
  script, attachments (tables, charts, diffs, logs, links; images, video and
  files as links toolyard copies), and every tool with its parameters. When
  you approve, `inbox.wait` hands each allowed tool a `tyg_…` token; the
  agent passes it as `_grant` on the call. Questions and blockers go through
  `inbox.ask`, outcomes through `inbox.post`.
- **Toolyard** checks each request in the background and adds only flags
  (Production, Can't be undone, Deletes data, Value not known yet, …).
  An optional judge model (Gemini, off by default) flags calls that
  contradict the agent's own description. "Summarize with toolyard" and
  "Ask toolyard" give its own reading on demand.
- **You** see the Inbox tab: the agent's message and voice note first,
  evidence next, the decision last. The Sessions tab shows what each agent
  is doing, its live permissions, and a kill switch.

`approval_mode` (Settings → Inbox & permissions) decides what happens when
an agent calls a restricted tool without a grant: `execute` (default)
keeps the existing queue-and-run approvals; `inbox` runs nothing and
returns `permission_required` with a pre-filled draft request, so agents
learn the protocol from their first mistake. Grants work in both modes.

Agents learn the rules from `inbox.guide` (served from
[`docs/guidelines/agent-protocol.md`](docs/guidelines/agent-protocol.md)),
the `toolyard://guide` MCP resource, `GET /v1/guide`, or the
`toolyard-inbox` skill (`toolyard skills install toolyard-inbox`, or the
"Teach it the rules" tab when you enroll an agent). The design is in
[`docs/guidelines/`](docs/guidelines/).

## Personal data lake (TUS-104)

toolyard ships a DuckDB-backed warehouse so any "data not modified per row"
— finance JSONs, daily memory markdowns, Nova SQLite stores, ingest logs —
can land in one queryable place. File: `~/.toolyard/lake.duckdb`.

**Tools agents can call (no approval, additive):**

- `lake.query` — read-only SQL (SELECT/WITH/SHOW/PRAGMA only).
- `lake.list_tables`, `lake.describe_table` — catalog introspection.
- `lake.insert` — append rows (structured or raw INSERT).
- `lake.create_table` — CREATE TABLE/VIEW/INDEX. CREATE OR REPLACE allowed.
- `lake.ingest` — bulk load CSV/Parquet/JSON/JSONL/SQLite/HTTP into a target.

**Tools agents can call (phone approval required):**

- `lake.update`, `lake.delete` — WHERE is mandatory.
- `lake.alter`, `lake.drop`.

**Schema layout:**

- `raw.*` — immutable mirrors of source files; lineage record only.
- `mart.*` — Kimball dimensional model: `dim_*` (account, category,
  investment, physical_asset, learning_goal, exercise, date) and
  `fact_*` (account_balance, investment_valuation, credit_card_bill,
  net_worth_snapshot, recurring_schedule, workout, exercise_set,
  chat_interaction, daily_memory, deployment_plan).
- `app.*` — agent's free-form scratch space.

**Ingest:** drop new files in `~/.toolyard/inbox/`, then call
`lake.ingest` (CSV/Parquet/JSON/JSONL/SQLite/HTTP). DuckDB is the
sole source of truth; there is no scheduled refresh from any external
location.

**Backup:** `toolyard lake backup` runs `EXPORT DATABASE` to
`<data>/backups/lake-<ts>/`.

**Dashboard:** [/lake/](http://192.168.1.179:18787/lake/) — manifest-
driven, vanilla JS + ECharts. Adding a new chart = drop a `.sql` file in
`web/lake/queries/<tab>/` and append a panel to
`web/lake/manifest.json`. No JS change required.

Architectural decisions live in
[`docs/adr/0004-lake-engine.md`](docs/adr/0004-lake-engine.md).

## Verification

End-to-end smoke test (run while the binary is up on `:18787`):

```bash
go test -tags=e2e ./scripts -run TestEndToEnd -toolyard=http://localhost:18787 -v
```

It covers the full happy path: enroll, list tools, read passes, write
holds, dashboard approves, write completes, audit log fills.

The inbox flow (coaching, dry run, request, approval, single-use grant,
closing update) has its own test; `-media` optionally points at a server
with `shot.jpg` and `rec.mp4` to check attachment copying:

```bash
go test -tags=e2e ./scripts -run TestE2EInbox -toolyard=http://localhost:18787 -v
```

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
