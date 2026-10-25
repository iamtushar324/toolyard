// Skills page: the Add source wizard's "Use my GitHub connection" (web/dashboard/skills.js).
// Run with: node --test scripts/skills-github-ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../web/dashboard/skills.js'), 'utf8');

function harness(answers) {
  const requests = [];
  const context = vm.createContext({
    URLSearchParams, URL, console, Date, JSON, Number, Math, Set, Map,
    location: { origin: 'https://toolyard.example', search: '' },
    state: { user: { id: 'u_1', role: 'admin' } },
    requests, answers: answers || {},
  });
  vm.runInContext(source, context);
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat(Infinity).filter(x => x != null && x !== false) });
    render = () => {};
    toast = () => {};
    relTime = () => 'just now';
    navigate = () => {};
    copyToButton = () => {};
    api = async (path, opts) => {
      requests.push({ path, method: (opts && opts.method) || 'GET', body: opts && opts.body });
      const a = answers[path];
      if (a && a.__throw) throw a.__throw;
      return a === undefined ? {} : a;
    };
  `, context);
  return { context, requests, run: (code) => vm.runInContext(code, context) };
}

function text(node) {
  if (!node || typeof node !== 'object') return String(node == null ? '' : node);
  return (node.children || []).map(text).join(' ');
}

const connected = [
  { server: 'Linear', transport: 'http', state: 'connected', enabled: true },
  { server: 'GitHubForUsers', transport: 'github', state: 'connected', enabled: true },
];

test('only a connected, enabled GitHub server counts as a GitHub connection', () => {
  const h = harness();
  h.context.list = connected;
  assert.equal(h.run('skillsHasGitHubConnection(list)'), true);
  assert.equal(h.run(`skillsHasGitHubConnection([{ transport: 'github', state: 'needs_signin', enabled: true }])`), false);
  assert.equal(h.run(`skillsHasGitHubConnection([{ transport: 'github', state: 'connected', enabled: false }])`), false);
  assert.equal(h.run(`skillsHasGitHubConnection([{ transport: 'http', state: 'connected', enabled: true }])`), false);
  assert.equal(h.run('skillsHasGitHubConnection({})'), false);
});

test('the wizard offers the GitHub connection only to an admin who has one', async () => {
  const h = harness({ '/v1/me/connections': connected });
  h.run('openSkillsWizard()');
  await h.run('loadSkillsGitHubConnection()');
  assert.equal(h.run('skillsUI.githubConnection'), true);
  h.run(`skillsUI.wizard.auth = 'token'`);
  assert.match(text(h.run('renderSkillsWizard()')), /Use my GitHub connection/);
  h.run(`skillsWizardSet(skillsUI.wizard, 'auth', 'github')`);
  const shown = text(h.run('renderSkillsWizard()'));
  assert.match(shown, /reads this repository with your own toolyard GitHub connection/);
  assert.doesNotMatch(shown, /Read-only token/);

  const none = harness({ '/v1/me/connections': [{ transport: 'github', state: 'needs_signin', enabled: true }] });
  none.run('openSkillsWizard()');
  await none.run('loadSkillsGitHubConnection()');
  none.run(`skillsUI.wizard.auth = 'token'`);
  assert.doesNotMatch(text(none.run('renderSkillsWizard()')), /Use my GitHub connection/);
});

test('inspect and save with the GitHub connection send auth github and no token', async () => {
  const h = harness({
    '/v1/me/connections': connected,
    '/v1/skills/sources/inspect': { owner: 'acme', repo: 'private', default_branch: 'main', branches: ['main'], private: true, trusted_owner: false },
  });
  h.run('openSkillsWizard()');
  await h.run('loadSkillsGitHubConnection()');
  h.run(`skillsUI.wizard.url = 'acme/private'; skillsWizardSet(skillsUI.wizard, 'auth', 'github');`);
  await h.run('skillsWizardInspect()');
  const inspect = h.requests.find((r) => r.path === '/v1/skills/sources/inspect');
  assert.equal(JSON.stringify(inspect.body), JSON.stringify({ url: 'acme/private', auth: 'github' }));
  assert.equal(h.run('skillsUI.wizard.auth'), 'github', 'a private repository keeps the GitHub connection');
  assert.equal(h.run('skillsWizardCanPreview(skillsUI.wizard)'), true);
  const body = h.run('skillsWizardInput(skillsUI.wizard, true)');
  assert.equal(body.auth, 'github');
  assert.equal(body.token, undefined);
});

test('a refused read with the GitHub connection does not switch to a token', async () => {
  const err = Object.assign(new Error('your GitHub connection cannot read acme/private'), { status: 400, body: { code: 'invalid' } });
  const h = harness({ '/v1/me/connections': connected, '/v1/skills/sources/inspect': { __throw: err } });
  h.run('openSkillsWizard()');
  await h.run('loadSkillsGitHubConnection()');
  h.run(`skillsUI.wizard.url = 'acme/private'; skillsWizardSet(skillsUI.wizard, 'auth', 'github');`);
  await h.run('skillsWizardInspect()');
  assert.equal(h.run('skillsUI.wizard.auth'), 'github');
  assert.match(h.run('skillsUI.wizard.error'), /GitHub connection cannot read/);

  // Without a GitHub auth choice, a private repository still asks for a token and names both ways.
  const needs = Object.assign(new Error('private'), { status: 422, body: { code: 'needs_token' } });
  const t = harness({ '/v1/me/connections': connected, '/v1/skills/sources/inspect': { __throw: needs } });
  t.run('openSkillsWizard()');
  await t.run('loadSkillsGitHubConnection()');
  t.run(`skillsUI.wizard.url = 'acme/private';`);
  await t.run('skillsWizardInspect()');
  assert.equal(t.run('skillsUI.wizard.auth'), 'token');
  assert.match(t.run('skillsUI.wizard.error'), /use your GitHub connection/);
});
