# toolyard skills hub

Status: in progress on the `t3/skills-hub` branch (TEC-1782). This file is the
contract between the parallel work streams ("slices") that build the skills
hub. Each slice works on its own branch and opens a draft pull request into
`t3/skills-hub`; the integration pull request `t3/skills-hub` → `main` ships
them together.

Architecture before and after the skills hub: [skills-hub-architecture.excalidraw](skills-hub-architecture.excalidraw) (open it in Excalidraw).

## Goal

An admin adds a GitHub repository and a branch once. toolyard copies the
[Agent Skills](https://agentskills.io/specification) in it, at an exact
commit, into its database, keeps that copy current, and serves it to every
client: MCP tools (Claude.ai, ChatGPT, Claude Code, Cursor), code mode, REST,
and the MCP Skills extension
([SEP-2640](https://modelcontextprotocol.io/seps/2640-skills-extension), final
2026-09-13). Agents never query GitHub themselves.

Why a synced copy and not live GitHub reads: one database read instead of a
GitHub call per read; one small check per source every few minutes instead of
every agent read spending the shared GitHub rate limit; skills keep working
when GitHub is down; every replica serves the same commit; a search index; a
review gate before a change reaches agents; and stable SHA-256 digests, which
SEP-2640 requires.

The old skills feature (local `<data-dir>/skills` folder, the `npx`
filesystem MCP server registered as the `skills` upstream, MemPalace
indexing, `skills.publish`, `skills.install`, `skills.list_tags`,
`skills.bundle`) is removed. `skills.list` and `skills.get` keep their names
with the new meaning below.

## Concepts

- **Source**: one GitHub repository + branch (+ optional folder) that holds
  skills. Identified by `id` (`src_<hex>`) and a unique `slug`
  (`^[a-z0-9]+(-[a-z0-9]+)*$`, ≤ 40 chars; default `<repo>`, made unique).
- **Version**: the skills of a source at one commit SHA. Status `staged`
  (built, not live), `active` (live; at most one per source), `retired`
  (was active), `rejected` (review said no), `failed` (sync error; no
  skills). The last 10 non-active versions are kept; older ones and their
  unreferenced file bytes are deleted.
- **Skill**: a folder that contains `SKILL.md`, inside the source folder.
  Its *skill path* is the folder path relative to the source folder
  (`agent-tooling/pr-merge-ready`). Its URI is
  `skill://<slug>/<skill path>/SKILL.md`; its other files are
  `skill://<slug>/<skill path>/<file path>`. The last segment of the skill
  path must equal the frontmatter `name` (SEP-2640); a skill whose `name`
  differs is skipped with a warning. Nested skills are published flat (each
  has its own entry) and are also ordinary supporting files of the enclosing
  skill (SEP-2640 "Nested content").
- **Runs**: `anywhere` or `host`. A `host` skill needs the T3 host
  (its paths, `tools/*.sh`) and is hidden from callers that are not
  on the host unless they ask for host skills. Order of precedence: an
  admin override for that skill, then frontmatter `metadata.toolyard-runs`
  (`host` | `anywhere`), then the source default (`default_runs`).
- **Visibility**: `everyone` (every active member and their agents) or
  `members` (only the listed user IDs and their agents). Admins and the
  owner always see every source. An agent sees what its owner sees.
- **Update mode**: `auto` (a new commit is validated and made active),
  `review` (a new commit becomes a staged version and an Inbox item for the
  source's creator; approval makes it active), `pinned` (new commits are
  ignored; `pinned_sha` is served). The dashboard defaults to `auto` when
  the repository owner is in the setting `skills_trusted_github_owners`
  (comma list; default empty) and to `review` otherwise.

## Rules for every slice

- **Both engines.** Every feature works on SQLite (one node) and on Postgres
  (many nodes). Shared tests use `internal/store/storetest`.
- **Migrations.** Paired files with the same name under
  `internal/store/migrations/sqlite/` and `internal/store/migrations/postgres/`,
  only from your range: A `0068`–`0069`, C `0070`, B `0071` (only if needed),
  D `0072`. Never edit a migration that is on `main`. Add a `Rollback:` note.
- **Background loops** run under `cluster.RunLeader` (via `leaderLoop` in
  `cmd/gateway`). A sync of one source runs under
  `cluster.LockCtx(ctx, "skillhub:sync:"+sourceID)` on whichever node starts
  it (poll loop, webhook, "Sync now"), so two syncs of one source never
  overlap.
- **Caches.** Each node keeps an in-memory index of the active versions
  (entries and file lists, not file bytes). After an activation, publish
  `cluster.Publish(ctx, "skillhub", sourceID)`; every node reloads that
  source. Subscribers also reload everything on the periodic resync.
- **Stateless MCP.** Nothing depends on a server-initiated message reaching
  the client over the same node. `list_changed` notifications are
  best-effort.
- **Secrets.** A source token is write-only: stored sealed with the existing
  secret sealing (`internal/sealbox` / `internal/secrets`, whichever the
  other credential stores use), never returned by any API, never logged,
  never in an error string. The repository is public: no secrets, internal
  host names or internal IPs in code, tests, docs or fixtures.
- **Network.** The fetcher talks only to the configured GitHub API base
  (default `https://api.github.com`) and `codeload.github.com` (archive
  redirects). Tests use an `httptest` fake GitHub. No other hosts.
- **Untrusted content.** Skill files are data. toolyard never executes them,
  never follows symlinks in archives, rejects absolute paths and `..`, caps
  sizes, and blocks files that look like secrets.
- **Audit.** Every tool call is audited by the gateway as today. Non-tool
  reads (MCP `skills/get`, `resources/read` of `skill://`, REST) write an
  audit event too. Every answer names the source and the commit
  (`owner/repo@<12-hex>`), so the audit row records which version was read.
- **Compatibility.** Existing tools, REST routes and tokens keep working. The
  `-skills` flag stays: `on` (default) or `off`; `auto` means `on`. The flags
  `-skills-dir`, `-skills-sync-interval` and `-claude-skills-dir` are still
  accepted, ignored, and log one deprecation line. Production sets
  `TOOLYARD_SKILLS=off`, so the hub stays off there until the stack enables it.
- **Docs.** `docs/skills-hub.md` (new: admin and agent guide),
  `docs/self-hosting.md` (flags, settings, webhook), `inbox.guide` topics if
  the Inbox changes, and the server instructions.
- **Verification.** No builds, tests, vet or lint on the dev host. Run only
  `gofmt -l`/`gofmt -w` on the files you change. CI runs the tests
  (`.github/workflows/inbox-integration.yml`, SQLite and Postgres jobs) on
  your draft pull request; read failures with `gh run view <id> --log-failed`.
- **Ownership.** Touch only the files of your slice (below). The shared
  types live in `internal/skillhub/api.go`; change it only with the
  orchestrator. `cmd/gateway/main.go`: each slice may add its own wiring
  block, marked `// skillhub (slice X)`; the orchestrator resolves conflicts.

## Shared types: `internal/skillhub/api.go`

`internal/skillhub/api.go` holds the types and the three interfaces. Slice A
implements `Reader` and `Admin` with `*skillhub.Service`; slices B and C
program against the interfaces and test with fakes, so every slice compiles
on its own.

- `Reader` — what agents use (B calls it).
- `Admin` — what admins use (C calls it).
- `ReviewHook` — A calls it when a `review` source has a new staged version;
  C implements it (Inbox item).
- `Proposals` — B's `skills.propose_source` / `skills.propose_change` call it;
  C implements source proposals, D implements change proposals.

## A. Store and sync engine (owner: slice A)

Files: `internal/skillhub/**` except `api.go`, `review*.go`, `proposal*.go`;
migrations `0068`–`0069`; `cmd/gateway` wiring of the service, the leader
loop (`skills-sync`) and the flag; removal of `internal/skills` and of the old
wiring in `cmd/gateway`.

1. **Tables** (names fixed; columns are A's choice): `skill_sources`,
   `skill_versions`, `skill_entries` (one row per skill per version:
   path, name, description, frontmatter JSON verbatim, runs, SKILL.md
   digest), `skill_files` (one row per file per version: skill path,
   relative path, sha256, size, media type), `skill_blobs` (content by
   sha256, stored once), `skill_usage` (reads per skill per day),
   `skill_runs_overrides`.
2. **GitHub fetcher** (`internal/skillhub/github*.go`): parse URLs
   (`owner/repo`, `https://github.com/owner/repo(.git)`,
   `.../tree/<branch>/<path>`, `.../blob/<branch>/<path>/SKILL.md`);
   repository info and branches (paginated, cap 1,000); branch head with
   `Accept: application/vnd.github.sha` and `If-None-Match` (ETag stored per
   source); full download with `GET /repos/{o}/{r}/tarball/{sha}` streamed
   and filtered (never buffer the whole archive); incremental update with
   `GET /repos/{o}/{r}/compare/{base}...{head}` and raw contents of changed
   files when the change touches ≤ 100 files under the source folder,
   otherwise a full download. Honour `Retry-After` and
   `X-RateLimit-Remaining`. Auth: none, a sealed token (`Bearer`), or a
   `TokenSource` func (slice D plugs in the owner's GitHub connection).
3. **Discovery and validation**: every folder under the source folder with
   a `SKILL.md`. Parse YAML frontmatter; require `name` and `description`;
   warn (and still serve) for spec violations other than the name/folder
   mismatch (skip); warn for description > 1024 chars. Exclude globs, plus
   built-in excludes (`.git/`, `node_modules/`, `__pycache__/`, `*.pyc`,
   `.DS_Store`). Text files only (no NUL in the first 8 KiB, valid UTF-8);
   other files are omitted and listed as omitted. Limits per source
   (defaults): 1 MiB per file, 50 MB in total, 2,000 skills, 20,000 files;
   over a limit the sync fails with a clear error and the active version
   stays. Secret check: block files that contain private key blocks or
   well-known token shapes (GitHub, AWS, Slack, OpenAI/Anthropic `sk-`),
   omit them, warn.
4. **Versions**: build a version in one transaction (blobs first, then
   rows), compute a diff against the active version (skills added, changed,
   removed, by SKILL.md digest and file set), then per mode: `auto` →
   activate; `review` → keep staged and call `ReviewHook.PendingVersion`;
   `pinned` → only the pinned SHA is ever fetched. A skill contributor's
   source that is not approved keeps every new version staged, whatever
   its mode (section F). Activation publishes on `skillhub`. Retention
   after each sync.
5. **Loops**: the leader checks each enabled source when its
   `poll_seconds` (default 300, min 60) has passed since the last check;
   `SyncNow` and `HandleWebhook` run a sync at once on the calling node
   under the per-source lock. A failed sync records `last_error` and backs
   off (doubling, max 1 h).
6. **Reader**: search uses `internal/toolsearch` (keyword BM25 and RE2
   regex) over name, description and path; grep scans file contents of the
   caller's visible active versions with RE2 (caps: 200 hits, 5,000 files,
   20 MB scanned, 2 s); read supports line `offset`/`limit`; get returns the
   body and the complete file list with `sha256:<hex>` digests (SEP-2640).
   Reads count usage. `Bundle` writes a `.tar.gz` of the chosen skills in the
   Agent Skills layout (one folder per skill), for Claude Code on a laptop.
7. **Webhook**: `HandleWebhook` checks `X-Hub-Signature-256` (HMAC-SHA256
   with the source's webhook secret, constant-time), accepts `ping` and
   `push` for the source's branch, and starts a sync.

## B. Agent surface (owner: slice B)

Files: `internal/gateway/builtin_skills.go` (rewritten), new
`internal/gateway/skills_*.go`, `internal/api/skills_routes.go` (agent
REST), the `tools.search` integration and the server instructions in the
gateway, `internal/access` (move `skills` from `BuiltinGroups` to the
always-on groups), the `ty` client script if it has a skills command, and
their tests.

1. **Tools** (always on; they show only what the caller may see):
   - `skills.search` — `query`, `mode` (`keyword` default | `regex`),
     `source`, `limit` (default 10, max 50), `include_host`.
   - `skills.get` — `skill` (URI, `slug/skill-path`, or a bare name when it
     is unique), `include_files` (default true: the file list, not bytes).
   - `skills.read` — `uri`, `offset`, `limit` (lines).
   - `skills.grep` — `pattern` (RE2), `source`, `path` (glob), `ignore_case`,
     `limit`.
   - `skills.sources` — the sources the caller sees.
   - `skills.list` — paginated listing (`cursor`, `limit`), same entries as
     MCP `skills/list`.
   - `skills.propose_source` — `url`, `branch`, `path`, `reason`; calls
     `Proposals.ProposeSource`; returns the pending proposal.
   - `skills.propose_change` — `skill`, `files` (path → new content; empty
     content deletes), `title`, `summary`; calls `Proposals.ProposeChange`.
   - `skills.bundle` — `skills` (list); returns a short-lived download URL
     for a `.tar.gz` (REST below).
   Each result carries `source`, `repo`, `commit` and `uri`. Results are JSON
   text plus `structuredContent`.
2. **`tools.search`**: when the caller sees skills, add up to 3 matching
   skills under a `skills` key (name, description, uri). **Server
   instructions**: one short paragraph saying the skills catalog exists and
   to call `skills.search` before a multi-step task.
3. **Code mode**: the tools are callable from Starlark and JavaScript like
   other built-ins, with stubs.
4. **REST** (agent tokens, same auth as `/v1/jobs`): `GET /v1/skills/search`,
   `GET /v1/skills/get`, `GET /v1/skills/read`, `GET /v1/skills/grep`,
   `GET /v1/skills/sources`, `GET /v1/skills/list`, `POST /v1/skills/bundle`
   (returns a URL) and `GET /v1/skills/bundle/{token}` (the file; token
   single-use, 10 minutes).
5. **MCP Skills extension (SEP-2640)**: declare
   `capabilities.extensions["io.modelcontextprotocol/skills"] = {}` on both
   protocol versions; implement `skills/list` (cursor, the caller's visible
   skills; entries `{uri, frontmatter, resources: [{uri, digest, size}]}`),
   `skills/get` (`{skill: entry}`; an unknown or invisible URI is an error),
   `resources/read` for `skill://` URIs (text contents, `mimeType`), and
   `resources/directory/read`; `resources/templates/list` may advertise
   `skill://{path}`. mcp-go has no hook for custom methods: route these in
   toolyard's MCP HTTP handler before mcp-go, as the 2026-07-28
   `server/discover` handling does.
6. **Viewer**: build `skillhub.Viewer` from the caller: user, agent, admin
   role, and `OnHost` (true for T3 host-connection and federation agents,
   which run on the T3 host; false for OAuth clients such as Claude.ai and
   ChatGPT, and for ordinary agent keys unless their client says so).

## C. Admin surface and review (owner: slice C)

Files: `internal/api/skills_admin_routes.go`, `internal/skillhub/review*.go`,
`internal/skillhub/proposal*.go` (source proposals), migration `0070`,
`web/dashboard/skills.js` (new) and the small nav/route edits in
`web/dashboard/app.js`, `index.html`, `style.css`; removal of the old skills
UI in `app.js`.

1. **REST** (admin or owner role; the existing role guard):
   - `POST /v1/skills/sources/inspect` — `{url, token?}` → owner, repo,
     private, default branch, branches, branch and path parsed from the URL.
   - `POST /v1/skills/sources/preview` — a `SourceInput` → `Preview` (dry run:
     commit, skills found, warnings, sample, sizes). Nothing is stored.
   - `GET /v1/skills/admin/sources` (every source, for admins),
     `POST /v1/skills/sources` (create; a `GET /v1/skills/sources` with an
     agent bearer is the agent listing, slice B),
     `GET/PATCH/DELETE /v1/skills/sources/{id}`.
   - `POST /v1/skills/sources/{id}/sync`,
     `GET /v1/skills/sources/{id}/versions`,
     `POST /v1/skills/sources/{id}/versions/{vid}/activate` (rollback or
     review approval), `POST .../versions/{vid}/reject`.
   - `GET /v1/skills/sources/{id}/skills` (with runs and 30-day reads),
     `PUT /v1/skills/sources/{id}/skills/runs` (`{path, runs}`; empty runs
     clears the override).
   - `POST /v1/skills/sources/{id}/webhook` — public, signature-checked
     (`Admin.HandleWebhook`); `POST /v1/skills/sources/{id}/webhook-secret`
     — admin; rotates and returns the secret once, with the payload URL.
2. **Review hook**: implement `ReviewHook` with one Inbox item for the
   source's creator: title, Markdown summary (skills added / changed /
   removed, commit link, warnings), one question "Make this version live?"
   (approve / reject). The decision calls `Admin.Activate` or
   `Admin.Reject`. Use the Inbox's own mechanism for server-created items and
   their decisions (find it; do not fake an agent). If the Inbox cannot call
   back into server code, the dashboard's Approve/Reject buttons are the
   decision and the Inbox item is a notice with a link; say so in the PR.
3. **Source proposals**: `Proposals.ProposeSource` creates an Inbox item for
   the agent's owner with the repository, branch, folder, the preview
   (counts, warnings) and the agent's reason; approval creates the source
   (`Admin.CreateSource`, mode by the trusted-owner rule). Store proposals
   in `skill_proposals` (migration `0070`).
4. **Dashboard**: a **Skills** page.
   - Sources list: slug, repo@branch, commit, mode, status (ok, syncing,
     pending review, error), skills count, last check.
   - **Add source** wizard: paste URL → Inspect → branch dropdown (default
     branch preselected) → folder (prefilled from the URL) → for a private
     repository a token field (write-only) or "use my GitHub connection"
     when slice D enables it → **Preview** (commit, counts, warnings, sample)
     → mode (default by the trusted-owner rule) → visibility (everyone /
     chosen members) → default runs → Save (runs the first sync).
   - Source page: commit with a GitHub link, last and next check, last
     error, Sync now, Pause/Resume, Edit, Delete; versions table with
     Activate (rollback) and, for a staged version, Approve/Reject;
     warnings; skills table (name, path, runs with an override toggle,
     reads in 30 days); webhook setup (payload URL, Rotate secret, steps).
   Match the existing dashboard style and code (vanilla JS, `app.js`
   patterns, dashboard tests if any).

## D. Phase 3 (owner: slice D, after A–C are merged)

1. `Proposals.ProposeChange`: an Inbox approval by the source's creator,
   then a branch, a commit and a pull request on the source repository
   through the creator's toolyard GitHub connection (the existing
   `internal/githubapi` user-token surface; extend it with the minimal
   contents and pull-request calls). GitHub writes keep the existing rule:
   they run only after the account owner's explicit approval.
2. `AuthGitHub`: a source may read with its creator's GitHub connection
   (`TokenSource`), shown in the wizard when the creator has one.
3. `ty skills pull <skill>...`: download a bundle into `~/.claude/skills`.
4. Docs for all of the above.

## F. Sharing and contributors (worker F2 and the review fixes)

1. **Who is a viewer.** The sharing rule is `srcIndex.skillVisible`
   (`index.go`); every read path asks it. `Viewer.Delegated` is false
   only for a person in their own dashboard page (a `dashboard:<uid>`
   caller with the browser raiser, `gateway/skills_viewer.go`); every
   other caller is delegated, a caller with no ID at all (anonymous
   `/mcp`, stdio) included, so audience nobody, a hidden source and a
   source that is not approved stay out of every agent's view.
2. **Contributors read anonymously.** Every GitHub read made for a skill
   contributor is anonymous unless it carries their own token or GitHub
   connection; the admins' default token (`skills_github_token`) is never
   used for them. The API derives this in one place
   (`skillSourceInput`: `SourceInput.PendingApproval` for preview and
   create); inspect without a token is `InspectAnonymous`; a stored
   source of a contributor reads through `clientFor`, which is anonymous
   for any source with an approval state and no credential of its own
   (sync now, webhook and scheduled syncs, `PreviewSource`). A
   contributor manages only sources with an approval state
   (`skillManager.owns`, on every source route and the list): a source
   they added while an admin (`ApprovalNone`, read with the default token,
   no approval) stays admin-managed after they lose the role.
3. **Approval is bound to a commit.** While a contributor's source is
   not approved, a sync stages every new version (as review mode) and
   never activates one; a staged version found again for the source's
   commit and settings (the folder went A, B, A) becomes the waiting
   version again, and `UpdateSource` drops the waiting version when the
   branch, pinned commit, folder, excludes or limits change. A question
   is bound to a version: its input keeps the commit, the waiting version
   ID (none for the dry run of a new or changed source) and the settings
   digest (`configHash`); it is current for a waiting version it names, or
   whose commit and settings it shows. Every ask runs in
   `lockedApproval`: one transaction that holds the source's row, reads
   the source, its waiting version and its open questions again, and
   inserts only what still holds, so a caller's outdated source or
   version (a sync that read it before a settings change) asks nothing,
   a dry run of settings the source no longer has asks nothing, the card
   shows the source as locked, and two askers on any replica never both
   add one. At every check the hook's
   `ApprovalHook.PendingApprovalVersion` keeps one open question that is
   current for the waiting version: it asks a new one first and then
   closes the others, closes leftovers next to a current one, asks when
   none is open (the first ask failed, it expired), and waits while an
   answer is being applied. An answer refused for its version asks again
   for the waiting version (none, or one built with old settings: the sync
   that stages the current one asks).
   `DecideApproval(approve, sha)` (the source page) makes the waiting
   version at `sha` with the source's current settings live (before the
   approval is stored), and refuses with `ErrConflict` when another
   version would go live: a waiting version of an automatic or pinned
   source that does not match, a live version not at `sha`, or an
   automatic or pinned source with nothing synced. The Inbox decides with
   `DecideApprovalFrom(..., ApprovalPending, ..., binding)`, the binding
   being the question's commit, version and settings digest: only a
   waiting (or live) version that matches all of them goes live, so a
   card never approves a version built with other settings; and a late
   answer never undoes a decision made on the source page (the question
   is settled as cancelled). The approval is stored only while the live
   and waiting versions are the ones checked.
4. **At most three waiting sources per contributor.** `CreateSource`
   counts and inserts under `cluster.LockCtx(ctx,
   "skillhub:contributor:"+userID)`, so parallel creates on any replica
   (Postgres) or on one node (SQLite) cannot pass the cap; the API's own
   count only answers early, before the dry run.
5. **Source updates.** `UpdateSource` writes only the fields the update
   names (the hide lock changes only through the admin hide path), only
   while `updated_at` is still the value it read, and moves `updated_at`
   with `bumpUpdatedAt`; a write in between makes it read again (at most
   five rounds), and a change of the approval, hide lock, paused state,
   mode, branch, folder, excludes, pinned commit, limits, read access or
   token in between answers `ErrConflict`. The API passes the `updated_at`
   of the source its checks read (`SourceUpdate.IfUpdatedAt`): a source
   that changed since is `ErrConflict` too, so a check never applies to a
   newer source.

## E. Integration (orchestrator)

Merge order A → B → C → D into `t3/skills-hub`; `cmd/gateway` conflicts;
the Kubernetes test overlay sets `TOOLYARD_SKILLS=on`; deploy the PR image to
the test cluster; end-to-end checks with `anthropics/skills` (public) and
bk-docs (private, a read-only token entered by an admin); then the
integration pull request to `main`.
