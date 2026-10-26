# inbox.request: fields, constraints and limits

Every Inbox tool also takes `_reason` (20-2000 characters) for the audit
record. `inbox.guide({"topic": "requests"})`, `"tools"`, `"attachments"` and
`"dry-run"` serve the same contract from the server.

## Top-level fields

| field | required | rules |
|---|---|---|
| `task.objective` | yes | The concrete task this batch advances. At most 600 characters. `task.title` and `task.url` are optional. |
| `title` | yes | At most 60 characters. Verb + object + target. |
| `summary` | yes | At most 200 characters. The one line on the Inbox card. |
| `message` | yes | First person: what you want and why. Markdown, at most 20,000 characters. |
| `facts.why_now` | yes | Why this is needed now. At most 600 characters. |
| `facts.if_it_goes_wrong` | yes | What breaks, concretely. At most 600 characters. |
| `facts.undo` | yes | How you would undo it, or a plain statement that it cannot be undone. At most 600 characters. |
| `urgency` | yes | `now`, `soon`, `digest` or `fyi` (see Urgency). |
| `tools` | yes | 1-12 calls; up to 50 with `schema_version: 3`. |
| `audio.script` | no | A voice note the owner may hear first: at most 75 words, first person, no URLs (a URL is a problem), no hashes or IDs (a warning). |
| `attachments` | no | Up to 24 pieces of evidence. Strongly recommended. |
| `schema_version` | no | `1`, `2` or `3`. `3` allows `questions` and up to 50 calls. |
| `questions` | no | With `schema_version: 3`: up to 20 questions answered in the same decision. Shapes: `skill://toolyard/toolyard-permissions-and-inbox/references/questions-and-items.md` (`skills.read`). |
| `client_request_id` | no | Your stable retry key: at most 128 bytes of printable ASCII, no backslash. The same key with the same content returns the stored request; the same key with changed content is a conflict. |
| `callback_ref` | no | An opaque receiver reference (at most 512 bytes) for the decision callback. |
| `callback_refs` | no | Up to 5 more receivers. With neither, your default receiver follows the item. |
| `pending_ttl_seconds` | no | Deadline for the owner's decision: 60 to 604800 seconds, default 86400 (24 hours). An administrator limit can be lower: a deadline above it, the default included, is refused with `exceeds the administrator deadline limit`. |
| `ttl_seconds` | no | How long grants last after acceptance: 60 to 86400 seconds, default 1800. Ask for the shortest that works. |
| `session_id` | no | From `session.start`; ties related requests to one piece of work. |
| `dry_run` | no | `true` checks the request and shows its flags without sending it. |

The whole request is at most 1 MiB as JSON, and one call's `params` at most
64 KiB. Attach large evidence as an upload instead.

## Each call in `tools`

| field | required | rules |
|---|---|---|
| `call_id` | yes | Stable unique ID: 1-80 characters, starts with a letter or digit, then letters, digits, `.`, `:`, `_`, `-`. Repeated tools need distinct IDs. |
| `tool` | yes | The catalog name. An unknown tool is a problem; a tool blocked for you is a problem; an open tool is a warning (call it directly). |
| `required` | yes | A planning hint only. The owner can reject the call and still complete the decision. |
| `summary` | yes | At most 200 characters. In plain words: what this call does and how it serves `task.objective`. |
| `target` | yes | The exact resource or a bounded target. At most 600 characters. |
| `operation` | yes | `read` or `write`. `read` is accepted only when the catalog marks the tool read-only; otherwise use `write`. |
| `expected_effects` | yes | What the call will change or return. At most 600 characters. |
| `affected_scope` | writes | What changes. At most 600 characters. |
| `material_risks` | writes | Concrete risks. At most 600 characters. |
| `undo` | writes | A concrete undo path, or a plain statement that it cannot be undone. At most 600 characters. |
| `params` | yes | Every argument you will pass, each as a value or a constraint (below). |
| `after` | no | Call IDs in this request that must run first. Unknown IDs and cycles are rejected. Dependencies never authorize a rejected call or widen an accepted one. |

Server validation rejects missing fields and placeholder text: anything in
angle brackets (`<...>`), and words such as `todo`, `tbd`, `n/a`, `test`,
`placeholder`, `reason`, `purpose`, `safe`, `none` or `...` used as the whole
value. Validation checks the shape, not the truth: write what is true.

## Constraints in `params`

Each parameter takes exactly one operator.

| form | meaning |
|---|---|
| `"env": "prod"` or `{"eq": "prod"}` | Exact value. Prefer it. |
| `{"in": ["api", "worker"]}` | One of these values (a non-empty list). |
| `{"prefix": "release/"}` | A string starting with this (string parameters only). |
| `{"gte": 1, "lte": 6}` | A number in this range. Give both bounds (numeric parameters only, within the tool's own schema limits). |
| `{"limit": "merge commit of PR 218", "pattern": "^[0-9a-f]{7,40}$"}` | A value that does not exist yet. Toolyard checks the real value against `pattern` when you call. `pattern` is required. |
| `{"any": true}` | Unbounded: rejected. |

Rules the server enforces:

- Include every parameter the tool requires; a missing one is a problem.
- Exact values must match the tool's input schema.
- Do not put `_reason` or `_grant` in `params`: they are not scope.
- Numbers must survive a browser's JSON parser (at most 2^53 - 1 and exactly
  representable). Use a string ID where the tool allows it.
