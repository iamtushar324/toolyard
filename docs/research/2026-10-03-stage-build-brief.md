# Toolyard stage: focused build and design brief

Date: 3 October 2026. Reviewed source: dacb8d387b132e789b0e5434ea1620a9076ce9e2.

**Build a small, complete stage product around Inbox, Connections, and Activity. Make questions a first-class interaction. Use a calm, Linear-inspired interface.**

The requested destination is [Toolyard stage](https://toolyard.stage.dev.beknown.live), on dev-server-1. The user selected **fresh stage data and test connectors**. This brief replaces the earlier four-destination proposal for the first stage release.

This is a research and design deliverable. It does not deploy Toolyard, change DNS, configure identity providers, or modify the application. The [interactive design prototype](2026-10-03-stage-ui-prototype.html) runs only local demonstrations. It uses no application API.

## 1. Changes to the previous recommendation

| Previous proposal | Revised stage decision | Why |
| --- | --- | --- |
| Four main destinations, including Sessions | Three: Inbox, Connections, Activity | A separate task monitor adds another concept before it proves useful |
| Sessions as a primary view | Task context inside a request; optional task filter and source link | The person needs context, not another place to manage work |
| Generic improvements to question threads | Free text, single choice, multiple choice, stable answers, and explicit submission in the first release | The current contract cannot express the requested interaction |
| A broad product roadmap | One usable stage build with fresh data and fixture services | This gives a concrete environment for validation |
| General professional styling | A defined Linear-inspired layout and component system | Theme colors alone will not remove structural clutter |
| Voice as a smaller control | Optional voice in both the UI and the request contract | The backend currently requires an audio script even when voice is disabled |

The current Sessions data is a lightweight record of agent work. It is not a terminal, execution engine, or authoritative tracker. It stores a task title, repository, branch, host, reported status, and heartbeat.

Keep that metadata and compatibility API. Display “Research assistant · Toolyard stage review” above the request. Add a link to the originating T3 thread or other task when available. Let the person filter related requests. Do not require an agent to create a session before it can ask a question.

Only add a separate work overview later if users repeatedly need to find stalled tasks across hosts. Even then, avoid a second issue tracker.

## 2. Deeper findings

These findings extend the [initial research](2026-10-03-product-review-and-plan.md). They come from current source or read-only host checks. They do not imply a production incident.

| ID | Evidence | Finding | Stage consequence |
| --- | --- | --- | --- |
| Q01 | Source | Questions require two to four options. No question type represents free text. | Add an explicit question schema with text, single-choice, and multiple-choice types |
| Q02 | Source | A decision accepts one integer option index. The stored answer is the selected label. | Persist stable option IDs and the exact text as a structured response |
| Q03 | Source | Clicking an option immediately submits the answer | Separate selection from Send answer |
| Q04 | Source | Every request requires an audio script, even if voice is off | Make audio optional; a short question must not require four versions of the same prose |
| Q05 | Source | Options have no stable IDs. Validation does not reject duplicate labels. | Give options immutable IDs and reject ambiguous duplicate labels |
| Q06 | Source | Agent status and wait responses expose a scalar answer string | Extend submission, storage, API, agent response, and UI together |
| Q07 | Source | Notification answers encode a single option index; the service worker shows at most two actions | Open the full question for rich responses; do not try to answer a multi-select question from one notification button |
| Q08 | Source | Notification success says the agent has been told after the server accepts the response | Distinguish answer saved from agent retrieved; do not claim receipt from a database write |
| S01 | Source | The Sessions view treats any open question as blocking, even when the agent reports that other work continues | Show explicit blocking state, not an inference from question type |
| S02 | Source | Live grant counts in Sessions are grouped by agent, then repeated on each session for that agent | Do not present these values as precise task-level permission counts |
| H01 | Host | Stage DNS resolves to the same address as the current Toolyard host | DNS is not the immediate blocker |
| H02 | HTTPS check | Stage certificate validation fails with a self-signed certificate | Add and verify the stage route and certificate before a browser trial |
| H03 | Host | The current service uses systemd behind Traefik and a small Swarm proxy | Reuse that host pattern; do not introduce Caddy or Kubernetes |
| H04 | Host + Source | The running service already disables MemPalace, voice, notes, and skills, but the frontend still exposes related navigation | Make navigation follow enabled capabilities |

Q01–Q07 are supported by [validation](../../internal/inbox/validate.go), [request types](../../internal/inbox/types.go), [decisions and agent views](../../internal/inbox/service.go), [tool schemas](../../internal/gateway/inbox_tools.go), [attention actions](../../internal/inbox/attention.go), and [the dashboard](../../web/dashboard/app.js). Q08 also appears in [the service worker](../../web/dashboard/sw.js). S01–S02 are in [Inbox API routes](../../internal/api/inbox_routes.go), especially inboxSessions.

## 3. A better question tool

### The user experience

The agent asks one clear question. The user can select an answer, select several answers where appropriate, or reply in their own words.

| Question type | Input | Submission rule |
| --- | --- | --- |
| Free text | A multiline text field | Non-empty text |
| Single choice | Radio buttons and a text field | One option, text alone, or one option with additional context |
| Multiple choice | Checkboxes and a text field | A valid set of options, text alone, or a valid set with additional context |

Use the label “Select one option” or “Select all that apply.” Keep “Add context, or write your own answer” visible below choices. Do not require a fake Other selection before text becomes available.

If text is the complete answer, the selection list is empty. If the person chooses options and adds text, the text supplements those choices. Do not infer selections from prose.

If selected options conflict with the written text, show both exactly as submitted. The agent can ask for clarification. The system must not silently choose which part to honor.

The first version must support:

1. An explicit Send answer button.
2. No submission when an option is selected.
3. No default selection, including the recommended option.
4. A recommendation label with a short reason where useful.
5. Draft retention through events, retries, and navigation.
6. Multiline text with Enter for a new line.
7. Ctrl/Cmd+Enter as an explicit submit shortcut.
8. A persistent validation error beside the input.
9. A clear answer receipt with the exact selected labels and text.
10. Later, Dismiss, and a link to the original task.

Use native radios, checkboxes, labels, and fieldsets. The [GOV.UK radio guidance](https://design-system.service.gov.uk/components/radios/) and [checkbox guidance](https://design-system.service.gov.uk/components/checkboxes/) support the distinction between one and several selections. Its [question-page guidance](https://design-system.service.gov.uk/patterns/question-pages/) also supports clear purpose, simple questions, and preserved Back behavior.

Question answers are direction, not tool permission. An answer such as “deploy it” must not mint a grant. A restricted action still needs the separate scoped approval flow.

### Question quality

Do not reduce this work to a textarea beside the current buttons. Improve what agents ask.

The request must explain:

- The actual question.
- Why the answer is necessary now.
- The consequence of each option.
- Whether the agent is blocked or can continue other work.
- Any relevant evidence or source task.

Use one main question per request. Do not ask for an answer the user already gave unless the facts changed. Offer “I do not know” when that is a valid response. Keep options distinct and similar in detail.

For the first stage, support two to eight options. Prefer three to five for ordinary questions. Treat eight as a limit, not a target. Use a text question when a fixed list would distort the answer.

Reject duplicate IDs, duplicate labels, invalid selection limits, and empty prompts. If an option means “None,” mark it exclusive. It must not coexist with other selected options. Free text remains available.

Do not require a language model to validate simple field rules. Model feedback can suggest clearer prose later. It must not block an otherwise valid question during an external model outage.

Make the question prompt authoritative. Derive short queue text where possible. A simple question must not require separate title, summary, body, and audio-script composition.

### Proposed versioned contract

Keep the existing inbox.ask tool name. Add an explicit versioned shape. The example below is a proposal, not an implemented API.

    {
      "schema_version": 2,
      "client_request_id": "stage-review-checks-001",
      "prompt": "Which checks belong in the stage review?",
      "context": "Your answer sets the order of the stage test cases.",
      "blocking": false,
      "question": {
        "type": "multiple_choice",
        "min_selections": 1,
        "max_selections": 3,
        "options": [
          {
            "id": "questions",
            "label": "Question responses",
            "detail": "Free text, multiple selections, and drafts."
          },
          {
            "id": "permissions",
            "label": "Scoped permissions",
            "detail": "Exact scope, expiry, and visible results."
          },
          {
            "id": "connections",
            "label": "Connection failures",
            "detail": "Diagnosis and a precise repair action."
          }
        ]
      },
      "task": {
        "title": "Toolyard stage review"
      }
    }

The first stage always permits a custom text answer. Selection limits apply when the answer contains selected options. A non-empty custom answer with no selection is valid.

Proposed answer:

    {
      "action": "answer",
      "request_revision": 1,
      "submission_id": "answer-001",
      "response": {
        "selected_option_ids": ["questions", "permissions"],
        "text": "Include keyboard access and a failed connection test."
      }
    }

Store the exact response, selected label snapshots, request revision, actor, source, and timestamp. Return the structured response through inbox.status and inbox.wait. Keep a readable answer string for older consumers.

Suggested initial limits are 300 characters for the prompt, 4,000 for context, and 4,000 for the answer. Count Unicode characters consistently. Also enforce a byte limit on the complete body.

Keep old requests valid. Map the legacy option index to an immutable option ID on the reviewed request. A legacy question remains single-choice unless its new schema explicitly permits multiple selections.

Do not convert a multiple-choice response to only its first option for an older client. Return a complete textual summary and the structured field.

### State, conflict, and delivery rules

| Event | Required behavior |
| --- | --- |
| The user changes a selection | Update the draft only |
| An unrelated event arrives | Preserve draft, focus, cursor, and scroll |
| The user submits twice with the same submission ID and body | Return the original accepted result |
| The same submission ID carries different content | Return a conflict |
| Another person answers first | Preserve the draft and show who answered; do not overwrite |
| The request expires before submission | Reject the answer as expired and retain the draft for a replacement request |
| The question changes materially | Require the new revision; do not apply the old answer to it |
| The network fails before acceptance is known | Retain the draft and reconcile by submission ID before a retry |
| The agent reads the answer | Record retrieval only if that event is measured |
| The user wants to change an accepted answer | Create a linked correction; do not silently rewrite an answer the agent may have acted on |

Drafts must use the user ID, request ID, and revision as their key. Keep a short-lived local copy for reload recovery, with a clear logout cleanup rule. Do not imply cross-device draft sync unless the server implements it.

The [request migration](../../internal/store/migrations/0021_inbox.sql) stores the request document as JSON. This permits additive question and response fields without a database replacement. A unique idempotency lookup may require a small SQLite migration. Inspect and test that change separately.

Notifications must open the full question for free-text and multiple-choice responses. Keep a short, useful notification summary. Do not expose long free-text answers on the lock screen by default.

## 4. Linear-inspired interface

Use Linear as a reference for hierarchy and interaction consistency. Preserve Toolyard's own name and purpose.

Linear's March 2026 refresh uses a quieter sidebar, consistent headers, fewer decorative elements, and softer separators. These principles fit Toolyard's current clutter problem. See the [design explanation](https://linear.app/now/behind-the-latest-design-refresh) and [release note](https://linear.app/changelog/2026-03-12-ui-refresh).

### Navigation and layout

| Region | First-stage design |
| --- | --- |
| Primary sidebar | Inbox, Connections, Activity |
| Secondary sidebar | People & agents, Policies, Settings, subject to existing permissions |
| Inbox filters | Needs you, Questions, Permissions; Snoozed and History as explicit views |
| Queue | Compact rows with title, kind, agent, age, and draft or response state |
| Request detail | Question or permission first; concise task context; evidence; input; explicit action |
| Connections | Existing services first; Add connection opens a focused setup flow |
| Activity | A chronological record with filters; execution and human decisions remain distinct |
| Mobile | One main pane at a time with a reliable Back action |
| Environment | A small persistent Stage label and release revision in About |

Remove the sixteen-link top bar. Remove the separate primary Sessions and Approvals destinations. Hide disabled capabilities. Move raw tool execution, hooks, and diagnostic controls into their relevant advanced views.

For navigation migration, preserve old links and route them to the corresponding new view. Do not remove a caller's API just because its page moves.

### Visual specification

The prototype uses the following proposed values. They are Toolyard design choices, not claimed Linear source tokens.

| Token or component | Proposal |
| --- | --- |
| Main surface | Warm charcoal, #18191b |
| Sidebar | Darker charcoal, #141517 |
| Raised controls | #1e1f22 to #25262a |
| Primary text | #ededf0 |
| Secondary text | #a4a6af |
| Muted text | #8c8f9a |
| Accent | Muted indigo; primary button #686dcc |
| Dividers | Sparse 1px separators, #2b2d32 |
| Corners | 5–7px for controls; no large rounded card around every section |
| Type | 13–14px navigation and dense rows; 14–16px task content; 22–26px detail title |
| Icons | Consistent small outline icons; text labels for actions |
| Layout | Stable sidebar and queue; generous space around the active question |
| Motion | Short state changes; reduced-motion support; no decorative animation |

The prototype's computed text contrast is 15.06:1 for body text, 7.25:1 for secondary text, and 4.52:1 on the primary button. These checks cover those color pairs only. They are not a full accessibility audit.

Keep recommendations visually quiet. Reserve stronger color for the current action and meaningful state. Avoid repeated badges, bright navigation, giant cards, and a large audio player above each question.

Professional behavior matters as much as appearance: stable drafts, exact status text, local errors, predictable keyboard focus, working browser history, and no sudden content jumps.

### Prototype evidence

The [prototype](2026-10-03-stage-ui-prototype.html) demonstrates:

- Multiple selections plus a free-text note.
- A custom text answer without any selection.
- Single-choice radio behavior.
- A standalone free-text question.
- Selection without automatic submission.
- Draft retention when the user changes example requests.
- Separate Inbox, Connections, and Activity views.

Browser probes confirmed these local interactions. A 390px CSS viewport had a 390px page width. A desktop measurement also showed no horizontal overflow. These are browser layout checks, not tests on a physical phone.

The [prototype probe record](evidence/2026-10-03-stage-prototype-probes.json) contains the observed values. The [desktop screenshot](evidence/2026-10-03-stage-prototype-desktop.png) records the design. The prototype does not implement server persistence, authentication, grants, SSE behavior, or agent delivery.

## 5. Stage hosting on this server

### Verified host facts

| Item | Current evidence |
| --- | --- |
| Host | dev-server-1, hostname ip-10-31-39-131 |
| Requested hostname | toolyard.stage.dev.beknown.live |
| DNS | Both the current and requested Toolyard names resolve to 13.232.74.51 |
| Current Toolyard HTTPS | Valid certificate and HTTP 200 health response |
| Stage HTTPS | Certificate verification fails with a self-signed certificate |
| Existing edge | Traefik v3.7.5 owns ports 80 and 443 |
| Existing application | systemd service toolyard-inbox.service |
| Existing private listener | 172.23.0.1:18790 |
| Existing proxy | toolyard-proxy on the bk-dev overlay network |
| Existing TLS pattern | Exact host rule, websecure entrypoint, le certificate resolver |
| Current resource snapshot | About 14GB available memory; about 3GB free; load about 3; swap nearly full |

No route or service changed during these checks. A failed certificate check is not a complete diagnosis of every edge setting. The deployment must verify the new route, certificate, and application independently.

### Proposed topology

    Browser or test agent
        → HTTPS: toolyard.stage.dev.beknown.live
        → existing Traefik edge
        → dedicated stage proxy
        → dedicated Toolyard stage process
        → separate SQLite database, keys, attachments, and test connectors

Use one application instance for this stage. Retain SQLite. Reuse the host's existing route pattern. Do not install another edge proxy or add ClickHouse, Redis, PostgreSQL, or Kubernetes for this milestone.

Proposed deployment values, subject to a final collision check:

| Resource | Proposed value |
| --- | --- |
| Service account and systemd unit | toolyard-stage |
| Release directory | /opt/toolyard-stage/releases/<commit-sha>/ |
| Current release link | /opt/toolyard-stage/current |
| Data directory | /var/lib/toolyard-stage |
| Environment file | /etc/toolyard-stage/stage.env |
| Private listener | 172.23.0.1:18792 |
| Edge proxy and route names | Dedicated toolyard-stage names |
| Public origin | https://toolyard.stage.dev.beknown.live |
| Initial limits | CPU quota of one core; memory high watermark 384MB; memory maximum 768MB |

The port was not present in the listener inventory. These names and limits are proposals, not reserved resources. Recheck them immediately before deployment.

Use a separate Unix account so the stage process cannot read the current service's private data. Restrict its writable paths to the stage directory. Keep test connectors in-process or behind private HTTP endpoints. Preserve the existing no-stdio protection for the public dashboard.

Set the public URL to the exact stage origin. Keep MCP authentication enabled. Use separate stage agent tokens, grant-signing keys, session keys, push keys, and OAuth state.

Do not copy the current database or environment file. Do not mount the current Toolyard data into the stage process. Do not import production connector credentials, real notifications, or organization secrets.

Make memory, notes, skills, and live voice explicitly off. Disable the ClickHouse runtime environment write for stage. Leave paid text-to-speech and model review off for the first trial. Question rendering and validation must work without them.

### Authentication and routing dependencies

Use the existing identity approach with a separate stage configuration. Confirm that the identity provider supports the exact new origin. Keep the initial pilot restricted to its intended testers. Do not broaden an identity allowlist as a side effect of the UI work.

The identity administrator owns any provider-origin or allowlist change. The deployment owner owns the new Traefik service and certificate request. These are concrete setup dependencies, not reasons to postpone source work or the local prototype.

Verify that the stage rejects a token minted for the current dashboard origin where the verifier requires the stage origin. The current session cookie has no Domain attribute, which supports host isolation. Still test CSRF, sibling-origin requests, and the exact public URL.

Register stage-specific OAuth callback URLs only when real OAuth test connectors enter the scope. Register passkeys on the stage origin for the relevant test. Do not assume the current host's passkeys or push subscriptions transfer.

Do not expose an unclaimed first-admin setup page. Establish the intended sign-in path before the route becomes generally reachable.

### Deployment and rollback

Build an immutable artifact from the reviewed revision. Prefer a suitable CI runner for heavier checks. The shared host must not run a repo-wide build or suite.

Record the commit SHA in the binary and About view. Deploy only the stage service. Keep the prior binary and a consistent stage database backup before a migration. If a migration is incompatible with the older binary, rollback requires the matching data snapshot.

Verify health, readiness, sign-in, static assets, SSE, MCP, answer submission, and a fixture permission cycle through the actual HTTPS hostname. A health endpoint alone is insufficient.

The stage pilot uses bounded resources. Load tests belong in a separately sized environment. Host resource measurements are a snapshot, not a capacity guarantee.

## 6. Test connectors and stage data

Create fixtures that exercise the product's difficult states, not just an empty dashboard.

| Fixture | Purpose | External effect |
| --- | --- | --- |
| Read service | Return predictable synthetic records | None |
| Write service | Modify one disposable stage record after a scoped grant | Stage database only |
| Failure service | Return a logical tool error and a transport failure on demand | None |
| Slow service | Exercise deliberate wait and cancellation behavior | None |
| Authorization simulator | Show missing and expired access states | No real OAuth account |

Seed questions of all three types, plus pending, answered, expired, snoozed, and conflicted examples. Seed an approved but unused permission and a failed execution. Include long titles, duplicate-label rejection, Unicode text, and narrow-screen content.

Provide an idempotent fixture seed command. Reset only the dedicated fixture dataset. Keep a reset control out of normal product navigation. A stage label must remain visible in the browser title, app shell, and notification text.

A test connector must not resolve real organization secrets or forward arbitrary URLs. It must clearly identify synthetic results. This protects the meaning of the tests as well as current work.

## 7. What to change first

The first stage release must deliver one complete path: an agent asks, the person answers, the agent retrieves the exact answer, and Activity records the result.

| Order | Work package | Required output | Initial effort |
| --- | --- | --- | --- |
| 1 | Versioned question contract | Text, single-choice, multiple-choice, stable option IDs, structured answer, compatibility | 4–6 engineer-days |
| 2 | UI shell and answer form | Three main destinations, Linear-inspired components, explicit submit, stable drafts, mobile layout | 4–6 engineer-days |
| 3 | Trust and result fixes | Expiry checks, passkey read errors, audit classification and persistence, URL redaction, wait deadline | 4–7 engineer-days |
| 4 | Isolated stage deployment | Separate identity configuration, data, fixture services, route, certificate, limits, release ID | 2–3 engineer-days, excluding external waits |
| 5 | Integrated validation | Real agent round trip, fault cases, mobile and keyboard review, rollback rehearsal | 3–4 engineer-days |

Total initial range: **17–26 engineer-days**. With two engineers and overlapping UI/backend work, plan for roughly **two to four calendar weeks**. This is an estimate for the defined stage scope, not the full earlier roadmap.

Start packages 1 and 2 together. Prepare deployment configuration after the existing route and identity details are confirmed. Do not wait for the full visual refresh to write contract tests.

Trust fixes must pass before the fixture permission path is treated as a valid demonstration. If a durable audit fix expands beyond the estimate, reduce the stage release's claimed capabilities. Do not represent incomplete enforcement as complete.

For this release, defer a separate Sessions page, broad analytics work, a marketplace, new memory features, live voice, team delegation, a general chat client, and multi-host availability.

Keep search simple: query, kind, agent, status, and task. Defer a full saved-view builder until real use demonstrates the need. Keep policy editing in one administrator view rather than scattering it through reports.

## 8. Acceptance examples

| Test | Required result |
| --- | --- |
| Free-text question | The person replies without any option; the agent receives the exact Unicode text |
| Single choice | One selection is possible; selection does not submit; a text-only reply remains valid |
| Multiple choice | Several IDs and optional text survive storage and the agent response unchanged |
| Recommendation | The badge does not preselect or submit an answer |
| Blank reply | The server rejects it and the UI retains the draft |
| Duplicate options | The server rejects duplicate IDs and ambiguous labels |
| Exclusive None | None cannot coexist with another selected option |
| Live update | Twenty unrelated events do not erase text, change selection, or move focus |
| Retry | The same submission produces one accepted answer and one recorded decision |
| Conflict | A second different answer cannot silently replace the first |
| Expiry | An expired pending request cannot accept a late answer or issue a grant |
| Permissions | An ordinary text answer cannot authorize a restricted call |
| Outcome | Logical tool failure is never recorded as success |
| Delivery | Saved and retrieved are separate, evidence-based states |
| Mobile | The main flows work at 390px and on a real phone with its keyboard open |
| Accessibility | Labels, focus, errors, and selection state work with keyboard and screen reader |
| Stage isolation | Current-service credentials and tokens do not authorize stage resources |
| Hostname | Trusted HTTPS, sign-in, SSE, MCP, and fixture calls work through the requested host |
| Rollback | The prior stage artifact and compatible data snapshot can be restored |

The stage contract work touches [tool schemas](../../internal/gateway/inbox_tools.go), [validation](../../internal/inbox/validate.go), [request types](../../internal/inbox/types.go), [decisions and agent responses](../../internal/inbox/service.go), [API routes](../../internal/api/inbox_routes.go), [notifications](../../internal/inbox/attention.go), [service worker](../../web/dashboard/sw.js), [dashboard](../../web/dashboard/app.js), and [styles](../../web/dashboard/style.css). Update the CLI examples, agent protocol, and relevant tests in the same release.

Existing open PRs #9–#11 cover documentation and repository cleanup. Reconcile their work rather than duplicating it. The stage work does not require a rewrite of the gateway or a new frontend framework.

## 9. Status of this discussion

The user selected fresh stage data and test connectors. The revised proposal removes Sessions from primary navigation and includes the requested answer types.

The interactive prototype passed local browser checks for multi-selection, custom text, free text, explicit submission, draft retention across request changes, and empty-answer rejection. These checks validate the prototype only.

The current application remains unchanged. The requested hostname is not a ready stage deployment: HTTPS validation still fails in this review. The next build milestone is the question contract, the focused UI shell, and the isolated stage deployment described above.
