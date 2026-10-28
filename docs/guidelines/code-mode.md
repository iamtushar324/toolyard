# Code mode

Code mode lets an agent write a small program that calls many tools in one
round trip, instead of one MCP call per tool. Every call the program makes
goes through toolyard's normal pipeline (access, policy, approval, identity
forwarding, audit, metrics) with via `code_mode`, exactly as if the agent had
called the tool itself.

The four tools are Bifrost's, so agents and skills written for the Bifrost
gateway work unchanged:

| Tool | What it does |
|---|---|
| `listToolFiles` | The stub files of the tools you may use (`language: "javascript"` for TypeScript stubs). |
| `readToolFile` | One stub: `servers/<Server>/<tool>.pyi`, or `.d.ts` for JavaScript. |
| `getToolDocs` | One tool's full documentation (Python flavoured). |
| `executeToolCode` | Runs the program: `code`, and optionally `language` and `echo`. |

`tools.search` finds tools by what they do (see [Finding tools](#finding-tools)).

By default an agent's tool list shows five tools: `tools.search`,
`getToolDocs`, `executeToolCode`, `inbox.request` and `inbox.status` (see
[the tool list](access-tools.md#the-tool-list-five-tools-by-default)).
`listToolFiles` and `readToolFile` are not among them, but they still answer
a direct call, and an agent its owner switched to the full list sees them.

## The workflow

1. Find the tool: `tools.search` with a few words about the task. Each
   result's `code` is the name to call it by, and its `run` says whether a
   call runs now (`now`), needs your owner's yes (`needs_approval`) or is
   denied by policy (`blocked`).
2. Read its parameters: `getToolDocs` with the two halves of `code`
   (`server` before the dot, `tool` after it), or its stub with
   `readToolFile` (the `.d.ts` for JavaScript, the `.pyi` for Starlark).
   Never guess a name or a parameter.
3. Run the program with `executeToolCode`.
4. Ask for permission once per round. Call restricted tools with
   `tools.tryCall` (JavaScript) or `try_call` (Starlark), which answer
   `permission_required` instead of stopping the program. Put every such
   call, and every question you have for your owner, into one
   `inbox.request` (`tools[]` and `questions[]`), read the decision with
   `inbox.status`, and run the program again with each grant as the call's
   `_grant` argument and exactly the approved parameters. Ask again only for
   what the answers open.

## JavaScript

Pass `language: "javascript"`. The engine is QuickJS-NG (ES2023: `async`
and `await`, arrow functions, classes, destructuring, `Map`, `Set`,
template strings), compiled to WebAssembly and run inside the same sandboxed
worker as Starlark. The program is the **body of an async function**: `await`
tool calls, and `return` the value you want back.

```js
// Find, read, run: search the catalog, then call what it found.
const hits = await tools.search("create a linear issue", { limit: 3 });
console.log(hits.map((h) => h.code));          // ["Linear.save_issue", ...]

const issue = await tools.Linear.get_issue({ id: "TEC-1" });
const team = await tools.call("Linear.get_team", { id: issue.teamId }); // by name

// A failed or held call rejects with a ToolCallError; tryCall never does.
const r = await tools.tryCall("Linear.save_issue", { title: "Follow up" });
if (!r.ok) console.log(r.status, r.error);      // e.g. permission_required

// Several calls at once, at most `limit` in flight (4 by default, 8 at most).
const ids = ["TEC-1", "TEC-2", "TEC-3"];
const all = await tools.parallel(ids.map((id) => () => tools.Linear.get_issue({ id })), { limit: 3 });

return { team: team.name, titles: all.map((i) => i.title) };
```

| API | Answer |
|---|---|
| `tools.<Server>.<tool>(args)` | The result. Rejects with a `ToolCallError` (`status`, `callId`, `approvalId`) when the call fails or is held. |
| `tools.call("Server.tool", args)` | The same, by name. Any form `readToolFile` accepts works on either side of the dot. |
| `tools.tryCall(nameOrFunction, args)` | `{ok: true, status: "succeeded", result}` or `{ok: false, status, error, approval_id?}`. Never rejects. |
| `tools.parallel(fns, {limit})` | The results in order, like `Promise.all`: rejects on the first failure. Use `tryCall` inside the functions to collect every outcome. |
| `tools.search(query, opts)` | The matching tools only, the answer's `results` (`name`, `upstream`, `description`, `code`, `score`, `run`). `tools.call("toolyard.tools_search", { query })` returns the whole answer, with `ranked_by` and `fallback`. See below. |
| `console.log` (`info`, `warn`, `error`, `debug`) | One print line; objects are printed as JSON. |

A result is the tool's `structuredContent` when it sends one, else its text
parsed as JSON, else the text. Arguments must be a JSON object.

Not available: `setTimeout`, `fetch`, files, modules from disk and the
environment. A promise that nothing settles ends the program with an error.
An unhandled rejection ends it too, as in Node.js. A thrown error is
reported with its stack, using the program's own line numbers. Unbounded
recursion is a `RangeError` you can catch. Recursion inside the engine's own
code, such as `JSON.parse` of data nested many thousands of levels deep,
can instead reach the WebAssembly call-frame limit below QuickJS's own
check: the engine then stops at once with an error that no `try`/`catch`
sees ("nested deeper than the JavaScript engine's call stack allows", kind
`limit`). Flatten such data, or use a loop instead of recursion.

The engine is QuickJS's `qjs` command. Its host modules `qjs:std`, `qjs:os`
and `qjs:bjson` are not globals, but `await import("qjs:os")` still loads
them; do not use them. Inside WebAssembly they reach nothing outside the
module: no file is mounted (`std.open` returns `null`), the environment is
empty, there is no network and no `os.exec`. `os.sleep` and `os.setTimeout`
work and count against the program's clock. Writing to `std.out` writes into
toolyard's own channel to the engine and breaks only your program.

The TypeScript stubs (`listToolFiles` with `language: "javascript"`):

- `servers/<Server>/<tool>.d.ts`: the tool's `Input` and `Output` types, made
  from its input and output JSON schemas (nested objects, arrays, enums as
  literal unions, required and optional members, descriptions as JSDoc), and
  its `Call` signature.
- `servers/<Server>.d.ts`: every tool of one server.
- `servers/index.d.ts`: the `tools` object, its helpers and `console`.

## Starlark

Starlark is the default (`language` omitted or `"starlark"`), and its output is
Bifrost's, byte for byte, so skills that parse it keep working.

```python
issue = Linear.get_issue(id="TEC-1")              # a failed or held call ends the script

held = try_call(Linear.save_issue, title="Follow up")
if not held["ok"]:
    print(held["status"], held["error"])          # the script carries on

many = parallel([
    (Linear.get_issue, {"id": "TEC-1"}),
    ("Linear.get_issue", {"id": "TEC-2"}),
], limit=2)                                        # try_call results, in order
result = {"titles": [m["result"]["title"] for m in many if m["ok"]]}
```

- `try_call(tool, param=value)` or `try_call("Server.tool", {...})` answers
  `{"ok", "status", "result" | "error"}` instead of ending the script.
- `parallel(calls, limit=4)` makes the calls (each a tool, `(tool, {args})` or
  `[tool, {args}]`) at most `limit` at a time (8 at most) and returns their
  `try_call` results in the order given.
- A server named `try_call` or `parallel` keeps its name; the helper then is
  not bound.

## Output: echo and the call ledger

`executeToolCode`'s text repeats each tool's answer as a `[TOOL] <tool> raw
response: …` line by default (`echo: "full"`, Bifrost's behaviour). Large
answers then crowd the output, so:

- `echo: "preview"` shows the first 500 bytes of each answer.
- `echo: "none"` shows no answers, only failures (cut to 2 KiB).

Tool answers may use three quarters of the output limit (1 MiB). Past that,
one line says the echo stopped, so the program's own prints and its result
always fit.

Every result carries the **call ledger**: each call with its status and
duration in milliseconds. It is in the result's `_meta` (`toolyard.calls`,
with `toolyard.run_id`) for every run, and in the text's `Environment`
block for JavaScript and for `echo` `preview` or `none`. A Starlark run with
`echo: "full"` keeps Bifrost's exact text. `_meta` also carries the run's
`toolyard.status` (`succeeded`, `failed`, `permission_required`,
`deferred` or `outcome_unknown`) and, when it did not succeed,
`toolyard.error_kind`; an `outcome_unknown` run (a call was cut short, or
the run was interrupted after a call was sent) also sets
`toolyard.outcome_unknown: true` and has structured content
`{"status": "outcome_unknown", "error_kind": …, "run_id": …}`. A run that toolyard stops because the
server shuts down, or because the client went away, is kind `interrupted`:
`outcome_unknown` once it had sent a tool call, `failed` (nothing ran)
before. The script's own clock is kind `timeout`.

The run id (`cm_…` for `executeToolCode`, `cr_…` for a REST run) is also on
every audit row of the run: the outer call's and every nested call's
(`audit_events.run_id`).

## Finding tools

`tools.search` (also `GET /v1/code/search`, `tools.search()` in JavaScript and
`toolyard.tools_search(...)` in Starlark) searches only the tools you may use:

| Argument | Values |
|---|---|
| `query` | The task in plain words (auto, semantic, keyword) or a pattern (regex). Empty lists every tool by name. |
| `mode` | `auto` (default): ranked by meaning with Jev, a classification model, where the operator turned the ranker on (see [Tool search ranking](../self-hosting.md#tool-search-ranking)), else by keyword; it also falls back to keyword when Jev is slow or fails. `semantic`: Jev with the same fallback, refused where the ranker is off. `keyword`: BM25 ranking over tool names (split at dots, underscores, dashes and camelCase), descriptions, parameter names and parameter descriptions, with the name weighted most. `regex`: an RE2 pattern of at most 200 characters, case-insensitive unless the pattern says `(?-i)`. `keyword` and `regex` never ask Jev. |
| `detail` | `summary`: `name`, `upstream`, `description`, `code` (the name to call it by), `score`, `run`. `name`: the name only (no `run`). `full`: also `input_schema` and `output_schema`. The default is `full` for an agent's own call of the MCP tool `tools.search` (its answer has always carried the input schemas) and `summary` inside a script (JavaScript and Starlark, also through `tools.execute`) and for `GET /v1/code/search`. |
| `limit` | 50 by default, 200 at most. |
| `upstream` | Only one server's tools. |

The answer has `total`, `matched` (before the limit), `mode` (the mode that
ran: `semantic` when Jev ranked, else `keyword` or `regex`), `ranked_by`
(`jev`, `keyword`, `regex`, or `name` for an empty query), `fallback` and
`results`. `fallback` says why an `auto` or `semantic` search ranked by
keyword: `off` (no ranker), `no_key`, `timeout`, `error`, `empty` (you have
no tools) or `busy`; it is `""` when Jev ranked, and for `keyword`, `regex`
and an empty query. With `ranked_by: "jev"` a result's `score` is Jev's
probability (0 to 1) that the tool fits; tools Jev did not judge follow,
without a score. Each result's `run` is what a call with no arguments gets
now: `now`, `needs_approval` (ask with `inbox.request`) or `blocked`. A call
whose arguments escalate it may still need approval where it says `now`.

In JavaScript `tools.search` returns only `results`; the whole answer is
`tools.call("toolyard.tools_search", { query, ... })`. Starlark's
`toolyard.tools_search(query="...")` returns the whole answer.

A keyword query is at most 1,000 characters, and only its first 32
distinct words count. A regex may repeat something at most `{100}` times and
compile to at most 5,000 instructions. It is matched against the first
4 KiB of each text (a name, a description, a parameter's), a pattern whose
instructions times the bytes of text to scan pass a fixed budget is
refused before it runs (make it simpler, or use `keyword`), and one regex
search stops after 250 ms. One agent runs at most 2 regex searches at
once; a third waits up to 2 seconds for one of them to finish, then is
refused. Keyword searches are not capped. An `auto` or `semantic` search
that would ask Jev takes one of the same 2 slots but never waits: with both
taken it ranks by keyword at once and says `fallback: "busy"`.

## Limits

Per run: 5 minutes of wall clock, 100 tool calls, 1 MiB of output, a 256 MiB
worker (a JavaScript program's heap is 88 MiB of it; going past it is a
catchable `out of memory` error), a 1 MiB returned value and 6 MiB per tool
result. At most 8 calls of one program are in flight at once.

At most 4 programs run at once on one gateway (`-codemode-max-concurrent`),
and at most 3 for one agent (`-codemode-max-per-agent`). A program past
either cap waits in one fair queue, for up to 30 seconds
(`-codemode-queue-wait`). A freed slot goes to the
earliest waiter whose agent is under its own cap, so one agent's backlog
never holds another agent up. After the wait the program is refused as
busy: `executeToolCode` says so, and a run of `POST /v1/code/run` (stored
`queued` at once, so the request answers within its own wait) ends
`failed` with kind `busy`, answered 429 with `Retry-After`. The queue is
bounded: one agent may have twice its own cap
waiting, and all agents together four times the global cap (16 at least);
a program past that is refused as busy at once.
