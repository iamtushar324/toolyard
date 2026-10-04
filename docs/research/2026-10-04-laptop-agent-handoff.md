# Toolyard stage handoff — laptop agent

## Goal and authority

Continue the user's goal: “build it out as stage , and let me know when i can use it and give you more feedbacks”.

The user requested a deeper product review, removal of unnecessary product surfaces, professional UI and UX in Linear's style, issue fixes, and useful features. The user approved fresh stage data and test connectors. The latest requests target Settings, automatic online model prices instead of manual cost fields, and removal of the visible Settings category scrollbar.

The user now requested this GitHub handoff to an agent with laptop access. This is a handoff of the existing Toolyard task. The proposed stop of the server AI provider did not receive approval and did not occur. Do not treat historical pending permission questions as approval. No new sign-in or role-change request is pending for this server agent.

Work from `origin/t3code/jakarta` in your own checkout or worktree. The server agent stops source edits after this handoff. Check other agents and open PRs before new work. Do not merge to `main`, change the live service, reset stage data, or modify T3 itself without the user's explicit approval for that action.

## Repository and delivered code

- Repository: <https://github.com/iamtushar324/toolyard> (public).
- Branch: `t3code/jakarta`.
- Latest application commit: `a9aec1fd2493ca548eb859f9f124085ca54ff383`.
- This handoff commit adds research, prototypes, evidence, and this document. It does not change the application binary.
- No PR exists for this branch. GitHub rejected PR creation by the server's CLI actor with `must be a collaborator`. Branch pushes work.
- No Linear ticket is linked to the source session, “Research Product Improvements”.

Three application commits contain the work:

| Commit | Result |
| --- | --- |
| `9eac6b2` | Isolated stage, synthetic connectors, focused navigation, structured questions, durable answer drafts, retry protection, permission and audit fixes. |
| `af4c912` | Shared workspace theme, seven Settings categories, explicit Save/Discard, durable preference drafts, consistent tools and dialogs. |
| `a9aec1f` | Automatic online model catalog, removal of manual rates and false spend estimates, hidden mobile Settings scrollbar with horizontal access. |

## Stage that the user can use

- Stage: <https://toolyard.stage.dev.beknown.live/>.
- Settings: <https://toolyard.stage.dev.beknown.live/?build=a9aec1f#settings/general>.
- Health: <https://toolyard.stage.dev.beknown.live/v1/health>.
- Latest handoff check: `ok: true`, `environment: stage`, `version: stage-a9aec1f`, `metrics_dropped: 0`.
- Server: dev-server-1 (`ip-10-31-39-131`, tailnet `dev-server-1.tailab6257.ts.net`).
- Server source worktree: `/home/ubuntu/.t3/bkt3-dev/worktrees/toolyard/jakarta`.
- Stage gateway unit: `toolyard-stage.service`.
- Synthetic connector unit: `toolyard-stage-fixtures.service`.
- Live unit, outside the authorized stage work: `toolyard-inbox.service`.
- Stage release: `/opt/toolyard-stage/releases/a9aec1f`.
- Current release link: `/opt/toolyard-stage/current`.
- Stage data: `/var/lib/toolyard-stage/toolyard.db`.
- Catalog cache: `/var/lib/toolyard-stage/model-prices.json`.
- Backup before the current application release: `/var/lib/toolyard-stage/backups/before-a9aec1f.sqlite`.
- Previous application release: `/opt/toolyard-stage/releases/af4c912`.
- Rollback metadata: `/etc/toolyard-stage/before-a9aec1f.json`.

Read `deploy/stage/README.md` before a stage deployment. Preserve the stage database. Restart only the stage gateway when a validated application change requires it. Do not expose environment files, bootstrap credentials, live-view links, or authentication state in GitHub. The repository is public.

## Product decisions already accepted

- Keep Inbox as the primary place for questions and permissions.
- Remove Sessions from primary navigation. Agent session context remains available through existing APIs.
- Support free text, one choice, and several choices. Allow custom text with or without a selection.
- Use stable option IDs, explicit Send, selection bounds, exclusive options, and durable drafts.
- Preserve drafts through live events, navigation, failed requests, and reloads. Clear them on logout.
- Keep credentials only in memory in the UI. Do not persist secret field drafts.
- Use quiet dark surfaces, restrained purple accents, consistent icons, compact navigation, clear headings, and simple forms.
- Keep stage data and test connectors separate from current work.

The synthetic connectors are `sandbox-files` and `sandbox-errors`. They provide disposable reads/writes, a logical failure, a slow response, and a sign-in-required response. Use those tools for review rather than real accounts or production writes.

## Automatic prices and the spend boundary

`internal/pricing/catalog.go` retrieves <https://models.dev/api.json?type=all>. It validates a bounded response, supports conditional ETags, refreshes every six hours, and stores an atomic cache. A failed refresh preserves the previous valid catalog and retries after 15 minutes. Stale or missing data has a visible state.

`GET /v1/insights/pricing` serves an authenticated local catalog with exact provider/model filters and pagination. Rates are USD per million tokens. Missing rates stay null; published zero rates stay zero. The UI provides input, output, cache read, and cache write columns, source attribution, freshness, search, provider filters, and paging. Tiered or additional rate rules receive a label.

The removed settings are `cost_input_usd_per_m` and `cost_output_usd_per_m`. Reads hide them and writes reject them. Old draft values are removed. Deprecated stored values remain only for rollback.

