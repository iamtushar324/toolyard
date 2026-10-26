---
name: toolyard-code-mode
description: Run many toolyard tool calls in one round trip with executeToolCode (Starlark by default, or JavaScript) and filter large results before they reach your context. Covers finding tools and their code names with tools.search, listToolFiles, readToolFile and getToolDocs (toolyard.inbox_request in Starlark, tools.toolyard.skills_search in JavaScript); try_call, tools.tryCall and parallel; echo modes and the call ledger; limits (5 minutes, 100 calls, 1 MiB output, busy); and how policy, permission_required, deferred approvals and outcome_unknown behave inside a script. Also covers the REST fallback when MCP fails (POST /v1/code/run, the ty command, TOOLYARD_TOKEN_FILE, resume with a request_id). Use when you plan several tool calls, loop over items, need to trim a big result, hit executeToolCode errors (script_error, script_diverged, limit, busy), or lose your MCP connection to toolyard.
metadata:
  toolyard-runs: anywhere
---

# Code mode: many tool calls in one round trip

`executeToolCode` runs a small program that calls tools. One MCP call can then
make many tool calls, loop, join results, and return only the few fields you
need. Big answers stay inside the program instead of filling your context.

Code mode is not a way around anything. Every call the program makes goes
through the same access, policy, approval, identity forwarding and audit as a
direct call (recorded with via `code_mode`).

## When to use

- Several calls, a loop over items, or calls that feed each other.
- A tool answers far more than you need: filter it in code, return a summary.
- Independent reads you want at once (`parallel`, `tools.parallel`).
- Your MCP connection to toolyard is broken: run the same code over HTTPS
  (see [REST fallback](references/rest-fallback.md)).

Call a tool directly instead when you make one call, or when the call needs an
Inbox grant (`_grant`) that you already hold.

## Recipe

1. Find the tool. Search by what it does; each hit's `code` is the name to
   call it by in a script.

   ```text
   tools.search
   {"query": "list pull requests", "detail": "summary", "limit": 5, "_reason": "Find the tool that lists open pull requests so a script can rank them"}
   ```

   Or list every stub: `listToolFiles` (`{"language": "javascript"}` for
   TypeScript stubs).
2. Read the stub. Never guess a name or a parameter.

   ```text
   readToolFile
   {"fileName": "servers/github/list_pull_requests.pyi", "_reason": "Confirm the parameters before the script calls this tool"}
   ```

   `servers/<Server>.pyi` shows a whole server. For JavaScript read
   `servers/<Server>/<tool>.d.ts`, `servers/<Server>.d.ts` or
   `servers/index.d.ts`. When the stub is not enough, call `getToolDocs`
   with `server` and `tool`.
3. Write the program. Start it with a one-line comment that says why it runs.
   Filter in code and return a small value.
4. Run it. Pass `_reason`; it is optional here, but it becomes the reason of
   every nested call. Use `echo: "none"` when tool answers are large.

   ```text
   executeToolCode
   {"code": "# Pick the oldest open PR to review\nprs = github.list_pull_requests(owner=\"acme\", repo=\"api\", state=\"open\")\noldest = sorted([[p[\"created_at\"], p[\"number\"]] for p in prs])\nresult = oldest[0][1] if oldest else None", "echo": "none", "_reason": "Find the oldest open pull request to review next, returning only its number"}
   ```

5. Read the answer: the text (prints, `Return value: …`), and the result's
   `_meta`: `toolyard.status`, `toolyard.run_id` (`cm_…`, also on every audit
   row of the run), `toolyard.calls` (the call ledger) and, on failure,
   `toolyard.error_kind`.
