// Skills: the skills hub admin page (TEC-1782). An admin adds a GitHub
// repository and branch once; toolyard copies the Agent Skills in it into
// its database at an exact commit, keeps that copy current, and serves it
// to every agent. This page lists the sources, adds one (a wizard: URL,
// branch, folder, token, preview, mode, visibility), and shows a source:
// versions with rollback and review, warnings, skills with a runs override
// and their own sharing, the source's sharing (audience and block list),
// and the GitHub webhook setup. Admins see every source; a skill
// contributor (a member an admin named in the Skill contributors panel)
// sees only the sources they added, and an admin approves those before
// agents see them.
//
// Loaded before app.js. Nothing here runs at load time: app.js calls
// viewSkills / loadSkills / skillsRoute from its router, and these
// functions use app.js's helpers (el, api, toast, render, relTime, state,
// navigate, copyToButton) only when called.

const SKILLS_DOCS_URL = 'https://github.com/iamtushar324/toolyard/blob/main/docs/skills-hub.md';
const skillsUI = {
  loading: false, loaded: false, error: '', sources: [],
  openId: null,     // the source page shown, or null for the list
  detail: null,     // { id, source, versions, skills, error, loading, busy, secret }
  wizard: null,     // the Add source wizard (skillsWizardNew)
  edit: null,       // the Edit source modal { id, draft, saving, error }
  share: null,      // one skill's Sharing modal { src, skill, draft, saving, error }
  users: null,      // /v1/skills/admin/people rows for the people pickers, once loaded
  // me is GET /v1/skills/admin/me ({ user_id, admin, contributor }); a
  // contributor is a member who may add skill sources (their own only).
  me: null, contributor: false,
  contributors: null, // admin: /v1/skills/admin/contributors rows, once loaded
  contrib: { pick: '', busy: false, error: '' }, // the Skill contributors panel
  syncing: {},      // source id -> true while Sync now runs
  // access is the GitHub access panel: the default read token's state
  // (/v1/skills/admin/github-token) and its editor.
  access: { data: null, error: '', editing: false, token: '', busy: false },
  watching: {},     // source id -> true while its first sync is polled
  // githubConnection shows "Use my GitHub connection" in the wizard: a
  // private repository read with the admin's own toolyard GitHub
  // connection. openSkillsWizard sets it from GET /v1/me/connections.
  githubConnection: false,
};

// ---- pure helpers (scripts/skills-ui.test.cjs) -------------------------------

// skillsMs turns an API time (RFC 3339 string or Unix ms) into Unix ms.
function skillsMs(v) {
  if (!v) return 0;
  if (typeof v === 'number') return v;
  const t = Date.parse(v);
  return Number.isFinite(t) ? t : 0;
}

function skillsShort(sha) { return sha ? String(sha).slice(0, 12) : ''; }

function skillsCount(n, noun) { return n === 1 ? '1 ' + noun : (n || 0) + ' ' + noun + 's'; }

function skillsRepoURL(src) {
  return 'https://github.com/' + encodeURIComponent(src.owner || '') + '/' + encodeURIComponent(src.repo || '');
}

function skillsCommitURL(src, sha) { return skillsRepoURL(src) + '/commit/' + encodeURIComponent(sha || ''); }

// skillsBuiltin reports the built-in source: the skills that ship inside
// toolyard. It has no repository, branch, token, mode or webhook, and
// cannot be deleted; admins change only who sees it and where its skills
// run (docs/skills-hub.md#built-in-skills).
function skillsBuiltin(src) { return !!src && src.kind === 'builtin'; }

// skillsRepoLabel is "owner/repo@branch", plus the folder when there is one.
function skillsRepoLabel(src) {
  if (skillsBuiltin(src)) return 'built into toolyard';
  const folder = (src.path || '').replace(/^\/+|\/+$/g, '');
  return (src.owner || '') + '/' + (src.repo || '') + '@' + (src.branch || '') + (folder ? ' · ' + folder : '');
}

// skillsStatus is the one word the list shows for a source. A rejected
// contributor source is off, so rejected comes before paused.
function skillsStatus(src, syncing) {
  if (syncing) return 'syncing';
  if (src.approval === 'rejected') return 'rejected';
  if (!src.enabled) return 'paused';
  if (src.hidden) return 'hidden';
  if (src.approval === 'pending') return 'pending approval';
  if (src.last_error && /rate limit/i.test(src.last_error)) return 'rate limited';
  if (src.last_error) return 'error';
  if (src.pending_version_id) return 'pending review';
  if (!src.active_version_id) return 'syncing';
  return 'ok';
}

function skillsStatusClass(status) {
  return ({ ok: 'allowed', 'pending review': 'pending', 'pending approval': 'pending', 'rate limited': 'pending', error: 'denied', rejected: 'denied', paused: 'expired', hidden: 'expired', syncing: 'ask' })[status] || '';
}

// skillsIsAdmin is app.js's isAdmin: an owner or admin. A member on the
// Skills page is a skill contributor, limited to their own sources.
function skillsIsAdmin() { return !!state.user && state.user.role !== 'member'; }

function skillsPeople(n) { return n === 1 ? '1 person' : (n || 0) + ' people'; }

// skillsPersonName is a person's name from the people list, or the ID
// until the list is loaded.
function skillsPersonName(id) {
  const u = (skillsUI.users || []).find((x) => x.id === id);
  return u ? (u.display_name || u.username || u.email || u.id) : (id || '');
}

// skillsAudienceText says who sees a source: everyone, its chosen members
// or nobody (admins and the person who added it, in the dashboard), and
// how many people its block list keeps out.
function skillsAudienceText(src) {
  const v = src.visibility || 'everyone';
  const who = v === 'members' ? skillsCount((src.members || []).length, 'chosen member')
    : v === 'nobody' ? 'nobody (only admins and the person who added it, in the dashboard)' : 'everyone';
  const blocked = (src.blocked || []).length;
  return (src.hidden ? 'hidden by an admin: no agent sees its skills · ' : '') + who + (blocked ? ' · ' + blocked + ' blocked' : '');
}

// skillsApprovalText is the Approval fact of a contributor's source ('' for
// a source an admin added).
function skillsApprovalText(src) {
  if (src.approval === 'pending') return 'waits for an admin';
  if (src.approval === 'approved') return 'approved' + (src.approval_by ? ' by ' + skillsPersonName(src.approval_by) : '');
  if (src.approval === 'rejected') return 'rejected' + (src.approval_note ? ': ' + src.approval_note : '');
  return '';
}

// skillsSharingLabel is the Shared with column for one skill: its own
// audience, or the source's, and its own block list.
function skillsSharingLabel(s) {
  const sh = (s && s.sharing) || {};
  const blocked = (sh.blocked || []).length;
  const more = blocked ? ' · ' + blocked + ' blocked' : '';
  if (sh.mode === 'everyone') return 'Everyone' + more;
  if (sh.mode === 'members') return skillsCount((sh.members || []).length, 'member') + more;
  if (sh.mode === 'nobody') return 'Nobody (admins only)' + more;
  return 'Same as source' + more;
}

// skillsSharingBody is the PUT .../skills/sharing body for a skill's
// draft: mode '' follows the source; members go only with mode members.
function skillsSharingBody(draft, path) {
  const mode = draft.mode === 'inherit' ? '' : (draft.mode || '');
  const body = { path, mode, blocked: (draft.blocked || []).slice() };
  if (mode === 'members') body.members = (draft.members || []).slice().sort();
  return body;
}

// skillsSharingDraft is the Sharing panel's editable copy of a source's
// audience and block list; from keeps what it was made from.
function skillsSharingDraft(src) {
  const from = { visibility: src.visibility || 'everyone', members: (src.members || []).slice(), blocked: (src.blocked || []).slice() };
  return { id: src.id, from, visibility: from.visibility, members: from.members.slice(), blocked: from.blocked.slice(), memberFilter: '', blockFilter: '' };
}

// skillsSourceSharingPatch is the Sharing panel's PATCH body: only what
// changed among visibility, members and blocked. Members go only with the
// members audience, and always when the audience becomes members.
function skillsSourceSharingPatch(src, draft) {
  const body = {};
  const sorted = (a) => (a || []).slice().sort();
  const same = (a, b) => JSON.stringify(sorted(a)) === JSON.stringify(sorted(b));
  const vis = draft.visibility || 'everyone';
  if (vis !== (src.visibility || 'everyone')) body.visibility = vis;
  if (vis === 'members' && (body.visibility || !same(draft.members, src.members))) body.members = sorted(draft.members);
  if (!same(draft.blocked, src.blocked)) body.blocked = sorted(draft.blocked);
  return body;
}

// skillsWizardModes are the Updates choices the wizard offers. A
// contributor gets review, plus automatic for a trusted owner; never
// pinned (the server refuses the rest).
function skillsWizardModes(w, contributor) {
  const auto = ['auto', 'Automatic: a valid new commit goes live'];
  if (contributor) return (w && w.info && w.info.trusted_owner ? [auto] : []).concat([['review', 'Review: an admin approves each new commit']]);
  return [auto, ['review', 'Review: each new commit waits for my approval'], ['pinned', 'Pinned: stay on this commit']];
}

// skillsEditModes are the Updates choices of the Edit modal. A contributor
// keeps review, and the source's own mode (automatic was allowed for a
// trusted owner; pinned is an admin's choice).
function skillsEditModes(src, contributor) {
  const all = [['auto', 'Automatic'], ['review', 'Review each new commit'], ['pinned', 'Pinned to one commit']];
  return contributor ? all.filter(([v]) => v === 'review' || v === src.mode) : all;
}

// skillsGroupWarnings groups warnings by code, the largest group first.
function skillsGroupWarnings(warnings) {
  const groups = new Map();
  for (const w of warnings || []) {
    if (!groups.has(w.code)) groups.set(w.code, []);
    groups.get(w.code).push(w);
  }
  return [...groups.entries()].map(([code, items]) => ({ code, items })).sort((a, b) => b.items.length - a.items.length);
}

// skillsWebhookURL is the payload URL GitHub posts to.
function skillsWebhookURL(src, origin) {
  return (origin || '') + (src.webhook_url_path || '/v1/skills/sources/' + encodeURIComponent(src.id) + '/webhook');
}

