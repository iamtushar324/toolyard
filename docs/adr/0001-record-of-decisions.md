# ADR 0001 — v0.1 decisions of record

Lightweight log of decisions taken during the v0.1 build. ADRs 0002+ will be
written as we hit each decision point in v0.2.

## Locked in

- **Name**: `toolyard`. Cleared name-collision check in May 2026.
- **Mobile**: PWA + Web Push for v0.1/v0.2. Native (Flutter + APNs) at v0.3.
- **Go MCP library**: `github.com/mark3labs/mcp-go` (currently `v0.52.0`).
- **v0.1 auth**: single-user local password (argon2id). OIDC at v0.2.
- **SQLite driver**: `github.com/mattn/go-sqlite3` (CGO). We accept the CGO
  build cost in exchange for stable, well-tested SQLite semantics. Will
  re-evaluate `modernc.org/sqlite` if cross-compilation pressure arises.
- **Approval-token signing**: Ed25519 (stdlib `crypto/ed25519`).
- **Realtime fan-out**: SSE only. WebSocket has no advantage for a unidirectional
  feed and adds proxy headaches.
- **Dashboard tooling**: vanilla HTML/JS (no Vite, no React, no shadcn)
  served from `web/dashboard/` via `go:embed`. The plan called for Vite +
  React + shadcn but explicitly said "slip dashboard polish before slipping
  schema-wrap or approval bus." We slipped accordingly. v0.2 can rewrite.

## Deferred

- **Policy DSL**: still hardcoded in `internal/policy/policy.go`. CEL-based
  rule loader lands in v0.2.
- **Multi-user / RBAC**: postponed to v0.3 alongside the Postgres backend.
- **FCM / APNs**: v0.2.
- **Native mobile**: v0.3.
