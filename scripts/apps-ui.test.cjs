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
    URLSearchParams, URL, console,
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
    tool_list: 'full',
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
  assert.equal(out.tool_list, 'small', 'the small tool list is the default');
  assert.deepEqual(JSON.parse(JSON.stringify(out.deny_tools)), [{ server: 'fixture', tool: 'fixture.echo' }]);
});

test('the summary says what a scope allows in plain words', () => {
  const h = harness();
  assert.equal(h.run('appsScopeSummary({ mode: "all" })'), 'Everything its owner can use');
  assert.equal(h.run('appsScopeSummary({ mode: "listed", allow_servers: ["a"], allow_tools: [] })'), 'Only 1 server and 0 tools');
  assert.equal(h.run('appsScopeSummary({ mode: "all", deny_tools: [{ server: "a", tool: "b" }] })'),
    'Everything its owner can use, except 0 blocked servers and 1 blocked tool');
  assert.equal(h.run('appsScopeSummary({ mode: "all", tool_list: "full" })'), 'Everything its owner can use, full tool list');
  assert.equal(h.run('appsScopeSummary({ mode: "all", tool_list: "small" })'), 'Everything its owner can use');
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
  assert.match(page, /ChatGPT \(name not checked\) · Self-registered · returns to chatgpt\.com/);
  assert.match(page, /only 1 server and 0 tools/);
  for (const label of ['Rename', 'Change access', 'Activity', 'Revoke']) assert.match(page, new RegExp(label));
});

test('a metadata document client is labelled by its host, never as verified', () => {
  const h = harness();
  assert.equal(h.run(`appsKindLabel({ client_kind: 'cimd', client_id: 'https://apps.example.com/client.json' })`), 'Identified by apps.example.com');
  assert.equal(h.run(`appsKindLabel({ client_kind: 'dcr', client_id: 'mcpc_x' })`), 'Self-registered');
  assert.doesNotMatch(h.run(`appsKindLabel({ client_kind: 'cimd', client_id: 'https://x.example/c' })`), /[Vv]erified/);
});

test('only the owner, an admin, sends may_manage: true', async () => {
  const h = harness();
  h.run(`appsUI.editor = { appId: 'grt_1', name: 'x', servers: [], error: '', saving: false, open: {},
    draft: Object.assign(appsDraftFromScope(null), { mayManage: true }) };`);
  await h.run('saveScopeEditor()');
  let post = h.requests.find(r => r.path === '/v1/me/apps/scope');
  assert.equal(Object.hasOwn(post.body, 'may_manage'), false, 'a member must not send may_manage: true');
  h.requests.length = 0;
  // An admin may not turn it on for another person's app.
  h.run(`state.user.role = 'admin'; appsUI.editor = { appId: 'grt_1', ownerId: 'u_other', name: 'x', servers: [], error: '', saving: false, open: {},
    draft: Object.assign(appsDraftFromScope(null), { mayManage: true }) };`);
  await h.run('saveScopeEditor()');
  post = h.requests.find(r => r.path === '/v1/me/apps/scope');
  assert.equal(Object.hasOwn(post.body, 'may_manage'), false, "an admin must not send may_manage: true for another person's app");
  h.requests.length = 0;
  h.run(`appsUI.editor = { appId: 'grt_1', ownerId: 'u_1', name: 'x', servers: [], error: '', saving: false, open: {},
    draft: Object.assign(appsDraftFromScope(null), { mayManage: true }) };`);
  await h.run('saveScopeEditor()');
  post = h.requests.find(r => r.path === '/v1/me/apps/scope');
  assert.equal(post.body.may_manage, true);
});

test('the flag is shown as without effect when the owner is not an admin', () => {
  const h = harness();
  h.run(`appsUI.loaded = true; appsUI.config = { enabled: true, mcp_url: 'https://toolyard.example/mcp' };
    appsUI.apps = [{ id: 'grt_1', agent_id: 'ag_1', nickname: 'App', client_name: 'X', client_kind: 'dcr', client_id: 'mcpc_x',
      redirect_hosts: [], status: 'active', created_at: 1, may_manage: true, owner_is_admin: false, scope: { mode: 'all' } }];`);
  assert.match(text(h.run('viewApps()')), /no effect: the owner is not an admin/);
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

test('the editor offers the tool list, small by default, and saves the choice', async () => {
  const h = harness();
  h.run(`appsUI.editor = { agentId: 'ag_1', name: 'bot', servers: [], error: '', saving: false, open: {},
    draft: appsDraftFromScope({ mode: 'all' }) };`);
  const radios = [];
  const walk = (n) => {
    if (!n || typeof n !== 'object') return;
    if (n.tag === 'input' && n.attrs.name === 'apps-tool-list') radios.push(n);
    (n.children || []).forEach(walk);
  };
  const modal = h.run('renderScopeEditor()');
  walk(modal);
  const page = text(modal);
  assert.match(page, /Tool list/);
  assert.match(page, /Small: 5 tools \(search, docs, run code, inbox\)/);
  assert.match(page, /Full: every tool it may use/);
  assert.deepEqual(radios.map(r => [r.attrs.value, r.attrs.checked]), [['small', true], ['full', false]]);
  radios[1].attrs.on.change();
  assert.equal(h.run('appsUI.editor.draft.toolList'), 'full');
  await h.run('saveScopeEditor()');
  const post = h.requests.find(r => r.path === '/v1/me/agent-scopes');
  assert.ok(post, 'no scope request');
  assert.equal(post.body.agent_id, 'ag_1');
  assert.equal(post.body.scope.tool_list, 'full');
});