// skillsFirstSyncText says what a new source's first sync did, once its
// first check is recorded (last_checked_at), or that it still runs.
function skillsFirstSyncText(src) {
  if (!src || !src.last_checked_at) return 'The first sync runs now. A large repository can take a minute; this updates by itself.';
  if (src.last_error) return 'The first sync failed: ' + src.last_error + '. toolyard tries again on its next check; Sync now on the source page tries at once.';
  if (src.pending_version_id) return 'First sync: the first version waits for review on the source page.';
  if (src.active_sha) return 'First sync: commit ' + skillsShort(src.active_sha) + ' is live, with ' + skillsCount(src.skill_count, 'skill') + '.';
  return 'First sync: no new commit to copy.';
}

// skillsVersionState is how the versions table shows a version: a staged
// version other than the source's pending one was replaced (by a newer
// commit or a settings change) and can no longer be approved.
function skillsVersionState(src, v) {
  if (v.status === 'staged' && v.id !== src.pending_version_id) return 'superseded';
  return v.status;
}

function skillsSleep(ms) { return new Promise((resolve) => setTimeout(resolve, ms)); }

// skillsClock is a time as HH:MM in the browser's time zone.
function skillsClock(v) {
  const ms = skillsMs(v);
  if (!ms) return '';
  const d = new Date(ms);
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}

// skillsErrorText is the message to show for a failed API call: a GitHub
// rate limit names when it ends and the way out.
function skillsErrorText(e) {
  const b = (e && e.body) || {};
  if (b.code === 'rate_limited') {
    return 'GitHub rate limit reached' + (b.reset_at ? ' until ' + skillsClock(b.reset_at) : '') + ' — add a GitHub token in GitHub access.';
  }
  return (e && e.message) || 'Something went wrong.';
}

// skillsRoute reads "skills" or "skills/<source id>" into skillsUI.openId.
function skillsRoute(route) {
  const id = String(route || '').split('/')[1] || '';
  skillsUI.openId = /^[A-Za-z0-9_-]{1,64}$/.test(id) ? id : null;
  if (!skillsUI.openId) skillsUI.detail = null;
  skillsUI.share = null;
  return 'skills';
}

// ---- the Add source wizard: a small state machine ----------------------------
//
//   url --inspect--> repo --preview--> review --save--> done
//
// Changing the branch, folder or token after a preview drops the preview
// and goes back to "repo", so what is saved is always what was previewed.

function skillsWizardNew() {
  return {
    step: 'url', url: '', info: null,
    branch: '', branchFilter: '', path: '',
    auth: 'none', token: '',          // auth: none | token | github
    preview: null,
    mode: '',                          // '' until inspect picks the default
    visibility: 'everyone', members: [], memberFilter: '',
    defaultRuns: 'anywhere',
    busy: false, error: '', result: null,
  };
}

// skillsWizardAfterInspect takes the inspect answer: the branch from the
// URL or the default branch, the folder from the URL, a token field when
// the repository is private, and the default mode by the trusted-owner
// rule (auto for a trusted owner, else review).
function skillsWizardAfterInspect(w, info) {
  w.info = info;
  w.branch = info.branch || info.default_branch || '';
  w.path = info.path || '';
  if ((info.private || info.needs_token) && w.auth === 'none') w.auth = 'token';
  w.mode = info.trusted_owner ? 'auto' : 'review';
  w.preview = null; w.error = ''; w.step = 'repo';
  return w;
}

// skillsWizardSet changes one field and drops a preview it made stale.
function skillsWizardSet(w, field, value) {
  w[field] = value;
  if (['branch', 'path', 'auth', 'token'].includes(field) && w.preview) {
    w.preview = null;
    if (w.step === 'review') w.step = 'repo';
  }
  return w;
}

// skillsWizardBranches lists the branches to offer: those matching the
// filter, with the chosen branch always first.
function skillsWizardBranches(w, max) {
  const all = (w.info && w.info.branches) || [];
  const f = (w.branchFilter || '').trim().toLowerCase();
  let list = f ? all.filter((b) => b.toLowerCase().includes(f)) : all.slice();
  list = list.filter((b) => b !== w.branch);
  if (w.branch) list.unshift(w.branch);
  return list.slice(0, max || 200);
}

// skillsWizardInput is the SourceInput the wizard sends. The token goes
// only with auth "token"; the source settings only with the final save.
function skillsWizardInput(w, forSave) {
  const body = { url: w.url.trim() };
  if (w.branch) body.branch = w.branch;
  if (w.path.trim()) body.path = w.path.trim().replace(/^\/+|\/+$/g, '');
  if (w.auth === 'token' && w.token.trim()) { body.auth = 'token'; body.token = w.token.trim(); }
  if (w.auth === 'github') body.auth = 'github';
  if (forSave) {
    if (w.mode) body.mode = w.mode;
    body.visibility = w.visibility;
    if (w.visibility === 'members') body.members = w.members.slice();
    body.default_runs = w.defaultRuns;
  }
  return body;
}

function skillsWizardCanInspect(w) { return !w.busy && w.url.trim() !== ''; }
function skillsWizardCanPreview(w) { return !w.busy && !!w.info && !!w.branch && (w.auth !== 'token' || w.token.trim() !== ''); }
function skillsWizardCanSave(w) {
  return !w.busy && !!w.preview && (w.visibility !== 'members' || w.members.length > 0);
}

// skillsWizardRefreshNext enables or disables the wizard's next button after
// a keystroke. A text field changes the wizard without a re-render (which
// would move the focus), so the button must follow on its own.
function skillsWizardRefreshNext(w) {
  const b = typeof document !== 'undefined' && document.getElementById('skills-wiz-next');
  if (!b) return;
  b.disabled = w.step === 'url' ? !skillsWizardCanInspect(w)
    : w.step === 'repo' ? !skillsWizardCanPreview(w)
    : w.step === 'review' ? !skillsWizardCanSave(w) : false;
}

// ---- data --------------------------------------------------------------------

// loadSkillsMe asks what the signed-in person may do on the Skills page.
// app.js shows a member the Skills page only when they are a contributor;
// a failed read (the hub is off: 404) leaves that off.
async function loadSkillsMe() {
  try {
    const me = await api('/v1/skills/admin/me');
    skillsUI.me = me || null;
    skillsUI.contributor = !!(me && me.contributor && !me.admin);
  } catch (e) {
    skillsUI.me = null;
    skillsUI.contributor = false;
  }
  render();
}

async function loadSkills() {
  if (skillsUI.loading) return;
  skillsUI.loading = true; skillsUI.error = '';
  // The default GitHub token and the contributors are admins' business.
  if (skillsIsAdmin()) { loadSkillsAccess(); loadSkillsContributors(); }
  try {
    const out = await api('/v1/skills/admin/sources');
    skillsUI.sources = (out && out.sources) || [];
    skillsUI.loaded = true;
  } catch (e) {
    skillsUI.error = e.status === 404 ? 'The skills hub is off on this toolyard (start it with -skills=on).' : (e.message || 'Could not load skill sources.');
  } finally {
    skillsUI.loading = false;
    render();
  }
  if (skillsUI.openId && (!skillsUI.detail || skillsUI.detail.id !== skillsUI.openId)) loadSkillSource(skillsUI.openId);
}

// loadSkillsAccess reads the default GitHub token's state (never the
// token).
async function loadSkillsAccess() {
  const a = skillsUI.access;
  try {
    a.data = await api('/v1/skills/admin/github-token');
    a.error = '';
  } catch (e) {
    a.data = null;
    a.error = e.status === 404 ? '' : skillsErrorText(e);
  }
  render();
}

// saveSkillsAccess sets (token) or clears ('') the default token.
async function saveSkillsAccess(token) {
  const a = skillsUI.access;
  if (a.busy) return;
  if (!token && !confirm('Clear the default GitHub token? Sources without their own token then read anonymously: 60 GitHub calls an hour from this server.')) return;
  a.busy = true; a.error = ''; render();
  try {
    a.data = await api('/v1/skills/admin/github-token', { method: 'PUT', body: { token } });
    a.editing = false;
    toast(token ? 'GitHub token saved. Sources without their own token now read with it.' : 'GitHub token cleared.');
  } catch (e) { a.error = skillsErrorText(e); }
  a.token = ''; // write-only: the browser forgets it either way
  a.busy = false; render();
}

async function loadSkillSource(id) {
  const d = skillsUI.detail && skillsUI.detail.id === id ? skillsUI.detail : { id, source: null, versions: [], skills: [], error: '', busy: '', secret: null };
  d.loading = true; skillsUI.detail = d; render();
  try {
    const [src, vs, sk] = await Promise.all([
      api('/v1/skills/sources/' + encodeURIComponent(id)),
      api('/v1/skills/sources/' + encodeURIComponent(id) + '/versions?limit=20'),
      api('/v1/skills/sources/' + encodeURIComponent(id) + '/skills'),
    ]);
    d.source = src; d.versions = (vs && vs.versions) || []; d.skills = (sk && sk.skills) || []; d.error = '';
  } catch (e) {
    d.error = e.status === 404 ? 'This source does not exist any more.' : e.message;
  } finally {
    d.loading = false;
    render();
  }
}

// loadSkillsUsers reads the active people for the pickers. Admins and
// contributors may read it (/v1/users is admins only).
async function loadSkillsUsers() {
  if (skillsUI.users) return;
  skillsUI.users = [];
  try {
    const out = await api('/v1/skills/admin/people');
    skillsUI.users = (out && out.people) || [];
  } catch (e) { toast('Could not load members: ' + e.message, 'error'); }
  render();
}

// loadSkillsContributors reads the skill contributors (admins only). The
// panel stays hidden while it is not loaded, or when the server has none.
async function loadSkillsContributors() {
  const c = skillsUI.contrib;
  try {
    const out = await api('/v1/skills/admin/contributors');
    skillsUI.contributors = (out && out.contributors) || [];
  } catch (e) {
    if (e.status === 404) skillsUI.contributors = null;
    else c.error = skillsErrorText(e);
  }
  render();
}

async function addSkillsContributor(id) {
  const c = skillsUI.contrib;
  if (!id || c.busy) return;
  c.busy = true; c.error = ''; render();
  try {
    await api('/v1/skills/admin/contributors/' + encodeURIComponent(id), { method: 'PUT' });
    c.pick = '';
    toast(skillsPersonName(id) + ' can add skill sources now. Their sources wait for an admin\'s approval.');
  } catch (e) { c.error = skillsErrorText(e); }
  c.busy = false;
  await loadSkillsContributors();
}

