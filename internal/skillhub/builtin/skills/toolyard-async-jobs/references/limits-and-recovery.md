# Job limits and recovery

Defaults. An administrator can change the run time limit
(`-async-job-timeout`).

## Limits

| Limit | Value |
|---|---|
| One job's run time | 1 hour; it replaces the per-call upstream timeout for that call |
| Queued and running jobs | 32 per agent; 64 for all agents of one owner; 8 for all anonymous callers together |
| Over that cap | A tool the administrator made async runs inline instead. `jobs.start` and MCP tasks answer `job_budget_exceeded`, `POST /v1/jobs` answers 429; nothing ran. List them with `jobs.list` `{"status": "active"}` |
| Jobs running at once per replica | 64, at most 16 of them one agent's; more wait `queued` |
| One wait (`jobs.wait`, `Prefer: wait`) | 60 seconds |
| Waits in progress per agent, per replica | 8 (`jobs.wait`, `tasks/result`, `Prefer: wait`); past it `jobs.wait` and `tasks/result` answer an error, and `Prefer: wait` answers the current state at once |
| `jobs.wait` job ids | 32 per call |
| Result carried by `jobs.status` and `jobs.wait` | 256 KiB; a larger one says `result_omitted`: read it with `jobs.result` |
| Largest result kept | 32 MiB; above it the result is not kept (`result_bytes` 0), and `error` says why with its size, unless the job already has an error |
| Result bytes kept | 256 MiB per agent, 1 GiB per owner over the retention; a result past either is not kept (the job still ends with its status; `error` says why unless it already holds another error) |
| `callback_ref` | at most 512 characters |
| Retention | 7 days after the job ends; a task recorded for a call answered at once, 1 hour |

## What a job checks while it waits and runs

- When a queued job starts, access and policy are checked again. A tool
  whose access was removed, or whose policy now denies the call (or asks for
  approval, for a call the policy had allowed), fails without running.
- At each heartbeat the access of a queued or running job's agent is checked.
  Two refusals in a row stop the job (one may be a short database error).
- A job stopped before its call was sent says nothing ran.

## Restarts, shutdowns and other replicas

- On shutdown a replica refuses new jobs, ends waits, and gives running jobs
  until its drain timeout. A job still running then is cancelled and
  recorded `outcome_unknown`, with a `job.failed` callback whose
  `may_have_run` is true. A call that had already succeeded keeps its result;
  one that provably never ran is `failed`.
- If a replica dies, recovery on another replica takes its expired jobs: a
  `running` job becomes `outcome_unknown` (its call may have run; it is never
  run again), a `queued` one `failed` (nothing ran).
- `jobs.cancel` works from any replica: the owning replica sees the cancel
  within one heartbeat and interrupts the call.
- Waits see jobs on other replicas too (a database poll every second).

## Progress

When an upstream sends progress notifications for a job's call, toolyard
records the latest one (at most once a second). `jobs.status` shows it as
`progress` (`progress`, `total`, `message`, `at`), and MCP `tasks/get` as
the `statusMessage`.

## `after` mode and a caller that gives up

When the caller goes away (a client timeout, a script's deadline) before the
`after` wait ends, the call becomes a job at once, so it can still be found
with `jobs.list`, waited on and cancelled. When no job can be stored (the
budget is full, the server is restarting), the call goes on as an inline
call with an inline call's limits: it stops when its caller goes away, and at
the per-call upstream timeout. A call cut short by that timeout may have run.
