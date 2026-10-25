# Self-hosting toolyard

## Quickstart (binary)

```bash
git clone https://github.com/tusharbhardwaj/toolyard.git
cd toolyard
go build -o toolyard ./cmd/gateway
./toolyard serve -addr :8787 -data ~/.toolyard
```

Open <http://localhost:8787>. The first visit asks you to create the local
admin account. Subsequent visits land on `/Approvals`.

## Quickstart (Docker)

```bash
docker compose -f deploy/docker-compose.yml up --build
```

Persistent data lives in the named volume `toolyard-data`. Pass
`-e TOOLYARD_DATA_DIR=/var/lib/toolyard/data` to use a custom path. For the
published image and production settings, see [Containers](#containers).

## Wiring an agent

1. In the dashboard, **Agents → Generate code**. You get a one-time code
   like `Fipvh28M-lQ`, valid for 15 minutes.
2. From your machine:

   ```bash
   ./toolyard exchange -url http://localhost:8787 -code Fipvh28M-lQ
   ```

   You get back an `agent_id` and a long-lived `token`.

3. Configure your MCP client to talk to toolyard. Examples:

   - **Streamable HTTP** (Claude Code, Cursor, etc.):

     ```json
     {
       "mcpServers": {
         "toolyard": {
           "url": "http://localhost:8787/mcp",
           "headers": { "Authorization": "Bearer <token>" }
         }
       }
     }
     ```

   - **stdio**: run `./toolyard serve -stdio` from inside the agent's
     stdio config so the gateway speaks MCP on stdin/stdout in addition
     to listening on HTTP.

4. (Optional) install the deferred-approval hook:

   ```bash
   cp scripts/claude-code-hook.sh ~/.claude/hooks/
   ```

   Add to `~/.claude/settings.json`:

   ```json
   {
     "hooks": {
       "PostToolUse": [
         { "type": "command", "command": "~/.claude/hooks/claude-code-hook.sh" }
       ]
     }
   }
   ```

5. (Optional) install interaction logging hooks. These send agent lifecycle
   events to `POST /v1/hooks/ingest`, where toolyard stores redacted payloads
   and forwards useful prompt/stop/tool summaries into MemPalace when it is
   enabled.

   - Claude Code can use direct HTTP hooks in `~/.claude/settings.json`:

     ```json
     {
       "hooks": {
         "UserPromptSubmit": [
           {
             "hooks": [
               {
                 "type": "http",
                 "url": "http://localhost:8787/v1/hooks/ingest?source=claude_code",
                 "headers": { "Authorization": "Bearer <token>" }
               }
             ]
           }
         ]
       }
     }
     ```

   - Codex and Cursor can call the generic command forwarder:

     ```bash
     mkdir -p ~/.toolyard/hooks
     cp scripts/toolyard-hook-forwarder.sh ~/.toolyard/hooks/
     chmod +x ~/.toolyard/hooks/toolyard-hook-forwarder.sh
     ```

     Then point their hook config at:

     ```bash
     ~/.toolyard/hooks/toolyard-hook-forwarder.sh codex http://localhost:8787 <token>
     ~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor http://localhost:8787 <token>
     ```

   Conductor workspaces use the same hook config as the agent type they run
   (Claude Code or Codex); there is no separate Conductor hook endpoint.

## OAuth for MCP clients

toolyard is an OAuth 2.1 authorization server for its own `/mcp`, so people
can add it to ChatGPT, Claude.ai, Claude Code, Codex, Cursor and other MCP
clients by URL (`<public-url>/mcp`) and sign in through the browser. The
step-by-step guide for each client is
[docs/guidelines/connect-clients.md](guidelines/connect-clients.md).
Agent tokens keep working next to it.

**When it is on.** It needs `-public-url`: that origin is the issuer and,
with `/mcp`, the only resource a token is valid for. With `-public-url` it
is on by default. An admin turns it off or on with the `mcp_oauth_enabled`
setting (the Connected apps page, or `PATCH /v1/settings`). While it is
off, the endpoints below answer 404, `/mcp` sends no `WWW-Authenticate`
hint, and issued OAuth tokens stop working.

**Endpoints.** Your reverse proxy must pass these through to toolyard, next
to `/mcp` and the dashboard:

| Path | What |
|---|---|
| `/.well-known/oauth-protected-resource` (and `…/mcp`) | RFC 9728 protected-resource metadata |
| `/.well-known/oauth-authorization-server` | RFC 8414 authorization server metadata |
| `/oauth/register` | RFC 7591 dynamic client registration (60 per client IP per hour) |
| `/oauth/authorize`, `/oauth/consent` | the browser steps: sign-in, then the consent screen |
| `/oauth/token` | `authorization_code` (PKCE `S256` required) and `refresh_token` |
| `/oauth/revoke` | RFC 7009 revocation |

Every other `/.well-known/` path still answers 404.
`/.well-known/toolyard-instance` (federation) is unchanged.

**Settings** (admin; `PATCH /v1/settings` or the Connected apps page):

| Key | Default | What |
|---|---|---|
| `mcp_oauth_enabled` | `true` with `-public-url` | the instance switch |
| `mcp_oauth_redirect_hosts` | empty (any) | allow list of client redirect hosts, comma separated: `claude.ai`, `chatgpt.com`, `*.example.com`, `localhost`, `127.0.0.1`, or `cursor:` for a private-use scheme |
| `mcp_oauth_access_ttl_minutes` | 60 | access token lifetime, 5 to 1440 |
| `mcp_oauth_refresh_ttl_days` | 30 | refresh token lifetime, 1 to 365, renewed on every refresh |
| `mcp_oauth_max_session_days` | 90 | absolute limit from consent, 1 to 365: no token outlives it, however often it is refreshed; the person then signs in again |
| `mcp_oauth_custom_schemes` | `cursor,vscode,vscode-insiders,windsurf,zed` | private-use redirect schemes allowed without a reverse-DNS name |

**How a connection works.** Each person's authorization of one client is a
grant. It runs as an agent that the person owns: its name is the nickname
from the consent screen, and it reports `agent_kind` `oauth_client` in
audit rows and `access.whoami`. (The `agents.kind` column keeps `agent`;
the grant row marks the agent as a client.) The consent screen also sets a
per-agent tool scope (`agent_scopes`): everything the owner can use, or
only chosen servers and tools, with blocks. The same per-agent scope works
for every agent. Approval rules do not change.

**Admin control is opt-in.** An app never inherits its owner's admin
control: even an admin's app with "everything you can use" cannot change
policies (`policies.set`, `policies.clear`), reconnect servers or sign in
shared connections. An admin can give one of their own apps that control with **May
manage toolyard (policies, servers, shared connections)** on the consent
screen or the Connected apps page; it is off by default and recorded on the
grant (`may_manage`). Only the app's owner, while an admin, can turn it on
(an admin cannot turn it on for another person's app); anyone who manages
the app can turn it off. A member's app never gets admin control: a stored
flag then has no effect, and the Connected apps page says so.

**Audit.** Every audit row an app's agent raises carries the OAuth client
id in `audit_events.oauth_client_id` (a trigger fills it from the grant).
The grant row is the agent's tombstone: deleting the agent keeps the
mapping, and connecting the client again recreates the agent under the same
id. Deleting an app's agent revokes its grant, tokens and unused codes in
the same transaction and cancels what it had pending. A new consent for a
client stops the unused codes issued before it; it stops earlier token
chains only when the grant comes back from a revoke or a deleted or
disabled agent, or when the person ticks "Sign out this app's other
sessions" (a client metadata document client shares one client_id across
installs, so one person's devices share a grant). An app's agent cannot be given a
static agent token (rotation and enrolment refuse it).

**Security notes.**

- Redirect URIs must be `https` to a valid host name or IP address, `http`
  on the loopback interface (the port may change, RFC 8252), or a
  private-use scheme: a reverse-DNS name such as `com.example.app`, or one
  of `mcp_oauth_custom_schemes`. They are matched exactly otherwise.
  `javascript:`, `data:`, `file:`, `jar:`, `ms-msdt:`, `search-ms:`,
  `ms-officecmd:` and similar schemes are always refused.
- Before the person answers, an invalid authorization request is shown to
  the person as an error page. It goes back to the client only when the
  client, its redirect URI and `response_type=code` are all valid and
  `mcp_oauth_redirect_hosts` names the redirect host. Neither a
  self-registered client nor a client metadata document makes a redirect
  URI trusted (anyone can host a document that lists their own site), so
  toolyard is never an open redirect. After the person answers, allow and
  deny always redirect.
  `state` is optional (PKCE is required); a request without `resource` gets
  `<public-url>/mcp`.
- Client ID metadata documents are fetched only while handling an
  authorization request (the token and revocation endpoints use the cached
  copy, stale included, and never fetch), only on port 443, and only from
  public addresses: only `2000::/3` for IPv6, and loopback, private,
  carrier-grade NAT (`100.64.0.0/10`, Tailscale), link-local, unique local,
  multicast, documentation, benchmarking and reserved ranges are refused,
  also when wrapped in an IPv4-mapped, NAT64 or 6to4 address. The
  connection goes to the address that was checked, TLS verifies the host,
  redirects are not followed, and the size (64 KiB) and time (8 s) are
  bounded. One client address may cause 20 fetches per 10 minutes (and its
  network 80), all addresses together 600; one client_id is fetched once at
  a time, and a requester that leaves does not fail the others waiting. A
  definite failure (the document answered wrongly, or toolyard refuses its
  address) is not retried for 5 minutes. A network failure or a temporary
  answer (HTTP 408, 429, 5xx) is not cached, and an expired copy keeps
  serving until a fetch succeeds, also while the fetch limits are full,
  for at most 7 days after it expired. The requester gets a generic error
  (the detail goes to the log). A document is cached for its `max-age` (5
  minutes to 24 hours).
- Rate limits count per address (IPv6 by /64) and per network (IPv4 /24,
  IPv6 /48), and requests without a known address in one strict bucket.
  Global caps apply to registering a client and fetching a metadata
  document, and a high ceiling (50,000 per 10 minutes) to authorization
  requests. A request counts toward a global cap only after it passed its
  address's and network's limits, so one address cannot use up a global
  budget. A registration nobody authorized is swept after a day. An
  in-process pre-limit checks every bucket before it counts any, and
  refuses floods before they write to the database; it never evicts a
  global bucket.
- Codes are single use and last 60 seconds. A code used twice revokes the
  tokens it produced. A refresh token used twice revokes the whole grant
  and disables its agent; the person connects the app again. A retry within
  60 seconds, while the token that replaced it is still unused, is refused
  without revoking anything. Revoking a grant (by its owner, its client or
  reuse detection) also cancels its agent's pending approvals (personal
  ones included) and Inbox requests, recorded as cancelled by the system
  because the grant was revoked, and nothing it queued can run.
- Tokens, codes and client secrets are stored as SHA-256 hashes. Access
  tokens start `tyat_`, refresh tokens `tyrt_`.
- All of this state is in the database (`mcp_oauth_*` tables), so several
  replicas on one Postgres database share it. The sweeper that deletes
  expired rows is safe to run on every node.

## Code mode (Bifrost-compatible)

Agents and skills written for the Bifrost MCP gateway's code mode work
unchanged against toolyard: the four tools `listToolFiles`, `readToolFile`,
`getToolDocs` and `executeToolCode` are always on, need no `_reason`, and
use the same stub format and output shapes. In code a server is its upstream
name (`BkCoreServices.get_client(clientId="vai-us")`); every call a script
makes goes through the normal pipeline (access, policy, approvals, identity
forwarding, audit) under via `code_mode`, so a held or denied call aborts the
script with the gateway's answer. Names that are not Starlark identifiers are
bound under a sanitised one (`bk-core` becomes `bk_core`; the stubs say so).
toolyard's own tools (inbox, memory, events, session, the `tools.*` helpers,
and the access tools) are one server, `toolyard`, called as
`toolyard.<group>_<name>(...)`; see
[guidelines/access-tools.md](guidelines/access-tools.md), which also explains
what an agent may change about tool policies (into ask, or between allow and
deny; never out of ask) and who may change them.

