# Jobs over REST and MCP Tasks

Both read and write the same jobs as the `jobs.*` tools: a task id is a job
id, and a job started over REST shows in `jobs.list`.

## REST

Use an agent token or a session token in `Authorization: Bearer <token>`,
never an operator token. `TOOLYARD_URL` is your toolyard's base URL and
`TOOLYARD_TOKEN_FILE` the file that holds your token (`chmod 600`). Read the
header from that file, as below, so the token never appears in argv.

| Route | What it does |
|---|---|
| `POST /v1/jobs` | Start a call as a job. JSON body `{"tool", "arguments", "reason", "callback_ref", "wait_seconds"}`. Headers: `Idempotency-Key`, `Prefer: wait=N`. |
| `GET /v1/jobs?status=&limit=&before=` | Your jobs, newest first. `status` as in `jobs.list`, `limit` 1 to 100, page with the `next` cursor as `before`. |
| `GET /v1/jobs/{id}` | One job. `Prefer: wait=N` waits for it to end. |
| `GET /v1/jobs/{id}/result` | A finished job's tool result (the `result_ref` of a callback). |
| `POST /v1/jobs/{id}/cancel` | Cancel it. |

```sh
auth() { printf 'Authorization: Bearer '; tr -d ' \r\n\t' < "$TOOLYARD_TOKEN_FILE"; echo; }

curl -sS -X POST "$TOOLYARD_URL/v1/jobs" -H @<(auth) \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: monthly-2026-09' -H 'Prefer: wait=10' \
  -d '{"tool": "reports.build-monthly", "arguments": {"month": "2026-09"},
       "reason": "Build the September report the owner asked for"}'
curl -sS "$TOOLYARD_URL/v1/jobs/job_…" -H @<(auth) -H 'Prefer: wait=60'
curl -sS "$TOOLYARD_URL/v1/jobs/job_…/result" -H @<(auth)
```

- `POST /v1/jobs` answers `202` with `Location` while the job runs, and `200`
  once it ended within the wait (`Prefer: wait` or `wait_seconds`, 0 to 60,
  default 0).
- A call answered without a job (unknown tool, refused, held for approval)
  is `{"status": "not_started", "result": …}`; it is `422` when that answer
  is an error.
- The same `Idempotency-Key` (1 to 128 characters) with the same tool and
  arguments returns the existing job; with other ones it is
  `422 idempotency_key_reused`. A retry whose `_grant` the first job already
  used still gets that job. Use a key on every start you might retry.
- Over the job budget: `429`, nothing runs.
- Async jobs off on the server: `503 async jobs are not enabled on this server`.

## MCP Tasks

Clients that speak MCP Tasks (protocol 2025-11-25) add `"task": {}` to
`tools/call`. `tools/list` marks every tool `execution.taskSupport:
"optional"`, because any tool can run as a job; the mark does not mean the
tool is slow.

- toolyard answers `CreateTaskResult`; the task id is the job id.
- `tasks/get`: the task's status. `tasks/result`: waits until the task ends,
  then answers the tool's result. `tasks/cancel` and `tasks/list` also work.
- Task statuses: `working` (queued or running), `completed` (succeeded),
  `failed` (failed, or outcome unknown, with a `statusMessage` that starts
  `outcome_unknown:`), `cancelled`. The poll interval hint is 2 seconds.
- Any replica answers any task, and a task survives a restart.
- A call answered at once (refused before it could run, held for approval,
  or answered inline) still gets a task, already final, whose result is that
  answer. Such a task is kept 1 hour, and one agent may hold 200 of them.
  Past that there is no task: a toolyard refusal (nothing ran) comes back as
  a JSON-RPC error, any other answer as the plain `tools/call` result.
- On protocol 2026-07-28 there is no `tasks/result` or `tasks/list`:
  `tasks/get` carries the result once the task ended.
- `tasks/result` counts against your waits in progress (8 per agent on one
  replica). Past that it answers an error: use `tasks/get`.

Clients without tasks get the job envelope from the `jobs.*` flow instead.
