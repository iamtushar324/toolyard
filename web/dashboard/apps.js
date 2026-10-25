// Connected apps: the MCP clients (ChatGPT, Claude.ai, Claude Code, Codex,
// Cursor) a person added by URL and signed in through OAuth, and the
// per-agent scope editor that every agent shares (an app is an agent too).
//
// Loaded before app.js. Nothing here runs at load time: app.js calls
// viewApps / loadApps from its router and appsResumeOAuth after sign-in, and
// these functions use app.js's helpers (el, api, toast, render, relTime,
// state, navigate) only when called.

const APPS_DOCS_URL = 'https://github.com/iamtushar324/toolyard/blob/main/docs/guidelines/connect-clients.md';
const appsUI = { loading: false, loaded: false, error: '', apps: [], all: false, config: null, editor: null, audit: null, settings: null, busy: '' };

// ---- pure helpers (scripts/apps-ui.test.cjs) ---------------------------------

function appsCount(n, noun) { return n === 1 ? '1 ' + noun : n + ' ' + noun + 's'; }

// appsScopeSummary says in a few words what a scope allows.
function appsScopeSummary(scope) {
  const s = scope || {};
  const allowS = (s.allow_servers || []).length, allowT = (s.allow_tools || []).length;
  const denyS = (s.deny_servers || []).length, denyT = (s.deny_tools || []).length;
  let out = s.mode === 'listed' ? 'Only ' + appsCount(allowS, 'server') + ' and ' + appsCount(allowT, 'tool') : 'Everything its owner can use';
  if (denyS + denyT > 0) out += ', except ' + appsCount(denyS, 'blocked server') + ' and ' + appsCount(denyT, 'blocked tool');
  return out;
}

// appsDraftFromScope turns a stored scope into the editor's sets.
function appsDraftFromScope(scope) {
  const s = scope || {};
  const tools = (list) => new Set((list || []).map((t) => t.server + '\u0000' + t.tool));
  return {
    mode: s.mode === 'listed' ? 'listed' : 'all',
    allowServers: new Set(s.allow_servers || []), denyServers: new Set(s.deny_servers || []),
    allowTools: tools(s.allow_tools), denyTools: tools(s.deny_tools),
    filter: '',
  };
}

// appsScopeFromDraft is the request body's scope. Allows only count in
// "listed" mode, so they are left out in "all" mode.
function appsScopeFromDraft(d) {
  const tools = (set) => [...set].sort().map((k) => { const [server, tool] = k.split('\u0000'); return { server, tool }; });
  return {
    mode: d.mode,
    allow_servers: d.mode === 'listed' ? [...d.allowServers].sort() : [],
    allow_tools: d.mode === 'listed' ? tools(d.allowTools) : [],
    deny_servers: [...d.denyServers].sort(),
    deny_tools: tools(d.denyTools),
  };
}

// appsResumeOAuth sends a person who signed in with a password back to the
// MCP client's consent screen they came from (/?password=1&oauth_request=…).
function appsResumeOAuth(loc) {
  const where = loc || location;
  const id = new URLSearchParams(where.search).get('oauth_request');
  if (!/^[A-Za-z0-9_-]{43}$/.test(id || '')) return false;
  where.replace('/oauth/consent?request=' + encodeURIComponent(id));
  return true;
}

function appsKindLabel(kind) {
  return kind === 'cimd' ? 'Verified by its URL' : 'Self-registered';
}

// ---- data --------------------------------------------------------------------

async function loadApps() {
  if (appsUI.loading) return;
  appsUI.loading = true; appsUI.error = '';
  try {
    const [list, config] = await Promise.all([
      api('/v1/me/apps' + (appsUI.all ? '?all=true' : '')),
      api('/v1/me/apps/config'),
    ]);
    appsUI.apps = (list && list.apps) || [];
    appsUI.config = config || null;
    if (config && config.settings && !appsUI.settings) appsUI.settings = Object.assign({}, config.settings);
    appsUI.loaded = true;
  } catch (e) {
    appsUI.error = e.message || 'Could not load connected apps.';
  } finally {
    appsUI.loading = false;
    render();
  }
}

