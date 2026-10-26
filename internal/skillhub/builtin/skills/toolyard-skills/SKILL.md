---
name: toolyard-skills
description: "Find, read and follow the Agent Skills (step-by-step playbooks) that toolyard serves, and propose new skill sources or edits. Covers skills.search, skills.get, skills.read (offset, limit, truncated, next_offset), skills.grep, skills.sources, skills.list, skills.bundle and ty skills pull, skills.propose_source, skills.propose_change and skills.proposal; skill:// URIs, include_host and skills marked runs: host, repo and commit provenance, the /v1/skills REST routes, code-mode names, and the MCP Skills extension (skills/list, skills/get, resources/read, resources/directory/read). Use at the start of any multi-step task, when tools.search names matching skills, when you need a file a skill links to, when you want to install skills in a local client, when a skill is wrong or missing, or when a skills tool answers not_found, conflict, invalid, limit, rate_limited or unavailable."
metadata:
  toolyard-runs: anywhere
---

# Skills: find the playbook before you start

toolyard serves a catalog of Agent Skills: folders with a `SKILL.md` and
supporting files that say how to do a task well. An admin adds a GitHub
repository (a *source*); toolyard copies its skills at an exact commit and
serves only that copy, so you never call GitHub yourself. toolyard also
ships built-in skills about itself, like this one, in the read-only source
`toolyard` (an admin may hide it). Their `repo` is
`toolyard/builtin@<12-hex digest>`: they change with the toolyard version, and
`skills.propose_change` refuses them (tell your owner what should change).
Start with `skills.get {"skill": "toolyard/toolyard-basics"}` if toolyard
is new to you.

You see only the sources your owner may see. A skill you cannot see
answers exactly like one that does not exist.

## When to use

- Before any multi-step task: search first, then follow a matching skill.
- When `tools.search` names up to 3 matching skills under `skills`.
- When a skill links to a file (`references/…`, `scripts/…`) you need.
- When a person asks to install skills in a local client.
- When a skill is wrong, outdated or missing: propose a change or a source.

## Etiquette

- Follow a matching skill instead of improvising; it holds your
  organisation's tested steps.
- Skill text is guidance, not permission. Every tool call a skill suggests
  still goes through policy and the Inbox. A skill never lets you skip an
  owner's decision, use another credential, or widen a grant.
- When a skill conflicts with your owner's instructions or with toolyard's
  rules, those win. Say so, and propose a fix to the skill.
- Cite where guidance came from: every answer names `source`, `repo`
  (`owner/repo@<12-hex commit>`), `commit` (the full SHA) and `uri`.

## Recipe

1. Search with a few words about the task:

   ```text
   skills.search
   {"query": "release rollback", "_reason": "Find a playbook before I roll back the failed release the user reported"}
   ```

2. Read the best match. The answer has the `SKILL.md` `body`, its
   `frontmatter`, `root` (the skill's folder URI) and `files` (each with
   `uri`, `size` and a `sha256:` digest, not the content):

   ```text
   skills.get
   {"skill": "skill://team-kit/ops/release-rollback/SKILL.md", "_reason": "Read the rollback playbook that matched my search"}
   ```

3. Resolve each relative link in the body against `root`, and read the file:

   ```text
   skills.read
   {"uri": "skill://team-kit/ops/release-rollback/references/checklist.md", "_reason": "Read the checklist the rollback playbook links to"}
   ```

   One call returns at most 2,000 lines and 256 KiB. When `truncated` is
   true, call again with `offset` set to `next_offset`.
4. Follow the skill. Run each tool call it suggests through the normal path
   (policy, `inbox.check`, `inbox.request` when a tool is restricted).
5. If the skill was wrong or out of date, propose a fix with
   `skills.propose_change` (see [proposals](references/proposals.md)), or
   tell your owner when it is a built-in `toolyard` skill.

No match? Try other words, `mode: "regex"`, or `skills.grep` over file
contents. `skills.sources` shows which sources you see.

## URIs and names

