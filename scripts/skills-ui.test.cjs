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
