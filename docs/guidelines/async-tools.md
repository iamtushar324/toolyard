# Async tools

Some tools take minutes: a report, a backfill, a crawl. Holding an MCP call
open that long hits client timeouts, the gateway's `-upstream-call-timeout`
(120 s by default) and load-balancer limits, and it ties up the agent's
turn. An **async tool** answers at once with a **job id** instead; the call
keeps running in toolyard, and the agent reads the result later, blocks on
it for up to a minute at a time, or gets a callback when it ends.

A job is not a way around anything. It starts only once the call was
admitted exactly as an inline call would be: access, policy, approval or a
redeemed grant, identity forwarding and the audit rows all apply first.

## 1. For administrators: marking tools async

An administrator marks a whole upstream server, or single tools of it,
async. Each rule has a mode:

| Mode | What a call does |
|---|---|
| `off` | Runs inline, as without async tools (the default). |
| `always` | Answers with a job id at once. |
| `after` | Runs inline, and answers with a job id only if it is still running after `after_ms` milliseconds (10 – 600000). A fast call answers as before and stores no job. |

A tool rule overrides the server's, so a server can be `always` with one
quick tool `off`, or `off` with one slow tool `always`.

- **Dashboard:** Servers → Edit server → *Async tool calls*, and the
  per-tool box (`tool_name: always`, `tool_name: after 5000`,
  `tool_name: off`, one per line; the name is the tool's own name on the
  server, without the `Server.` prefix).
- **REST (admin):**

  ```sh
  curl -X PUT "$TOOLYARD_URL/v1/servers/BkCoreServices/async" \
    -H 'Content-Type: application/json' -H 'X-Requested-With: toolyard' --cookie … \
    -d '{"mode":"off","tools":{"backfill-amz-ads-reports":{"mode":"always"},
         "get-client":{"mode":"after","after_ms":5000}}}'
  curl "$TOOLYARD_URL/v1/servers/BkCoreServices/async" --cookie …
  ```

  `{"mode":"off"}` with no tool rules clears the setting.

Saving never reconnects the server. The setting is stored in
`upstream_servers.async_json`, shows in `GET /v1/servers` (`async`) and in
the agents' `servers.list`, and applies on this replica at once and on the
others within 5 seconds. Tools marked async carry
`execution.taskSupport: "optional"` in `tools/list` for clients that speak
MCP Tasks.

Any tool can also run as a job on the agent's own request (`jobs.start`,
`POST /v1/jobs`, an MCP task), whatever its setting.

## 2. For agents: what an async call returns

```json
{ "job_id": "job_3f…", "status": "queued", "tool": "BkCoreServices.backfill-amz-ads-reports",
  "poll": "jobs.status", "wait": "jobs.wait", "result": "jobs.result", "cancel": "jobs.cancel",
  "next_steps_for_agent": { … } }
```

The same is in the text content. **Do not call the tool again for the same
work**: the job is already running. Then:

| Tool | What it does |
|---|---|
| `jobs.wait {job_id \| job_ids[], mode?, timeout_seconds ≤ 60}` | Blocks until the job ends (or one/all of several), then returns its status and result. Call it again while it is still running. |
| `jobs.status {job_id}` | Never blocks: status, last progress, and the result once it ended (results up to 256 KiB; larger ones only through `jobs.result`). |
| `jobs.result {job_id}` | The tool's result exactly as the tool returned it (content, structured content, `isError`). While the job runs it answers with the status. |
| `jobs.cancel {job_id}` | Cancels it: a queued job never runs; a running call is interrupted, but **a write may already have taken effect**, so check the target before calling the tool again. |
| `jobs.list {status?, limit?}` | Your jobs, newest first (`status`: `active`, `queued`, `running`, `succeeded`, `failed`, `cancelled`, `outcome_unknown`). |
| `jobs.start {tool, arguments, callback_ref?}` | Runs any tool you may call as a job. Same rules as a direct call. |

