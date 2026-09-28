# toolyard — owner inbox and scoped grants: design spec

Status: **implemented (v1)**. Where this early spec differs from the code
(table layout, tool names in §7, digest scheduling, biometric confirmation),
[agent-onboarding.md → What shipped in v1](agent-onboarding.md#what-shipped-in-v1)
and [agent-protocol.md](agent-protocol.md) are authoritative. Decision records: [ADR 0005](../adr/0005-scoped-grants.md), [ADR 0006](../adr/0006-coach-dont-queue.md). How agents learn the protocol: [agent-onboarding.md](agent-onboarding.md).
Principles: [principles.md](principles.md). Agent rules:
[agent-protocol.md](agent-protocol.md).

## 1. Problem

The owner runs 10+ agents at once, on a laptop, in cloud sandboxes and on
remote runners. Today toolyard can hold a restricted tool call and push an
approval card, but:

1. **Approval is the only way for an agent to reach the owner.** Questions,
   "PR ready", blockers and outcomes go through other channels, or nowhere.
2. **There's no view of the sessions.** The owner can't see which of the
   ten sessions are working, waiting, stuck or finished.
3. **Approving runs the call immediately.** The gateway runs the exact call
   when the owner taps Approve. That suits one-off writes but not multi-step
   jobs ("deploy, watch health, retry once"), where the agent should hold a
   bounded permission and use it when it's ready.
4. **Every approval interrupts the same way.** There's no notion of
   urgency, grouping, digest or quiet hours.

## 2. Goals / non-goals

**Goals**

- One inbox holding everything agents send the owner, readable on iPhone
  and actionable from Apple Watch.
- Approving a request issues a **scoped grant** that the agent uses through
  the gateway.
- A live list of agent sessions with their status.
- A notification policy that protects the owner's attention.

**Non-goals (for now)**

- Multiple approvers or teams: this is a single-owner product.
- A native iOS/watchOS app: PWA + Web Push first (see §9 for the
  conditions that would change this).
- Handing out raw credentials: that's broker mode, later and opt-in (§5.6).

## 3. Concepts

```
 agent session ──posts──► inbox item ──(if kind=approval, on approve)──► grant
      ▲                      │                                            │
      └──── status ──────────┘◄──────── outcome update ◄── redeemed via ──┘
                                                          the gateway
```

- **Agent session**: one piece of work by one agent (one Claude Code
  session, one Cursor task). An enrolled agent (`agents` table) can run
  many sessions.
- **Inbox item**: anything that needs, or might need, the owner's
  attention. Its kind is one of `approval | question | review | blocker |
  update`.
- **Grant**: a signed, scoped, bounded, revocable permission, issued when
  an `approval` item is approved.

## 4. Data model

New migrations (next free number, currently `0021_*`). Kept
Postgres-portable like the rest of the schema.

```sql
-- One row per unit of agent work. Heartbeats keep it alive.
CREATE TABLE agent_sessions (
  id                 TEXT PRIMARY KEY,          -- 'ses_'+uuid
  agent_id           TEXT NOT NULL REFERENCES agents(id),
  title              TEXT NOT NULL,
  host               TEXT,                      -- hostname / 'cloud:<provider>'
  repo               TEXT,
  branch             TEXT,
  status             TEXT NOT NULL,             -- working|waiting_on_owner|blocked_on_owner|idle|done
  status_note        TEXT,
  last_heartbeat_at  INTEGER NOT NULL,
  started_at         INTEGER NOT NULL,
  ended_at           INTEGER
);
CREATE INDEX idx_agent_sessions_live ON agent_sessions(status, last_heartbeat_at)
  WHERE ended_at IS NULL;

-- Everything the owner might look at. approval_requests stays the
-- source of truth for the approval state machine; inbox_items is the
-- owner-facing projection plus the non-approval kinds.
CREATE TABLE inbox_items (
  id                 TEXT PRIMARY KEY,          -- 'ib_'+uuid
  kind               TEXT NOT NULL,             -- approval|question|review|blocker|update
  agent_id           TEXT NOT NULL,
  session_id         TEXT REFERENCES agent_sessions(id),
  approval_id        TEXT REFERENCES approval_requests(id),  -- kind=approval
  grant_id           TEXT REFERENCES grants(id),             -- kind=update closing a grant
  title              TEXT NOT NULL,             -- ≤ 60 chars (enforced)
  summary            TEXT NOT NULL,             -- ≤ 200 chars
  body_md            TEXT,                      -- why / blast radius / rollback
  links              TEXT,                      -- JSON [{label,url}]
  options            TEXT,                      -- JSON, kind=question, ≤ 4
  answer             TEXT,                      -- JSON, kind=question
  requested_urgency  TEXT NOT NULL,             -- now|soon|digest|fyi (from agent)
  urgency            TEXT NOT NULL,             -- effective, after policy (§6)
  risk               TEXT NOT NULL DEFAULT 'normal',  -- normal|high (drives step-up)
  status             TEXT NOT NULL,             -- open|snoozed|resolved|expired|cancelled
  snooze_until       INTEGER,
  seen_at            INTEGER,
  resolved_at        INTEGER,
  resolved_note      TEXT,                      -- owner's note back to the agent
  created_at         INTEGER NOT NULL,
  expires_at         INTEGER
);
CREATE INDEX idx_inbox_open ON inbox_items(status, urgency, created_at)
  WHERE status IN ('open','snoozed');
CREATE INDEX idx_inbox_session ON inbox_items(session_id, created_at);

CREATE TABLE grants (
  id                 TEXT PRIMARY KEY,          -- 'gr_'+uuid
  approval_id        TEXT NOT NULL REFERENCES approval_requests(id),
  agent_id           TEXT NOT NULL,             -- binding: only this agent may redeem
  session_id         TEXT,
  scope              TEXT NOT NULL,             -- JSON, see §5.1 (as approved, not as requested)
  requested_scope    TEXT NOT NULL,             -- JSON, for audit
  max_uses           INTEGER NOT NULL,
  uses               INTEGER NOT NULL DEFAULT 0,
  not_before         INTEGER NOT NULL,
  expires_at         INTEGER NOT NULL,
  token_hash         TEXT NOT NULL,             -- sha256 of the token
  status             TEXT NOT NULL,             -- active|exhausted|expired|revoked|released
  revoked_at         INTEGER,
  revoked_reason     TEXT,
  outcome_item_id    TEXT,                      -- the 'update' that closed the loop
  created_at         INTEGER NOT NULL
);
CREATE INDEX idx_grants_agent_active ON grants(agent_id, status) WHERE status = 'active';

CREATE TABLE grant_redemptions (
  id                 TEXT PRIMARY KEY,
  grant_id           TEXT NOT NULL REFERENCES grants(id),
  tool_name          TEXT NOT NULL,
  arguments_hash     TEXT NOT NULL,
  result_is_error    INTEGER,
  redeemed_at        INTEGER NOT NULL
);
```

## 5. Scoped grants

### 5.1 Scope shape

```json
{
  "tools": ["deploy.run"],
  "args": {
    "service": { "eq": "api" },
    "env":     { "eq": "prod" },
    "ref":     { "in": ["abc123"] },
    "replicas":{ "lte": 6 }
  }
}
```

- `tools`: wrapped catalog names. They must all be on the same upstream.
- `args`: constraints per argument. Operators: `eq`, `in`, `prefix`,
  `lte`/`gte`, `any` (explicitly unconstrained). **Arguments not listed
  are pinned to the value in the original request.** Loosening a scope has
  to be explicit, so a grant never silently covers arguments nobody looked
  at.
- One-off approval (today's behaviour) is just the degenerate case: one
  tool, every argument `eq`, `max_uses = 1`.

### 5.2 Token

```
tyg_<grant_id>.<base64url(ed25519_sig)>
sig = Sign(k_grant, "toolyard-grant-v1\n" || grant_id || "\n" || agent_id || "\n" || expires_at)
```

- Signed with a new `server_keys` purpose, `grant_sign`, separate from
  `approval_sign`, so rotating one key doesn't invalidate the other kind
  of token.
- Only `sha256(token)` is stored. The plaintext is returned exactly once,
  to the agent that owns it, through the wait/poll tools.
- The signature doesn't encode the scope. The scope is always read from
  the DB, so the owner can narrow or revoke a grant after it's issued.

### 5.3 Redemption (the gateway's hot path)

The schema-wrap layer adds an optional `_grant` property next to
`_reason` and `_approval_id` (`internal/gateway/schema_wrap.go`). On
`tools/call`:

1. Remove `_grant` from the args along with the other meta fields.
2. Verify the signature, look up the grant, then check:
   `status = active`, `agent_id = caller`, `not_before ≤ now < expires_at`,
   the tool is in `scope.tools`, and the args satisfy `scope.args`.
3. `UPDATE grants SET uses = uses + 1 WHERE id = ? AND status = 'active'
   AND uses < max_uses`. If no row changed, the grant has been used up.
   This atomic update is what enforces use limits under parallel calls.
   Set `status = exhausted` once `uses = max_uses`.
4. Policy returns **allow** with `RuleID = grant:<id>`. Dispatch, write a
   `grant_redemptions` row, and audit `grant.redeemed`.
5. On any failure, return a structured error (`grant_expired`,
   `grant_scope_mismatch`, and so on) and **do not fall back to a new
   approval request**. The agent must decide whether to ask again.

Policy order becomes: forced builtins → **valid grant** → explicit tool
policy → … (the rest as today in `internal/policy`). An explicit `deny`
policy still wins over a grant; check it before step 3 so a denied call
doesn't use up a grant.

### 5.4 Lifecycle

```
approval_requests: pending ──approve──► allowed ──► (grant issued)
                           ├─deny────► denied
                           ├─expire──► expired
                           └─cancel──► cancelled

grants: active ──uses==max──► exhausted
               ├─now≥exp────► expired
               ├─owner──────► revoked
               └─agent──────► released
```

The existing approval-signing token (`decision_token`, used by
one-tap push actions) is unchanged. It authorises the *owner's decision*;
the grant authorises the *agent's action*.

### 5.5 What the agent sees

`tools.poll_approval(s)` / `tools.wait_for_approval(s)` return, for
`status = allowed`:

```json
{ "status": "granted",
  "grant": { "id": "gr_…", "token": "tyg_…", "scope": {…},
             "max_uses": 1, "expires_at": "…" },
  "owner_note": "narrowed to service=api" }
```

The token appears **only in the first response after it's issued**, and
only to the owning agent. Later polls show the grant without the token.
If the agent loses it, it asks again (and the owner can see that it did).

### 5.6 Broker mode (later, opt-in per upstream)

For tools that must run inside the agent's sandbox (a CLI, `kubectl`,
`terraform`), an upstream can declare a **credential minter**: a function
that creates a short-lived credential limited to the grant's scope (for
example, a GitHub App installation token restricted to one repo with
`contents:write` for 10 minutes). Redeeming the grant returns that
credential instead of making a call. The grant is always `risk=high` and
never auto-approved.

