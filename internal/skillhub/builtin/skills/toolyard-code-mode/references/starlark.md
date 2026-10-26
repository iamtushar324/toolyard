# Starlark in code mode

Starlark is the default language of `executeToolCode` (`language` omitted,
`"starlark"` or `"python"`). Its output matches the Bifrost gateway's code
mode byte for byte, so skills that parse that output keep working.

## Shape of a script

```python
# Count open bugs per label (this first comment becomes the nested calls' reason)
issues = github.list_issues(owner="acme", repo="api", state="open")
counts = {}
for i in issues:
    for l in i.get("labels", []):
        counts[l["name"]] = counts.get(l["name"], 0) + 1
print("issues seen:", len(issues))
result = counts
```

- Servers are global objects: `result = server.tool(param="value")`.
  A single dict also works: `server.tool({"param": "value"})`.
- Calls are synchronous. Keyword arguments only.
- Assign the value you want back to `result`. `print()` writes log lines.
- Each run starts in a fresh, isolated scope: nothing carries over between
  runs.
- Top-level `for`, `if` and `while`, `set`, recursion and reassigning a
  global are allowed.

## Differences from Python

- No `try`, `except`, `finally` or `raise`. A failed or held call ends the
  script. Use `try_call` when a failure is expected.
- No classes, no imports (`load()` too), no network, no filesystem.
- No `is`. No f-strings: use `%` formatting (`"%s: %d" % (name, n)`).
- Dict access uses brackets or `.get()`: `r["key"]`, `r.get("key", default)`.

## try_call

```python
r = try_call(github.create_issue, owner="acme", repo="api", title="Follow up")
r = try_call("github.create_issue", {"owner": "acme", "repo": "api", "title": "Follow up"})
if not r["ok"]:
    print(r["status"], r["error"])   # e.g. permission_required, with the gateway's text
```

It answers `{"ok": True, "status": "succeeded", "result": …}` or
`{"ok": False, "status": …, "error": …}`, with `"approval_id"` added for a
deferred call. `status` is `failed`, `permission_required`, `deferred` or
`outcome_unknown`.

## parallel

```python
many = parallel([
    (github.get_issue, {"owner": "acme", "repo": "api", "number": 1}),
    ("github.get_issue", {"owner": "acme", "repo": "api", "number": 2}),
], limit=2)
result = [m["result"]["title"] for m in many if m["ok"]]
```

- Each item is a tool, `(tool, {args})` or `[tool, {args}]`.
- At most `limit` calls run at once: 4 by default, 8 at most.
- It returns one `try_call` result per item, in the order given. One failed
  item does not end the script.
- A server named `try_call` or `parallel` keeps its name; the helper is then
  not bound.

## Results

A tool's value is its `structuredContent` when it sends one, else its text
parsed as JSON, else the text as a string. Print a sample first when you do
not know the shape: `print(r)`.

## Searching from a script

```python
hits = toolyard.tools_search(query="create issue", limit=3)
print([h["code"] for h in hits["results"]])
```

Inside a script `detail` defaults to `summary` (name, upstream, description,
`code`, score). Read the stub for the schema.

## Limits that matter here

- 50,000,000 execution steps: a tight loop stops long before the clock.
- One call's arguments: at most 4 MiB as JSON.
- One tool result: at most 6 MiB (a larger one is a failed call, kind `limit`).
- The returned `result`: at most 1 MiB as JSON. Return less, or print a
  summary and return the key fields.
- One `print()` line: at most 1 MiB. All output: 1 MiB.

## Errors

- A syntax error answers `Execution syntax error` with the position.
- A runtime error answers with a traceback that names the line.
- An unknown name lists the available server keys in the `Environment`
  block. Read the stubs again instead of guessing.
