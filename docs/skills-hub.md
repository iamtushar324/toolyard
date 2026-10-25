# Skills hub

toolyard serves [Agent Skills](https://agentskills.io/specification) from
GitHub repositories to every client: MCP tools, code mode, REST and the MCP
Skills extension ([SEP-2640](https://modelcontextprotocol.io/seps/2640-skills-extension)).
An admin adds a repository and a branch once; toolyard copies the skills in
it, at an exact commit, into its database, keeps that copy current, and
serves only the copy. Agents never call GitHub.

Operators: flags, the trusted-owner setting, network and webhook setup are
in [self-hosting.md](self-hosting.md#skills-hub). The design and the
contract between the parts are in
[design/skills-hub.md](design/skills-hub.md).

## Terms

- **Source**: one GitHub repository, a branch and a folder in it (the
  repository root when empty). Its *slug* (`skills`, `team-kit`) starts
  every URI of its skills.
- **Skill**: a folder that holds a `SKILL.md`, inside the source folder.
  Its path is the folder's path relative to the source folder
  (`agent-tooling/pr-merge-ready`), and its URI is
  `skill://<slug>/<path>/SKILL.md`; its other files are
  `skill://<slug>/<path>/<file>`. A `SKILL.md` at the source folder itself
  is a skill too: its URI is `skill://<slug>/SKILL.md` and its name must be
  the source folder's name (the repository's, for the root).
- **Version**: the skills of a source at one commit. A version is
  `staged` (built, not served), `active` (served; one per source),
  `retired` (was active), `rejected` (a reviewer said no) or `failed` (the
  commit broke a rule, such as a limit; nothing of it is served).
- **Update mode**: `auto` (a new commit is checked and goes live), `review`
  (a new commit becomes a staged version that waits for an admin), or
  `pinned` (one commit, `pinned_sha`, is served; new commits are ignored).
  A new source defaults to `auto` when its owner is in the setting
  `skills_trusted_github_owners`, else to `review`.

## For admins

The skills hub is on with `-skills=on` (`TOOLYARD_SKILLS=on`). Everything
below needs the owner or an admin; members do not see the Skills page.

### Add a source

Open **Skills** in the dashboard and choose **Add source**:

1. Paste the repository: `owner/repo`, `https://github.com/owner/repo`, or a
   link to a folder (`https://github.com/owner/repo/tree/<branch>/<folder>`).
   **Inspect** reads the repository and its branches.
2. Pick the branch (the default branch, or the one in the link, is
   preselected; type to filter when there are many) and the folder (taken
   from the link; empty means the repository root). Every folder under it
   that holds a `SKILL.md` becomes a skill.
3. For a private repository, paste a read-only token: a fine-grained token
   with **Contents: read** on that repository is enough. The token is
   write-only: toolyard stores it sealed, uses it only to read that
   repository, and never shows or logs it again.
4. **Preview** is a dry run: the commit, how many skills and files it found,
   the warnings grouped by kind, and some of the skills. Nothing is stored.
5. Choose the update mode, who sees the source, and where its skills run by
   default, then **Save**. toolyard adds the source and runs its first sync
   at once; the dialog shows what the sync did.

### Update modes and review

- **Automatic**: a new commit that passes validation goes live for agents.
  This is the default when the repository owner is in the setting
  `skills_trusted_github_owners` (a comma-separated list of GitHub owners).
- **Review**: a new commit becomes a staged version that waits for approval.
  This is the default for every other owner. The person who added the
  source gets one Inbox question, "Make this version live?", with the
  skills added, changed and removed, a link to the commit on GitHub and the
  number of warnings. **Approve** makes the version live; **Reject** (or a
  deny) keeps the live version, and the next commit asks again. The same
  version also shows **Approve** and **Reject** on the source page; a
  decision in either place closes the other. Only the newest staged commit
  waits: a newer commit closes the older question. Only an admin can make a
  version live, so a person who lost the role since cannot.
- **Pinned**: toolyard serves one commit and ignores new ones.

The source page lists the last versions. **Activate** on an older version
rolls back to it at once.

### Who sees a source, and where skills run

**Everyone** shows the source to every active member and their agents;
**chosen members** to the people you pick (and their agents). Admins and
the owner always see every source.

A skill **runs anywhere** (Claude.ai, ChatGPT, laptops, the T3 host) or
**on the T3 host only** (it uses the host's paths and tools; agents
elsewhere see it only when they ask for host skills). The order of
precedence: the override on the source page's skills table, then the
skill's own `metadata.toolyard-runs`, then the source default.

### Webhook

Without a webhook toolyard checks each branch every few minutes (5 by
default). With one, a push reaches agents (or the review question) within
seconds. On the source page, open **Webhook**:

1. **Make a secret**. toolyard shows it once; **Rotate secret** makes a new
   one and the old one stops working.
2. In the repository on GitHub, open **Settings → Webhooks → Add webhook**.
3. **Payload URL**: the URL the page shows,
   `<public URL>/v1/skills/sources/<source id>/webhook`.
4. **Content type**: `application/json`. **Secret**: the secret from step 1.
5. **Which events**: just the push event. GitHub then sends a ping, which
   toolyard accepts.

The webhook route needs no session: GitHub's `X-Hub-Signature-256`
signature over the body is the credential, and a delivery with a wrong
signature gets `401`. Bodies are capped at 5 MiB.

### Agents propose sources

An agent can ask for a source with `skills.propose_source` (a URL, branch,
folder and its reason). toolyard previews the source and puts one question
in the Inbox of the agent's owner, with the repository, branch, folder,
counts, warnings and the agent's reason. **Approve** adds the source (mode
by the trusted-owner rule) and runs its first sync; **Reject** adds nothing.
The agent polls the outcome with the proposal ID. Only an agent whose owner
is an admin can propose, a proposal never carries a token (add a private
repository yourself on the Skills page), and one agent may have at most
five proposals waiting.

### Admin REST

Session-authenticated like the rest of the dashboard API (owner or admin;
operator tokens need the owner scope for writes and never rotate a webhook
secret). Errors are `{"error", "code"}` with the codes `invalid` (400),
`not_found` (404), `conflict` (409), `needs_token` and `limit` (422),
`skills_hub_off` (404 while the hub is off).

| Method and path | What it does |
| --- | --- |
| `POST /v1/skills/sources/inspect` | `{url, token?}`: owner, repo, private, default branch, branches, the branch and folder in the URL, `trusted_owner` |
| `POST /v1/skills/sources/preview` | a source (as for create): dry run with commit, counts, warnings, sample |
| `GET /v1/skills/admin/sources` | every source with its status (the agent list is `GET /v1/skills/sources`) |
| `POST /v1/skills/sources` | create `{url, branch?, path?, exclude?, mode?, pinned_sha?, auth?, token?, poll_seconds?, enabled?, visibility?, members?, default_runs?}`; `201 {source, sync}` after the first sync |
| `GET`, `PATCH`, `DELETE /v1/skills/sources/{id}` | read, change (only the fields sent; `token` replaces, `clear_token` removes) or delete a source |
| `POST /v1/skills/sources/{id}/sync` | sync now |
| `GET /v1/skills/sources/{id}/versions?limit=` | the last versions (default 20, at most 100) |
| `POST /v1/skills/sources/{id}/versions/{vid}/activate` | make a version live (approve or roll back) |
| `POST /v1/skills/sources/{id}/versions/{vid}/reject` | `{note?}`: reject a staged version |
| `GET /v1/skills/sources/{id}/skills` | skills with where they run and reads in 30 days |
| `PUT /v1/skills/sources/{id}/skills/runs` | `{path, runs}`: `anywhere`, `host`, or `""` to clear the override |
| `POST /v1/skills/sources/{id}/webhook-secret` | a new webhook secret, once, with the payload URL |
| `POST /v1/skills/sources/{id}/webhook` | GitHub's push webhook (public, signed) |

Every change is in the audit log (`skills.source_create`,
`skills.source_update`, `skills.source_delete`, `skills.source_sync`,
`skills.version_activate`, `skills.version_reject`,
`skills.runs_override`, `skills.webhook_secret_rotate`,
`skills.source_propose`, `skills.source_proposal_reject`). No response and
no audit row carries a token; the webhook secret appears only in the rotate
response.

### How the Inbox carries these questions

Review questions and source proposals are Inbox *system items*: questions
that toolyard itself raises (`internal/inbox/system.go`). A review question
comes from the sender "toolyard" and goes only to the person who added the
source; a proposal comes from the proposing agent and goes to its owner.
When the answer is recorded, toolyard applies it at once and adds the
outcome ("Version … is live.", or why it could not) to the item's timeline.
If toolyard stops between the two, the decision stays recorded: a proposal
is applied when the agent next polls it, and a staged version keeps its
**Approve** and **Reject** buttons on the source page.

## For agents

### What you see

The catalog shows each caller only the skill sources its owner may see
(an agent sees what its owner sees; admins see every source). A skill
marked `runs: host` needs the T3 host (its paths and `tools/*.sh`): agents
that run on the host (T3 host connections and federated T3 environments)
see those skills; everyone else (ordinary agent keys, identity keys, OAuth
clients such as Claude.ai and ChatGPT) sees them only when a call passes
`include_host: true`. A skill you cannot see answers exactly like one that
does not exist.

Every answer names where the content comes from: `source` (the source's
slug), `repo` (`owner/repo@<12-hex commit>`), `commit` (the full SHA) and
`uri`. Skill URIs follow the MCP Skills extension (SEP-2640):

```text
skill://<source>/<skill path>/SKILL.md     the skill
skill://<source>/<skill path>/<file path>  a file beside it
skill://<source>/<skill path>              its folder
```

Relative links inside a `SKILL.md` resolve against the skill's folder
(`root` in `skills.get`). Skill text is guidance, not permission: every
tool call a skill suggests still goes through policy and the Inbox.

### Tools

The `skills.*` tools are on for every active caller (the always-on
`skills` access group); an owner can still switch them off for one agent
with its deny list, or for everyone with an explicit deny policy. In code
mode they are `toolyard.skills_<name>` (Starlark) and
`tools.toolyard.skills_<name>` (JavaScript).

| Tool | Arguments | Answer |
| --- | --- | --- |
| `skills.search` | `query`, `mode` (`keyword` default, `regex`), `source`, `limit` (10, at most 50), `include_host` | matching skills: name, description, uri, source, repo, commit, runs |
| `skills.get` | `skill` (URI, `source/path`, or a unique bare name), `include_files` (default true) | the SKILL.md body and frontmatter, `root`, and every file with its URI, `sha256:` digest and size |
| `skills.read` | `uri`, `offset`, `limit` (lines) | the file's content; when `truncated`, call again with `offset: next_offset` |
| `skills.grep` | `pattern` (RE2), `source`, `path` (glob), `ignore_case`, `limit` (50, at most 200), `include_host` | matching lines with their file URI, skill and line number |
| `skills.sources` | none | the sources you see, with the commit each serves |
| `skills.list` | `cursor`, `limit` (100, at most 500), `source`, `include_host` | a page of skills as MCP `skills/list` entries, plus `next_cursor` |
| `skills.propose_source` | `url`, `branch`, `path`, `reason` | a pending proposal: an Inbox item for your owner; nothing is added before they approve |
| `skills.propose_change` | `skill`, `files` (path → new content, `""` deletes), `title`, `summary` | a pending proposal against the version you see; after the source owner's approval toolyard opens a pull request |
| `skills.bundle` | `skills` (1 to 50) | a `.tar.gz` download URL: one use, 10 minutes, no token needed |

Before a multi-step task, call `skills.search` with a few words about the
task; when a skill matches, read it with `skills.get` and follow it.
`tools.search` also names up to three matching skills under `skills`.

To install skills in a local client such as Claude Code:

```sh
# url from skills.bundle (or POST /v1/skills/bundle); it is the credential
curl -sf -o skills.tar.gz "$url" && mkdir -p ~/.claude/skills && tar -xzf skills.tar.gz -C ~/.claude/skills
```

MCP clients can also read skills as resources through the MCP Skills
extension: see [MCP Skills extension](#mcp-skills-extension).

### REST

The same tools over HTTPS, with an agent token or a session token as the
bearer (operator tokens are refused). Each route runs the tool through the
gateway's call pipeline, so access, policy, audit and metrics are those of
the tool call, and the body is the tool's structured answer.

| Route | Tool |
| --- | --- |
| `GET /v1/skills/search?query=&mode=&source=&limit=&include_host=` | `skills.search` |
| `GET /v1/skills/get?skill=&include_files=` | `skills.get` |
| `GET /v1/skills/read?uri=&offset=&limit=` | `skills.read` |
| `GET /v1/skills/grep?pattern=&source=&path=&ignore_case=&limit=&include_host=` | `skills.grep` |
| `GET /v1/skills/sources` | `skills.sources` |
| `GET /v1/skills/list?cursor=&limit=&source=&include_host=` | `skills.list` |
| `POST /v1/skills/bundle` `{"skills": [...], "reason": "..."}` | `skills.bundle` |
| `GET /v1/skills/bundle/{ticket}` | the `.tar.gz` (no token: the ticket is the credential) |

Each GET takes `?reason=` for the audit record (20 to 2000 characters;
the default names the route). An unknown query parameter is refused.
Refusals are `{"error": "...", "code": "..."}`: `not_found` 404,
`conflict` 409, `invalid` 400, `limit` 422, `unavailable` 503 (also when
the hub is off), and 403 when the gateway refused the call (an explicit
deny policy, the agent's own limits).

The `ty` command (`GET /v1/code/ty`) wraps the read routes and keeps the
token out of argv and logs:

```sh
ty skills search deploy
ty skills get skill://team/ops/deploy/SKILL.md
ty skills read skill://team/ops/deploy/references/checklist.md --raw
ty skills grep 'rollback' --path '**/*.md'
ty skills sources
ty skills list --limit 50
```

## How sync works

1. **Check.** The cluster leader looks every 15 seconds for sources whose
   check is due: each enabled source is checked every `poll_seconds`
   (default 300, at least 60). The check asks GitHub for the branch head
   with the ETag of the last answer; an unchanged branch answers
   `304 Not Modified`, which costs no rate limit. A GitHub push webhook, or
   *Sync now* on the dashboard, checks at once on the replica that got it.
   A pinned source never asks for the branch head.
2. **Lock.** A sync of a source runs under a cluster lock named after the
   source, so two syncs of one source never overlap, whichever replicas
   start them.
3. **Known commit?** When the commit already has a version built with the
   source's current settings, nothing is downloaded: an active version
   stays; a staged one keeps waiting for review (an `auto` source makes it
   live); a retired one stays retired (a rollback holds until a new commit
   arrives); a rejected one stays rejected; a failed one stays failed until
   *Sync now* tries it again. A pinned source makes its commit's version
   live whatever its state.
4. **Fetch.** toolyard builds the new version from the active one when it
   can: it asks GitHub which files changed between the two commits and
   reads only those (the contents API), when the active version was built
   with the same folder, excludes and limits, the new commit descends from
   it, at most 100 files changed under the source folder, and no skill
   appears or goes away (no `SKILL.md` added, removed or renamed). Anything
   else downloads the commit's archive (`/tarball/<sha>`, a gzip'd tar from
   `codeload.github.com`). The archive is read as a stream: only text files
   under the source folder are kept, and only within the limits below. A
   private repository's token goes to the GitHub API only, never to the
   archive host.
5. **Check the files.** Every file under the source folder goes through
   the same checks:
   - built-in excludes (`.git/`, `node_modules/`, `__pycache__/`, `*.pyc`,
     `.DS_Store`) and the source's exclude globs (.gitignore style: a glob
     without `/` matches a name at any depth, one with `/` matches from the
     source folder, a trailing `/` matches folders, `**` crosses folders);
   - text only: no NUL byte and valid UTF-8 (images and other binaries are
     left out);
   - no links: symbolic links, hard links and devices are left out, never
     followed; an archive entry with an absolute path or a `..` segment
     fails the whole sync;
   - no secrets: a file that holds a private key block or a well-known
     token shape (GitHub `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`/`github_pat_`,
     AWS `AKIA…`/`ASIA…`, Slack `xox…`, Anthropic `sk-ant-…`, OpenAI
     `sk-proj-…` and long `sk-…` keys) is left out with a `secret`
     warning, which never repeats the match.
   A file left out inside a skill folder is listed in the skill's
   `omitted` list with the reason.
6. **Find the skills.** Every folder with a `SKILL.md` is a skill. Its YAML
   frontmatter is stored verbatim as JSON (SEP-2640). A skill is skipped,
   with a warning, when its frontmatter is missing or not valid YAML, it
   has no `name` or no `description`, or its `name` differs from its
   folder's name. Other departures from the specification are warnings
   and the skill is still served: a name that is not lowercase letters,
   digits and hyphens, a description over 1,024 characters, a
   compatibility note over 500, fields the specification does not define,
   or a `metadata.toolyard-runs` other than `host` or `anywhere`. A skill
   inside another skill's folder is a skill of its own, and its files are
   also files of the outer skill.
7. **Store.** The version is written in one transaction: each file's bytes
   once by SHA-256 (shared by every version that holds the same bytes),
   then the version, its skills and its files. Digests are
   `sha256:<64 lowercase hex>` of the raw bytes. toolyard also records the
   difference from the active version: skills added, changed (a different
   `SKILL.md` or file set) and removed.
8. **Go live.** `auto` and `pinned` sources make the version active at
   once; `review` sources keep it staged and send the source's creator an
   Inbox item. Activation is one transaction (the old version becomes
   retired) and is announced on the cluster: every replica reloads that
   source into its in-memory catalog within moments. Replicas also compare
   their catalog with the database every 30 seconds (Postgres) and every 5
   minutes, so a missed message heals by itself.
9. **Keep.** Each source keeps its active version, the version waiting for
   review, and the 10 newest others; older versions, and file contents no
   version uses any more, are deleted.

A failed sync (GitHub down, a token revoked, a limit) records the error on
the source and leaves the active version as it was. The next check waits
twice as long after each failure in a row, at most an hour; a GitHub rate
limit answer (`Retry-After`, or no calls left until `X-RateLimit-Reset`)
pauses calls with that credential until the time GitHub gives.

An admin can make any staged or retired version active again (a review
approval or a rollback), or reject a staged one. A rollback on an `auto`
source holds until the next commit; pin the source to keep it longer.

## Where a skill runs

A skill runs `anywhere` or on the `host` (it needs the T3 host's paths and
tools). Order of precedence: an admin's override for that skill, then the
frontmatter `metadata.toolyard-runs`, then the source's `default_runs`.
Listings, search and grep leave out `host` skills for callers that are not
on the host unless they ask for them (`include_host`); a direct reference
to a skill (its URI, `slug/path` or its name) still reads it.

## Limits

Each source has these limits; an admin may change them per source within
the ceilings.

| Limit | Default | Ceiling |
| --- | --- | --- |
| Bytes per file | 1 MiB | 16 MiB |
| Bytes of text under the source folder | 50 MB | 1 GB |
| Skills | 2,000 | 20,000 |
| Files under the source folder | 20,000 | 200,000 |

A file over the per-file limit is left out with a `too_large` warning.
The other limits fail the sync with a clear error, record a `failed`
version for that commit, and leave the active version as it was. The byte
and file limits count every text file under the source folder that is not
excluded, inside a skill folder or not: point the source at the folder
that holds the skills, or add exclude globs, to stay well under them. A
repository archive larger than 4 GiB uncompressed is refused.

Reads have their own caps: search returns at most 50 results (10 by
default); a listing page at most 500 skills (100 by default); grep stops
at 200 matching lines, 5,000 files, 20 MB scanned or 2 seconds, and
returns what it found by then; a bundle holds at most 200 skills and
64 MiB.

Each source records reads per skill per day (a `get`, a file `read`, or a
bundle); the dashboard shows the last 30 days.

## MCP Skills extension

While the hub is on, toolyard declares
`capabilities.extensions["io.modelcontextprotocol/skills"] = {"directoryRead": true}`
(with the `resources` capability) on both protocol versions: the
2025-11-25 `initialize` result and the 2026-07-28 `server/discover`
result. It serves:

- `skills/list` (`cursor`): your skills, each entry
  `{uri, frontmatter, resources: [{uri, digest, size}]}` with every file of
  the skill, `SKILL.md` included. 2026-07-28 results carry `ttlMs` and
  `cacheScope: "private"`.
- `skills/get` (`uri` of a `SKILL.md`): one entry, listed or not.
- `resources/read` of any `skill://` file URI (text, with its `mimeType`),
  and `resources/templates/list` with the template `skill://{+path}`.
- `resources/directory/read` (`uri` of a skill folder or a folder in it):
  its direct children; folders have `mimeType: "inode/directory"`.

An unknown URI, or one you cannot see, is JSON-RPC error `-32602` (invalid
params). When a source's active version changes, toolyard sends
`notifications/resources/list_changed`, best effort: a stateless MCP
connection never hears it, so re-list when you need fresh digests.

Reads of skill content outside a tool call (`skills/get`,
`resources/read`, `resources/directory/read`, a bundle download) write a
`skill.read` audit row with the URI and `owner/repo@commit`, as tool calls
do.

## Security

What protects the hub, in one place (the sections above give the
details):

- **Agents never call GitHub.** toolyard serves only its own copy of a
  source at an exact commit, and every answer names it
  (`owner/repo@<commit>`). Skill text is guidance, not permission: every
  tool call a skill suggests still goes through policy and the Inbox.
- **Source tokens** are write-only. toolyard seals them with
  `secrets.key` (bound to the source), uses them only for that
  repository's GitHub API calls, and never sends them to the archive host.
  No response, log line, error, `last_error` or audit row carries one; a
  token in a request is redacted from every error message. Redirects are
  followed by hand and only to `codeload.github.com` or the API's own
  host.
- **Webhooks.** The webhook route takes no session: GitHub's
  `X-Hub-Signature-256` HMAC-SHA256 over the raw body (checked in constant
  time) is the credential, and a wrong signature gets `401`. The secret is
  sealed like a token and shown once, on rotation. The route reads no
  cookie, so it is exempt from the CSRF header and the Origin check; it
  has a 5 MiB body cap and a per-IP limit (1,200 an hour).
- **What gets copied.** Text files only; symbolic links, hard links and
  devices are never followed; an archive path that is absolute or has a
  `..` segment fails the whole sync; a file that holds a private key or a
  well-known token shape is left out (the warning never repeats the
  match); a repository archive over 4 GiB uncompressed is refused
  ([How sync works](#how-sync-works), [Limits](#limits)).
- **Who may change what.** The admin routes and the Skills page are for
  the owner and admins only (the role guard and each handler check).
  Operator tokens need the owner scope for writes and can never rotate a
  webhook secret. Only an admin can make a version live; a review question
  goes only to the person who added the source, and is applied only while
  they are still an admin.
- **Who may read what.** The agent routes take an agent or session token
  as the bearer and never read a cookie; operator tokens are refused. Each
  caller sees only the sources its owner may see, and a skill it cannot
  see answers exactly like one that does not exist. An owner can switch
  the skills tools off for one agent (its deny list) or for everyone (an
  explicit deny policy).
- **Proposals.** Only an agent whose owner is an admin may propose a
  source, a proposal never carries a token, one agent may have at most
  five waiting, and nothing is added before the owner approves. Agents
  cannot raise toolyard's own Inbox questions (system items).
- **Bundle downloads.** The ticket in the URL is 256 random bits, works
  once, for 10 minutes, and is stored only as its SHA-256; a caller holds
  at most 20 live tickets, visibility is checked again at download, and
  downloads have a per-IP limit (600 an hour).
- **Audit.** Every admin change is audited (`skills.*`), and every read of
  skill content outside a tool call writes a `skill.read` row with the URI
  and `owner/repo@commit`.