## 6. Notification policy

**Effective urgency** = `min(requested_urgency, cap)`, where the cap comes
from:

- **Per-agent `now` budget**: 3 per hour by default. Anything over it
  becomes `soon`, and the item is labelled "downgraded".
- **Kind caps**: `update` and `review` can never be higher than `digest`.
- **Quiet hours** (owner setting): only `now` items on tools in the
  `quiet_hours_allow` list break through; everything else is held until
  quiet hours end.

| urgency | push | grouping | watch |
|---|---|---|---|
| `now` | immediately | none | yes |
| `soon` | after a 90 s grouping window | one notification per session: "3 requests from *Ship billing v2*" | yes |
| `digest` | none; in the next digest (default 09:30 / 13:30 / 18:30) | one digest notification | digest only |
| `fyi` | never | — | no |

**Reminders**: an `approval` or `question` whose session is
`blocked_on_owner` gets **one** reminder after `max(15 min, p50 decision
latency)`. There are never repeat reminders; the item turns amber in the
inbox instead.

**Push de-duplication**: use `tag = session_id` for grouped pushes, so a
new push replaces the earlier one instead of stacking. `tag = item_id`
for `now`.

## 7. Agent-facing tools (MCP)

**Superseded.** The agent-facing tools are now the `inbox.*` set in
[agent-onboarding.md](agent-onboarding.md#new-tools-all-under-inbox), with the
request format in [agent-protocol.md](agent-protocol.md). The main change from
the earlier draft ([ADR 0006](../adr/0006-coach-dont-queue.md)): calling a
restricted tool without a grant **no longer creates a request**. It returns a
`permission_required` result with a pre-filled draft, and only
`inbox.request` / `inbox.ask` reach the owner's inbox.

**Session binding.** If an agent passes no `session_id`, items attach to
that agent's most recent live session, or an automatic
`"<agent name> (unnamed session)"` if it has none.

## 8. Owner-facing surfaces

### 8.1 REST (for the PWA)

```
GET  /v1/inbox?status=open&kind=&session=      list, sorted: urgency, blocked-first, age
GET  /v1/inbox/{id}
POST /v1/inbox/{id}/approve    {scope?, ttl_seconds?, max_uses?, note?}   → issues grant
POST /v1/inbox/{id}/deny       {note?}
POST /v1/inbox/{id}/answer     {option_id | text}
POST /v1/inbox/{id}/snooze     {until}
POST /v1/inbox/{id}/resolve    {note?}                                    (review/update/blocker)
POST /v1/inbox/batch           {ids[], action}                            (normal risk only)
GET  /v1/sessions?live=1
GET  /v1/grants?status=active
POST /v1/grants/{id}/revoke    {reason?}
POST /v1/grants/revoke-all     {agent_id?}                                (kill switch)
POST /v1/inbox/decide-by-token {token, action}   (extends the existing push one-tap endpoint)
```

The existing SSE hub gets `inbox.*`, `session.*` and `grant.*` events.

### 8.2 iPhone (PWA)

Three tabs:

1. **Needs you**: `approval`, `question` and `blocker` items that are open.
   Blocked sessions first, then by urgency, then oldest first.
2. **Sessions**: one row per live session: title, agent, host, status
   badge, time since last heartbeat, and a count of open items. Tap to see
   that session's timeline (items, grants, outcomes).
3. **Updates**: `review` and `update` items, plus the digest.

Approval card:

- Title, agent · session · host.
- Summary, then *Why*, *Blast radius* and *Rollback*, collapsed on the
  watch and expanded on the phone.
- **Scope editor**: shows the requested scope; the owner can narrow it
  (drop tools, tighten args, fewer uses, shorter TTL) but never widen it.
- Links (PR, diff, CI).
- **Approve** / **Deny with note** / **Snooze** (1 h, tonight, tomorrow).
- High-risk items need a **passkey (WebAuthn) confirmation** before
  Approve.

Grants view: active grants with uses left and a countdown, Revoke on each,
and the kill switch.

### 8.3 Apple Watch

Through the iPhone's mirrored Web Push notifications. The rules:

- Notification title = item `title`. Body = `summary` + a one-line scope
  (`api@abc123 → prod · 1 use · 30m`).
- Actions:
  - `approval`, normal risk: **Approve**, **Deny**. Snoozing is
    swipe-to-dismiss; the item stays in the inbox.
  - `approval`, high risk: **no Approve action**. The body ends with
    "Open on iPhone to approve", because a passkey step can't be done from
    a notification.
  - `question` with ≤ 2 options: one action per option. With more
    options, tap through to the phone.
  - Everything else: no actions.
- `risk = high` is set when: the tool is `is_destructive`, any scope
  argument matches `env ∈ {prod, production}`, the grant is broker-mode,
  or the operator marked the tool high risk.

**Check on real devices before building on these assumptions:**

1. Whether iOS Web Push shows notification `actions` at all, and whether
   they're mirrored to the Watch. If not, a tap deep-links to the card on
   the phone. The design still works, just with one more tap.
2. Web Push has no equivalent of APNs `interruption-level:
   time-sensitive`. So `now` can't break through Focus; the owner has to
   allow the toolyard PWA in their Focus settings.

## 9. When to reconsider native

ADR 0001 plans a native app for v0.3. Build it earlier only if, after
Phase 3, at least one of these holds: the Watch can't show action buttons
(§8.3 check 1), `now` items are being missed because of Focus (check 2),
or approving high-risk items on the phone is so slow that the owner starts
approving them less carefully.

## 10. Migration from gateway-runs-it-on-approval

Today, approving runs the call immediately (migration 0010,
`result_envelope`). Planned sequence:

1. Ship grants **alongside** the current behaviour, behind a setting
   `approval_mode = execute | grant` (default `execute`).
2. In `grant` mode, a restricted call without a grant returns the
   `permission_required` coaching result (ADR 0006) instead of queuing an
   approval. Grants come only from `inbox.request`, and the agent redeems
   them by calling the tool with `_grant`.
3. Update `scripts/claude-code-hook.sh` and the tool descriptions
   (`descriptionBanner`, the wait/poll tools) for the grant flow.
4. Change the default to `grant`. Remove `execute` after one release.

## 11. Phases

| phase | scope | done when |
|---|---|---|
| 1 — Grants | `grants` tables, token, `_grant` redemption, policy order, `approval_mode`, revoke + kill switch | e2e: approve → token → redeem once → second redeem fails |
| 2 — Inbox + sessions | `inbox_items`, `agent_sessions`, `inbox.post`/`inbox.wait`/`session.*`/`tools.request_grant`, REST + SSE | ten agents on three hosts show up correctly in the session list |
| 3 — Attention | urgency caps, grouping, digest, quiet hours, one reminder, PWA tabs, scope editor | a day of normal use produces fewer than N pushes (N set by the owner) |
| 4 — Trust | passkey confirmation for high risk, Watch formatting, answering from notifications, agent-protocol skill shipped | high-risk approvals can't be completed from a notification |
| 5 — Broker | credential minters per upstream (GitHub App first) | a CLI in a sandbox gets a 10-minute, one-repo token |

## 12. Open questions

1. Default and maximum grant TTL. Proposed default 30 min, maximum 24 h,
   configurable per tool.
2. Should `question` items expire, or wait indefinitely?
3. Digest times: fixed, or learned from when the owner usually opens the
   inbox?
4. Can an agent request `now` for tools outside the quiet-hours list at
   all, or is `now` itself an allowlist?
