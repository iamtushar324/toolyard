# Questions, rich items and updates

`inbox.guide({"topic": "questions"})` and `{"topic": "updates"}` serve the
same contract from the server. An answer is information, never permission:
only accepted tool calls get grants.

## Pick the shape

| you need | use |
|---|---|
| Tool approvals, maybe with questions | `inbox.request` (add `schema_version: 3` and `questions`) |
| Several questions of any type | `inbox.ask` with `schema_version: 3` and `questions` |
| One simple question | `inbox.ask` with `schema_version: 2`, `prompt` and `question` |
| To report what happened | `inbox.post` |

Older `inbox.ask` items with `title`, `summary`, `message`, `urgency` and 2-4
`options` (`{label, detail}`) still work; they appear as a single-choice
question with a custom text field. Set `kind: "blocker"` when the question
blocks your progress (default `question`). A question item cannot carry tools.

## Schema version 3 questions

One item carries up to 20 questions (and, on `inbox.request`, up to 50 tool
calls and 24 attachments). The owner answers everything in ONE decision;
drafts and partial answers are never sent to you.

On `inbox.ask` with exactly one question, `title`, `summary` and `message`
may be left out: the question's prompt stands in. With several questions,
give `title`, `summary` and `message` (or a `prompt`). On version 2 and 3
questions, `urgency` defaults to `soon` and audio is optional. A single `question` (or version 1 `options`) sent with version 3
becomes `questions[0]` with the ID `answer`.

Every question has:

| field | rules |
|---|---|
| `id` | Stable and unique: 1-80 letters, digits, `.`, `:`, `_`, `-`, starting with a letter or digit. Answers are keyed by it. `constructor`, `prototype`, `toString` and other JavaScript object names are reserved. |
| `type` | One of the types below. |
| `prompt` | The question in plain words, at most 1000 characters. |
| `help` | Optional, at most 2000 characters. |
| `required` | Default `true`. |
| `default` | Optional pre-filled answer, typed like the answer value. |
| `recommended` | Optional suggestion, shown but never chosen for the owner. |

| type | extra fields | answer `value` |
|---|---|---|
| `free_text` | `min_length`, `max_length` (at most 10000), `multiline` | a string |
| `single_choice` | `options` (2-12), `allow_other` | one option ID |
| `multiple_choice` | `options` (2-12), `min_selections`, `max_selections`, `allow_other` | a list of option IDs |
| `yes_no` | none | `true` or `false` |
| `number` | `min`, `max`, `step` (positive), `unit` (at most 24 characters) | a number in range, a whole number of steps from `min` (or 0) |
| `date` | none | `"YYYY-MM-DD"` |
| `datetime` | none | RFC 3339, e.g. `"2026-10-25T14:30:00+05:30"` |
| `scale` | `scale` (2-10 points, default 5), `min_label`, `max_label` (at most 80 characters each) | a whole number from 1 to `scale` |
| `ranking` | `options` (2-12) | every option ID, most preferred first |

Options are `{"id", "label", "detail"?, "exclusive"?, "recommended"?}`: unique
IDs (at most 80 bytes), unique labels (at most 80 characters), `detail` at
most 200 characters. An `exclusive` option is selected alone (not on a
ranking). With `allow_other`, a choice question also accepts custom text
instead of a selection. Fields that do not belong to a type are rejected.

```text
inbox.ask
{"schema_version": 3,
 "title": "Pick the rollout plan for billing v2",
 "summary": "Three quick choices decide how I roll out the new handler.",
 "message": "The handler is deployed with its flag off. I need your rollout choices.",
 "callback_ref": "cb_…",
 "questions": [
   {"id": "rollout", "type": "single_choice", "prompt": "Which rollout do you want?",
    "recommended": "staged",
    "options": [{"id": "staged", "label": "Staged, 10% a day"}, {"id": "all", "label": "Everyone at once"}]},
   {"id": "notify_team", "type": "yes_no", "prompt": "Shall I tell the team channel when it is live?", "default": true},
   {"id": "start_on", "type": "date", "prompt": "From which day may the rollout start?", "required": false}],
 "_reason": "Ask the owner the rollout choices that block the billing v2 rollout"}
```

