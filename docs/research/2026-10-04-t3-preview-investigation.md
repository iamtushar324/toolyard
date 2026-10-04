# T3 preview investigation

User authorized a bounded read-only T3 connection investigation on 2026-10-04. The Toolyard stage goal remains unchanged. No T3 repository changes, session creation, cookie copy, host focus fabrication, role changes, or service restart.

## Measured

- Native preview automation responds on client preview-client-A. Direct stage tab is tab_8, profile default, measured1280x800. Auth check returned401, and DOM contains the Google sign-in form.
- Native registry now has tab_f, a Browser Use live viewer, and tab_8, the direct Toolyard page. These are distinct tabs. The viewer points to the cloud session previously tested; its credential URL is deliberately omitted.
- The separately authorized Browser Use login returned200 for the test member account, role member, and settings403. This does not authenticate the native T3 browser.
- Two native snapshot attempts failed with PreviewAutomationExecutionError, failureCount1, for the same client. Evaluation works. Journal confirms the snapshot failure but records only operation, errorTag, and failureCount. The underlying desktop cause is unavailable.
- T3 bkmain service runs from /home/ubuntu/repos/t3code-bkmain. Checked-out source commit f21dad17f. Source contents cannot prove the installed desktop client version.

## Source-supported conclusions

PreviewAutomationBroker keys assignments by environmentId plus providerSessionId. A live client stream pins the provider session to that runtime. Focus changes do not reassign it. Disconnection removes assignments for that exact connection. Tests explicitly cover pinning despite later focus changes and reject silent runtime migration. Closing ordinary web T3 tabs does not guarantee that an Electron automation stream disconnects.

PreviewAutomationHosts returns null outside Electron; ordinary web presence clients are not native preview automation hosts. Presence client IDs and automation IDs are separate and cannot be equated. Prior suggestions to close web tabs did not establish a fix for the native stream.

Electron BrowserSession derives persistent local partitions independently of Browser Use Cloud profiles. The cloud viewer tab does not transfer authentication into the direct native Toolyard tab. Login mismatch is measured; an incorrect physical runtime assignment is still a hypothesis because the live tool surface does not expose all broker clients or allow a scoped assignment reset.

Snapshot capture uses Runtime, Accessibility, Electron page capture, and diagnostic history together. A generic execution error does not identify which part failed. A successful evaluate is not evidence that screenshot or AX capture works.

## Concrete recovery proposal

Open this session in the intended T3 desktop app, fully quit and reopen that desktop app so the old stream disconnects, then select the direct Toolyard stage tab (not the Browser Use viewer) and use an existing stage Admin account. Recheck auth200 and roleadmin before UI review. Recheck snapshot separately. The agent cannot quit the operator desktop remotely through the available native tools. No shared server restart is justified.

If the intended direct tab already shows an Admin dashboard and automation still gets401 after reconnect, the fault requires desktop runtime/connection diagnostics. The minimal product fix would expose the assigned automation host and an explicit, audited per-session reconnect operation. It must retain runtime pinning during multi-step actions; do not globally route every call to whichever app recently gained focus. This is a proposal, not an implemented or tested fix.

## Reported reconnect verification (08:24 UTC)

The operator reported a reconnect and direct Admin dashboard. The native tool still reports tab_8, hidden, and /v1/auth/me401. A fresh snapshot failure still names exactly preview-client-A. The page user agent identifies MacIntel and BKT3Code/0.0.41-production-nightly.20260927.1, Electron44.1.0, Chrome152.0.7977.65. This proves the runtime reached by automation is the production-branded desktop app. It does not prove which app the operator reopened. The app branding source defines BK T3 Code and Stage BK T3 Code as separate applications with separate user-data directories and update channels. A diagnostic question now asks which application holds the signed-in dashboard. No repeated sign-in request, no repair or server restart.

## Same-app answer and provider recovery proposal (08:26 UTC)

The operator confirms BK T3 Code contains the signed-in Admin dashboard, so different brand is not the explanation. Native status remains the existing tab_8 and1280x800 runtime. No tool can list broker hosts or reset only the assignment.

Source recovery proof: ProviderService.prepareMcpSession issues a credential on provider start; McpSessionRegistry.issue creates a new random providerSessionId; PreviewAutomationBroker assignments are keyed by that ID plus environment ID. Stopping the current provider and starting it with the next user message therefore creates a new assignment key. This does not prove the next host selection will find the operator dashboard.

The restart action by itself merely calls ensureSessionForThread and may reuse a live session. The bounded recovery is a stop of this exact session provider, then an explicit user Continue message. A pending gate requests that stop and includes the resumption message. No lifecycle mutation has occurred. Existing thread/history/worktree remain. No other session or T3 service restart is proposed.