async function renameApp(app) {
  const nickname = prompt('New nickname for this connection (letters, digits, spaces and . _ : ( ) -):', app.nickname);
  if (nickname == null || nickname.trim() === '' || nickname.trim() === app.nickname) return;
  try {
    await api('/v1/me/apps/rename', { method: 'POST', body: { id: app.id, nickname: nickname.trim() } });
    toast('Renamed to ' + nickname.trim() + '.');
    await loadApps();
  } catch (e) { toast(e.message, 'error'); }
}

async function revokeApp(app) {
  if (!confirm(`Revoke ${app.nickname}? ${app.client_name} loses access at once and its agent is disabled. To use it again, connect it again from the app.`)) return;
  try {
    await api('/v1/me/apps/revoke', { method: 'POST', body: { id: app.id } });
    toast(app.nickname + ' was revoked.');
    await loadApps();
  } catch (e) { toast(e.message, 'error'); }
}

// openAppActivity shows an app's audit rows: on the Activity page for an
// admin, in a list here for a member (who has no Activity page).
async function openAppActivity(app) {
  if (isAdmin()) {
    state.auditFilter.agent = app.agent_id;
    navigate('audit');
    return;
  }
  appsUI.audit = { app, rows: null, error: '' };
  render();
  try {
    const out = await api('/v1/me/apps/audit?id=' + encodeURIComponent(app.id) + '&limit=50');
    appsUI.audit.rows = (out && out.events) || [];
  } catch (e) { appsUI.audit.error = e.message; }
  render();
}

// openScopeEditor edits an app's scope (target.appId) or any agent's
// (target.agentId).
async function openScopeEditor(target) {
  appsUI.editor = Object.assign({ servers: null, error: '', saving: false, open: {} }, target, { draft: appsDraftFromScope(target.scope) });
  render();
  try {
    const q = target.appId ? 'id=' + encodeURIComponent(target.appId) : 'agent_id=' + encodeURIComponent(target.agentId);
    const [catalog, current] = await Promise.all([
      api('/v1/me/apps/catalog?' + q),
      target.appId ? Promise.resolve(null) : api('/v1/me/agent-scopes?agent_id=' + encodeURIComponent(target.agentId)),
    ]);
    if (!appsUI.editor) return;
    appsUI.editor.servers = (catalog && catalog.servers) || [];
    if (current && current.scope) appsUI.editor.draft = appsDraftFromScope(current.scope);
  } catch (e) {
    if (appsUI.editor) appsUI.editor.error = e.message;
  }
  render();
}

async function saveScopeEditor() {
  const ed = appsUI.editor;
  if (!ed || ed.saving) return;
  ed.saving = true; ed.error = ''; render();
  const scope = appsScopeFromDraft(ed.draft);
  try {
    if (ed.appId) await api('/v1/me/apps/scope', { method: 'POST', body: { id: ed.appId, scope } });
    else await api('/v1/me/agent-scopes', { method: 'POST', body: { agent_id: ed.agentId, scope } });
    appsUI.editor = null;
    toast('Access saved: ' + appsScopeSummary(scope) + '.');
    await loadApps();
  } catch (e) {
    ed.saving = false; ed.error = e.message; render();
  }
}

async function saveAppsSettings() {
  const s = appsUI.settings;
  if (!s) return;
  appsUI.busy = 'settings'; render();
  try {
    await api('/v1/settings', { method: 'PATCH', body: {
      mcp_oauth_enabled: !!s.mcp_oauth_enabled,
      mcp_oauth_redirect_hosts: String(s.mcp_oauth_redirect_hosts || '').trim(),
      mcp_oauth_access_ttl_minutes: Number(s.mcp_oauth_access_ttl_minutes) || 60,
      mcp_oauth_refresh_ttl_days: Number(s.mcp_oauth_refresh_ttl_days) || 30,
    } });
    appsUI.settings = null;
    toast('OAuth settings saved.');
    await loadApps();
  } catch (e) { toast(e.message, 'error'); }
  finally { appsUI.busy = ''; render(); }
}