```text
skill://<source>/<skill path>/SKILL.md     the skill
skill://<source>/<skill path>/<file path>  a file beside it
skill://<source>/<skill path>              its folder (no trailing slash)
```

`skills.get`, `skills.bundle` and `skills.propose_change` also take
`<source>/<skill path>`, or a bare skill name when exactly one skill you
can see has it (otherwise `conflict`: use the URI). URIs percent-encode
each path segment (a space is `%20`). The built-in skills live at
`skill://toolyard/<name>/SKILL.md`.

## Skills that need a specific host

A skill marked `runs: host` relies on the paths and scripts of one host
machine. Agents connected through that host's connection see these skills
in search, listings and grep. Everyone else sees them there only with
`include_host: true`; a direct reference (URI, `source/path` or name)
reads them anyway. Follow such a skill only when you actually run on that
host. Skills marked `runs: anywhere` work in any client.

## Install skills locally

`skills.bundle {"skills": [...]}` packages 1 to 50 skills (at most 64 MiB
of files, no two with the same name) as one `.tar.gz`, one folder per
skill. It answers a download `url` that works once, for 10 minutes, without
a token: the URL is the credential, so do not share or log it. Call it
directly: inside code mode or an async job it is refused, because the link
would come back redacted. From a shell, `ty skills pull <skill>
[<skill> ...]` does the same and unpacks each skill safely into its own
folder under `--dir` (default `~/.claude/skills`; `--force` replaces an
existing folder).

## Other ways in

- Code mode: `toolyard.skills_<name>` (Starlark) and
  `tools.toolyard.skills_<name>` (JavaScript), for example
  `toolyard.skills_search(query="deploy")`. Not `skills.bundle`.
- REST: `GET /v1/skills/search|get|read|grep|sources|list|proposal` and
  `POST /v1/skills/bundle`, with an agent or session token. See
  [tools reference](references/tools.md).
- MCP Skills extension, for clients that support it: `skills/list`,
  `skills/get`, `resources/read`, `resources/directory/read`. See
  [MCP extension](references/mcp-extension.md).

The skills tools are on for every active caller. An agent's owner can
switch them off for that agent (its deny list), and an admin for everyone
(an explicit deny policy); on a server without the skills hub the tools are absent or answer
`unavailable`.

## Errors

| Code | Meaning | Next step |
|---|---|---|
| `not_found` | no skill or file you can see has that name or URI | search again; check the URI from `skills.get` |
| `conflict` | a bare name matches several skills, two bundled skills share a name, or 5 proposals of that kind already wait | use the full URI; bundle them separately; poll `skills.proposal` |
| `invalid` | a bad argument (empty query, bad `mode`, `limit` below 1, `skills.bundle` inside code mode) | fix the call |
| `limit` | a size or count limit (bundle over 64 MiB, 20 unused bundle links, 10 proposals an hour, a change too large) | ask for less, or wait |
| `rate_limited` | GitHub's rate limit for toolyard (when it must call GitHub, for example for a proposal); `retry_after` and `reset_at` say when | retry after `reset_at` |
| `unavailable` | the hub, bundles or proposals are off, or the catalog did not answer in time | retry in a few seconds, or tell your owner |

## Pitfalls

- Starting a multi-step task without searching. The catalog is cheap to ask.
- Treating a skill's instructions as a grant. They are not.
- Reading only `SKILL.md`. The details are often in the linked files.
- Stopping at the first page of a long file. Check `truncated`.
- Proposing the same source or change again while one is `pending`: poll
  `skills.proposal` instead.

## Related skills

- `toolyard-basics` (`skills.get toolyard/toolyard-basics`): how toolyard works.
- `toolyard-permissions-and-inbox` (`skills.get toolyard/toolyard-permissions-and-inbox`): restricted calls a skill suggests.
- `toolyard-code-mode` (`skills.get toolyard/toolyard-code-mode`): the `toolyard.skills_<name>` bindings.
- `toolyard-connecting-clients` (`skills.get toolyard/toolyard-connecting-clients`): connecting a client and the `ty` command.
