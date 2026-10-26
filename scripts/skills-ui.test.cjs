// Skills page checks (web/dashboard/skills.js). Run with: node --test scripts/skills-ui.test.cjs
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
    // Timers never fire here: the first-sync polls wait forever.
    setTimeout: () => 0,
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
    navigate = (r) => { requests.push({ navigate: r }); };
    copyToButton = () => {};
    confirm = () => true;
    prompt = () => '';
    api = async (path, opts) => {
      requests.push({ path, method: (opts && opts.method) || 'GET', body: opts && opts.body });
      const a = answers[path];
      // An Error from this file is not instanceof the context's Error.
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

function plain(v) { return JSON.parse(JSON.stringify(v)); }

const source1 = {
  id: 'src_1', slug: 'team-skills', owner: 'acme', repo: 'skills', branch: 'main', path: 'skills', mode: 'review',
  enabled: true, active_version_id: 'skv_1', active_sha: '0123456789abcdef0123456789abcdef01234567', skill_count: 12,
  warning_count: 2, visibility: 'everyone', default_runs: 'anywhere', has_token: true, auth: 'token',
  last_checked_at: '2026-10-25T09:00:00Z', webhook_url_path: '/v1/skills/sources/src_1/webhook', poll_seconds: 300,
};

test('the status chip says ok, pending review, error, paused or syncing', () => {
  const h = harness();
  h.context.s = source1;
  assert.equal(h.run('skillsStatus(s)'), 'ok');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { pending_version_id: "skv_2" }))'), 'pending review');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { last_error: "GitHub said 404" }))'), 'error');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { enabled: false, last_error: "x" }))'), 'paused');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { active_version_id: "" }))'), 'syncing');
  assert.equal(h.run('skillsStatus(s, true)'), 'syncing');
  assert.equal(h.run('skillsStatusClass("pending review")'), 'pending');
  assert.equal(h.run('skillsStatusClass("error")'), 'denied');
});

test('inspect picks the URL branch or the default branch, the folder, and the default mode', () => {
  const h = harness();
  const w = h.run(`(() => { const w = skillsWizardNew(); w.url = 'acme/skills';
    return skillsWizardAfterInspect(w, { owner: 'acme', repo: 'skills', default_branch: 'main', branches: ['main', 'dev'], trusted_owner: true }); })()`);
  assert.equal(w.step, 'repo');
  assert.equal(w.branch, 'main');
  assert.equal(w.path, '');
  assert.equal(w.mode, 'auto');
  assert.equal(w.auth, 'none');
  const fromURL = h.run(`skillsWizardAfterInspect(skillsWizardNew(), { owner: 'o', repo: 'r', default_branch: 'main', branch: 'dev', path: 'skills/team',
    branches: ['main', 'dev'], private: true, trusted_owner: false })`);
  assert.equal(fromURL.branch, 'dev');
  assert.equal(fromURL.path, 'skills/team');
  assert.equal(fromURL.mode, 'review');
  assert.equal(fromURL.auth, 'token', 'a private repository asks for a token');
});

test('changing the branch, folder or token after a preview drops the preview', () => {
  const h = harness();
  const out = h.run(`(() => {
    const w = skillsWizardAfterInspect(skillsWizardNew(), { owner: 'o', repo: 'r', default_branch: 'main', branches: ['main', 'dev'] });
    w.preview = { sha: 'abc' }; w.step = 'review';
    skillsWizardSet(w, 'mode', 'pinned');
    const kept = !!w.preview && w.step === 'review';
    skillsWizardSet(w, 'branch', 'dev');
    return { kept, preview: w.preview, step: w.step };
  })()`);
  assert.equal(out.kept, true, 'the mode is a setting, not part of the preview');
  assert.equal(out.preview, null);
  assert.equal(out.step, 'repo');
});

test('the branch list filters and keeps the chosen branch first', () => {
  const h = harness();
  const list = h.run(`(() => {
    const w = skillsWizardAfterInspect(skillsWizardNew(), { owner: 'o', repo: 'r', default_branch: 'main',
      branches: ['main', 'release/1', 'release/2', 'feature/x'] });
    w.branchFilter = 'release';
    return skillsWizardBranches(w);
  })()`);
  assert.deepEqual(plain(list), ['main', 'release/1', 'release/2']);
});

test('the token is sent only with token auth, and the settings only on save', () => {
  const h = harness();
  const out = h.run(`(() => {
    const w = skillsWizardAfterInspect(skillsWizardNew(), { owner: 'o', repo: 'r', default_branch: 'main', branches: ['main'], private: true });
    w.url = ' https://github.com/o/r '; w.path = '/skills/'; w.token = ' tok ';
    const preview = skillsWizardInput(w, false);
    w.visibility = 'members'; w.members = ['u_2']; w.defaultRuns = 'host';
    const save = skillsWizardInput(w, true);
    w.auth = 'none';
    const pub = skillsWizardInput(w, true);
    return { preview, save, pub };
  })()`);
  assert.deepEqual(plain(out.preview), { url: 'https://github.com/o/r', branch: 'main', path: 'skills', auth: 'token', token: 'tok' });
  assert.equal(out.save.mode, 'review');
  assert.equal(out.save.visibility, 'members');
  assert.deepEqual(plain(out.save.members), ['u_2']);
  assert.equal(out.save.default_runs, 'host');
  assert.equal(Object.hasOwn(out.pub, 'token'), false);
  assert.equal(Object.hasOwn(out.pub, 'auth'), false);
});

