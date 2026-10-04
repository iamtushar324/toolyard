# Toolyard stage delivery

Stage URL: https://toolyard.stage.dev.beknown.live

Release: `stage-9eac6b2`
Source commit: `9eac6b24d745894efdf1bd15a2cc1256adc2e5f8`
Source branch: `t3code/jakarta`, published to origin.

The stage uses fresh SQLite data and two synthetic connectors. It runs on this server
under its own Linux account and systemd services. The existing Toolyard process was
not restarted. The stage shares only the existing Clerk identity provider configuration.
Application data, application sessions, signing keys, connector state, and notification
subscriptions are separate.

Delivered:

- Inbox, Connections, Activity navigation; people, policies, and settings remain secondary.
- Compact dark theme, quiet controls, disclosure panels, mobile navigation, stage version label.
- Version 2 questions: free text, single choice, multiple choices, stable option IDs, exact text,
  explicit submission, optional audio, custom text without a selection, and exclusive options.
- Local drafts survive events, navigation, reload, and a failed request. Logout clears them.
- Submission deduplication, answer retry protection, revision checks, and agent retrieval state.
- Expiry checked during decisions, passkey lookup failures fail closed, durable dispatch intent,
  truthful logical error events, unknown completion outcome, URL credential masking, wait deadline fix.
- Synthetic file read/write, failure, delayed response, and sign-in-required tools.
- Six initial sample inbox items; verification requests remain as truthful resolved history.

Measured validation:

- Full inbox package passed; focused gateway, API, passkey, and upstream tests passed.
- HTTPS, secure cookies, unauthorized access refusal, CSRF checks, and Google sign-in passed.
- Both synthetic connectors report healthy (five tools total).
- MCP question submission deduplicates a repeated key.
- A tool cannot write without a grant, with changed parameters, or twice with one grant.
- Browser multiple-choice and custom-only text answers reached the agent unchanged.
- Empty answers are rejected and exclusive options deselect other choices.
- Drafts survive a full reload and a simulated failed submission; retry succeeds.
- Live events report connected. The list count excludes snoozed requests.
- Saved answers and retrieval state survived a release restart and a real rollback/restore cycle.
- The current release reports `stage-9eac6b2`, with two connected upstreams.

The preview screenshot and resize operations fail in the attached desktop host.
Browser evaluation and navigation work. Responsive layout checks used the real application
in a 390-pixel same-origin frame. This is not a physical mobile or passkey device test.

GitHub rejected both draft and regular PR creation for `bk-agent-01` with
`GraphQL: must be a collaborator (createPullRequest)`. The source branch is published.
No PR was created, linked, or merged. This restriction does not affect the live stage.
Research notes and evidence were initially local. The user authorized publication with the laptop-agent handoff on 2026-10-04.