async function removeSkillsContributor(id) {
  const c = skillsUI.contrib;
  if (c.busy) return;
  if (!confirm('Remove ' + skillsPersonName(id) + ' as a skill contributor? Their sources stay; only admins can change them then.')) return;
  c.busy = true; c.error = ''; render();
  try {
    await api('/v1/skills/admin/contributors/' + encodeURIComponent(id), { method: 'DELETE' });
    toast(skillsPersonName(id) + ' is no longer a skill contributor.');
  } catch (e) { c.error = skillsErrorText(e); }
  c.busy = false;
  await loadSkillsContributors();
}

// skillsBusy runs one source action with its button disabled, then
// reloads the source and the list.
async function skillsBusy(label, fn, okMsg) {
  const d = skillsUI.detail;
  if (d) { d.busy = label; render(); }
  try {
    const out = await fn();
    if (okMsg) toast(typeof okMsg === 'function' ? okMsg(out) : okMsg);
    return out;
  } catch (e) {
    toast(skillsErrorText(e), 'error');
    return null;
  } finally {
    if (d) d.busy = '';
    if (skillsUI.openId) await loadSkillSource(skillsUI.openId);
    skillsUI.loaded = false;
    await loadSkills();
  }
}

async function syncSkillSource(id) {
  skillsUI.syncing[id] = true; render();
  try {
    const res = await api('/v1/skills/sources/' + encodeURIComponent(id) + '/sync', { method: 'POST' });
    toast(skillsSyncSummary(res));
  } catch (e) { toast(skillsErrorText(e), 'error'); }
  delete skillsUI.syncing[id];
  if (skillsUI.openId === id) await loadSkillSource(id);
  skillsUI.loaded = false;
  await loadSkills();
}

// skillsSyncSummary says in one line what a sync did.
function skillsSyncSummary(res) {
  if (!res) return 'Sync finished.';
  if (res.error) return 'Sync failed: ' + res.error;
  if (res.unchanged) return 'No new commit; ' + skillsShort(res.sha) + ' is current.';
  if (res.staged) return 'Commit ' + skillsShort(res.sha) + ' waits for review.';
  if (res.activated) return 'Commit ' + skillsShort(res.sha) + ' is live.';
  return 'Synced ' + skillsShort(res.sha) + '.';
}

// hideSkillSource sets or clears an admin's hide lock: while it is set no
// agent sees any of the source's skills, whatever its sharing says, and a
// contributor cannot change it.
function hideSkillSource(src) {
  const on = !src.hidden;
  if (on && !confirm('Hide ' + src.slug + '? No agent sees any of its skills until an admin shows it again, whatever its sharing says. The person who added it cannot undo this.')) return null;
  return skillsBusy('hide', () => api('/v1/skills/sources/' + encodeURIComponent(src.id), { method: 'PATCH', body: { hidden: on } }),
    on ? src.slug + ' is hidden from every agent.' : src.slug + ' is shown again, by its sharing settings.');
}

function toggleSkillSource(src) {
  const on = !src.enabled;
  return skillsBusy('pause', () => api('/v1/skills/sources/' + encodeURIComponent(src.id), { method: 'PATCH', body: { enabled: on } }),
    on ? src.slug + ' resumed.' : src.slug + ' paused: agents keep the live version, and toolyard stops checking for new commits.');
}

async function deleteSkillSource(src) {
  if (!confirm(`Delete ${src.slug}? Agents lose its skills at once, and its versions and read counts are deleted. The GitHub repository is not touched.`)) return;
  try {
    await api('/v1/skills/sources/' + encodeURIComponent(src.id), { method: 'DELETE' });
    toast(src.slug + ' deleted.');
    skillsUI.detail = null;
    navigate('skills');
    skillsUI.loaded = false;
    await loadSkills();
  } catch (e) { toast(skillsErrorText(e), 'error'); }
}

function activateSkillVersion(src, v, staged) {
  const what = staged ? 'Approve commit ' + skillsShort(v.sha) + ' and make it live for agents?' : 'Roll back to commit ' + skillsShort(v.sha) + '? It becomes the live version for agents at once.';
  if (!confirm(what)) return null;
  return skillsBusy('version', () => api('/v1/skills/sources/' + encodeURIComponent(src.id) + '/versions/' + encodeURIComponent(v.id) + '/activate', { method: 'POST' }),
    'Commit ' + skillsShort(v.sha) + ' is live.');
}

function rejectSkillVersion(src, v) {
  const note = prompt('Reject commit ' + skillsShort(v.sha) + '? The live version stays. Optional note:', '');
  if (note == null) return null;
  return skillsBusy('version', () => api('/v1/skills/sources/' + encodeURIComponent(src.id) + '/versions/' + encodeURIComponent(v.id) + '/reject', { method: 'POST', body: { note: note.trim() } }),
    'Commit ' + skillsShort(v.sha) + ' rejected.');
}

function setSkillRuns(src, path, runs) {
  return skillsBusy('runs', () => api('/v1/skills/sources/' + encodeURIComponent(src.id) + '/skills/runs', { method: 'PUT', body: { path, runs } }),
    runs ? path + ' now runs ' + (runs === 'host' ? 'on the T3 host only.' : 'anywhere.') : path + ' follows its skill and source again.');
}

// decideSkillSource approves or rejects a contributor's source (admins
// only). Approving after a rejection is allowed; a rejection turns it off.
function decideSkillSource(src, decision) {
  let note = '';
  if (decision === 'reject') {
    note = prompt('Reject ' + src.slug + '? It is turned off and agents see nothing of it. Optional reason for the person who added it:', '');
    if (note == null) return null;
  } else if (!confirm('Approve ' + src.slug + '? The people it is shared with, and their agents, get its skills at once.')) return null;
  const body = { decision };
  if (note.trim()) body.note = note.trim();
  // The approval is bound to the version shown on this page (its commit
  // and its ID): the waiting one, else the live one. The server refuses it
  // when another version would go live, so nothing goes live that the
  // admin did not see.
  const d = skillsUI.detail;
  const waiting = decision === 'approve' && d && d.id === src.id && (d.versions || []).find((v) => v.id === src.pending_version_id);
  if (waiting && waiting.sha) {
    body.sha = waiting.sha;
    body.version_id = waiting.id;
  } else if (decision === 'approve' && !src.pending_version_id && src.active_sha) {
    body.sha = src.active_sha;
    if (src.active_version_id) body.version_id = src.active_version_id;
  }
  return skillsBusy('approval', () => api('/v1/skills/sources/' + encodeURIComponent(src.id) + '/approval', { method: 'POST', body }),
    decision === 'approve' ? src.slug + ' approved: agents it is shared with get its skills now.' : src.slug + ' rejected and turned off.');
}

// saveSkillsSharing saves the source Sharing panel: one PATCH with only
// what changed. The draft is dropped once saved, so the panel shows the
// reloaded source.
function saveSkillsSharing(src) {
  const d = skillsUI.detail, s = d && d.sharing;
  const body = s ? skillsSourceSharingPatch(src, s) : {};
  if (!Object.keys(body).length) return null;
  return skillsBusy('sharing', async () => {
    const out = await api('/v1/skills/sources/' + encodeURIComponent(src.id), { method: 'PATCH', body });
    d.sharing = null;
    return out;
  }, 'Sharing of ' + src.slug + ' saved.');
}

// openSkillsShare opens one skill's Sharing modal. Mode '' follows the
// source's audience.
function openSkillsShare(src, s) {
  const sh = s.sharing || {};
  skillsUI.share = { src, skill: s, saving: false, error: '', draft: {
    mode: sh.mode === 'inherit' ? '' : (sh.mode || ''), members: (sh.members || []).slice(), blocked: (sh.blocked || []).slice(),
    memberFilter: '', blockFilter: '',
  } };
  render();
}

async function saveSkillsShare() {
  const sh = skillsUI.share;
  if (!sh || sh.saving) return;
  sh.saving = true; sh.error = ''; render();
  try {
    const body = skillsSharingBody(sh.draft, sh.skill.path);
    await api('/v1/skills/sources/' + encodeURIComponent(sh.src.id) + '/skills/sharing', { method: 'PUT', body });
    skillsUI.share = null;
    toast(body.mode === '' && !body.blocked.length ? sh.skill.path + ' follows the source\'s sharing again.' : 'Sharing of ' + sh.skill.path + ' saved.');
    if (skillsUI.openId) await loadSkillSource(skillsUI.openId);
  } catch (e) { sh.saving = false; sh.error = skillsErrorText(e); render(); }
}

async function rotateSkillSecret(src) {
  if (src.has_webhook_secret && !confirm('Make a new webhook secret? The old one stops working: paste the new one into GitHub right away.')) return;
  const d = skillsUI.detail;
  if (d) { d.busy = 'secret'; render(); }
  try {
    const out = await api('/v1/skills/sources/' + encodeURIComponent(src.id) + '/webhook-secret', { method: 'POST' });
    if (d) d.secret = out;
  } catch (e) { toast(skillsErrorText(e), 'error'); }
  if (d) d.busy = '';
  render();
}

// ---- wizard actions ----------------------------------------------------------

function openSkillsWizard() { skillsUI.wizard = skillsWizardNew(); render(); loadSkillsGitHubConnection(); }

// skillsHasGitHubConnection reports whether a GET /v1/me/connections list
// holds a connected, enabled GitHub server (a per-user GitHub connection).
function skillsHasGitHubConnection(list) {
  return Array.isArray(list) && list.some((c) => c && c.transport === 'github' && c.state === 'connected' && c.enabled !== false);
}

// loadSkillsGitHubConnection offers "Use my GitHub connection" only to an
// admin who has one.
async function loadSkillsGitHubConnection() {
  try { skillsUI.githubConnection = skillsHasGitHubConnection(await api('/v1/me/connections')); } catch (e) { skillsUI.githubConnection = false; }
  render();
}

async function skillsWizardInspect() {
  const w = skillsUI.wizard;
  if (!w || !skillsWizardCanInspect(w)) return;
  w.busy = true; w.error = ''; render();
  try {
    const body = { url: w.url.trim() };
    if (w.auth === 'token' && w.token.trim()) body.token = w.token.trim();
    if (w.auth === 'github') body.auth = 'github';
    skillsWizardAfterInspect(w, await api('/v1/skills/sources/inspect', { method: 'POST', body }));
  } catch (e) {
    w.error = skillsErrorText(e);
    if (e.body && e.body.code === 'needs_token' && w.auth !== 'github') {
      w.auth = 'token';
      w.error = 'This repository is private or does not exist. ' + (skillsUI.githubConnection ? 'Paste a read-only token, or use your GitHub connection, and inspect again.' : 'Paste a read-only token and inspect again.');
    }
  }
  w.busy = false; render();
}