test('each step can go on only when it has what it needs', () => {
  const h = harness();
  const out = h.run(`(() => {
    const w = skillsWizardNew();
    const r = { inspectEmpty: skillsWizardCanInspect(w) };
    w.url = 'o/r'; r.inspect = skillsWizardCanInspect(w);
    skillsWizardAfterInspect(w, { owner: 'o', repo: 'r', default_branch: 'main', branches: ['main'], needs_token: true });
    r.previewNoToken = skillsWizardCanPreview(w);
    w.token = 'x'; r.preview = skillsWizardCanPreview(w);
    r.saveNoPreview = skillsWizardCanSave(w);
    w.preview = { sha: 'a' }; w.visibility = 'members';
    r.saveNoMembers = skillsWizardCanSave(w);
    w.members = ['u_2']; r.save = skillsWizardCanSave(w);
    return r;
  })()`);
  assert.deepEqual(plain(out), { inspectEmpty: false, inspect: true, previewNoToken: false, preview: true, saveNoPreview: false, saveNoMembers: false, save: true });
});

test('the wizard walks inspect, preview and save, then forgets the token', async () => {
  const h = harness({
    '/v1/skills/sources/inspect': { owner: 'acme', repo: 'skills', default_branch: 'main', branches: ['main'], private: true, trusted_owner: false },
    '/v1/skills/sources/preview': { sha: '0123456789abcdef', skill_count: 3, file_count: 9, warnings: [], sample: [], source: { owner: 'acme', repo: 'skills' } },
    '/v1/skills/sources': { source: { id: 'src_9', slug: 'skills', enabled: true }, status: 'syncing' },
    '/v1/skills/admin/sources': { sources: [] },
  });
  h.run(`openSkillsWizard(); skillsUI.wizard.url = 'acme/skills';`);
  await h.run('skillsWizardInspect()');
  assert.equal(h.run('skillsUI.wizard.step'), 'repo');
  assert.equal(h.run('skillsUI.wizard.auth'), 'token');
  h.run(`skillsWizardSet(skillsUI.wizard, 'token', 'read-only-token')`);
  await h.run('skillsWizardPreview()');
  assert.equal(h.run('skillsUI.wizard.step'), 'review');
  await h.run('skillsWizardSave()');
  assert.equal(h.run('skillsUI.wizard.step'), 'done');
  assert.equal(h.run('skillsUI.wizard.token'), '');
  const save = h.requests.find((r) => r.path === '/v1/skills/sources' && r.method === 'POST');
  assert.equal(save.body.token, 'read-only-token');
  assert.equal(save.body.mode, 'review');
  // The first sync runs in the background; the wizard says so, then shows
  // the result once the polled source has its first check.
  assert.match(text(h.run('renderSkillsWizard()')), /The first sync runs now/);
  h.run(`skillsUI.wizard.result.source = Object.assign({}, skillsUI.wizard.result.source,
    { last_checked_at: '2026-10-25T09:00:00Z', active_sha: '0123456789abcdef', skill_count: 3 })`);
  assert.match(text(h.run('renderSkillsWizard()')), /First sync: commit 0123456789ab is live, with 3 skills\./);
});

test('the first sync text covers review, errors and a retried create', () => {
  const h = harness();
  assert.match(h.run(`skillsFirstSyncText({ last_checked_at: 'x', pending_version_id: 'skv_1' })`), /waits for review/);
  assert.match(h.run(`skillsFirstSyncText({ last_checked_at: 'x', last_error: 'GitHub said 404' })`), /failed: GitHub said 404/);
  h.run(`skillsUI.wizard = skillsWizardNew(); skillsUI.wizard.step = 'done';
    skillsUI.wizard.result = { existing: true, status: 'ok', source: { id: 'src_1', slug: 'team', last_checked_at: 'x', active_sha: 'abc' } };`);
  assert.match(text(h.run('renderSkillsWizard()')), /already added a moment ago/);
});

test('the repository link is built from owner and repo, never the API URL', () => {
  const h = harness();
  h.run(`skillsUI.wizard = skillsWizardAfterInspect(skillsWizardNew(), { owner: 'acme', repo: 'skills', default_branch: 'main',
    branches: ['main'], html_url: 'javascript:alert(1)' });`);
  const view = h.run('renderSkillsWizardRepo(skillsUI.wizard)');
  const links = [];
  const walk = (n) => { if (n && typeof n === 'object') { if (n.tag === 'a') links.push(n.attrs.href); (n.children || []).forEach(walk); } };
  walk(view);
  assert.ok(links.includes('https://github.com/acme/skills'), JSON.stringify(links));
  assert.ok(!links.some((l) => String(l).startsWith('javascript:')));
});

