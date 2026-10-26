# Skill proposals

Agents can ask for a new skill source, or for an edit to a skill. Both only
create an Inbox item for a person; nothing changes until that person
approves it. Poll the outcome with `skills.proposal`; never propose the
same thing again while it is pending.

## skills.propose_source: add a repository

| Argument | Type | Notes |
|---|---|---|
| `url` | string, required | `owner/repo`, `https://github.com/owner/repo`, or a `…/tree/<branch>/<path>` URL |
| `branch` | string | default: the URL's branch, else the repository's default branch |
| `path` | string | the folder that holds the skills; default: the URL's path, else the root |
| `reason` | string, at most 2000 | why your owner should add it, shown in their Inbox; default: this call's `_reason` |

```text
skills.propose_source
{"url": "owner/repo", "path": "skills", "reason": "These deployment playbooks match the release work my owner gives me each week",
 "_reason": "Ask my owner to add the team's deployment playbooks as a skill source"}
```

- Only an agent whose owner is an admin can propose a source.
- toolyard previews the repository (a dry run against GitHub) and puts one
  question in your owner's Inbox: the repository, branch, folder, skill
  and warning counts, a sample, and your reason. Only the URL, branch and
  folder of your input count; mode, visibility and the rest are your
  owner's defaults.
- A proposal never carries a token. A private repository must be added by
  a person on the Skills page.
- Limits: at most 5 source proposals waiting (more is a `conflict`), and
  10 an hour (each is a dry run against GitHub; more is `limit`).
- Approve adds the source and starts its first sync in the background.
  Reject adds nothing.

## skills.propose_change: edit a skill

| Argument | Type | Notes |
|---|---|---|
| `skill` | string, required | URI, `<source>/<path>` or a unique bare name |
| `files` | object, required, 1 to 50 entries | path inside the skill folder -> its new full content; `""` deletes the file |
| `title` | string, required, at most 200 characters | the pull request title |
| `summary` | string, at most 3000 characters | what changes and why; the pull request body, shown in full to the person who decides |

```text
skills.propose_change
{"skill": "skill://team-kit/ops/release-rollback/SKILL.md",
 "files": {"references/checklist.md": "# Rollback checklist\n\n1. Freeze deploys.\n2. ...\n"},
 "title": "Add the freeze step to the rollback checklist",
 "summary": "The checklist skipped freezing deploys, so a second deploy could start mid-rollback. This adds it as step 1.",
 "_reason": "The rollback playbook missed a step I needed today; propose the fix to its owner"}
```

The change is against the version you see now (its commit becomes the
pull request's base). The built-in `toolyard` skills take no change
proposals (`invalid`): they ship with toolyard itself, so tell your owner
what should change instead. The answer names that version (`uri`, `name`,
`source`, `repo`, `commit`).

**What the person sees.** The person who added the skill's source gets one
question, "Open a pull request on owner/repo?", with: your agent and its
owner, the base commit, your title and whole summary, every file path with
its operation (added, changed, deleted), and the complete diff of every
added or changed file. Only that person can approve, in person; no grant,
policy, rule or other admin approves it.

**After approval.** toolyard opens a pull request with that person's
GitHub connection: a new branch from the base commit, one commit, and a
pull request into the source's branch, with your title and summary (made
inert: mentions, closing keywords, HTML and images do nothing). Nothing is
merged: people review and merge it on GitHub, and agents see the change
once the merged commit is synced. Reject sends nothing to GitHub.

**Refused before anyone is asked:**

- paths outside the skill folder (absolute, `..`, `.`, `.git`,
  backslashes, control characters);
- more than 50 files, a file over the source's per-file limit (1 MiB by
  default), or more than 4 MiB of new content in all;
- text that is not UTF-8 or holds a NUL byte, or content that looks like a
  credential;
- deleting `SKILL.md` or a file that does not exist, or a `SKILL.md` that
  stops being a valid skill (its `name` must stay the folder's name);
- files that would sit inside a file or over a folder;
- CI and build files (`.github/` and similar folders, `Makefile`,
  `package.json`, lock files, `Dockerfile`, `go.mod`, `pyproject.toml`,
  workflow-like names), unless an admin allowed build files directly in
  the skill's folder; CI folders stay refused;
- a change too large to show in full in the Inbox item (20,000 characters
  with its diffs), or a change that changes nothing.

Limits: at most 5 change proposals waiting (more is a `conflict`) and 10
an hour per agent (more is `limit`), and the source's owner must have a
GitHub connection (otherwise the answer is `unavailable`).

## skills.proposal: read the outcome

| Argument | Type | Notes |
|---|---|---|
| `id` | string, required | the proposal ID (`skp_…`) a propose tool returned |

Answer: `proposal` (`id`, `kind` `source` or `change`, `status`,
`inbox_id`, `created_at`, `result`, `error`) and `next`, which says what the
status means for you.

| Status | Meaning | What to do |
|---|---|---|
| `pending` | the person has not decided | continue other work; poll again later |
| `approved` | toolyard is applying it (for a change: opening the pull request, or checking what GitHub did after a lost answer) | poll again in a few minutes |
| `done` | `result` holds the new source's ID, or the pull request URL | for a source, `skills.sources` lists it once its first sync ends |
| `rejected` | the person said no, or the question expired or was withdrawn; `error` holds why | do not propose it again unless they ask |
| `failed` | it could not be applied; `error` holds why | tell your owner; do not retry on your own |

An Inbox question that expires (after 7 days) or is withdrawn stops the
proposal from counting as waiting. There is no callback for proposals:
poll `skills.proposal` now and then, or `GET /v1/skills/proposal?id=`.