### The script worker

Each `executeToolCode` run happens in a child process: the gateway starts its
own binary again as `toolyard codemode-worker`, with an empty environment
(no `.env`, no tokens), and talks to it over stdin and stdout. The child only
asks the parent to make tool calls; the parent makes them. The child caps its
own memory (`RLIMIT_DATA`, default 256 MiB, four workers at once), marks itself
first in line for the kernel's OOM killer, and is killed at the script's wall
clock (5 minutes). A script that allocates past the cap, spins, or crashes the
interpreter costs itself the run and nothing else. Defaults are in
`internal/codemode` (`DefaultLimits`, `DefaultWorker`); the executeToolCode
tool description states the limits that are enforced.

On Linux (arm64 and amd64) the worker is also sandboxed. The gateway starts
it from a thread that has confined itself first, so the child inherits:

- **no new privileges** (`PR_SET_NO_NEW_PRIVS`);
- **a Landlock ruleset**: the worker may open only its own binary and ELF
  interpreter (to execute) and the shared libraries and `/etc/ld.so.cache`
  (to read). It cannot open, create or remove anything else: not the data
  directory, `/etc`, `/proc` or `/tmp`. From Landlock ABI 4 it cannot bind or
  connect TCP sockets; from ABI 6 it cannot signal processes outside itself.
  It can still `stat`, `readlink` and `chdir` anywhere, so it can learn which
  paths exist, but not what files hold;
- **a seccomp filter** that refuses socket creation, io_uring, ptrace and
  `process_vm_*`, bpf, perf events, userfaultfd, the kernel key store, new
  namespaces (`unshare`, `setns`, `clone` with a namespace flag or
  `CLONE_PARENT`, `clone3`), mounting, resource limits or scheduling of other
  processes (`prlimit64` on another pid, `setpriority`, `sched_set*`,
  `ioprio_set`), and changes to file metadata (chmod, chown, utimes, extended
  attributes, truncate).

`TOOLYARD_CODEMODE_SANDBOX` picks the mode:

| Value | Behaviour |
|---|---|
| `auto` (default) | Apply what the kernel supports. Without Landlock, keep no_new_privs and the seccomp filter; with neither, run the worker with its resource caps only. |
| `required` | Refuse to run scripts unless both Landlock and the seccomp filter apply. |
| `off` | Resource caps only: the worker runs as the same user, with the same filesystem view and network as the gateway. |

Any other value stops the gateway at start.

`GET /v1/health` reports the result in `codemode_sandbox`:

- `landlock-abi-<n>+seccomp`: both apply. Below ABI 6 the value ends in
  ` (signals unscoped)`: the worker can still signal the gateway and other
  processes of the same user.
- `seccomp-only: <reason>`: no Landlock, the filter applies.
- `none: <reason>`: neither applies.
- ` (sandbox required: scripts are refused)` is appended in `required` mode
  when either is missing.
- `off`, or `probing` for the moment after start while the gateway checks
  the kernel.

Two things to know when hardening the unit:

- **`SystemCallFilter` must allow the Landlock calls** (`landlock_create_ruleset`,
  `landlock_add_rule`, `landlock_restrict_self`, in systemd's `@sandbox`
  group) and `prctl` (in `@process`). In systemd 259, `@default` includes
  `@sandbox`, so `@system-service` allows them; check
  `systemd-analyze syscall-filter @default` on your version. systemd added
  `@sandbox` in version 254; on older versions, list the three calls by name.
  The gateway checks this in a throwaway child process, so a filter that
  kills on these calls costs the sandbox, not the gateway: `codemode_sandbox`
  then says which call the filter refused and which group to allow.
- **`SystemCallFilter` must allow `setrlimit` and `prlimit64`.** The shipped
  units (`deploy/toolyard.service`, the strict tier in `deploy/install.sh`)
  deny `@resources` and then allow these two back. Without them the worker
  dies with `SIGSYS` and every script fails with "code mode worker could not
  apply its memory cap".

Size `MemoryMax=` for the gateway plus the workers: four at 256 MiB is
1 GiB, and the shipped units use `MemoryMax=1536M` / `MemoryHigh=1280M`.
Decoding a tool result into script values takes many times the JSON's size;
256 MiB is what lets a result at the 6 MiB cap decode, so lowering the cap
means smaller results, not just fewer of them.

## Adding upstream MCP servers

Create `upstreams.json`:

```json
[
  {
    "name": "github",
    "transport": "stdio",
    "command": "uvx",
    "args": ["mcp-server-github"],
    "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_…" }
  },
  {
    "name": "linear",
    "transport": "http",
    "url": "https://mcp.linear.app/mcp"
  }
]
```

Then:

```bash
./toolyard serve -upstreams upstreams.json
```

