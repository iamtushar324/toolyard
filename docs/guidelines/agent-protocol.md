# toolyard — agent protocol

This file is written **to agents**. It's the single source for everything an
agent is told about asking its owner for permission:

- `inbox.guide` returns it (split by topic);
- `GET /v1/guide` serves it to agents that don't use MCP;
- the `toolyard-inbox` skill ([skill/toolyard-inbox/SKILL.md](skill/toolyard-inbox/SKILL.md)) is a shorter version of it.

How agents are taught this, and why it's layered, is in
[agent-onboarding.md](agent-onboarding.md).

---

## The short version

By default your tool list shows six tools: `tools.search` (find a tool;
each result carries its `input_schema`), `tools.execute` (run one call),
`executeToolCode` (run several calls, loops or filtering), `getToolDocs`
(read parameters when the schema is unclear), `inbox.request` (ask for
permission) and `inbox.status` (read the decision). Every other tool you may use still works. The other toolyard
tools named here (`inbox.guide`, `inbox.upload`, `inbox.wait`, `inbox.ask`,
`inbox.post`, `inbox.cancel`, `inbox.check`, `session.start`, …) run inside
`executeToolCode` as `toolyard.<group>_<name>`, for example
`toolyard.inbox_guide(topic="requests")`.

1. Some tools are **restricted**. Calling one without permission does
   nothing and returns `permission_required` with a draft request. Nothing
   reaches your owner until you send a proper request.
2. `inbox.request` is the one way to get permission. `tools.search` says for
   each tool whether a call runs now, needs approval or is blocked (`run`:
   `now`, `needs_approval`, `blocked`). Plan the round, then put **every**
   restricted call it needs, and every question you have for your owner,
   into **one** `inbox.request` (`tools[]`, and `questions[]` with
   `schema_version: 3`, §14). Ask again only for what the answers open,
   never one call at a time. `inbox.check` is optional (§1).
3. Write the request as yourself, in the first person: a message, a voice-note
   script, and the evidence. Your owner reads it on a phone and often listens
   first.
4. Run `inbox.request` with `dry_run: true` first. Fix every problem it lists.
5. Obtain an authorized callback reference before the request when your client supports it
   (`callbacks.register`, or your T3 session webhook; §15).
   Pass `callback_ref`, continue independent work, and retrieve authoritative `inbox.status` after the callback.
   `inbox.wait` remains optional.
6. For each allowed tool you get a **grant**. Call the tool with
   `_grant: "tyg_…"`, exactly within its parameters. In `executeToolCode`
   the grant is one more argument of the call (§9).
7. When you're done, `inbox.post` an update, also in the first person.
8. A denial is an answer. Ask again only with new information.
9. When a call answers that a server isn't connected (your owner has to
   sign in), show the person the connect link it gives you as a clickable
   Markdown link, and retry after they say they're done; `connections.status`
   lists every sign-in and `connections.link` gets a fresh link. Never ask
   them for a password, token or one-time code: the sign-in happens at the
   provider, in their browser.

---

## 1. Which tools are restricted

