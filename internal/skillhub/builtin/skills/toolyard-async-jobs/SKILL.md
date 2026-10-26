---
name: toolyard-async-jobs
description: Work with toolyard async jobs, where a tool answers with a job_id instead of its result, using jobs.start, jobs.status, jobs.wait, jobs.result, jobs.cancel and jobs.list. Explains how to recognise an async answer (job_id, status queued, toolyard.job in _meta), why you never call the tool again for the same work, waiting up to 60 seconds at a time, MCP Tasks (task-augmented tools/call, tasks/get, tasks/result), job.completed and job.failed callbacks, result retention (7 days), job_budget_exceeded, how permission_required, grants and grant_invalid work with jobs, and what failed, cancelled, outcome_unknown and may_have_run mean for retries. Use when a call returns a job_id, when a tool is slow or times out, when you want work to run in the background or from code mode, or when a job ends failed, cancelled or outcome_unknown.
metadata:
  toolyard-runs: anywhere
---

# Async jobs

Some tools take minutes: a report, a backfill, a crawl. An async call answers
at once with a **job id**. The call keeps running inside toolyard, and you
read the result later, block on it for up to a minute at a time, or get a
callback when it ends.

A job is not a way around anything. It starts only after the call was
admitted exactly as an inline call: access, policy, approval or a redeemed
grant, identity forwarding and audit all come first.

## When to use

- A call answered with a `job_id` instead of its result.
- A tool is slow, or a direct call timed out: run it with `jobs.start`.
- You want several slow calls to run while you do other work.
- A job ended `failed`, `cancelled` or `outcome_unknown` and you must decide
  what to do next.

If the `jobs.*` tools are not in your catalog, async jobs are off on this
gateway.

## Recognise an async answer

An administrator marks upstream servers, or single tools, async:

- `always`: every call answers with a job id.
- `after N ms`: the call runs inline, and only a call still running after
  N ms becomes a job. The same tool can answer inline once and with a job
  the next time, so always check the shape of the answer.

`servers.list` shows each server's `async` setting. Any tool can also run as
a job on your own request (`jobs.start`), whatever its setting.

An async answer looks like this (also summarised in the text content):

```json
{"job_id": "job_3f…", "status": "queued", "tool": "reports.build-monthly",
 "poll": "jobs.status", "wait": "jobs.wait", "result": "jobs.result", "cancel": "jobs.cancel",
 "next_steps_for_agent": {"…": "…"},
 "message": "The call runs as an async job. Do not call the tool again for the same work: …"}
```

Its `_meta` carries `toolyard.job: true` and `toolyard.job_id`.

## Recipe

1. Call the tool, or start it as a job yourself:

   ```text
   jobs.start
   {"tool": "reports.build-monthly", "arguments": {"month": "2026-09"}, "_reason": "Build the September report the owner asked for; it takes several minutes"}
   ```

2. You get a `job_id`. **Do not call the tool again for the same work**: the
   job is already running.