Answers come back in `inbox.status`, `inbox.wait` and the decision callback:

```json
{"request_id": "rq_…", "status": "answered",
 "answers": {
   "rollout": {"value": "staged", "labels": ["Staged, 10% a day"]},
   "notify_team": {"value": true, "labels": ["Yes"], "text": "Keep it short."}}}
```

- An unanswered optional question is absent.
- `text` is a custom answer (`allow_other`) or a comment.
- A question item ends `answered`; the legacy `answer` string lists every
  answer as `prompt: answer` lines.
- On an access item the answers sit next to the verdicts. When the owner
  rejects every call, required questions become optional.
- Reading an answered item through `inbox.status` or `inbox.wait` marks it
  retrieved for the owner.

## Schema version 2: one question

```json
{"schema_version": 2,
 "client_request_id": "stable-key-for-this-question",
 "prompt": "Which checks must run?",
 "context": "Choose the checks for this stage release.",
 "question": {"type": "multiple_choice", "min_selections": 1, "max_selections": 2,
   "options": [{"id": "mobile", "label": "Mobile layout"},
               {"id": "keyboard", "label": "Keyboard access"},
               {"id": "none", "label": "Neither", "exclusive": true}]},
 "task": {"title": "Review stage"},
 "blocking": false}
```

Version 2 types are `free_text`, `single_choice` and `multiple_choice`.
`prompt` is at most 1000 characters and `context` at most 3000. Free text has
no options; choice questions take 2-12 options with unique IDs and labels. The
owner can always submit custom text without a selection. `task.url`, when
given, is a public HTTPS URL without credentials. The answer comes back as
`response.selected_option_ids`, `response.selected_labels` and `response.text`,
plus the legacy `answer` string.

## Updates with inbox.post

Required: `title`, `summary`, `message`, `urgency`. Optional: `request_id`
(the request this update closes; it must be one of yours), `audio`,
`attachments`, `session_id`. An update cannot carry tools, options or
questions. `now` and `soon` are lowered to `fyi`. The owner marking it read
sets its status to `read`.

```text
inbox.post
{"request_id": "rq_…", "urgency": "fyi",
 "title": "Billing fix is live in prod, flag still off",
 "summary": "Deployed the merge commit. Errors are flat since.",
 "message": "Done. I deployed the merge commit with the flag off and watched the error rate for 15 minutes.",
 "attachments": [{"type": "chart", "chart": "line", "title": "Error rate after deploy", "unit": "%",
   "x": ["-10m", "-5m", "now"], "series": [{"name": "api", "values": [0.2, 0.2, 0.19]}]}],
 "_reason": "Report the finished deploy to the owner and close the permission request"}
```

## Sessions

`session.start` (`title`, optional `repo`, `branch`, `host`) names the piece
of work so the owner can see it among their agents. Pass the returned
`session_id` on Inbox items, and `_session_id` on other calls for attribution.
`session.update` reports `working`, `waiting_on_owner`, `blocked_on_owner`,
`idle` or `done` (with an optional `note`) and counts as a heartbeat. A
session ID grants no permission.

## Item statuses

| status | meaning |
|---|---|
| `pending` | Waiting for the owner (`snoozed_until` is set when they snoozed it). |
| `approved` | At least one call accepted; only accepted calls have grants. |
| `denied` | Every call rejected, or the item denied. No grants. |
| `returned` | Sent back to replan. Read `owner_note` and say what changed next time. |
| `answered` | A question item was answered. |
| `cancelled` | You withdrew it, or toolyard cancelled it because your access was removed. |
| `expired` | Nobody decided before the deadline. Ask again once, and say it is a re-request. |
| `read` | The owner read your update. |
| `not_found` | No item with this ID belongs to you. |
