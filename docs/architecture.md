# toolyard — architecture (v0.1)

A self-hosted MCP gateway with per-tool-call human approval, signed-token
phone push, audit log, and built-in shared memory. One Go binary + SQLite +
a vanilla SPA dashboard served from the same process.

```
   Claude Code ──stdio──┐
   Cursor      ──HTTP──►│   ┌────────────┐    ┌──────────────────────┐    ┌──────────────┐
   Codex CLI   ──stdio──┤──►│  Gateway   │◄──►│    Policy Engine     │───►│ GitHub MCP   │
   Web Claude  ──HTTP──►│   │  Core (Go) │    └──────────────────────┘    │ Filesystem   │
                        │   │            │    ┌──────────────────────┐    │ Custom MCPs  │
                        │   └─────┬──────┘    │   Approval Bus       │    └──────────────┘
                        │         │           └─────────┬────────────┘
                        │         ▼                     ▼
                        │   ┌────────────┐   ┌─────────────────────┐
                        │   │ Audit Log  │   │  Push Fan-out       │───► WebPush (FCM/APNs v0.2)
                        │   │ (SQLite)   │   │  (signed Ed25519)   │
                        │   └────────────┘   └─────────────────────┘
                        │   ┌───────────┐         ┌──────────────┐
                        │   │ Memory MCP│         │  SSE Hub     │◄──── Dashboard / Mobile PWA
                        │   │ (built-in)│         │              │
                        │   └───────────┘         └──────────────┘
```

## Routing a tool call

1. Agent calls `tools/call name="github.create_issue" args={...}` over stdio
   or streamable HTTP at `/mcp`.
2. Gateway looks up the wrapped tool in its catalog. The `_reason` field
   prepended at startup is now required (20–2000 chars).
3. Schema-wrap layer extracts `_reason` and `_intent_category`, strips them,
   leaves the rest of the args alone.
4. Audit row written: `event_type=call.start`.
5. Policy engine evaluates `(agent, tool, params, intent)`. Explicit tool
   and upstream rules decide first; otherwise names starting with
   `get/list/search/...` pass and the rest go to approval. The declared
   `_intent_category` can only escalate: `write`, `destructive`,
   `external_communication`, `financial` and `privileged_admin` force
   approval even on a read-looking name, while `read` is ignored, so an
   agent can't skip approval by declaring a write a read.
6. **Allow** path: forward to upstream MCP client / built-in handler,
   write `event_type=call.succeeded`, return result.
7. **Approve** path: persist an `approval_requests` row, fan out to push +
   SSE, block in-line up to `inLineWait`. If decision lands inside the
   window, dispatch and return; otherwise return the deferred response
   `_meta.toolyard.deferred=true`. Decision is then driven by the
   dashboard, the mobile push action, or the `claude-code-hook.sh`.

## Storage

SQLite with WAL. Schema is written Postgres-portable so v0.3's multi-tenant
migration is mechanical. Tables (see `internal/store/migrations/0001_init.sql`):

- `users`               local password (argon2id), single user in v0.1
- `agents`              enrolled agents (sha256 token hash)
- `approval_requests`   pending and historical approvals
- `audit_events`        append-only event stream
- `memory_entries`      KV store backing the memory MCP
- `push_subscriptions`  browser PushSubscription rows
- `server_keys`         VAPID + Ed25519 approval-signing keys

## Security model

- Dashboard auth: HS256 JWT cookie, signed with a key persisted at
  `<data-dir>/session.key` (chmod 600).
- Agent auth: `Bearer <agent-token>` over the streamable HTTP MCP transport
  at `/mcp`. Tokens are sha256-hashed at rest; we only return the plaintext
  token at exchange time.
- Approval one-tap from the phone: each approval row carries a base64url
  Ed25519 signature over its ID. The push notification's action button
  POSTs to `/v1/approvals/decide-by-token` with the signed token, which the
  server verifies independent of any session cookie.
- VAPID keys: auto-generated on first run, stored in `server_keys`.

## Failure modes / observed timeouts

- An MCP client that is paused waiting for an approval longer than its
  configured tool-call timeout will receive a transport error. The decision
  still lands in `approval_requests` and the audit log; the agent host can
  resume by re-issuing the call with `_approval_id`.
- If the gateway crashes mid-approval, restarting it picks up pending rows
  on demand (decisions land via REST as before); only in-flight in-process
  waiters lose their notification path, which the deferred-response model
  expects.
