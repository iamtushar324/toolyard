# toolyard — agent protocol

How an agent should behave when it works through toolyard. The rules are
written to the agent. They're meant to ship as a skill (see
`internal/skills`) or be pasted into a repo's `CLAUDE.md` / `AGENTS.md`.

> Tools marked **(proposed)** are defined in
> [inbox-spec.md](inbox-spec.md) and don't exist yet. Everything else is
> available today.

---

## The short version

1. Restricted actions go through toolyard. Always. No workarounds.
2. Ask for permission **as early as you know you'll need it**, then keep
   working.
3. Write every request so the owner can decide from a watch in ten
   seconds.
4. Ask for the **narrowest** scope and the **shortest** lifetime that will
   do the job.
5. Don't block unless you have nothing else to do, and say so when you do.
6. Use a grant exactly as scoped, then report what happened.
7. A denial is an answer. Don't ask again without new information.

---

## 1. Identify yourself and keep your session current

When you start, open a session **(proposed: `session.start`)** with:

- `title`: what you're working on, as a human would describe it
  ("Ship billing v2 webhooks").
- `repo`, `branch`, `host`: where you are.

Update it whenever your state changes **(proposed: `session.update`)**:

| status | meaning |
|---|---|
| `working` | making progress, needs nothing |
| `waiting_on_owner` | you have open requests but still have other work to do |
| `blocked_on_owner` | you can't make progress until the owner answers |
| `idle` / `done` | finished, or parked |

This is how the owner sees ten parallel sessions at a glance. A session
that stops updating shows up as stale.

## 2. When to ask

| You need… | Send | Example |
|---|---|---|
| permission to do something restricted | **grant request** | deploy to prod, run a DB migration, send an email to customers |
| a decision between options | **question** (at most 4 options) | "Drop the legacy endpoint now or deprecate for 30 days?" |
| the owner to look at something, no decision needed yet | **review** | "PR #42 is ready for review" |
| help because you're stuck and asking won't fix it | **blocker** | "CI runner has no GPU; can't run eval suite" |
| to report an outcome | **update / done** | "Deployed abc123 to prod; error rate flat" |

Don't send an update for routine progress. Update your session status
instead. The inbox is for things the owner might act on.

## 3. Ask early, then keep working

As soon as your plan includes a restricted step, ask for it, even if that
step is an hour away.

```
plan: implement → test → open PR → deploy to prod
                                      ^ request the prod grant *now*,
                                        with the PR link once it exists
```

Then keep working on everything that doesn't depend on the answer. Check
back at natural checkpoints:

- `tools.wait_for_approvals(ids, mode="any", timeout_seconds=…)` when you
  have run out of independent work.
- `tools.poll_approvals(ids)` between steps. It never blocks.
- **Never** busy-poll in a tight loop. Use the `expected_decision_in_seconds`
  value in the deferred response to pace yourself.
- If you restart, `tools.list_my_pending_approvals` gives you your open
  requests back. Don't send duplicates.

If you truly can't continue, set your session to `blocked_on_owner` and
wait. Don't invent busywork.

## 4. Write the request for a watch

The owner will usually see your request first as a notification on a
watch. Structure it like this:

| field | rule | example |
|---|---|---|
| `title` | ≤ 60 chars; verb + object + target | `Deploy api@abc123 to prod` |
| `summary` | one sentence; what changes for users | `Ships the billing v2 webhook handler behind flag billing_v2 (off).` |
| `why` | why now, why you | `PR #42 approved and green; unblocks the invoice migration.` |
| `scope` | exactly what the grant allows (see §5) | `deploy.run {service: api, ref: abc123, env: prod}`, 1 use, 30 min |
| `blast_radius` | what breaks if this goes wrong | `api only; flag is off so no user-visible change` |
| `rollback` | how to undo, concretely | `deploy.run ref=9f1e2d0 (current prod)` |
| `links` | evidence | PR, diff, CI run, dashboard |
| `urgency` | honest; see §6 | `soon` |

Rules:

- **Lead with the consequence, not the mechanism.** "Emails 1,204
  customers" beats "calls sendgrid.send_batch".
