# Toolyard product scope cleanup — 2026-10-04

The laptop review began at `d5dac28` from `origin/t3code/jakarta`. After reviewing the
current desktop and mobile UI, the owner chose product simplification over another
visual redesign. The owner requested removing pricing, full-session streaming UI,
and hook options, then explicitly chose to remove the built-in memory, Events hub,
and voice-call UI entirely.

## Product boundary

Toolyard connects tools to agents, obtains bounded permission, collects answers,
and records tool calls and outcomes. It is not the agent's chat client, session
recorder, model billing dashboard, memory editor, or voice runtime.

Removed from the dashboard:

- Model catalog, automatic-price Settings card, spend placeholder, and payload-token table.
- Hooks/Agent events feed, hook recipes, and Claude/Codex/Cursor/Conductor hook setup tabs.
- Leftover Sessions dashboard and session-event subscription.
- Memory editor and import/export UI, MemPalace metrics and ingestion webhooks.
- General Events hub, webhook/poller configuration and its event subscription.
- Live voice-call panel, microphone capture, audio worklet and call hardware integration.
- Memory fetch at dashboard bootstrap and cost fetch in tool activity.
- About's backend capability badges, which advertised removed UI features.

The agent setup dialog keeps MCP connection instructions and the permission guide.
Request evidence, on-demand Listen, free text and choice answers, snooze, device
notifications, passkeys, secrets, people/agent administration, connector recovery,
Policies and the tool-call audit remain core features.

Old Hooks/Events links open Activity. Memory/MemPalace links open Settings exports.
Call/Sessions links open Inbox. Request links keep their exact IDs, and Settings
category links remain supported. Both initial URLs and later navigation use these
redirects. The Activity sections are now Calls and Tool activity.

## Data and compatibility

This is a UI removal, with no database migration, purge, seed, credential change or
production deployment. Existing data and compatibility APIs remain available.
Session identifiers remain in request/audit attribution; they do not show full
agent conversations. Actual backend capabilities can still appear in the Tools
catalog when available. Pricing refresh/cache and legacy hooks ingestion remain
backend compatibility features; retiring those protocols is a separate decision.
`/v1/insights/cost` still reports unmetered status and null billed spend. MCP payload
sizes are never presented as measured provider billing.

## Further candidates identified

| Candidate | Recommendation | Reason |
| --- | --- | --- |
| Policy toggles and auto-approval configuration inside Tool activity | Move to Policies | Reports duplicate configuration and can unexpectedly widen access. |
| Legacy Approval queue beside Inbox | Eventually combine | Preserve execute-mode callers and exact decision links during a migration. |
| Raw JSON/schema tool workbench | Keep in an advanced developer section | Useful for connector troubleshooting but secondary to reviewing requests. |
| JWT, push diagnostics and destructive push recovery | Keep behind advanced controls | Troubleshooting should not compete with notification setup. |
| Core overview metrics | Keep | Call/error/latency and approval counts describe Toolyard's own work. |
| Session IDs and actor details in audit | Keep | Identify who executed a tool call without collecting an agent transcript. |

The first two are follow-up product changes, not silently applied security or
protocol changes in this cleanup.

## Validation

- JavaScript syntax checks and `git diff --check` passed.
- `node --test scripts/settings-ui.test.cjs scripts/product-scope-ui.test.cjs`: 11 checks passed.
- `GOMAXPROCS=2 go test -p 1 ./cmd/gateway ./internal/api -run '^(TestWorkspaceAssets|TestPricing.*|TestStage.*)$' -count=1`: passed.
- The retained backend pricing/unmetered API checks remain intentional compatibility coverage.
- Live stage checks and screenshots are recorded after the stage update below.

Earlier laptop observations: desktop and 320/394px Settings categories were
accessible; a preference draft survived category changes, navigation and reload,
and Discard restored the saved value. Question drafts survived navigation and
reload. Two issues remain outside this product cleanup: dialog dismissal loses
keyboard focus, and Cmd/Ctrl+Enter does not send a question answer. These must not
be reported as fixed by the feature removals.
