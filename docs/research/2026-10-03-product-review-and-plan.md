# Toolyard product review and improvement plan

Research date: 3 October 2026. Source revision: dacb8d387b132e789b0e5434ea1620a9076ce9e2.

**Stage update:** The [stage build brief](2026-10-03-stage-build-brief.md) supersedes this report's first-release scope and four-destination navigation. It uses fresh stage data, removes Sessions from primary navigation, and specifies free-text and multiple-choice answers with a Linear-inspired interface. The findings below remain the broader research baseline.

**Recommendation: make Toolyard the place where a person understands an agent request, grants precise permission, and verifies the result.**

The product already has much of the required foundation. Its interface gives too many secondary capabilities the same prominence as this main task. Several state and audit defects also weaken confidence. Fix these defects before a broad visual redesign or another feature expansion.

This report is an assessment and a proposed delivery plan. It does not change the application, approve live requests, alter policies, or deploy code.

## 1. Evidence and limits

I inspected the authenticated browser preview, source code, repository documents, recent session history, and relevant official product documentation. I also exercised the current frontend against synthetic API responses in a separate browser tab.

The live JavaScript asset matched the local asset by SHA-256:

    75adc888e55054231545fdaeb7849d1572bc6944caf5f99ec70e0771495944a5

The fixture used the unchanged application HTML, JavaScript, and CSS. It replaced API responses, blocked writes, and used synthetic requests and identities. It proves frontend behavior with those responses. It does not prove backend behavior under production faults.

Evidence labels in this report mean:

| Label | Meaning |
| --- | --- |
| Live | Observed in the authenticated preview during this review |
| Reproduced | Repeated in the current frontend with synthetic API responses |
| Source | Directly supported by the current code; no new backend fault test |
| Hypothesis | Plausible explanation or product opportunity that requires validation |

The [browser probe results](evidence/2026-10-03-ui-probes.json) preserve several repeatable findings. The [live Inbox screenshot](evidence/2026-10-03-live-inbox.png) records the navigation and empty state. The [static Inbox concept](2026-10-03-inbox-concept.html) illustrates the proposed structure with example data. It does not settle the final theme.

I did not execute live approvals, deployments, paid audio generation, connector changes, passkey challenges, or push subscriptions. I did not conduct user interviews, a full accessibility audit, a load test, or an installation test. Browser automation later lost access to the authenticated context. I do not classify that event as a Toolyard logout defect.

The following snapshot describes this development instance. It is not an adoption or production reliability baseline.

| Observation | Measured result | Interpretation |
| --- | --- | --- |
| Desktop navigation | 16 destinations | Main tasks compete with administration and extensions |
| Desktop page width | 1,482px content at a 1,402px CSS viewport | The page has horizontal overflow |
| Inbox | 0 Needs you, 5 Updates, 13 Done | Empty state and history were available for review |
| Enrolled agents | 6 | Too little evidence to infer active daily use |
| Server entries | 19 | Connection management already matters |
| Server status | 14 OK, 3 disabled, 2 without a recorded status shown as errors | These are distinct states, not five verified outages |
| Tool catalog | 668 tools | A flat catalog is a poor default operator view |
| Seven-day Insights | 307 calls, 82 errors, 30 historical agent identities | Test activity is mixed into this sample |
| Latency | p50 20ms; p95 120,012ms | Wait operations distort a combined latency metric |

## 2. Product direction

The primary user is an operator who supervises several agents and must respond from either a desktop or a phone. A developer connects agents and services. An administrator manages access, policy, and recovery. These roles can belong to one person, but their tasks require different views.

The central question is: **What needs me, what exactly will I permit, and what happened after I permitted it?**

Toolyard's own [product principles](../guidelines/principles.md) already describe this direction. They specify one inbox, bounded authority, asynchronous work, clear evidence, and a complete result record. The proposed changes make that promise more visible and dependable.

An earlier architecture session selected one organization with many users and agents, with Kubernetes availability as a longer-term direction. Treat that as context. This review does not authorize an infrastructure migration.

Four concepts must remain distinct:

| Concept | User question | Example state |
| --- | --- | --- |
| Decision | Did a person permit the request? | Approved, denied, returned, unanswered |
| Permission | Can this agent still use the permission? | Active, consumed, expired, revoked |
| Execution | Did the tool run? | Not started, in progress, failed, completed |
| Outcome | Did the intended result occur? | Awaiting evidence, agent reported, independently verified |