- **Numbers over adjectives.** "3 rows" not "a few rows".
- **Link to the evidence**, don't paste it. The phone renders links; the
  watch shows the title and summary.
- `_reason` (already required on every call) must say *why this call*,
  not repeat the tool description.
- If the request is part of a batch ("deploy 3 services"), send one
  request with a combined scope, not three notifications.

## 5. Ask for the narrowest scope

A grant covers a tool (or a few tools) plus constraints on the arguments.
Ask for as little as will do:

| prefer | over |
|---|---|
| `ref = abc123` | any ref |
| `env = prod`, `service = api` | any env, any service |
| `max_uses = 1` | unlimited |
| `ttl = 30m` | the default maximum |
| one grant per intent | one grant that "covers the rest of today" |

If you need to retry (a flaky deploy), ask for `max_uses = 2` up front and
say why. Don't ask for a new grant after using the first one up.

The owner can narrow your scope before approving. Always check the grant
you get back. **The scope you receive is the one that counts, not the one
you asked for.**

## 6. Urgency is a promise

| urgency | use when | owner experience |
|---|---|---|
| `now` | production is broken or something is losing money/data right now | immediate push, even in quiet hours (allowlisted tools only) |
| `soon` | you'll be blocked within the hour | push, grouped with other requests from your session |
| `digest` | you need it today, not this hour | no push; delivered in the next digest |
| `fyi` | no action needed | inbox only |

toolyard rate-limits `now` per agent and **downgrades** requests that
overuse it. Crying wolf costs you the owner's attention later.

## 7. Using a grant

Once your request is approved, the wait/poll tools return a grant:

```json
{ "status": "granted",
  "grant": { "token": "tyg_…", "scope": { … }, "max_uses": 1,
             "expires_at": "2026-09-28T18:30:00Z" } }
```

Then:

1. **Check the scope.** If it's narrower than you asked for, work within
   it or ask again, explaining why the narrower scope isn't enough.
2. **Pass the grant with the call**: add `_grant: "tyg_…"` to the tool
   arguments, next to `_reason`. toolyard checks it and runs the call.
3. **Treat the token as a secret.** Don't log it, commit it, pass it to
   another agent, or paste it into a PR. It only works for your agent
   identity anyway, so passing it on is pointless as well as forbidden.
4. **Report the outcome (close the loop)** by posting an `update` that
   links to the grant **(proposed: `inbox.post`)**: what happened, and how
   you checked it.
5. If you no longer need an unused grant, release it **(proposed:
   `grants.release`)** so it stops showing up as outstanding.

## 8. When the answer is no, late or never

- **Denied**: read the owner's note. Change the plan. Ask again only with
  new information, and say what changed ("re-request: scope narrowed to
  `service=api` per your note").
- **Expired**: the owner didn't get to it. If you still need it, ask again
  once and mention it's a re-request. Don't raise the urgency just because
  it expired.
- **Cancelled by you**: use `tools.cancel_my_approval` as soon as a
  request no longer applies (the plan changed, someone else did the work).
  A stale request wastes the owner's attention.
- **Revoked grant**: stop at once and report where you stopped.

## 9. Never

- Never get around a restricted action: no other credentials, no direct
  API calls, no asking another agent to do it, no rewriting a restricted
  call to look like a read.
- Never mislabel `_intent_category` to avoid review.
- Never split one risky action into small calls to stay under a threshold.
- Never pretend you're blocked (or not) to change how you're prioritised.

---

## Reference: toolyard tools for agents

| tool | status | purpose |
|---|---|---|
| `tools.poll_approval(s)` | available | non-blocking status check |
| `tools.wait_for_approval(s)` | available | block ≤ 300 s; `mode=any\|all` for fan-in |
| `tools.list_my_pending_approvals` | available | recover after restart |
| `tools.cancel_my_approval` | available | withdraw a request you no longer need |
| `tools.approval_stats` | available | how quickly the owner usually decides |
| `tools.request_grant` | proposed | explicit, richly described grant request |
| `inbox.post` | proposed | question / review / blocker / update / done |
| `inbox.wait` | proposed | wait on any inbox items (questions and grants) |
| `session.start` / `session.update` | proposed | register and report session state |
| `grants.release` | proposed | give back an unused grant |
