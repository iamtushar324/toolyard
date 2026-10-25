# Skills hub

toolyard copies the [Agent Skills](https://agentskills.io/specification) in
GitHub repositories ("skill sources") into its database at an exact commit
and serves that copy to every client. The design is in
[design/skills-hub.md](design/skills-hub.md).

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

### MCP Skills extension

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