test('a private repository found at inspect asks for a token', async () => {
  const err = Object.assign(new Error('the repository is private or missing'), { status: 422, body: { code: 'needs_token' } });
  const h = harness({ '/v1/skills/sources/inspect': { __throw: err } });
  h.run(`openSkillsWizard(); skillsUI.wizard.url = 'acme/private';`);
  await h.run('skillsWizardInspect()');
  assert.equal(h.run('skillsUI.wizard.step'), 'url');
  assert.equal(h.run('skillsUI.wizard.auth'), 'token');
  assert.match(h.run('skillsUI.wizard.error'), /read-only token/);
});

test('warnings are grouped by code, the largest group first', () => {
  const h = harness();
  const groups = h.run(`skillsGroupWarnings([{ code: 'binary', path: 'a' }, { code: 'secret', path: 'b' }, { code: 'secret', path: 'c' }])`);
  assert.deepEqual(plain(groups).map((g) => [g.code, g.items.length]), [['secret', 2], ['binary', 1]]);
});

test('the edit form sends only what changed, and a token only when typed', () => {
  const h = harness();
  h.context.orig = source1;
  const out = h.run(`(() => {
    openSkillsEdit(orig);
    const d = skillsUI.edit.draft;
    const unchanged = skillsEditPatch(orig, d);
    d.mode = 'auto'; d.exclude = 'drafts/**\\n';
    const changed = skillsEditPatch(orig, d);
    d.token = ' new-token ';
    const withToken = skillsEditPatch(orig, d);
    d.token = ''; d.clear_token = true;
    const cleared = skillsEditPatch(orig, d);
    return { unchanged, changed, withToken, cleared };
  })()`);
  assert.deepEqual(plain(out.unchanged), {});
  assert.deepEqual(plain(out.changed), { mode: 'auto', exclude: ['drafts/**'] });
  assert.equal(out.withToken.token, 'new-token');
  assert.equal(out.withToken.auth, 'token');
  assert.equal(out.cleared.clear_token, true);
  assert.equal(Object.hasOwn(out.cleared, 'token'), false);
});

test('the route opens a source page, and a bad id stays on the list', () => {
  const h = harness();
  assert.equal(h.run(`skillsRoute('skills/src_1')`), 'skills');
  assert.equal(h.run('skillsUI.openId'), 'src_1');
  h.run(`skillsRoute('skills/../x')`);
  assert.equal(h.run('skillsUI.openId'), null);
  h.run(`skillsRoute('skills')`);
  assert.equal(h.run('skillsUI.openId'), null);
});

test('the list shows each source with its repository, commit, mode and status', () => {
  const h = harness();
  h.context.s = source1;
  h.run(`skillsUI.loaded = true; skillsUI.sources = [s, Object.assign({}, s, { id: 'src_2', slug: 'paused-one', enabled: false })];`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /team-skills/);
  assert.match(page, /acme\/skills@main · skills/);
  assert.match(page, /0123456789ab/);
  assert.match(page, /review/);
  assert.match(page, /\bok\b/);
  assert.match(page, /paused/);
  assert.match(page, /Add source/);
});

test('the source page has review actions, rollback, runs overrides and the webhook steps', () => {
  const h = harness();
  h.context.s = Object.assign({}, source1, { pending_version_id: 'skv_3' });
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_1'; skillsUI.detail = { id: 'src_1', source: s, busy: '', secret: null, error: '',
    versions: [
      { id: 'skv_3', sha: 'cccccccccccccccc', status: 'staged', skill_count: 13, warnings: [{ code: 'secret', path: 'skills/x/.env', severity: 'skip', message: 'looks like a secret' }],
        diff: { added: ['new-skill'], changed: [], removed: [] }, created_at: '2026-10-25T09:00:00Z' },
      { id: 'skv_1', sha: '0123456789abcdef', status: 'active', skill_count: 12, warnings: [], created_at: '2026-10-24T09:00:00Z' },
      { id: 'skv_0', sha: 'aaaaaaaaaaaaaaaa', status: 'retired', skill_count: 11, warnings: [], created_at: '2026-10-23T09:00:00Z' },
    ],
    skills: [{ path: 'pdf', name: 'pdf', runs: 'anywhere', reads_30d: 7 }] };`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /Waiting for review/);
  assert.match(page, /Added \(1\):\s+new-skill/);
  assert.match(page, /Approve/);
  assert.match(page, /Reject/);
  assert.match(page, /Activate/, 'a retired version can be made live again');
  assert.match(page, /Warnings in cccccccccccc/);
  assert.match(page, /No override/);
  assert.match(page, /https:\/\/toolyard\.example\/v1\/skills\/sources\/src_1\/webhook/);
  assert.match(page, /application\/json/);
  assert.match(page, /Just the push event/);
  assert.match(page, /Rotate secret|Make a secret/);
  assert.match(page, /read-only token \(stored sealed\)/);
});

test('Approve on a staged version calls activate, Reject sends the note', async () => {
  const h = harness({ '/v1/skills/admin/sources': { sources: [] } });
  h.context.s = source1;
  h.run(`skillsUI.openId = null; skillsUI.detail = null;`);
  await h.run(`activateSkillVersion(s, { id: 'skv_3', sha: 'cccccccccccc' }, true)`);
  assert.ok(h.requests.find((r) => r.path === '/v1/skills/sources/src_1/versions/skv_3/activate' && r.method === 'POST'));
  h.run(`prompt = () => 'not this week'`);
  await h.run(`rejectSkillVersion(s, { id: 'skv_4', sha: 'dddddddddddd' })`);
  const rej = h.requests.find((r) => r.path === '/v1/skills/sources/src_1/versions/skv_4/reject');
  assert.equal(rej.body.note, 'not this week');
  await h.run(`setSkillRuns(s, 'pdf', 'host')`);
  const runs = h.requests.find((r) => r.path === '/v1/skills/sources/src_1/skills/runs');
  assert.equal(runs.method, 'PUT');
  assert.deepEqual(plain(runs.body), { path: 'pdf', runs: 'host' });
});

test('a rotated secret is shown once on the page', async () => {
  const h = harness({ '/v1/skills/sources/src_1/webhook-secret': { secret: 'whsec-shown-once', payload_url: 'https://toolyard.example/v1/skills/sources/src_1/webhook' } });
  h.context.s = source1;
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_1'; skillsUI.detail = { id: 'src_1', source: s, versions: [], skills: [], busy: '', secret: null, error: '' };`);
  await h.run('rotateSkillSecret(s)');
  assert.match(text(h.run('viewSkills()')), /whsec-shown-once/);
  h.run('skillsUI.detail.secret = null');
  assert.doesNotMatch(text(h.run('viewSkills()')), /whsec-shown-once/);
});

