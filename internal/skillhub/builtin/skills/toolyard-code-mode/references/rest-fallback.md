# REST fallback: code mode over plain HTTPS

Use this only when your MCP connection to toolyard is broken: the server will
not connect, calls time out, or your client lost the toolyard tools. Go back
to MCP as soon as it works again. `inbox.guide` topic `fallback` has the same
procedure.

It is the same code mode as `executeToolCode`: the same agent identity, the
same owner policy, the same Inbox approvals. It is never a way around a
restricted tool or a denial.

## What you need

- `TOOLYARD_URL` (required): the base URL of your toolyard, the address
  your MCP client uses without `/mcp`, for example
  `https://<your-toolyard>`. Set it yourself before you fetch or run `ty`.
  The `ty` that your toolyard serves defaults to that toolyard; a copy from
  anywhere else has no default and refuses to run without `TOOLYARD_URL`.
  Never point it at a host your owner did not give you: `ty` sends your
  token there. `ty` refuses `http://` unless `TOOLYARD_URL_INSECURE=1`.
- `TOOLYARD_TOKEN_FILE`: a file that holds your toolyard token, readable only
  by you (`chmod 600`; `ty` refuses a file others can read unless
  `TOOLYARD_TOKEN_FILE_INSECURE=1`). Use an agent token (`ag_…`) or a session
  token (`tys_…`) that a host may write there for your session. Operator
  tokens (`tyop_…`) are refused (403).

Never print, `echo` or `cat` the token. Never put it in a command line, a
script, a log, a commit or an Inbox item.

## Get and use `ty`

`ty` is one Python 3 file (standard library only). Fetching it needs no token:

```sh
curl -fsS "$TOOLYARD_URL/v1/code/ty" -o ty && chmod +x ty
./ty files                       # like listToolFiles (--language javascript for .d.ts)
./ty stub github/create_issue    # like readToolFile
./ty search "create issue"       # like tools.search
./ty run pick.star --reason "List the open pull requests to pick the next one to review"
./ty call github.list_pull_requests '{"owner": "acme", "repo": "api", "state": "open"}' --reason "List open pull requests to pick one to review"
./ty status cr_… --wait 120
./ty resume cr_… --request rq_… --wait 120
```

- A file ending in `.js` or `.mjs` runs as JavaScript unless `--language`
  says otherwise.
- `--reason` (20 to 2000 bytes) goes to the audit record, like
  `_reason`. Without it `ty` writes one from the file name and the first
  line that is not a comment.
- `ty` prints the run view as JSON on stdout. Exit codes:

| Exit | HTTP | Run status | What to do |
|---|---|---|---|
| 0 | 200 | `succeeded` | Read `result`, `logs` and `calls`. |
| 1 | 200 | `failed` | Read `error.kind` and `error.message`; fix the script. |
| 3 | 403 | `permission_required` | A call needs your owner's approval. See below. |
| 4 | 202 | `queued`, `running`, `deferred` | Still running, or a legacy approval waits: `./ty status cr_… --wait 120`. |
| 5 | 429 | none, or `failed` with kind `busy` | Busy, and nothing ran. Wait `Retry-After` seconds, then run again. |
| 6 | 409 | `outcome_unknown` | A call may have committed. Check the target; never repeat it blindly. |
| 2 | other | none | Usage or transport error (401: missing, expired or revoked token; 404: unknown run). |

## Idempotency and retries

Every `ty run` and `ty call` sends an `Idempotency-Key`. When you pass no
`--key`, `ty` makes one and prints it on stderr. After a timeout or a lost
answer, run the same command with `--key <that key>`: the same key with the
same script and reason returns the existing run and runs nothing new. The
same key with another script or reason is `422 idempotency_key_reused`.

The run continues on the server after the answer, so you never hold the
connection. Runs and their call records are kept 7 days.

## When a call needs permission (exit 3, HTTP 403)

Nothing after the held call ran. `held` names it (`seq`, `tool`, `status`),
`held.draft` is the Inbox request draft, and `held.message` is the next step.

1. Ask as the `toolyard-permissions-and-inbox` skill says, from code:

   ```sh
   ./ty call toolyard.inbox_request - < request.json   # "dry_run": true first, then for real
   ./ty call toolyard.inbox_status '{"ids": ["rq_…"]}'
   ```

2. When the owner has decided, resume with the request id:
   `./ty resume cr_… --request rq_… --wait 120`.

The server adds that request's grants to the matching calls (same tool,
parameters within the grant) and runs the script again from the top:

- A recorded call that succeeded is answered from the record
  (`replayed: true`), never sent again.