6. If a call was held or failed, follow [Approvals inside a script](#approvals-inside-a-script).

## Starlark or JavaScript

| | Starlark (default) | JavaScript (`"language": "javascript"`) |
|---|---|---|
| Calls | `github.get_issue(number=1)`, synchronous | `await tools.github.get_issue({number: 1})` |
| Return a value | assign it to `result` | `return value` |
| Errors | no `try`/`except`: use `try_call` | `try`/`catch`, or `tools.tryCall` |
| Fan-out | `parallel([...], limit=4)` | `tools.parallel([...], {limit: 4})` |
| Stubs | `.pyi` | `.d.ts` |

Pick Starlark when a skill or a parser expects Bifrost's exact output, or for
a short linear script. Pick JavaScript when you need `try`/`catch`, regular
expressions, classes or heavy JSON reshaping. `language` also accepts
`python` (Starlark) and `js`. Details: [Starlark](references/starlark.md),
[JavaScript](references/javascript.md).

## How tool names become code names

| Catalog name | Starlark | JavaScript |
|---|---|---|
| `inbox.request` (toolyard's own) | `toolyard.inbox_request(...)` | `tools.toolyard.inbox_request({...})` |
| `skills.search` (toolyard's own) | `toolyard.skills_search(query="...")` | `tools.toolyard.skills_search({query: "..."})` |
| `jobs.wait` (toolyard's own) | `toolyard.jobs_wait(job_id=j)` | `tools.toolyard.jobs_wait({job_id: j})` |
| `github.list_pull_requests` (upstream) | `github.list_pull_requests(...)` | `tools.github.list_pull_requests({...})` |
| `reports.get-All-Clients` (upstream) | `reports.get_all_clients()` | `tools.reports.get_all_clients({})` |

- Every toolyard tool (`inbox.*`, `callbacks.*`, `jobs.*`, `memory.*`,
  `events.*`, `session.*`, `tools.*`, `policies.*`, `servers.*`, `audit.*`,
  `access.*`, `skills.*` and the like) hangs off one server, `toolyard`, as
  `toolyard.<group>_<name>`. The old form `inbox.request(...)` fails with
  `undefined: inbox`.
- An upstream keeps its server key. A key that is not an identifier gets
  `-`, `.` and spaces turned into `_`, other punctuation dropped, a `_` before
  a leading digit.
- A tool name is lowercased, `-` and spaces become `_`, other punctuation is
  dropped. A case-preserving alias is bound too when it is a valid
  identifier (`getClient` and `getclient`). A name that is a Starlark keyword
  gets a trailing `_`.
- Two names that map to the same identifier are refused, not merged. Call
  such a tool by its exact catalog name through `toolyard.tools_execute`.
- By string: `try_call("github.get_issue", {...})`, `tools.call("github.get_issue", {...})`
  accept any form `readToolFile` accepts, or the catalog name
  (`"tools.search"`).
- A script cannot call `listToolFiles`, `readToolFile`, `getToolDocs` or
  `executeToolCode`: code mode does not nest.

## Approvals inside a script

- A call that needs permission answers `permission_required` and runs
  nothing. A plain Starlark call then ends the script; in JavaScript it
  rejects with a `ToolCallError` whose `status` is `permission_required`.
  `try_call` and `tools.tryCall` return `{ok: false, status, error}` instead,
  and the script carries on. The run's `toolyard.status` says
  `permission_required`, and the error text is the gateway's next step.
- Ask exactly as the `toolyard-permissions-and-inbox` skill says
  (`skills.get toolyard/toolyard-permissions-and-inbox`). From code, those
  tools are `toolyard.inbox_check` and `toolyard.inbox_request`.
- `executeToolCode` over MCP has no resume. Once the owner accepts, make the
  granted call directly (not in a script) with its `_grant` and exactly the
  approved parameters. Then run the rest of the work again, without the
  calls that already ran.
- Need a script that resumes? Use a REST run: `POST /v1/code/runs/{id}/resume`
  with the `request_id` replays the recorded calls and adds the grants. See
  [REST fallback](references/rest-fallback.md).
- Never paste a grant token (`tyg_…`) into a script. The REST route refuses
  such a script (400), because it stores scripts.
- `deferred` (a legacy approval, or a GitHub write) carries an
  `approval_id` (`try_call` returns it as `approval_id`, a `ToolCallError` as
  `approvalId`). Collect the result with `tools.poll_approval` and never send
  the write again. GitHub details: the `toolyard-github-writes` skill.
- An owner rule on `executeToolCode` itself still applies: `deny` refuses the
  run, `ask` holds the whole run for approval.
- A wait inside a script (`toolyard.tools_wait_for_approval`,
  `toolyard.inbox_wait`, `toolyard.jobs_wait`) ends with the script's clock.
  Prefer to end the script, then wait outside it or use a callback.

## Output: echo and the call ledger

- `echo: "full"` (default) repeats each tool answer as a `[TOOL] <tool> raw response: …` line.
- `echo: "preview"` shows the first 500 bytes of each answer.
- `echo: "none"` shows no answers, only failures (cut to 2 KiB).

Tool echoes may use three quarters of the 1 MiB output; past that one line
says the echo stopped, so your prints and result still fit. With `preview` or
`none` (and always in JavaScript) the text's `Environment` block lists each
call with its status and milliseconds.

Run statuses: `succeeded`, `failed`, `permission_required`, `deferred`,
`outcome_unknown`. Error kinds include `script_error`, `tool_error`,
`timeout` (the script's own clock), `limit`, `busy`, `interrupted` (the
server stopped the run, or the client went away), `grant_invalid`,
`outcome_unknown` and `internal`; a resumed REST run can also end
`script_diverged`.

`outcome_unknown` means a call was cut short or the run was interrupted after
a call was sent: it may have taken effect. Check the target's own state
before you run anything again.

## What code mode never returns

- `skills.bundle` is refused inside a script and inside an async job: its
  one-time download link would come back redacted. Call `skills.bundle`
  directly, or use `ty skills pull`.
- The program has no network, no files, no environment and no credentials;
  it can only call tools.
- A REST run stores its calls, result, logs and held call with grant tokens,
  webhook secrets and skills bundle tickets replaced (`tyg_[redacted]`,
  `whsec_[redacted]`, `tyskb_[redacted]`). Do not read grant tokens or
  register a callback receiver (its secret is shown once) inside a REST run.

## Limits

Per run: 5 minutes, 100 tool calls, 1 MiB of output, a 1 MiB returned value,
6 MiB per tool result, at most 8 calls in flight. At most 3 scripts per agent
and 4 per gateway run at once; more wait up to 30 seconds, then are refused
as busy. Full table: [limits](references/limits.md).

## Pitfalls

- Rerunning a script that already wrote something repeats the write. Read
  the previous output and ledger, and drop calls that already ran.
- Return little. Print counts and keys, return only the fields you need.
- Tool results are the tool's `structuredContent`, else its text parsed as
  JSON, else the text. In Starlark use `r["key"]` or `r.get("key")`, not
  `r.key`.
- Top-level `for`, `if` and `while` work in toolyard's Starlark, although the
  `getToolDocs` header says to wrap loops in a function.
- A Starlark script that hits a failed call stops there; nothing after it
  runs. Use `try_call` where a failure is expected.
- `busy`: every script slot stayed taken. Wait a moment, then run again.

## Related skills

- `toolyard-basics`: `_reason`, tool discovery, sessions.
- `toolyard-permissions-and-inbox`: `permission_required`, `inbox.request`, grants.
- `toolyard-async-jobs`: slow tools that answer with a `job_id`, and `toolyard.jobs_wait` in scripts.
- `toolyard-github-writes`: deferred GitHub approvals.
- `toolyard-skills`: `skills.*` and `ty skills`.
