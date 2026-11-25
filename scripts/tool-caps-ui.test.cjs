const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const app = fs.readFileSync(path.join(__dirname, '../web/dashboard/app.js'), 'utf8');
const settings = fs.readFileSync(path.join(__dirname, '../web/dashboard/settings.js'), 'utf8');

function harness() {
  const requests = [];
  const context = vm.createContext({
    URLSearchParams, console, crypto: require('node:crypto').webcrypto,
    window: { location: { origin: 'https://stage.example' }, addEventListener() {} },
    document: { addEventListener() {}, getElementById: () => null, querySelector: () => null },
    navigator: {}, sessionStorage: { getItem: () => null, setItem() {} },
    localStorage: { getItem: () => null },
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0,
    confirm: () => true,
  });
  vm.runInContext(settings, context);
  vm.runInContext(app.slice(0, app.lastIndexOf('(async () => {')), context);
  context.requests = requests;
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat(Infinity).filter(x => x != null && x !== false) });
    uiIcon = name => el('span', {}, name);
    render = () => {};
    toast = () => {};
    api = async (path, opts) => {
      requests.push({ path, opts });
      if (path === '/v1/tool-caps' && !opts) return { day: '2026-11-25', reset_at: '2026-11-26T00:00:00Z', caps: [
        { upstream: 'bk', tool: 'bk.get_rank', per_caller_daily: 5, total_daily: 0, note: 'credits', used_today_total: 7,
          used_today: [{ caller: 'user:u1', username: 'ada', calls: 4 }, { caller: 'user:u2', username: 'guest1', guest: true, calls: 3 }] },
      ] };
      return {};
    };
    state.user = { id: 'test', role: 'admin' };
    state.tools = [{ name: 'bk.get_rank', upstream: 'bk' }, { name: 'bk.get_other', upstream: 'bk' }];
  `, context);
  return { context, requests, run: code => vm.runInContext(code, context) };
}
function text(node) {
  if (!node || typeof node !== 'object') return String(node || '');
  return (node.children || []).map(text).join(' ');
}

test('Daily caps card lists caps with today\'s use and top callers', async () => {
  const h = harness();
  await h.run('loadToolCaps()');
  const card = text(h.run('renderToolCapsCard()'));
  assert.match(card, /Daily caps/);
  assert.match(card, /bk\.get_rank/);
  assert.match(card, /no cap/);
  assert.match(card, /ada: 4/);
  assert.match(card, /guest1 \(guest\): 3/);
  assert.match(card, /00:00 UTC/);
});

test('saving and removing a cap call the admin routes', async () => {
  const h = harness();
  h.run("state.toolCaps.form = { tool: 'bk.get_other', per: '3', total: '', note: 'x' }");
  await h.run('saveToolCap()');
  const post = h.requests.find(r => r.path === '/v1/tool-caps' && r.opts && r.opts.method === 'POST');
  assert.ok(post, 'POST /v1/tool-caps sent');
  assert.equal(post.opts.body.tool, 'bk.get_other');
  assert.equal(post.opts.body.per_caller_daily, 3);
  assert.equal(post.opts.body.total_daily, 0);
  await h.run("removeToolCap('bk.get_rank')");
  assert.ok(h.requests.some(r => r.path === '/v1/tool-caps/bk.get_rank' && r.opts && r.opts.method === 'DELETE'));
});

test('the Policies page shows the Daily caps card', () => {
  const h = harness();
  h.run("state.insights = state.insights || {}; state.insights.tools = state.insights.tools || []");
  assert.match(text(h.run('viewPolicies()')), /Daily caps/);
});