test('the sync summary says what happened', () => {
  const h = harness();
  assert.equal(h.run(`skillsSyncSummary({ unchanged: true, sha: '0123456789abcdef' })`), 'No new commit; 0123456789ab is current.');
  assert.equal(h.run(`skillsSyncSummary({ staged: true, sha: '0123456789abcdef' })`), 'Commit 0123456789ab waits for review.');
  assert.equal(h.run(`skillsSyncSummary({ error: 'GitHub is down' })`), 'Sync failed: GitHub is down');
});

test('only the pending version can be approved; rejected versions are not offered for rollback', () => {
  const h = harness();
  h.context.s = Object.assign({}, source1, { pending_version_id: 'skv_new' });
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_1'; skillsUI.detail = { id: 'src_1', source: s, busy: '', secret: null, error: '',
    versions: [
      { id: 'skv_new', sha: 'eeeeeeeeeeeeeeee', status: 'staged', skill_count: 3, warnings: [], diff: { added: ['a'] }, created_at: '2026-10-25T10:00:00Z' },
      { id: 'skv_old', sha: 'dddddddddddddddd', status: 'staged', skill_count: 3, warnings: [], diff: { added: ['b'] }, created_at: '2026-10-25T09:00:00Z' },
      { id: 'skv_rej', sha: 'bbbbbbbbbbbbbbbb', status: 'rejected', skill_count: 3, warnings: [], created_at: '2026-10-24T09:00:00Z' },
    ], skills: [] };`);
  const versions = h.run(`renderSkillsVersions(s, skillsUI.detail.versions, false)`);
  const rows = [];
  const walk = (n) => { if (n && typeof n === 'object') { if (n.tag === 'tr') rows.push(n); (n.children || []).forEach(walk); } };
  walk(versions);
  const byCommit = (sha) => text(rows.find((r) => text(r).includes(sha.slice(0, 12))));
  assert.match(byCommit('eeeeeeeeeeee'), /Approve/);
  assert.doesNotMatch(byCommit('dddddddddddd'), /Approve|Reject/);
  assert.match(byCommit('dddddddddddd'), /superseded/);
  assert.doesNotMatch(byCommit('bbbbbbbbbbbb'), /Activate/);
  const review = text(h.run('viewSkills()'));
  assert.match(review, /Waiting for review/);
  assert.match(review, /eeeeeeeeeeee/);
  // The review card lists only the pending version.
  const card = [];
  const find = (n) => { if (n && typeof n === 'object') { if (n.attrs && n.attrs.class === 'card skills-review') card.push(n); (n.children || []).forEach(find); } };
  find(h.run('viewSkills()'));
  assert.equal(card.length, 1);
  assert.doesNotMatch(text(card[0]), /dddddddddddd/);
});

test('a GitHub rate limit names when it ends and the way out', () => {
  const h = harness();
  h.context.err = { message: 'x', body: { code: 'rate_limited', reset_at: '2026-10-25T21:27:50Z', retry_after: 60 } };
  const msg = h.run('skillsErrorText(err)');
  assert.match(msg, /^GitHub rate limit reached until \d\d:\d\d — add a GitHub token in GitHub access\.$/);
  h.context.other = { message: 'branch nope does not exist', body: { code: 'invalid' } };
  assert.equal(h.run('skillsErrorText(other)'), 'branch nope does not exist');
  h.context.s = Object.assign({}, source1, { last_error: 'github: rate limited until 2026-10-25T21:27:50Z' });
  assert.equal(h.run('skillsStatus(s)'), 'rate limited');
  assert.equal(h.run('skillsStatusClass("rate limited")'), 'pending');
});

test('the GitHub access panel shows the token state and the rate limit, never a token', () => {
  const h = harness();
  h.run(`skillsUI.access.data = { set: false, rate: { limit: 60, remaining: 0, reset: '2026-10-25T21:27:50Z' },
    paused_until: new Date(Date.now() + 600000).toISOString() };`);
  let panel = text(h.run('renderSkillsAccess()'));
  assert.match(panel, /no token: 60 calls an hour/);
  assert.match(panel, /0 of 60 calls left/);
  assert.match(panel, /rate limit reached until/);
  assert.match(panel, /Set token/);
  assert.match(panel, /fine-grained token/);
  h.run(`skillsUI.access.data = { set: true, rate: { limit: 5000, remaining: 4990, reset: '2026-10-25T21:27:50Z' } };`);
  panel = text(h.run('renderSkillsAccess()'));
  assert.match(panel, /default token set/);
  assert.match(panel, /Replace token/);
  assert.match(panel, /Clear/);
  assert.doesNotMatch(panel, /rate limit reached/);
});

test('saving the default token PUTs it once and the browser forgets it', async () => {
  const h = harness({ '/v1/skills/admin/github-token': { set: true, rate: { limit: 5000, remaining: 4999 } } });
  h.run(`skillsUI.access.editing = true; skillsUI.access.token = 'github_pat_x';`);
  await h.run(`saveSkillsAccess(skillsUI.access.token)`);
  const put = h.requests.find((r) => r.path === '/v1/skills/admin/github-token' && r.method === 'PUT');
  assert.equal(put.body.token, 'github_pat_x');
  assert.equal(h.run('skillsUI.access.token'), '');
  assert.equal(h.run('skillsUI.access.editing'), false);
  assert.equal(h.run('skillsUI.access.data.set'), true);
  // Clearing asks first and sends an empty token.
  await h.run(`saveSkillsAccess('')`);
  const clear = h.requests.filter((r) => r.method === 'PUT').pop();
  assert.equal(clear.body.token, '');
});

test('typing in a wizard field enables the next button without a re-render', () => {
  const h = harness();
  const out = h.run(`(() => {
    const button = { disabled: true };
    document = { getElementById: (id) => (id === 'skills-wiz-next' ? button : null) };
    const w = skillsWizardNew();
    skillsWizardRefreshNext(w);
    const empty = button.disabled;
    w.url = 'owner/repo';
    skillsWizardRefreshNext(w);
    return { empty, typed: button.disabled };
  })()`);
  assert.deepEqual(plain(out), { empty: true, typed: false });
});

// ---- sharing controls and skill contributors (TEC-1782 F2) -------------------

test('a contributor source shows pending approval, a rejected one rejected', () => {
  const h = harness();
  h.context.s = source1;
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { approval: "pending" }))'), 'pending approval');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { approval: "pending", enabled: false }))'), 'paused');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { approval: "rejected", enabled: false }))'), 'rejected');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { approval: "approved" }))'), 'ok');
  assert.equal(h.run('skillsStatus(Object.assign({}, s, { approval: "pending" }), true)'), 'syncing');
  assert.equal(h.run('skillsStatusClass("pending approval")'), 'pending');
  assert.equal(h.run('skillsStatusClass("rejected")'), 'denied');
});

test('the Shared with label names each mode and the block list', () => {
  const h = harness();
  assert.equal(h.run(`skillsSharingLabel({ path: 'pdf' })`), 'Same as source');
  assert.equal(h.run(`skillsSharingLabel({ sharing: { mode: '', blocked: ['u_2', 'u_3'] } })`), 'Same as source · 2 blocked');
  assert.equal(h.run(`skillsSharingLabel({ sharing: { mode: 'everyone' } })`), 'Everyone');
  assert.equal(h.run(`skillsSharingLabel({ sharing: { mode: 'members', members: ['u_2', 'u_3'] } })`), '2 members');
  assert.equal(h.run(`skillsSharingLabel({ sharing: { mode: 'members', members: ['u_2'], blocked: ['u_4'] } })`), '1 member · 1 blocked');
  assert.equal(h.run(`skillsSharingLabel({ sharing: { mode: 'nobody' } })`), 'Nobody (admins only)');
});

test('a skill sharing body sends mode "" to follow the source, and members only with mode members', () => {
  const h = harness();
  const out = h.run(`({
    inherit: skillsSharingBody({ mode: '', members: ['u_9'], blocked: ['u_3', 'u_2'] }, 'pdf'),
    inheritWord: skillsSharingBody({ mode: 'inherit', members: [], blocked: [] }, 'pdf'),
    members: skillsSharingBody({ mode: 'members', members: ['u_3', 'u_2'], blocked: [] }, 'team/x'),
    nobody: skillsSharingBody({ mode: 'nobody', members: ['u_2'], blocked: ['u_5'] }, 'pdf'),
  })`);
  assert.deepEqual(plain(out.inherit), { path: 'pdf', mode: '', blocked: ['u_3', 'u_2'] });
  assert.deepEqual(plain(out.inheritWord), { path: 'pdf', mode: '', blocked: [] });
  assert.deepEqual(plain(out.members), { path: 'team/x', mode: 'members', members: ['u_2', 'u_3'], blocked: [] });
  assert.deepEqual(plain(out.nobody), { path: 'pdf', mode: 'nobody', blocked: ['u_5'] });
});

test('the facts say who sees a source and where its approval stands', () => {
  const h = harness();
  assert.equal(h.run(`skillsAudienceText({ visibility: 'everyone', blocked: ['u_2', 'u_3'] })`), 'everyone · 2 blocked');
  assert.equal(h.run(`skillsAudienceText({ visibility: 'members', members: ['u_2'] })`), '1 chosen member');
  assert.match(h.run(`skillsAudienceText({ visibility: 'nobody' })`), /^nobody \(only admins and the person who added it/);
  assert.equal(h.run(`skillsAudienceText({})`), 'everyone');
  h.run(`skillsUI.users = [{ id: 'u_1', username: 'ada', role: 'admin' }]`);
  assert.equal(h.run(`skillsApprovalText({ approval: 'pending' })`), 'waits for an admin');
  assert.equal(h.run(`skillsApprovalText({ approval: 'approved', approval_by: 'u_1' })`), 'approved by ada');
  assert.equal(h.run(`skillsApprovalText({ approval: 'approved', approval_by: 'u_9' })`), 'approved by u_9');
  assert.equal(h.run(`skillsApprovalText({ approval: 'rejected', approval_note: 'not ours' })`), 'rejected: not ours');
  assert.equal(h.run(`skillsApprovalText({})`), '');
});

test('a contributor chooses review, or automatic for a trusted owner, never pinned', () => {
  const h = harness();
  const out = h.run(`(() => {
    const values = (list) => list.map(([v]) => v);
    const untrusted = { info: { trusted_owner: false } }, trusted = { info: { trusted_owner: true } };
    return {
      admin: values(skillsWizardModes(untrusted, false)),
      untrusted: values(skillsWizardModes(untrusted, true)),
      trusted: values(skillsWizardModes(trusted, true)),
      editAdmin: values(skillsEditModes({ mode: 'review' }, false)),
      editReview: values(skillsEditModes({ mode: 'review' }, true)),
      editAuto: values(skillsEditModes({ mode: 'auto' }, true)),
    };
  })()`);
  assert.deepEqual(plain(out), { admin: ['auto', 'review', 'pinned'], untrusted: ['review'], trusted: ['auto', 'review'],
    editAdmin: ['auto', 'review', 'pinned'], editReview: ['review'], editAuto: ['auto', 'review'] });
  // The review step a contributor sees: no pinned or automatic mode for an
  // untrusted owner, and a "nobody yet" audience that sends no members.
  h.context.state.user.role = 'member';
  const step = h.run(`(() => {
    const w = skillsWizardAfterInspect(skillsWizardNew(), { owner: 'o', repo: 'r', default_branch: 'main', branches: ['main'], trusted_owner: false });
    w.url = 'o/r'; w.preview = { sha: 'abc', skill_count: 1, file_count: 1, warnings: [], sample: [] }; w.step = 'review';
    const view = renderSkillsWizardReview(w);
    w.visibility = 'nobody'; w.members = ['u_2'];
    return { view, input: skillsWizardInput(w, true) };
  })()`);
  const review = text(step.view);
  assert.doesNotMatch(review, /Pinned/);
  assert.doesNotMatch(review, /Automatic/);
  assert.match(review, /Nobody yet \(only admins and me, in the dashboard\)/);
  assert.match(review, /an admin approves each new commit/);
  assert.equal(step.input.mode, 'review');
  assert.equal(step.input.visibility, 'nobody');
  assert.equal(Object.hasOwn(step.input, 'members'), false);
});

test('the source Sharing panel sends only what changed, and members only with the members audience', () => {
  const h = harness();
  h.context.orig = Object.assign({}, source1, { visibility: 'members', members: ['u_2'] });
  const out = h.run(`(() => {
    const d = skillsSharingDraft(orig);
    const r = { none: skillsSourceSharingPatch(orig, d) };
    d.blocked = ['u_4']; r.block = skillsSourceSharingPatch(orig, d);
    d.blocked = []; d.members = ['u_3', 'u_2']; r.members = skillsSourceSharingPatch(orig, d);
    d.visibility = 'everyone'; r.everyone = skillsSourceSharingPatch(orig, d);
    d.visibility = 'nobody'; d.blocked = ['u_5']; r.nobody = skillsSourceSharingPatch(orig, d);
    return r;
  })()`);
  assert.deepEqual(plain(out.none), {});
  assert.deepEqual(plain(out.block), { blocked: ['u_4'] });
  assert.deepEqual(plain(out.members), { members: ['u_2', 'u_3'] });
  assert.deepEqual(plain(out.everyone), { visibility: 'everyone' });
  assert.deepEqual(plain(out.nobody), { visibility: 'nobody', blocked: ['u_5'] });
  // Becoming members sends the members, even when the stored list is the same.
  h.context.pub = Object.assign({}, source1, { members: ['u_2'] });
  assert.deepEqual(plain(h.run(`skillsSourceSharingPatch(pub, Object.assign(skillsSharingDraft(pub), { visibility: 'members' }))`)),
    { visibility: 'members', members: ['u_2'] });
  // The Edit modal sends nobody without members as well.
  const edit = h.run(`(() => { openSkillsEdit(orig); const d = skillsUI.edit.draft; d.visibility = 'nobody'; return skillsEditPatch(orig, d); })()`);
  assert.deepEqual(plain(edit), { visibility: 'nobody' });
});

test('the source page Sharing panel saves the audience and block list with one PATCH', async () => {
  const h = harness({ '/v1/skills/admin/sources': { sources: [] } });
  h.context.s = source1;
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_1';
    skillsUI.users = [{ id: 'u_2', username: 'bea', role: 'member' }, { id: 'u_1', username: 'ada', role: 'admin' }];
    skillsUI.detail = { id: 'src_1', source: s, versions: [], skills: [], busy: '', secret: null, error: '' };`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /Sharing/);
  assert.match(page, /Block list/);
  assert.match(page, /Nobody: only admins and the person who added it, in the dashboard; agents never/);
  assert.match(page, /admin: sees every skill anyway/);
  h.run(`skillsUI.detail.sharing.visibility = 'nobody'; skillsUI.detail.sharing.blocked = ['u_2'];`);
  h.run('viewSkills()');
  assert.equal(h.run('skillsUI.detail.sharing.visibility'), 'nobody', 'a render keeps the unsaved draft');
  await h.run('saveSkillsSharing(s)');
  const patch = h.requests.find((r) => r.path === '/v1/skills/sources/src_1' && r.method === 'PATCH');
  assert.deepEqual(plain(patch.body), { visibility: 'nobody', blocked: ['u_2'] });
});

