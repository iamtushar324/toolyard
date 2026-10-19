# Code mode over REST: the backup route when MCP fails (TEC-1627)

An agent whose MCP connection fails can still run a toolyard script with plain
HTTPS: `curl` or the `ty` command. The script runs under the same agent
identity, the same policy and the same Inbox approvals as MCP code mode
(`executeToolCode`). This document is the contract the code, the OpenAPI
document (`docs/openapi/code.json`) and `ty` follow.

## Credentials

| Token | Format | Who holds it | Allowed on |
|---|---|---|---|
| Agent token | `ag_<uuid>.<secret>` | an agent (or T3's server, per user) | every agent route, including minting session tokens |
| Session token | `tys_<id>.<secret>` | one agent session (T3 writes it to `TOOLYARD_TOKEN_FILE`) | `/v1/code/*`, `GET /v1/agents/approvals/{id}`, `GET /v1/agents/whoami`, `DELETE /v1/agents/session-tokens/{id}` (its own id) |
| Operator token | `tyop_<id>.<secret>` | a person's automation | never on `/v1/code/*` or `/v1/agents/session-tokens*`: **403** |

All use `Authorization: Bearer <token>`.

### Session tokens

`POST /v1/agents/session-tokens` (agent token only; a session token cannot
mint another one)

```json
{ "session_id": "t3-thread-123", "ttl_seconds": 3600 }
```

- `session_id`: 1–128 printable ASCII characters, required. It becomes the
  verified client session id in the audit of every call made with the token.
- `ttl_seconds`: optional, 60–3600, default 3600. The server never issues a
  longer lifetime.

`201 Created`

```json
{
  "token": "tys_st_9f2c….<secret>",
  "token_id": "st_9f2c…",
  "agent_id": "ag_…",
  "session_id": "t3-thread-123",
  "expires_at": 1792200000000
}
```

`DELETE /v1/agents/session-tokens/{token_id}`: the parent agent token, or the
session token itself. `204`, idempotent (an unknown or already revoked id of
the same agent is also `204`; another agent's id is `404`).

A session token is refused (`401 invalid session token`) when it is expired,
revoked, its agent is revoked or blocked, or the parent credential it was
minted with is no longer valid (rotated past its grace period, or revoked).
Only the token's SHA-256 is stored.

## Runs

### `POST /v1/code/run`

Body, one of:

- `Content-Type: text/x-starlark`: the raw script. The reason comes from the
  `X-Toolyard-Reason` header (20–2000 characters, required).
- `Content-Type: application/json`:
  `{"code": "<script>", "reason": "<20–2000 chars>", "wait_seconds": 25}`.

The script is at most 1 MiB. A script that contains an Inbox grant token
(`tyg_…`) is `400`: scripts are stored, so resume the held run with its
`request_id` instead of writing a token into it.

Optional headers:

- `Idempotency-Key`: 1–128 printable ASCII characters, scoped to the agent.
  The same key with the same script and reason returns the existing run in its
  current state and runs nothing new. The same key with a different script or
  reason is `422 idempotency_key_reused`.
- `Prefer: wait=<seconds>` (0–60, default 25; `wait_seconds` in a JSON body
  wins). The server answers when the run reaches a final or held status, or
  when the wait ends, whichever is first. A larger value counts as 60; a
  value that is not a whole number of seconds is ignored.

The run continues on the server after the response; the client never has to
hold the connection.

### Run view

Every run route answers this JSON:

```json
{
  "run_id": "cr_…",
  "status": "succeeded",
  "result": {"any": "json"},
  "error": null,
  "logs": ["print output"],
  "calls": [
    {"seq": 1, "tool": "context7.resolve-library-id", "status": "succeeded", "replayed": false,
     "started_at": 1792200000000, "finished_at": 1792200000800}
  ],
  "held": null,
  "created_at": 1792200000000,
  "updated_at": 1792200000900,
  "links": {"self": "/v1/code/runs/cr_…", "resume": "/v1/code/runs/cr_…/resume"}
}
```

- `status`: `queued`, `running`, `succeeded`, `failed`,
  `permission_required`, `deferred`, `outcome_unknown`.
- `error`: `{"kind": "...", "message": "..."}` when `failed` or
  `outcome_unknown`. Kinds include `script_error`, `tool_error`,
  `timeout`, `limit`, `script_diverged`, `grant_invalid`, `busy`,
  `outcome_unknown`, `internal`, and `interrupted` (the server stopped:
  a `queued` run is `failed` with it, a `running` one `outcome_unknown`).
- `calls`: the calls of the latest execution (a resume runs the script
  again), in order.
- `calls[].status`: `succeeded`, `failed`, `permission_required`,
  `deferred`, `outcome_unknown`. `replayed: true` marks a call answered from
  the call record during a resume, not sent upstream again.
- `held`: set when `status` is `permission_required` or `deferred`:
  `{"seq": 3, "tool": "github.merge_pull_request", "status": "permission_required", "draft": {…}, "approval_id": null, "message": "…"}`.
  `draft` is the Inbox request draft the gateway returned; `approval_id` is
  set for a legacy deferred approval; `message` is the gateway's text for
  the call (what to do next).
- `links.resume` is present only while the run is held.

### HTTP status per run status

| Run status | HTTP |
|---|---|
| `succeeded`, `failed` | 200 |
| `permission_required` | 403 |
| `deferred`, `queued`, `running` (the wait ended first) | 202, with `Location: /v1/code/runs/{id}` |
| `outcome_unknown` | 409 |

Other answers: `400` bad input, `401` no or bad token, `403` operator token or a
session token outside its routes, `404` unknown run (or another agent's run),
`413` script over 1 MiB, `415` other content types, `422`
`idempotency_key_reused` or a resume of a run that is not held (`run_not_held`,
with the run's `status`), `429` code mode busy (with `Retry-After`; no run is
created), `503` the server is restarting or the token could not be checked
(with `Retry-After`; no run is created).

### `GET /v1/code/runs/{id}`

The run view. `Prefer: wait=<seconds>` (0–60, default 0) waits for a final
or held status. Only the agent that created the run sees it (with its agent
token or any of its session tokens).

### `POST /v1/code/runs/{id}/resume`

Resumes a run whose status is `permission_required` or `deferred`, after the
owner decided.

```json
{ "request_id": "rq_…" }
```

- `request_id`: the approved Inbox request that covers the held call. The
  server reads its grants for this agent and adds `_grant` to each matching
  call (same tool, parameters within the grant).
- For a `deferred` legacy approval, no body is needed (an empty body or
  `{}`): the held call resumes with its `approval_id`.
- A `permission_required` run needs `request_id` (`400` without it), and the
  request must belong to the agent (`400` otherwise). A call no unused grant
  of the request covers is sent without one, so it is held again (`403`).

The script runs again from the top. Each call before the held one is answered
from the call record when its tool and arguments are the same as recorded
(`replayed: true`); a different call is `failed` with `script_diverged` and
nothing more runs. A recorded call with an unknown outcome stops the resume
with `outcome_unknown`; nothing is repeated blindly. Inbox status reads
(`toolyard.inbox_status`, `toolyard.inbox_wait`) are sent again instead of
replayed, because their answers can carry grant tokens. The same
`Prefer: wait` and HTTP mapping apply. A recorded answer that held a grant
token is stored redacted, so it cannot be replayed with the same value: the
resume stops with `script_diverged` instead.

Grant tokens (`tyg_…`) never rest in plain text: the call record, the
result, the logs and the held call are stored with each token replaced by
`tyg_[redacted]`. A grant is matched to a call by tool and parameters, the
way the gateway checks it; a call no unused grant covers is sent without
one, so it is held again and the run stays resumable.

## Stubs

- `GET /v1/code/files`: the same listing as `listToolFiles` (text/plain).
- `GET /v1/code/files/{server}/{tool}.pyi`: the same stub as `readToolFile`.
- `GET /v1/code/docs/{server}/{tool}`: the same text as `getToolDocs`.

These show only the tools the agent may see. Unknown files are `404`.

## Discovery (no token needed)

- `GET /v1/code/openapi.json`: this API as OpenAPI 3.1.
- `GET /v1/code/ty`: the `ty` command (Python 3, standard library only).

## The `ty` command

```text
ty run FILE|-  [--wait N] [--key IDEMPOTENCY_KEY] [--reason TEXT]
ty status RUN_ID [--wait N]
ty resume RUN_ID [--request rq_…] [--wait N]
ty files
ty stub SERVER/TOOL
ty call SERVER.TOOL '{"json": "arguments"}' [--reason TEXT]
```

- `TOOLYARD_URL` (default `https://toolyard.beknown.live`) and
  `TOOLYARD_TOKEN_FILE` (required). The token never appears in argv, the
  environment of child processes, or the output.
- `run` sends an `Idempotency-Key` (the given one, or a random one it prints),
  so a retry after a lost answer returns the same run.
- Output: the run view as JSON on stdout. Exit codes: 0 `succeeded`, 1
  `failed`, 2 usage or transport error, 3 `permission_required`, 4 `deferred`
  or still running, 5 busy, 6 `outcome_unknown`.

## Limits

The MCP code-mode limits apply: 5 minutes wall clock, 100 tool calls, 1 MiB
output per run, 4 concurrent scripts per server. Runs and their call records
are kept 7 days from their start (an hourly sweep deletes older ones). A run
that is `running` when the server stops is `outcome_unknown` after the
restart; one still `queued` is `failed` (nothing ran). Both carry the error
kind `interrupted`.

## Storage

`code_runs` holds one row per run: the agent, the verified client session
(a session token's), the `Idempotency-Key` (unique per agent) with the
fingerprint of the script and reason it was used with, the script (so a
resume can run it again), and the outcome. `code_run_calls` holds one row
per call: the tool, a SHA-256 of the canonical JSON of the script's own
arguments (never a grant token), the call status, and the value the script
received, which a resume hands back instead of sending the call again.
