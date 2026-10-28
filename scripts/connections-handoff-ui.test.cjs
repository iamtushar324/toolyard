const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const app = fs.readFileSync(path.join(__dirname, '../web/dashboard/app.js'), 'utf8');
const settings = fs.readFileSync(path.join(__dirname, '../web/dashboard/settings.js'), 'utf8');

// The stop before a sign-in leaves toolyard: Connect on My connections,
// Auth… for a shared account, a marketplace install and Register GitHub
// App first offer "Continue in this browser" or "Copy link for another
// browser". Nothing is requested or opened until the person picks one.

const LINK = 'https://toolyard.example/v1/connect/link/ticket-1';
const AUTHORIZE = 'https://idp.example.com/authorize?state=s1';

function harness(responses = {}) {
  const calls = [];
  const opened = [];
  const context = vm.createContext({
    URLSearchParams, console, crypto: require('node:crypto').webcrypto,
    window: { location: { origin: 'https://toolyard.example' }, addEventListener() {}, open: (...args) => { opened.push(args); } },
    document: { addEventListener() {}, getElementById: () => null, querySelector: () => null, querySelectorAll: () => [] },
    navigator: {}, sessionStorage: { getItem: () => null, setItem() {} },
    localStorage: { getItem: () => null },
    setTimeout: () => 0, clearTimeout() {}, setInterval: () => 0, clearInterval() {},
  });
  vm.runInContext(settings, context);
  vm.runInContext(app.slice(0, app.lastIndexOf('(async () => {')), context);
  context.calls = calls;
  context.responses = responses;
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat(Infinity).filter(x => x != null && x !== false) });
    render = () => {};
    toast = () => {};
    reloadServers = async () => {};
    api = async (path, opts = {}) => {
      calls.push({ path, body: opts.body });
      const r = responses[path];
      return typeof r === 'function' ? r(opts.body || {}) : (r || {});
    };
    state.user = { id: 'u1', role: 'admin', email: 'ada@example.com' };
  `, context);
  return { calls, opened, run: code => vm.runInContext(code, context) };
}

// beginResponder answers a begin route: a link in link mode, else an
// authorize URL.
function beginResponder(body) {
  return body.mode === 'link'
    ? { mode: 'link', connect_link: LINK, expires_in: 600 }
    : { authorize_url: AUTHORIZE, state: 's1', expires_in: 600 };
}

function text(node) {
  if (!node || typeof node !== 'object') return String(node == null ? '' : node);
  return (node.children || []).map(text).join(' ');
}
function nodes(node, tag) {
  if (!node || typeof node !== 'object') return [];
  return [...(node.tag === tag ? [node] : []), ...(node.children || []).flatMap((child) => nodes(child, tag))];
}
function button(tree, label) {
  const b = nodes(tree, 'button').find((n) => text(n).trim() === label);
  assert.ok(b, `no "${label}" button in: ${nodes(tree, 'button').map(text).join(' | ')}`);
  return b;
}
const tick = () => new Promise((resolve) => setImmediate(resolve));

test('Connect stops first, and a copied link starts nothing in this browser', async () => {
  const h = harness({ '/v1/me/connections/linear/begin': beginResponder });
  h.run("connectServer('linear')");
  assert.equal(h.calls.length, 0);
  assert.equal(h.opened.length, 0);
  let tree = h.run('renderHandoffModal()');
  button(tree, 'Continue in this browser');
  await button(tree, 'Copy link for another browser').attrs.on.click();
  assert.deepEqual(h.calls.map((c) => [c.path, c.body.mode]), [['/v1/me/connections/linear/begin', 'link']]);
  assert.equal(h.opened.length, 0);
  tree = h.run('renderHandoffModal()');
  const shown = text(tree);
  assert.ok(shown.includes(LINK));
  assert.match(shown, /expires in 10 minutes/);
  assert.match(shown, /ada@example\.com/);
  assert.match(shown, /another email is refused/);
  // The page waits for the other browser to finish.
  assert.equal(h.run('state.connections.pending'), 'linear');
  assert.equal(h.run('state.connections.pendingLink'), true);
});

test('Continue in this browser keeps the browser-bound flow', async () => {
  const h = harness({ '/v1/me/connections/linear/begin': beginResponder });
  h.run("connectServer('linear')");
  button(h.run('renderHandoffModal()'), 'Continue in this browser').attrs.on.click();
  await tick();
  assert.equal(h.run('state.handoff'), null);
  assert.deepEqual(h.calls.map((c) => [c.path, c.body.mode]), [['/v1/me/connections/linear/begin', undefined]]);
  assert.equal(h.opened.length, 1);
  assert.equal(h.opened[0][0], AUTHORIZE);
  assert.equal(h.run('state.connections.pendingLink'), false);
});

test('Register GitHub App offers a setup link for the other browser', async () => {
  const h = harness({ '/v1/github/setup/begin': () => ({ mode: 'link', connect_link: LINK, expires_in: 600 }) });
  h.run("setupGitHubApp('GitHubForUsers')");
  assert.equal(h.calls.length, 0);
  await button(h.run('renderHandoffModal()'), 'Copy link for another browser').attrs.on.click();
  // Bodies come from the dashboard's realm: compare their fields.
  assert.deepEqual(h.calls.map((c) => [c.path, c.body.server, c.body.mode]), [['/v1/github/setup/begin', 'GitHubForUsers', 'link']]);
  const shown = text(h.run('renderHandoffModal()'));
  assert.ok(shown.includes(LINK));
  assert.match(shown, /admin/);
  assert.match(shown, /GitHub account signed in to that browser/);
});

test('Auth… stops in callback mode; paste and device code start at once', async () => {
  const h = harness({ '/v1/servers/shared/oauth/begin': beginResponder });
  h.run("state.oauthFlow = { name: 'shared', mode: 'callback', state: '', authorize_url: '', paste: '', error: '', pat: '', busy: false }; oauthBeginClick()");
  assert.equal(h.calls.length, 0);
  assert.equal(h.run('state.handoff.kind'), 'shared');
  await button(h.run('renderHandoffModal()'), 'Copy link for another browser').attrs.on.click();
  assert.deepEqual(h.calls.map((c) => [c.path, c.body.mode]), [['/v1/servers/shared/oauth/begin', 'link']]);
  assert.match(text(h.run('renderHandoffModal()')), /sign in to toolyard there as an admin/);
  assert.equal(h.run('state.oauthFlow.viaLink'), true);
  assert.equal(h.opened.length, 0);

  h.run("state.handoff = null; state.oauthFlow.mode = 'paste'; oauthBeginClick()");
  await tick();
  assert.equal(h.run('state.handoff'), null);
  assert.equal(h.calls.at(-1).path, '/v1/servers/shared/oauth/begin');
  assert.equal(h.calls.at(-1).body.mode, 'paste');
  assert.equal(h.opened.length, 1);
});

test('A marketplace install stops before authorizing; Cancel leaves it installed', async () => {
  const h = harness({ '/v1/servers/notion/oauth/begin': beginResponder });
  h.run("state.marketModal = { authMode: 'discover', busy: true, phase: 'form' }");
  await h.run("marketAuthorize({ name: 'Notion', auth: {} }, state.marketModal, 'notion')");
  assert.deepEqual(h.calls.map((c) => c.path), ['/v1/servers/notion/oauth/discover']);
  assert.equal(h.opened.length, 0);
  assert.equal(h.run('state.handoff.server'), 'notion');
  button(h.run('renderHandoffModal()'), 'Cancel').attrs.on.click();
  assert.equal(h.run('state.handoff'), null);
  assert.equal(h.run('state.marketModal'), null);
  assert.equal(h.opened.length, 0);
});

test('Escape closes the stop panel before the dialog under it', () => {
  const h = harness();
  h.run("state.oauthFlow = { name: 'shared', mode: 'callback' }; connectServer('linear')");
  assert.equal(h.run('closeTopmostOverlay()'), true);
  assert.equal(h.run('state.handoff'), null);
  assert.equal(h.run('state.oauthFlow.name'), 'shared');
});
