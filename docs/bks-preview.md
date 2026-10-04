# BKS Kubernetes Preview MCP

This integration is **uninstalled and disabled by default**. Its draft PR is
not an installation, a live Kubernetes deployment, or a working preview URL.
Toolyard queues bounded requests. It never holds branch builds or Kubernetes jobs.

## Fixed catalog

| Toolyard builtin | Control operation | Scope |
| --- | --- | --- |
| `bks_preview.create` | `preview_create` | Queue a same-repository source with a reviewed synthetic snapshot |
| `bks_preview.inspect` | `preview_inspect` | Read owned environment metadata |
| `bks_preview.heartbeat` | `preview_heartbeat` | Report owned real activity without extending the absolute deadline |
| `bks_preview.release` | `preview_release` | Release this caller's job or ownership |
| `bks_preview.snapshots` | `preview_snapshots` | Read the reviewed synthetic catalog |
| `bks_preview.results` | `preview_results` | Read owned durable result receipts, not raw logs |

These are local builtins, not a generic upstream registration. Startup makes no
network request. The reserved `bks_preview` prefix cannot be an external server.
Normal access, `_reason`, policy, approval, grant, audit, metrics, deferred
execution and timeouts still apply. The privileged `CallInternal` route refuses
these tools. No forced policy override is used. Annotations do not grant access.

The current policy name heuristic does not recognize all preview read names.
Default approval is therefore expected. This PR creates no policy or access grant.
The control service's independent ownership and cost gates remain authoritative.

## Disabled startup and private configuration

The `serve` flag `-bks-preview` defaults to false. It has no environment-variable
alternative. A future reviewed installation must explicitly enable it with these
fixed files under the service's own `-data` directory:

- `bks-preview/assertion.key`: private issuer bytes, 32–4096 bytes.
- `bks-preview/bindings.json`: the bounded private operator registry, at most 64 KiB.

Do not place either file in an agent-readable home or a shared runtime directory.
No file, directory, key, account, enrollment or listener is created by this adapter.
Missing, invalid or unsafe configuration leaves the preview catalog absent and
ordinary Toolyard startup available. An empty version-one registry binds nobody
and refuses all callers. Unsupported secure-file platforms remain closed.

Files must be service-owned private regular files with one link and no executable
bits. The immediate directory must be service-owned and private. Every ancestor
must belong to root or the service and deny group/other writes. Descriptor-based
no-follow traversal rejects symbolic links, unsafe ancestors and path substitution.
macOS installations must use a real absolute path, not a symlinked `/tmp` path.
The registry and issuer file are reread and revalidated, not trusted from startup.
Operators must publish private temporary files by atomic rename in the protected
directory. The service and privileged host administrators are trusted issuers.

## Operator identity proof and durable ledger

There is no automatic trusted T3 session map in the inspected Toolyard APIs.
An operator must independently verify the real T3 session and its current owner,
the corresponding Toolyard owner, the dedicated enrolled agent ID, and delivery
of that agent credential only to that session. No real binding exists in this PR.
Do not invent a proof receipt or infer the mapping from an agent name, environment
variable, hook body, dashboard JWT, MCP session header or tool argument.

Each registry binding contains the canonical fields `principal_id`,
`owner_user_id`, `session_id`, `issued_at`, `expires_at`, `credential_mode`,
`proof_sha256` and `approved_by`. The mode is exactly `dedicated-per-session`.
The proof hash names retained private non-secret operator evidence, not a generated
receipt. The approving Toolyard owner ID must equal `owner_user_id`. Validity is
positive and at most six hours. Unknown, duplicate or case-aliased fields are refused.

Only a fully enrolled ordinary agent with an active existing owner is accepted.
Identity-key agents, disabled agents, missing owners and pending enrollments fail.
The strict preview lookup does not change legacy ordinary MCP authentication.

Before approval or grant use, local preflight commits the verified immutable tuple
to `bks_preview_enrollment_ledger` in Toolyard's own SQLite store. Migration
`0100_bks_preview_enrollment_ledger.sql` adds an append-only table. Update, delete
and replacement-insert triggers protect existing rows. The table has no cascading
foreign key, so agent or owner removal cannot remove an enrollment commitment.
No proof contents, token, key or assertion enter that ledger.
Each commitment pins its SQLite connection and verifies `synchronous=FULL`
before the transaction, so a successful WAL commit includes disk synchronization.
The ordinary disabled startup path does not change the SQLite sync setting.

Agent ID, owner ID, T3 session ID, proof hash, mode and approver never change.
Registry expiry may renew only with the identical immutable tuple. Registry removal
revokes requests but does not free the ID. Token rotation retains the same binding.
A new owner or T3 session requires a new agent ID and fresh operator verification.
Deferred approval, restart recovery and every POST check the current registry,
current principal and durable commitment again. Historical audit labels are not
identity authority. The ledger closes the process-memory restart gap; it does not
independently prove T3 ownership.
An approved dispatch also verifies its current persisted status, expiry, agent,
tool and upstream. Its original ledger commitment must already exist and precede
the approval record. A historical generic-upstream approval cannot enroll an ID
for the first time through the new builtin.

Preserve this SQLite ledger and its migration state in reviewed backups. Do not
reset it, restore an older enrollment history, or reuse IDs after loss of that
history. This design trusts privileged database operators; it is not protection
against an administrator who erases the database or alters its triggers.

## Fixed transport and assertions

The only endpoint is `http://127.0.0.1:18791/mcp`. No tool argument or environment
variable can change it. Proxies, redirects and stateful MCP responses are refused.
Each call uses the stateless `2025-11-25` handshake and exactly one fixed control
operation. Responses must be JSON; notification responses must have no body or
session header. Requests are at most 16 KiB, responses at most 1 MiB, connect
timeout two seconds, HTTP timeout five seconds, and whole call timeout ten seconds.

Each POST receives a fresh HS256 assertion with issuer `toolyard`, audience
`bk-agent-test-pilot-preview-v1`, subject `toolyard:agent:<actual-agent-id>`, the
operator-verified T3 session UUID, and `human=false`. Lifetime is at most 30 seconds
and never extends beyond registry expiry. There is no human assertion route,
global privileged JWT, anonymous assertion or static upstream authorization header.
Credentials and assertions never belong in argv, environment variables, logs,
Git, documentation, caller inputs or error bodies. Transport errors are fixed
safe messages, not remote HTTP bodies or raw exceptions.

## Validation and remaining parent gates

The scoped GitHub workflow compiles the full module on hosted Linux and macOS,
then runs focused adapter, gateway, startup and immutable-ledger cases. Linux adds
race checks. The actual pinned BKS Python verifier validates Go assertions; its
test fixtures retain commit and digest provenance. Synthetic test assertions travel
through stdin, not argv or failure output. Local Go compilation and tests are not
part of this task.

CI contract results do not prove a deployed service, private tunnel, real enrollment
or usable preview. Separate parent/operator gates remain for control installation,
private tunnel and transport denial checks, HTTPS/viewer identity, real enrollment
proof, issuer provisioning, operator access/policies and runtime validation.
No service restart/deployment, AWS change, worker activation, production data,
Tailscale handler change or production credential is authorized here.

The parent's AWS 21-resource package and any fresh worker-cost window remain
unapproved. Previous allowances are exhausted. Existing pilot Spot groups remain
zero and fenced closed. A queued result is not build, restore or readiness evidence.
