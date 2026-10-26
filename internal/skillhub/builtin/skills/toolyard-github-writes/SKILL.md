---
name: toolyard-github-writes
description: Propose GitHub pull request comments, inline review comments and reviews (COMMENT, REQUEST_CHANGES, APPROVE) through toolyard's per-user GitHub connector, and collect the result. Every GitHub write needs the account owner's explicit approval; calling create_pull_request_comment, create_review_comment or submit_pull_request_review returns a deferred approval_id, never runs on an Inbox grant or an automatic policy rule, and is collected with tools.poll_approval or tools.wait_for_approval. Use when you must comment on or review a pull request through toolyard, when a GitHub tool answers pending_approval, when a GitHub write was denied or expired or says the pull request, account or connection changed, or when you write the _reason and review body an owner must decide from.
metadata:
  toolyard-runs: anywhere
---

# GitHub writes through toolyard

Toolyard's GitHub connector acts through your owner's own GitHub connection.
Reads work under your owner's normal policy. Every write, an `APPROVE` review
included, waits for the account owner's explicit approval. Nothing posts to
GitHub until the owner allows that exact request.

This skill is guidance, not permission. It never replaces the owner's
decision.

## When to use

- You must comment on a pull request, leave an inline comment, or submit a
  review (`COMMENT`, `REQUEST_CHANGES` or `APPROVE`) through toolyard.
- A GitHub tool answered `status: "pending_approval"` with an `approval_id`.
- A GitHub write came back denied, expired, or with "request fresh approval".

## The tools

The tool names start with the name the GitHub server has in your catalog;
find them with `tools.search` (for example `{"query": "pull request review"}`).
Below, `<server>` stands for that name.

| tool | kind | arguments besides `owner`, `repo`, `pull_number`, `_reason` |
|---|---|---|
| `<server>.get_pull_request` | read | none |
| `<server>.list_pull_request_files` | read | `page` (100 results per page) |
| `<server>.list_pull_request_comments` | read | `page` |
| `<server>.list_pull_request_reviews` | read | `page` |
| `<server>.list_review_comments` | read | `page` |
| `<server>.create_pull_request_comment` | write | `body` |
| `<server>.submit_pull_request_review` | write | `body`, `event` (`COMMENT`, `REQUEST_CHANGES`, `APPROVE`) |
| `<server>.create_review_comment` | write | `body`, `path`, `line` (positive), `side` (`LEFT` or `RIGHT`) |

No other argument is accepted (`unsupported GitHub argument`). `pull_number`
is a positive integer. A `body` holds 1 to 60000 bytes. Writes work only on an
open pull request. Reads usually answer at once; if your owner's policy makes
a read need approval, it too answers with a deferred `approval_id` that only
the account owner decides.

## Why the owner always decides

- GitHub writes never run on Inbox grants. Do not put them in
  `inbox.request`: a `_grant` cannot run them, and a call with one still
  creates an owner approval.
- Explicit allow rules, a declared read intent and automatic approval rules
  cannot skip the request. An explicit deny rule still blocks the call.
- The request is personal: only the account owner sees and decides it, in
  Approvals. It does not go to shared chat channels, and other users cannot
  see it, its result or its audit events.
- The approval card shows the repository, the pull request, the exact text,
  the review event, the GitHub account and the commit. Your `_reason` is shown
  verbatim to the approver.

## The recipe

1. **Read first.** Use the read tools to see the pull request, its files, its
   comments and its reviews. Note the head commit from `get_pull_request`.
2. **Write the proposal.** Write the `body` exactly as it must appear on
   GitHub, and a `_reason` the owner can decide from (see below).
3. **Call the write tool once.** The answer is deferred: `status:
   "pending_approval"`, `approval_id` (`ap_…`), `expires_at` and
   `reason_to_human`. Nothing is posted yet. (With an operator's in-line
   wait above 0, the call may instead stay open and return the final result
   or the denial if the owner decides within that wait. Handle both.)
4. **Tell your user** that the owner must allow it in toolyard Approvals.
   Continue other work, or end the turn.
