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
below needs the owner or an admin, except what [skill
contributors](#contributors) may do with their own sources; other members
do not see the Skills page.

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
   repository, and never shows or logs it again. If you have connected
   GitHub in **My connections**, you can choose **Use my GitHub
   connection** instead ([GitHub connection](#github-connection)).
4. **Preview** is a dry run: the commit, how many skills and files it found,
   the warnings grouped by kind, and some of the skills. Nothing is stored.
5. Choose the update mode, who sees the source, and where its skills run by
   default, then **Save**. toolyard adds the source at once and starts its
   first sync in the background (a large repository can take a minute);
   the dialog follows it and shows what the sync did. Saving the same
   repository, branch and folder again within two minutes (a retry, a
   double click) returns the source already made instead of a second one.

### Update modes and review

- **Automatic**: a new commit that passes validation goes live for agents.
  This is the default when the repository owner is in the setting
  `skills_trusted_github_owners` (a comma-separated list of GitHub owners).
- **Review**: a new commit becomes a staged version that waits for approval.
  This is the default for every other owner. The person who added the
  source gets one Inbox question, "Make this version live?", with the
  skills added, changed and removed, a link to the commit on GitHub and the
  number of warnings; when that person is no longer an admin (or is
  blocked), the owner gets it instead. **Approve** makes the version live;
  **Reject** (or a deny) keeps the live version, and the next commit asks
  again. The same version also shows **Approve** and **Reject** on the
  source page; a decision in either place closes the other. Only the
  newest staged commit waits: a newer commit, or a change of the source's
  mode, branch, folder or pinned commit, closes the older question, and an
  answer to a question whose version no longer waits is refused (it never
  rolls back a newer live commit; the item says why). Older staged commits
  show as *superseded* on the source page. Only an admin can make a
  version live, so a person who lost the role since cannot.
- **Pinned**: toolyard serves one commit and ignores new ones.

The source page lists the last versions. **Activate** on an older version
that was live rolls back to it at once; a rejected version is never
offered.

### Who sees a source, and where skills run

**Everyone** shows the source to every active member and their agents;
**chosen members** to the people you pick (and their agents); **nobody**
hides it from every agent (admins and the person who added it still see it
in the dashboard). Admins and the owner see every skill that is not set to
nobody. [Sharing](#sharing) below adds a block list and a setting per
skill.

A skill **runs anywhere** (Claude.ai, ChatGPT, laptops, the T3 host) or
**on the T3 host only** (it uses the host's paths and tools; agents
elsewhere see it only when they ask for host skills). The order of
precedence: the override on the source page's skills table, then the
skill's own `metadata.toolyard-runs`, then the source default.

### Sharing

Who sees what is set on the source page, in two places.

- **Sharing** (a panel on the source page): the source's **audience**
  (everyone, only the members you choose, or nobody) and its **block
  list**: people who never see any of its skills, whatever the audience.
- **Hide from agents** (a button on the source page, admins only): a lock
  that hides every skill of the source from every agent, whatever its
  audience or its skills' own settings say. The person who added the
  source cannot clear it. Use it to take a source away from agents at
  once; **Show to agents** clears it.
- **Shared with** (a column of the skills table): one skill's own
  setting. **Same as source** (the default) follows the source; or the
  skill is shared with **everyone**, **only these people**, or **nobody**,
  whatever the source says. A skill can also have its own block list,
  which adds to the source's.

The rule, for each skill:

1. A source an admin hid, and a [contributor's](#contributors) source
   that waits for approval or was rejected, count as audience nobody for
   every skill. This comes first: no skill's own setting can show one.
2. Otherwise the skill's audience is its own setting when it has one,
   else the source's audience.
3. Its block list is the source's block list plus its own.
4. Audience **nobody**: only an admin, or the person who added the
   source, sees the skill, in their own dashboard page. No agent sees it,
   an admin's agent included, and neither does a caller that acts for a
   person (an operator token's CLI, or the voice assistant) or a caller
   that names nobody (an anonymous `/mcp` call on a server without
   `-require-auth-on-mcp`, or stdio).
5. Otherwise a person sees the skill when they are an admin, or when they
   are in the audience (everyone; or listed) and not blocked.
6. An agent, an OAuth app (Claude.ai, ChatGPT) included, sees what its
   owner sees.

A file belongs to the innermost skill that holds it, so a skill set to
nobody inside a shared skill stays hidden: its files are left out of the
outer skill's file list and bundle. A source shows in `skills.sources`
when one of its skills is visible, with the count of visible skills only.
Every read path applies the rule: search, get, read, grep, list, folder
reads, bundles (when the link is made and again when it is used),
sources, the MCP Skills extension, `tools.search`, code mode and REST. A
hidden skill answers exactly like one that does not exist.

A skill's setting is kept by source and skill path, so it survives new
versions; a skill that disappears keeps its setting harmlessly, and it
applies again if the skill comes back. A change reaches every replica
within moments. Every change is in the audit log with who made it and the
setting before and after (`skills.source_sharing`, `skills.skill_sharing`).

### Contributors

An admin can let chosen members add skill sources: the **Skill
contributors** panel on the Skills page (admins only) adds and removes
them. A contributor sees the Skills page in a limited view: the sources
they added as a contributor and the Add source wizard.

- A contributor can inspect, preview and add a source, and edit, sync,
  pause or delete their own sources, their sharing included. Other
  people's sources, the trusted owners, the default GitHub token and the
  Contributors panel stay hidden from them. A source they added while
  they were an admin stays admin-managed when they lose the role (it
  reads with the default token and needs no approval): they no longer
  see or change it.
- Their new source starts **pending approval**: it syncs (at most once an
  hour while it waits), but agents see nothing of it, and no commit goes
  live while it waits, whatever its update mode: each new commit becomes
  its waiting version. The owner gets one Inbox question with the
  repository, branch, folder, audience, and the commit with its counts
  and warnings; when a newer commit (or the same commit with other
  settings) comes to wait, the question is asked again for it (and the
  old one closed). The question always shows the source as it is when it
  is asked. **Approve** is bound to what the question shows: its commit,
  built with the folder, excludes and limits it showed (on the source
  page: the version the page shows, by its commit and ID); exactly that
  version goes live with the approval. With nothing waiting, the question
  and the approval are about the live version (one an admin made live
  while the source waits). When another version would go live instead (a
  newer waiting commit of an automatic source, the same commit with other
  settings, a live commit that is not the one shown, a live version of an
  automatic source built with older settings, or nothing synced yet), the
  approval is refused and the source keeps waiting: the question is asked
  again for the version it waits with (else its live one), or, with
  nothing synced yet or only older settings, by the next sync. A
  review-mode source's newer commit waits for a normal review. The question is never lost: a check
  of a waiting source that finds no open question (the first ask failed,
  or it expired) asks again, and one that finds a question left over next
  to the current one closes it.
  **Reject** disables it, with the reason. A change of branch, folder,
  excludes, credential or update mode while it waits closes the question
  and asks again with a new dry run; a change of branch, folder or
  excludes also drops the waiting version, which the old settings built
  (the next check stages the new one). Admins can also approve or reject on
  the source page (an admin may approve a rejected source there), and
  **hide** or delete any contributor's source at any time. An Inbox answer
  applies only while the source still waits: a late one never undoes a
  decision made on the source page. A contributor may have at most three
  sources waiting at once, also when they add several at the same
  moment.
- Their updates are **review** (each new commit waits for an admin: only
  an admin makes a version live), or **auto** when the repository owner is
  trusted. Only an admin can pin a source, change its pinned commit or
  limits, hide or show it, turn on a rejected source, or change the
  branch, folder or excludes of a source whose commits go live without
  review (pinned, or automatic for an owner who is not trusted).
- They never read with the admins' default GitHub token: without their
  own credential, every read made for them (inspect, preview, add, sync
  now, webhook and scheduled syncs, the dry run of a new approval
  question) is anonymous, so only a public repository works. A private
  repository needs a read-only token or their own GitHub connection.
- A contributor who is removed keeps their sources; only admins can
  change them then. An agent is never a contributor: the list holds
  people, and an operator token needs the owner scope to change it.

### GitHub access and rate limits

Without a token, GitHub allows this server's address 60 API calls an hour,
and for anonymous calls even an unchanged branch (a `304` to the
conditional check) counts. A few public sources checked every five
minutes, plus an admin trying sources, use that up. On the Skills page,
open **GitHub access** and **Set token**: every source without its own
token then reads with it, GitHub allows 5,000 calls an hour, and the
`304`s stop counting. A fine-grained token with read access to public
repositories is enough (give it *Contents: read* on private repositories
if it should read those too, though a private source usually has its own
token). toolyard checks the token with one call GitHub does not count
(`/rate_limit`), refuses a token GitHub rejects, stores it sealed with
`secrets.key` (the setting `skills_github_token`, which no settings API
returns), and never shows or logs it again. **Clear** removes it. A
source's own token, or its creator's GitHub connection (`auth: github`),
always wins over the default.

The panel shows whether a token is set and the current rate limit (calls
left and when it resets). When GitHub answers with a rate limit, every
call with that credential waits until the limit resets: one shared pause
for all the sources without their own token (anonymous, or the default
token), while sources with their own token go on. A rate limit is not
counted as a source's failure, and its next check is when the limit ends.
The admin routes and `skills.propose_source` answer `429` with
`retry_after` (seconds) and `reset_at`, and the dashboard says "GitHub
rate limit reached until HH:MM — add a GitHub token in GitHub access".

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

### GitHub connection

A source can read its repository with the toolyard GitHub connection of
the admin who adds it (source `auth: github`) instead of a token. The
connection is the per-user GitHub server on the Servers page (the
marketplace's "GitHub" entry, a GitHub App that each person connects in
**My connections**). The wizard offers **Use my GitHub connection** only
when you have a connected GitHub server; Inspect, Preview and every later
sync then read with your connection, and toolyard keeps the token fresh
(an expired token is refreshed before use). If you disconnect GitHub, lose
access to the repository or are blocked, the source's syncs fail with a
clear error and the live version stays until you reconnect or give the
source a token.

Only you lend your connection, and only in the dashboard: an operator
token cannot create, preview, inspect or switch a source to `auth:
github` (403), and an admin cannot switch a source that someone else
added to `github` (it would read with that person's connection); give it
a read-only token instead. A source keeps reading with the connection of
the person who added it.

The same connection opens the pull requests of
[change proposals](#change-proposals) for the sources you added. The
toolyard GitHub App must be installed on the repository with these
permissions (the App's settings on GitHub, then accept the new permissions
on each installation):

| Use | Repository permissions |
| --- | --- |
| Read a source (`auth: github`) | **Contents: read**, **Metadata: read** |
| Open change proposals as pull requests | **Contents: write**, **Pull requests: write**, **Metadata: read** |

The App that toolyard's GitHub setup creates asks for Contents: read,
Metadata: read and Pull requests: write; add **Contents: write** in the
App's settings on GitHub if your admins want change proposals. Your
GitHub account must also be able to push branches to the repository.

### Change proposals

An agent that sees a skill can propose an edit with
`skills.propose_change` ([For agents](#change-proposals-for-agents)).
toolyard checks it against the version agents see now and puts one
question, "Open a pull request on owner/repo?", in the Inbox of the person
who added the skill's source. The item shows everything the approval
writes: the proposing agent and its owner, the base commit, the agent's
title and its whole summary, every file path with its operation (added,
changed, deleted), and the complete diff of every added or changed file.
A proposal too large to show in full is refused when the agent makes it,
never shown in part. Nothing reaches GitHub before that person decides.

- **Approve** opens the pull request with *your* GitHub connection: a new
  branch `toolyard/skills/<proposal id>-<random>` from the base commit
  (the random part is never shown to the agent, so nobody can prepare
  that branch), one commit with the files, and a pull request into the
  source's branch, with the agent's title and summary and a footer naming
  the agent and the proposal. Pushing the branch and opening the pull
  request runs the repository's CI on it, as for any pull request.
  Nothing is merged: review and merge it on GitHub like any pull request.
  Once merged, the source picks the commit up like any other (and, in
  review mode, asks you again before it goes live).
- **Reject** (or a deny) sends nothing to GitHub.
- Only you can approve it, in person: in the dashboard, from your own
  notification, or with a passkey. The Inbox refuses an approval from an
  operator token, a batch click, another admin, a rule or a grant, and
  the question stays open for you.
- The pull request carries the summary you read. Its body, its title and
  the commit message are made inert on GitHub: mentions, issue
  references, closing keywords ("Fixes #12"), HTML and images do nothing
  (a zero-width space breaks them; the text reads the same).
- **CI and build files are refused**: anything in `.github/`,
  `.gitlab/`, `.circleci/`, `.buildkite/` and similar folders, and files
  such as `Makefile`, `package.json`, lock files, `Dockerfile`,
  `Jenkinsfile`, `.gitlab-ci.yml`, `go.mod`, `pyproject.toml` or names
  that look like a workflow or pipeline. They would run on the pushed
  branch before anyone reviews the pull request. An admin can allow such
  a file only when it sits directly in the skill's own folder, with the
  setting `skills_change_allow_build_files` (default off); a CI folder
  stays refused.
- toolyard computes the tree you approved (the base commit with the
  proposed files) and adopts nothing else it finds on GitHub: a branch of
  that name must hold exactly that tree on the base commit, and a pull
  request from it must have been opened by your own GitHub account.
  Otherwise the proposal fails and says so. When committing, an added
  file must still be new and an edited one must still exist at the base
  commit.
- If GitHub refuses (the App lacks a permission, the base commit is gone),
  the proposal fails with the reason on the Inbox item. If GitHub's
  answer is lost, toolyard never writes blindly again: the next attempt
  (the Inbox sweep, after a few minutes) first looks for the branch and
  the pull request it may already have made. It stops after ten such
  attempts or a day, and the proposal fails.

### Agents propose sources

An agent can ask for a source with `skills.propose_source` (a URL, branch
and folder, and its reason; nothing else of the agent's input counts: the
mode, visibility and the rest are your defaults). toolyard previews the
source and puts one question in the Inbox of the agent's owner, with the
repository, branch, folder, counts, warnings and the agent's reason.
**Approve** adds the source (mode by the trusted-owner rule) and starts its
first sync in the background; **Reject** adds nothing. The agent polls the
outcome with `skills.proposal` (`GET /v1/skills/proposal?id=`). Only an
agent whose owner is an admin can propose, a proposal never carries a token
(add a private repository yourself on the Skills page), one agent may have
at most five proposals waiting, and at most ten proposals an hour (each is
a dry run against GitHub). A proposal whose question expires (after 7
days) or is withdrawn stops counting as waiting.

### Admin REST

Session-authenticated like the rest of the dashboard API (owner or admin;
operator tokens need the owner scope for writes and never rotate a webhook
secret). A skill contributor may use the routes marked *contributor* for
their own sources (another person's source answers 404); everything else
answers them 403 `admin_only`. Errors are `{"error", "code"}` with the codes `invalid` (400),
`not_found` (404), `conflict` (409), `needs_token` and `limit` (422),
`rate_limited` (429, with `retry_after` in seconds, `reset_at` and a
`Retry-After` header), `skills_hub_off` (404 while the hub is off).

| Method and path | What it does |
| --- | --- |
| `GET /v1/skills/admin/me` | `{user_id, admin, contributor}`: what the signed-in person may do on the Skills page (any signed-in person) |
| `POST /v1/skills/sources/inspect` | *contributor*. `{url, token?, auth?}`: owner, repo, private, default branch, branches, the branch and folder in the URL, `trusted_owner`; `auth: "github"` reads with your GitHub connection (never with an operator token) |
| `POST /v1/skills/sources/preview` | *contributor*. A source (as for create): dry run with commit, counts, warnings, sample. A contributor's dry run never reads with the default token, and has the bounds of their create (403 `contributor_limit`) |
| `GET /v1/skills/admin/sources` | *contributor*. Every source with its status (a contributor: their own); the agent list is `GET /v1/skills/sources` |
| `POST /v1/skills/sources` | *contributor*. Create `{url, branch?, path?, exclude?, mode?, pinned_sha?, auth?, token?, poll_seconds?, enabled?, visibility?, members?, blocked?, default_runs?}`; `201 {source, status: "syncing"}` at once, the first sync running in the background (poll `GET /v1/skills/sources/{id}` until `last_checked_at` is set); the same create by the same person within two minutes answers `200 {source, existing: true}`. A contributor's source has `approval: "pending"`; a contributor's mode `pinned`, mode `auto` for an untrusted owner, or `limits` answer 403 `contributor_limit` |
| `GET`, `PATCH`, `DELETE /v1/skills/sources/{id}` | *contributor*. Read, change (only the fields sent; `token` replaces, `clear_token` removes; `visibility`, `members` and `blocked` are the source's sharing) or delete a source; a change of mode, branch, folder or pinned commit, and a delete, close the open review question. A PATCH writes only the fields it sends, and only to the source as it was read when the request was checked: one that meets a change made since answers 409 `conflict` (read the source again) |
| `POST /v1/skills/sources/{id}/sync` | *contributor*. Sync now |
| `GET /v1/skills/sources/{id}/versions?limit=` | *contributor*. The last versions (default 20, at most 100) |
| `POST /v1/skills/sources/{id}/versions/{vid}/activate` | make a version live (approve or roll back) |
| `POST /v1/skills/sources/{id}/versions/{vid}/reject` | `{note?}`: reject a staged version |
| `POST /v1/skills/sources/{id}/approval` | `{decision: "approve" or "reject", note?, sha?, version_id?}`: decide a contributor's source; closes its Inbox question. The approval is bound to the version you saw: `sha` and `version_id` of the waiting version, or of the live one when nothing waits (the dashboard always sends both). That version goes live, or stays live, with the approval; 409 `conflict` when another version would (a waiting or live version with another ID or commit, a live version of an automatic source built with older settings, or an automatic source that synced nothing yet), or when the source was decided meanwhile, and a source that still waits keeps its approval question. Without `version_id` (an API client), the approval is bound to the commit `sha`, and for an automatic or pinned source the version that goes live must have the source's current settings. A review-mode source's waiting version that is not the one named waits for review |
| `GET /v1/skills/sources/{id}/skills` | *contributor*. Skills with where they run, their own sharing (`sharing`, absent when they follow the source) and reads in 30 days |
| `PUT /v1/skills/sources/{id}/skills/runs` | *contributor*. `{path, runs}`: `anywhere`, `host`, or `""` to clear the override |
| `PATCH /v1/skills/sources/{id}` with `{hidden}` | admin: hide the source from every agent (`true`) or show it again |
| `PUT /v1/skills/sources/{id}/skills/sharing` | *contributor*. `{path, mode, members?, blocked?}`: one skill's sharing; `mode` `everyone`, `members`, `nobody`, or `""` (`inherit`) to follow the source; inherit with no `blocked` clears the setting |
| `POST /v1/skills/sources/{id}/webhook-secret` | *contributor*. A new webhook secret, once, with the payload URL |
| `GET /v1/skills/admin/people` | *contributor*. The active people a skill can be shared with or blocked from (emails for admins only) |
| `GET /v1/skills/admin/contributors` | the skill contributors `{user_id, added_by, added_at}` |
| `PUT`, `DELETE /v1/skills/admin/contributors/{user id}` | add an active person as a skill contributor, or remove one (their sources stay); operator tokens need the owner scope |
| `POST /v1/skills/sources/{id}/webhook` | GitHub's push webhook (public, signed) |
| `GET /v1/skills/admin/github-token` | `{set, rate: {limit, remaining, used, reset}, paused_until?}`: the default read token's state, never the token |
| `PUT /v1/skills/admin/github-token` | `{token}`: set the default read token (checked with GitHub first) or, with `""`, clear it; answers like the GET |

Every change is in the audit log (`skills.source_create`,
`skills.source_update`, `skills.source_delete`, `skills.source_sync`,
`skills.version_activate`, `skills.version_reject`,
`skills.runs_override`, `skills.webhook_secret_rotate`,
`skills.source_sharing`, `skills.skill_sharing` (with the setting before
and after), `skills.contributor_add`, `skills.contributor_remove`,
`skills.source_approve`, `skills.source_reject`,
`skills.source_propose`, `skills.source_proposal_reject`,
`skills.change_propose`, `skills.change_proposal_reject`,
`skills.change_pull_request`, `skills.change_proposal_fail`). No response and
no audit row carries a token; the webhook secret appears only in the rotate
response.

### How the Inbox carries these questions

Review questions, approvals of contributors' sources, source proposals
and change proposals are Inbox *system items*: questions that toolyard
itself raises (`internal/inbox/system.go`). A review question and a change
proposal come from the sender "toolyard" and go only to the person who
added the source (the owner, when that person is no longer an admin); an
approval question goes to the owner; a source proposal comes from the
proposing agent and goes to its owner.
In the Inbox they carry a **toolyard** badge, which only toolyard can set:
an agent that calls itself "toolyard" gets none. When the answer is
recorded, toolyard applies it at once and adds the outcome ("Version … is
live.", or why it could not) to the item's timeline.

The Inbox runs no handler for an item that expires or is withdrawn, and a
decision can land while no handler runs (a replica stopped in between). A
sweep settles both, every minute on one replica (the `skills-inbox-sweep`
leader loop): it applies a recorded decision that was not applied yet,
closes the proposals and reviews whose items expired or were withdrawn,
closes an open review whose version no longer waits, and resumes an
approved change proposal whose GitHub writes stopped or got no answer
(after three minutes, looking on GitHub for what it already wrote first).
An agent's next proposal and its `skills.proposal` poll settle its own
proposals too. A version that still waits after its question expired (7
days) gets a new question at the source's next check; while a question is
open, or its answer is being applied, the checks add nothing.

## For agents

### What you see

The catalog shows each caller only the skills its owner may see (an
agent sees what its owner sees; admins see every skill that is not set to
nobody; see [Sharing](#sharing)). A skill
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
`tools.toolyard.skills_<name>` (JavaScript), except `skills.bundle`: a
code-mode run and an async job store their results with secrets redacted,
so a bundle link would come back dead. Call `skills.bundle` directly, or
use `ty skills pull`.

| Tool | Arguments | Answer |
| --- | --- | --- |
| `skills.search` | `query`, `mode` (`keyword` default, `regex`), `source`, `limit` (10, at most 50), `include_host` | matching skills: name, description, uri, source, repo, commit, runs |
| `skills.get` | `skill` (URI, `source/path`, or a unique bare name), `include_files` (default true) | the SKILL.md body and frontmatter, `root`, and every file with its URI, `sha256:` digest and size |
| `skills.read` | `uri`, `offset`, `limit` (lines) | the file's content, at most 2,000 lines and 256 KiB per call; when `truncated`, call again with `offset: next_offset` |
| `skills.grep` | `pattern` (RE2), `source`, `path` (glob), `ignore_case`, `limit` (50, at most 200), `include_host` | matching lines with their file URI, skill and line number |
| `skills.sources` | none | the sources you see, with the commit each serves |
| `skills.list` | `cursor`, `limit` (100, at most 500), `source`, `include_host` | a page of skills as MCP `skills/list` entries, plus `next_cursor` |
| `skills.propose_source` | `url`, `branch`, `path`, `reason` | a pending proposal: an Inbox item for your owner; nothing is added before they approve (at most five waiting, ten an hour) |
| `skills.proposal` | `id` | one of your proposals: `pending`, `approved`, `done` (with `result`: the new source's id, or a change proposal's pull request URL), `rejected` or `failed` (with `error`); poll this instead of proposing again |
| `skills.propose_change` | `skill`, `files` (path → new content, `""` deletes), `title`, `summary` | a pending proposal against the version you see; after the source owner's approval toolyard opens a pull request |
| `skills.bundle` | `skills` (1 to 50; no two with the same name; at most 64 MiB of files) | a `.tar.gz` download URL: one use, 10 minutes, no token needed |

Before a multi-step task, call `skills.search` with a few words about the
task; when a skill matches, read it with `skills.get` and follow it.
`tools.search` also names up to three matching skills under `skills`.

To install skills in a local client such as Claude Code, run
`ty skills pull` (below), or download a bundle yourself:

```sh
# url from skills.bundle (or POST /v1/skills/bundle); it is the credential
curl -sf -o skills.tar.gz "$url" && mkdir -p ~/.claude/skills && tar -xzf skills.tar.gz -C ~/.claude/skills
```

### Change proposals (for agents)

To improve a skill, call `skills.propose_change` with the skill, the files
(path inside the skill folder → its new full content; `""` deletes a
file), a short `title` (the pull request title, at most 200 characters)
and a `summary` (what changes and why, at most 3,000 characters; the pull
request body, shown in full to the owner). The answer
is a pending proposal; the person who added the skill's source decides in
their Inbox, and after their approval toolyard opens a pull request on the
source repository. Poll the outcome with the proposal ID (`skills.proposal`):
`pending`, then `done` with the pull request URL in `result`, or
`rejected` or `failed` with the reason in `error`. `approved` means
toolyard is writing to GitHub, or is waiting to check what GitHub did
after a lost answer.

The change is against the version you see (its commit is the pull
request's base). toolyard refuses, before anyone is asked: paths outside
the skill folder (absolute, `..`, `.`, `.git`, backslashes, control
characters), more than 50 files, a file over the source's per-file limit
(1 MiB by default), more than 4 MiB of new content in all, text that is
not UTF-8 or holds a NUL byte, content that looks like a credential,
deleting `SKILL.md` or a file that does not exist, a `SKILL.md` that would
stop being a valid skill (its `name` must stay the folder's name), files
that would sit inside a file or over a folder, CI and build configuration
(see [Change proposals](#change-proposals)), a change too large to show
in full in the owner's Inbox item (20,000 characters with its diffs), and
a change that changes nothing. One agent may have at most five change
proposals waiting, and the source's owner must have a GitHub connection.

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
| `GET /v1/skills/proposal?id=` | `skills.proposal` |
| `POST /v1/skills/bundle` `{"skills": [...], "reason": "..."}` | `skills.bundle` |
| `GET /v1/skills/bundle/{ticket}` | the `.tar.gz` (no token: the ticket is the credential) |

Each GET takes `?reason=` for the audit record (20 to 2000 characters;
the default names the route). An unknown query parameter is refused.
Refusals are `{"error": "...", "code": "..."}`: `not_found` 404,
`conflict` 409, `invalid` 400, `limit` 422, `rate_limited` 429 (GitHub's
rate limit, with `retry_after` and `reset_at`), `unavailable` 503 (also
when the hub is off), and 403 when the gateway refused the call (an
explicit deny policy, the agent's own limits).

The `ty` command (`GET /v1/code/ty`) wraps these routes and keeps the
token out of argv and logs:

```sh
ty skills search deploy
ty skills get skill://team/ops/deploy/SKILL.md
ty skills read skill://team/ops/deploy/references/checklist.md --raw
ty skills grep 'rollback' --path '**/*.md'
ty skills sources
ty skills list --limit 50
ty skills pull team/ops/deploy pdf --dir ~/.claude/skills
```

`ty skills pull` asks for a bundle (`POST /v1/skills/bundle`), downloads
it once without sending the token, and unpacks each skill into its own
folder under `--dir` (default `~/.claude/skills`). It refuses the whole
bundle when a member is absolute, has a `..` segment, is a link or a
special file, or lies outside the skills the server named, and writes
nothing then. A skill folder that already exists is replaced only with
`--force` (the old folder is moved aside and removed once the new one is
in place).

## Built-in skills

toolyard ships a few skills of its own that teach any connected agent how
toolyard works and how to use it well. They live in the repository under
`internal/skillhub/builtin/skills/` and are compiled into the binary:

| Skill | What it teaches |
| --- | --- |
| `toolyard-basics` | what toolyard is, identity, `_reason`, finding tools, policies, connections, sessions, common errors (the starting point) |
| `toolyard-permissions-and-inbox` | `inbox.check` and `inbox.request`, the owner's decision, grants (`_grant`), `outcome_unknown`, `inbox.post`, `inbox.ask`, uploads, legacy approvals |
| `toolyard-code-mode` | `executeToolCode` in Starlark or JavaScript, stubs, limits, errors inside scripts, the REST fallback and `ty` |
| `toolyard-async-jobs` | `jobs.*`, MCP Tasks, job callbacks, retries after `failed`, `cancelled` or `outcome_unknown` |
| `toolyard-callbacks` | webhook receivers (`callbacks.*`), `callback_ref`, events, signatures, retries |
| `toolyard-skills` | this hub from the agent's side: search, read, bundles, proposals, the MCP Skills extension |
| `toolyard-github-writes` | GitHub comments and reviews that wait for the account owner's approval |
| `toolyard-connecting-clients` | helping a person connect Claude.ai, ChatGPT, Claude Code or another MCP client |

While the hub is on, toolyard serves them as the source **`toolyard`**
(kind `builtin`, version label `toolyard.builtin/skills@<digest>`), seen by
everyone and runnable anywhere by default. The server instructions a
caller gets at `initialize` (and `server/discover`) gain one line that
points it at `skills.get` with `toolyard/toolyard-basics`, while that
caller can see the guide: worked out per request, so a late install adds
the line and hiding the source removes it. Agents use them like any other
skills: `skills.search`, `skills.get`, `skills.read`, `skills.grep`,
`skills.list`, bundles, `ty skills pull` and the MCP Skills extension. The
source never calls GitHub. Its stored repository settings point at nothing
GitHub can serve (an owner name with a dot, which no GitHub account can
have; a pin to the all-zero commit; no token; a next check in year 9999),
so a toolyard build that does not know built-in sources, such as an older
one after a rollback, cannot fetch anything for it either; a current build
puts those settings back if such a build changed them.

**How they upgrade.** The version's commit is the SHA-256 of the embedded
files (every path and the SHA-256 of its content). At start each replica
compares the live version with its own files; when they differ it takes
the source's sync lock, looks again, and makes its files live: a version
it already has (a downgrade) or a new one built through the same checks
as a GitHub commit (text only, no secrets, the Agent Skills validation).
Two replicas that start together make one version; a restart with the
same files makes none. While replicas of two toolyard versions run side
by side, the one that started last decides, so a rolling upgrade or a
rollback never flips the skills back and forth. Every five minutes a
replica retries a start that failed and puts back a source that went
missing. Older versions follow the usual retention.

**What admins can change.** On the Skills page the source carries a
**Built in** badge. Admins choose who sees it (**Who sees it**, the same
sharing as other sources) and where its skills run (the source default and
the per-skill override). Nothing else applies: its branch, folder, excludes,
limits, update mode, pinned commit, token, check interval, paused state and
webhook cannot change, its versions cannot be activated or rejected by
hand, and it cannot be deleted. The admin routes answer `409 conflict`
with the reason; "Sync now" only installs the running binary's skills if
they are not live. Agents cannot propose changes to it
(`skills.propose_change` answers `invalid`): change the skills in the
toolyard repository instead.

**The slug is reserved.** No source can be created with the slug
`toolyard`, and a repository named `toolyard` gets `toolyard-2` by
default. A source that already held the slug before an upgrade keeps it;
the built-in source is then not installed, and the log says why:
delete that source and add its repository again under
another slug.

## How sync works

1. **Check.** The cluster leader looks every 15 seconds for sources whose
   check is due, and checks up to three at once: each enabled source is
   checked every `poll_seconds` (default 300, at least 60). The check asks
   GitHub for the branch's ref (`/git/ref/heads/<branch>`, so a tag or a
   commit SHA never matches a branch name) with the ETag of the last
   answer; an unchanged branch answers `304 Not Modified`, which costs no
   rate limit when the source reads with a token (its own, or the default
   one in GitHub access; anonymous `304`s still count). While GitHub's
   rate limit holds back the sources without their own token, the leader
   skips them until it resets. *Sync now* on the dashboard checks at once on the replica
   that got it, without the ETag and with `Cache-Control: no-cache`. A
   signed GitHub push webhook names the pushed commit, and the sync
   serves that commit without asking for the branch head when it is the
   head last seen or descends from the active commit (one comparison). An
   older commit (a redelivered or out-of-order push) makes the sync read
   the branch head instead, so a redelivery never rolls agents back. A
   pinned source never asks for the branch head.

   Without a token, GitHub may answer a public repository's branch head
   from a cache up to 60 seconds old, so a poll can see a push up to a
   minute late. Add the push webhook (or a token) for updates within
   seconds. A poll never moves a source back to an older commit on such a
   stale answer: it waits for a fresh one.
2. **Lock.** A sync of a source runs under a cluster lock named after the
   source, so two syncs of one source never overlap, whichever replicas
   start them.
3. **Known commit?** When the commit already has a version built with the
   source's current settings, nothing is downloaded: an active version
   stays; a staged one keeps waiting for review (an `auto` source makes it
   live; a `review` source asks the review hook again, so an Inbox item
   that could not be created is retried); a retired one stays retired
   while the branch head stays where it is (an admin's rollback holds), and
   goes live again in an `auto` source when the branch moves back to it (a
   force-push or a revert to that commit); a rejected one stays rejected;
   a failed one stays failed until *Sync now* tries it again. A pinned
   source makes its commit's version live whatever its state.
4. **Fetch.** toolyard builds the new version from the active one when it
   can: it asks GitHub which files changed between the two commits and
   reads only those (the contents API), when the active version was built
   with the same folder, excludes and limits, the new commit descends from
   it, at most 100 files changed under the source folder, no skill
   appears or goes away (no `SKILL.md` added, removed or renamed), no
   `.gitattributes` changed, and every changed file is a plain file at the
   path asked for (the contents API answers for a symbolic link with its
   target's content, which toolyard never follows). Anything else
   downloads the commit's archive (`/tarball/<sha>`, a gzip'd tar from
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
   - storable paths: a path that is not valid UTF-8, or holds a backslash
     or a control character, is left out (`invalid_path`);
   - no secrets: a file that holds a private key block or a well-known
     token shape (GitHub `ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`/`github_pat_`,
     AWS `AKIA…`/`ASIA…`, Slack `xox…`, Anthropic `sk-ant-…`, OpenAI
     `sk-proj-…` and long `sk-…` keys) is left out with a `secret`
     warning, which never repeats the match.
   A file left out inside a skill folder is listed in the skill's
   `omitted` list with the reason (the first 10,000 such files of a
   commit; a warning counts the rest).
6. **Find the skills.** Every folder with a `SKILL.md` is a skill. Its YAML
   frontmatter is stored verbatim as JSON (SEP-2640). A skill is skipped,
   with a warning, when its frontmatter is missing or not valid YAML, it
   has no `name` or no `description`, its `name` or `description` holds a
   NUL byte or invalid UTF-8 (a YAML escape can make one), or its `name`
   differs from its folder's name. Other departures from the specification are warnings
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
   once, unless an admin changed what the source fetches (or made another
   version live) while the sync ran: then the version is dropped and the
   next check builds with the new settings; `review` sources keep it staged and send the source's creator an
   Inbox item. Activation is one transaction (the old version becomes
   retired) and is announced on the cluster: every replica reloads that
   source into its in-memory catalog within moments. Replicas also compare
   their catalog with the database every 30 seconds (Postgres) and every 5
   minutes, so a missed message heals by itself.
9. **Keep.** Each source keeps its active version, the version waiting for
   review, and the 10 newest others; older versions, and file contents no
   version uses any more, are deleted.

A failed sync (GitHub down, a token revoked, a limit) records the error on
the source and leaves the active version as it was. A commit whose content
the database refuses is recorded as a failed version, like a limit, so it
is not downloaded again and again. A sync that stops because its replica
lost the leader lease, or is shutting down, is not a failure: another
replica checks the source. The next check waits
twice as long after each failure in a row, at most an hour; a GitHub rate
limit answer (`Retry-After`, or no calls left until `X-RateLimit-Reset`)
pauses calls with that credential until the time GitHub gives.

An admin can make the staged version that waits for review active (an
approval) or any retired version (a rollback), or reject a staged one. A
staged version that a newer commit replaced cannot be approved. A rollback on an `auto`
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
version for that commit, and leave the active version as it was. The file
limit counts every file under the source folder that is not excluded
(kept, binary, linked or too large), inside a skill folder or not; the
byte limit counts every text file toolyard reads and checks, files left
out by the secret check included. Point the source at the folder that
holds the skills, or add exclude globs, to stay well under them. A
repository archive larger than 4 GiB uncompressed is refused.

Reads have their own caps: search returns at most 50 results (10 by
default); a listing page at most 500 skills (100 by default); grep stops
at 200 matching lines, 5,000 files, 20 MB scanned or 2 seconds, and
returns what it found by then; a bundle (`skills.bundle`) holds at most
50 skills and 64 MiB of files, one folder per skill name; one
`skills.read` call returns at most 2,000 lines and 256 KiB. A skill over
the MCP Skills extension's per-skill limits (512 files or 16 MiB) is
still served, with a warning on its version: some MCP hosts may refuse
it.

Each source records reads per skill per day (a `get`, a file `read`, or a
bundle); the dashboard shows the last 30 days, and counts older than 90
days are deleted.

## MCP Skills extension

While the hub is on, toolyard declares
`capabilities.extensions["io.modelcontextprotocol/skills"] = {"directoryRead": true}`
(with the `resources` capability) on both protocol versions of its HTTP
endpoint (`/mcp`): the 2025-11-25 `initialize` result and the 2026-07-28
`server/discover` result. `-stdio` does not declare it (it serves the
tools and `resources/read`, not the extension's methods). It serves:

- `skills/list` (`cursor`): your skills, each entry
  `{uri, frontmatter, resources: [{uri, digest, size}]}` with every file of
  the skill, `SKILL.md` included. 2026-07-28 results carry `ttlMs` and
  `cacheScope: "private"`.
- `skills/get` (`uri` of a `SKILL.md`): one entry, listed or not.
- `resources/read` of any `skill://` file URI (text, with its `mimeType`),
  and `resources/templates/list` with the template `skill://{+path}`.
- `resources/directory/read` (`uri` of a skill folder or a folder in it):
  its direct children; folders have `mimeType: "inode/directory"`.

URIs percent-encode each path segment (a space is `%20`, `#` is `%23`,
`%` is `%25`, non-ASCII text is its UTF-8 bytes); folder URIs have no
trailing slash. A skill at a source's folder itself is served under its
name, `skill://<source>/<name>/SKILL.md`, so the last segment is always
the skill's name; `skill://<source>/SKILL.md` still resolves. Listing a
source's folder, or a folder above skills, leaves out the folders of
skills that need the T3 host unless you run on it; a skill's own folder
lists everything.

These methods pass the same checks as the matching tool (`skills.list`,
`skills.get`, `skills.read` for `resources/read` and
`resources/directory/read`): your access, your agent's deny list and an
explicit deny policy. A refused call answers like an unknown URI. An
unknown URI, or one you cannot see, is JSON-RPC error `-32602` (invalid
params), except `resources/read` on 2025-11-25, which answers `-32002`
(resource not found) as for any other resource. When a source's active
version changes, toolyard sends
`notifications/resources/list_changed`, best effort: a stateless MCP
connection never hears it, so re-list when you need fresh digests.

Reads of skill content outside a tool call (`skills/get`,
`resources/read`, `resources/directory/read`, a bundle download) write a
`skill.read` audit row with the URI and `owner/repo@commit`, as tool calls
do, also when they are refused; a bundle download lists every skill and
its version in the row's arguments. A bundle link is a credential: the
audit log, stored job results and code-run records keep it redacted.

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
  explicit deny policy); the same checks guard the MCP Skills extension
  methods, bundle downloads (checked again when the link is used) and the
  skills that `tools.search` adds.
- **Proposals.** Only an agent whose owner is an admin may propose a
  source, a proposal never carries a token and keeps only the URL, branch
  and folder of the agent's input, one agent may have at most five waiting
  and ten an hour, and nothing is added before the owner approves. Agents
  cannot raise toolyard's own Inbox questions (system items), and only
  those carry the Inbox's toolyard badge. Text from a repository or an
  agent cannot break out of its line in an item toolyard sends.
- **Change proposals.** toolyard, not the agent, writes to GitHub, and
  only after the person who added the source approves that exact change
  in their Inbox, in person (dashboard, own notification or passkey),
  with their own GitHub connection. The Inbox enforces this for the item
  kind (`HumanOnly`): no operator token, batch, grant, policy, rule or
  other admin approves it. The item shows every path and the complete
  diff, or the proposal is refused. Proposed files are checked like
  synced ones (text only, no credentials, paths inside the skill folder),
  CI and build configuration is refused, the branch name has a part the
  agent never sees, nothing found on GitHub is adopted unless it is
  exactly the approved tree (and, for a pull request, opened by the
  creator), retries stop after ten attempts or a day, and nothing is ever
  merged by toolyard. The pull request names the proposing agent and the
  proposal, never a person, and its text is inert (no mentions, closing
  keywords, HTML or images).
- **Bundle downloads.** The ticket in the URL is 256 random bits, works
  once, for 10 minutes, and is stored only as its SHA-256; a caller holds
  at most 20 live tickets, visibility is checked again at download, and
  downloads have a per-IP limit (600 an hour).
- **Audit.** Every admin change is audited (`skills.*`), and every read of
  skill content outside a tool call writes a `skill.read` row with the URI
  and `owner/repo@commit`.