test('the skills table says who sees each skill, and its modal saves one PUT', async () => {
  const h = harness();
  h.context.s = source1;
  h.run(`skillsUI.loaded = true; skillsUI.openId = 'src_1'; skillsUI.users = [{ id: 'u_2', username: 'bea', role: 'member' }];
    skillsUI.detail = { id: 'src_1', source: s, versions: [], busy: '', secret: null, error: '',
      skills: [{ path: 'pdf', name: 'pdf', runs: 'anywhere', reads_30d: 7, sharing: { mode: 'members', members: ['u_2'], blocked: ['u_3'] } },
        { path: 'xlsx', name: 'xlsx', runs: 'anywhere', reads_30d: 0 }] };`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /Shared with/);
  assert.match(page, /1 member · 1 blocked/);
  assert.match(page, /Same as source/);
  h.run(`openSkillsShare(s, skillsUI.detail.skills[0])`);
  assert.deepEqual(plain(h.run('skillsUI.share.draft')), { mode: 'members', members: ['u_2'], blocked: ['u_3'], memberFilter: '', blockFilter: '' });
  assert.match(text(h.run('viewSkills()')), /Who sees pdf/);
  h.run(`skillsUI.share.draft.mode = 'nobody';`);
  await h.run('saveSkillsShare()');
  const put = h.requests.find((r) => r.path === '/v1/skills/sources/src_1/skills/sharing');
  assert.equal(put.method, 'PUT');
  assert.deepEqual(plain(put.body), { path: 'pdf', mode: 'nobody', blocked: ['u_3'] });
  assert.equal(h.run('skillsUI.share'), null);
});