Statuses: `queued` → `running` → `succeeded` | `failed` (the tool answered
with an error, or the job never started) | `cancelled` | `outcome_unknown`
(the call may or may not have run: inspect the target's own state and never
repeat a write blindly).

A job is scoped to the agent that started it; another agent's id reads as
unknown. A dashboard caller sees its own agents' jobs (an admin: all).

**Code mode.** A script that calls an async tool receives the job envelope
as the call's value and can wait for it:

```python
job = BkCoreServices.backfill_amz_ads_reports(account="acme")
done = toolyard.jobs_wait(job_id=job["job_id"], timeout_seconds=60)
result = done["status"]
```

`toolyard.jobs_start(tool=..., arguments=...)` starts any tool as a job from
a script. Calls a job makes itself (a script run as a job, `tools.execute`,
and `jobs.start` inside a job) run inline inside it: `jobs.start` there
returns the tool's own result, not a job id.

**Permissions.** A call that needs approval answers exactly as before
(`permission_required`, or a pending approval) and starts no job. Once you
hold the grant, call the tool with `_grant` as usual: if the tool is async,
the grant is redeemed once, inside the job. A queued job checks access and
policy again when it starts: a tool whose access was removed, or whose
policy now denies the call (or asks for approval, for a call the policy
had allowed), fails without running. A running job stops when its agent
or owner loses access (disabled, blocked, server access withdrawn).

## 3. MCP Tasks

Clients that speak MCP Tasks (protocol 2025-11-25) add `"task": {}` to
`tools/call`; `tools/list` marks every tool `execution.taskSupport:
"optional"` because any tool can run as a job. toolyard answers `CreateTaskResult` (the task id is the job
id) and serves `tasks/get`, `tasks/result` (waits for the end), `tasks/cancel`
and `tasks/list` from the jobs table, so any replica answers any task and a
task survives a restart. Task statuses: `working` (queued or running),
`completed` (succeeded), `failed` (failed or outcome unknown, with a
`statusMessage`), `cancelled`. A call refused before it could run still gets
a task, already final, whose result is the refusal. Clients without tasks
get the job envelope above.

