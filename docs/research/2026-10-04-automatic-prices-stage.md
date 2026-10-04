# Automatic model prices — stage-a9aec1f

The stage release is live at https://toolyard.stage.dev.beknown.live/.
Source commit `a9aec1f` is pushed on `t3code/jakarta`.
Release: `/opt/toolyard-stage/releases/a9aec1f`.
Backup: `/var/lib/toolyard-stage/backups/before-a9aec1f.sqlite`.
Rollback release: `/opt/toolyard-stage/releases/af4c912`.
Rollback record: `/etc/toolyard-stage/before-a9aec1f.json`.

Manual price fields are removed. The API hides legacy keys and rejects manual
price writes atomically. Existing values stay on disk for rollback. Old price
drafts are removed from browser state. The Usage page has a read-only catalog
with exact provider filters, model search, pagination, input and output rates,
cache read and write rates, source attribution, and freshness.

The server retrieves Models.dev every six hours with conditional ETag requests,
a timeout, and a size bound. It validates prices before an atomic disk cache
replacement. A failed refresh retains the valid cache and retries after 15
minutes. Missing prices remain null. Additional rate rules receive a label.
Late UI responses cannot overwrite a newer provider selection.

Settings tabs hide their scrollbar and retain horizontal swipe access. A
category change exposes the active tab. A render preserves horizontal position
when the selected category stays the same.

## Metering boundary

Toolyard records MCP request and result byte sizes, not the client model or
provider token counts. The byte estimate cannot measure billed model spend.
The old cost multiplication was removed. Spend remains unavailable rather than
an invented zero. No provider usage integration or spend limit enforcement was
introduced. A real spend integration must account for model, provider, input,
output, cache categories, context tiers, and billing mode.

## Primary research

- [Models.dev API](https://models.dev/).
- [Models.dev source and rate units](https://github.com/anomalyco/models.dev).
- [OpenAI cache usage](https://developers.openai.com/api/docs/guides/prompt-caching).
- [Claude pricing](https://platform.claude.com/docs/en/about-claude/pricing).
- [OpenRouter models API](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties).

Models.dev provides provider-specific reference rates in USD per million
input, output, cache read, and cache write tokens. The provider documentation
shows why cache, region, batch, and service mode must be accounted for before
an estimate can represent spend. OpenRouter prices apply to that route.

## Measured checks

Four catalog checks passed, including an opt-in live source check. Three API
checks passed for authentication, unknown spend, and atomic manual-price
rejection. Existing operator tests passed with the new route. Ten Node tests
passed for drafts, validation, retries, missing prices, and late responses.
Embedded asset, JavaScript syntax, diff, and target build checks passed.

Fourteen HTTPS checks passed against the deployed stage. The valid catalog
contains 8,190 text models from 226 providers. Its cache resides at
`/var/lib/toolyard-stage/model-prices.json`. Health reports two test connectors
and zero dropped metrics. Only the stage gateway restarted. Stage PID: 4007978.
The live PID remained 1446651. The fixture PID remained 1367099.
Stage data was not reset. Details are in `evidence/2026-10-04-online-price-checks.json`.

## Browser check

The native preview changed from a signed-in 394-pixel browser to a signed-out
1280-pixel browser during release. The new module URLs resolve correctly.
The Google sign-in page is open. An authenticated browser check waits for the
human to sign in. No browser substitution or login bypass was used.
Full authenticated UI verification remains pending.

No Linear ticket is linked. No PR was created. The source branch is published.
The prior turn measured that the GitHub actor lacks collaborator rights to
create a PR. This report was initially local. The user authorized publication with the laptop-agent handoff on 2026-10-04.

### Preview follow-up

The human reported a signed-in T3 preview. Native automation still returned
HTTP 401 for `/v1/auth/me` and displayed the Clerk sign-in form after a refresh.
The native preview registry listed one thread tab, `tab_8`, with profile
`default`. Automation reported that browser as hidden.

Read-only inspection of the T3 preview broker source showed that a provider
session retains its browser runtime assignment while that connection is live.
This can explain separate cookie states across T3 app instances, but the actual
client mismatch remains an inference. No browser identity, cookie, or broker
state was changed. The human was asked to keep the signed-in T3 instance open
and close other instances before a retry. The deployed stage remains healthy.

## Browser Use verification (07:49 UTC)

User explicitly requested Browser Use and the bk-docs procedure. Used only the canonical tools/browser.sh Cloud wrapper. The operator signed in. Auth returned 200 for the test member account, role member. GET /v1/settings returned 403. No role changes or credentials injected. Member agents, available services, and connections pages loaded at measured width1536 without body overflow. Viewport commands did not persist across wrapper calls; all measured widths stayed1536, so mobile checks are unverified. Initial wrong route probes redirected to agents and are not counted as service/connection checks. Correct route checks are in evidence/2026-10-04-browser-use-member-checks.json. A pending human gate offers stage-only Admin grant for this exact account, existing-admin sign-in, or member-only review. Pricing and settings UI checks remain blocked by that choice.