// ---- views -------------------------------------------------------------------

function viewApps() {
  if (!appsUI.loaded && !appsUI.loading && !appsUI.error) loadApps();
  const cfg = appsUI.config || {};
  const mcpURL = cfg.mcp_url || (location.origin + '/mcp');
  return el('div', {},
    appsUI.editor ? renderScopeEditor() : null,
    appsUI.audit ? renderAppActivity() : null,
    el('div', { class: 'card' },
      el('h2', { style: 'margin-top: 0;' }, 'Connected apps'),
      el('p', { class: 'meta' }, 'Apps you added to toolyard by URL: ChatGPT, Claude.ai, Claude Code, Codex, Cursor and other MCP clients. Each one signs in as you through OAuth and acts as its own agent, with the nickname and tool access you chose.'),
      cfg.enabled === false
        ? el('p', { class: 'err' }, cfg.public_url ? 'OAuth for MCP clients is turned off on this toolyard. An admin can turn it on below.' : 'OAuth for MCP clients needs a public HTTPS address: start toolyard with -public-url.')
        : el('div', {},
            el('p', {}, 'To connect an app, add this MCP server URL in it and sign in when it asks:'),
            el('div', { class: 'row' }, el('code', { class: 'grow' }, mcpURL),
              el('button', { on: { click: (e) => copyToButton(e.currentTarget, mcpURL) } }, 'Copy')),
            el('p', { class: 'meta' }, el('a', { href: APPS_DOCS_URL, target: '_blank', rel: 'noopener' }, 'Step-by-step for each app'))),
      isAdmin() ? el('label', { class: 'row', style: 'margin-top: 8px;' },
        el('input', { type: 'checkbox', checked: appsUI.all, on: { change: (e) => { appsUI.all = e.target.checked; appsUI.loaded = false; loadApps(); } } }),
        el('span', {}, "Show everyone's apps")) : null,
    ),
    appsUI.error ? el('div', { class: 'card err' }, appsUI.error) : null,
    !appsUI.loaded && appsUI.loading ? el('div', { class: 'card empty' }, 'Loading…')
      : appsUI.apps.length === 0 ? el('div', { class: 'card empty' }, 'No connected apps yet.')
      : appsUI.apps.map(renderAppCard),
    renderAgentLimitsCard(),
    isAdmin() && appsUI.settings ? renderAppsSettings() : null,
  );
}

function renderAppCard(app) {
  const active = app.status === 'active';
  return el('div', { class: 'card', style: active ? '' : 'opacity: 0.7;' },
    el('div', { class: 'row' },
      el('h3', { class: 'grow', style: 'margin: 0;' }, app.nickname),
      el('span', { class: 'badge ' + (active ? 'allowed' : 'denied') }, app.status)),
    el('div', { class: 'meta' }, app.client_name + ' · ' + appsKindLabel(app.client_kind) +
      (app.redirect_hosts && app.redirect_hosts.length ? ' · returns to ' + app.redirect_hosts.join(', ') : '')),
    appsUI.all ? el('div', { class: 'meta' }, 'Owner: ' + (app.owner_name || app.owner_email || app.owner_id)) : null,
    el('div', { class: 'meta' }, 'Connected ' + relTime(app.created_at) + ' · last used ' + (app.last_used_at ? relTime(app.last_used_at) : 'never') +
      (app.revoked_at ? ' · revoked ' + relTime(app.revoked_at) : '')),
    el('p', {}, 'Access: ' + (app.scope_summary || appsScopeSummary(app.scope))),
    el('div', { class: 'meta' }, 'Client ID: ', el('code', {}, app.client_id), ' · Agent: ', el('code', {}, app.agent_id)),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      active ? el('button', { on: { click: () => renameApp(app) } }, 'Rename') : null,
      active ? el('button', { on: { click: () => openScopeEditor({ appId: app.id, name: app.nickname, scope: app.scope }) } }, 'Change access') : null,
      el('button', { on: { click: () => openAppActivity(app) } }, 'Activity'),
      active ? el('button', { class: 'danger', on: { click: () => revokeApp(app) } }, 'Revoke') : null),
  );
}

