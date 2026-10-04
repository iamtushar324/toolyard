// Read-only online prices. Tool payload sizes are never billed model usage.
const pricingUI = { data: null, resultKey: '', error: '', busy: false, loadedAt: 0, query: '', provider: 'openai', offset: 0, revision: 0, timer: null };
function pricingResultKey() { return JSON.stringify([pricingUI.provider, pricingUI.query, pricingUI.offset]); }

function pricingEnsure() {
  if (!pricingUI.busy && !pricingUI.error && (!pricingUI.data || Date.now() - pricingUI.loadedAt > 300000)) pricingLoad();
}
async function pricingLoad() {
  clearTimeout(pricingUI.timer); pricingUI.timer = null;
  const revision = ++pricingUI.revision;
  const resultKey = pricingResultKey();
  pricingUI.busy = true; pricingUI.error = '';
  const query = new URLSearchParams({ provider: pricingUI.provider, q: pricingUI.query, offset: pricingUI.offset, limit: 30 });
  try {
    const data = await api('/v1/insights/pricing?' + query);
    if (revision !== pricingUI.revision) return;
    pricingUI.data = data; pricingUI.resultKey = resultKey; pricingUI.loadedAt = Date.now();
  } catch (e) {
    if (revision !== pricingUI.revision) return;
    pricingUI.error = 'Model prices could not load. ' + e.message;
  } finally {
    if (revision === pricingUI.revision) {
      pricingUI.busy = false;
      if (state.route === 'settings' || state.route === 'insights') render();
    }
  }
}
function pricingFilter(key, value) {
  pricingUI[key] = value; pricingUI.offset = 0;
  // Invalidate an older response before the debounce starts.
  pricingUI.revision++;
  pricingUI.busy = true;
  clearTimeout(pricingUI.timer);
  pricingUI.timer = setTimeout(() => { pricingLoad(); render(); }, key === 'query' ? 250 : 0);
}
function pricingStatus() {
  pricingEnsure();
  const data = pricingUI.data;
  if (pricingUI.error) return el('div', { class: 'settings-notice error', role: 'alert' }, pricingUI.error, ' ',
    el('button', { type: 'button', on: { click: () => { pricingLoad(); render(); } } }, 'Try again'));
  if (!data) return el('p', { class: 'meta', role: 'status' }, 'Model prices will appear after the catalog loads.');
  const label = data.status === 'ready' ? 'Automatic prices' : data.status === 'stale' ? 'Cached prices' : 'Prices unavailable';
  return el('div', { class: 'pricing-status', role: 'status' },
    el('span', { class: 'badge' + (data.status === 'ready' ? ' allowed' : '') }, label),
    data.checked_at ? el('span', { class: 'meta' }, 'Last checked ' + new Date(data.checked_at).toLocaleString()) : null,
    data.status === 'stale' ? el('p', { class: 'meta' }, 'The last valid prices remain available. Toolyard will retry the online source automatically.') : null,
    data.status === 'unavailable' ? el('p', { class: 'meta' }, 'The online catalog is not available yet. Toolyard will retry automatically.') : null);
}
function pricingRate(value) {
  return typeof value === 'number' && Number.isFinite(value) ? '$' + value.toLocaleString('en-US', { maximumFractionDigits: 6 }) : '—';
}
function viewModelPrices() {
  pricingEnsure();
  const data = pricingUI.data || {}, current = pricingUI.resultKey === pricingResultKey();
  const models = current ? (data.models || []) : [], total = current ? (data.total || 0) : 0;
  return el('section', { class: 'card', id: 'model-prices', 'aria-labelledby': 'model-prices-title' },
    el('h2', { id: 'model-prices-title' }, 'Model prices'),
    el('p', { class: 'meta' }, 'Public reference prices in USD per million tokens. Toolyard refreshes the catalog every six hours.'),
    pricingStatus(),
    el('div', { class: 'pricing-filters' },
      el('label', {}, el('span', {}, 'Provider'),
        el('select', { id: 'pricing-provider', value: pricingUI.provider, on: { change: e => pricingFilter('provider', e.target.value) } },
          el('option', { value: '', selected: pricingUI.provider === '' }, 'All providers'),
          ...(data.providers || [{ id: 'openai', name: 'OpenAI' }]).map(p => el('option', { value: p.id, selected: pricingUI.provider === p.id }, p.name)))),
      el('label', {}, el('span', {}, 'Model'),
        el('input', { id: 'pricing-search', type: 'search', value: pricingUI.query, placeholder: 'Search by name or model ID…',
          on: { input: e => pricingFilter('query', e.target.value) } }))),
    el('div', { 'aria-busy': pricingUI.busy },
      pricingUI.busy ? el('p', { class: 'meta', role: 'status' }, 'Please wait for the price results.') : null,
      !models.length ? (pricingUI.busy || pricingUI.error ? null : el('div', { class: 'empty' }, data.status === 'unavailable' || !pricingUI.data ? 'The price catalog is not available yet.' : 'No models match these filters.')) :
        el('div', { class: 'table-scroll' }, el('table', { class: 'pricing-table' },
          el('thead', {}, el('tr', {}, ...['Model', 'Input', 'Output', 'Cache read', 'Cache write'].map(label => el('th', {}, label)))),
          el('tbody', {}, ...models.map(m => el('tr', {},
            el('td', {}, el('strong', {}, m.name), el('div', { class: 'meta' }, m.provider_name), el('code', {}, m.id),
              m.variable_rates ? el('div', { class: 'meta' }, 'Additional rate rules') : null),
            ...['input', 'output', 'cache_read', 'cache_write'].map(key => el('td', {}, pricingRate(m.rates?.[key])))))))),
      el('div', { class: 'pricing-pagination' },
        el('span', { class: 'meta' }, total ? (pricingUI.offset + 1) + '–' + Math.min(pricingUI.offset + models.length, total) + ' of ' + total.toLocaleString() + ' models' : 'No price results'),
        el('div', { class: 'row' },
          el('button', { disabled: pricingUI.busy || pricingUI.offset === 0, on: { click: () => { pricingUI.offset = Math.max(0, pricingUI.offset - 30); pricingLoad(); render(); } } }, 'Previous'),
          el('button', { disabled: pricingUI.busy || pricingUI.offset + models.length >= total, on: { click: () => { pricingUI.offset += 30; pricingLoad(); render(); } } }, 'Next')))),
    el('p', { class: 'meta pricing-source' }, 'Source: ', el('a', { href: 'https://models.dev', target: '_blank', rel: 'noopener noreferrer' }, 'Models.dev'),
      '. A dash means no published rate. Context tiers, region, batch, and service mode can change the provider charge.'));
}
