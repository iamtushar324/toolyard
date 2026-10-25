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
