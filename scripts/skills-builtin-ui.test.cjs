// Skills page checks for the built-in source (web/dashboard/skills.js).
// Run with: node --test scripts/skills-builtin-ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../web/dashboard/skills.js'), 'utf8');

function harness() {
  const requests = [];
  const context = vm.createContext({
    URLSearchParams, URL, console, Date, JSON, Number, Math, Set, Map,
    setTimeout: () => 0,
    location: { origin: 'https://toolyard.example', search: '' },
    state: { user: { id: 'u_1', role: 'admin' } },
    requests,
  });
  vm.runInContext(source, context);
  vm.runInContext(`
    el = (tag, attrs = {}, ...children) => ({ tag, attrs, children: children.flat().filter(x => x != null && x !== false) });
    render = () => {};
    toast = () => {};
    relTime = () => 'just now';
    navigate = (r) => { requests.push({ navigate: r }); };
    copyToButton = () => {};
    confirm = () => true;
    api = async (path, opts) => { requests.push({ path, method: (opts && opts.method) || 'GET', body: opts && opts.body }); return {}; };
  `, context);
  return { context, requests, run: (code) => vm.runInContext(code, context) };
}

function text(node) {
  if (!node || typeof node !== 'object') return String(node == null ? '' : node);
  return (node.children || []).map(text).join(' ');
}

// buttons lists the labels of every button in a rendered tree.
function buttons(node, out = []) {
  if (!node || typeof node !== 'object') return out;
  if (node.tag === 'button') out.push(text(node).trim());
  for (const c of node.children || []) buttons(c, out);
  return out;
}

const builtinSource = {
  id: 'src_toolyard_builtin', slug: 'toolyard', kind: 'builtin', owner: 'toolyard.builtin', repo: 'skills', branch: 'builtin', path: '',
  mode: 'pinned', pinned_sha: '0'.repeat(40), enabled: true, active_version_id: 'skv_1', active_sha: 'a'.repeat(64), skill_count: 8, warning_count: 0,
  visibility: 'everyone', default_runs: 'anywhere', has_token: false, auth: 'none', poll_seconds: 300,
  last_checked_at: '2026-10-26T09:00:00Z', webhook_url_path: '/v1/skills/sources/src_toolyard_builtin/webhook',
};

test('the list marks the built-in source and shows no repository or mode', () => {
  const h = harness();
  h.context.b = builtinSource;
  h.run(`skillsUI.loaded = true; skillsUI.openId = null; skillsUI.sources = [b];`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /Built in/);
  assert.match(page, /built into toolyard/);
  assert.match(page, /with toolyard/);
  assert.doesNotMatch(page, /toolyard\.builtin\/skills@/);
  assert.equal(h.run('skillsBuiltin(b)'), true);
  assert.equal(h.run('skillsBuiltin({ kind: "github" })'), false);
});

test('the built-in source page offers sharing and runs, never repository, sync, webhook or delete', () => {
  const h = harness();
  h.context.b = builtinSource;
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_toolyard_builtin'; skillsUI.detail = { id: 'src_toolyard_builtin', source: b, busy: '', secret: null, error: '',
    versions: [{ id: 'skv_1', sha: 'a'.repeat(64), status: 'active', skill_count: 8, warnings: [], diff: { added: ['toolyard-basics'], changed: [], removed: [] }, created_at: '2026-10-26T09:00:00Z' },
               { id: 'skv_0', sha: 'b'.repeat(64), status: 'retired', skill_count: 7, warnings: [], created_at: '2026-10-20T09:00:00Z' }],
    skills: [{ path: 'toolyard-basics', name: 'toolyard-basics', runs: 'anywhere', reads_30d: 3 }] };`);
  const tree = h.run('viewSkills()');
  const page = text(tree);
  assert.match(page, /Built in/);
  assert.match(page, /ship inside toolyard/);
  assert.match(page, /toolyard-basics/);
  assert.match(page, /No override/, 'the per-skill runs override stays');
  assert.doesNotMatch(page, /github\.com/);
  assert.doesNotMatch(page, /Webhook/);
  const labels = buttons(tree);
  for (const forbidden of ['Delete', 'Sync now', 'Pause', 'Resume', 'Activate', 'Rotate secret', 'Make a secret']) {
    assert.ok(!labels.includes(forbidden), forbidden + ' must not show for the built-in source: ' + labels.join(', '));
  }
  assert.ok(labels.includes('Who sees it'), labels.join(', '));
});

test('editing the built-in source shows only who sees it and where it runs, and sends only those', async () => {
  const h = harness();
  h.context.b = builtinSource;
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_toolyard_builtin'; skillsUI.detail = null; openSkillsEdit(b);`);
  const modal = text(h.run('renderSkillsEdit()'));
  assert.match(modal, /Who sees it/);
  assert.match(modal, /Default runs/);
  for (const field of ['Branch', 'Folder', 'Updates', 'Check every', 'Leave out', 'token']) {
    assert.doesNotMatch(modal, new RegExp(field), field + ' must not show for the built-in source');
  }
  h.run(`skillsUI.edit.draft.default_runs = 'host';`);
  await h.run('saveSkillsEdit()');
  const patch = h.requests.find((r) => r.method === 'PATCH');
  assert.equal(patch.path, '/v1/skills/sources/src_toolyard_builtin');
  assert.deepEqual(JSON.parse(JSON.stringify(patch.body)), { default_runs: 'host' });
});