A 2026-07-28 request is checked like any other on that version (the
`Mcp-Protocol-Version` header must match `_meta`, and `Mcp-Method` /
`Mcp-Name` the body; errors carry HTTP 400 or 404). It gets `tasks/get`
(with the tool's `result` once the task ended) and `tasks/cancel`;
`tasks/result` and `tasks/list` do not exist in that version. The tasks
methods apply the same DNS rebinding protection as the rest of `/mcp`.

## 4. Callbacks

Any call may carry the reserved argument `_callback_ref` (stripped before
the tool sees it, like `_reason`); `jobs.start` takes `callback_ref`, and
`POST /v1/jobs` takes `callback_ref` in its body. When the job ends,
toolyard queues a callback event to that receiver, inside the transaction
that writes the final status:

```json
{ "type": "job.completed",            // or job.failed: failed, cancelled, outcome_unknown
  "data": { "job_id": "job_…", "tool": "…", "upstream": "…", "status": "succeeded",
            "is_error": false, "result_ref": "/v1/jobs/job_…/result", "result_bytes": 1832,
            "error": "", "callback_ref": "cb_…", "finished_at": 1790621520000 } }
```

The event never carries the result: read it with `jobs.result` or
`GET result_ref` under your own token. A call with `async after N ms` that
finished in time answered inline and sends no callback. Receivers are
registered with the `callbacks.*` tools (see the agent protocol), and get
job events only when their `events` list asks for them: `job.completed`,
`job.failed`, `job.*` or `*` (a receiver that names no events gets the
Inbox events only). Without a `_callback_ref` the agent's default
receiver, if any, applies. A `_callback_ref` that is not one of the
agent's active receivers sends nothing; the job records why in
`callback_error`.

## 5. REST

Same tokens as the code routes (an agent token or a session token, never an
operator token):

```sh
curl -X POST "$TOOLYARD_URL/v1/jobs" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: backfill-acme-1' -H 'Prefer: wait=10' \
  -d '{"tool":"BkCoreServices.backfill-amz-ads-reports","arguments":{"account":"acme"},
       "reason":"Backfill the ads reports the owner asked for","callback_ref":"cb_…"}'
curl "$TOOLYARD_URL/v1/jobs/job_…" -H 'Prefer: wait=60' -H "Authorization: Bearer $TOKEN"
curl "$TOOLYARD_URL/v1/jobs/job_…/result" -H "Authorization: Bearer $TOKEN"
curl -X POST "$TOOLYARD_URL/v1/jobs/job_…/cancel" -H "Authorization: Bearer $TOKEN"
curl "$TOOLYARD_URL/v1/jobs?status=active&limit=20" -H "Authorization: Bearer $TOKEN"
```

- `POST /v1/jobs` answers `202` with `Location` while the job runs, `200`
  once it ended within `Prefer: wait=N` / `wait_seconds` (0 – 60, default
  0). A call answered without a job (unknown tool, refused, held for
  approval) is `{"status": "not_started", "result": …}`: `422` when that
  answer is an error.
- The same `Idempotency-Key` with the same tool and arguments returns the
  existing job; with other ones it is `422 idempotency_key_reused`.
- `GET /v1/jobs` pages with `next` → `?before=<cursor>`.

## 6. Limits

| Limit | Value |
|---|---|
| One job's run time | `-async-job-timeout` (default 1 h); it replaces `-upstream-call-timeout` for the call |
| Queued + running jobs per agent | 32; all agents of one owner together 64; anonymous callers together 8. Over the cap a tool the administrator made async runs inline instead; `jobs.start`, `POST /v1/jobs` (`429`) and MCP tasks are refused (`job_budget_exceeded`) |
| Jobs running at once per replica | 64, at most 16 of them one agent's; more wait `queued` |
| One wait (`jobs.wait`, `Prefer: wait`) | 60 s |
| Result kept inline in the row | 256 KiB; larger results go to the blob store (files under `<data>/job-results` on SQLite, chunks in Postgres) |
| Largest result kept | 32 MiB (above: the job records the size and an error, not the bytes) |
| Retention | 7 days after the job ends |

## 7. High availability

- Every job row carries `owner_instance` (the replica that runs it) and
  `lease_until`, on the database's clock (so clock drift between replicas
  never ends a lease early). That replica renews the lease every 5 s while
  the job is queued or running (30 s leases), and claims it `queued` →
  `running` with a compare-and-set. A replica that finds one of its jobs
  final or owned elsewhere at a renewal stops its local run.
- The caps are checked and the job inserted in one transaction, under a
  Postgres advisory lock on the owner (SQLite has one writer).
- Large results are stored under a domain-separated key (the sha256 of
  `tool_job`, a NUL byte and the result), so the Inbox's blob sweeps can
  never remove one.
- Recovery runs on every replica and takes only rows whose lease ended: a
  `running` job becomes `outcome_unknown` (its call may have run; it is
  never run again), a `queued` one `failed` (nothing ran). A live
  replica's jobs are never touched. Recovery, a job that never started, an
  unknown outcome and a job stopped because access was removed write
  `job.recovered`, `job.not_started`, `job.outcome_unknown` and
  `job.revoked` audit rows.
- `jobs.cancel` on another replica sets `cancel_requested`; the owning
  replica sees it within one heartbeat and interrupts the call.
- Waiters (`jobs.wait`, `tasks/result`, `Prefer: wait`) are woken in
  process and fall back to a database poll every second, so a job running
  on another replica is seen.
- On shutdown a replica refuses new jobs, ends waits, and gives running
  jobs until `-drain-timeout`; what is still running then is cancelled and
  recorded `outcome_unknown`.
- Upstream `notifications/progress` for a job's call (toolyard sends its
  own progress token on each upstream call a job makes) are recorded on the
  job, the latest one at most once a second, off the upstream's read loop,
  and shown by `jobs.status` and `tasks/get` (`statusMessage`).