Upstreams' tool catalogs are fetched at startup (every page of a paged
`tools/list`), schema-wrapped, and registered under
`<upstream-name>.<original-tool-name>`. Every `-catalog-refresh` (default
`15m`, `0` disables) toolyard reads the catalog of each live shared
upstream again and applies what changed: new and changed tools are
registered, removed ones go, and connected clients get
`notifications/tools/list_changed`. A suspended upstream is not dialled for
it, and the read does not count as use for `-upstream-idle-timeout`. An
upstream that restarted and forgot toolyard's session gets a new one, and
its catalog is read again in the same refresh. An upstream tool whose
`x-mcp-header` annotations are invalid is left out (logged), not
registered.

### MCP protocol versions

`/mcp` serves MCP 2026-07-28 and the earlier versions (2025-11-25 and
before) on the same endpoint; each request's version decides how it is
handled.

- A 2026-07-28 client sends no `initialize` and gets no session: it starts
  with `server/discover`, and every request carries its version, client
  capabilities and client info in `_meta` plus the `Mcp-Protocol-Version`
  and `Mcp-Method` (and `Mcp-Name`) headers. A request whose headers do
  not match its body is refused with `400`. The audit log names the
  client from each request's `_meta`.
- `tools/list` answers 2026-07-28 clients with `cacheScope: private`
  (each caller sees its own catalog) and `ttlMs: 60000`.