// renderAgentLimitsCard lets a person limit their ordinary agents the same
// way (the agents that are not connected apps).
function renderAgentLimitsCard() {
  const appAgents = new Set(appsUI.apps.map((a) => a.agent_id));
  const mine = state.user ? state.user.id : '';
  const agents = (state.agents || []).filter((a) => !appAgents.has(a.id) && a.kind !== 'identity' && (!a.owner || a.owner === mine || isAdmin()));
  if (!agents.length) return null;
  return el('div', { class: 'card' },
    el('h3', { style: 'margin-top: 0;' }, 'Limit your other agents'),
    el('p', { class: 'meta' }, 'Every agent can use what its owner can use. Narrow one agent to chosen servers and tools, or block some.'),
    el('table', {}, el('tbody', {}, agents.map((a) => el('tr', {},
      el('td', {}, a.name), el('td', {}, el('code', {}, a.id)),
      el('td', {}, el('button', { on: { click: () => openScopeEditor({ agentId: a.id, name: a.name, scope: null }) } }, 'Change access')))))),
  );
}

function renderScopeEditor() {
  const ed = appsUI.editor, d = ed.draft;
  const close = () => { appsUI.editor = null; render(); };
  const toggle = (set, key, on) => { if (on) set.add(key); else set.delete(key); };
  const box = (checked, onChange, label, disabled) => el('label', { class: 'access-opt' },
    el('input', { type: 'checkbox', checked, disabled: !!disabled, on: { change: (e) => { onChange(e.target.checked); render(); } } }), label);
  const filter = d.filter.trim().toLowerCase();
  const servers = (ed.servers || []).map((g) => ({ g, tools: filter ? g.tools.filter((t) => t.name.toLowerCase().includes(filter) || g.name.toLowerCase().includes(filter)) : g.tools }))
    .filter((x) => !filter || x.tools.length);
  const listed = d.mode === 'listed';
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) close(); } } },
    el('div', { class: 'modal', style: 'max-width: 760px;' },
      el('h3', {}, 'Change access · ' + ed.name),
      el('p', { class: 'meta' }, 'Applies on top of what the owner can use. Search, the Inbox and the other always-on tools stay available. Calls that need approval still need it.'),
      el('div', {},
        el('label', { class: 'row' }, el('input', { type: 'radio', name: 'apps-mode', checked: !listed, on: { change: () => { d.mode = 'all'; render(); } } }), 'Everything the owner can use'),
        el('label', { class: 'row' }, el('input', { type: 'radio', name: 'apps-mode', checked: listed, on: { change: () => { d.mode = 'listed'; render(); } } }), 'Only the servers and tools I allow')),
      el('input', { id: 'apps-scope-filter', type: 'search', placeholder: 'Find a server or tool', value: d.filter, on: { input: (e) => { d.filter = e.target.value; render(); } } }),
      ed.servers === null && !ed.error ? el('p', { class: 'meta' }, 'Loading tools…') : null,
      el('div', { class: 'access-list', style: 'max-height: 50vh; overflow: auto;' }, servers.map(({ g, tools }) => {
        const open = !!ed.open[g.name] || !!filter;
        return el('div', { class: 'access-section' },
          el('div', { class: 'row' },
            el('button', { class: 'link', on: { click: () => { ed.open[g.name] = !ed.open[g.name]; render(); } } }, (open ? '▾ ' : '▸ ') + g.name),
            el('span', { class: 'meta grow' }, appsCount(g.tools.length, 'tool')),
            box(d.allowServers.has(g.name), (on) => toggle(d.allowServers, g.name, on), 'Allow all', !listed),
            box(d.denyServers.has(g.name), (on) => toggle(d.denyServers, g.name, on), 'Block')),
          open ? tools.map((t) => {
            const key = g.name + '\u0000' + t.name;
            return el('div', { class: 'row', style: 'padding-left: 18px;' },
              el('code', { class: 'grow', title: t.description || '' }, t.name),
              box(d.allowTools.has(key), (on) => toggle(d.allowTools, key, on), 'Allow', !listed),
              box(d.denyTools.has(key), (on) => toggle(d.denyTools, key, on), 'Block'));
          }) : null);
      })),
      el('p', { class: 'meta' }, 'Result: ' + appsScopeSummary(appsScopeFromDraft(d))),
      ed.error ? el('div', { class: 'err' }, ed.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { disabled: ed.saving, on: { click: close } }, 'Cancel'),
        el('button', { class: 'primary', disabled: ed.saving || ed.servers === null, on: { click: saveScopeEditor } }, ed.saving ? 'Saving…' : 'Save')),
    ),
  );
}