test('only an admin sees the Skill contributors panel; Add and Remove call the contributors routes', async () => {
  const h = harness();
  h.run(`skillsUI.loaded = true; skillsUI.sources = [];
    skillsUI.users = [{ id: 'u_1', username: 'ada', role: 'admin' }, { id: 'u_2', username: 'bea', role: 'member' }, { id: 'u_3', username: 'cal', role: 'member' }];
    skillsUI.contributors = [{ user_id: 'u_2', added_by: 'u_1', added_at: '2026-10-25T09:00:00Z' }];`);
  const page = text(h.run('viewSkills()'));
  assert.match(page, /Skill contributors/);
  assert.match(page, /Members you name here can add skill sources/);
  assert.match(page, /bea/);
  assert.match(page, /added just now by ada/);
  const options = [];
  const walk = (n) => { if (n && typeof n === 'object') { if (n.tag === 'option') options.push(n.attrs.value); (n.children || []).forEach(walk); } };
  walk(h.run('renderSkillsContributors()'));
  assert.deepEqual(options, ['', 'u_3'], 'only members who are not contributors yet');
  await h.run(`addSkillsContributor('u_3')`);
  const put = h.requests.find((r) => r.path === '/v1/skills/admin/contributors/u_3');
  assert.equal(put.method, 'PUT');
  assert.ok(h.requests.find((r) => r.path === '/v1/skills/admin/contributors' && r.method === 'GET'), 'the list reloads');
  await h.run(`removeSkillsContributor('u_2')`);
  assert.ok(h.requests.find((r) => r.path === '/v1/skills/admin/contributors/u_2' && r.method === 'DELETE'));
  // A contributor (a member) gets their own list, without the panel.
  h.run(`state.user.role = 'member'; skillsUI.contributor = true; skillsUI.contributors = [{ user_id: 'u_2' }];`);
  const mine = text(h.run('viewSkills()'));
  assert.doesNotMatch(mine, /Skill contributors/);
  assert.match(mine, /Your skill sources/);
  assert.match(mine, /A new source waits for an admin's approval/);
});

test('an admin approves or rejects a contributor source; reject sends the reason', async () => {
  const h = harness({ '/v1/skills/admin/sources': { sources: [] } });
  h.context.s = Object.assign({}, source1, { approval: 'pending', created_by: 'u_2' });
  h.run(`skillsUI.openId = null; skillsUI.detail = null;`);
  await h.run(`decideSkillSource(s, 'approve')`);
  const approve = h.requests.find((r) => r.path === '/v1/skills/sources/src_1/approval');
  assert.equal(approve.method, 'POST');
  assert.deepEqual(plain(approve.body), { decision: 'approve' });
  h.run(`prompt = () => '  not our repository '`);
  await h.run(`decideSkillSource(s, 'reject')`);
  const reject = h.requests.filter((r) => r.path === '/v1/skills/sources/src_1/approval').pop();
  assert.deepEqual(plain(reject.body), { decision: 'reject', note: 'not our repository' });
  // Cancelling the reason prompt sends nothing.
  h.run(`prompt = () => null`);
  await h.run(`decideSkillSource(s, 'reject')`);
  assert.equal(h.requests.filter((r) => r.path === '/v1/skills/sources/src_1/approval').length, 2);
});

test('a contributor source page hides version and approval buttons; an admin gets them', () => {
  const h = harness();
  h.context.state.user.role = 'member';
  h.context.s = Object.assign({}, source1, { pending_version_id: 'skv_3', approval: 'pending', created_by: 'u_1' });
  h.run(`skillsUI.contributor = true; skillsUI.loaded = true; skillsUI.openId = 'src_1'; skillsUI.users = [];
    skillsUI.detail = { id: 'src_1', source: s, busy: '', secret: null, error: '',
      versions: [
        { id: 'skv_3', sha: 'cccccccccccccccc', status: 'staged', skill_count: 13, warnings: [], diff: { added: ['new-skill'], changed: [], removed: [] }, created_at: '2026-10-25T09:00:00Z' },
        { id: 'skv_1', sha: '0123456789abcdef', status: 'active', skill_count: 12, warnings: [], created_at: '2026-10-24T09:00:00Z' },
        { id: 'skv_0', sha: 'aaaaaaaaaaaaaaaa', status: 'retired', skill_count: 11, warnings: [], created_at: '2026-10-23T09:00:00Z' },
      ],
      skills: [{ path: 'pdf', name: 'pdf', runs: 'anywhere', reads_30d: 7 }] };`);
  const page = text(h.run('viewSkills()'));
  assert.doesNotMatch(page, /\bApprove\b/);
  assert.doesNotMatch(page, /\bReject\b/);
  assert.doesNotMatch(page, /\bActivate\b/);
  assert.match(page, /An admin makes versions live/);
  assert.match(page, /Waiting for approval/);
  assert.match(page, /Waits for an admin's approval\./);
  assert.match(page, /Approval\s+waits for an admin/);
  assert.match(page, /Waiting for review/);
  assert.match(page, /pending approval/);
  h.context.state.user.role = 'admin';
  const admin = text(h.run('viewSkills()'));
  assert.match(admin, /\bApprove\b/);
  assert.match(admin, /\bReject\b/);
  assert.match(admin, /\bActivate\b/);
});

test('loadSkillsMe makes a member a contributor; a contributor never loads the admin panels', async () => {
  const h = harness({ '/v1/skills/admin/me': { user_id: 'u_1', admin: false, contributor: true }, '/v1/skills/admin/sources': { sources: [] } });
  h.context.state.user.role = 'member';
  await h.run('loadSkillsMe()');
  assert.equal(h.run('skillsUI.contributor'), true);
  await h.run('loadSkills()');
  assert.ok(h.requests.find((r) => r.path === '/v1/skills/admin/sources'));
  assert.equal(h.requests.some((r) => r.path === '/v1/skills/admin/github-token'), false);
  assert.equal(h.requests.some((r) => r.path === '/v1/skills/admin/contributors'), false);
  // An admin is never a contributor, and a hub that is off (404) leaves it off.
  const admin = harness({ '/v1/skills/admin/me': { user_id: 'u_1', admin: true, contributor: true } });
  await admin.run('loadSkillsMe()');
  assert.equal(admin.run('skillsUI.contributor'), false);
  const off = harness({ '/v1/skills/admin/me': { __throw: Object.assign(new Error('not found'), { status: 404 }) } });
  await off.run('loadSkillsMe()');
  assert.equal(off.run('skillsUI.contributor'), false);
});

test('the people pickers read /v1/skills/admin/people', async () => {
  const h = harness({ '/v1/skills/admin/people': { people: [{ id: 'u_2', username: 'bea', role: 'member' }] } });
  await h.run('loadSkillsUsers()');
  assert.ok(h.requests.find((r) => r.path === '/v1/skills/admin/people'));
  assert.equal(h.requests.some((r) => r.path === '/v1/users'), false);
  assert.deepEqual(plain(h.run('skillsUI.users')).map((u) => u.id), ['u_2']);
});

test('the wizard says a contributor source waits for an admin', () => {
  const h = harness();
  h.run(`skillsUI.wizard = skillsWizardNew(); skillsUI.wizard.step = 'done';
    skillsUI.wizard.result = { status: 'pending approval', source: { id: 'src_2', slug: 'mine', approval: 'pending' } };`);
  assert.match(text(h.run('renderSkillsWizard()')), /agents see nothing of it until an admin approves it\. The owner has the question in the Inbox\./);
});