Toolyard observes MCP payload byte sizes. It does not observe the client's actual model, provider, or provider token categories. Payload bytes divided by four are approximate payload tokens, not billed usage. The Usage UI therefore reports spend as unavailable. `/v1/insights/cost` returns unmetered status and null spend. No real provider usage integration or enforceable spending limit exists yet. Do not restore arbitrary rates or present an invented zero as actual spend.

## Why the server review stopped

The user reports that the direct Toolyard Admin dashboard is visible in BK T3 Code on their laptop. The server agent's native preview remains attached to a different observed state: a hidden 1280×800 direct Toolyard tab, a sign-in form, and `/v1/auth/me` HTTP 401. Snapshot calls fail while JavaScript evaluation works. The same automation client remained attached after the operator reported a reconnect.

The observed browser identifies itself as `BKT3Code/0.0.41-production-nightly.20260927.1` on macOS. This does not prove which desktop runtime contains the user's authenticated dashboard.

A separately authorized Browser Use review achieved HTTP 200, but the account had the Member role. Settings returned 403. Only member pages were checked there. That cloud browser was closed through the approved wrapper. Do not reuse old cloud live-view links.

Read-only T3 source inspection found that each provider session stays pinned to one browser runtime while its connection remains live. A fresh provider credential receives a new assignment key. An incorrect runtime assignment remains a hypothesis; the available server tools could not inspect all broker clients or reset just this assignment. No cookie transfer, forged host state, T3 code change, service restart, or account permission change occurred.

Use your actual laptop access to select the authenticated Toolyard tab. Verify `/v1/auth/me` returns HTTP 200 and `role: admin` before admin tests. Check the URL directly. A Browser Use viewer is a different browser from the direct T3 preview. If the authenticated dashboard is visible but automation sees 401, diagnose your local browser target instead of asking the user to sign in repeatedly.

## First work for the laptop agent

1. Verify the authenticated Admin browser and the deployed version.
2. Review every main page and all seven Settings categories on the actual stage app.
3. Test the current price catalog UI: provider changes, model search, paging, cache columns, missing prices, and late responses.
4. Confirm that General has no manual rate fields and that Usage explains unmetered spend.
5. Test Settings navigation at 320 and 394 CSS pixels. Verify hidden scrollbars, horizontal access to every category, active-category visibility, and scroll position after live renders.
6. Test preference drafts, Save/Discard, failed saves, reloads, keyboard access, dialog focus, and form errors.
7. Test Inbox free text, single and multiple selections, custom text, exclusive options, retries, and draft preservation through live events.
8. Review layout, spacing, typography, empty states, error recovery, mobile navigation, connection states, and permission clarity against the accepted Linear-style direction.
9. Fix observed issues within the Toolyard stage scope. Record each reproduction and result. Use synthetic data.
10. Validate targeted checks, push the changes, and deploy approved stage changes through the server workflow. Report a stage URL for the user's next feedback.

The latest price and scrollbar UI still require authenticated browser verification. Do not report them as browser-tested from the server evidence alone. Earlier `af4c912` draft and layout checks do not prove the latest release's new behavior. Same-origin frame checks do not replace physical-device checks.

## Relevant files and evidence

- `web/dashboard/workspace.css`: shared theme, responsive shell, Settings nav, prices.
- `web/dashboard/settings.js`: categories, forms, draft state, Save/Discard, automatic price status.
- `web/dashboard/pricing.js`: catalog state, filters, paging, late-response isolation.
- `web/dashboard/app.js`: navigation, rendering, Inbox, Usage, tools, dialogs, auth shell.
- `internal/pricing/catalog.go`: online source, validation, refresh, cache, filters.
- `internal/api/insights.go`: price and unmetered usage API.
- `internal/settings/settings.go`: legacy manual-rate rejection.
- `docs/research/2026-10-03-product-review-and-plan.md`: original deep review.
- `docs/research/2026-10-03-stage-build-brief.md`: accepted stage direction.
- `docs/research/2026-10-04-settings-ui-stage.md`: Settings release and measured browser checks.
- `docs/research/2026-10-04-automatic-prices-stage.md`: current price release and limits.
- `docs/research/2026-10-04-t3-preview-investigation.md`: connection investigation and unverified hypotheses.
- `docs/research/evidence/`: measured checks and historical prototype screenshots.

The HTML prototypes are reference designs, not current application builds. The historical live screenshot shows the old UI. All historical findings must be checked against the current stage release before another change.

## Checks that already passed

The current application change passed catalog/cache/outage tests, a live online source check, API authentication and query checks, manual-rate rejection, operator checks, embedded assets, ten Node UI behavior tests, JavaScript syntax, and a targeted gateway build. Fourteen live HTTPS stage checks passed. The price catalog then contained 8,190 text models from 226 providers; that count can change.

For new changes, choose the tests that cover the changed behavior. On the shared server, check memory and load first. Use at most two workers and avoid repository-wide suites.

```sh
node --test scripts/settings-ui.test.cjs scripts/pricing-ui.test.cjs
GOMAXPROCS=2 go test -p 1 ./internal/api -run '^(TestPricing.*|TestOperator.*)$' -count=1
TOOLYARD_TEST_ONLINE_PRICES=1 GOMAXPROCS=2 go test -p 1 ./internal/pricing ./internal/api ./cmd/gateway -run '^(TestCatalog.*|TestPricing.*|TestWorkspaceAssets)$' -count=1
git diff --check
```

Complete the authenticated laptop review before a claim that the stage is fully validated. The user must confirm that the product meets the goal before session archival or a final Done state.