async function skillsWizardPreview() {
  const w = skillsUI.wizard;
  if (!w || !skillsWizardCanPreview(w)) return;
  w.busy = true; w.error = ''; render();
  try {
    w.preview = await api('/v1/skills/sources/preview', { method: 'POST', body: skillsWizardInput(w, false) });
    w.step = 'review';
  } catch (e) { w.error = skillsErrorText(e); }
  w.busy = false; render();
}

async function skillsWizardSave() {
  const w = skillsUI.wizard;
  if (!w || !skillsWizardCanSave(w)) return;
  w.busy = true; w.error = ''; render();
  try {
    w.result = await api('/v1/skills/sources', { method: 'POST', body: skillsWizardInput(w, true) });
    w.token = ''; // write-only: the browser forgets it once it is sealed
    w.step = 'done';
    skillsUI.loaded = false;
    loadSkills();
    if (w.result && w.result.source && !w.result.source.last_checked_at) skillsWatchWizard(w, w.result.source.id);
  } catch (e) { w.error = skillsErrorText(e); }
  w.busy = false; render();
}

// skillsWatchWizard polls a new source until its first check is recorded
// (the first sync runs in the background), for at most three minutes.
async function skillsWatchWizard(w, id) {
  for (let i = 0; i < 90; i++) {
    await skillsSleep(2000);
    if (skillsUI.wizard !== w) return;
    try {
      const src = await api('/v1/skills/sources/' + encodeURIComponent(id));
      if (skillsUI.wizard !== w) return;
      w.result.source = src;
      if (src.last_checked_at) { render(); skillsUI.loaded = false; loadSkills(); return; }
    } catch (_) { /* try again on the next tick */ }
  }
}

// skillsWatchSource reloads the open source page until its first check is
// recorded, for at most three minutes.
async function skillsWatchSource(id) {
  if (skillsUI.watching[id]) return;
  skillsUI.watching[id] = true;
  try {
    for (let i = 0; i < 90; i++) {
      await skillsSleep(2000);
      if (skillsUI.openId !== id) return;
      await loadSkillSource(id);
      const d = skillsUI.detail;
      if (!d || !d.source || d.source.last_checked_at) return;
    }
  } finally { delete skillsUI.watching[id]; }
}

function openSkillsEdit(src) {
  skillsUI.edit = { id: src.id, saving: false, error: '', draft: {
    branch: src.branch || '', path: src.path || '', mode: src.mode || 'review', pinned_sha: src.pinned_sha || '',
    poll_seconds: src.poll_seconds || 300, visibility: src.visibility || 'everyone', members: (src.members || []).slice(), memberFilter: '',
    default_runs: src.default_runs || 'anywhere', exclude: (src.exclude || []).join('\n'), token: '', clear_token: false,
  }, orig: src };
  if ((src.visibility || '') === 'members') loadSkillsUsers();
  render();
}

// skillsEditPatch is the PATCH body: only what changed. A token goes only
// when one was typed.
function skillsEditPatch(orig, d) {
  const body = {};
  const set = (k, v, was) => { if (JSON.stringify(v) !== JSON.stringify(was)) body[k] = v; };
  set('branch', d.branch.trim(), orig.branch || '');
  set('path', d.path.trim().replace(/^\/+|\/+$/g, ''), orig.path || '');
  set('mode', d.mode, orig.mode || '');
  if (d.mode === 'pinned') set('pinned_sha', d.pinned_sha.trim(), orig.pinned_sha || '');
  set('poll_seconds', Number(d.poll_seconds) || 300, orig.poll_seconds || 300);
  set('visibility', d.visibility, orig.visibility || 'everyone');
  if (d.visibility === 'members') set('members', d.members.slice().sort(), (orig.members || []).slice().sort());
  set('default_runs', d.default_runs, orig.default_runs || 'anywhere');
  set('exclude', d.exclude.split('\n').map((s) => s.trim()).filter(Boolean), orig.exclude || []);
  if (d.token.trim()) { body.token = d.token.trim(); body.auth = 'token'; }
  else if (d.clear_token) { body.clear_token = true; body.auth = 'none'; }
  return body;
}

async function saveSkillsEdit() {
  const ed = skillsUI.edit;
  if (!ed || ed.saving) return;
  const body = skillsEditPatch(ed.orig, ed.draft);
  if (!Object.keys(body).length) { skillsUI.edit = null; render(); return; }
  ed.saving = true; ed.error = ''; render();
  try {
    await api('/v1/skills/sources/' + encodeURIComponent(ed.id), { method: 'PATCH', body });
    skillsUI.edit = null;
    toast('Source saved.');
    if (skillsUI.openId) await loadSkillSource(skillsUI.openId);
    skillsUI.loaded = false;
    await loadSkills();
  } catch (e) { ed.saving = false; ed.error = skillsErrorText(e); render(); }
}

// ---- views -------------------------------------------------------------------

function skillsSelect(attrs, options, value) {
  const s = el('select', attrs, ...options.map(([v, label]) => el('option', { value: v }, label)));
  s.value = value;
  return s;
}

function skillsField(label, input, hint) {
  return el('label', { style: 'display: block; margin-top: 10px;' }, el('div', {}, label), input, hint ? el('div', { class: 'meta' }, hint) : null);
}

// skillsGroup is skillsField for a group of radios or a people picker: a
// div, so a click on its heading does not change the first radio.
function skillsGroup(label, content, hint) {
  return el('div', { style: 'margin-top: 10px;' }, el('div', {}, label), content, hint ? el('div', { class: 'meta' }, hint) : null);
}

// skillsRadio is one radio of a group; pick runs when it is chosen.
// The label sits in its own span so a long one wraps beside the radio,
// not onto a line of its own.
function skillsRadio(name, checked, label, pick) {
  return el('label', { class: 'row skills-radio' }, el('input', { type: 'radio', name, checked, on: { change: () => { pick(); render(); } } }), el('span', {}, label));
}

function skillsChip(src) {
  const st = skillsStatus(src, !!skillsUI.syncing[src.id]);
  return el('span', { class: 'badge ' + skillsStatusClass(st), title: src.last_error || '' }, st);
}

function viewSkills() {
  if (!skillsUI.loaded && !skillsUI.loading && !skillsUI.error) loadSkills();
  return el('div', {},
    skillsUI.wizard ? renderSkillsWizard() : null,
    skillsUI.edit ? renderSkillsEdit() : null,
    skillsUI.share ? renderSkillsShare() : null,
    skillsUI.openId ? renderSkillSource() : renderSkillsList());
}

function renderSkillsList() {
  const rows = skillsUI.sources;
  const admin = skillsIsAdmin();
  return el('div', {},
    el('div', { class: 'card' },
      el('div', { class: 'row' },
        el('h2', { class: 'grow', style: 'margin: 0;' }, admin ? 'Skill sources' : 'Your skill sources'),
        el('button', { class: 'primary', on: { click: openSkillsWizard } }, 'Add source')),
      el('p', { class: 'meta' }, 'Each source is a GitHub repository and branch that holds Agent Skills. toolyard copies them at an exact commit, checks for new commits every few minutes (or at once with a webhook), and serves them to agents through skills.search, skills.get and the MCP Skills extension. ',
        admin ? null : 'You see only the sources you added. A new source waits for an admin\'s approval: it syncs, and agents see nothing of it until an admin approves it. ',
        el('a', { href: SKILLS_DOCS_URL, target: '_blank', rel: 'noopener' }, 'How it works'))),
    skillsUI.error ? el('div', { class: 'card err' }, skillsUI.error) : null,
    renderSkillsAccess(),
    renderSkillsContributors(),
    !skillsUI.loaded && skillsUI.loading ? el('div', { class: 'card empty' }, 'Loading…')
      : skillsUI.loaded && rows.length === 0 ? el('div', { class: 'card empty' }, admin ? 'No skill sources yet. Add a repository to give your agents its skills.' : 'You have not added a skill source yet. Add a repository to share its skills.')
      : rows.length ? el('div', { class: 'card' }, el('table', { class: 'skills-table' },
          el('thead', {}, el('tr', {}, ['Source', 'Repository', 'Commit', 'Mode', 'Status', 'Skills', 'Last check'].map((h) => el('th', {}, h)))),
          el('tbody', {}, rows.map((src) => el('tr', { class: 'skills-row', tabindex: 0,
            on: { click: () => navigate('skills/' + src.id), keydown: (e) => { if (e.key === 'Enter') navigate('skills/' + src.id); } } },
            el('td', {}, el('b', {}, src.slug), skillsBuiltin(src) ? [' ', el('span', { class: 'badge allowed' }, 'Built in')] : null),
            el('td', { class: 'meta' }, skillsRepoLabel(src)),
            el('td', {}, src.active_sha ? el('code', {}, skillsShort(src.active_sha)) : el('span', { class: 'meta' }, '—')),
            el('td', {}, skillsBuiltin(src) ? 'with toolyard' : src.mode + (src.mode === 'pinned' && src.pinned_sha ? ' ' + skillsShort(src.pinned_sha) : '')),
            el('td', {}, skillsChip(src)),
            el('td', {}, String(src.skill_count || 0)),
            el('td', { class: 'meta' }, src.last_checked_at ? relTime(skillsMs(src.last_checked_at)) : 'never')))))) : null);
}