- A recorded call that failed after it reached the gateway gives the script
  the same failure again, never a second attempt.
- A held call you caught gets the same hold again, unless a grant now covers
  it.
- A recorded call with an unknown outcome stops the resume with
  `outcome_unknown`.
- A different tool or different arguments at a recorded number, or a missing
  call, fails the run with `script_diverged`, and nothing on the new path is
  sent. So never change the script between the hold and the resume.
- `toolyard.inbox_status` and `toolyard.inbox_wait` are sent again instead of
  replayed, because their answers can carry grant tokens.
- A call no unused grant covers is sent without one, so it is held again.

A `deferred` run (a legacy approval, `held.approval_id` set) resumes with
`./ty resume cr_…` and no `--request` once the owner has decided. Two
resumes at once: the second answers `409 resume_conflict`; read the run and
resume again if it is still held.

## Without `ty`

Read the header from the file, so the token never appears in argv:

```sh
auth() { printf 'Authorization: Bearer '; tr -d ' \r\n\t' < "$TOOLYARD_TOKEN_FILE"; echo; }

curl -sS "$TOOLYARD_URL/v1/code/run" -H @<(auth) \
  -H 'Content-Type: text/x-starlark' \
  -H 'X-Toolyard-Reason: List the open pull requests to pick the next one to review' \
  -H 'Idempotency-Key: review-pick-1' \
  --data-binary @pick.star
curl -sS "$TOOLYARD_URL/v1/code/runs/cr_…" -H @<(auth) -H 'Prefer: wait=60'
curl -sS "$TOOLYARD_URL/v1/code/runs/cr_…/resume" -H @<(auth) \
  -H 'Content-Type: application/json' -d '{"request_id": "rq_…"}'
```

Never write `-H "Authorization: Bearer $TOKEN"` or `-u`: that puts the token
in argv.

## Routes

| Route | What it does |
|---|---|
| `POST /v1/code/run` | Start a run. Body: raw Starlark (`text/x-starlark`) or JavaScript (`text/javascript`) with the `X-Toolyard-Reason` header, or JSON `{"code", "reason", "language", "wait_seconds"}`. At most 1 MiB. A script that contains a grant token (`tyg_…`) is 400. |
| `GET /v1/code/runs/{id}` | The run view. `Prefer: wait=<0-60>` waits for a final or held status. Only the agent that made the run sees it. |
| `POST /v1/code/runs/{id}/resume` | Resume a held run: `{"request_id": "rq_…"}`, or no body for a legacy deferred approval. |
| `GET /v1/code/files`, `/v1/code/files/{server}/{tool}.pyi`, `.d.ts`, `/v1/code/files/index.d.ts` | The stubs (`?language=javascript` lists the TypeScript ones). |
| `GET /v1/code/docs/{server}/{tool}` | The same text as `getToolDocs`. |
| `GET /v1/code/search?q=…&mode=&detail=&limit=&upstream=` | The same JSON as `tools.search`. |
| `GET /v1/code/openapi.json` | This API as OpenAPI 3.1. No token needed. |
| `GET /v1/code/ty` | The `ty` command. No token needed. |

`Prefer: wait=<seconds>` (0 to 60, default 25 on `POST /v1/code/run`): the
server answers when the run is final or held, or when the wait ends. A run
past the script slots is stored `queued` at once, so the request still
answers within its wait.

## The run view

`run_id` (`cr_…`), `language`, `status`, `result`, `error`
(`{"kind", "message"}`), `logs`, `calls` (the ledger: `seq`, `tool`,
`status`, `replayed`, `ms`), `held`, `created_at`, `updated_at`, and `links`
(`self`, and `resume` only while the run is held).

| Run status | HTTP |
|---|---|
| `succeeded`, `failed` | 200 |
| `permission_required` | 403 |
| `deferred`, `queued`, `running` (the wait ended first) | 202 with `Location` |
| `failed` with kind `busy` (no slot came free; nothing ran) | 429 with `Retry-After` |
| `outcome_unknown` | 409 |

Other answers: 400 bad input, 401 no or bad token, 403 operator token, 404
unknown run, 413 script over 1 MiB, 415 other content types, 422
`idempotency_key_reused` or `run_not_held`, 429 the slot queue is full (no
run created), 503 restarting (with `Retry-After`; no run created).

## Stored redacted

A REST run stores its call records, result, logs and held call with grant
tokens, webhook secrets and skills bundle tickets replaced
(`tyg_[redacted]`, `whsec_[redacted]`, `tyskb_[redacted]`). A resume that
meets a recorded answer that held a grant token stops with
`script_diverged`. Do grant redemption with `resume`, not by reading tokens
in the script.
