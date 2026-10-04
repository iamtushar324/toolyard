# Settings and workspace UI stage release

## Delivered

- Stage URL: https://toolyard.stage.dev.beknown.live/?build=af4c912#settings/general
- Deployed version: `stage-af4c912`.
- Source: `af4c912aa7c4aa9d3a7b95814aa015457ddc46a8`, branch `t3code/jakarta`, pushed to origin.
- Release directory: `/opt/toolyard-stage/releases/af4c912`.
- Backup before release: `/var/lib/toolyard-stage/backups/before-af4c912.sqlite`.
- Original release before this turn: `/opt/toolyard-stage/releases/9eac6b2`.

The workspace now uses common navigation icons, page headers, section tabs,
form controls, dialog styles, table regions, and a matching sign-in theme.
Settings has seven focused categories instead of a collection of collapsed cards.
Preference edits have explicit Save and Discard controls. Non-secret drafts
survive navigation, live renders, failed saves, and reloads in the same tab.
Credential values stay only in memory. Logout clears the settings drafts.

Tools now separates Catalog, Policies, and Approval queue. Search retains focus,
tool rows are keyboard buttons, and Run is disabled while a call is in progress.
Connection panels retain their open state after a render. Advanced notification
controls and the Beknown identity card no longer dominate the primary views.
Secrets use password fields in the add and replacement forms.

## Measured checks

The native T3 browser was authenticated on stage. The old Settings implementation
lost an unsaved hosting note and collapsed its panel after `render()`. This was
the reproduction before the fix.

The actual stage forms passed these checks:

- A preference draft and text selection survived a render.
- The same draft survived category navigation and a full page reload.
- A simulated network error retained the draft and previous server value.
- A subsequent real save persisted the exact value through `/v1/settings`.
- The test restored the prior hosting note through the same API.
- A partial quiet-hours range produced an error without a server change.
- Discard cleared the unsaved range.
- Secret form values survived a render and category navigation, without storage.
- The replacement dialog retained its password field and cleared it on Escape.
- Tool search retained focus and produced keyboard-accessible filtered rows.
- Run was disabled during a simulated in-progress call, without executing a tool.
- A connection management panel remained open after a render.
- The agent dialog exposed its dialog role, kept focus inside, wrapped Tab, and closed on Escape.

All seven settings sections and the main routes passed the 394-pixel check.
A same-origin frame with the actual app assets passed the 320-pixel checks.
A similar frame verified the 1440-pixel layout: a 224-pixel application sidebar,
198-pixel settings navigation, and a content column up to 780 pixels.
The frame history functions were stubbed only in the test frame because its URL
was `about:srcdoc`. The product history functions were unchanged by the test.
All test frames were removed.

Automated checks passed:

- `node --test scripts/settings-ui.test.cjs`: six behavior tests.
- `GOMAXPROCS=2 go test -p 1 ./cmd/gateway -run '^TestWorkspaceAssets$' -count=1`.
- JavaScript syntax, manifest JSON, SVG parsing, and `git diff --check`.
- A two-worker target build of `cmd/gateway`.
- HTTPS health and fingerprinted JS/CSS delivery for the app and sign-in page.

Final health reported `ok: true`, `upstreams_live: 2`, `metrics_dropped: 0`,
and `version: stage-af4c912`. Stage PID was 3640628. The fixture PID remained
1367099. The live service PID remained 1446651 throughout this turn.
Only the stage gateway was restarted. The database was not reset.

## Limits

Native browser screenshots failed repeatedly. The review used actual browser
interactions, page text, computed styles, and layout geometry. Screenshot-based
visual review and physical-device passkey enrollment remain unverified.
The responsive frame checks do not replace physical-device tests.

This release is published on the source branch. No pull request exists because
the GitHub CLI actor lacked repository collaborator rights in the prior turn.
There is no linked Linear ticket. The session stays open for stage feedback.
