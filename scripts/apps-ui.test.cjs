// Connected apps page checks. Run with: node --test scripts/apps-ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../web/dashboard/apps.js'), 'utf8');

function harness() {
  const requests = [];
  const context = vm.createContext({
    URLSearchParams, console,
    location: { origin: 'https://toolyard.example', search: '' },
    state: { user: { id: 'u_1', role: 'member' }, agents: [], auditFilter: {} },
    requests,
  });
  vm.runInContext(source, context);
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat(Infinity).filter(x => x != null && x !== false) });
    render = () => {};
    toast = () => {};
    relTime = () => 'just now';
    isAdmin = () => state.user.role === 'admin';
    copyToButton = () => {};
    api = async (path, opts) => { requests.push({ path, body: opts && opts.body }); return {}; };
  `, context);
  return { context, requests, run: code => vm.runInContext(code, context) };
}

function text(node) {
  if (!node || typeof node !== 'object') return String(node == null ? '' : node);
  return (node.children || []).map(text).join(' ');
}

test('a scope round-trips through the editor draft', () => {
  const h = harness();
  const scope = {
    mode: 'listed',
    allow_servers: ['memory'], allow_tools: [{ server: 'Linear', tool: 'Linear_get_issue' }],
    deny_servers: ['events'], deny_tools: [{ server: 'memory', tool: 'memory.delete' }],
  };
  h.context.scope = scope;
  const back = h.run('appsScopeFromDraft(appsDraftFromScope(scope))');
  assert.deepEqual(JSON.parse(JSON.stringify(back)), scope);
});

test('allows are dropped in "everything" mode, blocks are kept', () => {
  const h = harness();
  const out = h.run(`(() => {
    const d = appsDraftFromScope(null);
    d.allowServers.add('memory');
    d.denyTools.add('fixture\\u0000fixture.echo');
    return appsScopeFromDraft(d);
  })()`);
  assert.equal(out.mode, 'all');
  assert.equal(out.allow_servers.length, 0);
  assert.deepEqual(JSON.parse(JSON.stringify(out.deny_tools)), [{ server: 'fixture', tool: 'fixture.echo' }]);
});

test('the summary says what a scope allows in plain words', () => {
  const h = harness();
  assert.equal(h.run('appsScopeSummary({ mode: "all" })'), 'Everything its owner can use');
  assert.equal(h.run('appsScopeSummary({ mode: "listed", allow_servers: ["a"], allow_tools: [] })'), 'Only 1 server and 0 tools');
  assert.equal(h.run('appsScopeSummary({ mode: "all", deny_tools: [{ server: "a", tool: "b" }] })'),
    'Everything its owner can use, except 0 blocked servers and 1 blocked tool');
});

test('after a password sign-in, only a well-formed request id returns to the consent screen', () => {
  const h = harness();
  const id = 'A'.repeat(43);
  const loc = { search: '?password=1&oauth_request=' + id, replaced: '', replace(u) { this.replaced = u; } };
  h.context.loc = loc;
  assert.equal(h.run('appsResumeOAuth(loc)'), true);
  assert.equal(loc.replaced, '/oauth/consent?request=' + id);
  const bad = { search: '?oauth_request=https://evil.example/', replaced: '', replace(u) { this.replaced = u; } };
  h.context.bad = bad;
  assert.equal(h.run('appsResumeOAuth(bad)'), false);
  assert.equal(bad.replaced, '');
});

test('the page lists an app with its nickname, client, access and actions', () => {
  const h = harness();
  h.run(`appsUI.loaded = true; appsUI.config = { enabled: true, public_url: true, mcp_url: 'https://toolyard.example/mcp' };
    appsUI.apps = [{ id: 'grt_1', agent_id: 'ag_1', nickname: 'My ChatGPT', client_name: 'ChatGPT', client_kind: 'dcr',
      client_id: 'mcpc_x', redirect_hosts: ['chatgpt.com'], status: 'active', created_at: 1, last_used_at: 0,
      scope: { mode: 'listed', allow_servers: ['memory'], allow_tools: [], deny_servers: [], deny_tools: [] },
      scope_summary: 'only 1 server and 0 tools' }];`);
  const page = text(h.run('viewApps()'));
  assert.match(page, /https:\/\/toolyard\.example\/mcp/);
  assert.match(page, /My ChatGPT/);
  assert.match(page, /ChatGPT · Self-registered · returns to chatgpt\.com/);
  assert.match(page, /only 1 server and 0 tools/);
  for (const label of ['Rename', 'Change access', 'Activity', 'Revoke']) assert.match(page, new RegExp(label));
});

test('saving the editor posts the app scope', async () => {
  const h = harness();
  h.run(`appsUI.editor = { appId: 'grt_1', name: 'x', servers: [], error: '', saving: false, open: {},
    draft: appsDraftFromScope({ mode: 'listed', allow_servers: ['memory'] }) };`);
  await h.run('saveScopeEditor()');
  const post = h.requests.find(r => r.path === '/v1/me/apps/scope');
  assert.ok(post, 'no scope request');
  assert.equal(post.body.id, 'grt_1');
  assert.deepEqual(JSON.parse(JSON.stringify(post.body.scope.allow_servers)), ['memory']);
  assert.equal(h.run('appsUI.editor'), null);
});
