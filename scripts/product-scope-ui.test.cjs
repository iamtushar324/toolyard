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
  });
  vm.runInContext(settings, context);
  vm.runInContext(app.slice(0, app.lastIndexOf('(async () => {')), context);
  context.requests = requests;
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat(Infinity).filter(x => x != null && x !== false) });
    uiIcon = name => el('span', {}, name);
    render = () => {};
    settingsForm = (page, ...children) => el('form', { page }, ...children);
    api = async path => { requests.push(path); return []; };
    state.user = { id: 'test', role: 'admin' };
    settingsInit();
  `, context);
  return { context, requests, run: code => vm.runInContext(code, context) };
}
function text(node) {
  if (!node || typeof node !== 'object') return String(node || '');
  return (node.children || []).map(text).join(' ');
}
function nodes(node, tag) {
  if (!node || typeof node !== 'object') return [];
  return [ ...(node.tag === tag ? [node] : []), ...(node.children || []).flatMap(child => nodes(child, tag)) ];
}

test('retired bookmarks redirect to core pages, while request links stay exact', () => {
  const h = harness();
  for (const [route, target] of Object.entries({ hooks: 'audit', events: 'audit', memory: 'settings/data', mempalace: 'settings/data', call: 'inbox', sessions: 'inbox', 'inbox/sessions': 'inbox' })) {
    assert.equal(h.run(`workspaceRoute(${JSON.stringify(route)})`), target);
  }
  assert.equal(h.run("workspaceRoute('inbox/rq_example')"), 'inbox/rq_example');
  assert.equal(h.run("workspaceRoute('settings/permissions')"), 'settings/permissions');
});

test('agent setup offers MCP connection and permission rules without session hooks', () => {
  const h = harness();
  const tree = h.run("renderAgentDoneStep({ name: 'Synthetic', snippetTab: 'cli', agent: { token: 'synthetic-token', agent_id: 'ag_test' } })");
  const labels = nodes(tree, 'button').map(text);
  assert.ok(labels.includes('Claude Code CLI'));
  assert.ok(labels.includes('Teach it the rules'));
  assert.doesNotMatch(text(tree), /hooks|Conductor|forwarder|UserPromptSubmit/);
  assert.match(text(tree), /mcp add/);
});

test('Settings retains all categories but has no pricing or memory import surface', () => {
  const h = harness();
  assert.equal(h.run('SETTINGS_PAGES.length'), 7);
  const general = h.run('settingsGeneral()'), data = h.run('settingsData()');
  assert.match(text(general), /Tool visibility/);
  assert.doesNotMatch(text(general), /prices|pricing|spend/i);
  assert.match(text(data), /Activity log/);
  assert.match(text(data), /Approvals/);
  assert.doesNotMatch(text(data), /memory|import/i);
  assert.equal(nodes(data, 'a').length, 4);
});

test('tool activity loads operational metrics without price or cost requests', async () => {
  const h = harness();
  await h.run('loadInsights()');
  assert.ok(h.requests.some(p => p.startsWith('/v1/insights/overview')));
  assert.ok(h.requests.some(p => p.startsWith('/v1/insights/tools')));
  assert.ok(h.requests.some(p => p.startsWith('/v1/insights/agents')));
  assert.ok(h.requests.every(p => !/pricing|cost|hooks|memory|voice/.test(p)));
  const tree = h.run('viewInsights()');
  assert.match(text(tree), /Calls/);
  assert.doesNotMatch(text(tree), /Model prices|Model spend|payload tokens|Request tokens/);
});

test('members can open their owner approvals from desktop and mobile navigation', () => {
  const h = harness();
  h.context.location = { hash: '#agents' };
  h.context.history = { pushState: (_state, _title, hash) => { h.context.location.hash = hash; } };
  h.run("state.user = { id: 'member', role: 'member', email: 'member@example.test' }; state.route = 'agents'");
  const tree = h.run("shell(el('div', {}, 'Member content'))");
  const navigation = nodes(tree, 'nav');
  assert.equal(navigation.length, 2);
  for (const nav of navigation) {
    const approval = nodes(nav, 'button').find(button => text(button).includes('Approvals'));
    assert.ok(approval, 'owner approvals must remain reachable');
    h.run("state.route = 'agents'");
    approval.attrs.on.click();
    assert.equal(h.run('state.route'), 'approvals');
    assert.equal(h.context.location.hash, '#approvals');
  }
  assert.equal(h.run("routeAllowed('settings')"), false);
});
