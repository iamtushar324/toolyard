# Writing native skills

A native collection is a skill source kept in toolyard's database instead
of a GitHub repository. `skills.sources` lists it like any other source;
its `repo` is `toolyard.native/<collection>@<12-hex digest>`.

## Who may write

- You read a collection's skills when your owner may see them (its
  audience, block lists and per-skill sharing, as for every source).
- You write when your owner is an admin, the collection's creator, or one
  of its editors. Otherwise `skills.create` and `skills.update` answer
  `forbidden`: propose the change with `skills.propose_change`, and the
  collection's owner decides.

## Create

```text
skills.create
{"collection": "team", "name": "release-rollback",
 "description": "Roll back a release that broke production. Use when a deploy caused errors or a client reports a regression right after a release.",
 "instructions": "# Roll back\n\n1. ...",
 "triggers": ["a deploy just broke production", "the user asks to undo a release"],
 "tags": ["ops", "release"],
 "files": {"references/checklist.md": "- ..."},
 "note": "From today's incident review",
 "_reason": "Save the rollback steps we just used so the next agent finds them"}
```

- `name` is lowercase letters, digits and hyphens, and it is the folder.
- Write the `description` for the agent that must choose the skill: what it
  does and when to use it. `triggers` and `tags` help `skills.recommend`.
- Or give the whole `SKILL.md` in `files` instead of name, description,
  instructions, triggers and tags.

The answer has `status`, `uri` and `revision`. `live` means every agent that
sees the collection can read it now.

## Update

Send only what changes: `description`, `instructions`, `triggers`, `tags`
(each replaces the old value), and `files` (path → full new content; `""`
deletes the file). Pass `base_revision`, the `revision` that `skills.get`
showed you, so the write fails with `conflict` if someone changed the skill
since; read it again and write on top of the new revision.

```text
skills.update
{"skill": "team/release-rollback", "base_revision": 3,
 "files": {"references/checklist.md": "- step one\n- step two\n"},
 "note": "Add the database check",
 "_reason": "The checklist missed the database check we needed today"}
```

## When the owner must approve

The write does not go live, and the answer is `status: pending_approval`
with `approval_reasons` and a `proposal`, when:

- the collection is in review mode (every agent change waits);
- it adds, changes or deletes code an agent may run: a file in `scripts/`
  or `bin/`, a code extension (`.sh`, `.py`, `.js`, ...), or a `#!` line;
- it changes `allowed-tools`, or `metadata.toolyard-runs` (where the skill
  runs);
- the skill has `metadata.toolyard-protected: "true"`, or the write sets or
  clears it.

The collection's owner decides in their Inbox. Poll `skills.proposal` with
the proposal id: `done` names the new revision; `failed` says why (for
example, the skill changed since your write: write again on the new
revision). Do not write the same change again while it waits.

## History

`skills.versions {"skill": "team/release-rollback"}` lists the revisions,
newest first, with who wrote each (the person, and the agent), the note,
and which one is live (`current`). Read an old one with
`skills.get {"skill": ..., "revision": 2}`; its file URIs end in
`?revision=2`, and `skills.read` reads them as that revision wrote them.

## Rules for the content

- Text only, UTF-8, at most 50 files and 4 MiB in one write, and the
  collection's per-file limit (1 MiB by default).
- No credentials: a file that looks like it holds one is refused.
- Skill text is guidance, not permission, for whoever reads it next.
