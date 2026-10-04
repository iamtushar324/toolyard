# BKS Preview through a normal MCP upstream

This draft is uninstalled and disabled by default. Contract tests do not prove a
live preview, private route, real enrollment or full Kubernetes job. BKS owns
source resolution, CI dispatch, Kubernetes, databases, leases, results and cleanup.
Toolyard authorizes tool access; BKS independently authorizes environment owners.

## Generic connector

Use a normal HTTP upstream with `auth_mode: per_user` and an `assertion_profile`
identifier. This selects a reusable agent assertion provider instead of OAuth.
Ordinary OAuth and identity-header forwarding remain unchanged. Assertion profiles
reject static headers, environment overrides, OAuth/PAT and identity forwarding.

The service-owned profile supplies endpoint, signing metadata, protocol version,
bounds and an operation allowlist. There is no BKS-specific Go registration.
`config/preview-mcp.profile.json` defines the pilot configuration:

| Alias under `bks_preview` | Remote operation |
| --- | --- |
| create | preview_create |
| inspect | preview_inspect |
| heartbeat | preview_heartbeat |
| release | preview_release |
| snapshots | preview_snapshots |
| results | preview_results |

Startup registers a reviewed local projection without network access. Each call
initializes authenticated MCP, discovers tools, verifies the allowlist and schemas,
then calls the mapped operation. Unlisted tools never enter Toolyard's catalog.
Normal permissions, `_reason`, policy, approvals, grants, audit, metrics and deferred
execution apply. There is no forced policy. Internal privileged calls are refused.
Annotations do not grant permissions. Default policy can still require approval.

## Default-off profiles and verified identity

`serve -trusted-mcp-profiles` defaults to false. `-bks-preview` is a deprecated alias
for this generic gate; it creates no server. New profile upstreams save disabled.
Missing private configuration does not break ordinary startup.

Future operator installation requires these files below the service data path:

```text
trusted-mcp/<reviewed-profile-id>/profile.json
trusted-mcp/<reviewed-profile-id>/assertion.key
trusted-mcp/<reviewed-profile-id>/bindings.json
```

Files must be service-owned private regular files with one link. The immediate
directory must be private. Ancestors must belong to root or the service and deny
group/other writes. No-follow descriptor traversal rejects symlinks. Unsupported
platforms refuse access. An empty registry authorizes nobody.

Before every POST, the connector rechecks the exact profile digest, enabled upstream
record, registry, actual enrolled AgentID, current active owner and durable ledger.
Profile changes require reviewed reconnect or restart. Caller arguments, names,
hooks, environment variables, dashboard JWTs and MCP session headers are not proof.
No trusted automatic T3 mapping exists in the inspected APIs. An operator must
verify the actual session, owners, dedicated agent and private credential delivery.
This draft creates no real mapping. Never invent verification evidence.

Bindings contain `principal_id`, `owner_user_id`, `session_id`, `issued_at`,
`expires_at`, `credential_mode`, `proof_sha256`, `approved_by`. The mode is
`dedicated-per-session`. The approver equals the owner. Evidence stays private.
Validity is at most six hours. Only enrolled ordinary agents with active owners
pass. Disabled agents, identity-key agents and pending enrollments fail.

Before approval, the append-only SQLite ledger commits the exact immutable tuple
with `synchronous=FULL`. Migration0100 retains its historical
`bks_preview_enrollment_ledger` name so this refactor preserves existing records.
Update/delete/replacement triggers prevent reassignment. Registry removal, owner
removal, restart and credential rotation never release an ID for another session.
Expiry renews only the same tuple. Preserve ledger history in reviewed backups.
Never reuse IDs after lost or restored older history. Host/database administrators
remain trusted. No token, key or proof contents enter the ledger.

Approved execution reloads the persisted request and original commitment. Its
stored expiry bounds the entire handshake and call. Delayed initialization cannot
permit a tools/call after expiry. Migration0101 only adds the generic profile column.
Approvals also store a server-authored canonical profile digest. Reconnect or
replacement with another profile cannot reuse the original approval. Caller
arguments cannot supply this reserved context. It never reaches tools/call.

## Transport and unknown outcomes

The reviewed pilot fixes `http://127.0.0.1:18791/mcp`. Profiles accept loopback HTTP
only. Proxies, redirects and stateful MCP sessions are refused. Each POST receives
a fresh HS256 assertion with issuer `toolyard`, audience
`bk-agent-test-pilot-preview-v1`, verified session UUID and `human=false`.
Assertion TTL is at most 30 seconds and never exceeds registry expiry.

The pilot permits 35 seconds for headers, 40 seconds per HTTP exchange and
65 seconds overall. A shorter approval/context deadline wins. The header bound
accommodates BKS's 30-second source resolver. Connect timeout is two seconds.
Request and response limits are 16 KiB and 1 MiB respectively.

The connector never retries writes automatically. A lost tools/call response can
mean an unknown outcome. Retain the original request ID and exact parameters on
retry. BKS must retain the first immutable resolved source for that ID. Reconnect
must not substitute a new ID or source. A queued receipt is not readiness evidence.

Credential reflection checks reject fragments, combined text blocks and decoded
JSON before results reach callers, audit or approval caching. Remote error bodies
and private diagnostics never pass through. Keys, assertions and credentials must
not enter argv, environment variables, Git, logs or documentation.

## Cloud validation

The hosted workflow compiles the complete Linux/Darwin module and runs focused
gateway, registry, restart, revocation, approval and transport tests. Linux adds
race checks. Real interoperability tests use pinned BKS `build_app`,
`IdentityMiddleware`, Python MCP SDK, synthetic keys, MemoryStore and a fake
same-repository resolver. The bounded loopback subprocess runs only in cloud CI.
No AWS client or production data is used. Fixture hashes record provenance.

## Disposable installation and rollback

This future operator procedure does not authorize activation:

1. Obtain the reviewed CI artifact for the exact accepted commit.
2. Use a separate service-owned disposable data directory with a private hierarchy.
3. Copy the reviewed profile as `profile.json`. Provision issuer material privately.
4. Keep the registry empty until an operator verifies real enrollment evidence.
5. Add a normal HTTP/per_user upstream with the exact profile URL and profile ID.
6. Keep it disabled until the parent approves the private BKS route and runtime.
7. Apply separately approved permissions and policies before activation.

For rollback, disable the upstream first. Remove its record if required. Stop only
the authorized disposable instance. Preserve its ledger and identity history.
Revoke bindings without erasing history or reassigning agent IDs. These steps do
not authorize production installation or shared-service changes.

## Remaining parent gates

The concrete BKS `ci_control_installation.dependencies()` factory is missing.
Runtime wiring, private route guard, supervisor, watchdog, alerts, safe snapshot
publication/restore, authenticated UI and physical cleanup remain unproved.
Durable GHCR pull credentials or refresh remain unverified; current preview CI
uses temporary `GITHUB_TOKEN` authentication.

The parent must retain the development Clerk tenant, same-repository sources,
Blacksmith ARM64 builds and CI deployment. BKS edits require its owner's isolated
worktree. Private route/HTTPS, real enrollment, issuer provisioning, policies and
runtime tests require separate approval.

The AWS 21-resource add-on remains unapproved at SHA256
`45dc5c2372597976b6585929277099e9813e5858cb402aedbcb44f5f9b87a34c`.
AWS execution is human-only. Prior worker allowances are exhausted; live workers
require a new exact approved cost window. No production change, deployment,
AWS mutation, worker activation or Tailscale handler change is authorized.