3. Block on it, up to 60 seconds per call, and call again while it runs:

   ```text
   jobs.wait
   {"job_id": "job_3f…", "timeout_seconds": 60, "_reason": "Wait for the September report job to finish before summarising it"}
   ```

   Or end your turn and check later with `jobs.status`, or let a callback
   tell you (see [Callbacks](#callbacks)).
4. Read the result. A final `jobs.status` or `jobs.wait` answer carries it
   when it is at most 256 KiB; otherwise it says `result_omitted`, so call
   `jobs.result`, which answers exactly what the tool returned.
5. Act on the final status (table below).

## The jobs tools

| Tool | Arguments | What it does |
|---|---|---|
| `jobs.start` | `tool` (catalog name), `arguments`, `callback_ref?` | Runs any tool you may call as a job; same rules as a direct call. A `_reason` inside `arguments` wins; else this call's is used. Its own `_callback_ref` works as `callback_ref`. |
| `jobs.wait` | `job_id` or `job_ids` (up to 32), `mode?` (`all` default, `any`), `timeout_seconds?` (1 to 60, default 60) | Blocks until the job ends (or one or all of several), then returns status and result. `timed_out: true` means it is still running. |
| `jobs.status` | `job_id` | Never blocks: status, last progress, error, and the result once ended (up to 256 KiB). |
| `jobs.result` | `job_id` | The tool's result exactly as returned (content, structured content, `isError`). While the job runs it answers the status. |
| `jobs.cancel` | `job_id` | A queued job never runs. A running call is interrupted, but a write may already have taken effect. A job that already ended is unchanged. |
| `jobs.list` | `status?`, `limit?` (default 20, at most 100) | Your jobs, newest first, without results. `status`: `active` (queued and running), `queued`, `running`, `succeeded`, `failed`, `cancelled`, `outcome_unknown`. |

A job belongs to the agent that started it. Another agent's job id, or one
already purged, reads as `unknown job_id`.

## Statuses and what to do

| Status | Meaning | Next step |
|---|---|---|
| `queued`, `running` | Still going. | `jobs.wait` again, or check later. Never start the work again. |
| `succeeded` | Done. | Read `result`. |
| `failed` | The tool answered with an error (`result`), or the job never started (`error`). | Read both before you call the tool again. |
| `cancelled` | Cancelled. `may_have_run` says whether the call was sent. | When `may_have_run` is true, check the target before you call again. |
| `outcome_unknown` | The call may or may not have run (`may_have_run: true`). | Inspect the target's own state. Never repeat a write blindly. |

If toolyard cannot read the job state (a restart, a database error),
`jobs.wait` answers a retryable error with `status: unavailable`, and the
other `jobs.*` tools answer a read error. Neither means the job is gone: call
again in a few seconds and do not start the work again.

## Approvals and grants

- A call that needs approval answers as it always does (`permission_required`,
  or a pending legacy approval) and starts **no** job. Ask through the Inbox:
  see the `toolyard-permissions-and-inbox` skill
  (`skills.get toolyard/toolyard-permissions-and-inbox`).
- Once you hold the grant, call the tool with `_grant` as usual. If the call
  becomes a job, the grant is checked at once (an invalid one answers
  `grant_invalid` and starts no job) and redeemed once, inside the job.
- A queued job checks access and policy again when it starts. If the access
  was removed, or the policy now denies or asks for the call, the job fails
  without running.
- A queued or running job stops when its agent or owner loses access
  (disabled, blocked, server access withdrawn). A job stopped before its call
  was sent says nothing ran.
- GitHub writes never become jobs that wait: they return a deferred
  `approval_id` (see the `toolyard-github-writes` skill).

## Callbacks

Pass `callback_ref` to `jobs.start`, or the reserved argument `_callback_ref`
on any call. When the job ends, toolyard sends the receiver one event:
`job.completed` (succeeded) or `job.failed` (failed, cancelled,
outcome_unknown). The event carries `job_id`, `tool`, `status`, `is_error`,
`result_ref`, `result_bytes`, `error`, `finished_at` and, for cancelled and
outcome_unknown, `may_have_run`. It **never** carries the result: read it
with `jobs.result` (or `GET result_ref` under your own token).

- The receiver gets job events only if its `events` list asks for them:
  `job.completed`, `job.failed`, `job.*` or `*`. A receiver that names no
  events gets only the Inbox events.
- Without a `callback_ref`, your default receiver (if any) gets the event.
- A `callback_ref` that is not one of your active receivers sends nothing;
  the job shows why in `callback_error`.
- An `after N ms` call that finished in time answered inline and sends no
  callback.

Registering receivers: the `toolyard-callbacks` skill.

## From code mode

A script that calls an async tool gets the job envelope as the call's value:

```python
# reports.build-monthly is marked async "always", so it answers with a job
job = reports.build_monthly(month="2026-09")
done = toolyard.jobs_wait(job_id=job["job_id"], timeout_seconds=60)
result = {"status": done["status"], "timed_out": done.get("timed_out")}
```

`toolyard.jobs_start(tool=..., arguments=...)` starts any tool as a job from
a script. A wait inside a script also ends with the script's clock (5 minutes
by default). Calls made inside a job (a script run as a job, `tools.execute`,
`jobs.start` there) run inline: `jobs.start` inside a job returns the tool's
own result, not a job id. A job a script started cannot start code mode.
See the `toolyard-code-mode` skill.

## Expiry and limits

- Results are kept 7 days after the job ends; after that the job id reads as
  unknown. Read what you need before then.
- One job runs at most 1 hour by default (the administrator's
  `-async-job-timeout`).
- At most 32 queued and running jobs per agent, 64 for all agents of one
  owner. Over the cap, `jobs.start` (and `POST /v1/jobs`, 429) is refused with
  `job_budget_exceeded` and nothing runs; a tool the administrator made async
  runs inline instead.
- At most 8 waits in progress per agent on one replica. Past that,
  `jobs.wait` answers an error: use `jobs.status`.

Full table, REST routes and MCP Tasks: [REST and MCP Tasks](references/rest-and-tasks.md),
[limits and recovery](references/limits-and-recovery.md).

## Pitfalls

- Calling the tool again "to check" starts a second job and a second write.
  Use the `job_id`.
- `jobs.cancel` on a running job does not undo a write that already
  happened. Check the target before you call the tool again.
- `outcome_unknown` after a restart: a job that was running when its replica
  stopped is never run again. Reconcile against the target.
- `jobs.wait` returning `timed_out: true` is not a failure: the job keeps
  running whether or not you wait.
- `skills.bundle` is refused inside an async job: call it directly.

## Related skills

- `toolyard-callbacks`: receivers for `job.completed` and `job.failed`.
- `toolyard-code-mode`: scripts that start and wait for jobs.
- `toolyard-permissions-and-inbox`: `permission_required` and grants.
- `toolyard-basics`: `_reason`, `servers.list`, tool discovery.