- At call time, an argument that was not in `params` blocks the call, and a
  missing constrained argument blocks it too.

## Urgency

| urgency | use when |
|---|---|
| `now` | Production is broken, or money or data is being lost right now. |
| `soon` | You will be blocked within the hour. |
| `digest` | You need it today, not this hour. |
| `fyi` | No action needed. Updates only. |

Each agent gets 3 `now` items an hour by default. Past that, toolyard lowers
the item to `soon`, shows the owner it was lowered, and returns a warning with
`path: "urgency"`. The owner's settings decide when their phone notifies them.

## Attachments

Every attachment has a `type` and an optional first-person `caption` (at most
300 characters). A chart also takes `spoken` (at most 120 characters): the
words your voice note uses for it.

| type | fields | limits and notes |
|---|---|---|
| `markdown` | `body` | At most 20,000 bytes. CommonMark with GitHub tables, fenced code, lists, quotes, http(s) and mailto links. Raw HTML shows as text. |
| `table` | `title`, `columns`, `rows` | 1-12 columns, 1-200 rows, each row as long as `columns`. |
| `chart` | `title`, `chart`, `unit`, `x`, `series: [{name, values}]` | `chart` is `line`, `bar`, `stacked_bar`, `area`, `pie` or `scatter`. 1-6 series of 1-500 values; `x`, when given, has one label per value. Labels, names and `unit` at most 80 characters. |
| `diff` | `file`, `patch` | A unified diff; at most 60,000 bytes. Keep the part that matters. |
| `code` | `file` or `title`, `language`, `body` | At most 60,000 bytes. |
| `log` | `title`, `body` | At most 60,000 bytes. |
| `image` | `upload` or `url`, `alt` | PNG, JPEG, WebP or GIF; at most 10 MB. |
| `video` | `upload` or `url`, `poster_upload` or `poster_url` | MP4, WebM or QuickTime; at most 50 MB. |
| `file` | `upload` or `url`, `name` | At most 25 MB. |
| `link` | `label`, `url` | An http(s) URL. |

Chart rules: `stacked_bar` and `pie` take values of 0 or more. A `pie` takes
exactly one series of up to 24 slices with a total above 0 and one `x` label
per slice. A `scatter` needs numeric `x` coordinates; each series' values are
the y coordinates.

```json
{"type": "chart", "title": "Where the 5xx errors come from", "chart": "pie",
 "x": ["checkout", "search", "login"], "series": [{"name": "errors", "values": [61, 27, 12]}]}
```

Media takes exactly one of `url` or `upload`. A `url` must be http(s) and
reachable by toolyard: `localhost`, `*.local`, `*.internal` and private IP
addresses fail the dry run unless the operator allows private media. Toolyard
fetches each link once when you send and keeps a copy.

## Uploads

Use `inbox.upload` for a screenshot, diagram, recording or file you hold
locally. You need no public host.

1. Call `inbox.upload` with `name` (at most 200 characters), `content_type`,
   `size` (exact bytes), `sha256` (64 lowercase hex) and `kind` (`image`
   default, `video`, `file`).
2. It returns `upload_url`, `ref`, `expires_at`, `method` (`PUT`), `headers`
   and a ready `curl` command. PUT the exact bytes within 15 minutes. The URL
   works once and is the credential: never paste it anywhere else. When the
   gateway has no public URL set, `upload_url` is a path; prefix it with the
   toolyard address you connect to.
3. Cite the ref: `{"type": "image", "upload": "upload:<sha256>", "alt": "..."}`.
   In Markdown (`message` or a `markdown` body), `![alt](upload:<sha256>)`
   shows inline (up to 24 inline images). Any other image source shows as a
   link.

```text
inbox.upload
{"name": "checkout-after.png", "content_type": "image/png", "size": 48213,
 "sha256": "<64 lowercase hex from sha256sum>", "kind": "image",
 "_reason": "Upload the screenshot that shows the checkout page after the fix"}
```

Toolyard checks the size, the sha256 and the real file type. If you uploaded
the same bytes before, the answer is `already_uploaded: true` with the ref.
Only you can cite your uploads. An upload that no item cites within 24 hours
is deleted. You can hold at most 50 open upload tickets.

## Dry run results

`dry_run: true` sends nothing. The answer has `ok`, `problems` (each with
`path` and `message`), `warnings`, `flags` and a `preview` (`card_title`,
`audio_seconds`, counts).

- Fix every problem: a request with problems is not sent.
- Warnings do not block sending, but the owner notices what they point at.
- Flags are what the owner will see. Rule flags always run; judge flags appear
  only when the owner has a judge model switched on. Add evidence where a flag
  is fair; fix the mistake where it is not.
- Dry runs are limited to 20 an hour per agent and are recorded.
- `schema_version 3 is turned off on this server while it is upgraded` means:
  send version 1 or 2 for now.
