const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, '../web/dashboard/pricing.js'), 'utf8');

function harness(api) {
  const context = vm.createContext({ api, state: { route: 'insights' }, render() {}, URLSearchParams, setTimeout, clearTimeout });
  vm.runInContext(source, context);
  return vm.runInContext('({ pricingUI, pricingLoad, pricingRate, pricingResultKey })', context);
}

test('a late result cannot replace prices for the selected provider', async () => {
  const pending = [];
  const h = harness(path => new Promise(resolve => pending.push({ path, resolve })));
  const first = h.pricingLoad();
  h.pricingUI.provider = 'anthropic';
  const second = h.pricingLoad();
  pending[1].resolve({ source: 'Models.dev', models: [{ provider_id: 'anthropic' }] }); await second;
  pending[0].resolve({ source: 'Models.dev', models: [{ provider_id: 'openai' }] }); await first;
  assert.equal(h.pricingUI.data.models[0].provider_id, 'anthropic');
  assert.equal(h.pricingUI.resultKey, h.pricingResultKey());
  assert.equal(h.pricingUI.busy, false);
});

test('a failed request retains valid data and exposes a retry state', async () => {
  let offline = false;
  const h = harness(async () => {
    if (offline) throw new Error('offline');
    return { models: [{ provider_id: 'openai' }] };
  });
  await h.pricingLoad(); offline = true; await h.pricingLoad();
  assert.equal(h.pricingUI.data.models.length, 1);
  assert.match(h.pricingUI.error, /offline/);
  assert.equal(h.pricingUI.busy, false);
});

test('unknown prices stay distinct from published zero prices', () => {
  const h = harness(async () => ({}));
  assert.equal(h.pricingRate(null), '—');
  assert.equal(h.pricingRate(undefined), '—');
  assert.equal(h.pricingRate(0), '$0');
  assert.equal(h.pricingRate(0.125), '$0.125');
});