The current Done filter includes approved requests with unused permissions. That is valid as a decision archive, but ambiguous as a task result. Rename and separate these concepts before richer analytics.

**Positioning proposal:** “One inbox for agent decisions. Precise permissions. A clear record of what happened.”

This is a product hypothesis, not a validated market claim. Toolyard does not need to become a general workspace, memory platform, or agent runtime to deliver it.

## 3. What to preserve, combine, and remove from the default experience

| Area | Decision | Reason and boundary |
| --- | --- | --- |
| Scoped, bounded permissions | Preserve | Exact parameters, identity binding, expiry, and limited use are central strengths |
| Narrower parameters and shorter expiry | Preserve and clarify | These let the person reduce authority without a new request |
| Passkey confirmation | Preserve | Strong confirmation belongs on consequential decisions |
| Per-user connections | Preserve | Users must understand whose upstream account executes a call |
| Reasons, evidence, dry runs | Preserve | They help the person judge the request before permission |
| Quiet hours, digests, snooze | Preserve and repair | These protect attention; snooze currently disagrees with the queue count |
| Inbox and legacy Approvals | Combine the interface | Keep a compatibility path until existing callers migrate |
| Servers and My connections | Combine under Connections | Separate organization configuration from each person's sign-in within one destination |
| Events, Audit, Hooks, Insights | Group under Activity | Keep filters and specialist views without four primary destinations |
| Alerts | Put actionable failures in Inbox | Keep alert rules in Settings; retain links to detailed event records |
| Policy controls inside Insights | Move to Policies | A report must not double as an easy route to broader permissions |
| Tools | Move under Connections and a developer view | Search by service, task, and access; do not lead with 668 rows |
| Users and Agents | Group administration | Keep live sessions distinct from enrolled agent identities |
| Memory, MemPalace, notes, skills | Make explicit extensions | Preserve data and APIs; remove default navigation and automatic dependency installation |
| Live Call | Keep experimental and opt-in | Its benefit and usage remain unverified; it introduces separate setup and cost |
| Listen on a request | Keep as a compact option | On-demand audio already exists; it must not dominate the request |
| Secret diagnostics and push recovery | Move to advanced settings | Normal setup must not compete with JWT details or destructive recovery controls |
| Legacy data-lake narrative | Retire from the main pitch | The README, architecture records, and current UI describe different products |

Do not delete existing user data or protocol endpoints as a visual cleanup. First remove the feature from default navigation. Then measure usage, provide export or migration, and publish a compatibility period. Remove the backend only after its users have a supported path.

The same rule applies to extensions. A deployment that uses memory must retain it during an upgrade. A fresh installation can default to the smaller core.

## 4. Findings that must shape the work

Priorities are proposed work order. P0 means a trust boundary requires early validation and repair. It does not mean this review proved an active incident.

### P0: trustworthy permission and execution records

| ID | Evidence | Finding and user impact | Proposed change and acceptance condition |
| --- | --- | --- | --- |
| T01 | Source | Dispatch ignores audit write errors. A restricted action can proceed without a durable record. | Require a durable dispatch intent before the side effect. If persistence fails, block the restricted action. Test this fault explicitly. |
| T02 | Source | The gateway records call.succeeded before it checks a tool result's IsError field. Metrics and audit can disagree. | Classify transport and logical tool results before the final audit event. An IsError result must never produce call.succeeded. |
| T03 | Source | Inbox Decide checks pending status, but does not compare request expiry before it issues grants. The sweep runs separately. | Check expiry inside the decision transaction. A pending request past its expiry must issue zero grants. |
| T04 | Source | Passkey Enabled ignores a database query error and returns false. A transient query fault could bypass the enabled gate. | Return and handle the error. A failed security-state read must not downgrade required confirmation. |
| T05 | Live + Source | Server rows display raw URLs. The live list included credential-bearing URL components. | Redact secrets before API display and logs. Use secret references for configuration. Test query values, userinfo, and opaque path tokens. |

T01 requires careful result semantics. An upstream action can succeed before its final audit write fails. Do not retry that side effect automatically. Preserve a durable attempt ID, expose an unknown outcome, and reconcile it with the upstream system.