function renderSkillSource() {
  const d = skillsUI.detail;
  const back = el('button', { class: 'link', on: { click: () => navigate('skills') } }, '← All sources');
  if (!d || (!d.source && d.loading)) return el('div', {}, back, el('div', { class: 'card empty' }, 'Loading…'));
  if (!d.source) return el('div', {}, back, el('div', { class: 'card err' }, d.error || 'Could not load this source.'));
  const src = d.source;
  if (skillsBuiltin(src)) return renderSkillsBuiltin(d, back);
  const busy = !!d.busy || !!skillsUI.syncing[src.id];
  if (src.enabled && !src.last_checked_at) skillsWatchSource(src.id);
  // Only the source's pending version can be approved; older staged ones
  // show as superseded in the versions table.
  const staged = d.versions.filter((v) => v.status === 'staged' && v.id === src.pending_version_id);
  const shown = d.versions.find((v) => v.id === src.pending_version_id) || d.versions.find((v) => v.status === 'active');
  const admin = skillsIsAdmin();
  return el('div', {}, back,
    el('div', { class: 'card' },
      el('div', { class: 'row' },
        el('h2', { class: 'grow', style: 'margin: 0;' }, src.slug),
        skillsChip(src)),
      el('p', { class: 'meta' }, el('a', { href: skillsRepoURL(src), target: '_blank', rel: 'noopener' }, src.owner + '/' + src.repo),
        ' · branch ', el('code', {}, src.branch), src.path ? [' · folder ', el('code', {}, src.path)] : ' · repository root'),
      el('table', { class: 'skills-facts' }, el('tbody', {},
        skillsFact('Live commit', src.active_sha ? el('a', { href: skillsCommitURL(src, src.active_sha), target: '_blank', rel: 'noopener' }, el('code', {}, skillsShort(src.active_sha))) : 'none yet'),
        skillsFact('Skills', String(src.skill_count || 0) + (src.warning_count ? ' · ' + skillsCount(src.warning_count, 'warning') : '')),
        skillsFact('Updates', src.mode === 'auto' ? 'automatic' : src.mode === 'pinned' ? 'pinned to ' + skillsShort(src.pinned_sha) : 'each new commit waits for review'),
        skillsFact('Who sees it', skillsAudienceText(src)),
        src.approval ? skillsFact('Approval', skillsApprovalText(src)) : null,
        skillsFact('Default runs', src.default_runs === 'host' ? 'on the T3 host only' : 'anywhere'),
        skillsFact('Read access', src.has_token ? 'read-only token (stored sealed)' : src.auth === 'github' ? 'your GitHub connection' : 'public repository'),
        skillsFact('Last check', src.last_checked_at ? relTime(skillsMs(src.last_checked_at)) : 'never'),
        skillsFact('Next check', !src.enabled ? 'paused' : src.next_check_at ? relTime(skillsMs(src.next_check_at)) : 'soon'),
        src.last_error ? skillsFact('Last error', el('span', { class: 'err' }, src.last_error)) : null)),
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', { class: 'primary', disabled: busy || !src.enabled, on: { click: () => syncSkillSource(src.id) } }, skillsUI.syncing[src.id] ? 'Syncing…' : 'Sync now'),
        admin || src.approval !== 'rejected' ? el('button', { disabled: busy, on: { click: () => toggleSkillSource(src) } }, src.enabled ? 'Pause' : 'Resume') : null,
        admin ? el('button', { disabled: busy, title: 'An admin lock: no agent sees any of its skills', on: { click: () => hideSkillSource(src) } }, src.hidden ? 'Show to agents' : 'Hide from agents') : null,
        el('button', { disabled: busy, on: { click: () => openSkillsEdit(src) } }, 'Edit'),
        el('button', { class: 'danger', disabled: busy, on: { click: () => deleteSkillSource(src) } }, 'Delete'))),
    renderSkillsApproval(src, busy),
    staged.length ? el('div', { class: 'card skills-review' },
      el('h3', { style: 'margin-top: 0;' }, 'Waiting for review'),
      el('p', { class: 'meta' }, admin ? 'Agents keep the live version until you approve. The source creator also has this question in the Inbox.'
        : 'Agents keep the live version until an admin approves the new commit. An admin makes versions live.'),
      staged.map((v) => renderSkillsVersionSummary(src, v, busy))) : null,
    renderSkillsSharing(src, d, busy),
    renderSkillsVersions(src, d.versions, busy),
    shown && (shown.warnings || []).length ? renderSkillsWarnings(shown) : null,
    renderSkillsSkills(src, d.skills, busy),
    renderSkillsWebhook(src, d));
}

// renderSkillsBuiltin is the built-in source's page: what it is, who sees
// it, its versions (one per toolyard upgrade that changed the skills) and
// its skills with the runs override. Nothing about a repository.
function renderSkillsBuiltin(d, back) {
  const src = d.source;
  const busy = !!d.busy;
  return el('div', {}, back,
    el('div', { class: 'card' },
      el('div', { class: 'row' },
        el('h2', { class: 'grow', style: 'margin: 0;' }, src.slug, ' ', el('span', { class: 'badge allowed' }, 'Built in')),
        skillsChip(src)),
      el('p', { class: 'meta' }, 'The skills that ship inside toolyard: they teach agents how this gateway works and how to use it well. ',
        'A toolyard upgrade that changes them makes a new version here. They cannot be edited, synced from GitHub or deleted; you choose who sees them and where they run. ',
        el('a', { href: SKILLS_DOCS_URL + '#built-in-skills', target: '_blank', rel: 'noopener' }, 'Built-in skills')),
      el('table', { class: 'skills-facts' }, el('tbody', {},
        skillsFact('Live version', src.active_sha ? el('code', {}, skillsShort(src.active_sha)) : 'none yet'),
        skillsFact('Skills', String(src.skill_count || 0) + (src.warning_count ? ' · ' + skillsCount(src.warning_count, 'warning') : '')),
        skillsFact('Who sees it', skillsAudienceText(src)),
        skillsFact('Default runs', src.default_runs === 'host' ? 'on the T3 host only' : 'anywhere'),
        skillsFact('Installed', src.last_checked_at ? relTime(skillsMs(src.last_checked_at)) : 'not yet'),
        src.last_error ? skillsFact('Last error', el('span', { class: 'err' }, src.last_error)) : null)),
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', { disabled: busy, on: { click: () => openSkillsEdit(src) } }, 'Who sees it'),
        skillsIsAdmin() ? el('button', { disabled: busy, title: 'An admin lock: no agent sees any of its skills', on: { click: () => hideSkillSource(src) } }, src.hidden ? 'Show to agents' : 'Hide from agents') : null)),
    renderSkillsSharing(src, d, busy),
    el('div', { class: 'card' },
      el('h3', { style: 'margin-top: 0;' }, 'Versions'),
      (d.versions || []).length === 0 ? el('p', { class: 'meta' }, 'No versions yet.')
        : el('table', {},
          el('thead', {}, el('tr', {}, ['Version', 'Status', 'Skills', 'Changes', 'Made'].map((h) => el('th', {}, h)))),
          el('tbody', {}, d.versions.map((v) => {
            const diff = v.diff || {};
            return el('tr', {},
              el('td', {}, el('code', {}, skillsShort(v.sha))),
              el('td', {}, el('span', { class: 'badge ' + (v.status === 'active' ? 'allowed' : v.status === 'failed' ? 'denied' : 'expired') }, v.status === 'active' ? 'live' : v.status)),
              el('td', {}, String(v.skill_count || 0)),
              el('td', { class: 'meta' }, v.status === 'failed' ? (v.error || 'failed') : '+' + (diff.added || []).length + ' ~' + (diff.changed || []).length + ' −' + (diff.removed || []).length),
              el('td', { class: 'meta' }, relTime(skillsMs(v.created_at))));
          })))),
    renderSkillsSkills(src, d.skills || [], busy));
}

function skillsFact(label, value) {
  return el('tr', {}, el('td', { class: 'meta' }, label), el('td', {}, value));
}

// renderSkillsApproval is the card of a contributor's source that waits
// for an admin's approval, or that an admin rejected. Only admins get the
// buttons.
function renderSkillsApproval(src, busy) {
  if (src.approval !== 'pending' && src.approval !== 'rejected') return null;
  const admin = skillsIsAdmin();
  const pending = src.approval === 'pending';
  return el('div', { class: 'card skills-approval' },
    el('h3', { style: 'margin-top: 0;' }, pending ? 'Waiting for approval' : 'Rejected'),
    el('p', { class: 'meta' }, pending
      ? 'A skill contributor added this source. It syncs, but agents see nothing of it until an admin approves it. The owner also has this question in the Inbox.'
      : 'An admin rejected this source. It is off, and agents see nothing of it.'),
    !pending && src.approval_note ? el('p', {}, 'Reason: ' + src.approval_note) : null,
    admin ? el('div', { class: 'row' },
      el('button', { class: 'primary', disabled: busy, on: { click: () => decideSkillSource(src, 'approve') } }, 'Approve'),
      pending ? el('button', { disabled: busy, on: { click: () => decideSkillSource(src, 'reject') } }, 'Reject') : null)
      : el('p', { class: 'meta' }, pending ? 'Waits for an admin\'s approval.' : 'Only an admin can approve it now.'));
}

// renderSkillsSharing is the source's Sharing panel: its audience (with
// the member picker) and its block list, saved together with one PATCH.
// The draft follows the source until someone changes it.
function renderSkillsSharing(src, d, busy) {
  let s = d.sharing;
  const dirty = s && Object.keys(skillsSourceSharingPatch(s.from, s)).length > 0;
  if (!s || s.id !== src.id || (!dirty && Object.keys(skillsSourceSharingPatch(src, s.from)).length > 0)) s = d.sharing = skillsSharingDraft(src);
  const changed = Object.keys(skillsSourceSharingPatch(src, s)).length > 0;
  const blocked = s.blocked.length;
  return el('details', { class: 'card', 'data-disclosure-key': 'skills-sharing', open: changed },
    el('summary', {}, el('b', {}, 'Sharing'), el('span', { class: 'meta' }, ' · ' + skillsAudienceText(src))),
    el('p', { class: 'meta' }, 'Who sees this source\'s skills, in the dashboard and through their agents. One skill can differ: use Edit in its Shared with column below.'),
    skillsGroup('Who sees it', el('div', {},
      skillsRadio('skills-share-vis', s.visibility === 'everyone', 'Everyone and their agents', () => { s.visibility = 'everyone'; }),
      skillsRadio('skills-share-vis', s.visibility === 'members', 'Only the members I choose', () => { s.visibility = 'members'; }),
      skillsRadio('skills-share-vis', s.visibility === 'nobody', 'Nobody: only admins and the person who added it, in the dashboard; agents never', () => { s.visibility = 'nobody'; }),
      s.visibility === 'members' ? renderSkillsMembers(s, 'memberFilter', 'members', 'skills-share-members') : null)),
    el('details', { 'data-disclosure-key': 'skills-sharing-blocked', open: blocked > 0, style: 'margin-top: 10px;' },
      el('summary', {}, 'Block list', el('span', { class: 'meta' }, blocked ? ' · ' + blocked + ' blocked' : ' · nobody blocked')),
      el('p', { class: 'meta' }, 'People on it never see this source\'s skills, whatever the audience. Admins always see everything.'),
      renderSkillsMembers(s, 'blockFilter', 'blocked', 'skills-share-blocked')),
    el('div', { class: 'row', style: 'margin-top: 12px;' },
      el('button', { class: 'primary', disabled: busy || !changed || (s.visibility === 'members' && !s.members.length), on: { click: () => saveSkillsSharing(src) } },
        d.busy === 'sharing' ? 'Saving…' : 'Save sharing'),
      changed ? el('button', { disabled: busy, on: { click: () => { d.sharing = skillsSharingDraft(src); render(); } } }, 'Undo changes') : null));
}

