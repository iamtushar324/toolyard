# skills.* tools: arguments, answers, REST and ty

Every call also takes `_reason` (20 to 2000 characters). In code mode the
tools are `toolyard.skills_<name>` (Starlark) and
`tools.toolyard.skills_<name>` (JavaScript); `tools.search` lists them
under `skills`. `skills.bundle` is refused in code mode and in async jobs.

## Read tools

### skills.search

| Argument | Type | Notes |
|---|---|---|
| `query` | string, required | words about the task (`keyword`), or an RE2 pattern (`regex`) |
| `mode` | `keyword` (default) or `regex` | keyword ranks names, descriptions and paths; regex matches against them |
| `source` | string | only this source, by its slug |
| `limit` | integer 1 to 50 | default 10 |
| `include_host` | boolean | also return skills marked `runs: host` |

Answer: `query`, `mode`, `total`, `skills`: each `uri`, `name`,
`description`, `source`, `path`, `repo`, `commit`, `runs`, and `score`.

### skills.get

| Argument | Type | Notes |
|---|---|---|
| `skill` | string, required | URI, `<source>/<path>`, or a bare name that only one skill you can see has |
| `include_files` | boolean | include the file list (default true) |

Answer: the summary fields above, `frontmatter` (as written), `body` (the
`SKILL.md` text after the frontmatter), `root` (the folder URI that
relative links resolve against), `files` (each `uri`, `path`, `digest`
`sha256:<hex>`, `size`, `mime_type`), and `omitted` (files not served, with
the reason).

### skills.read

| Argument | Type | Notes |
|---|---|---|
| `uri` | string, required | a file URI: a `files[].uri`, or a link resolved against `root` |
| `offset` | integer >= 0 | first line to return, 0-based (default 0) |
| `limit` | integer 0 to 2000 | lines to return; 0 or absent means 2000 |

Answer: `uri`, `path`, `digest`, `size`, `mime_type`, `skill`, `source`,
`repo`, `commit`, `content`, `offset`, `total_lines`, and when the answer
was cut (at 2,000 lines or 256 KiB, always on a line boundary) `truncated:
true` and `next_offset`. Call again with `offset: next_offset` until
`truncated` is absent.

### skills.grep

| Argument | Type | Notes |
|---|---|---|
| `pattern` | string, required | RE2, matched against each line |
| `source` | string | only this source |
| `path` | string | a glob: without `/` it matches the file name (`*.md`); with `/` it matches the path from the source's folder (`**/scripts/*`) |
| `ignore_case` | boolean | case-insensitive match |
| `limit` | integer 1 to 200 | default 50 |
| `include_host` | boolean | also search skills marked `runs: host` |

Answer: `pattern`, `total`, `hits`: each `uri`, `skill`, `line` (1-based),
`text`, `repo`, `commit`, `source`. Grep stops at `limit` hits, 5,000
files, 20 MiB scanned or 2 seconds, and returns what it found by then.

### skills.sources

No arguments. Answer: `total`, `sources`: each `slug` (also as `source`),
`uri` (`skill://<slug>`), `repo`, `branch`, `path`, `commit`, `synced_at`,
`skill_count`, `html_url`.

### skills.list

| Argument | Type | Notes |
|---|---|---|
| `cursor` | string | the `next_cursor` of the page before |
| `limit` | integer 1 to 500 | default 100 |
| `source` | string | only this source |
| `include_host` | boolean | also list skills marked `runs: host` |

Answer: `total`, `skills`, and `next_cursor` while more pages exist. Each
entry is the MCP `skills/list` entry (`uri`, `frontmatter`, `resources`:
every file with `uri`, `digest`, `size`) plus `name`, `description`,
`source`, `path`, `repo`, `commit`, `runs`. To find a skill for a task,
`skills.search` is quicker.

## skills.bundle

| Argument | Type | Notes |
|---|---|---|
| `skills` | array of 1 to 50 strings, required | URIs, `<source>/<path>` or unique bare names |

Answer: `url` (one use, 10 minutes, no token needed), `expires_at`,
`skills` (each `uri`, `name`, `source`, `repo`, `commit`), `next` (a ready
`curl … && tar -xzf …` line), and `note` when the server has no public URL
configured: then `url` is a path, so prefix it with the toolyard address
you connect to. Limits: at most 64 MiB of files, no two skills with the
same name, at most 20 unused links per caller. The download checks again
that you may still see every skill.

## Proposal tools

`skills.propose_source`, `skills.propose_change` and `skills.proposal`:
read `skill://toolyard/toolyard-skills/references/proposals.md` with
`skills.read`.

## Answers and errors

Every answer is JSON, as text and as structured content. A refusal has
structured content `{"error": "<code>", "message": "..."}`; the codes are
`not_found`, `conflict`, `invalid`, `limit`, `rate_limited` (with
`retry_after` in seconds and `reset_at`), `unavailable` and `internal`.

## REST

The same tools over HTTPS, with an agent token or a session token as the
bearer (operator tokens are refused). Each route runs the tool through the
normal call pipeline (access, policy, audit).

| Route | Tool |
|---|---|
| `GET /v1/skills/search?query=&mode=&source=&limit=&include_host=` | `skills.search` |
| `GET /v1/skills/get?skill=&include_files=` | `skills.get` |
| `GET /v1/skills/read?uri=&offset=&limit=` | `skills.read` |
| `GET /v1/skills/grep?pattern=&source=&path=&ignore_case=&limit=&include_host=` | `skills.grep` |
| `GET /v1/skills/sources` | `skills.sources` |
| `GET /v1/skills/list?cursor=&limit=&source=&include_host=` | `skills.list` |
| `GET /v1/skills/proposal?id=` | `skills.proposal` |
| `POST /v1/skills/bundle` `{"skills": [...], "reason": "..."}` | `skills.bundle` |
| `GET /v1/skills/bundle/{ticket}` | the `.tar.gz` (no token: the ticket is the credential) |

Each GET takes `?reason=` for the audit record (20 to 2000 characters).
An unknown query parameter is refused. Errors are
`{"error": "...", "code": "..."}`: `not_found` 404, `conflict` 409,
`invalid` 400, `limit` 422, `rate_limited` 429, `unavailable` 503 (also
when the hub is off), and 403 when the gateway refused the call (a deny
policy, the agent's own limits). There is no REST route for the two
propose tools: call them as tools.

## ty

The `ty` command (download it from `GET /v1/code/ty`) wraps these routes
and keeps the token out of argv and logs:

```sh
ty skills search QUERY [--mode keyword|regex] [--source SLUG] [--limit N] [--include-host]
ty skills get SKILL [--no-files]
ty skills read URI [--offset N] [--limit N] [--raw]
ty skills grep PATTERN [--source SLUG] [--path GLOB] [--ignore-case] [--limit N] [--include-host]
ty skills sources
ty skills list [--cursor CURSOR] [--limit N] [--source SLUG] [--include-host]
ty skills pull SKILL... [--dir ~/.claude/skills] [--force] [--reason TEXT]
```

`ty skills pull` asks for a bundle, downloads it once without sending the
token, and unpacks each skill into its own folder under `--dir`. It refuses
the whole bundle, and writes nothing, when a member is absolute, has a
`..` segment, is a link or a special file, or lies outside the skills the
server named. An existing skill folder is replaced only with `--force`.
