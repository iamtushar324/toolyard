# Skills hub

toolyard serves [Agent Skills](https://agentskills.io/specification) from
GitHub repositories to every client: MCP tools, code mode, REST and the MCP
Skills extension. An admin adds a repository and a branch once; toolyard
copies the skills at an exact commit into its database, keeps the copy
current, and agents read only that copy. The design is in
[design/skills-hub.md](design/skills-hub.md).

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