T05 proves credential exposure in an administrative display. It does not prove unauthorized access or exfiltration. Review who could access affected records before any separate credential response.

### P1: daily usability and reliable state

| ID | Evidence | Finding and user impact | Proposed change and acceptance condition |
| --- | --- | --- | --- |
| U01 | Reproduced | A decision detail refresh erases an unsent denial note. | Store drafts outside rendered DOM. Preserve text, focus, selection, and changed scope through at least 20 updates. |
| U02 | Reproduced | An audit event erases an unfinished custom-server name and command. | Retain form state and update only affected regions. Unrelated events must not reset forms. |
| U03 | Live + Reproduced | The desktop header overflows. Sixteen destinations compete for space. | Use four primary destinations and role-based secondary navigation. No page overflow at the target viewport sizes. |
| U04 | Reproduced | The Inbox badge shows 2 while Needs you shows 3, including a snoozed request. | Use one queue definition. A snoozed request leaves Needs you and returns at its due time. |
| U05 | Reproduced + Source | Approved requests with unused grants appear in Done. | Separate decision, permission, execution, and outcome. Approval must not imply task completion. |
| U06 | Reproduced + Source | The default admin route is Approvals even when approval_mode is inbox. | Route to the active decision experience. Preserve old links with an explicit compatibility view. |
| U07 | Reproduced | App navigation replaces browser history. Two route changes leave history length unchanged. | Record deliberate navigation. Back must restore the previous view, filter, scroll position, and request. |
| U08 | Reproduced | After an Inbox API failure, old data remains under a live stream indicator with only a transient toast. | Show per-resource freshness and a persistent retry action. Stream health must not imply fresh Inbox data. |
| U09 | Reproduced | The More sheet and Add agent modal lack dialog semantics and focus containment. Toasts lack live-region semantics. | Use accessible dialog and status patterns. Verify keyboard entry, Escape, focus return, and announcements. |
| U10 | Live + Source | Existing server connections appear below marketplace cards, JSON import, and a custom form. | Put connection health first. Open setup through Add connection. |
| U11 | Live | A server can show Connected beside an OAuth no-token state. | Show transport, catalog access, and the current user's authorization separately. Explain which action remains blocked. |
| U12 | Live + Source | Setup and technical recovery controls share one long Settings page. Some text still describes legacy approvals. | Split normal preferences, security, integrations, and advanced recovery. Match copy to the active mode. |
| U13 | Reproduced | A short synthetic request places its decision section below the first desktop viewport. Audio appears before the written message. | Show a compact summary and scope first. Make Listen optional. Keep clear access to evidence and decision controls. |
| U14 | Source | Audit pagination uses only a timestamp cursor. Equal-timestamp events can disappear across page boundaries. | Use a stable timestamp-plus-ID cursor. Retrieve every event exactly once in a same-millisecond boundary test. |
| U15 | Source | Inbox lists cap open items at 500 and closed items at 100, without a cursor. Some attention scans also cap at 500. | Add complete pagination and full due-item scans. Verify an older actionable request beyond item 1,000 remains reachable. |
| U16 | Source | Request persistence, publication, and notification enqueue are separate steps. Queue errors can only be logged. | Commit the request and notification intent together, then deliver through retries. Test a crash between commit and delivery. |
| U17 | Source + Hypothesis | inbox.wait advertises up to 300 seconds, while normal dispatch has a 120-second cap. Live wait latency clusters at 120 seconds. | Give intentional wait operations their own bounded deadline. Verify cancellation, timeout, and decision completion independently. |

U17 has strong source support for a mismatch. The live latency pattern is consistent with that mismatch, but this review did not trace a specific failed call end to end.

### P2: coherence, support, and maintainability

