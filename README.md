# toolyard

Self-hosted MCP gateway with **per-tool-call human approval pushed to your
phone** and built-in shared memory. One Go binary + SQLite + a PWA dashboard.

Every coding agent today (Claude Code, Cursor, Codex CLI, web Claude)
reimplements its own MCP config, approval UX, and audit log. `toolyard` sits
between any number of agents and any number of upstream MCP servers, gives
you a single place to enroll servers and approve writes, and pushes approval
cards to your phone with the model's reasoning visible.

## Status

**v0.1 — works end-to-end.** Reads pass, writes hold for human approval,
audit log streams live to the dashboard, Web Push lights up your phone, and
the Claude Code hook lets long approvals resume cleanly. See
[`docs/architecture.md`](docs/architecture.md).

## Quickstart

```bash
go build -o toolyard ./cmd/gateway
./toolyard serve -addr :8787 -data ~/.toolyard
```

Open <http://localhost:8787>, create the local admin account, and:

1. **Agents → Generate code**, get a one-time enrollment code.
2. From your terminal:
   ```bash
   ./toolyard exchange -url http://localhost:8787 -code <code>
   ```
   Save the returned `token`.
3. Point your MCP-aware agent at `http://localhost:8787/mcp` with
   `Authorization: Bearer <token>`. Or run the gateway with `-stdio` for
   stdio-only hosts.
4. (Optional) **Settings → Enable push** in the dashboard to get notifications
   on this device.

The full self-hosting guide is in [`docs/self-hosting.md`](docs/self-hosting.md).

## What's in v0.1

- Stdio + streamable HTTP MCP server, agent enrollment via short-lived
  codes, sha256-hashed long-lived tokens.
- Schema-wrap that injects a required `_reason` field (20–2000 chars) and
  optional `_intent_category` enum into every upstream tool's input schema.
- Hardcoded policy: `read|get|list|search|...` pass through, everything
  else holds for human approval.
- Approval bus with Ed25519-signed decision tokens, durable to SQLite,
  with hybrid in-line wait + deferred-response degradation per the
  architecture plan.
- Built-in memory MCP (`memory.set`, `memory.get`, `memory.list`,
  `memory.delete`) backed by the same SQLite DB.
- Live SSE dashboard: pending approvals, audit feed, agent enrollment,
  memory browser, push setup.
- Web Push (VAPID) with one-tap allow/deny actions in the notification.
- `scripts/claude-code-hook.sh` reference implementation of the
  PostToolUse hook that resumes deferred approvals.
- Multi-stage `deploy/Dockerfile` + `deploy/docker-compose.yml`.

## Verification

End-to-end smoke test (run while the binary is up on `:18787`):

```bash
go test -tags=e2e ./scripts -run TestEndToEnd -toolyard=http://localhost:18787 -v
```

It covers the full happy path: enroll, list tools, read passes, write
holds, dashboard approves, write completes, audit log fills.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