5. **Collect the result** with `tools.poll_approval` (non-blocking) or
   `tools.wait_for_approval` (blocks up to 300 seconds). Use
   `tools.poll_approvals` or `tools.wait_for_approvals` for several. When
   allowed, toolyard sends the write itself and saves the result: `status`
   `executed` with `result.content` and `result.structured_content`.
6. **Never re-issue the write.** The poll answer is the result. Re-calling the
   write tool is not how you get it.

```text
<server>.submit_pull_request_review
{"owner": "acme", "repo": "api", "pull_number": 218, "event": "APPROVE",
 "body": "Approving at head 7c1d2e9.\n\nWhat I checked:\n- All 6 changed files, including the retry loop in handlers/webhook.go.\n- CI: 42 of 42 checks passed on this commit.\n\nFindings: none blocking. One nit: the backoff constant could be named.\n\nRisk: failed webhook deliveries now retry up to 3 times; no schema change.",
 "_reason": "Propose an APPROVE review on acme/api PR 218 at head 7c1d2e9 after reviewing all 6 files; CI is green and the user asked me to approve it if clean."}
```

```text
tools.wait_for_approval
{"approval_id": "ap_…", "timeout_seconds": 120,
 "_reason": "Collect the result of the review the owner was asked to allow"}
```

## Write a proposal the owner can decide from the card

The owner sees the card, not your conversation. Make the card enough.

`_reason` (20-2000 bytes, shown verbatim): one or two sentences.

- What you propose: the event (or comment) and the pull request.
- The commit you reviewed, so it matches the commit on the card.
- Why: what you checked, the result, and who asked for it.

`body` (posted verbatim under the owner's GitHub account):

- Lead with the verdict and the commit it applies to.
- List what you checked (files, tests, CI) and the result.
- List findings, each with its file and why it matters. Mark which ones block.
- For `APPROVE`, state the risk and what merging changes.
- Keep it true, specific and public-safe: no secrets, tokens or private data.
- Do not add your own hidden markers. Toolyard appends one to detect
  duplicates.

Prefer `COMMENT` when you are unsure, `REQUEST_CHANGES` when a finding must
be fixed, and `APPROVE` only after you reviewed the current head.

## After the decision

- **Executed.** The write is done. Report it with the link or ID from
  `result`. If `result.is_error` is true, read the error; do not retry
  blindly.
- **"request fresh approval".** The pull request head, the GitHub account or
  the connection token (a reconnect or a token refresh) changed before the
  write was sent. Nothing was posted. Read
  the pull request again, check that your text still applies to the new head,
  then make one new proposal.
- **Denied.** Do not send the same call again. Tell your user what the owner
  declined, and ask for another way forward.
- **Expired.** Nobody decided before `expires_at` (by default 3 hours after
  the call; the operator can change this). Nothing was posted. If it is still
  relevant, call the write tool again to create a fresh request.
- **No longer needed.** You cannot withdraw a GitHub request:
  `tools.cancel_my_approval` answers `only the account owner can decide this
  GitHub request`. Ask the owner to deny it, or let it expire.
- **outcome_unknown.** The write may have reached GitHub. Read the pull
  request's comments or reviews before anything else. Never repeat it.

Statuses, answer fields and every error message:
[approval states and errors](references/approval-states-and-errors.md).

## Pitfalls

- Re-calling an identical write (same arguments, same head commit) while its
  request is pending joins the same request; it never speeds up the decision.
  Do not do it.
- `tools.list_my_pending_approvals` finds lost approval IDs; do not re-issue a
  write to find out where it stands.
- A connection error ("connect your GitHub account in My connections") means
  your owner must connect GitHub in toolyard. Tell them; never ask for a token.
- Every write is pinned to the head commit seen when you proposed it. A new
  push before the owner allows it stops the write.
- To be told about the decision instead of polling, add `_callback_ref` to the
  write call (the tool schema does not list it; toolyard reads and strips it).
  See the `toolyard-callbacks` skill
  (`skills.get toolyard/toolyard-callbacks`).
- Inbox permission flows, legacy approvals in general and `outcome_unknown`
  handling for other tools: see the `toolyard-permissions-and-inbox` skill
  (`skills.get toolyard/toolyard-permissions-and-inbox`).