function renderSkillsVersionSummary(src, v, busy) {
  const diff = v.diff || {};
  const list = (name, items) => (items && items.length) ? el('div', {}, el('b', {}, name + ' (' + items.length + '): '), items.slice(0, 20).map((p) => el('code', { style: 'margin-right: 6px;' }, p)), items.length > 20 ? ' and ' + (items.length - 20) + ' more' : '') : null;
  return el('div', { class: 'skills-version-summary' },
    el('div', {}, 'Commit ', el('a', { href: skillsCommitURL(src, v.sha), target: '_blank', rel: 'noopener' }, el('code', {}, skillsShort(v.sha))),
      ' · ' + skillsCount(v.skill_count, 'skill') + ' · ' + skillsCount((v.warnings || []).length, 'warning')),
    list('Added', diff.added), list('Changed', diff.changed), list('Removed', diff.removed),
    skillsIsAdmin() ? el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { class: 'primary', disabled: busy, on: { click: () => activateSkillVersion(src, v, true) } }, 'Approve'),
      el('button', { disabled: busy, on: { click: () => rejectSkillVersion(src, v) } }, 'Reject')) : null);
}

function renderSkillsVersions(src, versions, busy) {
  const admin = skillsIsAdmin();
  return el('div', { class: 'card' },
    el('h3', { style: 'margin-top: 0;' }, 'Versions'),
    admin ? null : el('p', { class: 'meta' }, 'An admin makes versions live: approves a new commit, rejects it, or rolls back to an older one.'),
    versions.length === 0 ? el('p', { class: 'meta' }, 'No versions yet. The first sync makes one.')
      : el('table', {},
        el('thead', {}, el('tr', {}, ['Commit', 'Status', 'Skills', 'Changes', 'Warnings', 'Made', ''].map((h) => el('th', {}, h)))),
        el('tbody', {}, versions.map((v) => {
          const diff = v.diff || {};
          const changes = v.status === 'failed' ? (v.error || 'failed')
            : '+' + (diff.added || []).length + ' ~' + (diff.changed || []).length + ' −' + (diff.removed || []).length;
          const state = skillsVersionState(src, v);
          let action = null;
          // A rejected version was never live: rolling back to it is not
          // offered, nor is approving a superseded staged one. A
          // contributor sees the state only.
          if (state === 'staged' && !admin) action = el('span', { class: 'meta' }, 'waits for an admin');
          else if (state === 'retired' && !admin) action = null;
          else if (state === 'staged') action = el('span', { class: 'row' },
            el('button', { disabled: busy, on: { click: () => activateSkillVersion(src, v, true) } }, 'Approve'),
            el('button', { disabled: busy, on: { click: () => rejectSkillVersion(src, v) } }, 'Reject'));
          else if (state === 'retired') action = el('button', { disabled: busy, title: 'Make this commit live again', on: { click: () => activateSkillVersion(src, v, false) } }, 'Activate');
          else if (state === 'active') action = el('span', { class: 'meta' }, 'live');
          else if (state === 'superseded') action = el('span', { class: 'meta', title: 'A newer commit or a change of the source settings replaced it' }, 'replaced');
          return el('tr', {},
            el('td', {}, el('a', { href: skillsCommitURL(src, v.sha), target: '_blank', rel: 'noopener' }, el('code', {}, skillsShort(v.sha)))),
            el('td', {}, el('span', { class: 'badge ' + ({ active: 'allowed', staged: 'pending', failed: 'denied', rejected: 'denied', retired: 'expired', superseded: 'expired' }[state] || '') }, state)),
            el('td', {}, String(v.skill_count || 0)),
            el('td', { class: v.status === 'failed' ? 'err' : 'meta' }, changes),
            el('td', {}, String((v.warnings || []).length)),
            el('td', { class: 'meta', title: v.decided_by ? 'decided by ' + v.decided_by : '' }, relTime(skillsMs(v.created_at))),
            el('td', {}, action));
        }))));
}

function renderSkillsWarnings(v) {
  const groups = skillsGroupWarnings(v.warnings);
  return el('div', { class: 'card' },
    el('h3', { style: 'margin-top: 0;' }, 'Warnings in ' + skillsShort(v.sha)),
    el('p', { class: 'meta' }, '"skip" means toolyard left the skill or file out; "warn" means it is served anyway.'),
    groups.map((g) => el('details', { 'data-disclosure-key': 'skills-warn-' + g.code },
      el('summary', {}, g.code + ' (' + g.items.length + ')'),
      el('table', {}, el('tbody', {}, g.items.slice(0, 200).map((w) => el('tr', {},
        el('td', {}, el('code', {}, w.path)), el('td', { class: 'meta' }, w.severity), el('td', {}, w.message))))))));
}

function renderSkillsSkills(src, skills, busy) {
  const effective = (s) => s.runs_override || s.runs || src.default_runs || 'anywhere';
  return el('div', { class: 'card' },
    el('h3', { style: 'margin-top: 0;' }, 'Skills'),
    el('p', { class: 'meta' }, 'A host skill needs the T3 host (its paths and tools); agents elsewhere see it only when they ask for host skills. An override here wins over the skill\'s own metadata.toolyard-runs and the source default. Shared with says who sees a skill: the source\'s audience, or its own.'),
    skills.length === 0 ? el('p', { class: 'meta' }, 'No skills in the live version.')
      : el('table', {},
        el('thead', {}, el('tr', {}, ['Name', 'Path', 'Runs', 'Override', 'Shared with', 'Reads (30 days)'].map((h) => el('th', {}, h)))),
        el('tbody', {}, skills.map((s) => el('tr', {},
          el('td', {}, s.name),
          el('td', {}, el('code', {}, s.path)),
          el('td', {}, effective(s)),
          el('td', {}, skillsSelect({ disabled: busy, 'aria-label': 'Where ' + s.name + ' runs', on: { change: (e) => setSkillRuns(src, s.path, e.target.value) } },
            [['', 'No override'], ['anywhere', 'Anywhere'], ['host', 'T3 host only']], s.runs_override || '')),
          el('td', {}, el('span', { class: 'row' }, el('span', {}, skillsSharingLabel(s)),
            el('button', { class: 'link', disabled: busy, 'aria-label': 'Change who sees ' + s.name, on: { click: () => openSkillsShare(src, s) } }, 'Edit'))),
          el('td', {}, String(s.reads_30d || 0)))))));
}

// renderSkillsShare is one skill's Sharing modal: follow the source, or
// its own audience, plus its own block list (on top of the source's).
function renderSkillsShare() {
  const sh = skillsUI.share, d = sh.draft, s = sh.skill, src = sh.src;
  const close = () => { skillsUI.share = null; render(); };
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget && !sh.saving) close(); } } },
    el('div', { class: 'modal', style: 'max-width: 640px; max-height: calc(100vh - 32px); overflow-y: auto;' },
      el('h3', {}, 'Who sees ' + s.name),
      el('p', { class: 'meta' }, src.slug + ' · ', el('code', {}, s.path), ' · the source is shared with ' + skillsAudienceText(src) + '.'),
      skillsGroup('Who sees this skill', el('div', {},
        skillsRadio('skills-skill-vis', d.mode === '', 'Same as the source', () => { d.mode = ''; }),
        skillsRadio('skills-skill-vis', d.mode === 'everyone', 'Everyone and their agents', () => { d.mode = 'everyone'; }),
        skillsRadio('skills-skill-vis', d.mode === 'members', 'Only these people', () => { d.mode = 'members'; }),
        skillsRadio('skills-skill-vis', d.mode === 'nobody', 'Nobody: only admins and the person who added the source, in the dashboard; agents never', () => { d.mode = 'nobody'; }),
        d.mode === 'members' ? renderSkillsMembers(d, 'memberFilter', 'members', 'skills-skill-members') : null)),
      skillsGroup('Block list', renderSkillsMembers(d, 'blockFilter', 'blocked', 'skills-skill-blocked'),
        'People on it never see this skill, on top of the source\'s block list. Admins always see everything. Same as the source with nobody blocked clears this skill\'s own setting.'),
      sh.error ? el('div', { class: 'err' }, sh.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { disabled: sh.saving, on: { click: close } }, 'Cancel'),
        el('button', { class: 'primary', disabled: sh.saving || (d.mode === 'members' && !d.members.length), on: { click: saveSkillsShare } }, sh.saving ? 'Saving…' : 'Save'))));
}

function renderSkillsWebhook(src, d) {
  const url = (d.secret && d.secret.payload_url) || skillsWebhookURL(src, location.origin);
  return el('details', { class: 'card', 'data-disclosure-key': 'skills-webhook' },
    el('summary', {}, el('b', {}, 'Webhook: update at once on every push'), src.has_webhook_secret ? el('span', { class: 'meta' }, ' · set up') : null),
    el('p', { class: 'meta' }, 'Without a webhook toolyard checks the branch every ' + Math.round((src.poll_seconds || 300) / 60) + ' minutes. With one, a push is live (or waiting for review) within seconds.'),
    el('ol', {},
      el('li', {}, 'Open ', el('a', { href: skillsRepoURL(src) + '/settings/hooks/new', target: '_blank', rel: 'noopener' }, 'Add webhook'), ' in the repository settings on GitHub.'),
      el('li', {}, 'Payload URL: ', el('code', {}, url), ' ', el('button', { on: { click: (e) => copyToButton(e.currentTarget, url) } }, 'Copy')),
      el('li', {}, 'Content type: ', el('code', {}, 'application/json'), '.'),
      el('li', {}, 'Secret: make one below and paste it. toolyard shows it only once.'),
      el('li', {}, 'Which events: ', el('b', {}, 'Just the push event'), '. Keep it active, then Add webhook. GitHub sends a ping that toolyard accepts.')),
    d.secret ? el('div', { class: 'skills-secret' },
      el('p', {}, 'New secret (shown once; it is gone when you leave this page):'),
      el('div', { class: 'row' }, el('code', { class: 'grow' }, d.secret.secret),
        el('button', { on: { click: (e) => copyToButton(e.currentTarget, d.secret.secret) } }, 'Copy'),
        el('button', { class: 'link', on: { click: () => { d.secret = null; render(); } } }, 'Hide'))) : null,
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { disabled: d.busy === 'secret', on: { click: () => rotateSkillSecret(src) } },
        d.busy === 'secret' ? 'Making…' : src.has_webhook_secret ? 'Rotate secret' : 'Make a secret')));
}