function renderAppActivity() {
  const a = appsUI.audit;
  const close = () => { appsUI.audit = null; render(); };
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) close(); } } },
    el('div', { class: 'modal', style: 'max-width: 760px;' },
      el('h3', {}, 'Activity · ' + a.app.nickname),
      a.error ? el('div', { class: 'err' }, a.error)
        : a.rows === null ? el('p', { class: 'meta' }, 'Loading…')
        : a.rows.length === 0 ? el('p', { class: 'meta' }, 'No activity yet.')
        : el('table', {}, el('tbody', {}, a.rows.map((r) => el('tr', {},
            el('td', { class: 'meta' }, relTime(r.ts)),
            el('td', {}, r.event_type),
            el('td', {}, r.tool_name || ''),
            el('td', {}, r.decision || ''),
            el('td', { class: 'meta' }, r.reason || r.result_summary || ''))))),
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' }, el('button', { on: { click: close } }, 'Close')),
    ),
  );
}

function renderAppsSettings() {
  const s = appsUI.settings;
  const field = (label, input) => el('label', { style: 'display: block; margin-top: 8px;' }, el('div', {}, label), input);
  return el('div', { class: 'card' },
    el('h3', { style: 'margin-top: 0;' }, 'OAuth for MCP clients (admin)'),
    el('label', { class: 'row' },
      el('input', { type: 'checkbox', checked: !!s.mcp_oauth_enabled, on: { change: (e) => { s.mcp_oauth_enabled = e.target.checked; } } }),
      el('span', {}, 'Let MCP clients sign in with OAuth')),
    field('Allowed client redirect hosts (empty: any). Comma separated, e.g. claude.ai, chatgpt.com, localhost, cursor:',
      el('input', { value: s.mcp_oauth_redirect_hosts || '', on: { input: (e) => { s.mcp_oauth_redirect_hosts = e.target.value; } } })),
    field('Access token lifetime (minutes, 5–1440)',
      el('input', { type: 'number', min: 5, max: 1440, value: s.mcp_oauth_access_ttl_minutes, on: { input: (e) => { s.mcp_oauth_access_ttl_minutes = e.target.value; } } })),
    field('Refresh token lifetime (days, 1–365, renewed on every use)',
      el('input', { type: 'number', min: 1, max: 365, value: s.mcp_oauth_refresh_ttl_days, on: { input: (e) => { s.mcp_oauth_refresh_ttl_days = e.target.value; } } })),
    el('div', { class: 'row', style: 'margin-top: 12px; justify-content: flex-end;' },
      el('button', { class: 'primary', disabled: appsUI.busy === 'settings', on: { click: saveAppsSettings } }, appsUI.busy === 'settings' ? 'Saving…' : 'Save settings')),
  );
}