| ID | Evidence | Finding | Proposed change |
| --- | --- | --- | --- |
| C01 | Live | A Beknown identity check reports a registry 403 without enough task-level recovery context. | Explain the affected connection, owner, consequence, and exact repair action. Do not label every connection broken. |
| C02 | Live + Source | Enabled agents can appear active despite stale last-seen times. | Separate enabled identity, online connection, active session, and last activity. |
| C03 | Source | Mobile More omits destinations that desktop exposes, including Events and MemPalace. | Use one route registry for desktop, mobile, roles, and extension visibility. |
| C04 | Live + Source | Insights mixes test traffic, intentional waits, policy coaching, and operational failures. | Add traffic labels and separate waiting time from execution time. Show a meaningful denominator. |
| C05 | Source | The UI file has about 8,200 lines and broad root renders. | Introduce stateful view modules and shared components as each main flow changes. Avoid a wholesale rewrite first. |
| C06 | Source | Health reports a fixed version and unconditional OK. The Docker healthcheck calls wget without an explicit runtime installation. | Expose the build version and dependency readiness. Verify the container probe in CI. The missing-binary concern remains untested. |
| C07 | Source | README, self-host instructions, and About contain stale product or repository details. | Align supported commands, installation paths, active approval mode, and scope with the current release. |
| C08 | Live | The empty Inbox says agents are working without evidence that they are active. | Say “Nothing needs your review.” Show actual session states separately. |

The interface already has focus-visible styles and reduced-motion support. The accessibility recommendation extends those strengths. It is not a claim that all accessibility support is absent.

## 5. Proposed information architecture

Use four primary destinations on desktop and mobile. Put organization administration in a secondary area that matches the user's role.

| Destination | Main content | Secondary views |
| --- | --- | --- |
| Inbox | Requests that require a person, updates, and recovery actions | Needs you, Snoozed, Updates, History; filters by agent, session, risk, and owner |
| Sessions | What each agent task does and what prevents progress | Active, Needs you, Stale, Completed; linked request and result timeline |
| Connections | Existing services and the current user's access | Service health, Your sign-in, Tools, Add connection, advanced configuration |
| Activity | Searchable account of requests, decisions, calls, and results | Audit, operational events, hooks, and Insights |
| Secondary administration | Policies, People & agents, Settings | Extensions and advanced recovery remain here |

This groups capabilities without flattening their permissions. A member still sees only authorized resources. A navigation change must not widen API access.

**Desktop layout.** Use a compact left rail, a queue pane, and a request detail pane. Preserve the selected request as new items arrive. At narrower widths, show one pane at a time and preserve Back behavior. Avoid a fixed-width three-column layout.

**Mobile layout.** Use the same four destinations in a bottom bar. Show the request summary before optional media. Keep decision controls reachable without covering scope, evidence, errors, or the phone's safe area.

## 6. The request experience

The first screen must explain the action in plain language. It must expose enough concrete detail to detect a wrong environment, recipient, resource, or revision.

Recommended order:

1. Show the task, agent, session, and person whose account will act.
2. State the requested action and its target.
3. Show required permissions, optional permissions, expiry, and allowed uses.
4. Show material risk, evidence freshness, and the recovery path.
5. Offer Approve selected, Request changes, Deny, and Snooze.
6. Keep the evidence and activity record directly accessible.

Use labels such as “Approve test deployment” when the exact scope supports them. Avoid a generic green button whose action is unclear. A successful approval message must say that permission was granted. It must not imply execution.

For a short request, the first desktop viewport must show the summary, target, scope, and next action. Longer evidence can expand below it. Raw JSON remains available for inspection, but the person must not decode JSON to understand a common action.

Default optional consequential permissions to unselected. Explain any prerequisite that makes a permission required. Preserve selected scope during updates. If the agent changes material scope, invalidate the prior review and require a new decision on the new version.

Keep the existing parameter narrowing and shorter expiry controls. Give them a plain-language summary beside the final action. Keep strong confirmation for consequential actions. Do not introduce extra confirmations for harmless interface actions.

Keep Listen beside the written summary. Display a large audio player only after the person chooses audio. Clearly mark audio or generated explanations as summaries of the authoritative request.

The system already supports deny notes and a return-to-replan action. Extend these into a clear request revision thread. Do not present this as a need to build basic denial from scratch.

A result timeline can show:

    Request received → Permission granted → Tool executed → Result reported → Result verified

Any stage can instead show failed, expired, revoked, cancelled, or outcome unknown. “Agent reported success” and “Toolyard verified success” must remain distinct.

## 7. Visual and interaction polish

Use a small design system before individual page restyling. Define typography, spacing, color, controls, states, and motion once. Apply it first to Inbox, Connections, and Settings.

