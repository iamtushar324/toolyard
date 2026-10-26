# JavaScript in code mode

Pass `"language": "javascript"` to `executeToolCode` (the schema lists only
`starlark` and `javascript`; the server also takes `js`, but send the listed
value). The engine
is QuickJS-NG (ES2023: `async`/`await`, arrow functions, classes,
destructuring, `Map`, `Set`, template strings), run in the same sandboxed
worker as Starlark.

The program is the **body of an async function**: `await` tool calls and
`return` the value you want back.

```js
// Read three issues and return their titles
const hits = await tools.search("get issue", { limit: 3 });
console.log(hits.map((h) => h.code));

const first = await tools.github.get_issue({ owner: "acme", repo: "api", number: 1 });

const r = await tools.tryCall("github.create_issue", { owner: "acme", repo: "api", title: "Follow up" });
if (!r.ok) console.log(r.status, r.error);      // e.g. permission_required

const nums = [1, 2, 3];
const all = await tools.parallel(
  nums.map((n) => () => tools.github.get_issue({ owner: "acme", repo: "api", number: n })),
  { limit: 3 },
);
return { first: first.title, titles: all.map((i) => i.title) };
```

## API

| API | Answer |
|---|---|
| `tools.<Server>.<tool>(args)` | The result. Rejects with a `ToolCallError` (`status`, `callId`, and `approvalId` for a deferred call) when the call fails or is held. |
| `tools.call("Server.tool", args)` | The same, by name. Any form `readToolFile` accepts works on either side of the dot. |
| `tools.tryCall(nameOrFunction, args)` | `{ok: true, status: "succeeded", result}` or `{ok: false, status, error, approval_id?}`. Never rejects. |
| `tools.parallel(fns, {limit})` | The results in order, like `Promise.all`: rejects on the first failure. Default limit 4, at most 8. Use `tryCall` inside the functions to collect every outcome. |
| `tools.search(query, opts)` | The matching tools (`name`, `upstream`, `description`, `code`, `score`). `opts` takes `mode`, `detail`, `limit`, `upstream`. |
| `console.log` (`info`, `warn`, `error`, `debug`), `print` | One output line; objects print as JSON. |

- Arguments must be a JSON object (`{}` when the tool takes none), at most
  4 MiB as JSON.
- A result is the tool's `structuredContent` when it sends one, else its
  text parsed as JSON, else the text.
- toolyard's own tools are under `tools.toolyard`:
  `await tools.toolyard.inbox_check({...})`,
  `await tools.toolyard.skills_search({ query: "..." })`.
- The helpers win over a server with the same name (`call`, `tryCall`,
  `parallel`, `search`); that server stays callable by name:
  `tools.call("call.tool", {...})`.
- Calls you start before the next `await` go out together, so un-awaited
  calls and `tools.parallel` really run in parallel (8 in flight at most).

## Stubs

`listToolFiles` with `{"language": "javascript"}` lists:

- `servers/<Server>/<tool>.d.ts`: the tool's `Input` and `Output` types
  (from its JSON schemas, descriptions as JSDoc) and its call signature.
- `servers/<Server>.d.ts`: every tool of one server.
- `servers/index.d.ts`: the `tools` object, its helpers, `TryResult`,
  `SearchOptions` and `console`.

## Not available

- `setTimeout`, `fetch`, files, modules from disk, the environment.
- The host modules (`qjs:std`, `qjs:os`, `qjs:bjson`) are not globals. Do not
  import them: they reach nothing outside the sandbox, and writing to
  `std.out` breaks your own program.

## How a program ends

- A promise that nothing settles ends the program with an error.
- An unhandled rejection ends it, as in Node.js. A failed call you never
  await is not an unhandled rejection.
- A thrown error is reported with its stack, using your own line numbers.
- Unbounded recursion is a `RangeError` you can catch. Very deep recursion
  inside the engine itself (for example `JSON.parse` of data nested many
  thousands of levels) can stop the engine at once with a `limit` error that
  no `catch` sees. Flatten such data, or use a loop.
- The heap is 88 MiB of the 256 MiB worker. Going past it is a catchable
  `out of memory` error.
- The returned value must encode as JSON and be at most 1 MiB. A function or
  a symbol returns nothing.