- An earlier client initializes as before; `-stateless-mcp` still applies
  to it. Its session lives in one replica's memory: with several replicas
  use `-stateless-mcp` or sticky routing (see [Several replicas on one
  database](#several-replicas-on-one-database)).
- A 2026-07-28 notification stream (`subscriptions/listen`, a long-lived
  `POST`) drains like a `GET` stream (see *Shutdown and drain*).
- Without `-public-url`, a request on a loopback address must carry a
  loopback `Host` (DNS rebinding protection). With `-public-url` that
  check is off: `/mcp` then always needs a token, and the reverse proxy
  sends the public `Host`.

Toward upstreams, `-upstream-protocol` chooses the version:

- `legacy` (default): every upstream connection speaks 2025-11-25, as
  before 2026-07-28 support.
- `auto`: a shared http upstream gets one `server/discover` at its first
  dial. When it offers 2026-07-28, every call is one stateless `POST`, with
  no session to recover, outside `-upstream-max-live` and the idle sweep.
  Otherwise (no `server/discover`, or a server that answers it with
  `UNSUPPORTED_PROTOCOL_VERSION` and lists only earlier versions) toolyard
  initializes at 2025-11-25 and asks for that version straight away on
  later dials. When a 2026-07-28 upstream is rolled back to a server that
  wants a session, the call it turns away probes again and is retried
  once. stdio and `per_user` upstreams stay on 2025-11-25.

## Inbox and permissions

Settings → **Inbox & permissions**:

- **approval_mode**: `execute` (default) queues restricted calls in
  Approvals as before; `inbox` coaches agents to send an `inbox.request`
  instead. Switch when your agents have the `toolyard-inbox` skill or the
  AGENTS.md block from the enrolment modal's "Teach it the rules" tab.
- **Copy linked media** (default on): images, videos and files an agent
  links to are downloaded once when the request arrives and served from
  toolyard, under `<data-dir>/inbox/blobs/`. Limits: image 10 MB, video
  50 MB, file 25 MB. Links to loopback or private-network addresses are
  refused unless you start the gateway with
  `-inbox-fetch-private-networks` (for evidence hosted on your own LAN).
  The fetcher connects directly, never through `HTTP(S)_PROXY`.
- **Native uploads** (always on): agents call `inbox.upload` and `PUT` a
  local file to the single-use `/v1/uploads/{ticket}` URL it returns, then
  cite `upload:<sha256>` in an attachment. Set `-public-url` so the URL is
  absolute. The ticket is the only credential: it works once, expires after
  15 minutes and is bound to one agent, size, sha256 and content type, and
  the real type is sniffed (images must be PNG, JPEG, WebP or GIF; no HTML or
  SVG files). Uploads share `<data-dir>/inbox/blobs/` and the same size
  limits; an upload no request cites within 24 hours is deleted. At most 8
  upload bodies are read at once (more get `503` with `Retry-After`). A
  reverse proxy in front must allow bodies of at least 50 MiB (52,428,800
  bytes) on `/v1/uploads/*`: in Caddy `max_size 53MB` (Caddy's `MB` is
  decimal; see `deploy/Caddyfile.example`).
- **Traefik in front (production):** production fronts Toolyard with
  Traefik v3 plus a socat TCP proxy. No buffering middleware is set on that
  router, so Traefik applies no body limit. But Traefik v3's entrypoint
  `respondingTimeouts.readTimeout` defaults to 60s, which can cut a slow,
  large upload. If agents upload large videos, set it on the `websecure`
  entrypoint, for example
  `--entryPoints.websecure.transport.respondingTimeouts.readTimeout=300s`
  (static config: `entryPoints.websecure.transport.respondingTimeouts.readTimeout: 300s`).
  Toolyard itself allows an upload body 5 minutes.
- **Judge model** (default off): needs `GEMINI_API_KEY`. Request text is
  sent to Gemini to check each tool call against the agent's own words.
  `inbox_judge_model` overrides the model (default `gemini-2.5-flash`;
  read at startup).
- **Where agents should host files**: shown to agents by
  `inbox.guide({"topic": "hosting"})`.

- **Voice notes** (default off): with `GEMINI_API_KEY` set, toolyard records
  each request's voice-note script once (Gemini text-to-speech,
  `inbox_voice_name`, default Kore; `TOOLYARD_INBOX_VOICE_MODEL` overrides the
  model) and stores it with the attachments. Off, or if recording fails,
  the browser reads the script.
- **When your phone buzzes**: `now` requests push at once (each agent gets
  `inbox_now_per_hour`, default 3; more are lowered to `soon`), `soon` ones
  are grouped per session after 90 seconds, `digest` ones wait for the
  digest (`inbox_digest_times`, default `09:30,13:30,18:30`; empty for none),
  and updates never push. **Quiet hours** (`inbox_quiet_hours`, e.g.
  `22:00-07:00`) hold everything except `now` requests whose tools are all on
  `inbox_quiet_allow` (comma-separated, `deploy.*` matches a prefix). Times
  use `inbox_timezone` (the dashboard offers your browser's). A request whose
  agent is blocked on you gets one reminder; a snoozed one comes back when
  the snooze ends.
- **Push details** (default off): pushes carry only the agent's name and the
  kind of request. On, they include the title, summary and a one-line scope,
  which is what an Apple Watch shows. Notification buttons are Deny, Snooze
  1 h, and the two answers of a two-option question. A notification never
  approves tools.
- **Schema version 3 items** (`inbox_v3_enabled`, default on): many tool
  calls and questions in one item. During a rolling upgrade to v0.1, roll
  every replica first, or turn the setting off until no older replica runs:
  an older replica that rewrites an item (a snooze, a reminder, a voice
  note) drops the version 3 fields it does not know. With it off, agents get
  a `schema_version` problem and can send version 1 or 2.
- **Webhook receivers**: a receiver gets only the event types it registered
  for; receivers from before v0.1 keep `inbox.decision`, `inbox.expired` and
  `inbox.cancelled` in the version 2 shape. Delivered and failed events are
  kept 30 days.
- **Passkeys**: add one per device. With a passkey registered, allowing
  production or red-flagged tools needs it (Face ID on an iPhone), and the
  confirmation is bound to exactly what you approve. Passkeys need the
  dashboard on `https` (or `localhost`). Removing one needs a passkey; if
  you lose every device, start the gateway once with
  `-inbox-reset-passkeys`.

## Web Push (PWA notifications)

1. Open the dashboard and visit **Settings → Enable push**.
2. Grant permission. The browser registers a Web Push subscription with
   your toolyard's VAPID public key.
3. iOS (16.4+): "Add to Home Screen" first, then enable push from the
   installed app.

A new pending approval pushes a notification with **Allow** / **Deny**
buttons. Tapping one POSTs to `/v1/approvals/decide-by-token` with the
Ed25519-signed token from the payload, so a deciding tap doesn't even need
your dashboard session.

## Sign in with Google (Clerk)

Staff can sign in with their Google account through [Clerk](https://clerk.com).
"Internal user" means a member of one Clerk organisation; nobody else gets
past the login page, whatever their email domain. The local password account
stays as the break-glass login.

1. In the Clerk dashboard, enable Google as a social connection and note
   the instance's **secret key**, **publishable key** and the
   **organisation ID** (`org_…`) whose members may sign in.
2. Put the three values in the systemd `EnvironmentFile` (or the `.env`
   next to the binary). They must never be passed on the command line:

   ```bash
   TOOLYARD_CLERK_SECRET_KEY=sk_live_…
   TOOLYARD_CLERK_PUBLISHABLE_KEY=pk_live_…
   TOOLYARD_CLERK_ORGANIZATION_ID=org_…
   ```

   Clerk sign-in is on only when all three are set; setting one or two is
   a startup error. It also requires `-public-url`, because a Clerk session
   token is only accepted when it was minted for that exact origin.
3. Start with the owner's email so the first Google sign-in with that
   address attaches to the existing password account (keeping its admin
   role, agents, passkeys and push subscriptions):

   ```bash
   ./toolyard serve -public-url https://toolyard.example.com \
     -owner-email you@example.com
   ```

Everyone else who signs in becomes a **member**: they see their own profile,
manage their own agents, and their agents reach only the servers and data
groups (memory, events, notes, skills) an admin grants them on the
**Users** page. Every approval and inbox decision stays with admins.

Once an hour (`-clerk-sync-interval`) toolyard lists the organisation's
members; anyone who left is blocked (`left_org`), their dashboard sessions
are revoked and their agents stop authenticating. A Clerk outage never
blocks anyone: on any error the sync logs and changes nothing.

### Personal Google account only

For a personal deployment, set the Clerk secret and publishable keys (an
organization ID is unnecessary), then start with:

```bash
./toolyard serve -public-url https://toolyard.example.com \
  -owner-email you@gmail.com -clerk-owner-only
```

This mode checks the Clerk Backend API for a verified primary email and a
verified Google external account with exactly that address. It disables
local password login/setup and organization offboarding sync. Non-owner
browser sessions cannot access the dashboard API. The first permitted
login links to the existing owner account, retaining its agents, grants,
passkeys, and data. Agent and operator token authentication is preserved.

Enable only Google in the dedicated Clerk app. Restrict Clerk signups to
the same email, using an allowlist or invite-only access as available on
your plan. The Toolyard server check remains mandatory: signup restrictions
alone do not authorize existing accounts. Store keys outside Git and argv.

Rollback: remove `-clerk-owner-only` and the Clerk environment variables,
and restore the prior image/configuration. No database migration is added.

## Inbox voice notes with ElevenLabs

In **Settings → Inbox & permissions → Voice notes**, select **ElevenLabs**,
enter the API key and click **Save API key**. Only an admin browser session can
save it; agent and operator tokens are rejected. Generic settings responses
return only `elevenlabs_api_key_present`, never the key. The key is stored in the
protected server database, outside source control. Protect the state volume and
its backups like other credentials. Alternatively provide `ELEVENLABS_API_KEY`
in the server environment; a key saved through the dashboard takes precedence.

Set a **Voice ID** from your ElevenLabs account, or leave it blank for George
(`JBFqnCBsd6RMkjVDRZzb`). **Speech model** defaults to `eleven_multilingual_v2`;
`ELEVENLABS_TTS_MODEL` supplies a server default. Enable **Generate voice notes
only when I click Listen** to send a script to ElevenLabs on its first Listen
click. Synthesis uses your account credits and returns MP3 audio, which Toolyard
stores for playback on every device. Provider, key and voice changes apply
without restart.
Existing recordings are retained. Failed, unavailable or quota-limited synthesis
falls back to browser speech, with a timeline note for failed attempts. It never
automatically sends a script to another cloud provider. The existing Gemini
provider and its voice settings remain available.

For Eleven v4, enter `eleven_v4` in Speech model. Toolyard sends one text/voice
pair to the Text to Dialogue API (up to 2,000 bytes per note). `eleven_v4_turbo`
requires realtime WebSocket synthesis and is not supported for these recordings.
Shared-library voices may need to be added to your ElevenLabs account first.

API: https://elevenlabs.io/docs/api-reference/text-to-speech/convert
v4 API: https://elevenlabs.io/docs/api-reference/text-to-dialogue/convert

Inbox speech is generated only on the first **Listen** click. Receiving, checking,
opening, or rechecking a request does not synthesize speech. The owner browser's
POST audio action stores the recording for subsequent plays; concurrent clicks
share one generation. In-flight generation finishes and caches even if the browser
leaves the page. Agent and operator tokens cannot trigger this action.
Free ElevenLabs accounts cannot use shared library voices through the API; use an
included voice such as Sarah (`EXAVITQu4vr4xnSDxMaL`) or resolve the account plan.

### Connecting bkt3 (T3 Code)

bkt3 signs people in through the same Clerk instance and organisation, and
can connect each of them to toolyard without a copy-pasted token. Its
server posts a fresh Clerk session token that its browser minted (so the
token's `azp` is the bkt3 origin) and gets back that person's agent token,
which it then sends as `Authorization: Bearer` on `/mcp`:

```http
POST /v1/connect/t3
Content-Type: application/json

{"token": "<Clerk session JWT>"}
```

```json
{"token": "ag_….…", "email": "ada@example.com", "agent_id": "ag_…", "user_id": "u_…"}
```

Turn it on by listing bkt3's origins (Clerk sign-in must be on):

```bash
./toolyard serve -public-url https://toolyard.example.com \
  -connect-azp https://stagebkt3.dev.beknown.live
```

- Comma-separated `https` origins (plain `http` only for `localhost`,
  `127.0.0.1` or `::1`); compared lowercase without a trailing slash. Empty
  (the default) leaves the endpoint off: `404 {"error":"connect_disabled"}`.
  The dashboard's own origin is refused at startup, and the two flows stay
  apart: a bkt3 token never opens a dashboard session, and a token minted
  for the dashboard never connects.
- It is a server-to-server call: no cookie, Origin or `X-Requested-With`
  is needed (the Clerk token is the credential), but the body must be sent
  as `Content-Type: application/json`.
- Each call checks the token (signature, expiry, issuer, `azp`) and that
  it is a fresh Clerk session token: it carries a `sid`, was issued
  (`iat`) at most 2 minutes ago and not in the future (10 s clock-skew
  leeway either way), and has no `aud` claim (JWT templates do). bkt3
  should mint a token right before each call. Then it checks the
  organisation membership, creates or refreshes the person's toolyard user
  exactly as a Google sign-in does, issues their Beknown identity key if
  they never had one (never rotating or replacing one: a person whose key
  an admin revoked stays without a key, `identity_key=missing` in the
  audit row, until an admin issues a new one), and creates their one
  `T3 Code (bkt3)` agent or rotates its token. The previous token stops
  working on `/mcp` at once, so bkt3 must keep only the latest.
- Errors are `{"error":"<code>"}`: `400 bad_request`, `401 invalid_token`,
  `403 not_org_member`, `403 user_disabled` (blocked in toolyard),
  `403 agent_disabled` (the person disabled their `T3 Code (bkt3)` agent;
  re-enable it on the Agents page), `429 rate_limited` (per client IP and
  per person), `503 clerk_unavailable` (retry later).
- Follow-up, not built: a shared secret between bkt3's server and
  toolyard (for example an `Authorization` header checked alongside the
  Clerk token), so a session token lifted from a bkt3 browser within its
  2-minute window can't be swapped for an agent token by anyone else.
- Every attempt is one `connect.t3` audit row (outcome `created` or
  `rotated`, or `denied` with the code) naming the person, the `azp`
  origin and the agent. No token is ever logged or audited.

The identity key it issues is registered in each registry upstream as
`connect:t3`, not as an admin, so in production prime-service's actor
registry will usually refuse it: the key shows a registry error on the
**Users** page. Registering a new person's key there stays a human step,
as it is for keys an admin issues: an admin opens the person on the
**Users** page and presses **Retry registration**
(`POST /v1/users/{id}/identity-key/register`, which runs as that admin),
or registers the fingerprint by hand.

## Database: SQLite or Postgres

By default toolyard keeps everything in SQLite: `toolyard.db` in the data
directory, in WAL mode, with one connection. That suits a single server,
development and tests, and it needs no setup.

toolyard can use Postgres 16 instead (Amazon Aurora PostgreSQL works too),
with a connection pool. On Postgres, two or more replicas may share one
database (see [Several replicas on one
database](#several-replicas-on-one-database)). On SQLite run exactly one
toolyard process per database. Our own production server still runs
SQLite until its planned move to Postgres.

### How toolyard picks the database

`serve`, `operator-token` and `keys-check` pick the database the same way;
the first match wins:

1. `$TOOLYARD_DATABASE_URL`, a `postgres://` or `postgresql://` URL. A
   `toolyard.env` file in a credentials directory can set it (see
   [Keys and credentials](#keys-and-credentials)).
2. A file named `database-url` in `$CREDENTIALS_DIRECTORY` (systemd
   `LoadCredential=`), then in `-credentials-dir`, that holds the URL.
   Surrounding whitespace is ignored.
3. SQLite at `<data>/toolyard.db`.

A URL that is not a Postgres URL, or an empty or unreadable `database-url`
file, stops the start with exit code 78. The URL holds the password, so
toolyard never logs it: the logs and `keys-check` name only the engine,
the host and the database name. For example:

```bash
printf '%s\n' 'postgres://toolyard:PASSWORD@db.example.com:5432/toolyard?sslmode=require' \
  > /run/secrets/database-url
chmod 600 /run/secrets/database-url
```

Use `sslmode=verify-full` with `sslrootcert=<CA bundle>` when you can, so
the client also checks the server's certificate.

### Creating the Postgres database

Create an empty database with C collation, so text sorts and compares byte
by byte, as it does in SQLite:

```sql
CREATE ROLE toolyard LOGIN PASSWORD 'PASSWORD';
CREATE DATABASE toolyard OWNER toolyard
  TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C';
```

`serve` refuses (exit 78) a database whose collation or character type is
not `C` or `POSIX`, or that uses the ICU provider. In the URL, percent-encode
`@`, `/`, `:` and `#` in the password; a URL that does not parse also exits
78, and no message ever quotes it.

The first start creates the whole schema in one transaction, and later
starts apply new migrations. Migrations run under a Postgres advisory lock,
so two processes that start at the same time do not both migrate. The role
must be able to create tables in the database (owning it is enough).

### Connection pool

`-db-max-conns` (default 10) is the most connections `serve` opens to
Postgres. With replica support on (`-cluster`, see [Several replicas on
one database](#several-replicas-on-one-database)) each replica also keeps
up to five connections of its own outside the pool. Keep the server's
`max_connections` above the total of every process that connects:
replicas × (`-db-max-conns` + 5), plus your own tools. SQLite always uses
one connection, and the flag does nothing there.

### What else changes on Postgres

- Inbox attachments (uploads, copied links and voice notes) are kept in
  the database too, in 1 MiB chunks, instead of under
  `<data>/inbox/blobs`. The data directory still holds keys (unless they
  come from a credentials directory), crash dumps, notes and skills.
- Back up the database with Postgres tools (`pg_dump`, or snapshots on
  Aurora) instead of copying `toolyard.db`. Back up the keys with it, but
  store them apart from it, as for SQLite.
- To move an existing SQLite installation to Postgres, stop toolyard, then
  run `toolyard db copy` and `toolyard db verify` (run `toolyard db` for
  their flags). `copy` creates the schema only in a database that records
  no migrations and holds no tables, refuses anything else before it
  writes, and is all-or-nothing, blobs included: a failed run leaves the
  target empty. `verify` only reads. Both refuse a source that looks in
  use, and check again before they finish. Never start a gateway on a copy
  of production data: it would refresh real OAuth tokens.

### Several replicas on one database

On Postgres you can run two or more toolyard replicas behind one load
balancer, all on one database, and replace them one at a time (a rolling
update). They share every row: a decision, a policy, a setting, a token or
a server saved on one replica is in force on the others within moments.
SQLite stays single-replica.

`-cluster` (`TOOLYARD_CLUSTER`) chooses: `auto` (the default) turns
replica support on for Postgres and off for SQLite; `on` asks for it (and
refuses SQLite); `off` runs one gateway per database even on Postgres: no
`LISTEN` connection, no periodic recovery, rate limits in process, and the
start settles leftover work at once, as on SQLite. A deployment that runs
exactly one replica on Postgres may set `off`; with two or more replicas
it must be `on` (or `auto`). Each gateway states its setting in the
database for its whole life (an advisory lock: exclusive for `off`,
shared for `on`), so a second gateway next to an `off` one, or an `off`
gateway next to running replicas, does not start: it waits up to 60
seconds for the other gateway to stop (a short overlap), logs the
`application_name` of each gateway that holds the lock, and exits with
code 78. A running gateway that loses its database connection and finds
the lock taken by a gateway with the other setting when it reconnects
drains and exits with code 78 too.

Because an `off` gateway allows no second gateway at all, a deployment
with `-cluster=off` must update stop-first (Swarm
`update_config.order: stop-first` and the same for `rollback_config`,
Kubernetes `strategy: Recreate`), as on SQLite. A change of the
`-cluster` setting (from `off` to `on` or back) must also be a
stop-first update: stop every gateway, then start the new ones.

Every replica must have the same configuration and the same keys
(identity, OAuth, secrets and session keys from the same credentials, with
`-keys=require` so no replica ever creates its own), and run the same
binary except during an update. Run the migrations once per
deploy (`toolyard db migrate`, with `-migrate=off` on the replicas) or let
the replicas migrate at start: migrations run under an advisory lock.

How the replicas share the work:

- **One replica at a time** runs each background job that must not run
  twice: the approval and Inbox sweepers, the Inbox attention loop
  (pushes, reminders, digests), the legacy Inbox sync, the OAuth token
  refresher, the Clerk membership sync, the session purge, the retention
  compactor, the auto-approval proposer, the anomaly detector and reason
  scorer, the events pollers and retention, the Telegram poller and chat
  reconciler, the memory-webhook worker, the notes and skills scanners,
  the retry of failed identity-key removals, and the recovery of stopped
  replicas' work. Each holds a Postgres advisory lock (a leader lease).
  A replica that stops cleanly hands its jobs over when its drain starts,
  and another replica takes them within about five seconds. A replica
  that vanishes (its node lost, its network cut) keeps its locks until the
  database ends its session: toolyard asks for TCP keepalives every 5
  seconds after 10 idle seconds and a 15-second TCP user timeout on these
  connections, so that takes up to about 30 seconds (the operating system
  default would be about two hours).
- **Every replica** runs what belongs to it: its own upstream connections
  (the live pool, the idle sweep and the catalog refresh of its own
  connections), its own code-mode script workers, the requests it serves
  (MCP over stateless requests, REST, dashboard streams), its price list,
  its temporary upload files, and the callback deliveries (each delivery
  is claimed in the database first). Because each replica reads tool lists
  over its own connections, two replicas may show different catalogs for
  up to `-catalog-refresh` after an upstream changes its tools.
- **Work that outlives a request** (an Inbox execution of a granted call, a
  legacy execution, a code run over REST, a memory-webhook job) names the
  replica that runs it and holds a lease that replica renews every 15
  seconds, timed by the database's clock (so clock drift between replicas
  does not matter). The leader settles only work whose lease ended (60
  seconds without a renewal): a granted call becomes `outcome_unknown` and
  never runs twice. It waits one lease (60 seconds) after it takes over
  and after the database last failed to answer, so a replica that could
  not renew during an outage keeps its work. A replica that starts settles
  nothing on its own, so a rolling restart never touches the live work of
  another replica. With `-cluster=off` (or on SQLite) the start settles
  everything it finds, as before. An auto-approved call that runs in line
  writes its claim in the same transaction as the decision, so no recovery
  ever runs it a second time.
- **Caches** (policies, settings, auto-approval rules, OAuth bearers, the
  upstream servers, MCP client names, the Web Push key pair) are refreshed
  on every replica when one replica writes; replicas send each other these
  signals with Postgres `LISTEN`/`NOTIFY`, and every replica reloads every
  30 seconds in case a signal was missed.
- **Waits** (`inbox.wait`, `tools.wait_for_approval`, an in-line approval
  hold) wake on a decision made on any replica, and re-read the database
  every 2 seconds as a fallback. Dashboard live events reach a dashboard
  connected to any replica.
- **Limits and locks** that guarded one process now hold across replicas:
  the login and unauthenticated-route rate limits, the connect-t3 limits
  and per-person lock, the auto-approval budget per agent, Inbox
  submissions per agent, policy writes, upstream edits, OAuth refreshes,
  federation connection writes and personal GitHub writes. Passkey
  ceremonies live in the database, so a confirmation started on one
  replica finishes on another.

Things to set:

- **Connections.** Each replica keeps up to five connections of its own
  outside the pool: leader leases, named locks, lease renewals, `LISTEN`
  and the `-cluster` setting. Each shows its purpose and the replica's
  instance ID as its `application_name` in `pg_stat_activity`; the
  keepalive settings are `SET` once it opens, and one the server refuses
  (`tcp_user_timeout` before Postgres 12) is skipped with one log line. They need session state, so connect the replicas to Postgres
  directly or through a pooler in session mode. A transaction-mode pooler
  (PgBouncer `pool_mode=transaction`) breaks session advisory locks and
  `LISTEN`: do not use one between toolyard and Postgres.
- **MCP sessions.** A 2026-07-28 client has no session and works with any
  replica. An earlier client opens a session (`Mcp-Session-Id`) that lives
  in one replica's memory: with several replicas run `-stateless-mcp`, or
  make the load balancer send each session to one replica (sticky routing
  on the `Mcp-Session-Id` header).
- **`-instance-id`** names the replica in leases and logs (for example the
  pod name: `TOOLYARD_INSTANCE_ID` from the Kubernetes downward API).
  Every process adds a random suffix of its own (`<name>-<8 hex digits>`),
  so a container that restarts in the same pod, or two replicas given the
  same name, never share an ID. Without the flag the host name is used.
- **Files under `-data`** are per replica: crash dumps, the price list
  cache, temporary uploads, and the notes, skills and MemPalace stores.
  With several replicas, run `-mempalace=off` (MemPalace keeps its store
  on one replica's disk, so memories written through one replica are
  missing on the others), and put notes and skills on a shared volume or
  turn them off (`-notes=off -skills=off`). Keys come from the shared
  credentials, not from `-data`.
- **The first move to v0.1 must stop the old gateway first.** A gateway
  older than v0.1 (no replica support) must never run next to another
  gateway on the same database, so the update that brings in v0.1, and any
  rollback to a version older than v0.1, must be stop-first: Swarm
  `update_config.order: stop-first`, Kubernetes `strategy: Recreate` with
  one replica. Only once every replica runs v0.1 or later may updates
  overlap.
- **Later updates** may overlap when every gateway runs with `-cluster`
  `on` or `auto`, and the update keeps that setting: run a rolling update
  (Kubernetes `RollingUpdate` with `maxUnavailable: 0`, Swarm
  `update_config.order: start-first`). With `-cluster=off`, or when the
  update changes the `-cluster` setting, update stop-first. Draining replicas hand over their leader jobs at once and
  finish or record their in-flight work as described in [Shutdown and
  drain](#shutdown-and-drain).

## Keys and credentials

toolyard seals stored credentials with AES-256-GCM under master keys,
one file per subsystem, and signs dashboard session cookies with a
session key:

| File | Protects |
| --- | --- |
| `identity.key` | per-person identity keys |
| `oauth.key` | upstream OAuth client secrets and tokens |
| `secrets.key` | the secrets broker, callback and connection credentials, the Telegram bot token |
| `session.key` | dashboard session cookies (signing only; nothing stored) |

Each file holds the base64 encoding of 32 random bytes. Surrounding
whitespace (such as the trailing newline of a Docker secret) is ignored,
and padded base64 is accepted, so `openssl rand -base64 32 > oauth.key`
makes a valid key.

### Where toolyard looks for keys

For each key, in this order; the first copy found is used:

1. `$CREDENTIALS_DIRECTORY/<name>`: systemd `LoadCredential=`.
2. `-credentials-dir` (default `$TOOLYARD_CREDENTIALS_DIR`; the Docker
   image sets `/run/secrets`, where Docker and Compose mount secrets).
3. The data directory (`-data`), where earlier versions created them.

A copy that exists but cannot be read or decoded stops the start, with an
error that names the key and the path. toolyard never replaces a key file
and never falls back to a copy further down the list. When a lower copy
differs from the one used, a warning says so.

The same two credentials directories may hold a `toolyard.env` file
(`KEY=value` lines). It is loaded at start; a variable already set in the
environment wins over it, and the first directory wins over the second.
The Clerk variables also accept a file: set
`TOOLYARD_CLERK_SECRET_KEY_FILE=/run/secrets/clerk_secret_key` instead of
`TOOLYARD_CLERK_SECRET_KEY` (setting both is an error).

### `-keys` modes

- `-keys=create-if-fresh` (the default): a missing key is created in the
  data directory, with `O_EXCL` and an fsync of the file and the directory,
  but only when the database is new: it has no key canaries and no sealed
  rows. On a database in use, a missing key stops the start. The one
  exception is `session.key`: a missing session key is recreated with a
  warning, and everyone signs in again.
- `-keys=require`: no key is ever created. Use it for clusters and for
  any deployment whose keys come from a secret store.

### Key canaries

The `key_canaries` table holds, for each key, a fixed value sealed with
that key and a short key id (a non-secret fingerprint). At every start,
each key must open its canary. A wrong key file stops the start with
`key <name> does not match this database`, naming the key id of the file
and the key id the database expects, before anything is written. A key
that has no canary gets one; that is how the first start with this version
adopts the keys of an existing installation. Before it adopts a key,
toolyard tries the key on values the database already holds sealed with
it (the newest few of each kind, with the same binding the owning feature
uses; one that opens is enough). A key that opens none of them stops the
start with `key <name> ... does not open existing data`, so a wrong key
file on disk at upgrade time is refused instead of being locked in. A key
with no sealed values yet, and the session key, which seals nothing, are
adopted as they are.

Key errors, credential-file errors and a bad `-keys` value exit with code
78 (`EX_CONFIG`). Restarting cannot fix them, so the systemd units in
`deploy/` set `RestartPreventExitStatus=78`. Docker restart policies have
no such exception: a container with a key problem restarts until you fix
it, so read its logs.

### Check keys before an upgrade

Before you upgrade a server that already has data, or move its keys, run
the read-only check with the same directories the server uses:

```bash
toolyard keys-check -data /var/lib/toolyard -credentials-dir /run/secrets
```

It opens the database `serve` would use read-only (`toolyard.db`, or a
Postgres session with `default_transaction_read_only=on`; no migrations,
no writes), runs the same search, decode, canary and sealed-data checks as
`serve`, and prints one
line per key: where it was found and its key id, and whether its canary
matches, is absent (the key is adopted on the next start, as on the first
start after an upgrade from a version without canaries, once it opens the
data sealed with it) or does not match, or whether the key `does not open
existing data`. It never
creates a key, writes a canary or changes any file, and it never prints
key material. It exits 0 when every key would load on a real start, and
78 when one would stop it. It checks `-keys=create-if-fresh` unless you
pass `-keys require`. A missing session key is reported as `warn`: the
start creates a new one and everyone signs in again.

Run it with the directories the service really uses. A shell does not see
a unit's `$CREDENTIALS_DIRECTORY`, so when the service gets its keys
through systemd `LoadCredential=`, pass that directory with
`-credentials-dir`, or `keys-check` falls back to the copies in `-data` and
checks different files than the service loads. While the unit runs,
systemd mounts its credentials at `/run/credentials/<unit>/`, readable by
root:

```bash
sudo toolyard keys-check -data /var/lib/toolyard \
  -credentials-dir /run/credentials/toolyard.service
```

Otherwise run it under the same credentials, for example with
`systemd-run --pipe --wait -p LoadCredential=…` and the unit's lines.

### Moving keys and recovering

- To move keys into a secret store: copy the four files from the data
  directory into the credentials directory (or `LoadCredential=` them),
  run `toolyard keys-check -keys require` with both directories, start
  with `-keys=require`, and once it starts cleanly remove the copies from
  the data directory.
- To move a server to a new host, copy `toolyard.db` together with all
  four key files (`deploy/install.sh --import-from <dir>` does), then run
  `toolyard keys-check` there before the first start.
- `does not match this database`: the key file belongs to another database
  (or `-data` points at another database). Find the file whose key id the
  error names (each start logs the key ids it adopts or creates), or point
  `-data` at the database the key belongs to.
- `not found ... set up with it` or `already in use`: restore the key file
  from the backup taken with this database. Back up the keys with the
  database, but store them apart from it.
- A key lost for good cannot be recovered, and neither can the values
  sealed with it. Restore the database and the keys from one backup.

## Health probes

| Route | Answer |
| --- | --- |
| `GET`/`HEAD /v1/livez` | `200 {"status":"live"}`: the process serves requests. It reads nothing, the database included. |
| `GET`/`HEAD /v1/readyz` | `200 {"status":"ready"}`, or `503 {"status":"not_ready","reason":"…"}` |

The readiness reasons are `draining` (shutdown started), `keys` (master
keys not yet verified), `starting` (startup recovery not finished) and
`db` (no database ping has succeeded yet, or the last two pings in a row
failed). The database has a single connection, so a ping waits behind a
long query; each ping may take up to 3 s, and one slow or failed ping does
not make the process unready. A probe answers from the latest ping results
and starts at most one new ping per second, in the background, so polling
cannot put load on the database and a probe does not wait on a busy one
(only the first check after the start waits for its ping). The answer
never carries more detail than the reason.

`/livez`, `/readyz` and `/healthz` (liveness) are aliases. The probes need
no session, have no rate limit and no CSRF check, and accept only `GET`
and `HEAD`. `/v1/health` keeps its diagnostic body (goroutines, upstream
pool, sandbox status); it is not a probe.

`toolyard healthcheck -url <probe URL> -expect ready|live` fetches a probe
with a 3 s timeout and exits 0 when its `status` matches, 1 otherwise. The
Docker image's `HEALTHCHECK` uses it on the liveness probe, because the
image has no `curl` or `wget`:

```bash
toolyard healthcheck -url http://127.0.0.1:8787/v1/livez -expect live
```

For an orchestrator, point the liveness check at `/v1/livez` and the
readiness check at `/v1/readyz`.

## Ops listener and metrics

`-ops-addr` starts a separate listener for monitoring; it is off by
default. It serves `/metrics` (Prometheus text format 0.0.4) and the two
probes; every other path is a 404. The main listener never serves
`/metrics`.

```bash
./toolyard serve -ops-addr 127.0.0.1:9090
# several addresses, one of them reachable by a scraper on the LAN:
./toolyard serve -ops-addr 127.0.0.1:9090,10.0.0.5:9090
```

- Without `-ops-token-file`, every address must be loopback, private
  (RFC 1918, `fc00::/7`), link-local or in `100.64.0.0/10`, or `localhost`.
  Any other address, an empty host (`:9090`) and `0.0.0.0` are refused at
  startup.
- With `-ops-token-file <path>` (at least 16 characters, whitespace
  trimmed), any address is allowed, and every request must send
  `Authorization: Bearer <token>`, from every peer, loopback included: a
  relay on the same host would otherwise let remote callers in. The token
  is compared in constant time. Probes on the ops listener then need the
  header too; container and orchestrator checks can use the main
  listener's `/v1/livez` and `/v1/readyz`, which need no token.

| Metric | Type | Meaning |
| --- | --- | --- |
| `toolyard_build_info{version}` | gauge | always 1 |
| `toolyard_process_start_time_seconds` | gauge | process start, Unix seconds |
| `toolyard_started` | gauge | 1 once startup recovery and upstream loading are done |
| `toolyard_keys_verified` | gauge | 1 once every key matched its canary |
| `toolyard_draining` | gauge | 1 during shutdown |
| `toolyard_db_up` | gauge | 1 when the database answered a ping within 1 s at scrape time |
| `toolyard_db_ping_seconds` | gauge | duration of that ping |
| `toolyard_goroutines` | gauge | goroutines that exist now |
| `toolyard_tool_calls_total{outcome}` | counter | `ok`, `error`, `denied`, `approval`, `refused_draining` |
| `toolyard_inflight_executions` | gauge | tool executions running now |
| `toolyard_audit_write_failures_total{stage}` | counter | audit records not written: `intent`, `outcome`, `approval` |
| `toolyard_executions_outcome_unknown_total{path,cause}` | counter | executions that may have run but have no recorded result (`path` is `inbox` or `legacy`) |
| `toolyard_goroutine_panics_total{loop}` | counter | panics recovered in background loops and goroutines; `loop="codemode-nested-call"` counts a tool that panicked during a code-mode script |
| `toolyard_loop_last_success_timestamp_seconds{loop}` | gauge | last successful pass of each background loop |
| `toolyard_legacy_sync_last_success_timestamp_seconds` | gauge | last successful legacy approval sync into the Inbox (0 = never) |
| `toolyard_legacy_sync_failures_total` | counter | failed legacy approval syncs |
| `toolyard_log_records_total{level}` | counter | log records by level |
| `toolyard_http_panics_total` | counter | panics recovered by the HTTP recovery middleware (every route of the main listener) and by the MCP tool-handler recovery |

Labels never name an agent, a user, a tool or an argument. Each metric
keeps at most 64 label sets; later new values are counted under `other`.

## Shutdown and drain

On SIGTERM (or SIGINT) toolyard drains before it exits:

1. Readiness turns `503` (`draining`), new tool calls get a retryable
   refusal, and a new MCP stream (a `GET`, or a 2026-07-28
   `subscriptions/listen` `POST`) gets `503`, so a load balancer stops
   sending work.
2. After `-drain-delay` (default `3s`), open MCP streams and
   dashboard event streams close, a new dashboard stream gets `503`, so
   clients reconnect to another instance, and the HTTP server stops
   accepting new requests.
3. Open HTTP requests and in-flight executions have until `-drain-timeout`
   (default `60s`, counted from the start of the drain) to finish. Work
   still running then is cancelled and recorded as outcome unknown.
4. The remaining services and the database close, each within a bound
   (about 10 s in total).

Give the process time to finish: set the container's `stop_grace_period`
(Compose) or `terminationGracePeriodSeconds` (Kubernetes), or the systemd
unit's `TimeoutStopSec`, to at least `75s`.

Under systemd also set `KillMode=mixed` (the shipped units do). The default
`KillMode=control-group` sends SIGTERM to every process of the unit at once,
code-mode script workers included; `mixed` sends it to the gateway only,
which drains and then ends its workers itself. Workers ignore SIGTERM, so a
stop signal sent to the whole unit cannot cut a running script either.

## Containers

The image `ghcr.io/iamtushar324/toolyard` is built from `deploy/Dockerfile`
for linux/amd64 and linux/arm64 on every push to `main`, after the Go tests
pass (`.github/workflows/publish-image.yml`). Every pull request builds it
on both architectures and runs it as a smoke test: read-only root, no
capabilities, uid 10001, against Postgres 16. A pull request from this
repository (not a fork) also publishes its image.

### Image

- Tags: `sha-<commit>` (the full 40-character commit) and `main` (moves on
  every push). The binary reports version `sha-<commit>` (`toolyard
  version`, `/v1/health`, `toolyard_build_info`).
- Pull requests from this repository: `pr-<number>-<head>`, where `<head>`
  is the first 7 characters of the pull request's head commit, built from
  the merge commit GitHub tests, after the container smoke test (the Go
  tests run in the pull request's own Inbox integration workflow). The
  binary reports the same string as its version. For test deployments only
  ([Kubernetes](#kubernetes)).
- Deploy by digest, never by tag:
  `ghcr.io/iamtushar324/toolyard@sha256:<digest>`. The publish run's summary
  prints the digest of the multi-arch index. Each image carries an SBOM and
  SLSA provenance, and the index has a GitHub attestation. Pull request
  images have valid attestations too, so a production check must also pin
  the ref the image was built from:

  ```bash
  gh attestation verify oci://ghcr.io/iamtushar324/toolyard@sha256:<digest> \
    --repo iamtushar324/toolyard --source-ref refs/heads/main
  ```

  A pull request image names `refs/pull/<number>/merge` instead, and this
  check refuses it.

- Entrypoint `tini -- toolyard`, default command `serve`, user and group
  10001. Ports 8787 (dashboard, `/mcp`, `/v1/*`) and 9090 (the ops
  listener, when enabled).

### Mounts

| Path | Purpose |
| --- | --- |
| `/var/lib/toolyard/data` | The data directory (`TOOLYARD_DATA_DIR`); must be writable. On Postgres it holds only crash dumps, the model-price cache and temporary upload files (up to 50 MiB each): a disposable disk volume of about 1 GiB. On SQLite it holds the database: use a persistent volume. |
| `/tmp` | A small tmpfs (64 MiB) when the root filesystem is read-only. |
| `/run/secrets` | The credentials directory (`TOOLYARD_CREDENTIALS_DIR`), where Docker and Swarm mount secrets. On Kubernetes, mount the Secret elsewhere (for example `/etc/toolyard/credentials`) and set the variable. |

The root filesystem can be read-only, and toolyard needs no capability:
run it with all capabilities dropped and `no-new-privileges`.

### Secrets

Files in the credentials directory, each readable by uid 10001 (Swarm:
`uid: "10001"`, `gid: "10001"`, `mode: 0400`):

- `identity.key`, `oauth.key`, `secrets.key`, `session.key`. Create fresh
  keys with the image itself: `toolyard keys generate -name <identity|oauth|secrets|session>`
  prints one key to stdout (for example into
  `docker secret create toolyard-oauth-key-v1 -`), and
  `toolyard keys generate -all -dir <dir>` writes all four files. Back them
  up apart from the database.
- `database-url`: the Postgres URL. For `sslmode=verify-full`, mount the CA
  bundle (a Swarm config or a ConfigMap) and point `sslrootcert=` at it.
- `ops-token`: the ops listener's bearer token, needed when it listens
  beyond loopback and private addresses.
- `toolyard.env` (optional): `KEY=value` lines, such as the
  `TOOLYARD_CLERK_*` variables.

### Configuration

Every `serve` flag can also be set as `TOOLYARD_<FLAG>`: the flag name in
upper case with `-` replaced by `_` (`-public-url` is
`TOOLYARD_PUBLIC_URL`, `-db-max-conns` is `TOOLYARD_DB_MAX_CONNS`). A flag
on the command line wins, an empty variable counts as unset, and
`toolyard.env` may set them too (except `TOOLYARD_CREDENTIALS_DIR`).
`-inbox-reset-passkeys` is command line only. `serve` warns about a
`TOOLYARD_*` variable that matches no flag and that nothing reads (a
misspelt name), naming the variable but never its value. Run
`toolyard help` for the variables the other commands read.

At startup, `serve` and `toolyard db migrate` keep trying a Postgres server
they cannot reach for `-db-connect-timeout` (env
`TOOLYARD_DB_CONNECT_TIMEOUT`, default `60s`, `0` = one attempt), so a
restart during a database failover recovers by itself. A bad URL, a wrong
collation or encoding, a refused login or a missing database fails at once.
The HTTP listener starts only after the database opens, so keep health
check grace periods at 60 s or more. Secrets stay in files, never in
variables. A production set:

```bash
TOOLYARD_PUBLIC_URL=https://toolyard.example.com
TOOLYARD_TRUSTED_PROXY=10.200.0.0/24   # the proxy's subnet, nothing wider
TOOLYARD_KEYS=require
TOOLYARD_MIGRATE=off
TOOLYARD_OPS_ADDR=0.0.0.0:9090
TOOLYARD_OPS_TOKEN_FILE=/run/secrets/ops-token
TOOLYARD_STATELESS_MCP=true
TOOLYARD_NO_STDIO_UPSTREAMS=true
TOOLYARD_NOTES=off                     # and SKILLS, MEMPALACE, VOICE
TOOLYARD_UPSTREAM_IDLE_TIMEOUT=15m
TOOLYARD_DB_MAX_CONNS=10
GOMEMLIMIT=200MiB
GOGC=50
```

- **`-keys require`**, always in a container. With the default
  `create-if-fresh`, the first start on an empty database creates the keys
  in the data directory; once that directory is gone, every later start
  fails with exit code 78.
- **`-trusted-proxy`**: the subnet the reverse proxy connects from. On
  Swarm, give the proxy and toolyard an overlay network with a fixed subnet
  that nothing else joins. Secure cookies, HSTS and per-IP rate limits
  depend on it, and an invalid CIDR is skipped, so check the value. The
  proxy must see real client addresses (Swarm: publish its ports in host
  mode).

### Schema migrations

`serve -migrate auto` (the default) migrates the schema when it starts.
With `-migrate off`, `serve` only checks it: a schema behind the binary
stops the start with exit code 78 and a message to run
`toolyard db migrate`. Run that command with the new image and the same
credentials and database before you deploy the image. It applies pending
migrations (under the Postgres advisory lock), prints what it applied and
exits 0; config errors exit 78.

### Probes

- Liveness: `/v1/livez` reads nothing. The image's `HEALTHCHECK` runs
  `toolyard healthcheck` on it (60 s start period), and so should a Swarm
  `healthcheck` or a Kubernetes startup and liveness probe.
- Readiness: `/v1/readyz`, for routing only (a Traefik
  `loadbalancer.healthcheck.path`, a Kubernetes readiness probe). Never
  restart on it: a database outage makes it fail, and a restart does not
  help.
- Port 8787 opens only once startup is done (about 20 s); until then only
  the ops port answers, with `readyz` reason `starting`. To check the ops
  port when it needs a token, pass `-token-file <path>` to
  `toolyard healthcheck`.

### Stopping and updating

- Give the container 75 s to stop (`stop_grace_period: 75s`,
  `terminationGracePeriodSeconds: 75`); see [Shutdown and
  drain](#shutdown-and-drain).
- On SQLite, never run two gateways on one database, not even for a moment
  during an update: use Swarm `update_config.order: stop-first` (and the
  same for `rollback_config`), or Kubernetes `strategy: Recreate`. Each
  deploy then has a short outage.
- On Postgres, once every running gateway is v0.1 or later with
  `-cluster` `on` or `auto`, replicas may overlap: use a rolling update
  (Swarm `start-first`, Kubernetes `RollingUpdate` with
  `maxUnavailable: 0`). The update that first brings in v0.1, a rollback
  below v0.1, any update of a `-cluster=off` gateway, and an update that
  changes the `-cluster` setting must still be stop-first. See
  [Several replicas on one database](#several-replicas-on-one-database).
- A key or configuration error exits 78 and a restart does not fix it:
  limit restarts (Swarm `restart_policy.max_attempts`) and read the logs.

### Code-mode sandbox

Script workers are confined with Landlock and seccomp. That needs no
capabilities and works under the default seccomp profiles of Docker,
containerd and CRI-O; the node kernel must have Landlock enabled
(`landlock` in `/sys/kernel/security/lsm`). Check what a running
container gets:

```bash
curl -s https://toolyard.example.com/v1/health | jq -r .codemode_sandbox
```

It should read `landlock-abi-<n>+seccomp`. Then set
`TOOLYARD_CODEMODE_SANDBOX=required`, so scripts are refused instead of
running with less confinement if a later node lacks it.

### Examples

- Docker Swarm: `deploy/swarm/stack.example.yml`, a generic stack with the
  settings above (secrets with owner and mode, read-only root, tmpfs,
  stop-first updates, 75 s grace, a liveness health check, Traefik labels).
- Kubernetes: `deploy/k8s/`, a reference Kustomize base (restricted pod
  security, `Recreate`, probes, emptyDir volumes) and an HA test overlay;
  see [Kubernetes](#kubernetes).

### Kubernetes

`deploy/k8s/` has two layouts; `deploy/k8s/README.md` explains both. CI
renders them and validates them with kubeconform.

- `base/`: one replica with `strategy: Recreate` on an external Postgres,
  under the `restricted` Pod Security Standard. A starting point for your
  own overlay.
- `overlays/ha-test/`: a test deployment with two replicas
  (`RollingUpdate`, `maxUnavailable: 0`), a PodDisruptionBudget, preferred
  spreading over nodes and zones, an in-cluster Postgres 16 StatefulSet on
  a persistent volume, a migration Job, an Ingress and NetworkPolicies.
  Not for production data: the Postgres has no backups and no replication.

`scripts/k8s-test-deploy.sh` deploys the overlay into one namespace with a
namespaced deployer kubeconfig. Cluster values (host, image, node selector,
tolerations, storage class, ingress class, trusted proxy) are environment
inputs, never committed. It creates the credentials Secret (four keys and
`database-url`) and the Postgres password once, from files in a private
temporary directory, then applies Postgres, runs `toolyard db migrate` as a
Job and waits for the gateway's rollout. Until the first account exists,
`/v1/auth/setup` is open to any caller, so the script then creates the admin
account through a port-forward (the generated password goes to the
`toolyard-admin` Secret) and only then applies the Ingress. Last it smoke
tests the result: `/v1/health` and `/v1/readyz` through the Ingress,
`/v1/readyz` on every pod through a port-forward, MCP `server/discover`, and,
with an agent token (`TOKEN_FILE`), a code-mode run. `--destroy` removes it
but keeps the database volume and the Secrets; `--destroy --purge` deletes
those too.

`REPLICAS` is required: 1 for an image without v0.1 high availability
(`Recreate`, no PodDisruptionBudget), 2 only for one with it. Every pull
request from this repository publishes an image tagged
`pr-<number>-<7-character head commit>`, so a branch can run in the test
cluster before it merges; the publish run's summary prints the digest to
pass as `IMAGE`.