| Area | Proposed standard |
| --- | --- |
| Type | 16px body text as the normal target; 14px secondary text; concise headings; tabular numerals for metrics |
| Spacing | A consistent 4px base with 8, 12, 16, 24, and 32px steps |
| Color | Neutral surfaces, one action accent, and semantic warning/error/success colors with text labels |
| Density | Comfortable default; an optional compact table view for large catalogs |
| Controls | One primary action per region; consistent destructive actions; clear disabled reasons |
| Touch | Aim for 44px main touch controls; verify WCAG target size or spacing requirements |
| Tables | Sticky headers where useful; explicit filters; readable cells; overflow within the table, not the page |
| Motion | Short state transitions; no animation requirement for task completion; honor reduced motion |
| Focus | Visible focus, predictable order, keyboard-safe menus, and correct dialog focus return |
| Feedback | Local pending state, field errors, persistent failure details, and accessible status announcements |

WCAG 2.2's minimum target criterion uses 24 CSS pixels, with exceptions such as sufficient spacing. The 44px recommendation is a design preference for this mobile task, not that criterion's minimum. See [target size](https://www.w3.org/WAI/WCAG22/Understanding/target-size-minimum), [reflow](https://www.w3.org/WAI/WCAG22/Understanding/reflow.html), [status messages](https://www.w3.org/WAI/WCAG22/Understanding/status-messages.html), and [modal dialogs](https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/).

Preserve existing content during background refresh. Use a skeleton only when no useful content exists. Show “Last updated” and a retry action when data becomes stale. Do not replace a request with an empty state after a temporary network error.

Preserve the existing dark theme while the shared design system is introduced. Add a light theme only after the main states and contrast are verified. A new brand, decorative animation, or new logo is not required for this release.

## 8. Setup and recovery

First use must lead to one complete, safe task. Do not ask a new user to understand policies, secrets, hooks, memory, and voice before that task.

Proposed first-run sequence:

1. Establish the user's identity and explain their role.
2. Connect one service from a curated set or import existing MCP configuration.
3. Connect one agent with client-specific instructions and an explicit connection check.
4. Submit a harmless sample request with a clear test label.
5. Complete the decision and show its execution result.
6. Offer push notifications after the person sees their value.

Track completion on the server so the user can continue on another device. Retain a developer route for custom endpoints and raw configuration. Do not force experienced users through every introductory step.

The connection card must distinguish:

| State | Meaning | Next action |
| --- | --- | --- |
| Service reachable | Toolyard can reach the upstream server | Inspect catalog status if tools are absent |
| Catalog available | Tool definitions were fetched | Inspect user authorization if execution is blocked |
| Your account connected | The current user's credential is valid | Show account identity and scope |
| Policy permits request | Toolyard permits or can request this action | Explain any required human decision |
| Last check failed | A particular check failed at a known time | Retry that check or direct the responsible person to repair it |

Use recovery text with four parts: what failed, which work is affected, who can repair it, and the next action. Hide raw traces under details. Redact credentials in every state.

Offer a safe connection test. Do not run an arbitrary write tool as a health probe. Provide a redacted diagnostic export for support. Make token rotation and deletion separate from ordinary reconnect actions.

## 9. Useful features, in value order

| Rank | Feature | Existing foundation | Benefit and scope boundary |
| --- | --- | --- | --- |
| 1 | Complete result tracking | Grants, audit, request activity, agent updates | Show unused permissions, failed execution, missing reports, and verified outcomes in one place |
| 2 | Search and saved views | Inbox categories and catalog search | Find requests across session, agent, service, status, owner, and date; preserve filters in links |
| 3 | Request changes and revisions | Return-to-replan and notes | Keep clarification with the original task; compare changed scope before a new approval |
| 4 | Connection diagnosis | Server status, OAuth state, per-user connections | Give one precise repair path without a tour through technical settings |
| 5 | Notification delivery status | Push queue, digests, quiet hours, snooze | Explain sent, deferred, failed, or suppressed notifications without more interruptions |
| 6 | Policy explanation and simulation | Policy engine and existing controls | Explain which rule applied; preview a proposed rule against historical or sample calls without executing them |
| 7 | Scoped assignment and backup approvers | Users, roles, identity attribution | Avoid an absent owner's bottleneck; preserve scope limits and distinct actor records |
| 8 | Command search and safe shortcuts | Existing focused-card shortcuts | Navigate quickly; keep approval shortcuts disabled in editable fields and require the correct focused request |

Ranks 1–5 directly address the observed product gaps. Ranks 6–8 require user validation before a broad build.

Group related requests by session to reduce clutter. Do not add blind bulk approval. If batch review is later justified, preserve every exact scope and make partial selection explicit.

Do not build a general chat client, autonomous workflow designer, marketplace business, new memory engine, or separate analytics warehouse in this polish release. No evidence from this review shows that these are the main user constraint.

## 10. External research and design lessons

These are first-party feature descriptions, not hands-on competitor tests or proof of comparative quality. The recommendations below are inferences for Toolyard.

| Source | Relevant pattern | Toolyard application |
| --- | --- | --- |
| [ToolHive documentation](https://docs.stacklok.com/toolhive) | Distinct paths for desktop setup, CLI operation, shared infrastructure, and curated server discovery | Keep a simple first connection path and an advanced administration path. Avoid exposing deployment architecture in the daily inbox. |
| [Arcade gateways](https://docs.arcade.dev/en/operate/governance/mcp-gateways) and [remote MCP servers](https://docs.arcade.dev/en/operate/governance/remote-mcp-servers) | Curated tool access and separate administration versus end-user authorization | Show the user's account and usable tools distinctly from server connectivity. |
| [Permit MCP Gateway](https://docs.permit.io/permit-mcp-gateway/overview/) | Per-call authorization, consent, and attributable decisions | Make the human, agent, scope, and applied rule visible. Avoid promises that a gateway makes every upstream system safe. |
| [Portkey MCP](https://portkey.ai/features/mcp) | Central catalog, access management, and call visibility | Join service, tool, identity, latency, and outcome in a useful investigation view. |
| [Linear Inbox](https://linear.app/docs/inbox) and [Triage](https://linear.app/docs/triage) | Focused intake, snooze, filters, and explicit queue actions | Make snooze remove work from the current queue. Make ownership and next action easy to find. |
| [GitHub deployment review](https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/review-deployments) | A concrete environment and explicit approval or rejection | Show the exact environment and revision. Preserve separate repository and deployment approval requirements. |

Toolyard's opportunity is the full human decision experience across agents. A larger server catalog alone will not establish that distinction. This conclusion is a product inference that requires user interviews and task trials.

The [MCP tool specification](https://modelcontextprotocol.io/specification/2025-06-18/server/tools) distinguishes logical tool errors through IsError. It also warns against trusting tool annotations from untrusted servers. Toolyard must preserve both distinctions in its audit and policy design.

The current policy engine uses tool-name heuristics as a read fallback. Caller-declared intent can escalate a request, which is a useful safeguard. Move toward trusted capability definitions, explicit overrides, and a visible policy reason. Do not simply trust an upstream read-only annotation or the agent's own risk description.

## 11. Engineering approach

Repair the existing product in bounded changes. Keep the Go gateway and embedded frontend while the core flows become easier to test and maintain.

1. Establish one request-state contract across API, frontend, notifications, and audit.
2. Add durable intent records for restricted dispatch and notification delivery.
3. Correct expiry, passkey-error handling, logical error classification, and cursor pagination.
4. Give forms and decision drafts stable state independent of DOM replacement.
5. Extract shared components and view modules as Inbox, Connections, and Settings change.
6. Generate navigation from one role-aware route registry.
7. Add focused browser tests for critical state transitions and keyboard access.
8. Verify installation, health probes, version reporting, and release checks in CI.

Choose a frontend framework only if a small prototype proves a material maintenance benefit. The observed defects require better state ownership. They do not prove that a specific framework is necessary.

For uncertain side effects, use a durable attempt identifier and idempotency where the upstream supports it. Where it does not, require reconciliation before retry. An optimistic success toast is not a substitute for a verified result.

Keep policy enforcement on the server. Request summaries and risk explanations can assist a decision, but they cannot expand permissions. Every decision must bind to the reviewed request version and exact allowed parameters.

The earlier high-availability direction needs a separate design for shared state, worker ownership, leases, migrations, and rollout. Do not represent a UI polish release as proof of safe multi-replica operation.

## 12. Delivery sequence and acceptance

Estimates are initial engineering ranges, not commitments. They assume one experienced frontend engineer and one backend engineer, with part-time design and QA support. Discovery may change the ranges.

| Phase | Work | Estimated effort | Release condition |
| --- | --- | --- | --- |
| A. Baseline | Confirm state definitions, reproduce trust risks in focused tests, collect current task timings | 2–3 engineer-days | Agreed acceptance examples and a sanitized baseline |
| B. Trust and state | T01–T05, U01–U02, U08, U14–U17; corrected result records and redaction | 10–15 engineer-days | Fault tests prove no unauthorized expiry path, no false success, no lost notification intent, and no erased drafts |
| C. Core experience | Four destinations, one decision surface, concise request detail, state labels, accessible components | 10–15 engineer-days | Main tasks pass desktop, mobile, keyboard, and browser-history checks |
| D. Setup and recovery | Connections, guided first task, clear repair paths, settings separation, current docs | 6–10 engineer-days | New users complete a safe first task without help; connection faults have exact repair paths |
| E. Focused feature pilot | Search, saved views, request revisions, missing-result follow-up, delivery visibility | 8–12 engineer-days | Pilot users complete real tasks with less confusion and no broader authority |
| F. Later team expansion | Scoped delegation, richer policy simulation, and availability work | Separate estimate | Real usage demonstrates the requirement and a dedicated design passes review |

Phases A–E total approximately 36–55 engineer-days. With two engineers, dependencies and validation suggest roughly five to eight calendar weeks. A narrower first release can include A–C. The estimate excludes a new database or Kubernetes availability migration.

The first implementation slice should fix draft retention, raw URL display, default routing, snooze counts, and audit result classification. In parallel, prove the permission and persistence fault cases. This provides early value without postponing the deeper trust work.

Roll out the new navigation and Inbox behind a reversible release control. Preserve request links and old API clients. Observe usage before removal of the legacy approval interface. Keep a documented rollback for UI changes and a tested migration path for persisted state.

### Required regression scenarios

| Area | Scenario that must pass |
| --- | --- |
| Drafts | Type a denial note and a connection form; deliver 20 unrelated updates; preserve values, focus, and selection |
| Expiry | Decide just before and just after expiry; include a paused sweeper and a concurrent decision |
| Security state | Fail the passkey-state query; issue no permission through a downgraded confirmation path |
| Audit | Fail intent persistence before dispatch; verify no restricted side effect; fail result persistence after dispatch; show unknown outcome |
| Tool errors | Return IsError with no transport error; display and record failure consistently |
| Pagination | Insert many events with one timestamp; retrieve all once; reach actionable request 1,001 |
| Notifications | Crash after request commit and before delivery; restart; deliver once or deduplicate safely |
| Waits | Let a 180-second wait exceed 120 seconds; verify deliberate wait behavior, cancellation, and prompt completion after a decision |
| Navigation | Open a request link, apply filters, change pages, and use Back without loss of context |
| Mobile | Verify 320, 390, 768, 1,280, and 1,440px widths; test the keyboard and safe areas on a real phone |
| Accessibility | Complete setup and a request by keyboard; verify dialogs and status announcements with a screen reader |
| Permissions | Repeat key views as admin and member; verify API denial as well as hidden navigation |
| Connections | Exercise unreachable service, missing user token, expired token, empty catalog, and forbidden tool states |
| Deployment | Run the built container's actual healthcheck; report the real build version; verify backup restore in a test environment |

## 13. Success measures and user validation

The targets below are proposed acceptance goals. They are not measured current performance.

| Measure | Proposed target | How to avoid a misleading metric |
| --- | --- | --- |
| First successful task | At least four of five new trial users finish within ten minutes without assistance | Include connection, request, decision, and visible result |
| Find the next task | At least four of five operators find the blocked session within ten seconds | Use a realistic queue with updates and snoozed items |
| Scope comprehension | At least 90% correct answers about target, account, allowed action, and expiry | Test understanding before the decision; speed alone is not success |
| Draft retention | Zero losses across the regression scenarios | Include concurrent events, retries, navigation, and detail refresh |
| State consistency | No approval labeled as verified completion before evidence exists | Compare UI, API, audit, and notification wording |
| Record completeness | Every restricted dispatch has a durable intent and a known or explicit unknown outcome | Do not count missing records as successful calls |
| Queue completeness | No actionable request disappears due to a list limit | Test beyond expected normal volume |
| Notification quality | Measure useful actions per interruption and overdue requests | Fewer notifications alone can conceal missed work |
| Recovery | Users identify the owner and repair action for each common connection fault | Separate user confusion from unavailable upstream services |

A five-person trial can expose usability problems. It cannot establish market demand or statistically reliable retention. Follow the first trial with a two-week pilot of real agent tasks.

The trial must include a new operator, an experienced operator, a mobile-heavy user, and a developer or administrator. Ask each person to connect one service, handle a scoped request, request a correction, find a failed result, and repair an expired sign-in.

Collect task duration, errors, missed scope details, backtracks, and the person's explanation of the outcome. Do not collect raw credentials or sensitive request arguments for product analytics.

Instrument the path from request creation through view, decision, permission use, result report, and verification. Separate test agents, expected policy refusals, human wait time, tool execution time, and actual service faults.

## 14. Existing work and source map

The repository already has open work that overlaps parts of this plan:

| Existing PR | Relationship to this review |
| --- | --- |
| [PR #9](https://github.com/iamtushar324/toolyard/pull/9) | README emphasis on the approval gateway and removal of data-lake sections |
| [PR #10](https://github.com/iamtushar324/toolyard/pull/10) | Removal of the stray macOS binary and Grafana leftovers |
| [PR #11](https://github.com/iamtushar324/toolyard/pull/11) | Tests, CI, and migration numbering |

Their existence is not proof that their changes are merged. Reconcile the relevant changes with their owners before implementation. This review did not edit or take ownership of those PRs.

Source anchors below refer to the reviewed revision. Function names make the findings easier to locate after lines move.

| Findings | Source |
| --- | --- |
| T01–T02; dispatch and audit | [gateway server](../../internal/gateway/server.go), especially dispatch around lines 1,620–1,695 |
| T03; approval expiry | [Inbox service](../../internal/inbox/service.go), Decide at line 509 and expiry sweep at line 1,163; [request state](../../internal/inbox/types.go), IsOpen |
| T04; passkey read failure | [passkey service](../../internal/passkey/passkey.go), Enabled at line 417 |
| T05; raw URLs | [dashboard](../../web/dashboard/app.js), transportLabel at line 2,876 |
| U01; decision draft | [dashboard](../../web/dashboard/app.js), loadInboxDetail, ibRefreshRegions, ibNote, and ibPanelEl |
| U02; form reset | [dashboard](../../web/dashboard/app.js), handleAuditEvent at line 526 and viewServers at line 2,691 |
| U04–U07; queue and navigation | [dashboard](../../web/dashboard/app.js), defaultRoute, navigate, inboxNeeds, inboxDone, and inboxBadgeCount |
| U08–U13; UI states | [dashboard](../../web/dashboard/app.js), loadInbox, toast, renderMoreSheet, renderAgentModal, and inboxDetailNode; [styles](../../web/dashboard/style.css) |
| U14; timestamp cursor | [audit queries](../../internal/audit/audit.go), Query and RecentBefore |
| U15; list limits | [Inbox routes](../../internal/api/inbox_routes.go), list handler; [Inbox service](../../internal/inbox/service.go), List; [attention](../../internal/inbox/attention.go), due-item scans |
| U16; notification gap | [Inbox service](../../internal/inbox/service.go), request creation near line 300; [attention](../../internal/inbox/attention.go), enqueuePush and enqueueArrival |
| U17; wait deadline | [Inbox tools](../../internal/gateway/inbox_tools.go), wait description; [gateway options](../../cmd/gateway/main.go), upstream-call-timeout; [dispatch](../../internal/gateway/server.go), noCallTimeout |
| C05; rendering scope | [dashboard](../../web/dashboard/app.js), render and state ownership |
| C06; health and version | [API](../../internal/api/api.go), health route; [Dockerfile](../../deploy/Dockerfile) |
| C07; documentation | [README](../../README.md), [self-host guide](../self-hosting.md), [lake ADR](../adr/0004-lake-engine.md) |
| Policy trust | [policy engine](../../internal/policy/policy.go), Eval and readVerbs |
| Optional dependencies | [gateway startup](../../cmd/gateway/main.go), mempalace, notes, skills, and voice options |

**Delivery recommendation:** release the trust fixes and the focused Inbox first. Then validate setup, recovery, and result tracking with real users. Expand the product only where those users demonstrate a repeated need.