You can't tell from the name. `tools.search` tells you: each result's `run`
is `now` (call it), `needs_approval` (put it in your request) or `blocked`
(denied by policy: don't ask). That is the answer for a call with no
arguments. Arguments can escalate a call, so a call that search says runs
`now` may still answer `permission_required`. In a script, call tools that
may be restricted with `tools.tryCall` (JavaScript) or `try_call`
(Starlark): they return `permission_required` instead of stopping the
script, so you can collect every such call into your one request.

`inbox.check` is optional. It answers for concrete calls, with their
arguments:

```json
inbox.check({ "calls": [
  { "tool": "github.merge_pull_request", "args": { "repo": "acme/api", "pull_number": 218 } },
  { "call_id": "deploy_api",
  "tool": "deploy.run",
  "target": "Production API service",
  "operation": "write",
  "expected_effects": "The API uses the new billing handler with its flag disabled.",
  "affected_scope": "Only the production API deployment",
  "material_risks": "A wrong release can interrupt invoice requests.",
  "undo": "Redeploy the current known good release 9f1e2d0.", "args": { "service": "api", "env": "prod" } }
]})
```

Each call comes back as one of:

| status | meaning |
|---|---|
| `open` | You can call it now. |
| `granted` | A live grant already covers it. Pass `_grant`. |
| `restricted` | Include it in a request. |
| `denied` | Your owner has blocked this tool for you. Don't ask. |

## 2. When you call a restricted tool anyway

You get this back (`isError: true`, because nothing ran):

```json
{
  "status": "permission_required",
  "executed": false,
  "message": "deploy.run is restricted. Nothing ran. Ask your owner with inbox.request, starting from the draft below.",
  "draft": {
    "title": "<≤60 chars>",
    "summary": "<≤200 chars, one line for the inbox card>",
    "message": "<first person: what you want to do and why>",
    "facts": { "why_now": "<…>", "if_it_goes_wrong": "<…>", "undo": "<…>" },
    "audio": { "script": "<≤75 words, first person>" },
    "tools": [
      { "tool": "deploy.run", "required": true, "summary": "<what this call does>",
        "params": { "service": { "eq": "api" }, "env": { "eq": "prod" }, "ref": { "eq": "a41c9e2" } } }
    ],
    "attachments": []
  },
  "also_restricted_nearby": ["db.migrate", "flags.set"],
  "guide": "inbox.guide({ \"topic\": \"requests\" })"
}
```

Don't retry the call. Fill in the draft, add every other restricted call
and every question of this round, and send it as one request.

## 3. The request

```json
inbox.request({
  "task": {"objective": "Deliver the billing handler with the rollout flag disabled."},
  "client_request_id": "billing-v2-permission-1",
  "callback_ref": "opaque-authorized-receiver-reference",
  "pending_ttl_seconds": 86400,
  "title": "Ship billing v2 to production",
  "summary": "I need 5 tools to merge PR #218, add one table, deploy, and let the team know.",
  "message": "PR #218 is approved and all 42 checks are green, so I'd like to ship the billing v2 webhook handler. It goes out behind the billing_v2 flag, which stays off…",
  "facts": {
    "why_now": "The invoice migration is waiting on this handler.",
    "if_it_goes_wrong": "The API service only. With the flag off there's no user-visible change.",
    "undo": "I redeploy 9f1e2d0, the current prod build. About 4 minutes."
  },
  "audio": { "script": "Hi, it's the billing agent. I'd like to ship billing v2 to production. …" },
  "urgency": "soon",
  "tools": [ … see §4 … ],
  "attachments": [ … see §6 … ],
  "ttl_seconds": 3600,
  "dry_run": true
})
```

| field | required | rules |
|---|---|---|
| `task.objective` | yes | The concrete objective this batch advances. |
| `client_request_id` | no | Stable per-agent retry key; changed content conflicts. |
| `callback_ref` | no | Opaque registered receiver reference; no credentials. |
| `callback_refs` | no | Up to 5 more receivers that follow this item. With neither, your default receiver follows it (§15). |
| `schema_version` | no | `3` to add `questions` (§14). |
| `questions` | no | With `schema_version: 3`: up to 20 questions your owner answers in the same decision (§14). |
| `pending_ttl_seconds` | no | Deadline for the human decision. Default 86400 seconds (24 hours); administrator limits apply. |
| `title` | yes | ≤ 60 characters. Verb + object + target. |
| `summary` | yes | ≤ 200 characters. The line on the inbox card. |
| `message` | yes | First person. What you want to do, and the context your owner needs. Markdown, ≤ 20,000 characters (§6). |
| `facts` | yes | `why_now`, `if_it_goes_wrong`, `undo`. Concrete; numbers over adjectives. |
| `audio.script` | no | Optional audio summary. See §5 for limits. |
| `urgency` | yes | `now` \| `soon` \| `digest` \| `fyi`. See §7. |
| `tools` | yes | 1–12 entries; up to 50 with `schema_version: 3` (§14). |
| `attachments` | no | Up to 24. Strongly recommended. |
| `ttl_seconds` | no | How long grants last once approved. Default 1800, max 86400. Ask for the shortest that works. |
| `session_id` | no | From `session.start`. Attaches task context to related requests. |
| `request_id` | no | On `inbox.post` updates: the request this update closes. |

For several questions of any type in one item (yes/no, numbers, dates,
scales, rankings), or for questions next to tool calls, use schema version 3
(§14). For one simple question, the version 2 `inbox.ask` contract below
still works. Audio is optional.

```json
{
  "schema_version": 2,
  "client_request_id": "stable-key-for-this-question",
  "prompt": "Which checks must run?",
  "context": "Choose the checks for this stage release.",
  "question": {
    "type": "multiple_choice",
    "min_selections": 1,
    "max_selections": 2,
    "options": [
      {"id": "mobile", "label": "Mobile layout"},
      {"id": "keyboard", "label": "Keyboard access"},
      {"id": "none", "label": "Neither", "exclusive": true}
    ]
  },
  "task": {"title": "Review stage"},
  "blocking": false
}
```

Question types are `free_text`, `single_choice`, and `multiple_choice`.
Free-text questions have no options. Choice questions accept 2–12 options with
unique stable IDs and labels. An exclusive option cannot accompany another
selection. A recommendation is a label, never a default selection.
The owner can always submit custom text without a selection. Text can also
accompany selected options. Selection alone never sends an answer.

`inbox.status` and `inbox.wait` return `response.selected_option_ids`,
`response.selected_labels`, and the exact `response.text`. The legacy `answer`
string remains available. A saved answer becomes retrieved when the agent reads
it through those tools. An answer grants no tool permission.

The dashboard submits a `request_revision`, a stable `submission_id`, and
`response` through the decision endpoint. A retry of the same answer from the
same actor returns the saved result. A conflicting retry returns 409. Expired
requests return 410. Drafts remain local to the user and browser tab for up to
24 hours, and logout clears them.

Older `inbox.ask` submissions with `title`, `summary`, `message`, and 2–4
`options` remain supported. They appear as single-choice questions with a
custom text field. Set `kind: "blocker"` when the question blocks progress.

## 4. Tools and parameters

```json
{ "tool": "deploy.run",
  "required": true,
  "summary": "Deploy the api service to prod at the merge commit from step 1, once the migration has succeeded.",
  "params": {
    "service": { "eq": "api" },
    "env":     { "eq": "prod" },
    "ref":     { "limit": "merge commit of PR #218", "pattern": "^[0-9a-f]{7,40}$" }
  },
  "after": ["migrate_billing"] }
```

- **`summary` is yours**, and your owner reads it. Describe what the call
  does in plain words, not the tool's description.

Fields of each `tools[]` entry:

| field | read | write |
|---|---|---|
| `call_id`, `tool`, `summary`, `target`, `operation`, `expected_effects`, `params` | required | required |
| `affected_scope`, `material_risks`, `undo` | — | required |
| `required`, `after` | optional | optional |

- **`call_id`** is a stable unique identifier, distinct from the tool name. Repeated tools need distinct call IDs.
- **`summary`** explains the purpose and its relationship to the task.
- **`operation`** is `read` or `write`. `read` is accepted only for a tool
  the catalog marks read-only (its `readOnlyHint`). `tools.search` shows
  `read_only` for each tool. When it is `false`, use `write`.
- For writes, supply **`affected_scope`**, **`material_risks`**, and **`undo`**.
  State explicitly when the action cannot be undone.

A minimal read:

```json
{ "call_id": "echo_hello", "tool": "fixture.echo", "summary": "Echo a test message to check the gateway path.",
  "target": "fixture echo tool", "operation": "read", "expected_effects": "Returns the message; nothing changes.",
  "params": { "message": { "eq": "hello" } } }
```

A minimal write:

```json
{ "call_id": "deploy_api", "tool": "deploy.run", "summary": "Deploy the api fix to prod.",
  "target": "Production API service", "operation": "write", "expected_effects": "The API runs the fixed release.",
  "affected_scope": "API service deployment", "material_risks": "A bad release can interrupt API requests.",
  "undo": "Redeploy the previous release.", "params": { "service": { "eq": "api" }, "env": { "eq": "prod" } } }
```
- **`required`** is only a planning hint. The human can reject any call and complete the decision.
  Return rejected calls to the agent without permission.
- **`params`**: give every argument you'll pass. **Your grant covers only
  the arguments you list**; a call that passes anything else is blocked.
  - `{"eq": value}` is an exact value. Prefer it. A bare value (`"env": "prod"`)
    means the same.
  - `{"in": [...]}`, `{"prefix": "..."}`, `{"gte": n, "lte": n}` narrow a value.
  - `{"limit": "what it will be", "pattern": "regex"}` is for a value that
    doesn't exist yet (a commit a merge will create). Toolyard checks the
    actual value against `pattern` when you make the call. Without an enforced pattern this scope is rejected.
  - `{"any": true}` is unbounded and is rejected.
- **`after`** references call IDs, never tool names. Invalid references and cycles are rejected.
  Dependencies do not authorize rejected calls or widen accepted parameters.

## 5. The voice note

Your owner often listens before reading. Toolyard turns your script into
speech (recorded on the server, or read by the owner's browser); you never
handle audio files. The script is read aloud exactly as written.

- **75 words at most** (about 30 seconds).
- **First person**: "I'd like to…", "I need…".
- **Written for the ear**: no hashes, IDs, URLs or code. Round numbers, and
  say units in words ("0.2 percent").
- **In this order**: what you want, why it's safe, the one or two numbers
  that matter, then the exact ask.
- **Consistent with your message.** If you say a flag stays off, don't
  request a tool that turns it on. Toolyard flags contradictions.

## 6. Attachments: show, don't describe

Send **data** when you can. Toolyard renders it natively, so it's sharp on a
phone and readable in both themes.

| type | fields | use it for |
|---|---|---|
| `markdown` | `title`, `body` | notes, lists, short explanations |
| `table` | `title`, `columns`, `rows` | test results, row counts, recipients |
| `chart` | `title`, `chart` (`line` \| `bar` \| `stacked_bar` \| `area` \| `pie` \| `scatter`), `unit`, `x`, `series: [{ name, values }]` | error rates, traffic, shares, correlations |
| `diff` | `file`, `patch` (unified diff) | the core of a code change; keep it to the part that matters |
| `code` | `file`, `language`, `body` | a migration, a config change |
| `log` | `title`, `body` | terminal output, error messages |
| `image` | `upload` or `url`, `alt` | screenshots, diagrams |
| `video` | `upload` or `url`, `poster_upload?` or `poster_url?` | screen recordings of the thing working (MP4 H.264, ≤ 2 min) |
| `file` | `upload` or `url`, `name` | CSVs, reports |
| `link` | `label`, `url` | PRs, CI runs, dashboards |

**Markdown.** `message` and `markdown` attachments render as CommonMark with
GitHub tables, fenced code blocks, lists, quotes and links. Links work for
`http`, `https` and `mailto` only. Raw HTML is shown as text, never run. An
image shows inline only when it cites one of your own image uploads:
`![Checkout after the fix](upload:<sha256>)` (upload it with `inbox.upload`
first); any other image source shows as a link.

**Charts.** Every chart has a `title` and 1–6 `series` of up to 500 values;
`x`, when given, labels each value.
- `line`, `bar`, `area`: series over `x`.
- `stacked_bar`: series stacked per `x`; values of 0 or more.
- `pie`: exactly one series of up to 24 values of 0 or more, with a label in
  `x` for each slice.
- `scatter`: `x` holds the numeric x coordinates; each series' values are
  the y coordinates.

```json
{ "type": "chart", "title": "Where the 5xx errors come from", "chart": "pie",
  "x": ["checkout", "search", "login"], "series": [{ "name": "errors", "values": [61, 27, 12] }] }
```

Every attachment takes an optional **`caption`**: one first-person sentence
on why it matters ("I replayed a failing delivery. It retried twice and got
through."). A chart takes an optional **`spoken`** phrase: the words your
voice note uses for it.

**Upload media you hold locally (preferred for screenshots and diagrams).**
You don't need a public host:

1. Call `inbox.upload` with `name`, `content_type`, `size` (bytes), `sha256`
   (64 lowercase hex, from `sha256sum`) and `kind` (`image` default, `video`,
   `file`). It returns `upload_url`, `ref` and a ready `curl` command.
2. `PUT` the exact bytes to `upload_url` within 15 minutes:
   `curl -sf -X PUT -H 'Content-Type: image/png' --data-binary @shot.png '<upload_url>'`.
   The URL works once and is the credential: never paste it anywhere else.
   Toolyard checks the size, the sha256 and the real file type (an image must
   really be PNG, JPEG, WebP or GIF).
3. Cite the ref instead of a URL:
   `{ "type": "image", "upload": "upload:<sha256>", "alt": "Checkout after the fix" }`.
   For a video poster use `poster_upload`.

Give exactly one of `url` or `upload`. Only you can cite your uploads. An
upload no request cites within 24 hours is deleted. If you already uploaded
the same bytes, `inbox.upload` answers `already_uploaded: true` with the ref.

**Or send media as links.**
- `image`, `video` and `file` also take an `https` URL that toolyard can reach.
- Toolyard **fetches each link once when you send the request, and keeps a
  copy**, so it still works after your sandbox is gone.
- A `localhost` or private-network URL fails the dry run: upload it instead.
- Limits (both ways): image 10 MB, video 50 MB, file 25 MB.
- `inbox.guide({ "topic": "hosting" })` has your owner's note on hosting.

## 7. Urgency is a promise

| urgency | use when |
|---|---|
| `now` | Production is broken, or money or data is being lost right now. |
| `soon` | You'll be blocked within the hour. |
| `digest` | You need it today, not this hour. |
| `fyi` | No action needed. Updates only. |

Your owner decides when their phone buzzes, not you: `now` pushes at once,
`soon` is grouped with your session's other requests for a minute and a half,
`digest` waits for the owner's next digest (by default 09:30, 13:30 and
18:30), and `fyi` never pushes. Quiet hours hold everything except the tools
your owner allows through.

Each agent gets **3 `now` requests an hour** (your owner can change this).
Past that, toolyard lowers the request to `soon`, shows your owner it was
lowered, and returns a warning with `path: "urgency"`.

## 8. Dry run

`dry_run: true` sends nothing to your owner. You get back:

```json
{
  "ok": false,
  "dry_run": true,
  "valid": false,
  "problems": [
    { "path": "audio.script", "message": "112 words; max 75. Keep the ask and one number." },
    { "path": "attachments[1].url", "message": "localhost isn't reachable from toolyard." }
  ],
  "warnings": [],
  "flags": [
    { "tool": "flags.set", "level": "red", "label": "Doesn't match the request",
      "why": "Your message says the flag stays off. This call turns it on." }
  ],
  "preview": { "card_title": "Ship billing v2 to production", "audio_seconds": 28 }
}
```

- **Fix every problem.** A request with problems is rejected. A dry run
  with problems is an error result (`isError: true`, `valid: false`), so
  `try_call` reports it as failed. A passed dry run returns `valid: true`.
- **Read the warnings.** They don't block sending, but your owner will notice
  what they point at.
- **Flags aren't errors.** They're what your owner will see. Judge flags
  ("Doesn't match the request") only appear when your owner has the judge
  model switched on. Where a flag is
  fair, add evidence for it. Where it's caused by a mistake, fix the mistake.
- **Don't reword a request to make a flag go away.** Dry runs are logged, and
  your owner sees how many you ran.

## 9. Waiting, grants and using them

`inbox.request` returns `request_id`, `status`, and the pending `expires_at`.

- Keep working on anything that doesn't depend on the answer.
- `inbox.status({ "ids": [...] })` never blocks. For one item,
  `{ "request_id": "rq_…" }` works too (also on `inbox.wait`).
- `inbox.wait({ "ids": [...], "mode": "any" | "all", "timeout_seconds": ≤300 })`
  blocks. Don't poll in a tight loop.
- A tool your owner made async answers with a `job_id` instead of its
  result, granted calls included. Read it with `jobs.wait` (blocks up to
  60 s), `jobs.status` or `jobs.result`, and never call the tool again for
  the same work. `_callback_ref` asks for a `job.completed` / `job.failed`
  callback. See `docs/guidelines/async-tools.md`.

A decision looks like this:

```json
{ "request_id": "rq_…", "kind": "access", "status": "approved",
  "tools": [
    { "tool": "github.merge_pull_request", "decision": "allowed", "grant_id": "gr_…", "grant": "tyg_…" },
    { "tool": "deploy.run", "decision": "allowed", "grant_id": "gr_…", "grant": "tyg_…",
      "narrowed": true, "params": { "env": { "eq": "prod" }, "ref": { "eq": "7c1d2e9" } } },
    { "tool": "flags.set",                 "decision": "refused" }
  ],
  "owner_note": "Don't touch the flag; I'll roll it out myself.",
  "grants_expire_at": 1790621520000,
  "next": "Call each allowed tool with its grant as `_grant`, …" }
```

`status` is one of `pending`, `approved`, `denied`, `returned`, `answered`,
`cancelled`, `expired`. Always read `next`: it says what to do. When the item
had questions (§14), `answers` holds your owner's answers keyed by question ID.

- Status reads do not consume grants. The authorized agent can recover the same still-valid unused token after a lost response.
  Recovery adds no use and never extends or replaces permission. Keep tokens out of logs, commits, and callbacks.
- Call the tool with `_grant` added to its arguments. Stay inside the
  parameters you asked for, or the call is blocked. In `executeToolCode`
  the grant is one more argument of the call: Starlark
  `server.tool(param="value", _grant="tyg_…")`, JavaScript
  `await tools.server.tool({ param: "value", _grant: "tyg_…" })`. A REST run
  (§13) never holds a token in its script: resume it with the `request_id`.
- If the answers open more restricted work, ask for all of it in one new
  `inbox.request`.
- Your owner can **narrow** a tool before allowing it: fewer values, an
  exact value instead of a `limit`, a tighter range, or a shorter
  `grants_expire_at`. They can never widen it. A narrowed tool has
  `"narrowed": true` and its `params` are what the grant allows: use those.
- Read `owner_note`, and honour it even for tools that were allowed.
- Read each call's `call_id`, `verdict` (`accepted` or `rejected`), and optional written `reason`.
  An all-rejected batch has status `denied` and no grants. Mixed batches grant only accepted calls.
- Execution is separate: `not_started`, `running`, `succeeded`, `failed`, or `outcome_unknown`.
  A claimed grant never executes again. If an upstream write times out or the process stops after the claim, inspect upstream state.
  Do not blindly repeat a write with an unknown outcome.
- A callback announces one committed decision, with every verdict and note, and no grant tokens or sensitive tool results.
  Retrieve current authoritative status after the callback; it is not permission itself.
- A pending request expires after 24 hours by default. Grant lifetime defaults to 1800 seconds after acceptance.
  Expired or revoked grants cannot be revived.

A human submission supplies the Inbox request ID in the URL and this complete contract:

```json
{
  "action": "submit",
  "request_revision": 1,
  "submission_id": "stable-decision-retry-key",
  "verdicts": {
    "migrate_billing": {"verdict": "rejected", "reason": "The migration is not ready."},
    "deploy_api": {"verdict": "accepted", "reason": "The release is ready."}
  },
  "note": "Keep the rollout flag disabled."
}
```

Every requested call ID must appear exactly once. An item with questions also
carries `answers` (keyed by question ID, every required question answered) in
the same submission. No callback is emitted for draft selections or notes.
The decision, accepted-call grants, audit, and callback event commit together.
The same submission ID and content return the saved result. Changed content or a stale revision returns a conflict.
A competing decision cannot overwrite a committed decision. Drafts remain available after a failed or conflicting submission.

## 10. Close the loop

When the task is done, send an update with what happened and how you checked
it, again with a voice note:

```json
inbox.post({ "kind": "update", "request_id": "rq_…",
  "title": "Billing v2 is live in prod, flag still off",
  "message": "Done. PR #218 is merged as 7c1d2e9, …",
  "audio": { "script": "All done. …" },
  "attachments": [ { "type": "chart", … } ] })
```

Withdraw requests you no longer need with `inbox.cancel`.

## 11. Never

- Never get around a restricted tool: no other credentials, no direct API
  calls, no asking another agent to do it.
- Never split one risky action into small requests to avoid a flag.
- Never ask one call at a time: every restricted call and every question of
  a round goes into one request.
- Never tune your wording across dry runs to hide what a call does.
- Never claim urgency you don't have.
- Never ask your owner for a password, token or one-time code to get past a
  sign-in; give them the connect link and wait.

## 12. Examples

**An access request.** Evidence first, every tool in one request, and the
voice note in the first person.

```json
inbox.request({
  "title": "Ship billing v2 to production",
  "summary": "I need 3 tools to merge PR #218, add one table, and deploy.",
  "message": "PR #218 is approved and all 42 checks are green, so I'd like to ship the billing v2 webhook handler. It goes out behind the billing_v2 flag, which stays off.",
  "facts": {
    "why_now": "The invoice migration is waiting on this handler.",
    "if_it_goes_wrong": "The API service only. With the flag off there's no user-visible change.",
    "undo": "I redeploy 9f1e2d0, the current prod build. About 4 minutes."
  },
  "audio": {
    "script": "Hi, it's the billing agent. I'd like to ship billing v2 to production. I need to merge the approved pull request, add one new table, and deploy. The new code sits behind a flag that stays off. All 42 checks passed. I'm asking for one merge, one migration and one deploy, valid for 30 minutes."
  },
  "urgency": "soon",
  "tools": [
    {
      "tool": "github.merge_pull_request",
      "required": true,
      "summary": "Squash-merge PR #218 into main.",
      "params": {
        "repo": "acme/api",
        "pull_number": 218,
        "merge_method": "squash"
      },
      "call_id": "merge_billing",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Squash-merge PR #218 into main.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration.",
      "after": []
    },
    {
      "tool": "db.migrate",
      "required": true,
      "summary": "Apply migration 0042, which adds the webhook_events table.",
      "params": {
        "env": "prod",
        "migration": "0042_webhook_events",
        "direction": "up"
      },
      "call_id": "migrate_billing",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Apply migration 0042, which adds the webhook_events table.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration.",
      "after": []
    },
    {
      "tool": "deploy.run",
      "required": true,
      "summary": "Deploy the api service to prod at the merge commit.",
      "params": {
        "service": "api",
        "env": "prod",
        "ref": {
          "limit": "merge commit of PR #218",
          "pattern": "^[0-9a-f]{7,40}$"
        }
      },
      "after": [
        "merge_billing",
        "migrate_billing"
      ],
      "call_id": "deploy_api",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Deploy the api service to prod at the merge commit.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration."
    }
  ],
  "attachments": [
    {
      "type": "table",
      "title": "Checks",
      "columns": [
        "Check",
        "Result"
      ],
      "rows": [
        [
          "Unit tests",
          "1,284 passed"
        ],
        [
          "Integration",
          "212 passed"
        ]
      ]
    },
    {
      "type": "chart",
      "chart": "line",
      "title": "Error rate, canary vs prod",
      "unit": "%",
      "x": [
        "-30m",
        "-15m",
        "now"
      ],
      "series": [
        {
          "name": "canary",
          "values": [
            0.19,
            0.21,
            0.2
          ]
        },
        {
          "name": "prod",
          "values": [
            0.2,
            0.2,
            0.2
          ]
        }
      ],
      "spoken": "the canary has been flat for 30 minutes"
    },
    {
      "type": "diff",
      "file": "handlers/webhook.go",
      "patch": "@@ -41,4 +41,6 @@\n-\treturn err\n+\treturn h.retry(ev)\n",
      "caption": "The core of the change: failed deliveries now retry."
    },
    {
      "type": "link",
      "label": "PR #218",
      "url": "https://github.com/acme/api/pull/218"
    }
  ],
  "ttl_seconds": 1800,
  "task": {
    "objective": "Deliver the billing handler with its rollout flag disabled."
  }
})
```

**A question.**

```json
inbox.ask({
  "title": "Retire /v1/invoices now, or in 30 days?",
  "summary": "Traffic to the old endpoint fell about 80% in two weeks. Three callers are left.",
  "message": "The migration to /v2/invoices is done on our side. Three callers are left; the biggest is our own reporting job, which I can update myself.",
  "audio": { "script": "I need a decision on the old invoices endpoint. Its traffic fell by about 80 percent in two weeks. Should I retire it now, or give it 30 more days?" },
  "urgency": "soon",
  "options": [
    { "label": "Retire now", "detail": "Returns 410 Gone. I update the reporting job first." },
    { "label": "Deprecate, retire in 30 days", "detail": "Adds a Sunset header and emails the 2 external callers." }
  ]
})
```

**An update that closes a request.**

```json
inbox.post({
  "kind": "update", "request_id": "rq_…",
  "title": "Billing v2 is live in prod, flag still off",
  "summary": "Merged, migrated and deployed. Errors are flat.",
  "message": "Done. PR #218 is merged, the webhook_events table exists, and the API is deployed. Error rate has been flat since.",
  "audio": { "script": "All done. The pull request is merged, the new table is in place, and the API is deployed. Errors have been flat since." },
  "urgency": "fyi"
})
```


## Local server connections

A server without a verified team identity can use the explicit API key mode in
its Toolyard settings. The human enters an existing Toolyard agent API key and
instance URL through authenticated settings. The server exchanges that key for
an independent environment/user credential and removes the bootstrap key from
client state. This mode does not provision users or prove a Clerk identity.
Never ask for the key in a prompt, place it in an Inbox item, or use one user's
connection for another user. Parent key rotation/revocation and disabled
accounts invalidate derived access. Explicit connection removal is persistent.

Local session callbacks use registered `transport: "pull"` receivers. The server
retrieves the signed persistent outbox through authenticated bounded requests,
verifies and persists each event, then acknowledges its event ID. Unacknowledged
events survive restart and may repeat. No public laptop address or agent
`inbox.wait` is necessary. The callback still announces a decision rather than
permission. Read authoritative Inbox status before any approved execution.

`Open Toolyard` uses a one-minute, single-use dashboard handoff link. The link
opens the same Toolyard instance's Inbox and never signs a browser in: it sets
no cookie and creates no session. A browser that already has a Toolyard session
shows the Inbox. A browser without one shows the normal sign-in, and the person
signs in there with the instance's usual method. Host handoff links behave the
same way. Credentials remain on the server and are never embedded in the
browser URL.

## Server-owned host connections

A host server's browser consent flow creates one dedicated Toolyard agent for the
selected server environment and authenticated server profile. The Toolyard
browser account is the agent owner. A hosted profile must match that account's
verified Clerk subject; the fixed `local-user` profile uses explicit account
owner consent. Server labels and platforms are self-reported, not verified
hardware identity.

The server signs a short-lived EdDSA proof to `/v1/connections/host/begin`.
The owner reviews `/connections/authorize` and explicitly accepts or rejects.
GET does not grant permission. Only the same server key can poll the result;
the browser never sees a credential. An approved result is recoverable without
issuing another credential while its connection generation remains active.
Replayed proof IDs, substituted environments/profiles, changed keys, and expired
pending requests are rejected. Consent does not approve any restricted call.

Host credentials remain in the destination server secret store. The host's
clients and authenticated native MCP callers use the server proxy. Requests therefore
carry the dedicated agent and its real account owner rather than a shared key.
Private hosts use outbound pull callbacks. The host's own connection token
authorizes access to the host; it is not Toolyard consent.

Each host has a 30-day renewable connection lease. API status and the dashboard
show the owner, host, agent, and expiry. Owners can revoke a host in Toolyard.
Revocation closes unused grants and callback registrations. Account/agent
restore cannot recreate that access; fresh browser consent is necessary.
Account ownership and connection lifecycle audit records are transactional.
Audit records distinguish owner consent from server claims and renewal.

## 13. Backup route when MCP fails

Use this only when your MCP connection to toolyard is broken: the server
won't connect, calls time out, or your client lost the toolyard tools. Go
back to MCP as soon as it works again.

It's the same code mode as `executeToolCode`, over plain HTTPS. You run as
the same agent, your owner's policy is the same, and a call that needs
permission still needs an Inbox approval. It's never a way around a
restricted tool or a denial (section 11).

**What you need.**

- `TOOLYARD_URL` (required): your toolyard's base URL, for example
  `https://toolyard.example.com`. Set it yourself. The `ty` that
  `GET /v1/code/ty` serves defaults to the toolyard that served it; any
  other copy has no default and refuses to run without it.
- `TOOLYARD_TOKEN_FILE`: a file that holds your toolyard token, readable
  only by you (`chmod 600`). Some hosts write a session token (`tys_…`)
  there for your session. Operator tokens (`tyop_…`) never work
  here.

Never print, `echo` or `cat` the token into your output. Never put it in a
command line, a script, a log, a commit or an Inbox item.

**Get `ty`.** It's one Python 3 file (standard library only). Fetching it
needs no token:

```sh
curl -fsS "$TOOLYARD_URL/v1/code/ty" -o ty && chmod +x ty
./ty files                      # the stubs you may use, like listToolFiles
./ty stub github/create_issue   # one stub, like readToolFile
```

**Run a script.** Write the same Starlark you'd pass to `executeToolCode`
(or JavaScript: a `.js` file runs as JavaScript, see the code-mode guide):

```sh
./ty run review.star --reason "List the open pull requests to pick the next one to review"
./ty call github.list_pull_requests '{"owner": "acme", "repo": "api", "state": "open"}'
```

`--reason` (20–2000 characters) goes to the audit record, like `_reason`;
without it, `ty` writes one from the file name and the first line. `ty`
prints the run view as JSON on stdout and exits with the run's status:

| Exit | HTTP | Run status | What it means |
|---|---|---|---|
| 0 | 200 | `succeeded` | Read `result`, `logs` and `calls`. |
| 1 | 200 | `failed` | Read `error.kind` and `error.message`, and fix the script. |
| 3 | 403 | `permission_required` | A call needs your owner's approval. See below. |
| 4 | 202 | `queued`, `running`, `deferred` | Still running, or waiting on a legacy approval: `./ty status cr_… --wait 120`. |
| 5 | 429 | none | Code mode is busy and no run was created. Wait `Retry-After` seconds, then run it again. |
| 6 | 409 | `outcome_unknown` | A call may have committed. Check the upstream state; never repeat it blindly. |
| 2 | other | none | A usage or transport error; stderr says which. 401 is a missing, expired or revoked token; 404 an unknown run. |

**Idempotency and retries.** Every `ty run` sends an `Idempotency-Key`.
When you don't pass `--key`, it makes one and prints it on stderr. After a
timeout or a lost answer, run the same command again with `--key <that
key>`: the same key with the same script and reason returns the existing
run and runs nothing new. The same key with a different script or reason is
`422 idempotency_key_reused`. The run keeps going on the server after the
answer, so you never need to hold the connection: `./ty status cr_…` reads
it later. Runs are kept 7 days.

**When a call needs permission (exit 3, HTTP 403).** Nothing after the held
call ran. `held` names it (`seq`, `tool`), and `held.draft` is the Inbox
request draft. Ask exactly as sections 1–9 say, from code; toolyard's own
tools are `toolyard.<group>_<name>`:

```sh
./ty call toolyard.inbox_request - < request.json   # "dry_run": true first, then for real
./ty call toolyard.inbox_status '{"ids": ["rq_…"]}'
```

When your owner has decided, resume the run with the request id:

```sh
./ty resume cr_… --request rq_… --wait 120
```

The server adds that request's grants to the matching calls, and the script
runs again from the top. Each call the run made before is answered from the
record (`replayed: true`) and not sent again: a succeeded call with its
value, a failed one with the same failure, a held one you caught with the
same hold unless a grant now covers it. So don't change the script between
the hold and the resume: a different call fails the run with
`script_diverged`, and nothing on the new path is sent. A recorded call with
an unknown outcome stops the resume with `outcome_unknown`. A `deferred` run
(a legacy approval, with `held.approval_id` set) resumes with
`./ty resume cr_…` and no `--request` once your owner has decided; it
resumes with that approval even when you pass `--request`.

**Without `ty`.** Use `curl` with the header read from a file, so the token
never appears in a command line or the process list:

```sh
auth() { printf 'Authorization: Bearer '; tr -d ' \r\n\t' < "$TOOLYARD_TOKEN_FILE"; echo; }

curl -sS "$TOOLYARD_URL/v1/code/run" -H @<(auth) \
  -H 'Content-Type: text/x-starlark' \
  -H 'X-Toolyard-Reason: List the open pull requests to pick the next one to review' \
  -H 'Idempotency-Key: review-pick-1' \
  --data-binary @review.star
curl -sS "$TOOLYARD_URL/v1/code/runs/cr_…" -H @<(auth) -H 'Prefer: wait=60'
curl -sS "$TOOLYARD_URL/v1/code/runs/cr_…/resume" -H @<(auth) \
  -H 'Content-Type: application/json' -d '{"request_id": "rq_…"}'
```

`Prefer: wait=<seconds>` (0–60, default 25) holds the answer until the run
finishes or is held. Never write `-H "Authorization: Bearer $TOKEN"` or
`-u`: that puts the token in argv. The full API is at
`GET /v1/code/openapi.json`.

**Limits.** The code-mode limits apply: 5 minutes per run, 100 tool calls,
a 1 MiB script, 1 MiB of output, and 4 scripts at a time per server (more
is 429). A run that was running when the server restarted is
`outcome_unknown`.

## 14. Questions and rich items (schema version 3)

With `"schema_version": 3` one Inbox item can unblock a whole session: up to
50 tool calls, up to 20 questions of every type, and up to 24 attachments.
Your owner answers everything in **one** decision; drafts and partial answers
are never sent to you.

- `inbox.request` with `questions`: tool approvals and questions together,
  the usual shape of a round (one request for everything the round needs).
- `inbox.ask` with `questions`: questions only. `title`, `summary` and
  `message` may be left out when there is exactly one question; its prompt
  stands in.
- The old single `question` (and version 1 `options`) still work with
  version 3: they become `questions[0]` with the ID `answer`.

Each question has a stable `id` (1–80 letters, digits, `.`, `:`, `_`, `-`),
a `type`, a `prompt` (≤ 1000 characters), an optional `help`, `required`
(default `true`), and an optional `default` (pre-filled) and `recommended`
(shown as a suggestion, never chosen for your owner). The answer's `value`
has the question's type:

| type | fields | answer `value` |
|---|---|---|
| `free_text` | `min_length`, `max_length` (≤ 10000), `multiline` | a string |
| `single_choice` | `options` (2–12), `allow_other` | one option ID |
| `multiple_choice` | `options` (2–12), `min_selections`, `max_selections`, `allow_other` | a list of option IDs |
| `yes_no` | none | `true` or `false` |
| `number` | `min`, `max`, `step`, `unit` | a number within the range, a whole number of steps from `min` |
| `date` | none | `"YYYY-MM-DD"` |
| `datetime` | none | RFC 3339, e.g. `"2026-10-25T14:30:00+05:30"` |
| `scale` | `scale` (2–10 points, default 5), `min_label`, `max_label` | a whole number from 1 to `scale` |
| `ranking` | `options` (2–12) | every option ID, most preferred first |

Options are `{ "id", "label", "detail"?, "exclusive"?, "recommended"? }`. An
exclusive option is selected alone. With `allow_other`, a choice question
also accepts a custom text answer instead of a selection.

```json
inbox.request({
  "schema_version": 3,
  "task": {"objective": "Ship billing v2 with the rollout flag disabled."},
  "title": "Ship billing v2 and pick the rollout",
  "summary": "2 tools to migrate and deploy, and 3 questions about the rollout.",
  "message": "PR #218 is approved and **all 42 checks** are green.\n\n| check | result |\n|---|---|\n| unit | 212 passed |\n| e2e | 18 passed |",
  "facts": {"why_now": "The invoice migration waits on it.", "if_it_goes_wrong": "API only; the flag stays off.", "undo": "I redeploy the current build in about 4 minutes."},
  "urgency": "soon",
  "tools": [
    {"call_id": "migrate", "tool": "db.migrate", "required": true, "summary": "Apply migration 0042.", "target": "Production billing database", "operation": "write",
     "expected_effects": "Adds the invoices_v2 table.", "affected_scope": "One new table", "material_risks": "A lock on the invoices table for under a second.",
     "undo": "Drop the new table.", "params": {"env": "prod", "migration": "0042"}},
    {"call_id": "deploy", "tool": "deploy.run", "required": true, "summary": "Deploy the merge commit.", "target": "Production billing API", "operation": "write",
     "expected_effects": "The new handler ships behind the flag.", "affected_scope": "The billing API deployment", "material_risks": "A bad build could fail invoice creation.",
     "undo": "Redeploy the previous release.", "params": {"service": "api", "env": "prod"}}
  ],
  "questions": [
    {"id": "rollout", "type": "single_choice", "prompt": "Which rollout do you want?", "recommended": "staged",
     "options": [{"id": "staged", "label": "Staged, 10% a day"}, {"id": "all", "label": "Everyone at once"}]},
    {"id": "notify_team", "type": "yes_no", "prompt": "Shall I post in #billing when it's live?", "default": true},
    {"id": "start_on", "type": "date", "prompt": "From which day may the rollout start?", "required": false}
  ]
})
```

The answers come back keyed by question ID in `inbox.status`, `inbox.wait`
and the `inbox.decision` callback:

```json
{ "request_id": "rq_…", "status": "approved",
  "answers": {
    "rollout": {"value": "staged", "labels": ["Staged, 10% a day"]},
    "notify_team": {"value": true, "labels": ["Yes"], "text": "Tag Priya."}
  },
  "tools": [ … ] }
```

An unanswered optional question is absent. `text` is a custom answer
(`allow_other`) or a comment. Answers never grant a tool: only accepted
calls get grants. On a question item the status is `answered`; the legacy
`answer` string lists every answer as `prompt: answer` lines.

## 15. Callbacks: receivers and events

A callback is toolyard calling you when something you wait on happens, so
you can end your turn instead of polling. Register a receiver once, then
name it on the calls that should report to it. The `callbacks.*` tools are
always available; in code mode they are `toolyard.callbacks_register(...)`,
`toolyard.callbacks_list(...)` and so on, and `tools.search` lists them
under the server `callbacks`.

**Register.** `callbacks.register({ "destination": "https://example.com/hooks/toolyard", "events": ["*"], "v3": true, "make_default": true })`
returns a `callback_ref` (`cb_…`). A receiver gets only the events it asks
for: `events` lists names, `"<prefix>.*"` or `"*"`; without it, only
`inbox.decision`, `inbox.expired` and `inbox.cancelled`. `v3: true` accepts
the schema version 3 decision shape (`answers`, more than 12 calls);
without it the receiver gets the version 2 shape, and an item with more
than 12 calls refuses it. Give your own Standard Webhooks `secret`
(`whsec_` and 24–64 random bytes in Base64), or leave it out and toolyard
generates one and returns it **once**: store it. Destinations are public
`https` URLs on port 443 and never change; register a new receiver for a new
URL. On a host that gives your session its own webhook, use that instead.

**Use it.** Pass `callback_ref` (and up to 5 `callback_refs`) on
`inbox.request` or `inbox.ask`; several receivers can follow one item. With
none, your default receiver (`callbacks.set_default`) follows it. If the
default cannot carry the item (more than 12 calls without `v3`, or no
`inbox.*` events), the answer has a `callback_ref` warning and the default
does not follow it: name another receiver or poll. On a call
that may wait for a legacy approval, add `_callback_ref` to its arguments
(at most 512 characters). A tool that declares a `_callback_ref` parameter
of its own keeps it: toolyard then neither reads nor removes the argument.
If toolyard cannot register the receiver, the deferred answer carries a
`callback_warning`.

**Manage.** `callbacks.list` (with `callback_ref`: its recent deliveries),
`callbacks.update` (`disable`, `rotate`, `remove`, with `expected_revision`),
`callbacks.set_default` (an empty `callback_ref` clears it), and
`callbacks.test`, which sends a signed `ping` (receivers whose `events`
include `ping`; one pending ping per receiver, 10 a minute). An agent keeps
at most 500 active receivers (the administrator may change it); disabled
ones do not count. A push receiver unused for 30 days (no delivery, nothing
new to follow) shows `disabled_reason: "idle"` and becomes active again when
you use it.

**Events.** Every event is a JSON envelope
`{ "type", "event_id", "timestamp", "data" }`, POSTed with the headers
`webhook-id` (the event ID), `webhook-timestamp` and `webhook-signature`.

| type | when | data |
|---|---|---|
| `inbox.decision` | the item is fully answered | `inbox_id`, `status` (`decided`), `item_status` (as `inbox.status` says: `approved`, `denied`, `returned`, `answered`), every call's `verdict` and `reason`, `overall_note`, `answers`, `status_ref` |
| `inbox.expired`, `inbox.cancelled` | the item closed without a decision | the same fields; `status` and `item_status` are `expired` or `cancelled` |
| `execution.completed` | a granted call ran | `inbox_id`, `call_id`, `grant_id`, `tool`, `state` (`succeeded`, `failed`, `outcome_unknown`) |
| `approval.decided` | a legacy deferred approval was decided | `approval_id`, `status`, `tool` |
| `approval.executed` | its call ran | `approval_id`, `execution_state`, `is_error` |
| `ping` | `callbacks.test` | `callback_ref`, `message` |

A callback never carries grants, tokens or tool results, and it is not
permission: read `inbox.status` (or `tools.poll_approval`) before you act.

**Verify every delivery.** Compute HMAC-SHA256 over
`<webhook-id>.<webhook-timestamp>.<raw body>` with the secret's bytes (the
Base64 after `whsec_`), and compare it in constant time with the Base64 after
`v1,` in `webhook-signature`. Refuse a timestamp more than 5 minutes old.

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers: dict, body: bytes) -> bool:
    key = base64.b64decode(secret.removeprefix("whsec_"))
    msg_id, ts = headers["webhook-id"], headers["webhook-timestamp"]
    if abs(time.time() - int(ts)) > 300:
        return False
    mac = hmac.new(key, f"{msg_id}.{ts}.".encode() + body, hashlib.sha256).digest()
    expected = base64.b64encode(mac).decode()
    return any(hmac.compare_digest(expected, sig.split(",", 1)[1])
               for sig in headers["webhook-signature"].split() if sig.startswith("v1,"))
```

Answer 2xx once you have stored the event. Delivery is at least once:
deduplicate on `event_id`. Failed deliveries retry after 5 s, 1 min, 5 min,
30 min, 2 h, 6 h, 12 h and 24 h; `410 Gone` stops them.

Events have no order: a retry or a parallel delivery can bring a later event
first. Never infer state from the order of arrival. Read the current state
from the event's `status_ref` (`inbox.status`, or `tools.poll_approval` for
an approval) before you act.