// renderSkillsMembers is a people picker: a filter and a checkbox per
// active person, editing the user-ID list target[listKey] ('members' by
// default; 'blocked' for a block list). idKey names the filter field, so
// two pickers on one page keep their own focus.
function renderSkillsMembers(target, filterKey, listKey, idKey) {
  listKey = listKey || 'members';
  if (!skillsUI.users) { loadSkillsUsers(); return el('p', { class: 'meta' }, 'Loading members…'); }
  const block = listKey === 'blocked';
  const chosen = target[listKey] || [];
  const f = (target[filterKey] || '').trim().toLowerCase();
  const users = skillsUI.users.filter((u) => !f || [u.display_name, u.username, u.email].some((x) => (x || '').toLowerCase().includes(f)));
  return el('div', { class: 'skills-members' },
    el('input', { id: (idKey || 'skills-member') + '-filter', type: 'search', placeholder: block ? 'Find a person to block' : 'Find a member', value: target[filterKey] || '',
      on: { input: (e) => { target[filterKey] = e.target.value; render(); } } }),
    el('div', { class: 'access-list', style: 'max-height: 30vh; overflow: auto;' }, users.map((u) => el('label', { class: 'row' },
      el('input', { type: 'checkbox', checked: chosen.includes(u.id), on: { change: (e) => {
        const now = target[listKey] || [];
        target[listKey] = e.target.checked ? [...new Set([...now, u.id])] : now.filter((x) => x !== u.id); render();
      } } }),
      el('span', {}, u.display_name || u.username || u.email || u.id),
      u.role !== 'member' ? el('span', { class: 'meta' }, block ? ' (admin: sees every skill anyway)' : ' (admin: sees every source anyway)') : null))),
    el('p', { class: 'meta' }, block
      ? (chosen.length ? skillsPeople(chosen.length) + ' blocked. They and their agents never see these skills.' : 'Nobody blocked.')
      : skillsCount(chosen.length, 'member') + ' chosen. Their agents see it too.'));
}

function renderSkillsWizard() {
  const w = skillsUI.wizard;
  const close = () => { skillsUI.wizard = null; render(); };
  const steps = [['url', 'Repository'], ['repo', 'Branch and folder'], ['review', 'Preview and settings'], ['done', 'Saved']];
  const at = steps.findIndex(([s]) => s === w.step);
  let body;
  if (w.step === 'url') body = el('div', {},
    skillsField('GitHub repository', el('input', { id: 'skills-wiz-url', autofocus: true, placeholder: 'owner/repo, or a github.com URL (…/tree/<branch>/<folder> works too)', value: w.url,
      on: { input: (e) => { w.url = e.target.value; skillsWizardRefreshNext(w); }, keydown: (e) => { if (e.key === 'Enter') skillsWizardInspect(); } } })),
    w.auth !== 'none' ? renderSkillsTokenField(w) : el('button', { class: 'link', on: { click: () => { w.auth = 'token'; render(); } } },
      skillsUI.githubConnection ? 'Private repository? Add a read-only token or use your GitHub connection' : 'Private repository? Add a read-only token'));
  else if (w.step === 'repo') body = renderSkillsWizardRepo(w);
  else if (w.step === 'review') body = renderSkillsWizardReview(w);
  else body = renderSkillsWizardDone(w);
  const next = w.step === 'url' ? el('button', { id: 'skills-wiz-next', class: 'primary', disabled: !skillsWizardCanInspect(w), on: { click: skillsWizardInspect } }, w.busy ? 'Inspecting…' : 'Inspect')
    : w.step === 'repo' ? el('button', { id: 'skills-wiz-next', class: 'primary', disabled: !skillsWizardCanPreview(w), on: { click: skillsWizardPreview } }, w.busy ? 'Reading the repository…' : 'Preview')
    : w.step === 'review' ? el('button', { id: 'skills-wiz-next', class: 'primary', disabled: !skillsWizardCanSave(w), on: { click: skillsWizardSave } }, w.busy ? 'Saving and syncing…' : 'Save')
    : el('button', { class: 'primary', on: { click: () => { const id = w.result && w.result.source && w.result.source.id; close(); if (id) navigate('skills/' + id); } } }, 'Open the source');
  const backTo = { repo: 'url', review: 'repo' }[w.step];
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget && !w.busy) close(); } } },
    el('div', { class: 'modal skills-wizard', style: 'max-width: 760px;' },
      el('h3', {}, 'Add a skill source'),
      el('ol', { class: 'skills-steps' }, steps.map(([s, label], i) => el('li', { class: i === at ? 'active' : i < at ? 'done' : '' }, label))),
      body,
      w.error ? el('div', { class: 'err' }, w.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        backTo ? el('button', { disabled: w.busy, on: { click: () => { w.step = backTo; w.error = ''; render(); } } }, 'Back') : null,
        w.step !== 'done' ? el('button', { disabled: w.busy, on: { click: close } }, 'Cancel') : null,
        next)));
}

function renderSkillsTokenField(w) {
  return el('div', {},
    skillsUI.githubConnection ? el('div', { class: 'row', style: 'margin-top: 10px;' },
      el('label', { class: 'row' }, el('input', { type: 'radio', name: 'skills-auth', checked: w.auth === 'token', on: { change: () => { skillsWizardSet(w, 'auth', 'token'); render(); } } }), 'A read-only token'),
      el('label', { class: 'row' }, el('input', { type: 'radio', name: 'skills-auth', checked: w.auth === 'github', on: { change: () => { skillsWizardSet(w, 'auth', 'github'); render(); } } }), 'Use my GitHub connection')) : null,
    w.auth === 'token' ? skillsField('Read-only token',
      el('input', { id: 'skills-wiz-token', type: 'password', autocomplete: 'new-password', placeholder: 'A fine-grained token with Contents: read on this repository', value: w.token,
        on: { input: (e) => { skillsWizardSet(w, 'token', e.target.value); skillsWizardRefreshNext(w); } } }),
      'Write-only: toolyard stores it sealed and never shows it again. It is used only to read this repository.') : null,
    w.auth === 'github' ? el('p', { class: 'meta' }, 'toolyard reads this repository with your own toolyard GitHub connection, now and on every sync. The toolyard GitHub App must be installed on it (Contents: read, Metadata: read).') : null);
}

function renderSkillsWizardRepo(w) {
  const info = w.info || {};
  const branches = skillsWizardBranches(w);
  const many = (info.branches || []).length > 15;
  return el('div', {},
    el('p', {}, el('a', { href: skillsRepoURL(info), target: '_blank', rel: 'noopener' }, info.owner + '/' + info.repo),
      info.private ? el('span', { class: 'badge pending', style: 'margin-left: 6px;' }, 'private') : null,
      info.trusted_owner ? el('span', { class: 'badge allowed', style: 'margin-left: 6px;' }, 'trusted owner') : null),
    many ? el('input', { id: 'skills-wiz-branch-filter', type: 'search', placeholder: 'Filter ' + info.branches.length + ' branches', value: w.branchFilter,
      on: { input: (e) => { w.branchFilter = e.target.value; render(); } } }) : null,
    skillsField('Branch', skillsSelect({ id: 'skills-wiz-branch', on: { change: (e) => { skillsWizardSet(w, 'branch', e.target.value); render(); } } },
      branches.map((b) => [b, b === info.default_branch ? b + ' (default)' : b]), w.branch),
      info.branches_more ? 'This repository has more branches than toolyard lists; type a URL with /tree/<branch> to pick another.' : null),
    skillsField('Folder', el('input', { id: 'skills-wiz-path', placeholder: 'the repository root', value: w.path, on: { input: (e) => { skillsWizardSet(w, 'path', e.target.value); skillsWizardRefreshNext(w); } } }),
      'Every folder under it that holds a SKILL.md becomes a skill.'),
    info.private || info.needs_token || w.auth !== 'none' ? renderSkillsTokenField(w) : null);
}

function renderSkillsWizardReview(w) {
  const p = w.preview || {};
  const groups = skillsGroupWarnings(p.warnings);
  const src = p.source || { owner: w.info && w.info.owner, repo: w.info && w.info.repo };
  const contributor = !skillsIsAdmin();
  const trusted = !!(w.info && w.info.trusted_owner);
  return el('div', {},
    el('div', { class: 'skills-preview' },
      el('p', {}, 'Commit ', el('a', { href: skillsCommitURL(src, p.sha), target: '_blank', rel: 'noopener' }, el('code', {}, skillsShort(p.sha))),
        ' on ', el('code', {}, w.branch), ': ', el('b', {}, skillsCount(p.skill_count, 'skill')), ', ' + skillsCount(p.file_count, 'file') + ', ' + skillsCount(groups.reduce((n, g) => n + g.items.length, 0), 'warning') + '.'),
      p.skill_count === 0 ? el('p', { class: 'err' }, 'No SKILL.md found under this folder. Check the branch and the folder.') : null,
      (p.sample || []).length ? el('details', { 'data-disclosure-key': 'skills-wiz-sample' }, el('summary', {}, 'Sample skills (' + p.sample.length + ')'),
        el('table', {}, el('tbody', {}, p.sample.map((s) => el('tr', {}, el('td', {}, el('code', {}, s.path)), el('td', { class: 'meta' }, s.description)))))) : null,
      groups.length ? el('details', { 'data-disclosure-key': 'skills-wiz-warnings' }, el('summary', {}, 'Warnings by kind'),
        groups.map((g) => el('div', {}, el('b', {}, g.code + ' (' + g.items.length + '): '), g.items.slice(0, 10).map((x) => el('code', { style: 'margin-right: 6px;' }, x.path)),
          g.items.length > 10 ? ' and ' + (g.items.length - 10) + ' more' : ''))) : null),
    skillsField('Updates', skillsSelect({ id: 'skills-wiz-mode', on: { change: (e) => { w.mode = e.target.value; render(); } } },
      skillsWizardModes(w, contributor), w.mode || 'review'),
      contributor ? (trusted ? 'The owner is in the trusted list, so automatic is allowed. With review, an admin approves each new commit.' : 'An admin approves each new commit before agents get it.')
        : trusted ? 'The owner is in the trusted list, so automatic is the default.' : 'The owner is not in the trusted list, so review is the default.'),
    skillsGroup('Who sees it', el('div', {},
      skillsRadio('skills-vis', w.visibility === 'everyone', 'Everyone and their agents', () => { w.visibility = 'everyone'; }),
      skillsRadio('skills-vis', w.visibility === 'members', 'Only the members I choose', () => { w.visibility = 'members'; }),
      skillsRadio('skills-vis', w.visibility === 'nobody', 'Nobody yet (only admins and me, in the dashboard)', () => { w.visibility = 'nobody'; }),
      w.visibility === 'members' ? renderSkillsMembers(w, 'memberFilter') : null),
      contributor ? 'Whatever you choose, agents see nothing of it until an admin approves the source.' : null),
    skillsField('Where its skills run by default', skillsSelect({ id: 'skills-wiz-runs', on: { change: (e) => { w.defaultRuns = e.target.value; } } },
      [['anywhere', 'Anywhere (Claude.ai, ChatGPT, laptops, the T3 host)'], ['host', 'The T3 host only (they use its paths and tools)']], w.defaultRuns),
      'A skill can say otherwise in metadata.toolyard-runs, and you can override one skill later.'));
}

function renderSkillsWizardDone(w) {
  const r = w.result || {};
  const src = r.source || {};
  return el('div', {},
    el('p', {}, el('b', {}, src.slug), r.existing ? ' was already added a moment ago; this is that source.' : ' was added.'),
    src.approval === 'pending' ? el('p', {}, 'Saved. It syncs now; agents see nothing of it until an admin approves it. The owner has the question in the Inbox.') : null,
    el('p', { class: src.last_error ? 'err' : '' }, skillsFirstSyncText(src)),
    el('p', { class: 'meta' }, 'Set up the webhook on the source page so pushes reach agents within seconds.'));
}

function renderSkillsEdit() {
  const ed = skillsUI.edit, d = ed.draft, src = ed.orig;
  const close = () => { skillsUI.edit = null; render(); };
  const admin = skillsIsAdmin();
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget && !ed.saving) close(); } } },
    el('div', { class: 'modal', style: 'max-width: 640px; max-height: calc(100vh - 32px); overflow-y: auto;' },
      el('h3', {}, 'Edit ' + src.slug),
      skillsBuiltin(src) ? el('p', { class: 'meta' }, 'The built-in toolyard skills: choose who sees them and where they run by default.') : [
      el('p', { class: 'meta' }, src.owner + '/' + src.repo + ' · a change of branch or folder takes effect on the next sync.'),
      skillsField('Branch', el('input', { id: 'skills-edit-branch', value: d.branch, on: { input: (e) => { d.branch = e.target.value; } } })),
      skillsField('Folder', el('input', { id: 'skills-edit-path', placeholder: 'the repository root', value: d.path, on: { input: (e) => { d.path = e.target.value; } } })),
      skillsField('Updates', skillsSelect({ id: 'skills-edit-mode', on: { change: (e) => { d.mode = e.target.value; render(); } } },
        skillsEditModes(src, !admin), d.mode), admin ? null : 'An admin approves each new commit; only an admin can pin a source.'),
      d.mode === 'pinned' && admin ? skillsField('Pinned commit', el('input', { id: 'skills-edit-pin', placeholder: 'full commit SHA (empty: the live commit)', value: d.pinned_sha, on: { input: (e) => { d.pinned_sha = e.target.value; } } })) : null,
      skillsField('Check every (seconds, at least 60)', el('input', { id: 'skills-edit-poll', type: 'number', min: 60, value: d.poll_seconds, on: { input: (e) => { d.poll_seconds = e.target.value; } } }))],
      skillsGroup('Who sees it', el('div', {},
        skillsRadio('skills-edit-vis', d.visibility === 'everyone', 'Everyone and their agents', () => { d.visibility = 'everyone'; }),
        skillsRadio('skills-edit-vis', d.visibility === 'members', 'Only the members I choose', () => { d.visibility = 'members'; }),
        skillsRadio('skills-edit-vis', d.visibility === 'nobody', 'Nobody (only admins and the person who added it, in the dashboard)', () => { d.visibility = 'nobody'; }),
        d.visibility === 'members' ? renderSkillsMembers(d, 'memberFilter') : null),
        'The block list is in the Sharing panel on the source page.'),
      skillsField('Default runs', skillsSelect({ id: 'skills-edit-runs', on: { change: (e) => { d.default_runs = e.target.value; } } },
        [['anywhere', 'Anywhere'], ['host', 'The T3 host only']], d.default_runs)),
      skillsBuiltin(src) ? null : [skillsField('Leave out (one glob per line)', el('textarea', { id: 'skills-edit-exclude', rows: 3, value: d.exclude, on: { input: (e) => { d.exclude = e.target.value; } } })),
      skillsField(src.has_token ? 'Replace the read-only token' : 'Read-only token (for a private repository)',
        el('input', { id: 'skills-edit-token', type: 'password', autocomplete: 'new-password', placeholder: src.has_token ? 'leave empty to keep the stored token' : '', value: d.token, on: { input: (e) => { d.token = e.target.value; } } }),
        'Write-only: stored sealed, never shown again.'),
      src.has_token ? el('label', { class: 'row' }, el('input', { type: 'checkbox', checked: d.clear_token, on: { change: (e) => { d.clear_token = e.target.checked; } } }), 'Remove the stored token (the repository is public now)') : null],
      ed.error ? el('div', { class: 'err' }, ed.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { disabled: ed.saving, on: { click: close } }, 'Cancel'),
        el('button', { class: 'primary', disabled: ed.saving || (d.visibility === 'members' && d.members.length === 0), on: { click: saveSkillsEdit } }, ed.saving ? 'Saving…' : 'Save'))));
}

// renderSkillsAccess is the GitHub access panel: the default read token
// sources without their own use, and the rate limit they read under.
function renderSkillsAccess() {
  const a = skillsUI.access;
  if (!skillsIsAdmin() || (!a.data && !a.error)) return null;
  const d = a.data || {};
  const rate = d.rate;
  const paused = d.paused_until && skillsMs(d.paused_until) > Date.now();
  const low = rate && rate.limit > 0 && rate.remaining === 0;
  return el('details', { class: 'card', 'data-disclosure-key': 'skills-github-access', open: !d.set || paused || low },
    el('summary', {}, el('b', {}, 'GitHub access'), el('span', { class: 'meta' }, d.set ? ' · default token set' : ' · no token: 60 calls an hour')),
    el('p', { class: 'meta' }, 'Sources without their own token read public repositories with this token. Without one, GitHub allows this server 60 calls an hour, and a few sources checked every five minutes use that up. ',
      'A fine-grained token with read access to public repositories is enough (or Contents: read for private ones). It is write-only: toolyard stores it sealed and never shows it again. A source\'s own token always wins.'),
    rate ? el('p', { class: low ? 'err' : '' }, 'Rate limit: ' + rate.remaining + ' of ' + rate.limit + ' calls left this hour, resets at ' + skillsClock(rate.reset) + '.') : null,
    d.rate_error ? el('p', { class: 'meta' }, 'Could not read the rate limit: ' + d.rate_error) : null,
    paused ? el('p', { class: 'err' }, 'GitHub rate limit reached until ' + skillsClock(d.paused_until) + ': sources without their own token wait until then' + (d.set ? '.' : ' — add a GitHub token here.')) : null,
    a.editing ? el('div', { class: 'row' },
      el('input', { id: 'skills-access-token', class: 'grow', type: 'password', autocomplete: 'new-password', placeholder: 'github_pat_… (read-only)',
        value: a.token, on: { input: (e) => { a.token = e.target.value; }, keydown: (e) => { if (e.key === 'Enter' && a.token.trim()) saveSkillsAccess(a.token.trim()); } } }),
      el('button', { class: 'primary', disabled: a.busy, on: { click: () => { if (a.token.trim()) saveSkillsAccess(a.token.trim()); } } }, a.busy ? 'Checking…' : 'Save'),
      el('button', { disabled: a.busy, on: { click: () => { a.editing = false; a.token = ''; render(); } } }, 'Cancel'))
      : el('div', { class: 'row' },
        el('button', { on: { click: () => { a.editing = true; render(); } } }, d.set ? 'Replace token' : 'Set token'),
        d.set ? el('button', { class: 'danger', disabled: a.busy, on: { click: () => saveSkillsAccess('') } }, 'Clear') : null),
    a.error ? el('div', { class: 'err' }, a.error) : null);
}

// renderSkillsContributors is the Skill contributors panel (admins only):
// the members who may add skill sources, a Remove button each, and an Add
// control that offers active members who are not contributors yet.
function renderSkillsContributors() {
  const c = skillsUI.contrib;
  if (!skillsIsAdmin() || (!skillsUI.contributors && !c.error)) return null;
  const list = skillsUI.contributors || [];
  const people = skillsUI.users;
  if (!people) loadSkillsUsers();
  const taken = new Set(list.map((x) => x.user_id));
  const candidates = (people || []).filter((u) => u.role === 'member' && !taken.has(u.id));
  return el('details', { class: 'card', 'data-disclosure-key': 'skills-contributors' },
    el('summary', {}, el('b', {}, 'Skill contributors'), el('span', { class: 'meta' }, list.length ? ' · ' + skillsPeople(list.length) : ' · none yet')),
    el('p', { class: 'meta' }, 'Members you name here can add skill sources. Their sources wait for an admin\'s approval before agents see them. They can change only their own sources.'),
    list.length ? el('table', {}, el('tbody', {}, list.map((x) => el('tr', {},
      el('td', {}, skillsPersonName(x.user_id)),
      el('td', { class: 'meta' }, 'added ' + relTime(skillsMs(x.added_at)) + (x.added_by ? ' by ' + skillsPersonName(x.added_by) : '')),
      el('td', {}, el('button', { disabled: c.busy, 'aria-label': 'Remove ' + skillsPersonName(x.user_id), on: { click: () => removeSkillsContributor(x.user_id) } }, 'Remove'))))))
      : el('p', { class: 'meta' }, 'No contributors yet: only admins add skill sources.'),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      people ? skillsSelect({ id: 'skills-contrib-pick', 'aria-label': 'Member to add as a skill contributor', on: { change: (e) => { c.pick = e.target.value; render(); } } },
        [['', candidates.length ? 'Choose a member' : 'No other members to add']].concat(candidates.map((u) => [u.id, u.display_name || u.username || u.email || u.id])), c.pick)
        : el('span', { class: 'meta' }, 'Loading members…'),
      el('button', { class: 'primary', disabled: c.busy || !c.pick, on: { click: () => addSkillsContributor(c.pick) } }, c.busy ? 'Saving…' : 'Add')),
    c.error ? el('div', { class: 'err' }, c.error) : null);
}
