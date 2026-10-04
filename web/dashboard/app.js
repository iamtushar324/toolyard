// toolyard dashboard — vanilla SPA. No build step.
//
// Single mutable `state` object. `render()` redraws the screen for the current
// route. SSE feed reconciles approvals + audit live.

const state = {
  user: null,              // /v1/auth/me; user.role 'admin'|'member' drives the shell
  setupRequired: false,
  // /v1/auth/config: { clerk: { publishable_key, frontend_api } | null, password_login }.
  // null when the fetch failed, which falls back to the password form.
  authConfig: null,
  showPasswordLogin: false, // login card: password form revealed under "Sign in with Google"
  myServers: [],           // /v1/me/servers: the groups the signed-in user may use
  // My connections: /v1/me/connections rows for the signed-in person, plus
  // the server whose sign-in they just started (polled until it lands).
  connections: { rows: [], loaded: false, loading: false, error: '', busy: '', pending: null, pendingSince: 0 },
  serverConnectionsModal: null, // { name, loading, data, error } while an admin looks at who signed in to a server
  // Users page (admin): /v1/users rows + the grantable groups.
  users: { rows: [], groups: [], loaded: false, loading: false, error: '', keyBusy: '', keyOpen: {} },
  userAccessModal: null,   // { id, name, selected: {group: bool}, dropped: [], error, saving } while editing access
  // "Your Beknown key" card: /v1/me/identity-key status, plus the raw key
  // held only in memory from a reveal until the owner dismisses it.
  myKey: freshMyKey(),
  serverEditModal: null,   // { name, http, url, headersText, identityOn, header, register, enabled, orig, error, saving } while editing a server
  route: 'inbox',
  approvals: [],
  recentApprovals: [],     // /v1/approvals: the latest decided rows, for "Recently decided"
  audit: [],
  agents: [],
  servers: [],
  tools: [],
  marketplace: [],
  marketModal: null,
  agentModal: null,        // { stage: 'name'|'done', name, agent, snippetTab }
  toolFilter: '',
  // Audit tab: client-side filters + pagination cursor state. owner,
  // decided_by, client_kind and session also go to the server
  // (AUDIT_SERVER_FILTERS); sessionDraft is the session box while typing.
  auditFilter: { q: '', event_type: '', decision: '', agent: '', owner: '', decided_by: '', client_kind: '', session: '', sessionDraft: null },
  auditOpen: {},          // audit row id -> "Raised by" details toggle open
  auditLoading: false,    // a server-filtered reload is in flight
  auditPaged: false,      // true once "Load older" has pulled extra rows
  auditEnd: false,        // true when a "Load older" returned nothing
  settings: { surface_mode: 'full', top_n_count: 20, top_n_personalize_after: 100, router_only_mode: false },
  usage: { per_tool: {}, rows: [] },
  workbench: {
    selected: null,    // tool name
    inputs: {},        // toolName -> { fieldKey: value }
    rawJSON: {},       // toolName -> string when in raw JSON mode
    rawMode: {},       // toolName -> bool
    result: null,      // last call result/error
    running: false,
  },
  insights: {
    range: '7d',
    overview: null,
    tools: [],
    agents: [],
    autoRules: [],
    loading: false,
  },
  anomalies: [],
  policies: [],            // explicit tool/upstream policies (Feature A)
  enrollment: null,
  errors: {},
  notice: '',
  // SSE health. status: connecting | live | reconnecting | offline.
  stream: { status: 'connecting', lastEventAt: 0, wasDown: false },
  vapidKey: null,
  pushReady: false,
  // oauthStatus is keyed by upstream name -> {has_client, has_token, state,
  // scopes, redirect_uri, device_supported, last_error, ...}.
  oauthStatus: {},
  // oauthFlow is the in-progress OAuth dance the user just kicked off.
  oauthFlow: null, // { name, mode, state, authorize_url, paste:'', error:'' }
  pushDiag: null,        // /v1/push/diag last response
  pushTestResult: null,  // /v1/push/test last response
  jwtPreview: null,      // /v1/push/jwt-preview last response
  moreSheet: false,      // bottom-nav "More" sheet open?
  // Secrets broker (Settings card): metadata only, never values.
  secrets: [],
  secretsLoaded: false,
  // Chat-notification channel status (Settings card).
  chat: null,
  chatLoaded: false,
};

// ---- helpers ----------------------------------------------------------------

const $ = (id) => document.getElementById(id);

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const k in attrs) {
    if (k === 'class') node.className = attrs[k];
    else if (k === 'on') for (const ev in attrs.on) node.addEventListener(ev, attrs.on[ev]);
    else if (k === 'html') node.innerHTML = attrs[k];
    else if (k in node) {
      try { node[k] = attrs[k]; } catch { node.setAttribute(k, attrs[k]); }
    } else node.setAttribute(k, attrs[k]);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    node.appendChild(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

async function api(path, opts = {}) {
  const init = { credentials: 'include', ...opts };
  // X-Requested-With is the dashboard's anti-CSRF tell — the gateway's
  // HardenAPI middleware refuses cookie-authenticated mutations without
  // it. Custom headers can't be set by cross-site form submissions, so
  // this is a robust supplementary defense to SameSite + Origin checks.
  init.headers = { 'X-Requested-With': 'toolyard', ...(init.headers || {}) };
  if (init.body && typeof init.body !== 'string') {
    init.body = JSON.stringify(init.body);
    init.headers = { 'Content-Type': 'application/json', ...init.headers };
  }
  const r = await fetch(path, init);
  let body = null;
  try { body = await r.json(); } catch (_) {}
  if (!r.ok) {
    const err = new Error((body && body.error) || `HTTP ${r.status}`);
    err.status = r.status;
    err.body = body; // on 409 this is the current approval row
    throw err;
  }
  return body;
}

function relTime(ms) {
  if (!ms) return '';
  const diff = Date.now() - ms;
  const abs = Math.abs(diff);
  const sec = Math.floor(abs / 1000);
  if (sec < 60) return `${sec}s ${diff > 0 ? 'ago' : 'from now'}`;
  if (sec < 3600) return `${Math.floor(sec/60)}m ${diff > 0 ? 'ago' : 'from now'}`;
  if (sec < 86400) return `${Math.floor(sec/3600)}h ${diff > 0 ? 'ago' : 'from now'}`;
  return new Date(ms).toLocaleString();
}

function badge(status) {
  return el('span', { class: `badge ${status}` }, status);
}

function toast(msg, type = 'info') {
  document.querySelectorAll('.toast').forEach((n) => n.remove());
  const t = el('div', { class: 'toast ' + (type === 'error' ? 'error' : '') }, msg);
  document.body.appendChild(t);
  setTimeout(() => { t.style.opacity = '0'; setTimeout(() => t.remove(), 300); }, 3500);
}

// staleNote shows a neutral, dismissible banner — used when an approval was
// already decided (possibly on another device) or expired, so the operator
// gets honest feedback instead of a false success/error toast.
function staleNote(msg) {
  document.querySelectorAll('.toast').forEach((n) => n.remove());
  const t = el('div', { class: 'toast stale' },
    el('span', { class: 'grow' }, msg),
    el('button', { class: 'link', on: { click: () => t.remove() } }, 'Dismiss'),
  );
  document.body.appendChild(t);
}

// ---- roles -----------------------------------------------------------------

// isAdmin: members only manage their own agents; everything else is admin.
// A user row without a role (older server) keeps today's full dashboard.
function isAdmin() {
  return !!state.user && state.user.role !== 'member';
}

const MEMBER_ROUTES = ['inbox', 'agents', 'myservers', 'connections'];

function defaultRoute() {
  return 'inbox';
}

function routeAllowed(route) {
  return isAdmin() || MEMBER_ROUTES.includes(route);
}

function userLabel(u) {
  return (u && (u.display_name || u.username || u.email || u.id)) || '';
}

// userAvatar is the account picture, or the first letter of the name when
// there's no picture (or it fails to load).
// Avatar URLs that failed once (blocked, 404) go straight to the initial on
// later renders instead of re-requesting on every redraw.
const failedAvatars = new Set();
function userAvatar(u, big) {
  const cls = 'user-av' + (big ? ' big' : '');
  const initial = () => el('span', { class: cls, 'aria-hidden': 'true' }, (userLabel(u).trim()[0] || '?').toUpperCase());
  if (!u || !u.avatar_url || failedAvatars.has(u.avatar_url)) return initial();
  return el('img', {
    class: cls, referrerpolicy: 'no-referrer', alt: '', src: u.avatar_url,
    on: { error: (e) => { failedAvatars.add(u.avatar_url); e.target.replaceWith(initial()); } },
  });
}

// toMs normalises the users table's integer timestamps (ms, or seconds from
// older rows) and ISO strings to epoch milliseconds for relTime().
function toMs(v) {
  if (!v) return 0;
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  const t = Date.parse(v);
  return isNaN(t) ? 0 : t;
}

// ---- bootstrap -------------------------------------------------------------

async function loadAuthConfig() {
  try { state.authConfig = await api('/v1/auth/config'); } catch (_) { state.authConfig = null; }
}

async function refreshUser() {
  try {
    const me = await api('/v1/auth/me');
    if (me && me.setup_required) {
      state.setupRequired = true;
      state.user = null;
    } else if (me && me.id) {
      state.user = me;
      state.setupRequired = false;
    } else {
      state.user = null;
    }
  } catch (e) {
    state.user = null;
  }
}

async function loadAll() {
  if (!state.user) return;
  // Fire-and-forget: the "Your Beknown key" card fills in once it lands.
  loadMyKey();
  // Members only reach their own agents and granted servers; every other
  // route would answer 403 admin_only.
  if (!isAdmin()) {
    try {
      const [agents, mine] = await Promise.all([
        api('/v1/agents'),
        api('/v1/me/servers').catch(() => []),
      ]);
      state.agents = agents || [];
      state.myServers = normGroups(mine);
    } catch (e) {
      toast(e.message, 'error');
    }
    loadInbox();
    return;
  }
  try {
    const [pendings, audits, agents, servers, tools, market, settingsRes, usageRes, vapid, policies, recent] = await Promise.all([
      api('/v1/approvals?status=pending'),
      api('/v1/audit?' + auditServerParams({ limit: '50' }).toString()),
      api('/v1/agents'),
      api('/v1/servers').catch(() => []),
      api('/v1/tools').catch(() => []),
      api('/v1/marketplace').catch(() => null),
      api('/v1/settings').catch(e => { state.settingsError = e.message; return {}; }),
      api('/v1/usage').catch(() => ({ per_tool: {}, rows: [] })),
      api('/v1/push/vapid_key').catch(() => null),
      api('/v1/policies').catch(() => []),
      api('/v1/approvals?limit=20').catch(() => []),
    ]);
    state.policies = policies || [];
    state.settings = Object.assign({}, state.settings, settingsRes || {});
    state.usage = usageRes || state.usage;
    state.approvals = pendings || [];
    state.recentApprovals = (Array.isArray(recent) ? recent : []).filter((a) => a && a.status && a.status !== 'pending');
    state.audit = audits || [];
    state.agents = agents || [];
    state.servers = servers || [];
    state.tools = tools || [];
    // /v1/marketplace returns { entries, oauth_redirect_uri }; tolerate the
    // legacy bare-array shape so a stale dashboard doesn't blank the tab.
    state.marketplace = Array.isArray(market) ? market : ((market && market.entries) || []);
    state.oauthRedirectURI = (market && market.oauth_redirect_uri) || '';
    state.vapidKey = vapid && vapid.public_key ? vapid.public_key : null;
    // Fire-and-forget — the badges fill in once the responses land.
    preloadOAuthStatus().then(() => render()).catch(() => {});
    loadInbox();
    ensureUserDir();
  } catch (e) {
    toast(e.message, 'error');
  }
}

async function reloadServers() {
  try {
    const [servers, tools] = await Promise.all([
      api('/v1/servers').catch(() => []),
      api('/v1/tools').catch(() => []),
    ]);
    state.servers = servers || [];
    state.tools = tools || [];
    preloadOAuthStatus().catch(() => {});
  } catch (e) { toast(e.message, 'error'); }
}

let evtSrc = null;
let streamWatchdog = null;

// markStreamEvent records that the stream is alive (any event, including the
// server's `ping` keepalive). On recovery from a down state it refetches
// core data so we don't miss events that fired while the socket was dead.
function markStreamEvent() {
  state.stream.lastEventAt = Date.now();
  if (state.stream.status !== 'live') {
    const recovered = state.stream.wasDown;
    state.stream.status = 'live';
    state.stream.wasDown = false;
    render();
    if (recovered) refetchCore();
  }
}

function startStream() {
  // The event stream is admin-only; a member would only collect 403s.
  if (!isAdmin()) return;
  if (evtSrc) try { evtSrc.close(); } catch {}
  state.stream.status = state.stream.lastEventAt ? 'reconnecting' : 'connecting';
  render();
  evtSrc = new EventSource('/v1/events/stream');
  evtSrc.addEventListener('open', () => { markStreamEvent(); });
  evtSrc.addEventListener('ping', () => { markStreamEvent(); });
  evtSrc.onerror = () => {
    // EventSource auto-reconnects on its own, but surface the gap so the
    // user sees "Offline" rather than a stale-but-confident UI.
    state.stream.status = 'offline';
    state.stream.wasDown = true;
    render();
  };
  evtSrc.addEventListener('approval', (e) => { markStreamEvent(); handleApprovalEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('audit', (e) => { markStreamEvent(); handleAuditEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('inbox', (e) => { markStreamEvent(); handleInboxEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('grant', () => { markStreamEvent(); handleGrantEvent(); });
  evtSrc.addEventListener('mcp_oauth_done', (e) => { markStreamEvent(); handleOAuthDone(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_refreshed', () => { markStreamEvent(); reloadServers(); render(); });
  evtSrc.addEventListener('mcp_oauth_needs_reauth', (e) => { markStreamEvent(); handleOAuthReauth(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_user_done', (e) => { markStreamEvent(); handleUserOAuthEvent(JSON.parse(e.data), 'done'); });
  evtSrc.addEventListener('mcp_oauth_user_needs_reauth', (e) => { markStreamEvent(); handleUserOAuthEvent(JSON.parse(e.data), 'reauth'); });

  // Watchdog: the server sends a `ping` every 15s. If we see nothing for
  // 45s the socket is wedged even though onerror never fired — common on
  // mobile Safari — so force a reconnect.
  if (!streamWatchdog) {
    streamWatchdog = setInterval(() => {
      if (!state.user || !state.stream.lastEventAt) return;
      if (Date.now() - state.stream.lastEventAt > 45000) {
        state.stream.wasDown = true;
        startStream();
      }
    }, 20000);
  }
}

// refetchCore re-pulls the data that SSE normally keeps live, after a
// stream outage. Pendings are authoritative; audit is dedupe-merged by id.
async function refetchCore() {
  try {
    const [pendings, audits] = await Promise.all([
      api('/v1/approvals?status=pending'),
      api('/v1/audit?' + auditServerParams({ limit: '50' }).toString()),
    ]);
    state.approvals = pendings || [];
    loadInbox();
    const fresh = audits || [];
    const seen = new Set(fresh.map((a) => a.id));
    state.audit = fresh.concat((state.audit || []).filter((a) => !seen.has(a.id))).slice(0, 200);
    render();
  } catch (_) { /* best-effort; the watchdog will retry */ }
}

function reconnectStream() {
  state.stream.wasDown = true;
  startStream();
}

// renderStreamPill is the tappable Live / Reconnecting… / Offline status in
// the header. Tapping (or Enter/Space) forces a reconnect when not live.
function renderStreamPill() {
  const s = state.stream || { status: 'connecting', lastEventAt: 0 };
  const live = s.status === 'live';
  const cls = live ? 'live' : (s.status === 'offline' ? 'offline' : 'reconnecting');
  const label = live ? 'Live' : (s.status === 'offline' ? 'Offline' : 'Reconnecting…');
  const ago = s.lastEventAt ? ` · last event ${relTime(s.lastEventAt)}` : '';
  return el('span', {
    class: 'stream-status ' + cls,
    title: (live ? 'Realtime stream connected' : 'Realtime stream ' + label.toLowerCase() + ' — tap to reconnect') + ago,
    role: 'button',
    tabindex: 0,
    on: {
      click: () => { if (!live) reconnectStream(); },
      keydown: (e) => { if (!live && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); reconnectStream(); } },
    },
  },
    el('span', { class: 'stream-pill ' + cls }),
    ' ' + label,
  );
}

function handleOAuthDone(payload) {
  const name = payload && payload.upstream;
  if (!name) return;
  if (state.oauthFlow && state.oauthFlow.name === name) {
    state.oauthFlow = null;
  }
  if (state.marketModal && state.marketModal.installedName === name) {
    state.marketModal = null;
  }
  toast(name + ': authorized.');
  reloadServers().then(() => loadOAuthStatus(name)).then(render);
}

function handleOAuthReauth(payload) {
  const name = payload && payload.upstream;
  if (!name) return;
  toast(name + ' needs reauthorization', 'error');
  state.oauthStatus = state.oauthStatus || {};
  state.oauthStatus[name] = { ...(state.oauthStatus[name] || {}), state: 'needs_reauth', last_error: payload.error || '' };
  render();
}

// handleUserOAuthEvent: somebody connected ('done') or lost ('reauth')
// their own sign-in to a per_user server. Only admins have the stream; the
// event names the person, so this tab speaks up only when it is theirs,
// and the Servers page refreshes its counts either way.
function handleUserOAuthEvent(payload, kind) {
  const name = payload && payload.upstream;
  if (!name) return;
  if (state.user && payload.user_id === state.user.id) {
    if (kind === 'done') toast(name + ': connected.');
    else toast(name + ' needs you to sign in again (My connections)', 'error');
    loadConnections();
  }
  if (state.route === 'servers') reloadServers().then(render);
}

async function loadOAuthStatus(name) {
  try {
    const st = await api('/v1/servers/' + encodeURIComponent(name) + '/oauth');
    state.oauthStatus = state.oauthStatus || {};
    state.oauthStatus[name] = st;
  } catch (_) {}
}

function handleApprovalEvent(req) {
  const i = state.approvals.findIndex((a) => a.id === req.id);
  if (req.status === 'pending') {
    if (i >= 0) state.approvals[i] = req;
    else state.approvals.unshift(req);
  } else {
    if (i >= 0) state.approvals.splice(i, 1);
    noteDecidedApproval(req);
  }
  render();
}

// noteDecidedApproval puts a decided row at the top of "Recently decided".
const RECENT_APPROVALS_CAP = 20;
function noteDecidedApproval(req) {
  if (!req || !req.id || !req.status || req.status === 'pending') return;
  const rest = (state.recentApprovals || []).filter((a) => a.id !== req.id);
  state.recentApprovals = [req].concat(rest).slice(0, RECENT_APPROVALS_CAP);
}

// handleHubEvent receives a live Events Hub event. When the operator is on the
// Events route we prepend it; otherwise we just bump the unacked badge so the
// nav label reflects the new arrival without a full reload.
const AUDIT_CAP = 1000;
function handleAuditEvent(ev) {
  state.audit.unshift(ev);
  // Cap higher than the old 200 so live events don't evict rows the user
  // pulled in via "Load older" pagination.
  if (state.audit.length > AUDIT_CAP) state.audit.length = AUDIT_CAP;
  render();
}

async function loadInsights() {
  state.insights.loading = true;
  render();
  const range = state.insights.range;
  try {
    const [overview, tools, agents, rules, anomalies, policies] = await Promise.all([
      api('/v1/insights/overview?range=' + range).catch(() => null),
      api('/v1/insights/tools?range=' + range).catch(() => []),
      api('/v1/insights/agents?range=' + range).catch(() => []),
      api('/v1/insights/auto/rules').catch(() => []),
      api('/v1/insights/anomalies?limit=50').catch(() => []),
      api('/v1/policies').catch(() => []),
    ]);
    state.insights.overview = overview;
    state.insights.tools = tools || [];
    state.insights.agents = agents || [];
    state.insights.autoRules = rules || [];
    state.anomalies = anomalies || [];
    state.policies = policies || [];
  } catch (e) { toast(e.message, 'error'); }
  state.insights.loading = false;
  render();
}

// ---- views -----------------------------------------------------------------

function viewSetup() {
  return el('div', { class: 'login-wrap' },
    el('div', { class: 'card' },
      el('h2', {}, 'Welcome to toolyard'),
      el('p', { class: 'meta' }, 'Create the local admin account to get started.'),
      el('label', {}, 'Username'),
      el('input', { id: 'su-user', autocomplete: 'username' }),
      el('label', {}, 'Password'),
      el('input', { id: 'su-pass', type: 'password', autocomplete: 'new-password' }),
      el('div', { class: 'err' }, state.errors.setup || ''),
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', {
          class: 'primary',
          on: { click: async () => {
            state.errors.setup = '';
            const username = $('su-user').value.trim();
            const password = $('su-pass').value;
            try {
              await api('/v1/auth/setup', { method: 'POST', body: { Username: username, Password: password } });
              await refreshUser(); await loadAll(); startStream();
              navigate('approvals');
            } catch (e) { state.errors.setup = e.message; render(); }
          }},
        }, 'Create account'),
      ),
    ),
  );
}

// viewLogin: with Clerk configured, "Sign in with Google" (the /login page)
// leads and the username/password form sits behind a toggle; without Clerk,
// or when the config didn't load, or on /?password=1, the form shows directly.
function viewLogin() {
  const cfg = state.authConfig;
  const clerk = !!(cfg && cfg.clerk);
  // password_login:false hides the form, but only when there's Google
  // sign-in to fall back on.
  const passwordAllowed = !clerk || cfg.password_login !== false;
  const showForm = passwordAllowed && (!clerk || state.showPasswordLogin);

  const form = [
    el('label', {}, 'Username'),
    el('input', {
      id: 'lg-user', autocomplete: 'username',
      on: { keydown: (e) => { if (e.key === 'Enter') submitPasswordLogin(); } },
    }),
    el('label', {}, 'Password'),
    el('input', {
      id: 'lg-pass', type: 'password', autocomplete: 'current-password',
      on: { keydown: (e) => { if (e.key === 'Enter') submitPasswordLogin(); } },
    }),
    el('div', { class: 'err' }, state.errors.login || ''),
    el('div', { class: 'row', style: 'margin-top: 12px;' },
      el('button', { class: clerk ? '' : 'primary', on: { click: submitPasswordLogin } }, 'Sign in'),
    ),
  ];

  return el('div', { class: 'login-wrap' },
    el('div', { class: 'card' },
      el('h2', {}, 'toolyard'),
      clerk ? [
        el('p', { class: 'meta' }, cfg.owner_only ? 'Sign in with your personal Google account.' : 'Sign in with your workspace Google account.'),
        el('a', { class: 'btn primary login-google', href: '/login' }, 'Sign in with Google'),
        showForm ? el('div', { class: 'login-or' }, 'or use your toolyard password') : null,
      ] : null,
      showForm ? form : null,
      clerk && passwordAllowed ? el('button', {
        class: 'login-toggle',
        on: { click: () => {
          state.showPasswordLogin = !state.showPasswordLogin;
          render();
          if (state.showPasswordLogin) { const u = $('lg-user'); if (u) u.focus(); }
        } },
      }, state.showPasswordLogin ? 'Hide password sign-in' : 'Use password instead') : null,
    ),
  );
}

let passwordLoginBusy = false;
async function submitPasswordLogin() {
  if (passwordLoginBusy) return;
  passwordLoginBusy = true;
  state.errors.login = '';
  try {
    await api('/v1/auth/login', { method: 'POST', body: {
      Username: $('lg-user').value.trim(),
      Password: $('lg-pass').value,
    }});
    await refreshUser(); await loadAll(); startStream();
    navigate(defaultRoute());
  } catch (e) { state.errors.login = e.message; render(); }
  finally { passwordLoginBusy = false; }
}

function viewApprovals() {
  if (!state.approvals.length) {
    return el('div', {}, el('div', { class: 'card empty' }, 'No pending approvals.'), renderRecentDecisions());
  }
  // Group by agent_id. Within each group, sort by created_at ascending so
  // the user reads the batch in chronological order. Anonymous calls fall
  // into a "—" bucket (likely stdio sessions or stale tokens; see Settings).
  const groups = new Map();
  for (const a of state.approvals) {
    const key = a.agent_id || '__anon__';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(a);
  }
  // Sort groups by the most recent approval in each, descending — newest
  // batch on top.
  const ordered = Array.from(groups.entries()).sort((x, y) => {
    const xMax = Math.max(...x[1].map((a) => a.created_at));
    const yMax = Math.max(...y[1].map((a) => a.created_at));
    return yMax - xMax;
  });

  return el('div', {}, ordered.map(([key, items]) => {
    items.sort((a, b) => a.created_at - b.created_at);
    return renderApprovalBatch(key, items);
  }), renderRecentDecisions());
}

// renderRecentDecisions lists the latest decided approvals with who decided
// each and how. Nothing renders until the server has decided rows to show.
function renderRecentDecisions() {
  const rows = (state.recentApprovals || []).filter((a) => a.status !== 'pending').slice(0, 10);
  if (!rows.length) return null;
  return el('div', { class: 'card recent-decisions' },
    el('h2', {}, 'Recently decided'),
    ...rows.map((a) => {
      const d = approvalDecider(a);
      const decided = deciderText(d);
      const raised = raiserLine(raiserOf(a.raised_by, a.agent_id));
      return el('div', { class: 'recent-row', 'data-recent-approval-id': a.id },
        el('div', { class: 'row' },
          el('span', { class: 'grow recent-tool' }, `${a.upstream_name || ''} · ${a.tool_name || ''}`),
          badge(a.status)),
        el('div', { class: 'meta', title: deciderTitle(d) },
          [decided, relTime(a.decided_at || a.created_at)].filter(Boolean).join(' · ')),
        raised ? el('div', { class: 'meta' }, raised) : null,
      );
    }),
  );
}

// renderArgs makes tool arguments phone-scannable: a flat object renders as
// a key/value table; anything nested falls back to pretty JSON.
function renderArgs(args) {
  if (!args || typeof args !== 'object' || Array.isArray(args)) {
    return el('pre', { class: 'json' }, JSON.stringify(args == null ? {} : args, null, 2));
  }
  const keys = Object.keys(args);
  if (keys.length === 0) return el('div', { class: 'meta' }, '(no arguments)');
  const flat = keys.every((k) => args[k] == null || typeof args[k] !== 'object');
  if (!flat) return el('pre', { class: 'json' }, JSON.stringify(args, null, 2));
  return el('table', { class: 'args-table' }, el('tbody', {}, keys.map((k) =>
    el('tr', {},
      el('td', { class: 'meta', style: 'vertical-align: top; padding-right: 10px; white-space: nowrap;' }, k),
      el('td', { style: 'word-break: break-word;' }, String(args[k])),
    ))));
}

function renderApprovalBatch(agentKey, items) {
  const now = Date.now();
  const ids = items.map((a) => a.id);
  const liveIds = items.filter((a) => a.expires_at > now).map((a) => a.id);
  const earliest = Math.min(...items.map((a) => a.expires_at));
  const isBatch = items.length > 1;
  const allExpired = liveIds.length === 0;
  // The group shares an agent id; the first row that recorded a raiser
  // names it. Rows from before raised_by keep showing the raw id.
  const groupRaiser = items.find((a) => a.raised_by);
  const labelAgent = agentKey === '__anon__' ? 'anonymous (no agent token)'
    : (groupRaiser && groupRaiser.raised_by.agent_name) || agentKey;

  // Compact summary line: "fs.write_file · github.create_issue · …"
  const toolSummary = items.map((a) => `${a.upstream_name}·${a.tool_name}`).join('  ·  ');

  const header = el('div', { class: 'row', style: 'margin-bottom: 8px;' },
    el('div', { class: 'grow' },
      el('div', { style: 'font-weight: 600; font-size: 14px;' },
        isBatch ? `Batch: ${items.length} pending writes` : `${items[0].upstream_name} · ${items[0].tool_name}`),
      el('div', { class: 'meta', style: 'margin-top: 2px;' },
        `agent: `, el('code', { title: agentKey === '__anon__' ? '' : agentKey }, labelAgent),
        ` · earliest expires ${relTime(earliest)}`),
      isBatch ? el('div', { class: 'meta', style: 'margin-top: 4px; font-family: ui-monospace, monospace;' }, toolSummary) : null,
    ),
    isBatch ? el('div', { class: 'row' },
      el('button', { class: 'primary', disabled: allExpired, on: { click: () => decideBatch(liveIds, 'allowed') }}, `Allow all (${liveIds.length})`),
      el('button', { class: 'danger', disabled: allExpired, on: { click: () => decideBatch(liveIds, 'denied')  }}, `Deny all`),
    ) : null,
  );

  const rows = items.map((a) => {
    const expired = a.expires_at <= now;
    const raiser = raiserOf(a.raised_by, a.agent_id);
    const raised = raiserLine(raiser);
    const card = el('div', {
      class: 'approval-card',
      tabindex: 0,
      'data-approval-id': a.id,
      style: 'border-top: 1px solid var(--border); padding: 12px 0; margin-top: 8px;' + (expired ? ' opacity: 0.55;' : ''),
    },
      el('div', { class: 'row' },
        el('span', { class: 'grow', style: 'font-weight: 500;' }, `${a.upstream_name} · ${a.tool_name}`),
        a.intent_category ? el('span', { class: 'badge' }, a.intent_category) : null,
        badge(expired ? 'expired' : a.status),
      ),
      raised ? el('div', { class: 'meta raised-by', title: raiserFacts(raiser).map(([k, v]) => `${k}: ${v}`).join('\n') }, raised) : null,
      el('div', { class: 'meta', style: 'margin: 4px 0 6px;' },
        `created ${relTime(a.created_at)} · ` + (expired ? 'expired' : `expires ${relTime(a.expires_at)}`)),
      el('div', { style: 'background: var(--bg); border: 1px solid var(--border); border-radius: 4px; padding: 8px; margin: 4px 0;' },
        el('div', { class: 'meta' }, 'reasoning'),
        a.reason || '(none provided)'),
      el('details', {},
        el('summary', {}, 'arguments'),
        renderArgs(a.arguments),
      ),
      el('div', { class: 'row', style: 'margin-top: 8px;' },
        el('button', { class: 'primary', disabled: expired, on: { click: () => decideApproval(a.id, 'allowed') } }, 'Allow'),
        el('button', { class: 'danger', disabled: expired, on: { click: () => decideApproval(a.id, 'denied')  } }, 'Deny'),
        expired ? null : el('span', { class: 'meta', style: 'margin-left: auto;' }, 'focus + a/d to decide'),
      ),
    );
    return card;
  });

  return el('div', { class: 'card' }, header, ...rows);
}

async function decideApproval(id, action) {
  try {
    const out = await api(`/v1/approvals/${id}/decide`, { method: 'POST', body: { Action: action } });
    state.approvals = state.approvals.filter((a) => a.id !== id);
    noteDecidedApproval(out);
    toast(action === 'allowed' ? 'Approved' : 'Denied');
    render();
  } catch (e) {
    if (e.status === 409 || e.status === 404 || e.status === 410) {
      // Already decided (maybe on another device) or expired. Drop the
      // card and show a neutral banner — not a false success or a scary
      // error. e.body is the current row on a 409.
      state.approvals = state.approvals.filter((a) => a.id !== id);
      const st = e.body && e.body.status;
      staleNote(st ? `Already ${st} — decided on another device or expired.`
                   : 'Already decided on another device or expired.');
      render();
      return;
    }
    toast(e.message, 'error');
  }
}

async function decideBatch(ids, action) {
  if (!ids.length) return;
  try {
    const out = await api('/v1/approvals/decide-batch', { method: 'POST', body: { ids, action }});
    const flipped = (out || []).filter((r) => r.status === action).length;
    const stale = ids.length - flipped;
    state.approvals = state.approvals.filter((a) => !ids.includes(a.id));
    (Array.isArray(out) ? out : []).forEach(noteDecidedApproval);
    if (flipped === 0) {
      staleNote(`All ${ids.length} were already decided on another device or expired.`);
    } else {
      toast(`${action === 'allowed' ? 'Approved' : 'Denied'} ${flipped} of ${ids.length}` +
            (stale ? ` · ${stale} already settled` : ''));
    }
    render();
  } catch (e) { toast(e.message, 'error'); }
}

// ---- Feature A: explicit tool/upstream policies ----------------------------

// toolPolicyMode resolves the effective 5-way mode for a tool: an explicit
// policy (allow/ask/deny) wins; otherwise auto (learned) or default.
function toolPolicyMode(toolName, autoApprove) {
  const p = (state.policies || []).find((x) => x.scope === 'tool' && x.target === toolName);
  if (p) return p.action;
  return autoApprove ? 'auto' : 'default';
}

const POLICY_MODES = [
  ['default', 'Default'],
  ['auto', 'Auto'],
  ['allow', 'Allow'],
  ['ask', 'Ask'],
  ['deny', 'Deny'],
];

// renderPolicyControl is the 5-segment pill: Default | Auto | Allow | Ask | Deny.
function renderPolicyControl(t) {
  const cur = toolPolicyMode(t.tool_name, t.auto_approve);
  return el('div', { class: 'policy-seg' }, ...POLICY_MODES.map(([m, label]) =>
    el('button', {
      class: 'seg' + (cur === m ? ' active' : '') + (m === 'deny' ? ' deny' : '') + (m === 'allow' ? ' allow' : ''),
      title: t.is_destructive && (m === 'auto' || m === 'allow')
        ? 'Destructive tool — Auto is still vetoed; Allow needs an explicit confirm.'
        : m,
      on: { click: () => setToolPolicyMode(t.tool_name, m) },
    }, label)));
}

async function setToolPolicyMode(toolName, mode, force) {
  try {
    const resp = await api('/v1/insights/tools/' + encodeURIComponent(toolName) + '/policy', {
      method: 'POST', body: { mode, force: !!force },
    });
    if (resp && resp.destructive_veto) {
      toast(toolName + ': saved, but destructive tools still require human review.', 'warn');
    } else {
      toast(toolName + ' → ' + mode);
    }
    // Refresh both the explicit-policy list and the insights table.
    state.policies = await api('/v1/policies').catch(() => state.policies);
    if (state.route === 'insights') loadInsights(); else render();
  } catch (err) {
    if (err.status === 409 && err.body && err.body.needs_force) {
      if (confirm(toolName + ' looks destructive. Allowing it bypasses the destructive veto — are you sure?')) {
        return setToolPolicyMode(toolName, mode, true);
      }
      return;
    }
    toast(err.message, 'error');
  }
}

// renderPolicyRulesCard lists explicit policies with a delete control.
function renderPolicyRulesCard() {
  const rules = state.policies || [];
  return el('div', { class: 'card' },
    el('h2', {}, 'Policy rules'),
    el('p', { class: 'meta' }, 'Explicit per-tool / per-upstream gates. These beat learned auto-approval (an explicit “Ask” always waits for you).'),
    rules.length === 0
      ? el('div', { class: 'empty' }, 'No explicit policies — tools fall back to the heuristic.')
      : el('table', {},
          el('thead', {}, el('tr', {}, el('th', {}, 'Scope'), el('th', {}, 'Target'), el('th', {}, 'Action'), el('th', {}, ''))),
          el('tbody', {}, rules.map((p) => el('tr', {},
            el('td', {}, p.scope),
            el('td', {}, el('code', {}, p.target)),
            el('td', {}, el('span', { class: 'badge ' + p.action }, p.action)),
            el('td', {}, el('button', { class: 'danger', on: { click: async () => {
              try { await api('/v1/policies/' + encodeURIComponent(p.id), { method: 'DELETE' });
                state.policies = await api('/v1/policies').catch(() => []);
                render();
              } catch (e) { toast(e.message, 'error'); }
            } } }, 'Delete')),
          ))),
        ),
  );
}

// setUpstreamPolicy is the per-server policy select handler (Servers tab).
async function setUpstreamPolicy(upstream, action) {
  try {
    if (action === 'default') {
      const p = (state.policies || []).find((x) => x.scope === 'upstream' && x.target === upstream);
      if (p) await api('/v1/policies/' + encodeURIComponent(p.id), { method: 'DELETE' });
    } else {
      await api('/v1/policies', { method: 'POST', body: { scope: 'upstream', target: upstream, action } });
    }
    state.policies = await api('/v1/policies').catch(() => state.policies);
    toast(upstream + ' → ' + action);
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function upstreamPolicyMode(upstream) {
  const p = (state.policies || []).find((x) => x.scope === 'upstream' && x.target === upstream);
  return p ? p.action : 'default';
}

function filteredAudit() {
  const f = state.auditFilter;
  const q = (f.q || '').toLowerCase();
  return (state.audit || []).filter((e) => {
    if (f.event_type && e.event_type !== f.event_type) return false;
    if (f.decision && (e.decision || '') !== f.decision) return false;
    if (f.agent && (e.agent_id || '') !== f.agent) return false;
    // The server already applied these to the loaded page; repeating them
    // keeps rows that arrive live on the stream in line.
    if (f.owner && (e.owner_user_id || '') !== f.owner) return false;
    if (f.decided_by && (e.decided_by_user_id || '') !== f.decided_by) return false;
    if (f.client_kind && (e.client_kind || '') !== f.client_kind) return false;
    if (f.session && (e.agent_session_id || '') !== f.session) return false;
    if (!q) return true;
    return [e.tool_name, e.upstream_name, e.reason, e.agent_id, agentLabel(e.agent_id), e.result_summary, e.event_type,
      e.agent_name, e.owner_name, e.owner_email, e.decided_by_name, e.decided_by_email, e.client_name,
      e.agent_session_id, e.client_session_id]
      .some((v) => (v || '').toLowerCase().includes(q));
  });
}

// AUDIT_SERVER_FILTERS are the actor filters sent to /v1/audit and the
// export as query params (the keys are the param names).
const AUDIT_SERVER_FILTERS = ['owner', 'decided_by', 'client_kind', 'session'];
function auditServerParams(extra = {}) {
  const p = new URLSearchParams(extra);
  for (const k of AUDIT_SERVER_FILTERS) if (state.auditFilter[k]) p.set(k, state.auditFilter[k]);
  return p;
}
function auditServerFiltered() {
  return AUDIT_SERVER_FILTERS.some((k) => state.auditFilter[k]);
}

// reloadAudit refetches the first page after an actor filter changed, so
// the table covers matching rows beyond what was already loaded.
let auditSeq = 0;
async function reloadAudit() {
  const seq = ++auditSeq;
  state.auditLoading = true;
  render();
  try {
    const rows = await api('/v1/audit?' + auditServerParams({ limit: '100' }).toString());
    if (seq !== auditSeq) return;
    state.audit = rows || [];
    state.auditEnd = false;
    state.auditPaged = false;
  } catch (e) {
    if (seq === auditSeq) toast(e.message, 'error');
  }
  if (seq !== auditSeq) return;
  state.auditLoading = false;
  render();
}

function uniqueSorted(vals) {
  return Array.from(new Set(vals.filter(Boolean))).sort();
}

// agentLabel resolves an agent_id to a human-readable label using the agent
// roster loaded into state.agents. Falls back to a shortened id for agents that
// are no longer in the roster (deleted/forgotten), and '—' for empty ids. The
// full id stays available via the cell title attribute at the call site.
function agentLabel(id) {
  if (!id) return '—';
  const a = (state.agents || []).find((x) => x.id === id);
  if (a) return a.name || a.id;
  return id.length > 12 ? id.slice(0, 12) + '…' : id;
}

// ---- who raised a call, who decided it --------------------------------------
//
// Audit rows carry the raiser fields inline, approvals under raised_by, inbox
// docs and grants their own decider fields. Rows written before the server
// recorded them have none of it: every helper falls back to the bare agent id
// or returns '' / null so the views show what they showed before.

const CLIENT_KIND_LABEL = {
  t3: 'T3', claude_code: 'Claude Code', codex: 'Codex', cursor: 'Cursor', opencode: 'opencode',
  cli: 'CLI', browser: 'Browser', unknown: 'Unknown',
};
const AGENT_KIND_LABEL = { dashboard: 'Dashboard', voice: 'Voice', cli: 'CLI', system: 'System', identity: 'Beknown key' };
const DECIDED_VIA_LABEL = {
  dashboard: 'dashboard', dashboard_batch: 'batch', push_token: 'notification tap', passkey: 'passkey',
  telegram: 'Telegram', auto_rule: 'auto-rule', policy: 'policy rule', inbox_grant: 'inbox grant',
  agent_cancel: 'cancelled by agent', expiry: 'expired',
};

function shortId(id, n = 12) {
  if (!id) return '';
  return id.length > n + 2 ? id.slice(0, n) + '…' : id;
}

// userNameById resolves a user id through the signed-in user and the /v1/users
// directory (ensureUserDir), falling back to the short id.
function userNameById(id) {
  if (!id) return '';
  if (state.user && state.user.id === id) return userLabel(state.user);
  const u = (state.users.rows || []).find((x) => x.id === id);
  return u ? userLabel(u) : shortId(id);
}

// ensureUserDir loads the users directory once (admin only) so owner and
// decider ids resolve to names and the audit owner filter can list people.
let userDirAsked = false;
function ensureUserDir() {
  if (userDirAsked || !isAdmin() || state.users.loaded || state.users.loading) return;
  userDirAsked = true;
  loadUsers().then(() => { if (state.inbox.openId) ibRefreshRegions(); });
}

// raiserOf normalises who raised a call. src is an audit row or an approval's
// raised_by; fallbackId is the approval's agent_id for rows without one.
function raiserOf(src, fallbackId) {
  const r = src || {};
  return {
    id: r.caller_id || r.agent_id || fallbackId || '',
    name: r.agent_name || '',
    kind: r.agent_kind || '',
    ownerId: r.owner_user_id || '', ownerName: r.owner_name || '', ownerEmail: r.owner_email || '',
    mcpSession: r.mcp_session_id || '', agentSession: r.agent_session_id || '',
    clientSession: r.client_session_id || '', claimed: !!r.client_session_claimed,
    clientKind: r.client_kind || '', clientName: r.client_name || '', clientIP: r.client_ip || '',
    via: r.via || '',
  };
}

function raiserName(r) { return r.name || (r.id ? agentLabel(r.id) : ''); }
function raiserOwner(r) { return r.ownerName || r.ownerEmail || userNameById(r.ownerId); }

// raiserChip is one short label for where the call came from: the entry
// point for dashboard / voice / CLI calls, else the MCP client.
function raiserChip(r) {
  if (['dashboard', 'voice', 'cli', 'system'].includes(r.kind)) return AGENT_KIND_LABEL[r.kind];
  if (r.clientKind && r.clientKind !== 'unknown') return CLIENT_KIND_LABEL[r.clientKind] || r.clientKind;
  if (r.kind === 'identity') return AGENT_KIND_LABEL.identity;
  return '';
}

// raiserFacts are the details behind the one-line summary, [label, value].
function raiserFacts(r) {
  const owner = raiserOwner(r);
  return [
    ['Agent', r.name && r.id ? `${r.name} (${r.id})` : (r.id || r.name)],
    ['Kind', r.kind],
    ['Owner', owner && r.ownerEmail && r.ownerEmail !== owner ? `${owner} <${r.ownerEmail}>` : owner],
    ['Client', r.clientName ? r.clientName + (r.clientKind ? ` (${r.clientKind})` : '') : r.clientKind],
    ['IP', r.clientIP],
    ['Via', r.via],
    ['Toolyard session', r.agentSession],
    ['MCP session', r.mcpSession],
    ['Client session', r.clientSession ? r.clientSession + (r.claimed ? ' (claimed)' : '') : ''],
  ].filter(([, v]) => v);
}

// raiserLine is the approvals summary: "Raised by <agent> · <owner> · <client>".
function raiserLine(r) {
  const parts = [raiserName(r), raiserOwner(r), raiserChip(r) || r.clientName].filter(Boolean);
  return parts.length ? 'Raised by ' + parts.join(' · ') : '';
}

// raiserEl is the audit "Raised by" cell: agent + chip, owner, and a
// details toggle for sessions, client, IP and via. openKey keeps the toggle
// open across the re-render every live event triggers.
function raiserEl(r, openKey) {
  const name = raiserName(r);
  if (!name && !r.ownerId && !r.ownerEmail) return '—';
  const chip = raiserChip(r);
  const owner = raiserOwner(r);
  const facts = raiserFacts(r);
  const more = facts.filter(([k]) => !['Agent', 'Owner'].includes(k));
  return el('div', { class: 'actor', title: facts.map(([k, v]) => `${k}: ${v}`).join('\n') },
    el('div', { class: 'actor-who' }, el('span', {}, name || '—'),
      chip ? el('span', { class: 'chip actor-chip' }, chip) : null),
    owner ? el('div', { class: 'meta' }, owner) : null,
    more.length ? el('details', {
      class: 'actor-more', open: !!(openKey && state.auditOpen[openKey]),
      on: { toggle: (ev) => { if (openKey) state.auditOpen[openKey] = ev.target.open; } },
    },
      el('summary', {}, 'details'),
      el('dl', { class: 'actor-facts' }, ...more.flatMap(([k, v]) => [
        el('dt', {}, k),
        el('dd', {}, k === 'Client session' && r.claimed
          ? [r.clientSession, el('span', { class: 'badge claimed inline-badge', title: 'The client’s own unverified claim' }, 'claimed')]
          : v),
      ])),
    ) : null,
  );
}

function decidedViaLabel(via, ref) {
  if (!via) return '';
  if (via === 'auto_rule') return ref ? 'auto-rule ' + shortId(ref) : 'auto-rule';
  return DECIDED_VIA_LABEL[via] || via;
}

// A decider is { name, email, userId, via, ref }; deciderWho is the person.
function deciderWho(d) { return d.name || d.email || userNameById(d.userId); }

function deciderTitle(d) {
  const who = deciderWho(d);
  return [
    who && d.email && d.email !== who ? `${who} <${d.email}>` : who,
    d.userId ? 'user ' + d.userId : '',
    d.via ? 'via ' + d.via : '',
    d.ref ? 'ref ' + d.ref : '',
  ].filter(Boolean).join('\n');
}

// deciderText: "Decided by <person> via <label>", the instrument alone when
// no person decided (policy, auto-rule), or '' when nothing is recorded.
function deciderText(d) {
  const who = deciderWho(d);
  const via = decidedViaLabel(d.via, d.ref);
  if (who) return 'Decided by ' + who + (via ? ' via ' + via : '');
  if (d.via === 'expiry') return 'Expired';
  if (d.via === 'agent_cancel') return 'Cancelled by agent';
  return via ? 'Decided by ' + via : '';
}

// deciderEl is the audit "Decided by" cell: the person, then "via <label>".
function deciderEl(d) {
  const who = deciderWho(d);
  const via = decidedViaLabel(d.via, d.ref);
  if (!who && !via) return '—';
  return el('div', { class: 'actor', title: deciderTitle(d) },
    el('div', { class: 'actor-who' }, who || via),
    who && via ? el('div', { class: 'meta' }, 'via ' + via) : null);
}

function auditDecider(e) {
  return { name: e.decided_by_name, email: e.decided_by_email, userId: e.decided_by_user_id, via: e.decided_via, ref: e.decider_ref };
}

// approvalDecider reads the typed decider fields, else the legacy decided_by
// (a user id, or "<via>:<ref>") and auto_decided_by (an auto-rule id).
function approvalDecider(a) {
  let via = a.decided_via || '', ref = a.decider_ref || '', userId = '';
  const legacy = a.decided_by || '';
  if (!via && a.auto_decided_by) { via = 'auto_rule'; ref = a.auto_decided_by; }
  if (legacy) {
    const i = legacy.indexOf(':');
    const head = i > 0 ? legacy.slice(0, i) : legacy;
    if (DECIDED_VIA_LABEL[head]) {
      if (!via) { via = head; ref = ref || (i > 0 ? legacy.slice(i + 1) : ''); }
    } else userId = legacy;
  }
  return { name: a.decider_name, email: a.decider_email, userId, via, ref };
}

// inboxDecider: decided_by is the display label the server stored; it is a
// raw id or "<via>:<ref>" when no person decided, so it only stands in for
// the name when it looks like one.
function inboxDecider(r) {
  const label = r.decided_by || '';
  const raw = !label || label === r.decider_user_id ||
    (r.decider_via && (label === r.decider_via || label.startsWith(r.decider_via + ':')));
  return { name: r.decider_name || (raw ? '' : label), email: r.decider_email, userId: r.decider_user_id, via: r.decider_via, ref: r.decider_ref };
}

function csvCell(v) {
  const s = v == null ? '' : String(v);
  // Quote when the cell contains a comma, quote, or newline; double inner quotes.
  return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}

// auditExportURL builds a server-side export link honoring the structured
// dropdown filters (the free-text q filter is client-only).
function auditExportURL(format) {
  const f = state.auditFilter;
  const p = auditServerParams({ format });
  if (f.event_type) p.set('event_type', f.event_type);
  if (f.decision) p.set('decision', f.decision);
  if (f.agent) p.set('agent_id', f.agent);
  return '/v1/audit/export?' + p.toString();
}

function exportAuditCSV() {
  const rows = filteredAudit();
  // The first nine columns keep their old order; who raised and who
  // decided follow.
  const header = ['when_iso', 'event_type', 'upstream', 'tool', 'decision', 'agent_id', 'agent', 'reason', 'result_summary',
    'agent_name', 'agent_kind', 'owner_user_id', 'owner_name', 'owner_email',
    'client_kind', 'client_name', 'client_ip', 'via',
    'agent_session_id', 'mcp_session_id', 'client_session_id', 'client_session_claimed', 'approval_id',
    'decided_by_user_id', 'decided_by_name', 'decided_by_email', 'decided_via', 'decider_ref'];
  const lines = [header.join(',')];
  for (const e of rows) {
    lines.push([
      new Date(e.ts).toISOString(), e.event_type, e.upstream_name, e.tool_name,
      e.decision, e.agent_id, raiserName(raiserOf(e)), e.reason, e.result_summary,
      e.agent_name, e.agent_kind, e.owner_user_id, e.owner_name, e.owner_email,
      e.client_kind, e.client_name, e.client_ip, e.via,
      e.agent_session_id, e.mcp_session_id, e.client_session_id,
      e.client_session_id ? String(!!e.client_session_claimed) : '', e.approval_id,
      e.decided_by_user_id, e.decided_by_name, e.decided_by_email, e.decided_via, e.decider_ref,
    ].map(csvCell).join(','));
  }
  const blob = new Blob([lines.join('\n')], { type: 'text/csv' });
  const a = el('a', { href: URL.createObjectURL(blob), download: `toolyard-audit-${Date.now()}.csv` });
  document.body.appendChild(a); a.click();
  setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 0);
}

async function loadOlderAudit() {
  if (!state.audit.length) return;
  const oldest = state.audit[state.audit.length - 1].ts;
  try {
    const older = await api('/v1/audit?' + auditServerParams({ before: String(oldest), limit: '100' }).toString());
    if (!older || !older.length) { state.auditEnd = true; render(); return; }
    const seen = new Set(state.audit.map((a) => a.id));
    state.audit = state.audit.concat(older.filter((a) => !seen.has(a.id)));
    state.auditPaged = true;
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function viewAudit() {
  const f = state.auditFilter;
  // With an actor filter on, an empty page still shows the filter bar so
  // the filter can be changed back.
  if (!state.audit.length && !auditServerFiltered() && !state.auditLoading) {
    return el('div', { class: 'card empty' }, 'No events yet.');
  }
  const setF = (k) => (ev) => { f[k] = ev.target.value; render(); };
  // Actor filters reload the page from the server (AUDIT_SERVER_FILTERS).
  const setServerF = (k) => (ev) => { f[k] = ev.target.value; reloadAudit(); };
  const eventTypes = uniqueSorted(state.audit.map((e) => e.event_type));
  const decisions = uniqueSorted(state.audit.map((e) => e.decision));
  const agents = uniqueSorted(state.audit.map((e) => e.agent_id));
  const agentNames = {};
  for (const e of state.audit) if (e.agent_id && e.agent_name && !agentNames[e.agent_id]) agentNames[e.agent_id] = e.agent_name;

  const opt = (v, label) => el('option', { value: v, selected: false }, label);
  const sel = (key, all, vals) => {
    const s = el('select', { on: { change: setF(key) } }, opt('', all), ...vals.map((v) => opt(v, v)));
    s.value = f[key] || '';
    return s;
  };
  // Agent dropdown shows names but keeps the agent_id as the option value so
  // filtering (filteredAudit compares e.agent_id) and server export keep working.
  const selAgent = () => {
    const s = el('select', { on: { change: setF('agent') } }, opt('', 'All agents'),
      ...agents.map((id) => opt(id, agentNames[id] || agentLabel(id))));
    s.value = f.agent || '';
    return s;
  };
  // Owner and decider dropdowns list the users directory plus anyone the
  // loaded rows name, keyed by user id (the server filters on ids).
  const people = new Map();
  for (const u of state.users.rows || []) if (u.id) people.set(u.id, userLabel(u));
  const notePerson = (id, name, email) => { if (id && !people.has(id)) people.set(id, name || email || shortId(id)); };
  for (const e of state.audit) {
    notePerson(e.owner_user_id, e.owner_name, e.owner_email);
    notePerson(e.decided_by_user_id, e.decided_by_name, e.decided_by_email);
  }
  for (const k of ['owner', 'decided_by']) notePerson(f[k], userNameById(f[k]));
  const peopleOpts = [...people.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  const selPerson = (key, all, title) => {
    const s = el('select', { class: 'audit-actor-filter', title, on: { change: setServerF(key) } },
      opt('', all), ...peopleOpts.map(([id, label]) => opt(id, label)));
    s.value = f[key] || '';
    return s;
  };
  const kinds = Object.keys(CLIENT_KIND_LABEL);
  for (const k of uniqueSorted(state.audit.map((e) => e.client_kind).concat([f.client_kind]))) if (!kinds.includes(k)) kinds.push(k);
  const selClient = () => {
    const s = el('select', { class: 'audit-actor-filter', title: 'Client the call came from', on: { change: setServerF('client_kind') } },
      opt('', 'All clients'), ...kinds.map((k) => opt(k, CLIENT_KIND_LABEL[k] || k)));
    s.value = f.client_kind || '';
    return s;
  };
  // The session box commits on Enter or blur; sessionDraft keeps what's
  // typed across live re-renders until then.
  const commitSession = (ev) => {
    const v = ev.target.value.trim();
    f.sessionDraft = null;
    if (v !== f.session) { f.session = v; reloadAudit(); }
  };
  const sessionBox = el('input', {
    id: 'audit-session', type: 'search', class: 'audit-session', placeholder: 'Session (ses_…)',
    title: 'Toolyard agent session id',
    on: {
      input: (ev) => { f.sessionDraft = ev.target.value; },
      change: commitSession,
      keydown: (ev) => { if (ev.key === 'Enter') commitSession(ev); },
    },
  });
  sessionBox.value = f.sessionDraft != null ? f.sessionDraft : (f.session || '');

  const filterBar = el('div', { class: 'row audit-filters', style: 'gap: 8px; margin-bottom: 12px; flex-wrap: wrap;' },
    (() => { const i = el('input', { id: 'audit-q', type: 'search', placeholder: 'Filter tool / reason / agent / result…', class: 'grow', on: { input: setF('q') } }); i.value = f.q || ''; return i; })(),
    sel('event_type', 'All events', eventTypes),
    sel('decision', 'All decisions', decisions),
    selAgent(),
    selPerson('owner', 'All owners', 'Owner of the calling agent'),
    selPerson('decided_by', 'Any decider', 'Person who approved or denied'),
    selClient(),
    sessionBox,
    el('button', { on: { click: exportAuditCSV }, title: 'Export the rows currently shown (includes the text filter)' }, 'Export shown'),
    el('a', { href: auditExportURL('csv'), target: '_blank', title: 'Server-side export of the full log honoring the dropdown filters' }, 'Full CSV'),
    el('a', { href: auditExportURL('json'), target: '_blank' }, 'Full JSON'),
  );

  const rows = filteredAudit();
  const tbl = el('table', { class: 'audit-table' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'When'),
      el('th', {}, 'Event'),
      el('th', {}, 'Tool'),
      el('th', {}, 'Raised by'),
      el('th', {}, 'Decision'),
      el('th', {}, 'Decided by'),
      el('th', {}, 'Reasoning / Result'),
    )),
    el('tbody', {}, rows.length ? rows.map((e) => el('tr', {},
      el('td', { class: 'meta' }, relTime(e.ts)),
      el('td', {}, e.event_type),
      el('td', {}, e.tool_name ? `${e.upstream_name || ''} · ${e.tool_name}` : '—'),
      el('td', { class: 'audit-raiser' }, raiserEl(raiserOf(e), e.id)),
      el('td', {}, e.decision || '—'),
      el('td', { class: 'audit-decider' }, deciderEl(auditDecider(e))),
      el('td', {}, e.reason ? el('div', {}, el('div', {}, e.reason),
        e.result_summary ? el('div', { class: 'meta', style: 'margin-top: 4px;' }, e.result_summary) : null)
        : (e.result_summary || '—')),
    )) : el('tr', {}, el('td', { class: 'empty', colSpan: 7 },
      state.auditLoading ? 'Loading…' : 'No events match these filters.'))),
  );

  const footer = el('div', { class: 'row', style: 'margin-top: 12px; align-items: center; gap: 12px;' },
    el('span', { class: 'meta' }, `${rows.length} of ${state.audit.length} loaded` +
      (auditServerFiltered() ? ' · filtered by owner, decider, client or session' : '')),
    state.auditLoading ? el('span', { class: 'meta' }, 'Loading…')
      : state.auditEnd
        ? el('span', { class: 'meta' }, 'No older events.')
        : el('button', { disabled: !state.audit.length, on: { click: loadOlderAudit } }, 'Load older'),
  );

  return el('div', { class: 'card' }, filterBar, tbl, footer);
}

// ---- "Your Beknown key" -----------------------------------------------------
//
// Each person holds at most one Beknown key: toolyard forwards it to Beknown
// services (BkCoreServices, BkDocsServices) so their change logs name the
// person. An admin provisions it; only its owner ever sees it, once, on
// reveal. The raw key stays in state.myKey.reveal (memory only) until the
// owner hides it, so a live re-render doesn't lose it mid-copy.

function freshMyKey() {
  return { loaded: false, loading: false, data: null, error: '', reveal: null, revealing: false, revealError: '' };
}

let myKeySeq = 0;
async function loadMyKey() {
  const k = state.myKey;
  const seq = ++myKeySeq;
  k.loading = true;
  try {
    const r = await api('/v1/me/identity-key');
    if (seq !== myKeySeq || state.myKey !== k) return; // a newer load (or a logout) owns the card
    k.data = r || { has_key: false };
    k.error = '';
  } catch (e) {
    if (seq !== myKeySeq || state.myKey !== k) return;
    k.error = e.message;
  }
  k.loading = false;
  k.loaded = true;
  render();
}

async function revealMyKey() {
  const k = state.myKey;
  if (k.revealing) return;
  k.revealing = true; k.revealError = ''; render();
  try {
    const r = await api('/v1/me/identity-key/reveal', { method: 'POST', body: {} });
    if (r && r.key) k.reveal = { key: r.key, fingerprint: r.fingerprint || '' };
    else k.revealError = 'toolyard didn\'t return a key. Ask a toolyard admin to rotate it.';
  } catch (e) {
    const code = e.body && e.body.error;
    k.revealError = code === 'already_revealed'
      ? 'This key was already revealed and can\'t be shown again. If you didn\'t save it, ask a toolyard admin to rotate it.'
      : code === 'no_key'
        ? 'You have no Beknown key yet. Ask a toolyard admin to provision one.'
        : e.message;
  }
  k.revealing = false;
  await loadMyKey(); // renders
}

function hideRevealedKey() {
  if (!confirm('Hide your key? toolyard will not show it again, so make sure it is pasted into T3 first.')) return;
  state.myKey.reveal = null;
  render();
}

// shortFingerprint trims the 64-hex sha256 to "1a2b3c4d…9f0e"; callers put
// the full value in the title for hover.
function shortFingerprint(fp) {
  return fp && fp.length > 16 ? fp.slice(0, 8) + '…' + fp.slice(-4) : (fp || '—');
}

function regStatusBadge(st) {
  const cls = st === 'registered' ? 'allowed' : st === 'error' ? 'denied' : 'expired';
  return el('span', { class: 'badge ' + cls }, st || 'unknown');
}

function renderRegistrations(regs) {
  if (!regs || !regs.length) return el('div', { class: 'meta' }, 'Not registered with any Beknown service yet.');
  return el('table', { class: 'reg-table' },
    el('thead', {}, el('tr', {}, el('th', {}, 'Service'), el('th', {}, 'Status'), el('th', {}, 'Details'))),
    el('tbody', {}, regs.map((r) => el('tr', {},
      el('td', {}, el('code', {}, r.upstream)),
      el('td', {}, regStatusBadge(r.status)),
      r.error
        ? el('td', { class: 'reg-error' }, r.error)
        : el('td', { class: 'meta' }, r.updated_at ? relTime(toMs(r.updated_at)) : ''),
    ))));
}

// renderOwnerPendingRemovals: old or revoked fingerprints a Beknown registry
// still holds. toolyard retries the removal in the background.
function renderOwnerPendingRemovals(list) {
  if (!list || !list.length) return null;
  const ups = [...new Set(list.map((p) => p.upstream))];
  return el('div', { class: 'key-pending' },
    el('ul', {}, ups.map((up) => el('li', {
      title: list.filter((p) => p.upstream === up).map((p) => p.error).filter(Boolean).join('\n'),
    }, '⚠ An old key is still being removed from ', el('code', {}, up), '.'))),
    el('div', { class: 'meta' }, 'toolyard keeps retrying this on its own; there is nothing for you to do.'),
  );
}

function renderRevealedKey(rv) {
  return el('div', {},
    el('div', { class: 'warn-pill' }, '⚠  Copy your key now. toolyard will not show it again.'),
    el('div', { class: 'snippet' },
      el('div', { class: 'head' },
        el('span', {}, 'Beknown key'),
        el('button', { class: 'copy-btn', on: { click: (e) => copyToButton(e.target, rv.key) } }, 'Copy'),
      ),
      el('pre', { class: 'key-value' }, rv.key),
    ),
    el('h4', {}, 'Paste it into T3'),
    el('ol', { class: 'key-steps' },
      el('li', {}, 'Click ', el('strong', {}, 'Copy'), ' above.'),
      el('li', {}, 'In T3, open ', el('strong', {}, 'Settings → Experiments'), '.'),
      el('li', {}, 'Under ', el('strong', {}, 'My managed MCP integrations'), ', choose ', el('strong', {}, 'Add Bifrost'), '.'),
      el('li', {}, 'Paste the key where your Bifrost key used to go, then save.'),
    ),
    el('p', { class: 'meta' },
      'The key takes effect only once your T3 points at toolyard. That switch is rolling out one person at a time.'),
    rv.fingerprint
      ? el('div', { class: 'meta' }, 'Fingerprint ', el('code', { title: rv.fingerprint }, shortFingerprint(rv.fingerprint)))
      : null,
    el('div', { class: 'row key-actions' },
      el('button', { on: { click: hideRevealedKey } }, 'I\'ve saved it, hide the key'),
    ),
  );
}

function renderKeyStatus(d) {
  const created = toMs(d.created_at);
  return el('div', {},
    el('div', { class: 'key-facts' },
      el('span', { class: 'badge allowed' }, 'active'),
      el('span', { class: 'meta' }, 'Fingerprint ', el('code', { title: d.fingerprint || '' }, shortFingerprint(d.fingerprint))),
      created
        ? el('span', { class: 'meta', title: new Date(created).toLocaleString() }, 'Created ' + new Date(created).toLocaleDateString())
        : null,
    ),
    el('h4', {}, 'Registered with'),
    renderRegistrations(d.registrations),
    el('p', { class: 'meta' }, 'Lost the key? Ask a toolyard admin to rotate it, then reveal the new one here.'),
  );
}

// renderIdentityKeyCard is "Your Beknown key", shown to every signed-in
// person on Agents (and on My servers for members).
function renderIdentityKeyCard() {
  const k = state.myKey;
  const d = k.data || {};
  let body;
  if (k.reveal) body = renderRevealedKey(k.reveal);
  else if (!k.loaded) body = el('div', { class: 'meta' }, 'Loading…');
  else if (!k.data) body = el('div', {},
    el('div', { class: 'err' }, 'Couldn\'t load your key: ' + (k.error || 'unknown error')),
    el('button', { disabled: k.loading, on: { click: () => loadMyKey() } }, 'Retry'),
  );
  else if (!d.has_key) body = el('div', { class: 'empty' }, 'Ask a toolyard admin to provision your Beknown key.');
  else if (!d.revealed) body = el('div', {},
    el('p', { class: 'meta' }, 'Your key is ready. It is shown only once, so have T3 open to paste it straight in.'),
    el('button', { class: 'primary', disabled: k.revealing, on: { click: revealMyKey } },
      k.revealing ? 'Revealing…' : 'Reveal key (shown once)'),
  );
  else body = renderKeyStatus(d);
  return el('div', { class: 'card key-card' },
    el('h2', {}, 'Your Beknown key'),
    el('p', { class: 'meta' },
      'Beknown services (BkCoreServices, BkDocsServices) use this key to record who made each change. It replaces your Bifrost virtual key.'),
    k.revealError ? el('div', { class: 'err' }, k.revealError) : null,
    body,
    k.data && !k.reveal ? renderOwnerPendingRemovals(d.pending_removals) : null,
  );
}

function viewAgents() {
  return el('div', {},
    state.agentModal ? renderAgentModal() : null,
    renderIdentityKeyCard(),
    el('div', { class: 'card' },
      el('div', { class: 'agent-add-bar' },
        el('h2', { style: 'margin: 0;' }, 'Agents'),
        el('button', {
          class: 'primary',
          on: { click: () => { state.agentModal = { stage: 'name', name: 'claude-code', snippetTab: 'cli' }; render(); } },
        }, '+ Add new agent'),
      ),
      el('p', { class: 'meta', style: 'margin: 4px 0 12px;' },
        'Each enrolled agent gets a long-lived bearer token. Tokens are shown once on creation — store them in your agent\'s MCP config and you\'re done.'),
      state.agents.length === 0
        ? el('div', { class: 'empty' }, 'No agents yet. Click "Add new agent" to enrol your first one.')
        : el('table', {}, el('thead', {}, el('tr', {},
            el('th', {}, 'Name'), el('th', {}, 'ID'), el('th', {}, 'Last seen'), el('th', {}, 'Status'), el('th', {}, ''))),
            // kind "identity" is the person's Beknown key: the generic
            // rotate/disable/delete routes refuse it (409 identity_agent),
            // so it gets no buttons, only a pointer to the key card.
            el('tbody', {}, state.agents.map((a) => el('tr', { style: a.disabled ? 'opacity: 0.6;' : '' },
              el('td', {}, a.name,
                a.kind === 'identity' ? el('span', { class: 'badge identity inline-badge' }, 'Beknown key') : null),
              el('td', {}, el('code', {}, a.id)),
              el('td', { class: 'meta' }, a.last_seen ? relTime(a.last_seen) : 'never'),
              el('td', {}, a.disabled === true
                ? el('span', { class: 'badge denied' }, 'disabled')
                : el('span', { class: 'badge allowed' }, 'active')),
              el('td', {}, a.kind === 'identity'
                ? el('span', { class: 'meta' }, 'Managed on the Beknown key card above')
                : el('div', { class: 'row' },
                    el('button', { on: { click: () => rotateAgent(a) } }, 'Rotate'),
                    a.disabled
                      ? el('button', { on: { click: () => setAgentDisabled(a, false) } }, 'Enable')
                      : el('button', { on: { click: () => setAgentDisabled(a, true) } }, 'Disable'),
                    el('button', { class: 'danger', on: { click: () => deleteAgent(a) } }, 'Delete'),
                  )),
            )))),
    ),
  );
}

async function rotateAgent(a) {
  if (!confirm(`Rotate ${a.name}'s token? The old token keeps working for a 10-minute grace window, then stops.`)) return;
  try {
    const resp = await api(`/v1/agents/${encodeURIComponent(a.id)}/rotate`, { method: 'POST', body: {} });
    // Reuse the enrollment "token + setup snippets" modal to reveal the new token.
    state.agentModal = { stage: 'done', name: a.name, agent: { id: a.id, token: resp.token }, snippetTab: 'cli' };
    await reloadAgents();
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function setAgentDisabled(a, disabled) {
  try {
    await api(`/v1/agents/${encodeURIComponent(a.id)}/${disabled ? 'disable' : 'enable'}`, { method: 'POST', body: {} });
    toast(`${a.name} ${disabled ? 'disabled' : 'enabled'}`);
    await reloadAgents();
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function deleteAgent(a) {
  if (!confirm(`Permanently delete ${a.name}? Its token stops working immediately and can't be recovered.`)) return;
  try {
    await api(`/v1/agents/${encodeURIComponent(a.id)}`, { method: 'DELETE' });
    toast(`${a.name} deleted`);
    await reloadAgents();
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function renderAgentModal() {
  const m = state.agentModal;
  if (m.stage === 'name') return renderAgentNameStep(m);
  if (m.stage === 'done') return renderAgentDoneStep(m);
  return null;
}

function closeAgentModal() {
  state.agentModal = null;
  render();
}

function renderAgentNameStep(m) {
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeAgentModal(); } } },
    el('div', { class: 'modal' },
      el('h3', {}, 'Add a new agent'),
      el('div', { class: 'meta' },
        'Pick a name for the agent. You\'ll see the bearer token and copy-paste setup snippets next.'),
      el('label', {},
        el('div', { class: 'meta' }, 'Name'),
        el('input', {
          id: 'agent-modal-name', autofocus: true,
          placeholder: 'e.g. claude-code, my-laptop, prod-bot',
          value: m.name,
          on: {
            input: (e) => { state.agentModal.name = e.target.value; },
            keydown: (e) => { if (e.key === 'Enter') createAgentFromModal(); },
          },
        }),
      ),
      m.error ? el('div', { class: 'err' }, m.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { on: { click: closeAgentModal } }, 'Cancel'),
        el('button', { class: 'primary', on: { click: createAgentFromModal } }, 'Create agent'),
      ),
    ),
  );
}

async function createAgentFromModal() {
  const m = state.agentModal;
  const name = (m.name || '').trim();
  if (!name) { state.agentModal.error = 'name required'; render(); return; }
  try {
    const ag = await api('/v1/agents', { method: 'POST', body: { Name: name }});
    state.agentModal = { stage: 'done', name, agent: ag, snippetTab: 'cli' };
    await reloadAgents();
    render();
  } catch (e) { state.agentModal.error = e.message; render(); }
}

async function reloadAgents() {
  try {
    const agents = await api('/v1/agents');
    state.agents = agents || [];
  } catch {}
}

function renderAgentDoneStep(m) {
  const url = window.location.origin + '/mcp';
  const baseUrlNoMcp = window.location.origin;
  const tok = m.agent.token;
  const tabs = [
    { id: 'cli',     label: 'Claude Code CLI' },
    { id: 'project', label: '.mcp.json (project)' },
    { id: 'global',  label: '~/.claude.json (global)' },
    { id: 'hermes',  label: 'hermes' },
    { id: 'rules',   label: 'Teach it the rules' },
  ];

  const cli =
`claude mcp add --transport http --scope user toolyard \\
  ${url} \\
  --header "Authorization: Bearer ${tok}"`;

  const project = JSON.stringify({
    mcpServers: {
      toolyard: {
        type: 'http',
        url,
        headers: { Authorization: `Bearer ${tok}` },
      },
    },
  }, null, 2);

  const global = `// merge into ~/.claude.json under "mcpServers":
${JSON.stringify({
  toolyard: {
    type: 'http',
    url,
    headers: { Authorization: `Bearer ${tok}` },
  },
}, null, 2)}`;

  // hermes (NousResearch hermes-agent). The `mcp add` one-liner registers the
  // HTTP server; auth headers aren't a CLI flag there, so the bearer token is
  // set in ~/.hermes/config.yaml under mcp_servers — both shown here.
  const hermes =
`# 1) register the server:
hermes mcp add toolyard --url ${url}

# 2) add the auth header — merge into ~/.hermes/config.yaml under mcp_servers:
mcp_servers:
  toolyard:
    url: "${url}"
    headers:
      Authorization: "Bearer ${tok}"`;

  const snippets = { cli, project, global, hermes, rules: agentRulesSnippet(baseUrlNoMcp, tok) };
  const snippet = snippets[m.snippetTab] || cli;
  const snippetLang = m.snippetTab === 'hermes' ? 'yaml' : ['cli', 'rules'].includes(m.snippetTab) ? 'bash' : 'json';

  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeAgentModal(); } } },
    el('div', { class: 'modal modal-wide' },
      el('h3', {}, '✓  Agent "', m.name, '" ready'),
      el('div', { class: 'warn-pill' }, '⚠  Save the token now — it cannot be retrieved later.'),
      el('div', { style: 'margin: 8px 0;' },
        el('div', { class: 'meta' }, 'agent_id'),
        el('code', { style: 'word-break: break-all;' }, m.agent.agent_id),
      ),
      el('div', { style: 'margin: 8px 0 12px;' },
        el('div', { class: 'meta' }, 'token'),
        el('div', { class: 'snippet' },
          el('div', { class: 'head' },
            el('span', {}, 'bearer token'),
            el('button', { class: 'copy-btn', on: { click: (e) => copyToButton(e.target, tok) }}, 'Copy'),
          ),
          el('pre', {}, tok),
        ),
      ),
      el('div', { class: 'tabs' },
        ...tabs.map((t) => el('button', {
          class: m.snippetTab === t.id ? 'active' : '',
          on: { click: () => { state.agentModal.snippetTab = t.id; render(); } },
        }, t.label)),
      ),
      el('div', { class: 'snippet' },
        el('div', { class: 'head' },
          el('span', {}, m.snippetTab === 'cli'
            ? 'Run this in your terminal'
            : m.snippetTab === 'project'
              ? 'Save as .mcp.json at the root of any project'
              : m.snippetTab === 'global'
                ? 'Open ~/.claude.json and merge under mcpServers'
                : m.snippetTab === 'rules'
                  ? 'Teach the agent how to ask you for permission'
                : m.snippetTab === 'hermes'
                  ? 'Run the add command, then merge the header into ~/.hermes/config.yaml'
                  : 'Run this in your terminal'),
          el('button', { class: 'copy-btn', on: { click: (e) => copyToButton(e.target, snippet) }}, 'Copy'),
        ),
        el('pre', { 'data-lang': snippetLang }, snippet),
      ),
      m.snippetTab === 'cli' ? el('div', { class: 'meta', style: 'margin-top: 8px;' },
        'After running, restart your Claude Code session and run ', el('code', {}, '/mcp'),
        '. You should see ', el('code', {}, 'toolyard ✔ connected'),
        isAdmin()
          ? [' with ', el('code', {}, String(state.tools.length || '...')), ' tools.']
          : ' with the tools of the servers you have access to.',
      ) : null,
      el('div', { class: 'meta', style: 'margin-top: 6px; font-size: 11px;' },
        'Endpoint: ', el('code', {}, url),
        ' · Dashboard: ', el('code', {}, baseUrlNoMcp),
      ),
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { class: 'primary', on: { click: closeAgentModal } }, 'Done'),
      ),
    ),
  );
}

async function copyToButton(btn, text) {
  try {
    await navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = 'Copied';
    btn.classList.add('copied');
    setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 1400);
  } catch (e) {
    // Fallback: select the next sibling pre's text.
    toast('Copy failed: ' + e.message, 'error');
  }
}

// ---- My servers (every user) and Users (admin) ------------------------------

// normGroups accepts the documented [{name, kind, tool_count, status}] and
// tolerates a wrapped object or bare group names.
function normGroups(r) {
  const list = Array.isArray(r) ? r : (r && (r.servers || r.groups)) || [];
  return list.map((g) => (typeof g === 'string' ? { name: g } : g));
}

async function loadMyServers() {
  try {
    const mine = await api('/v1/me/servers');
    state.myServers = normGroups(mine);
  } catch (e) { toast(e.message, 'error'); }
  render();
}

function groupKindBadge(kind) {
  return kind === 'builtin'
    ? el('span', { class: 'badge kind-builtin' }, 'built-in')
    : el('span', { class: 'badge' }, 'server');
}

function groupStatusBadge(st) {
  if (!st) return el('span', { class: 'meta' }, '—');
  const good = ['ok', 'active', 'connected', 'ready'].includes(st);
  const bad = ['error', 'failed', 'down', 'disconnected'].includes(st);
  return el('span', { class: 'badge ' + (good ? 'allowed' : bad ? 'denied' : 'pending') }, st);
}

function viewMyServers() {
  const rows = state.myServers || [];
  return el('div', {},
    renderIdentityKeyCard(),
    el('div', { class: 'card' },
      el('h2', {}, 'My servers'),
      el('p', { class: 'meta', style: 'margin: 4px 0 12px;' },
        'The MCP servers and built-in data tools your agents can reach through toolyard.'),
      rows.length === 0
        ? el('div', { class: 'empty' }, 'No servers granted to you yet.')
        : el('table', {},
            el('thead', {}, el('tr', {},
              el('th', {}, 'Name'), el('th', {}, 'Kind'), el('th', {}, 'Tools'), el('th', {}, 'Status'))),
            el('tbody', {}, rows.map((g) => el('tr', {},
              el('td', {}, el('code', {}, g.name)),
              el('td', {}, groupKindBadge(g.kind)),
              el('td', { class: 'meta' }, g.tool_count != null ? String(g.tool_count) : '—'),
              el('td', {}, groupStatusBadge(g.status)),
            )))),
      el('p', { class: 'meta', style: 'margin: 12px 0 0;' }, 'Ask an admin for access to more servers.'),
    ),
  );
}

// ---- My connections (every user) --------------------------------------------
//
// A server whose "Who signs in" is "Each person" acts as each person's own
// account. This page is where a person connects theirs: Connect opens the
// provider's authorize page in a new tab (as the admin OAuth modal does),
// the callback stores the token on their row, and the list is re-read
// until it shows up. Members have no event stream, so a short poll plus a
// refresh on focus does the job; admins get the SSE event as well.

let connectionsSeq = 0;
async function loadConnections(clearError) {
  const seq = ++connectionsSeq;
  const c = state.connections;
  c.loading = true;
  if (clearError) c.error = '';
  render();
  try {
    const rows = await api('/v1/me/connections');
    if (seq !== connectionsSeq) return; // a newer load owns the page
    c.rows = rows || [];
    c.error = '';
    if (c.pending) {
      const p = c.rows.find((r) => r.server === c.pending);
      if (p && p.state === 'connected' && (p.connected_at || 0) >= c.pendingSince) {
        toast(c.pending + ': connected.');
        c.pending = null;
      }
    }
  } catch (e) {
    if (seq !== connectionsSeq) return;
    c.error = e.message;
  }
  c.loading = false;
  c.loaded = true;
  render();
}

// pollConnections re-reads the list every few seconds while a sign-in is
// in flight, for as long as the pending flow lives (ten minutes).
let connectionsPoll = null;
function pollConnections() {
  if (connectionsPoll) clearInterval(connectionsPoll);
  const started = Date.now();
  connectionsPoll = setInterval(() => {
    const c = state.connections;
    if (!c.pending || !state.user || Date.now() - started > 10 * 60 * 1000) {
      clearInterval(connectionsPoll); connectionsPoll = null;
      if (c.pending) { c.pending = null; render(); }
      return;
    }
    loadConnections();
  }, 3000);
}
window.addEventListener('focus', () => {
  if (state.user && state.route === 'connections') loadConnections();
});

async function connectServer(name) {
  const c = state.connections;
  c.busy = name; c.error = ''; render();
  try {
    const out = await api('/v1/me/connections/' + encodeURIComponent(name) + '/begin', { method: 'POST', body: {} });
    c.pending = name;
    c.pendingSince = Date.now() - 60 * 1000; // allow for clock skew between browser and server
    window.open(out.authorize_url, '_blank', 'noopener');
    pollConnections();
  } catch (e) { c.error = e.message; }
  c.busy = ''; render();
}

async function disconnectServer(name) {
  if (!confirm(`Disconnect your ${name} account? Your agents stop reaching ${name} until you connect again.`)) return;
  const c = state.connections;
  c.busy = name; c.error = ''; render();
  try {
    await api('/v1/me/connections/' + encodeURIComponent(name), { method: 'DELETE' });
    toast(name + ' disconnected.');
  } catch (e) { c.error = e.message; }
  c.busy = '';
  await loadConnections();
}

function connectionStateBadge(st, lastError) {
  switch (st) {
    case 'connected':    return el('span', { class: 'badge allowed' }, 'connected');
    case 'needs_reauth': return el('span', { class: 'badge denied', title: lastError || '' }, 'needs sign-in again');
    case 'expired':      return el('span', { class: 'badge expired' }, 'expired');
    default:             return el('span', { class: 'badge pending' }, 'not connected');
  }
}

function renderConnectionCard(r) {
  const c = state.connections;
  const busy = c.busy === r.server;
  const waiting = c.pending === r.server;
  const connected = r.state === 'connected';
  const facts = [connectionStateBadge(r.state, r.last_error)];
  if (r.account_label) facts.push(el('span', { class: 'meta' }, 'as ', el('code', {}, r.account_label)));
  if (connected && r.last_refresh_at) {
    facts.push(el('span', { class: 'meta', title: new Date(r.last_refresh_at).toLocaleString() }, 'refreshed ' + relTime(r.last_refresh_at)));
  } else if (connected && r.connected_at) {
    facts.push(el('span', { class: 'meta', title: new Date(r.connected_at).toLocaleString() }, 'connected ' + relTime(r.connected_at)));
  }
  if (!r.enabled) facts.push(el('span', { class: 'badge' }, 'server disabled'));
  else if (r.server_status === 'waiting_signin') facts.push(el('span', { class: 'meta' }, 'nobody has connected this server yet'));

  let actions;
  if (!r.ready) {
    actions = el('p', { class: 'meta' }, 'An admin still has to finish this server\'s OAuth setup before anyone can connect.');
  } else if (waiting) {
    actions = el('div', { class: 'row key-actions' },
      el('span', { class: 'meta grow' }, 'Finish signing in in the tab that just opened; this page updates on its own.'),
      el('button', { on: { click: () => { c.pending = null; render(); } } }, 'Cancel'),
    );
  } else {
    actions = el('div', { class: 'row key-actions' },
      el('button', {
        class: connected ? '' : 'primary', disabled: busy || !r.enabled,
        on: { click: () => connectServer(r.server) },
      }, busy ? 'Working…' : (r.state === 'needs_signin' ? 'Connect' : 'Reconnect')),
      r.state !== 'needs_signin'
        ? el('button', { class: 'danger', disabled: busy, on: { click: () => disconnectServer(r.server) } }, 'Disconnect')
        : null,
    );
  }
  return el('div', { class: 'card key-card' },
    el('h2', {}, r.server),
    r.host ? el('p', { class: 'meta' }, r.host) : null,
    el('div', { class: 'key-facts' }, facts),
    r.state === 'needs_reauth' && r.last_error ? el('div', { class: 'err' }, r.last_error) : null,
    actions,
  );
}

function viewConnections() {
  const c = state.connections;
  let body;
  if (!c.loaded) body = el('div', { class: 'card' }, el('div', { class: 'meta' }, 'Loading…'));
  else if (c.rows.length === 0) body = el('div', { class: 'card' }, el('div', { class: 'empty' }, isAdmin()
    ? 'No services require a personal account yet. Choose personal account access in Connections.'
    : 'Your available services do not require a personal account.'));
  else body = c.rows.map(renderConnectionCard);
  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'My accounts'),
      el('p', { class: 'meta', style: 'margin: 4px 0 0;' },
        'Connect your account so your agents can act on your behalf. Each person uses their own account.'),
      c.error ? el('div', { class: 'err' }, c.error) : null,
    ),
    body,
  );
}

// loadUsers refetches the Users page. clearError drops the last error line;
// the reload after a failed change keeps it so the reason stays visible.
let usersSeq = 0;
async function loadUsers(clearError) {
  const seq = ++usersSeq;
  state.users.loading = true;
  if (clearError) state.users.error = '';
  try {
    const r = await api('/v1/users');
    if (seq !== usersSeq) return; // a newer load owns the page
    state.users.rows = (r && r.users) || [];
    state.users.groups = (r && r.groups) || [];
  } catch (e) {
    if (seq !== usersSeq) return;
    state.users.error = e.message;
  }
  state.users.loading = false;
  state.users.loaded = true;
  render();
}

// usersFail keeps a 400/409 reason on the page (the toast fades) until the
// next change goes through.
function usersFail(e) {
  state.users.error = e.message;
  toast(e.message, 'error');
}

async function patchUser(u, patch, okMsg) {
  try {
    await api(`/v1/users/${encodeURIComponent(u.id)}`, { method: 'PATCH', body: patch });
    state.users.error = '';
    if (okMsg) toast(okMsg);
  } catch (e) { usersFail(e); }
  await loadUsers();
}

async function setUserRole(u, role) {
  const name = userLabel(u);
  const msg = role === 'admin'
    ? `Make ${name} an admin? Admins see every approval, audit row and server, and manage users.`
    : `Make ${name} a member? They keep their own agents and only reach the servers granted to them.`;
  if (!confirm(msg)) { render(); return; } // re-render resets the <select>
  await patchUser(u, { role }, `${name} is now ${role === 'admin' ? 'an admin' : 'a member'}`);
}

async function blockUser(u) {
  const name = userLabel(u);
  const reason = prompt(`Block ${name}? They can't sign in and their agents stop authenticating.\n\nReason (optional, shown on the Users page):`, '');
  if (reason === null) return;
  await patchUser(u, { status: 'blocked', blocked_reason: reason.trim() }, `${name} blocked`);
}

async function unblockUser(u) {
  await patchUser(u, { status: 'active' }, `${userLabel(u)} unblocked`);
}

async function revokeUserSessions(u) {
  const name = userLabel(u);
  const self = state.user && u.id === state.user.id;
  if (!confirm(self
    ? 'Sign yourself out everywhere? Every browser session you have open ends now, including this one.'
    : `Sign ${name} out everywhere? Every browser session they have open ends now.`)) return;
  try {
    await api(`/v1/users/${encodeURIComponent(u.id)}/revoke-sessions`, { method: 'POST', body: {} });
    // Your own row ends this session too: reload onto the sign-in screen.
    if (state.user && u.id === state.user.id) { history.replaceState(null, '', '/'); location.reload(); return; }
    state.users.error = '';
    toast(`${name} signed out everywhere`);
  } catch (e) { usersFail(e); }
  await loadUsers();
}

// ---- Beknown keys (Users page) ----
//
// POST issues a key (or rotates the one there), DELETE revokes it, and
// POST …/register retries the fingerprint registration. POST answers the
// key's status object, so a registration that failed shows up at once.

function userKeyState(k) {
  if (!k || !k.has_key) return 'none';
  return k.revealed ? 'active' : 'not revealed';
}

// keyNeedsRetry: a registration that isn't "registered", or a registry
// server ("register keys here") the key has no row for yet. Pending
// removals are retried by the same POST …/register (see keyRetryLabel).
function keyNeedsRetry(k) {
  if (!k || !k.has_key) return false;
  const regs = k.registrations || [];
  if (regs.some((r) => r.status !== 'registered')) return true;
  return (state.servers || []).some((s) => s.identity && s.identity.register && !regs.some((r) => r.upstream === s.name));
}

function keyPending(k) {
  return (k && k.pending_removals) || [];
}

// keyRetryLabel names the POST …/register button, or '' when there is
// nothing to retry: it re-registers the key and retries pending removals,
// and with no key left it only cleans up.
function keyRetryLabel(k) {
  if (keyNeedsRetry(k)) return 'Retry registration';
  if (keyPending(k).length) return 'Retry cleanup';
  return '';
}

function renderPendingRemovals(list) {
  if (!list || !list.length) return null;
  return el('div', { class: 'key-pending' },
    el('div', { class: 'meta' }, 'Old keys still being removed'),
    el('ul', {}, list.map((p) => el('li', {},
      el('code', {}, p.upstream), ' ',
      el('code', { title: p.fingerprint || '' }, shortFingerprint(p.fingerprint)),
      p.error ? [' ', el('span', { class: 'reg-error' }, p.error)] : null,
      p.updated_at ? el('span', { class: 'meta' }, ' · ' + relTime(toMs(p.updated_at))) : null,
    ))),
  );
}

async function userKeyAction(u, method, suffix, okMsg) {
  if (state.users.keyBusy) return;
  const name = userLabel(u);
  state.users.keyBusy = u.id; render();
  try {
    const opts = method === 'DELETE' ? { method } : { method, body: {} };
    const r = await api(`/v1/users/${encodeURIComponent(u.id)}/identity-key${suffix}`, opts);
    state.users.error = '';
    const failed = ((r && r.registrations) || []).filter((x) => x.status === 'error');
    const pending = keyPending(r);
    const notes = [];
    if (failed.length) notes.push(`Registration failed on ${failed.map((x) => x.upstream).join(', ')}: ${failed[0].error || 'error'}.`);
    if (pending.length) {
      notes.push(`An old key is still being removed from ${[...new Set(pending.map((x) => x.upstream))].join(', ')}` +
        (pending[0].error ? `: ${pending[0].error}.` : '.'));
    }
    toast(notes.length ? `${okMsg} ${notes.join(' ')}` : okMsg, notes.length ? 'error' : 'info');
  } catch (e) {
    const code = e.body && e.body.error;
    if (code === 'no_key' && suffix === '/register') {
      // Nothing left to register or remove.
      state.users.error = '';
      toast(`Nothing left to clean up for ${name}.`);
    } else {
      usersFail(code === 'no_clerk_identity'
        ? new Error(`${name} has no Google sign-in yet. They must sign in with Google once first.`)
        : e);
    }
  }
  state.users.keyBusy = '';
  if (state.user && u.id === state.user.id) loadMyKey();
  await loadUsers();
}

async function provisionUserKey(u) {
  const name = userLabel(u);
  const rotate = !!(u.identity_key && u.identity_key.has_key);
  if (rotate && !confirm(`Rotate ${name}'s Beknown key?\n\nThe current key is retired. ${name} gets a new key to reveal once on their Agents page and must paste it into T3 again.`)) return;
  await userKeyAction(u, 'POST', '', rotate
    ? `New Beknown key issued for ${name}. They reveal it once on their Agents page.`
    : `Beknown key provisioned for ${name}. They reveal it once on their Agents page.`);
}

async function revokeUserKey(u) {
  const name = userLabel(u);
  if (!confirm(`Revoke ${name}'s Beknown key?\n\nServers that forward identity will refuse ${name}'s calls until a new key is provisioned.`)) return;
  await userKeyAction(u, 'DELETE', '', `${name}'s Beknown key revoked.`);
}

async function retryUserKeyRegistration(u) {
  const has = !!(u.identity_key && u.identity_key.has_key);
  await userKeyAction(u, 'POST', '/register', has
    ? `Registration retried for ${userLabel(u)}.`
    : `Cleanup retried for ${userLabel(u)}.`);
}

// userKeyCell is the Users page "Beknown key" column: state chip, a warning
// when a registration failed, the actions, and the registrations on demand.
function userKeyCell(row) {
  const k = row.identity_key || { has_key: false };
  const regs = k.registrations || [];
  const st = userKeyState(k);
  const busy = !!state.users.keyBusy;
  const failed = regs.filter((r) => r.status === 'error');
  const pending = keyPending(k);
  const retryLabel = keyRetryLabel(k);
  return el('div', { class: 'stack' },
    el('div', { class: 'chips' },
      st === 'active' ? el('span', { class: 'badge allowed' }, 'active')
        : st === 'not revealed' ? el('span', { class: 'badge pending', title: 'Provisioned; the person hasn\'t revealed it yet' }, 'not revealed')
        : el('span', { class: 'badge' }, 'none'),
      failed.length
        ? el('span', {
            class: 'badge denied',
            title: failed.map((r) => r.upstream + ': ' + (r.error || 'error')).join('\n'),
          }, '⚠ registration error')
        : null,
      pending.length
        ? el('span', {
            class: 'badge pending',
            title: 'Old keys still being removed from ' + [...new Set(pending.map((p) => p.upstream))].join(', '),
          }, '⚠ cleanup pending')
        : null,
    ),
    st === 'none' && row.auth === 'password'
      ? el('span', { class: 'meta' }, 'Needs one Google sign-in first')
      : null,
    st === 'none'
      ? el('button', { disabled: busy, on: { click: () => provisionUserKey(row) } }, 'Provision')
      : el('div', { class: 'row' },
          el('button', { disabled: busy, on: { click: () => provisionUserKey(row) } }, 'Rotate'),
          el('button', { class: 'danger', disabled: busy, on: { click: () => revokeUserKey(row) } }, 'Revoke'),
        ),
    retryLabel
      ? el('button', { disabled: busy, on: { click: () => retryUserKeyRegistration(row) } }, retryLabel)
      : null,
    k.has_key || pending.length
      ? el('details', {
          class: 'key-regs', open: !!state.users.keyOpen[row.id],
          on: { toggle: (e) => { state.users.keyOpen[row.id] = e.target.open; } },
        },
          el('summary', {}, [
            k.has_key ? (regs.length === 1 ? '1 registration' : `${regs.length} registrations`) : null,
            pending.length ? `${pending.length} pending removal${pending.length === 1 ? '' : 's'}` : null,
          ].filter(Boolean).join(' · ')),
          k.has_key
            ? [
                el('div', { class: 'meta' }, 'Fingerprint ', el('code', { title: k.fingerprint || '' }, shortFingerprint(k.fingerprint))),
                renderRegistrations(regs),
              ]
            : null,
          renderPendingRemovals(pending),
        )
      : null,
  );
}

function openUserAccess(u) {
  const known = new Set((state.users.groups || []).map((g) => g.name));
  const selected = {};
  for (const s of (u.servers || [])) if (known.has(s)) selected[s] = true;
  state.userAccessModal = {
    id: u.id,
    name: userLabel(u),
    selected,
    // Grants for groups that no longer exist; saving drops them (sending
    // them back would be a 400 unknown server).
    dropped: (u.servers || []).filter((s) => !known.has(s)),
    error: '',
    saving: false,
  };
  render();
}

function closeUserAccess() {
  if (state.userAccessModal && state.userAccessModal.saving) return;
  state.userAccessModal = null;
  render();
}

async function saveUserAccess() {
  const m = state.userAccessModal;
  if (!m || m.saving) return;
  const servers = (state.users.groups || []).map((g) => g.name).filter((n) => m.selected[n]);
  m.saving = true; m.error = ''; render();
  try {
    await api(`/v1/users/${encodeURIComponent(m.id)}`, { method: 'PATCH', body: { servers } });
    if (state.userAccessModal === m) state.userAccessModal = null;
    state.users.error = '';
    toast(`Access saved for ${m.name}`);
    await loadUsers();
  } catch (e) {
    m.saving = false;
    m.error = e.message;
    if (state.userAccessModal !== m) usersFail(e);
    render();
  }
}

function renderUserAccessModal() {
  const m = state.userAccessModal;
  const groups = state.users.groups || [];
  const option = (g) => el('label', { class: 'access-opt' },
    el('input', {
      type: 'checkbox', checked: !!m.selected[g.name],
      on: { change: (e) => { m.selected[g.name] = e.target.checked; } },
    }),
    el('code', {}, g.name),
    el('span', { class: 'grow' }),
    el('span', { class: 'meta' }, g.tool_count != null ? `${g.tool_count} tools` : ''),
  );
  const section = (title, note, list) => el('div', { class: 'access-section' },
    el('h4', {}, title),
    note ? el('div', { class: 'meta' }, note) : null,
    list.length
      ? el('div', { class: 'access-list' }, list.map(option))
      : el('div', { class: 'meta' }, 'None available.'),
  );
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeUserAccess(); } } },
    el('div', { class: 'modal' },
      el('h3', {}, 'Edit access · ' + m.name),
      el('div', { class: 'meta' }, `Pick what ${m.name}'s agents may use. Meta-tools and the inbox are always available.`),
      section('Servers', null, groups.filter((g) => g.kind !== 'builtin')),
      section('Built-in data tools', 'Shared across all agents.', groups.filter((g) => g.kind === 'builtin')),
      m.dropped.length
        ? el('div', { class: 'meta', style: 'margin-top: 12px;' }, 'No longer configured, dropped on save: ' + m.dropped.join(', '))
        : null,
      m.error ? el('div', { class: 'err' }, m.error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { disabled: m.saving, on: { click: closeUserAccess } }, 'Cancel'),
        el('button', { class: 'primary', disabled: m.saving, on: { click: saveUserAccess } }, m.saving ? 'Saving…' : 'Save'),
      ),
    ),
  );
}

function viewUsers() {
  const u = state.users;
  const me = state.user || {};
  const table = u.rows.length === 0
    ? (u.error ? null : el('div', { class: 'empty' }, u.loaded ? 'No users yet.' : 'Loading users…'))
    : el('table', { class: 'users-table' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'User'), el('th', {}, 'Email'), el('th', {}, 'Role'), el('th', {}, 'Status'),
          el('th', {}, 'Servers'), el('th', {}, 'Beknown key'), el('th', {}, 'Agents'), el('th', {}, 'Last seen'), el('th', {}, ''))),
        el('tbody', {}, u.rows.map((row) => {
          const self = row.id === me.id;
          const admin = row.role === 'admin';
          const blocked = row.status === 'blocked';
          return el('tr', { style: blocked ? 'opacity: 0.75;' : '' },
            el('td', {}, el('div', { class: 'user-cell' },
              userAvatar(row, true),
              el('div', { class: 'who' },
                el('span', {}, userLabel(row), self ? el('span', { class: 'meta' }, ' (you)') : null),
                el('span', { class: 'meta' },
                  row.auth === 'clerk' ? 'Google' : row.auth === 'password' ? 'password' : (row.username || '')),
              ),
            )),
            el('td', { class: 'meta' }, row.email || '—'),
            el('td', {}, el('select', {
              disabled: self,
              title: self ? "You can't change your own role" : '',
              on: { change: (e) => setUserRole(row, e.target.value) },
            },
              el('option', { value: 'admin',  selected: admin }, 'admin'),
              el('option', { value: 'member', selected: !admin }, 'member'),
            )),
            el('td', {}, el('div', { class: 'stack' },
              blocked ? el('span', { class: 'badge denied' }, 'blocked') : el('span', { class: 'badge allowed' }, 'active'),
              blocked && row.blocked_reason ? el('span', { class: 'meta' }, row.blocked_reason) : null,
              blocked
                ? el('button', { on: { click: () => unblockUser(row) } }, 'Unblock')
                : el('button', {
                    class: 'danger', disabled: self,
                    title: self ? "You can't block yourself" : '',
                    on: { click: () => blockUser(row) },
                  }, 'Block'),
            )),
            el('td', {}, el('div', { class: 'stack' },
              admin
                ? el('div', { class: 'chips' }, el('span', { class: 'chip all' }, 'All servers'))
                : (row.servers || []).length
                  ? el('div', { class: 'chips' }, row.servers.map((s) => el('span', { class: 'chip' }, s)))
                  : el('span', { class: 'meta' }, 'No servers'),
              el('button', {
                disabled: admin,
                title: admin ? 'Admins can use every server' : '',
                on: { click: () => openUserAccess(row) },
              }, 'Edit access'),
            )),
            el('td', {}, userKeyCell(row)),
            el('td', { class: 'meta' }, String(row.agent_count || 0)),
            el('td', { class: 'meta' }, row.last_seen_at ? relTime(toMs(row.last_seen_at)) : 'never'),
            el('td', {}, el('button', { on: { click: () => revokeUserSessions(row) } }, 'Sign out everywhere')),
          );
        })));

  return el('div', {},
    state.userAccessModal ? renderUserAccessModal() : null,
    el('div', { class: 'card' },
      el('div', { class: 'agent-add-bar' },
        el('h2', { style: 'margin: 0;' }, 'People'),
        el('button', { disabled: u.loading, on: { click: () => loadUsers(true) } }, u.loading ? 'Refreshing…' : 'Refresh'),
      ),
      el('p', { class: 'meta', style: 'margin: 4px 0 12px;' },
        'Members manage only their own agents, and those agents reach only the servers granted here. Admins see and manage everything.'),
      u.error ? el('div', { class: 'err', style: 'margin: 0 0 12px;' }, u.error) : null,
      table,
    ),
  );
}

function viewServers() {
  const defaultTransport = state.authConfig?.environment === 'stage' ? 'http' : 'stdio';
  const transport = (state._serverDraft && state._serverDraft.transport) || defaultTransport;
  const draft = state._serverDraft || (state._serverDraft = { transport: defaultTransport });

  const setDraft = (k, v) => { draft[k] = v; render(); };

  const transportRow = el('div', { class: 'row' },
    el('label', { style: 'flex: 1;' },
      el('div', { class: 'meta' }, 'Transport'),
      el('select', {
        on: { change: (e) => setDraft('transport', e.target.value) }
      },
        state.authConfig?.environment !== 'stage' ? el('option', { value: 'stdio', selected: transport === 'stdio' }, 'stdio (subprocess)') : null,
        el('option', { value: 'http',            selected: transport === 'http'            }, 'streamable HTTP'),
      ),
    ),
    el('label', { style: 'flex: 1;' },
      el('div', { class: 'meta' }, 'Server name'),
      el('input', { id: 'srv-name', on: { input: (e) => { draft.name = e.target.value; } }, placeholder: 'e.g. github', value: draft.name || '' }),
    ),
  );

  const transportFields = transport === 'stdio'
    ? el('div', {},
        el('div', { class: 'row' },
          el('label', { style: 'flex: 1;' },
            el('div', { class: 'meta' }, 'Command'),
            el('input', { id: 'srv-cmd', on: { input: (e) => { draft.command = e.target.value; } }, placeholder: 'e.g. uvx', value: draft.command || '' }),
          ),
          el('label', { style: 'flex: 2;' },
            el('div', { class: 'meta' }, 'Args (one per line)'),
            el('textarea', { id: 'srv-args', on: { input: (e) => { draft.args = e.target.value.split('\n'); } }, placeholder: 'mcp-server-github', value: (draft.args || []).join('\n') }),
          ),
        ),
      )
    : el('label', {},
        el('div', { class: 'meta' }, 'URL'),
        el('input', { id: 'srv-url', on: { input: (e) => { draft.url = e.target.value; } }, placeholder: 'https://example.com/mcp', value: draft.url || '' }),
      );

  const envRow = el('label', {},
    el('div', { class: 'meta' }, 'Environment (KEY=VALUE per line, optional). Use ', el('code', {}, 'KEY=secret://NAME'), ' to reference a stored secret.'),
    el('textarea', { id: 'srv-env', on: { input: (e) => { draft._envText = e.target.value; } }, placeholder: 'GITHUB_PERSONAL_ACCESS_TOKEN=secret://GITHUB_TOKEN', value: draft._envText ?? draftEnvAsText(draft) }),
  );

  const headersRow = transport !== 'stdio' ? el('label', {},
    el('div', { class: 'meta' }, 'HTTP headers (Header: value per line, optional). Values may be ', el('code', {}, 'secret://NAME'), '.'),
    el('textarea', { id: 'srv-headers', on: { input: (e) => { draft._headersText = e.target.value; } }, placeholder: 'X-Api-Key: secret://MY_API_KEY', value: draft._headersText || '' }),
  ) : null;

  // Identity forwarding is per HTTP server; its fields live on the draft.
  if (draft.header == null) draft.header = IDENTITY_HEADER_DEFAULT;
  const identityRow = transport !== 'stdio' ? identityFields(draft, 'srv-id') : null;
  const whoRow = transport !== 'stdio' ? whoSignsInField(draft, 'srv-auth') : null;

  const installedByName = new Map(state.servers.map((s) => [s.name, s]));

  return el('div', {},
    state.marketModal ? renderMarketModal() : null,
    state.serverEditModal ? renderServerEditModal() : null,
    state.serverConnectionsModal ? renderServerConnectionsModal() : null,
    el('div', { class: 'card' },
      el('h2', {}, 'Browse popular MCP servers'),
      el('p', { class: 'meta' },
        'Curated recipes — one tap installs them, secret-bearing ones open a small form for the env values.'),
      state.marketplace.length === 0
        ? el('div', { class: 'empty' }, 'No marketplace entries.')
        : el('div', { class: 'market-grid' }, state.marketplace.map((m) => {
            const existing = installedByName.get(m.suggested_name);
            const needsEnv = (m.env || []).some((v) => v.required);
            const failed = existing && existing.last_status && existing.last_status !== 'ok';
            const multi = !!(m.auth && m.auth.multi_instance);
            const addBtn = el('button', {
              class: 'primary',
              on: { click: () => openMarketAdd(m, needsEnv) },
            }, existing ? 'Add another' : ((needsEnv || m.auth) ? 'Configure & add' : 'Add'));
            return el('div', { class: 'market-card' + (existing && !failed ? ' installed' : '') },
              el('div', { class: 'title' },
                el('span', { class: 'grow' }, m.name),
                el('span', { class: 'cat' }, m.category),
              ),
              el('div', { class: 'tag' }, m.tagline),
              el('div', { class: 'desc' }, m.description),
              m.notes ? el('div', { class: 'meta' }, 'Note: ', m.notes) : null,
              failed ? el('div', { class: 'err' }, existing.last_error || 'connect failed') : null,
              el('div', { class: 'actions' },
                m.homepage ? el('a', { href: m.homepage, target: '_blank', class: 'meta' }, 'docs ↗') : el('span'),
                existing
                  ? (failed
                      ? el('div', { class: 'row' },
                          el('button', { on: { click: () => reconnectServer(existing.name) }}, 'Retry'),
                          el('button', { class: 'danger', on: { click: () => removeServer(existing.name) }}, 'Remove'),
                        )
                      : el('div', { class: 'row' },
                          el('span', { class: 'badge allowed' }, 'installed'),
                          multi ? addBtn : null,
                        ))
                  : addBtn,
              ),
            );
          })),
    ),
    el('div', { class: 'card' },
      el('h2', {}, 'Import from JSON'),
      el('p', { class: 'meta' },
        'Paste a standard ', el('code', {}, 'mcpServers'), ' config block (or a single server entry) and it will be parsed and added automatically. ',
        'Secrets paste as plaintext — move them into the encrypted store afterward with the ', el('strong', {}, '🔑 Secret'), ' button on each row.'),
      el('label', {},
        el('textarea', {
          id: 'srv-import-json',
          rows: 8,
          placeholder: '"my-server": {\n  "command": "npx",\n  "args": ["-y", "some-mcp"],\n  "env": { "API_KEY": "..." }\n}',
          value: state._importJson || '',
          on: { input: (e) => { state._importJson = e.target.value; } },
        }),
      ),
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', { class: 'primary', on: { click: () => importServersFromJson() }}, 'Import'),
        el('button', { on: { click: () => { state._importJson = ''; render(); } } }, 'Clear'),
      ),
    ),
    el('div', { class: 'card' },
      el('h2', {}, 'Add custom MCP server'),
      el('p', { class: 'meta' },
        'Connect an upstream MCP server. Its tools are wrapped with the required _reason field, fed through the policy engine, and surface in the catalog as ',
        el('code', {}, '<server>.<tool-name>'), '.'),
      transportRow,
      transportFields,
      envRow,
      headersRow,
      whoRow,
      identityRow,
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', { class: 'primary', on: { click: () => addServer() }}, 'Add server'),
        el('button', { on: { click: () => { state._serverDraft = { transport: defaultTransport }; render(); } } }, 'Reset'),
      ),
    ),
    el('div', { class: 'card' },
      el('h2', {}, 'Connected servers'),
      state.servers.length === 0
        ? el('div', { class: 'empty' }, 'No upstream MCP servers yet. Built-in tools (memory.*, tools.*, fixture.echo) are still available.')
        : el('table', {}, el('thead', {}, el('tr', {},
            el('th', {}, 'Name'),
            el('th', {}, 'Transport'),
            el('th', {}, 'Tools'),
            el('th', {}, 'Status'),
            el('th', {}, 'Auth'),
            el('th', {}, 'Who signs in'),
            el('th', {}, 'Policy'),
            el('th', {}, ''))),
            el('tbody', {}, state.servers.map((s) => el('tr', {},
              el('td', {}, el('code', {}, s.name),
                serverUsesSecret(s) ? el('span', { title: 'references a stored secret', style: 'margin-left:6px;' }, '🔒') : null,
                identityBadges(s)),
              el('td', {}, transportLabel(s)),
              el('td', {}, String(s.tool_count || 0)),
              el('td', {}, serverStatusBadge(s)),
              el('td', {}, oauthBadge(s)),
              el('td', {}, whoSignsInCell(s)),
              el('td', {}, (() => {
                const cur = upstreamPolicyMode(s.name);
                const sel = el('select', { title: 'Per-upstream gate (applies to all this server’s tools unless a tool has its own policy)',
                  on: { change: (e) => setUpstreamPolicy(s.name, e.target.value) } },
                  ...[['default', 'Default'], ['allow', 'Allow'], ['ask', 'Ask'], ['deny', 'Deny']].map(([v, l]) =>
                    el('option', { value: v }, l)));
                sel.value = cur;
                return sel;
              })()),
              el('td', {},
                el('div', { class: 'row' },
                  isHTTPUpstream(s) ? el('button', { on: { click: () => openOAuthPanel(s.name) }}, 'Auth…') : null,
                  (s.env_plaintext_keys && s.env_plaintext_keys.length)
                    ? el('button', { title: 'Move a plaintext env value into the encrypted secrets store', on: { click: () => convertEnvToSecret(s.name, s.env_plaintext_keys) }}, '🔑 Secret')
                    : null,
                  el('button', { on: { click: () => openServerEdit(s) }}, 'Edit'),
                  el('button', { on: { click: () => reconnectServer(s.name) }}, 'Reconnect'),
                  el('button', { class: 'danger', on: { click: () => removeServer(s.name) }}, 'Remove'),
                ),
              ),
            )))),
    ),
    state.oauthFlow ? renderOAuthModal() : null,
  );
}

function transportLabel(s) {
 if (s.transport === 'stdio') return s.command;
 try { const u = new URL(s.url); return u.host + u.pathname; } catch { return 'HTTP'; }
}

// serverStatusBadge: connected, or the recorded failure. A per_user server
// nobody has connected yet is not failing: its tools arrive with the first
// sign-in.
function serverStatusBadge(s) {
  if (s.last_status === 'ok') return el('span', { class: 'badge allowed' }, 'connected');
  if (s.last_status === 'waiting_signin') {
    return el('span', { class: 'badge pending', title: 'No one has connected this server yet; its tools appear after the first sign-in on My connections.' },
      'waiting for a first sign-in');
  }
  return el('span', { class: 'badge denied', title: s.last_error || '' }, s.last_status || 'error');
}

// ---- who signs in (auth_mode) ----
//
// shared: one account for everyone (the OAuth token on the Auth… panel);
// per_user: each person connects their own on My connections, and an
// agent's calls use its owner's account, never a shared one.

const AUTH_MODE_LABEL = { shared: 'One shared account', per_user: 'Each person' };

function isPerUserServer(s) { return s && s.auth_mode === 'per_user'; }

// whoSignsInField is the add/edit form control, bound to d.auth_mode.
function whoSignsInField(d, id) {
  if (!d.auth_mode) d.auth_mode = 'shared';
  const sel = el('select', { id, on: { change: (e) => { d.auth_mode = e.target.value; } } },
    el('option', { value: 'shared' }, AUTH_MODE_LABEL.shared),
    el('option', { value: 'per_user' }, AUTH_MODE_LABEL.per_user));
  sel.value = d.auth_mode;
  return el('label', {},
    el('div', { class: 'meta' }, 'Who signs in'),
    sel,
    el('div', { class: 'meta' },
      'One shared account: every change goes as that account (set it up under Auth…). ',
      'Each person: people connect their own account on My connections and their agents act as them; nothing ever falls back to a shared account.'),
  );
}

// whoSignsInCell is the per-row control on the Servers table: a select
// that PATCHes auth_mode, and for per_user servers a look at who has
// connected.
function whoSignsInCell(s) {
  if (!isHTTPUpstream(s)) return el('span', { class: 'meta' }, '—');
  const cur = s.auth_mode || 'shared';
  const sel = el('select', {
    title: 'Who signs in to this server',
    on: { change: (e) => setServerAuthMode(s, e.target.value) },
  },
    el('option', { value: 'shared' }, AUTH_MODE_LABEL.shared),
    el('option', { value: 'per_user' }, AUTH_MODE_LABEL.per_user));
  sel.value = cur;
  return el('div', { class: 'row' },
    sel,
    cur === 'per_user'
      ? el('button', { title: 'Who has connected their account to this server', on: { click: () => openServerConnections(s.name) } }, 'Who\'s connected…')
      : null,
  );
}

async function setServerAuthMode(s, mode) {
  if (mode === (s.auth_mode || 'shared')) return;
  const msg = mode === 'per_user'
    ? `Make ${s.name} act as each person's own account? Agents whose owner has not connected on My connections get a clear refusal instead of running as the shared account. The shared account's token, if any, stays stored but is not used.`
    : `Make ${s.name} act as one shared account for everyone? People's own sign-ins stay stored but stop being used (and refreshed) until you switch back.`;
  if (!confirm(msg)) { render(); return; } // re-render resets the <select>
  try {
    const out = await api('/v1/servers/' + encodeURIComponent(s.name), { method: 'PATCH', body: { auth_mode: mode } });
    const warning = out && out.warning;
    if (warning) toast(`Saved ${s.name}, but it failed to reconnect: ${warning}`, 'error');
    else toast(`${s.name}: ${AUTH_MODE_LABEL[mode].toLowerCase()}`);
  } catch (e) { toast(e.message, 'error'); }
  await reloadServers();
  await loadOAuthStatus(s.name);
  render();
}

// openServerConnections shows the admin who has connected a per_user
// server (GET /v1/servers/{name}/connections): names and states, never
// tokens.
async function openServerConnections(name) {
  const m = { name, loading: true, data: null, error: '' };
  state.serverConnectionsModal = m;
  render();
  try {
    m.data = await api('/v1/servers/' + encodeURIComponent(name) + '/connections');
  } catch (e) { m.error = e.message; }
  m.loading = false;
  if (state.serverConnectionsModal === m) render();
}

function renderServerConnectionsModal() {
  const m = state.serverConnectionsModal;
  const close = () => { state.serverConnectionsModal = null; render(); };
  const rows = (m.data && m.data.connections) || [];
  let body;
  if (m.loading) body = el('div', { class: 'meta' }, 'Loading…');
  else if (m.error) body = el('div', { class: 'err' }, m.error);
  else if (!rows.length) body = el('div', { class: 'empty' }, 'Nobody has connected yet. The server gets its tools from the first person who signs in on My connections.');
  else body = el('table', {},
    el('thead', {}, el('tr', {}, el('th', {}, 'Person'), el('th', {}, 'State'), el('th', {}, 'Account'), el('th', {}, 'Last refresh'))),
    el('tbody', {}, rows.map((r) => el('tr', {},
      el('td', {}, r.label || r.email || r.user_id, r.email && r.label ? el('div', { class: 'meta' }, r.email) : null),
      el('td', {}, connectionStateBadge(r.state, r.last_error)),
      el('td', { class: 'meta' }, r.account_label || '—'),
      el('td', { class: 'meta' }, r.last_refresh_at ? relTime(r.last_refresh_at) : (r.connected_at ? 'connected ' + relTime(r.connected_at) : '—')),
    ))));
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) close(); } } },
    el('div', { class: 'modal modal-wide' },
      el('h3', {}, 'Who signs in to ', m.name),
      m.data && m.data.waiting_sign_in ? el('div', { class: 'meta' }, 'Waiting for a first sign-in.') : null,
      body,
      el('div', { class: 'row modal-actions', style: 'justify-content: flex-end;' },
        el('button', { on: { click: close } }, 'Close'),
      ),
    ),
  );
}

// serverUsesSecret reports whether any env/header value on a (masked) server
// is a secret:// reference, so the list can show a lock badge.
function serverUsesSecret(s) {
  const refs = (m) => Object.values(m || {}).some((v) => typeof v === 'string' && v.startsWith('secret://'));
  return refs(s.env) || refs(s.headers);
}

// ---- per-server identity forwarding ----
//
// A server with identity set gets the caller's Beknown key in `header` on
// every tool call (toolyard refuses callers without one); `register` marks
// a server where toolyard registers each key's fingerprint.

const IDENTITY_HEADER_DEFAULT = 'x-bk-bifrost-vk';
const HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+.^_`|~-]+$/;
// Masked() shows plaintext header/env values as this; sending it back would
// overwrite the real value.
const MASKED_VALUE = '•••';

function identityDraftError(d) {
  const h = (d.header || '').trim() || IDENTITY_HEADER_DEFAULT;
  return HEADER_NAME_RE.test(h) ? '' : `"${h}" isn't a valid header name.`;
}

function identityFromDraft(d) {
  return { header: (d.header || '').trim() || IDENTITY_HEADER_DEFAULT, register: !!d.register };
}

// identityFields renders the "Identity" controls onto d { identityOn,
// header, register }. Ticking the box enables its fields in place rather
// than re-rendering, so the rest of the add form keeps what was typed.
function identityFields(d, idPrefix) {
  const dep = (node) => { node.disabled = !d.identityOn; return node; };
  const box = el('div', { class: 'identity-fields' },
    el('h4', {}, 'Identity'),
    el('label', { class: 'check-row' },
      el('input', {
        type: 'checkbox', id: idPrefix + '-on', checked: !!d.identityOn,
        on: { change: (e) => {
          d.identityOn = e.target.checked;
          box.querySelectorAll('.id-dep').forEach((n) => { n.disabled = !d.identityOn; });
        } },
      }),
      el('span', {}, 'Forward each person\'s Beknown key'),
    ),
    el('div', { class: 'meta' },
      'toolyard sends the caller\'s key on every tool call, and refuses calls from people who have no key.'),
    el('label', {},
      el('div', { class: 'meta' }, 'Header name'),
      dep(el('input', {
        id: idPrefix + '-header', class: 'id-dep', value: d.header == null ? IDENTITY_HEADER_DEFAULT : d.header,
        placeholder: IDENTITY_HEADER_DEFAULT,
        on: { input: (e) => { d.header = e.target.value; } },
      })),
    ),
    el('label', { class: 'check-row' },
      dep(el('input', {
        type: 'checkbox', id: idPrefix + '-register', class: 'id-dep', checked: !!d.register,
        on: { change: (e) => { d.register = e.target.checked; } },
      })),
      el('span', {}, 'Register keys here'),
    ),
    el('div', { class: 'meta' },
      'Tick on a server that exposes the key registry tools (', el('code', {}, 'upsert-bifrost-virtual-key-actor'),
      '). toolyard registers each person\'s key fingerprint in prime-service through it.'),
  );
  return box;
}

function identityBadges(s) {
  const id = s.identity;
  if (!id || !id.header) return null;
  return [
    el('span', { class: 'badge identity inline-badge', title: 'Forwards each caller\'s Beknown key in ' + id.header }, 'identity'),
    id.register
      ? el('span', { class: 'badge identity inline-badge', title: 'toolyard registers key fingerprints here' }, 'key registry')
      : null,
  ];
}

// ---- edit server (PATCH /v1/servers/{name}) ----

function headersAsText(h) {
  return Object.entries(h || {}).map(([k, v]) => k + ': ' + v).join('\n');
}

function openServerEdit(s) {
  const id = s.identity && s.identity.header ? s.identity : null;
  const m = {
    name: s.name,
    http: !!isHTTPUpstream(s),
    url: s.url || '',
    headersText: headersAsText(s.headers),
    identityOn: !!id,
    header: id ? id.header : IDENTITY_HEADER_DEFAULT,
    register: !!(id && id.register),
    auth_mode: s.auth_mode || 'shared',
    enabled: s.enabled !== false,
    error: '',
    saving: false,
  };
  m.orig = { url: m.url, headersText: m.headersText, identityOn: m.identityOn, header: m.header, register: m.register, auth_mode: m.auth_mode, enabled: m.enabled };
  state.serverEditModal = m;
  render();
}

function closeServerEdit() {
  if (state.serverEditModal && state.serverEditModal.saving) return;
  state.serverEditModal = null;
  render();
}

// serverEditBody is the partial PATCH body: only the fields that changed.
// Returns { error } when the form can't be sent.
function serverEditBody(m) {
  const o = m.orig;
  const body = {};
  if (m.http) {
    const url = m.url.trim();
    if (!url) return { error: 'URL is required.' };
    if (url !== o.url) body.url = url;
    if (m.headersText !== o.headersText) {
      const headers = parseHeadersText(m.headersText);
      const masked = Object.keys(headers).filter((k) => headers[k] === MASKED_VALUE);
      if (masked.length) {
        return { error: `Re-enter the value for ${masked.join(', ')}: saved values show as ${MASKED_VALUE} and can't be sent back. Use secret://NAME for secrets, or delete the line to drop the header.` };
      }
      body.headers = headers;
    }
    if (m.identityOn) {
      const idErr = identityDraftError(m);
      if (idErr) return { error: idErr };
    }
    const next = identityFromDraft(m);
    const changed = m.identityOn !== o.identityOn ||
      (m.identityOn && (next.header !== o.header || next.register !== o.register));
    if (changed) body.identity = m.identityOn ? next : null;
    if (m.auth_mode !== o.auth_mode) body.auth_mode = m.auth_mode;
  }
  if (m.enabled !== o.enabled) body.enabled = m.enabled;
  return { body };
}

async function saveServerEdit() {
  const m = state.serverEditModal;
  if (!m || m.saving) return;
  const { body, error } = serverEditBody(m);
  if (error) { m.error = error; render(); return; }
  if (!Object.keys(body).length) { state.serverEditModal = null; toast('No changes'); render(); return; }
  m.saving = true; m.error = ''; render();
  try {
    // 200 is the masked server; 202 is { server, warning } when the row was
    // saved but the reconnect failed.
    // Either may carry oauth_reset: the URL moved to another origin, so the
    // stored OAuth client and tokens were dropped.
    const out = await api('/v1/servers/' + encodeURIComponent(m.name), { method: 'PATCH', body });
    if (state.serverEditModal === m) state.serverEditModal = null;
    const srv = (out && out.server) || out || {};
    const warning = out && out.warning;
    if (srv.oauth_reset || (out && out.oauth_reset)) oauthResetNote(m.name, warning);
    else if (warning) toast(`Saved ${m.name}, but it failed to reconnect: ${warning}`, 'error');
    else toast(`Saved ${m.name}`);
    await reloadServers();
    await loadOAuthStatus(m.name);
    render();
  } catch (e) {
    m.saving = false;
    m.error = e.status === 403
      ? (e.message === 'admin_only' ? 'Only admins can edit servers.' : `${m.name} is a reserved built-in server and can't be edited.`)
      : e.status === 404 ? `${m.name} no longer exists. It may have been removed.`
      : e.message;
    if (e.status === 404) reloadServers().then(render);
    render();
  }
}

// oauthResetNote is a banner that stays until dismissed (a toast fades):
// the server must be authorised again before its tools work.
function oauthResetNote(name, warning) {
  document.querySelectorAll('.toast').forEach((n) => n.remove());
  const t = el('div', { class: 'toast stale' },
    el('span', { class: 'grow' },
      `Saved ${name}. OAuth was reset — authorise this server again.`,
      warning ? ` Reconnect failed: ${warning}` : ''),
    el('button', { class: 'link', on: { click: () => { t.remove(); openOAuthPanel(name); } } }, 'Authorise…'),
    el('button', { class: 'link', on: { click: () => t.remove() } }, 'Dismiss'),
  );
  document.body.appendChild(t);
}

function renderServerEditModal() {
  const m = state.serverEditModal;
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeServerEdit(); } } },
    el('div', { class: 'modal modal-wide server-edit' },
      el('h3', {}, 'Edit server · ' + m.name),
      m.http ? [
        el('label', {},
          el('div', { class: 'meta' }, 'URL'),
          el('input', {
            id: 'srv-edit-url', value: m.url, placeholder: 'https://example.com/mcp',
            on: { input: (e) => { m.url = e.target.value; } },
          }),
        ),
        el('label', {},
          el('div', { class: 'meta' },
            'HTTP headers (Header: value per line). Values may be ', el('code', {}, 'secret://NAME'),
            `. Saved plaintext values show as ${MASKED_VALUE}: leave this box as it is to keep them, or re-enter them if you edit it.`),
          el('textarea', {
            id: 'srv-edit-headers', placeholder: 'X-Api-Key: secret://MY_API_KEY', value: m.headersText,
            on: { input: (e) => { m.headersText = e.target.value; } },
          }),
        ),
        whoSignsInField(m, 'srv-edit-auth'),
        identityFields(m, 'srv-edit-id'),
      ] : el('div', { class: 'meta' }, 'URL, headers, who signs in and identity forwarding apply to HTTP servers only.'),
      el('label', { class: 'check-row' },
        el('input', {
          type: 'checkbox', id: 'srv-edit-enabled', checked: m.enabled,
          on: { change: (e) => { m.enabled = e.target.checked; } },
        }),
        el('span', {}, 'Enabled'),
      ),
      m.error ? el('div', { class: 'err' }, m.error) : null,
      el('div', { class: 'row modal-actions' },
        el('button', { disabled: m.saving, on: { click: closeServerEdit } }, 'Cancel'),
        el('button', { class: 'primary', disabled: m.saving, on: { click: saveServerEdit } }, m.saving ? 'Saving…' : 'Save'),
      ),
    ),
  );
}

// convertEnvToSecret moves a plaintext env value into the encrypted secrets
// store and rewrites the server's env to a secret:// reference.
async function convertEnvToSecret(serverName, plaintextKeys) {
  const envKey = plaintextKeys.length === 1
    ? plaintextKeys[0]
    : prompt('Which env key to convert? (' + plaintextKeys.join(', ') + ')', plaintextKeys[0]);
  if (!envKey || !plaintextKeys.includes(envKey)) return;
  const suggested = envKey.toUpperCase().replace(/[^A-Z0-9_]/g, '_').replace(/^[^A-Z]+/, '');
  const secretName = prompt('Name for the stored secret:', suggested || 'SECRET');
  if (!secretName) return;
  try {
    await api('/v1/servers/' + encodeURIComponent(serverName) + '/convert-env', {
      method: 'POST', body: { env_key: envKey, secret_name: secretName },
    });
    toast('Converted ' + envKey + ' → secret://' + secretName);
    reloadServers(); render();
    state.secretsLoaded = false;
  } catch (e) { toast(e.message, 'error'); }
}

function draftEnvAsText(d) {
  if (!d.env) return '';
  return Object.entries(d.env).map(([k, v]) => k + '=' + v).join('\n');
}

function parseEnvText(t) {
  const out = {};
  (t || '').split(/\r?\n/).forEach((line) => {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) return;
    const i = trimmed.indexOf('=');
    if (i <= 0) return;
    out[trimmed.slice(0, i).trim()] = trimmed.slice(i + 1);
  });
  return out;
}

// parseHeadersText parses "Header: value" lines into a map.
function parseHeadersText(t) {
  const out = {};
  (t || '').split(/\r?\n/).forEach((line) => {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) return;
    const i = trimmed.indexOf(':');
    if (i <= 0) return;
    out[trimmed.slice(0, i).trim()] = trimmed.slice(i + 1).trim();
  });
  return out;
}

// RESERVED_SERVER_NAMES mirrors the backend's reserved list in
// internal/upstreams/upstreams.go so we can reject doomed imports early.
const RESERVED_SERVER_NAMES = new Set(['builtin', 'fixture', 'memory', 'tools', 'mempalace', 'notes', 'skills']);

// normalizeTransport maps a Claude-format `type` (or an inferred kind) onto the
// transport values the backend accepts. Returns '' for unsupported transports.
function normalizeTransport(type) {
  switch (String(type || '').toLowerCase().replace(/[_-]/g, '')) {
    case 'stdio': return 'stdio';
    case 'http':
    case 'streamablehttp':
    case 'httpstream': return 'http';
    case 'sse': return ''; // not supported by the backend
    default: return '';
  }
}

// parseMcpJson tolerantly parses a pasted MCP config block (the standard
// Claude Desktop / .mcp.json `mcpServers` format, a bare name→config map, or a
// single entry) and normalizes each entry into a POST /v1/servers body.
// Returns { servers: [...], errors: [...] } — errors are human-readable strings.
function parseMcpJson(text) {
  const errors = [];
  const raw = (text || '').trim();
  if (!raw) return { servers: [], errors: ['Paste a JSON config first.'] };

  // Tolerant load: try as-is, then wrapped in braces (for bare comma-separated
  // "name": {…} fragments) and with trailing commas stripped.
  let parsed = null;
  const attempts = [raw];
  if (!raw.startsWith('{')) attempts.push('{' + raw + '}');
  for (const candidate of attempts) {
    const cleaned = candidate.replace(/,\s*([}\]])/g, '$1');
    try { parsed = JSON.parse(cleaned); break; } catch (e) { /* try next */ }
  }
  if (parsed === null || typeof parsed !== 'object') {
    return { servers: [], errors: ['Could not parse JSON. Paste a valid mcpServers block or a single server entry.'] };
  }

  // Locate the name→config map.
  let entries;
  const isObj = (v) => v && typeof v === 'object' && !Array.isArray(v);
  if (isObj(parsed.mcpServers)) {
    entries = Object.entries(parsed.mcpServers);
  } else if (isObj(parsed) && (parsed.command || parsed.url || parsed.type)) {
    entries = [[null, parsed]]; // single unnamed entry
  } else if (isObj(parsed)) {
    entries = Object.entries(parsed);
  } else {
    return { servers: [], errors: ['Unrecognized config shape.'] };
  }

  const servers = [];
  for (const [key, cfg] of entries) {
    if (!isObj(cfg)) { errors.push(`"${key}": skipped (not a server object)`); continue; }

    // Derive a name: explicit field, the object key, or the command basename.
    let name = (cfg.name || key || '').trim();
    if (!name && cfg.command) name = String(cfg.command).split(/[\\/]/).pop().replace(/\.[^.]+$/, '');
    if (!name) { errors.push('An entry is missing a server name — give it a key.'); continue; }
    if (/[ \t\n.]/.test(name)) { errors.push(`"${name}": skipped (name must not contain spaces or dots)`); continue; }
    if (RESERVED_SERVER_NAMES.has(name)) { errors.push(`"${name}": skipped (reserved name)`); continue; }

    // Resolve transport: explicit type wins, else infer from command/url.
    let transport;
    if (cfg.type) {
      transport = normalizeTransport(cfg.type);
      if (!transport) { errors.push(`"${name}": skipped (transport "${cfg.type}" not supported)`); continue; }
    } else if (cfg.command) {
      transport = 'stdio';
    } else if (cfg.url) {
      transport = 'http';
    } else {
      errors.push(`"${name}": skipped (no command or url)`); continue;
    }

    const body = { name, transport };
    if (transport === 'stdio') {
      if (!cfg.command) { errors.push(`"${name}": skipped (stdio requires a command)`); continue; }
      body.command = String(cfg.command);
      if (Array.isArray(cfg.args)) body.args = cfg.args.map(String);
    } else {
      if (!cfg.url) { errors.push(`"${name}": skipped (http requires a url)`); continue; }
      body.url = String(cfg.url);
      if (isObj(cfg.headers)) {
        body.headers = {};
        for (const [k, v] of Object.entries(cfg.headers)) body.headers[k] = String(v);
      }
    }
    if (isObj(cfg.env)) {
      body.env = {};
      for (const [k, v] of Object.entries(cfg.env)) body.env[k] = String(v);
    }
    servers.push(body);
  }

  if (servers.length === 0 && errors.length === 0) errors.push('No server entries found in the pasted JSON.');
  return { servers, errors };
}

// importServersFromJson parses the pasted config and POSTs each entry to the
// existing /v1/servers endpoint, then reports a per-server summary.
async function importServersFromJson() {
  const elInput = $('srv-import-json');
  const { servers, errors } = parseMcpJson(elInput ? elInput.value : '');
  if (servers.length === 0) { toast(errors[0] || 'Nothing to import', 'error'); return; }

  let connected = 0, savedNotConnected = 0, skipped = 0, failed = 0;
  const notes = [...errors];
  for (const body of servers) {
    try {
      const resp = await fetch('/v1/servers', {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'toolyard' },
        body: JSON.stringify(body),
      });
      let out = {};
      try { out = await resp.json(); } catch (e) { /* ignore */ }
      if (resp.status === 202) {
        savedNotConnected++;
        notes.push(`${body.name}: saved but not connected (${out.warning || 'unknown'})`);
      } else if (resp.status === 409) {
        skipped++;
        notes.push(`${body.name}: already exists`);
      } else if (!resp.ok) {
        failed++;
        notes.push(`${body.name}: ${out.error || ('HTTP ' + resp.status)}`);
      } else {
        connected++;
      }
    } catch (e) {
      failed++;
      notes.push(`${body.name}: ${e.message}`);
    }
  }

  const parts = [];
  if (connected) parts.push(`${connected} connected`);
  if (savedNotConnected) parts.push(`${savedNotConnected} saved (not connected)`);
  if (skipped) parts.push(`${skipped} skipped`);
  if (failed) parts.push(`${failed} failed`);
  const summary = parts.length ? parts.join(', ') : 'nothing imported';
  toast('Import: ' + summary, (failed || errors.length) ? 'error' : 'success');
  if (notes.length) console.warn('MCP import notes:\n' + notes.join('\n'));

  if (connected || savedNotConnected) {
    state._importJson = '';
    await reloadServers();
  }
  render();
}

async function addServer() {
  const draft = state._serverDraft || {};
  const body = {
    name:      $('srv-name').value.trim(),
    transport: draft.transport || 'stdio',
  };
  if (body.transport === 'stdio') {
    body.command = $('srv-cmd').value.trim();
    body.args = $('srv-args').value.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
  } else {
    body.url = $('srv-url').value.trim();
    const hdrEl = $('srv-headers');
    if (hdrEl && hdrEl.value.trim()) body.headers = parseHeadersText(hdrEl.value);
    if (draft.identityOn) {
      const idErr = identityDraftError(draft);
      if (idErr) { toast(idErr, 'error'); return; }
      body.identity = identityFromDraft(draft);
    }
    body.auth_mode = draft.auth_mode || 'shared';
  }
  body.env = parseEnvText($('srv-env').value);
  if (!body.name) { toast('name required', 'error'); return; }
  try {
    const resp = await fetch('/v1/servers', {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'toolyard' },
      body: JSON.stringify(body),
    });
    const out = await resp.json();
    if (resp.status === 202) {
      toast('Saved, but failed to connect: ' + (out.warning || 'unknown'), 'error');
    } else if (!resp.ok) {
      throw new Error(out.error || ('HTTP ' + resp.status));
    } else if (out && out.last_status === 'waiting_signin') {
      toast('Saved. Its tools appear once the first person connects on My connections.');
    } else {
      toast('Connected.');
    }
    state._serverDraft = { transport: 'stdio' };
    await reloadServers();
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function uniqueServerName(base) {
  const taken = new Set(state.servers.map((s) => s.name));
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) {
    const candidate = base + '-' + i;
    if (!taken.has(candidate)) return candidate;
  }
}

function openMarketAdd(entry, needsEnv) {
  if (!needsEnv && !entry.auth) {
    installFromMarket(entry, {});
    return;
  }
  state.marketModal = { entry, env: {}, name: uniqueServerName(entry.suggested_name), error: '' };
  for (const v of entry.env || []) {
    state.marketModal.env[v.name] = v.default || '';
  }
  if (entry.auth) {
    const options = {};
    for (const o of entry.auth.options || []) options[o.param] = o.default || '';
    Object.assign(state.marketModal, {
      authMode: entry.auth.supports_managed ? 'managed' : 'byo',
      clientId: '',
      clientSecret: '',
      options,
      pat: '',
      paste: '',
      phase: 'form', // 'form' | 'waiting' (authorize tab open, awaiting callback)
      installedName: '',
      busy: false,
    });
  }
  render();
}

function closeMarketModal() {
  state.marketModal = null;
  render();
}

function renderMarketModal() {
  const modal = state.marketModal;
  const { entry, env, name, error } = modal;

  if (entry.auth && modal.phase === 'waiting') {
    return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeMarketModal(); } } },
      el('div', { class: 'modal' },
        el('h3', {}, 'Authorize ', modal.installedName),
        el('div', { class: 'meta' }, 'Authorization page opened in a new tab — complete it there.'),
        el('div', { class: 'meta' }, 'This dialog closes automatically once the provider redirects back.'),
        el('label', { style: 'margin-top: 8px;' },
          el('div', { class: 'meta' }, 'Different browser? Paste the post-redirect URL here:'),
          el('textarea', {
            placeholder: 'https://your-callback/cb?code=…&state=…',
            rows: 3,
            on: { input: (e) => { modal.paste = e.target.value; } },
          }),
          el('div', { class: 'row', style: 'margin-top: 4px;' },
            el('button', { class: 'primary', disabled: modal.busy, on: { click: () => marketSubmitPaste() } },
              modal.busy ? 'Submitting…' : 'Submit pasted URL'),
          ),
        ),
        error ? el('div', { class: 'err' }, error) : null,
        el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
          el('button', { on: { click: closeMarketModal } }, 'Close'),
        ),
      ),
    );
  }

  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeMarketModal(); } } },
    el('div', { class: 'modal' },
      el('h3', {}, 'Add ', entry.name),
      el('div', { class: 'meta' }, entry.tagline),
      el('label', {},
        el('div', { class: 'meta' }, 'Server name (must be unique)'),
        el('input', {
          value: name,
          on: { input: (e) => { state.marketModal.name = e.target.value; } },
        }),
      ),
      ...(entry.env || []).map((v) => el('label', {},
        el('div', { class: 'meta' }, v.name, v.required ? ' *' : '', v.secret ? ' (secret)' : ''),
        el('input', {
          type: v.secret ? 'password' : 'text',
          value: env[v.name] || '',
          placeholder: v.description || '',
          autocomplete: v.secret ? 'off' : '',
          on: { input: (e) => { state.marketModal.env[v.name] = e.target.value; } },
        }),
        v.description ? el('div', { class: 'meta', style: 'margin-top: 2px;' }, v.description) : null,
      )),
      entry.auth ? renderMarketAuthSection(modal) : null,
      error ? el('div', { class: 'err' }, error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { on: { click: closeMarketModal } }, 'Cancel'),
        el('button', { class: 'primary', disabled: modal.busy, on: { click: () => {
          // Validate required env now.
          for (const v of entry.env || []) {
            if (v.required && !((state.marketModal.env[v.name] || '').trim())) {
              state.marketModal.error = v.name + ' is required';
              render();
              return;
            }
          }
          if (entry.auth) {
            if (modal.authMode === 'byo' && !modal.clientId.trim()) {
              modal.error = 'client_id is required'; render(); return;
            }
            if (modal.authMode === 'pat' && !modal.pat.trim()) {
              modal.error = 'API key is required'; render(); return;
            }
          }
          installFromMarket(entry, state.marketModal.env, state.marketModal.name);
        }}}, modal.busy ? 'Working…' : (entry.auth && modal.authMode !== 'pat' ? 'Install & authorize' : 'Install')),
      ),
    ),
  );
}

function marketAuthRadio(label, value, modal) {
  return el('label', { style: 'display: flex; align-items: center; gap: 6px; cursor: pointer;' },
    el('input', {
      type: 'radio', name: 'market-auth-mode', value,
      checked: modal.authMode === value,
      on: { change: () => { modal.authMode = value; render(); } },
    }),
    label,
  );
}

function renderMarketAuthSection(modal) {
  const a = modal.entry.auth;
  const sections = [
    el('div', { class: 'meta', style: 'margin-top: 12px;' }, 'Authentication'),
    el('div', { class: 'row', style: 'gap: 16px;' },
      a.supports_managed ? marketAuthRadio('Provider-managed (recommended)', 'managed', modal) : null,
      marketAuthRadio('Your own OAuth app', 'byo', modal),
      a.supports_pat ? marketAuthRadio('API key', 'pat', modal) : null,
    ),
  ];

  if (modal.authMode === 'byo') {
    sections.push(
      a.client_setup_url
        ? el('div', { class: 'meta' }, 'Create an OAuth app at ',
            el('a', { href: a.client_setup_url, target: '_blank' }, a.client_setup_url.replace(/^https?:\/\//, '')),
            ' and register this callback URL:')
        : null,
      state.oauthRedirectURI
        ? el('div', { class: 'row' },
            el('code', { style: 'word-break: break-all;' }, state.oauthRedirectURI),
            el('button', { on: { click: () => {
              navigator.clipboard && navigator.clipboard.writeText(state.oauthRedirectURI);
              toast('Callback URL copied.');
            }}}, 'Copy'),
          )
        : null,
      el('label', {},
        el('div', { class: 'meta' }, 'client_id *'),
        el('input', {
          value: modal.clientId,
          autocomplete: 'off',
          on: { input: (e) => { modal.clientId = e.target.value; } },
        }),
      ),
      el('label', {},
        el('div', { class: 'meta' }, 'client_secret'),
        el('input', {
          type: 'password',
          value: modal.clientSecret,
          autocomplete: 'off',
          on: { input: (e) => { modal.clientSecret = e.target.value; } },
        }),
      ),
      ...(a.options || []).map((o) => el('div', {},
        el('div', { class: 'meta' }, o.label),
        el('div', { class: 'row', style: 'gap: 16px;' },
          ...o.choices.map((c) => el('label', { style: 'display: flex; align-items: center; gap: 6px; cursor: pointer;', title: c.description || '' },
            el('input', {
              type: 'radio', name: 'market-auth-opt-' + o.param, value: c.value,
              checked: (modal.options[o.param] || '') === c.value,
              on: { change: () => { modal.options[o.param] = c.value; render(); } },
            }),
            c.label,
          )),
        ),
        (() => {
          const sel = (o.choices || []).find((c) => c.value === (modal.options[o.param] || ''));
          return sel && sel.description ? el('div', { class: 'meta', style: 'margin-top: 2px;' }, sel.description) : null;
        })(),
      )),
      a.scope ? el('div', { class: 'meta' }, 'Scopes: ', el('code', {}, a.scope)) : null,
    );
  } else if (modal.authMode === 'pat') {
    sections.push(
      el('label', {},
        el('div', { class: 'meta' }, 'API key *'),
        el('input', {
          type: 'password',
          value: modal.pat,
          placeholder: a.pat_hint || '',
          autocomplete: 'off',
          on: { input: (e) => { modal.pat = e.target.value; } },
        }),
      ),
    );
  } else {
    sections.push(el('div', { class: 'meta' },
      'The provider registers a client automatically (OAuth discovery + dynamic registration); you just approve in the browser.'));
  }
  return el('div', {}, ...sections.filter(Boolean));
}

async function marketSubmitPaste() {
  const modal = state.marketModal;
  if (!modal || !(modal.paste || '').trim()) return;
  modal.busy = true; modal.error = ''; render();
  try {
    await api('/v1/mcp-oauth/paste', { method: 'POST', body: { url: modal.paste.trim() } });
    // mcp_oauth_done fires via SSE and closes the modal.
  } catch (e) {
    modal.error = e.message;
  }
  if (state.marketModal) state.marketModal.busy = false;
  render();
}

async function installFromMarket(entry, env, overrideName) {
  const modal = state.marketModal;
  const authFlow = !!(entry.auth && modal && modal.authMode);
  const args = (entry.args || []).map((a) =>
    a.replace(/\$\{([A-Z0-9_]+)\}/g, (_, key) => env[key] || '')
  );
  const body = {
    name:      (overrideName || entry.suggested_name).trim(),
    transport: entry.transport,
    command:   entry.command,
    args,
    url:       entry.url,
    env,
  };
  // Strip placeholder env keys whose value is empty (e.g., FS uses ROOT for
  // arg substitution only, not as an actual env var the server needs).
  for (const k of Object.keys(env)) {
    if (!env[k]) delete body.env[k];
  }
  if (modal) { modal.busy = true; modal.error = ''; render(); }
  try {
    const resp = await fetch('/v1/servers', {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'toolyard' },
      body: JSON.stringify(body),
    });
    const out = await resp.json();
    if (resp.status === 202 && !authFlow) {
      // Saved but couldn't connect — for plain entries that's a failure
      // worth shouting about; for auth entries a 401 here is expected
      // (no token yet) and the flow below fixes it.
      toast('Saved, but failed to connect: ' + (out.warning || 'unknown'), 'error');
    } else if (!resp.ok && resp.status !== 202) {
      throw new Error(out.error || ('HTTP ' + resp.status));
    }
    if (!authFlow) {
      if (resp.ok && resp.status !== 202) toast(entry.name + ' installed.');
      state.marketModal = null;
      await reloadServers();
      render();
      return;
    }
    await marketAuthorize(entry, modal, body.name);
  } catch (e) {
    if (state.marketModal) { state.marketModal.busy = false; state.marketModal.error = e.message; render(); }
    else toast(e.message, 'error');
  }
}

// marketAuthorize chains the existing OAuth endpoints right after a
// marketplace install: seed the client (preset BYO creds or discovery+DCR),
// then open the authorize page. PATs short-circuit. Completion is driven by
// the mcp_oauth_done SSE event, which reconnects the upstream server-side.
async function marketAuthorize(entry, modal, name) {
  const base = '/v1/servers/' + encodeURIComponent(name) + '/oauth';
  if (modal.authMode === 'pat') {
    await api(base + '/pat', { method: 'POST', body: { token: modal.pat.trim() } });
    toast(name + ' connected.');
    state.marketModal = null;
    await reloadServers();
    render();
    return;
  }
  if (modal.authMode === 'byo') {
    const extra = {};
    for (const o of entry.auth.options || []) {
      if (modal.options[o.param]) extra[o.param] = modal.options[o.param];
    }
    await api(base + '/manual-client', { method: 'POST', body: {
      client_id:              modal.clientId.trim(),
      client_secret:          modal.clientSecret,
      issuer:                 entry.auth.issuer || '',
      authorization_endpoint: entry.auth.authorization_endpoint,
      token_endpoint:         entry.auth.token_endpoint,
      scopes:                 entry.auth.scope ? [entry.auth.scope] : [],
      extra_authorize_params: extra,
    }});
  } else {
    await api(base + '/discover', { method: 'POST', body: {} });
  }
  const begun = await api(base + '/begin', { method: 'POST', body: { mode: 'callback' } });
  window.open(begun.authorize_url, '_blank', 'noopener');
  modal.installedName = name;
  modal.phase = 'waiting';
  modal.busy = false;
  await reloadServers();
  render();
}

async function reconnectServer(name) {
  try {
    const resp = await fetch('/v1/servers/' + encodeURIComponent(name) + '/reconnect',
      { method: 'POST', credentials: 'include',
        headers: { 'X-Requested-With': 'toolyard' } });
    const out = await resp.json();
    if (resp.status === 202) toast('Still failing: ' + (out.warning || ''), 'error');
    else if (!resp.ok) throw new Error(out.error || ('HTTP ' + resp.status));
    else toast('Reconnected.');
    await reloadServers(); render();
  } catch (e) { toast(e.message, 'error'); }
}

async function removeServer(name) {
  try {
    await api('/v1/servers/' + encodeURIComponent(name), { method: 'DELETE' });
    await reloadServers(); render();
  } catch (e) { toast(e.message, 'error'); }
}

function viewTools() {
  const filter = (state.toolFilter || '').toLowerCase();
  const counts = (state.usage && state.usage.per_tool) || {};
  const tools = state.tools.filter((t) =>
    !filter ||
    t.name.toLowerCase().includes(filter) ||
    (t.description || '').toLowerCase().includes(filter) ||
    (t.upstream || '').toLowerCase().includes(filter)
  ).sort((a, b) => {
    // Sort by uses desc, then name asc, so the most-used tools surface
    // at the top of the catalog.
    const ca = counts[a.name] || 0;
    const cb = counts[b.name] || 0;
    if (ca !== cb) return cb - ca;
    return a.name.localeCompare(b.name);
  });

  // Auto-select the first tool if nothing is selected and we have results.
  if (!state.workbench.selected && tools.length) {
    state.workbench.selected = tools[0].name;
  }
  // If filter dropped the current selection, snap to first result.
  if (state.workbench.selected && !tools.find((t) => t.name === state.workbench.selected)) {
    state.workbench.selected = tools[0] ? tools[0].name : null;
  }

  const selected = tools.find((t) => t.name === state.workbench.selected);

  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Tool workbench'),
      el('p', { class: 'meta' },
        'Search connected MCP servers and inspect their tools. Restricted calls require an Inbox request with task context. The agent executes accepted calls with a scoped grant.'),
      el('input', {
        id: 'tool-search', 'aria-label': 'Search tools',
        placeholder: 'Search by name, description, or service (e.g. "github", "search", "context7")…',
        value: filter,
        on: { input: (e) => { state.toolFilter = e.target.value; render(); } },
      }),
      el('div', { class: 'meta', style: 'margin-top: 8px;' },
        `${tools.length} of ${state.tools.length} tools`),
    ),
    tools.length === 0
      ? el('div', { class: 'card empty' }, state.tools.length === 0 ? 'Add a connection to make its tools available here.' : 'No tools match your filter.')
      : el('div', { class: 'workbench' },
          el('div', { class: 'tool-list' },
            tools.map((t) => {
              const c = counts[t.name] || 0;
              return el('button', {
                type: 'button', 'aria-pressed': t.name === state.workbench.selected ? 'true' : 'false',
                class: 'item' + (t.name === state.workbench.selected ? ' active' : ''),
                on: { click: () => { state.workbench.selected = t.name; state.workbench.result = null; render(); } },
              },
                el('div', { style: 'display: flex; gap: 6px; align-items: baseline;' },
                  el('div', { class: 'name', style: 'flex: 1;' }, t.name),
                  c > 0 ? el('div', { class: 'upstream', title: 'successful calls', style: 'color: var(--accent);' }, String(c)) : null,
                ),
                el('div', { class: 'upstream' }, t.upstream),
              );
            }),
          ),
          selected ? renderToolPane(selected) : el('div', { class: 'pane empty' }, 'Pick a tool on the left.'),
        ),
  );
}

function renderToolPane(t) {
  const wb = state.workbench;
  const props = (t.input_schema && t.input_schema.properties) || {};
  const required = new Set((t.input_schema && t.input_schema.required) || []);
  const orderedKeys = Object.keys(props).sort((a, b) => {
    if (a === '_reason') return -1;
    if (b === '_reason') return 1;
    if (a === '_intent_category') return -1;
    if (b === '_intent_category') return 1;
    if (required.has(a) && !required.has(b)) return -1;
    if (required.has(b) && !required.has(a)) return 1;
    return a.localeCompare(b);
  });
  if (!wb.inputs[t.name]) wb.inputs[t.name] = {};
  const inputs = wb.inputs[t.name];

  const rawMode = !!wb.rawMode[t.name];

  return el('div', { class: 'pane' },
    el('h3', {}, t.name),
    el('div', { class: 'desc' }, (t.description || '').replace(/^\[Gated by toolyard.[^\]]+\]\s*/, '')),
    el('div', { class: 'row', style: 'gap: 6px; margin-bottom: 12px;' },
      el('span', { class: 'badge', title: 'upstream' }, t.upstream),
      el('button', {
        class: rawMode ? '' : 'primary',
        on: { click: () => { wb.rawMode[t.name] = false; render(); } },
      }, 'Form'),
      el('button', {
        class: rawMode ? 'primary' : '',
        on: { click: () => {
          // moving to raw: serialize current form state
          const filled = collectArgs(t, inputs);
          wb.rawJSON[t.name] = JSON.stringify(filled, null, 2);
          wb.rawMode[t.name] = true;
          render();
        }},
      }, 'Raw JSON'),
    ),
    rawMode
      ? el('textarea', {
          id: 'wb-raw-' + t.name,
          style: 'min-height: 220px; font-family: ui-monospace, monospace;',
          value: wb.rawJSON[t.name] != null ? wb.rawJSON[t.name] : '{}',
          on: { input: (e) => { wb.rawJSON[t.name] = e.target.value; }},
        })
      : el('div', {}, orderedKeys.map((k) => renderField(t, k, props[k], required.has(k), inputs))),

    el('div', { class: 'row', style: 'margin-top: 12px;' },
      el('button', {
        class: 'primary',
        disabled: !!wb.running,
        on: { click: () => runTool(t) },
      }, wb.running ? 'Running…' : 'Run'),
      el('button', {
        on: { click: () => { wb.inputs[t.name] = {}; wb.rawJSON[t.name] = '{}'; wb.result = null; render(); }},
      }, 'Reset'),
    ),
    wb.result && wb.result.toolName === t.name ? renderResult(wb.result) : null,
  );
}

function renderField(t, key, schema, required, inputs) {
  const type = (schema && schema.type) || 'string';
  const enumVals = schema && schema.enum;
  const value = inputs[key] != null ? inputs[key] : '';

  let input;
  if (enumVals && enumVals.length) {
    input = el('select', {
      on: { change: (e) => { inputs[key] = e.target.value; }},
    },
      el('option', { value: '' }, '—'),
      ...enumVals.map((v) => el('option', { value: v, selected: value === v }, v)),
    );
  } else if (type === 'boolean') {
    input = el('input', {
      type: 'checkbox',
      checked: !!value,
      on: { change: (e) => { inputs[key] = e.target.checked; }},
    });
  } else if (type === 'integer' || type === 'number') {
    input = el('input', {
      type: 'number',
      value: String(value),
      on: { input: (e) => {
        if (e.target.value === '') { delete inputs[key]; return; }
        const n = type === 'integer' ? parseInt(e.target.value, 10) : parseFloat(e.target.value);
        if (!isNaN(n)) inputs[key] = n;
      }},
    });
  } else if (type === 'object' || type === 'array') {
    input = el('textarea', {
      placeholder: type === 'array' ? '[]' : '{}',
      value: typeof value === 'string' ? value : JSON.stringify(value || (type === 'array' ? [] : {}), null, 2),
      style: 'font-family: ui-monospace, monospace; min-height: 80px;',
      on: { input: (e) => { inputs[key] = e.target.value; }},  // parsed at submit time
    });
  } else if (key === '_reason' || (schema && schema.maxLength && schema.maxLength > 200)) {
    input = el('textarea', {
      placeholder: schema && schema.description ? schema.description : '',
      value: String(value || ''),
      on: { input: (e) => { inputs[key] = e.target.value; }},
    });
  } else {
    input = el('input', {
      type: 'text',
      placeholder: schema && schema.description ? '' : '',
      value: String(value || ''),
      on: { input: (e) => { inputs[key] = e.target.value; }},
    });
  }
  return el('div', { class: 'field' },
    el('div', { class: 'label-row' },
      el('span', { class: 'name' }, key),
      required ? el('span', { class: 'req' }, 'required') : null,
      el('span', { class: 'type' }, type + (enumVals ? ' enum' : '')),
    ),
    schema && schema.description ? el('div', { class: 'desc' }, schema.description) : null,
    input,
  );
}

function collectArgs(t, inputs) {
  const out = {};
  const props = (t.input_schema && t.input_schema.properties) || {};
  for (const k of Object.keys(inputs)) {
    if (inputs[k] === '' || inputs[k] == null) continue;
    const propType = props[k] && props[k].type;
    if (propType === 'object' || propType === 'array') {
      try { out[k] = JSON.parse(inputs[k]); }
      catch { out[k] = inputs[k]; }   // leave invalid JSON for the server to reject
    } else {
      out[k] = inputs[k];
    }
  }
  return out;
}

async function runTool(t) {
  const wb = state.workbench;
  let args;
  if (wb.rawMode[t.name]) {
    try { args = JSON.parse(wb.rawJSON[t.name] || '{}'); }
    catch (e) { toast('Invalid JSON: ' + e.message, 'error'); return; }
  } else {
    args = collectArgs(t, wb.inputs[t.name] || {});
  }
  wb.running = true; wb.result = null; render();
  try {
    const r = await api('/v1/tools/run', { method: 'POST', body: { tool: t.name, arguments: args }});
    wb.result = { toolName: t.name, ...r };
  } catch (e) {
    wb.result = { toolName: t.name, is_error: true, error: e.message };
  }
  wb.running = false;
  render();
}

function renderResult(r) {
  const isPending = r.structured_content && r.structured_content.status === 'pending_approval';
  const cls = r.is_error ? 'result error' : (isPending ? 'result pending' : 'result');
  const title = r.is_error ? 'Error'
              : isPending ? 'Permission pending — open Inbox for the decision'
              : 'Result';
  let body = r.error || '';
  if (Array.isArray(r.content)) {
    for (const c of r.content) {
      if (c && c.type === 'text' && typeof c.text === 'string') {
        body += (body ? '\n' : '') + c.text;
      }
    }
  }
  if (!body && r.structured_content) body = JSON.stringify(r.structured_content, null, 2);
  return el('div', {},
    el('div', { class: 'meta', style: 'margin-top: 12px;' }, title),
    el('div', { class: cls }, body || '(empty)'),
    isPending && r.structured_content && r.structured_content.approval_id ? el('div', { class: 'meta' },
      'approval_id: ', el('code', {}, r.structured_content.approval_id),
    ) : null,
  );
}

// --- platform detection used by the push UI -------------------------------

function isIOS() {
  const ua = navigator.userAgent || '';
  // Includes iPad on iPadOS (which reports as Mac with touch) — covers the
  // "iPhone, iPad, iPod" trio plus the iPadOS 13+ desktop-UA case.
  return /iPad|iPhone|iPod/.test(ua) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
}

function isStandalonePWA() {
  // navigator.standalone is iOS-Safari-specific; the matchMedia check covers
  // Chrome/Edge/Firefox's Add-to-Home-Screen mode.
  return !!(window.navigator.standalone) ||
    (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches);
}

function pushSupportStatus() {
  if (!('serviceWorker' in navigator)) return { ok: false, reason: 'no-sw' };
  if (!('PushManager' in window))      return { ok: false, reason: 'no-push' };
  if (!('Notification' in window))     return { ok: false, reason: 'no-notif' };
  return { ok: true };
}

// showSecretBox displays a sensitive value with a copy-to-clipboard
// button and an explicit dismiss. Auto-clears the DOM node after 60s
// so an unattended browser tab doesn't keep the secret on screen.
function showSecretBox(label, value, hint) {
  const overlay = el('div', {
    style: 'position: fixed; inset: 0; background: rgba(0,0,0,0.55); display: flex; align-items: center; justify-content: center; z-index: 1000;',
  });
  const close = () => { if (overlay.parentNode) overlay.parentNode.removeChild(overlay); };
  const code = el('code', {
    style: 'display: block; word-break: break-all; padding: 12px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; font-size: 13px; user-select: all;',
  }, value);
  const card = el('div', {
    style: 'background: var(--card); border: 1px solid var(--border); border-radius: 8px; padding: 20px; max-width: 520px; width: 90%; box-shadow: 0 12px 32px rgba(0,0,0,0.35);',
  },
    el('h3', { style: 'margin: 0 0 8px 0;' }, label),
    el('div', { class: 'meta', style: 'margin-bottom: 12px;' }, 'Shown once. Copy now — toolyard will not display this value again.'),
    code,
    hint ? el('div', { class: 'meta', style: 'margin-top: 12px;' }, hint) : null,
    el('div', { style: 'margin-top: 16px; display: flex; gap: 8px; justify-content: flex-end;' },
      el('button', {
        class: 'btn',
        on: { click: async () => {
          try { await navigator.clipboard.writeText(value); toast('Copied'); }
          catch { toast('Copy blocked — select the text manually', 'error'); }
        }},
      }, 'Copy'),
      el('button', { class: 'btn', on: { click: close } }, 'Dismiss'),
    ),
  );
  overlay.appendChild(card);
  document.body.appendChild(overlay);
  setTimeout(close, 60000);
}

// renderChatCard is the "Chat Notifications" settings card (Telegram).
// Modeled on the Web Push section: status pill, token save, pairing link,
// Test/Unpair, last error.
function renderChatCard() {
  if (!state.chatLoaded) { loadChatStatus(); }
  const tg = (state.chat && state.chat.telegram) || {};
  const pill = (txt, ok) => el('span', {
    style: 'display:inline-block; padding:2px 8px; border-radius:10px; font-size:12px; margin-left:8px; '
      + (ok ? 'background:var(--ok-bg,#16331f); color:var(--ok,#5fd07a);' : 'background:var(--bg); border:1px solid var(--border); color:var(--muted);'),
  }, txt);
  let statusText = 'Not configured', statusOk = false;
  if (tg.paired) { statusText = 'Paired'; statusOk = true; }
  else if (tg.configured) { statusText = 'Token set — pair your chat'; }

  let tokenInput = '';
  const saveToken = async () => {
    if (!tokenInput.trim()) { toast('Paste a BotFather token', 'error'); return; }
    try {
      const r = await api('/v1/chat/telegram/configure', { method: 'POST', body: { token: tokenInput.trim() } });
      toast('Connected @' + (r.bot_username || 'bot'));
      state.chatLoaded = false; loadChatStatus();
    } catch (e) { toast(e.message, 'error'); }
  };
  const pair = async () => {
    try {
      const r = await api('/v1/chat/telegram/pair', { method: 'POST', body: {} });
      showSecretBox('Pair Telegram', r.deep_link, 'Open this link on the device with Telegram, then tap Start. The link expires in 10 minutes.');
    } catch (e) { toast(e.message, 'error'); }
  };

  return el('div', { class: 'card' },
    el('h2', {}, 'Chat notifications', pill(statusText, statusOk)),
    el('p', { class: 'meta' },
      'Get request updates in Telegram. Open Toolyard Inbox to review and submit the complete decision.'),
    el('div', { style: 'display:flex; gap:8px; align-items:center; margin-top:8px;' },
      el('input', {
        type: 'password', placeholder: 'BotFather token (123456:ABC-DEF…)',
        style: 'flex:1; padding:6px 8px;',
        on: { input: (e) => { tokenInput = e.target.value; } },
      }),
      el('button', { class: 'btn', on: { click: saveToken } }, tg.configured ? 'Replace token' : 'Save token'),
    ),
    tg.configured ? el('div', { style: 'margin-top:12px; display:flex; gap:8px; flex-wrap:wrap;' },
      el('button', { class: 'btn', on: { click: pair } }, tg.paired ? 'Re-pair chat' : 'Generate pairing link'),
      tg.paired ? el('button', { class: 'btn', on: { click: async () => {
        try { await api('/v1/chat/telegram/test', { method: 'POST', body: {} }); toast('Test sent'); }
        catch (e) { toast(e.message, 'error'); }
      }}}, 'Send test') : null,
      tg.paired ? el('button', { class: 'btn', on: { click: async () => {
        try { await api('/v1/chat/telegram/unpair', { method: 'POST', body: {} }); toast('Unpaired'); state.chatLoaded = false; loadChatStatus(); }
        catch (e) { toast(e.message, 'error'); }
      }}}, 'Unpair') : null,
    ) : null,
    tg.bot_username ? el('div', { class: 'meta', style: 'margin-top:8px;' }, 'Bot: @' + tg.bot_username) : null,
  );
}

async function loadChatStatus() {
  state.chatLoaded = true;
  try { state.chat = await api('/v1/chat/status'); render(); } catch (_) {}
}

// renderOperatorTokensCard lists the CLI operator tokens (toolyard admin /
// toolyard api) and lets the owner revoke them. New tokens are minted with
// `toolyard operator-token create` on the host or POST /v1/operator-tokens.
function renderOperatorTokensCard() {
  if (!state.opTokensLoaded) { loadOperatorTokens(); }
  const fmt = (ms) => ms ? new Date(ms).toLocaleString() : '—';
  const rows = (state.opTokens || []).map((t) => el('tr', {},
    el('td', {}, t.name, el('div', { class: 'meta' }, el('code', {}, t.id))),
    el('td', {}, (t.scopes || []).join(' ')),
    el('td', { class: 'meta' }, fmt(t.last_used_at)),
    el('td', {}, t.revoked_at ? el('span', { class: 'meta' }, 'revoked')
      : el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => revokeOperatorToken(t.id, t.name) } }, 'Revoke')),
  ));
  return el('div', { class: 'card' },
    el('h2', {}, 'Command line access'),
    el('p', { class: 'meta' },
      'Tokens permit command line access through ', el('code', {}, 'toolyard admin'), ' and ', el('code', {}, 'toolyard api'),
      ' to do what this dashboard does. The owner scope (approvals, policies, users, secret values) is never granted by default; secret values are never readable. Every change they make is in the audit log.'),
    state.opTokensLoading ? el('p', { class: 'meta' }, 'Load in progress…') : state.opTokensError ? el('div', { class: 'settings-notice error' }, state.opTokensError, el('button', { on: { click: loadOperatorTokens } }, 'Try again')) : rows.length ? el('table', { class: 'tbl', style: 'width:100%; margin-top:8px;' },
      el('thead', {}, el('tr', {}, el('th', {}, 'Name'), el('th', {}, 'Scopes'), el('th', {}, 'Last used'), el('th', {}, ''))),
      el('tbody', {}, ...rows),
    ) : el('p', { class: 'meta' }, 'No operator tokens.'),
  );
}

async function loadOperatorTokens() {
  state.opTokensLoaded = true; state.opTokensLoading = true; state.opTokensError = '';
  try { state.opTokens = await api('/v1/operator-tokens'); } catch (e) { state.opTokensError = e.message; }
  state.opTokensLoading = false; if (state.route === 'settings') render();
}
async function revokeOperatorToken(id, name) {
  if (!confirm('Revoke operator token "' + name + '"? Agents using it lose access immediately.')) return;
  try { await api('/v1/operator-tokens/' + encodeURIComponent(id), { method: 'DELETE' }); toast('Revoked ' + name); loadOperatorTokens(); }
  catch (e) { toast(e.message, 'error'); }
}

async function loadSecrets() {
  state.secretsLoaded = true; state.secretsLoading = true; state.secretsError = '';
  try { state.secrets = await api('/v1/secrets'); } catch (e) { state.secretsError = e.message; }
  finally { state.secretsLoading = false; if (state.route === 'settings') render(); }
}
function rotateSecret(name, pending) {
  settingsUI.secretEdit = { name, pending, value: '', busy: false, error: '' }; render();
}
async function deleteSecret(name, usedBy) {
  if (!confirm('Delete ' + name + '?' + (usedBy.length ? ' Connections that use it will lose access: ' + usedBy.join(', ') + '.' : ' You cannot restore its value.'))) return;
  const q = usedBy.length ? '?force=1' : '';
  try { await api('/v1/secrets/' + encodeURIComponent(name) + q, { method: 'DELETE' }); toast('Deleted ' + name); loadSecrets(); }
  catch (e) { toast(e.message, 'error'); }
}

function renderPushCard() {
  const ios = isIOS();
  const standalone = isStandalonePWA();
  const support = pushSupportStatus();

  // Helper: render the iOS install steps when relevant.
  const iosSteps = el('div', { style: 'margin-top: 8px;' },
    el('div', { class: 'meta', style: 'font-weight: 500; color: var(--text);' },
      'On iPhone (iOS 16.4+):'),
    el('ol', { style: 'margin: 4px 0 0 18px; padding: 0; color: var(--muted); font-size: 13px; line-height: 1.6;' },
      el('li', {}, 'Open this dashboard in Safari (other iOS browsers can\'t do PWA push).'),
      el('li', {}, 'Tap the ', el('b', {}, 'Share'), ' button, then ', el('b', {}, 'Add to Home Screen'), '.'),
      el('li', {}, 'Open the new ', el('b', {}, 'toolyard'), ' icon from your Home Screen.'),
      el('li', {}, 'In the installed app, return to ', el('b', {}, 'Settings'), ' and tap ', el('b', {}, 'Enable push'),
        ' — iOS will show its permission prompt.'),
    ),
    !standalone && ios ? el('div', { class: 'meta', style: 'margin-top: 8px;' },
      'You\'re currently on a Safari tab (not the installed app), so the Enable button will fail until step 3 is done.',
    ) : null,
  );

  let body;
  if (!state.vapidKey) {
    body = el('div', { class: 'settings-notice' }, 'Device notifications are not configured for this workspace.');
  } else if (!support.ok) {
    if (ios && !standalone) {
      // Most common iOS case: PushManager hidden inside Safari tabs.
      body = el('div', {},
        el('div', { class: 'warn-pill' }, '⚠  Web Push on iOS only works after Add to Home Screen.'),
        iosSteps,
      );
    } else if (support.reason === 'no-push') {
      body = el('div', { class: 'meta' },
        'This browser doesn\'t expose the PushManager API. Try a recent Chrome, Edge, Firefox, or Safari (16.4+).');
    } else if (support.reason === 'no-sw') {
      body = el('div', { class: 'meta' },
        'Service workers aren\'t available — usually because you\'re on a non-HTTPS / non-localhost origin. ',
        'Web Push requires a secure context.');
    } else {
      body = el('div', { class: 'meta' }, 'This browser doesn\'t support the Notifications API.');
    }
  } else {
    // Browser supports push. If iOS but not standalone, still warn.
    body = el('div', {},
      ios && !standalone
        ? el('div', { class: 'warn-pill' }, '⚠  Looks like Safari thinks push is available, but iOS only delivers when run from the Home Screen icon. If Enable fails, follow the install steps below.')
        : null,
      el('div', { class: 'settings-row' },
        el('div', { class: 'settings-row-copy' }, el('div', { class: 'settings-row-title' }, 'This device'),
          el('p', { class: 'meta' }, state.pushReady ? 'Device notifications are enabled.' : 'Enable notifications to receive requests on this device.')),
        el('div', { class: 'row' },
          el('button', { class: 'primary', disabled: !!state.pushEnabling, on: { click: enablePush } }, state.pushEnabling ? 'Enable in progress…' : state.pushReady ? 'Reconnect device' : 'Enable notifications'),
          el('button', { disabled: !state.pushReady, on: { click: testPush } }, 'Send test'))),
      el('details', { class: 'settings-advanced', 'data-disclosure-key': 'notification-diagnostics' },
        el('summary', {}, 'Advanced notification controls'),
        el('p', { class: 'meta' }, 'Use these controls to diagnose a device or reset notification keys.'),
        el('div', { class: 'row' },
          el('button', { on: { click: showPushDiag } }, 'Diagnostics'),
          el('button', { on: { click: showJWTPreview } }, 'Token details'),
          el('button', { on: { click: wipeAndReenroll } }, 'Reset subscriptions'),
          el('button', { class: 'danger', on: { click: rotateVapidKeypair } }, 'Reset workspace push keys'))),
      state.jwtPreview ? renderJWTPreview() : null,
      state.pushDiag ? renderPushDiag() : null,
      state.pushTestResult ? renderPushTestResult() : null,
      ios ? iosSteps : null,
    );
  }

  return el('div', { class: 'card' },
    el('h3', {}, 'Device notifications'),
    el('p', { class: 'meta' },
      'Receive a notification when an agent requires your attention.'),
    body,
  );
}

async function rotateVapidKeypair() {
  if (!confirm('Generate a brand-new VAPID keypair and wipe ALL push subscriptions? You will need to Enable push again on every device.')) return;
  try {
    const r = await api('/v1/push/rotate-vapid', { method: 'POST', body: {} });
    // Tear down browser-side subscription too, otherwise iOS hands back the stale one.
    if ('serviceWorker' in navigator) {
      try {
        const reg = await navigator.serviceWorker.getRegistration();
        if (reg) {
          const existing = await reg.pushManager.getSubscription();
          if (existing) await existing.unsubscribe();
        }
      } catch (_) {}
    }
    state.pushReady = false;
    state.pushDiag = null;
    state.pushTestResult = null;
    state.jwtPreview = null;
    state.vapidKey = r.new_public_key;
    toast('New VAPID keypair generated. Tap Enable push to re-enroll.');
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function showJWTPreview() {
  try {
    const out = await api('/v1/push/jwt-preview');
    state.jwtPreview = out;
    if (!out.entries || out.entries.length === 0) {
      toast('No subscriptions yet — tap Enable push first.', 'error');
    }
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function renderJWTPreview() {
  const j = state.jwtPreview;
  if (!j || !j.entries) return null;
  return el('div', { class: 'card', style: 'margin-top: 12px;' },
    el('div', { class: 'meta' }, 'Per-subscription JWT diagnostic. The "derived_pub_matches_stored" field MUST be true — if it\'s false, the VAPID keypair is corrupted and you should tap Rotate VAPID keys.'),
    ...j.entries.map((e, i) => el('div', { style: 'margin-top: 8px;' },
      el('div', {}, `Subscription #${i + 1}`),
      el('pre', { style: 'background: rgba(0,0,0,0.3); padding: 8px; border-radius: 4px; overflow-x: auto; font-size: 11px;' },
        JSON.stringify(e, null, 2)),
    )),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { on: { click: () => { state.jwtPreview = null; render(); } }}, 'Hide'),
    ),
  );
}

async function wipeAndReenroll() {
  try {
    // 1) Server side: drop every push_subscription row this user owns.
    const r = await api('/v1/push/subscribe', { method: 'DELETE' });
    // 2) Browser side: tear down the existing PushSubscription so the
    //    push service issues a fresh one bound to the *current* VAPID
    //    public key. Without this, pushManager.subscribe() returns the
    //    same stale subscription.
    if ('serviceWorker' in navigator) {
      try {
        const reg = await navigator.serviceWorker.getRegistration();
        if (reg) {
          const existing = await reg.pushManager.getSubscription();
          if (existing) await existing.unsubscribe();
        }
      } catch (_) {}
    }
    state.pushReady = false;
    state.pushTestResult = null;
    state.pushDiag = null;
    toast(`Removed ${r.removed || 0} subscription(s). Tap Enable push to re-enroll.`);
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function testPush() {
  try {
    const out = await api('/v1/push/test', { method: 'POST', body: {} });
    state.pushTestResult = out;
    if (out.delivered === 0 && out.results.length === 0) {
      toast('No push subscriptions registered yet — tap Enable push first.', 'error');
    } else if (out.failed > 0) {
      toast(`Push: ${out.delivered} delivered, ${out.failed} failed (see results below)`, 'error');
    } else {
      toast(`Push: ${out.delivered} delivered. Check your device.`);
    }
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function showPushDiag() {
  try {
    const out = await api('/v1/push/diag');
    state.pushDiag = out;
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function renderPushTestResult() {
  const r = state.pushTestResult;
  if (!r || !r.results) return null;
  return el('div', { class: 'card', style: 'margin-top: 12px;' },
    el('div', { class: 'meta' }, `Delivered: ${r.delivered} · Pruned: ${r.pruned} · Failed: ${r.failed}`),
    el('table', {}, el('thead', {}, el('tr', {},
      el('th', {}, 'Device'),
      el('th', {}, 'Push host'),
      el('th', {}, 'HTTP'),
      el('th', {}, 'Result'))),
      el('tbody', {}, r.results.map((rr) => {
        const host = (rr.endpoint || '').replace(/^https?:\/\//, '').replace(/\/.*$/, '');
        let resultLabel = '';
        let cls = 'badge';
        if (rr.pruned) { resultLabel = 'gone (subscription pruned)'; cls = 'badge denied'; }
        else if (rr.status >= 200 && rr.status < 300) { resultLabel = 'delivered'; cls = 'badge allowed'; }
        else if (rr.error) { resultLabel = rr.error; cls = 'badge denied'; }
        else if (rr.status === 401 || rr.status === 403) { resultLabel = 'rejected — VAPID/subject?'; cls = 'badge denied'; }
        else if (rr.status === 413) { resultLabel = 'payload too large'; cls = 'badge denied'; }
        else { resultLabel = 'status ' + rr.status; cls = 'badge'; }
        return el('tr', {},
          el('td', {}, rr.user_agent ? rr.user_agent.slice(0, 40) : '—'),
          el('td', {}, el('code', {}, host)),
          el('td', {}, String(rr.status || '—')),
          el('td', { style: 'word-break: break-word; max-width: 500px;' },
            el('span', { class: cls }, resultLabel)),
        );
      }))),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { on: { click: () => { state.pushTestResult = null; render(); } }}, 'Hide'),
    ),
  );
}

function renderPushDiag() {
  const d = state.pushDiag;
  if (!d) return null;
  return el('div', { class: 'card', style: 'margin-top: 12px;' },
    el('div', {}, 'VAPID subject: ', el('code', {}, d.subject || '(none)')),
    d.subject_warning
      ? el('div', { class: 'err', style: 'margin-top: 4px;' }, '⚠ ' + d.subject_warning)
      : el('div', { class: 'meta' }, 'Subject looks valid.'),
    el('div', { style: 'margin-top: 8px;' }, `Subscriptions registered for this user: ${(d.subscriptions || []).length}`),
    (d.subscriptions || []).length > 0
      ? el('table', {}, el('thead', {}, el('tr', {},
          el('th', {}, 'Push host'), el('th', {}, 'User agent'), el('th', {}, 'Added'))),
          el('tbody', {}, d.subscriptions.map((s) => el('tr', {},
            el('td', {}, el('code', {}, s.host)),
            el('td', {}, (s.user_agent || '').slice(0, 60)),
            el('td', {}, relTime(s.created_at)),
          ))))
      : el('div', { class: 'meta' }, 'None — tap Enable push, then check this dialog again.'),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { on: { click: () => { state.pushDiag = null; render(); } }}, 'Hide'),
    ),
  );
}

async function enablePush() {
  if (state.pushEnabling) return;
  state.pushEnabling = true;
  render();
  try {
    const support = pushSupportStatus();
    if (!support.ok) {
      if (isIOS() && !isStandalonePWA()) {
        throw new Error('On iPhone, tap Share → Add to Home Screen, open toolyard from the Home Screen, then try again. Web Push only works in installed PWAs on iOS.');
      }
      if (support.reason === 'no-sw') {
        throw new Error('Service workers unavailable — Web Push needs HTTPS or localhost.');
      }
      throw new Error('This browser does not support Web Push.');
    }
    const reg = await navigator.serviceWorker.register('/sw.js');
    await navigator.serviceWorker.ready;
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') throw new Error('Notification permission denied — enable it in browser settings.');
    const sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(state.vapidKey),
    });
    const json = sub.toJSON();
    await api('/v1/push/subscribe', { method: 'POST', body: json });
    state.pushReady = true;
    toast('Push enabled on this device.');
  } catch (e) {
    toast(e.message, 'error');
  } finally {
    state.pushEnabling = false;
    render();
  }
}

function urlBase64ToUint8Array(b64) {
  const padding = '='.repeat((4 - b64.length % 4) % 4);
  const base64 = (b64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; ++i) out[i] = raw.charCodeAt(i);
  return out;
}

// ---- insights view ---------------------------------------------------------

function viewInsights() {
  const ranges = ['1h', '24h', '7d', '30d', '90d'];
  const o = state.insights.overview || {};
  const cardNum = (label, value, sub) => el('div', {
    style: 'background: var(--bg); border: 1px solid var(--border); border-radius: 6px; padding: 10px 12px; flex: 1; min-width: 120px;',
  },
    el('div', { class: 'meta', style: 'font-size: 11px; text-transform: uppercase;' }, label),
    el('div', { style: 'font-size: 22px; font-weight: 600; margin-top: 2px;' }, String(value)),
    sub ? el('div', { class: 'meta', style: 'font-size: 11px;' }, sub) : null,
  );
  const fmtNum = (n) => (n == null ? '0' : Number(n).toLocaleString());
  const pct = (x) => (x == null ? '0%' : (Number(x) * 100).toFixed(1) + '%');
  const ms = (n) => (n == null || n === 0 ? '–' : n + 'ms');

  const rangePicker = el('div', { class: 'row' },
    el('span', { class: 'meta', style: 'margin-right: 4px;' }, 'Range'),
    ranges.map((r) => el('button', {
      class: state.insights.range === r ? 'primary' : '',
      on: { click: () => { state.insights.range = r; loadInsights(); }},
    }, r)),
  );

  const overviewCard = el('div', { class: 'card' },
    el('div', { class: 'row', style: 'justify-content: space-between; align-items: center;' },
      el('h2', { style: 'margin: 0;' }, 'Overview'),
      rangePicker,
    ),
    el('div', { class: 'row', style: 'gap: 10px; margin-top: 12px; flex-wrap: wrap;' },
      cardNum('Calls', fmtNum(o.calls), o.distinct_agents != null ? `${o.distinct_agents} agents` : ''),
      cardNum('Errors', fmtNum(o.errors), pct(o.error_rate) + ' rate'),
      cardNum('Writes', fmtNum(o.write_calls), 'gated by approval'),
      cardNum('Approvals', fmtNum(o.approvals), 'human-decided'),
      cardNum('Auto', fmtNum(o.auto_approvals), 'auto-decided'),
      cardNum('Denials', fmtNum(o.denials)),
      cardNum('p50', ms(o.p50_latency_ms), 'p95 ' + ms(o.p95_latency_ms)),
      cardNum('Tools', fmtNum(o.distinct_tools), 'distinct'),
    ),
  );

  // Tool table — each row carries a quick-switch toggle that flips the tool
  // between "auto-approve" and "needs approval (human review)". The toggle
  // calls POST /v1/insights/tools/{name}/policy and re-loads on success.
  // Destructive tools are still hard-vetoed by the engine even when toggled
  // on; the UI shows a "(destructive)" hint so the operator isn't surprised.
  const renderPolicyToggle = (t) => renderPolicyControl({ tool_name: t.tool_name, auto_approve: !!t.auto_approve, is_destructive: !!t.is_destructive });

  const toolRows = (state.insights.tools || []).map((t) =>
    el('tr', {},
      el('td', {}, el('code', {}, t.tool_name),
        t.is_destructive ? el('div', { class: 'meta', style: 'color: var(--warn);' }, 'destructive') : null,
      ),
      el('td', {}, t.upstream),
      el('td', {}, renderPolicyToggle(t)),
      el('td', {}, fmtNum(t.calls)),
      el('td', {}, t.distinct_agents),
      el('td', { style: t.error_rate > 0.1 ? 'color: var(--danger);' : '' }, pct(t.error_rate)),
      el('td', {}, t.approval_ratio ? pct(t.approval_ratio) : '–'),
      el('td', {}, ms(t.p50_latency_ms) + ' / ' + ms(t.p95_latency_ms)),
      el('td', {}, relTime(t.last_seen)),
    ),
  );
  const toolCard = el('div', { class: 'card' },
    el('h2', {}, 'Tools'),
    el('p', { class: 'meta' },
      'Toggle ',
      el('strong', {}, 'Auto'),
      ' to skip the human tap for that tool. ',
      el('strong', {}, 'Ask'),
      ' keeps the default — every call waits for review.',
    ),
    state.insights.tools.length === 0
      ? el('div', { class: 'empty' }, 'No tool calls in this range yet.')
      : el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, 'Tool'), el('th', {}, 'Upstream'), el('th', {}, 'Policy'),
          el('th', {}, 'Calls'),
          el('th', {}, 'Agents'), el('th', {}, 'Err %'), el('th', {}, 'Approval %'),
          el('th', {}, 'p50 / p95'), el('th', {}, 'Last seen'),
        )),
        el('tbody', {}, ...toolRows),
      ),
  );

  // Agents
  const agentRows = (state.insights.agents || []).map((a) => {
    const idShort = a.agent_id ? a.agent_id.slice(0, 12) : '(anonymous)';
    // The name leads when metrics recorded one (or the roster knows the id);
    // the short id stays underneath for "Forget" and cross-checking.
    const known = a.agent_id && (state.agents || []).find((x) => x.id === a.agent_id);
    const name = a.agent_name || (known && known.name) || '';
    return el('tr', {},
      el('td', { title: a.agent_id || '' }, name
        ? [el('div', {}, name), el('div', { class: 'meta' }, el('code', {}, idShort))]
        : el('code', {}, idShort)),
      el('td', {}, fmtNum(a.calls)),
      el('td', {}, a.distinct_tools),
      el('td', { style: a.error_rate > 0.1 ? 'color: var(--danger);' : '' }, pct(a.error_rate)),
      el('td', {}, fmtNum(a.write_calls)),
      el('td', {}, a.approval_ratio ? pct(a.approval_ratio) : '–'),
      el('td', {}, ms(a.p95_latency_ms)),
      el('td', {}, relTime(a.last_seen)),
      el('td', {},
        a.agent_id ? el('button', { class: 'danger', on: { click: () => purgeAgent(a.agent_id) }}, 'Forget') : null,
      ),
    );
  });
  const agentCard = el('div', { class: 'card' },
    el('h2', {}, 'Agents'),
    state.insights.agents.length === 0
      ? el('div', { class: 'empty' }, 'No agent activity in this range yet.')
      : el('table', {},
        el('thead', {}, el('tr', {},
          el('th', {}, 'Agent'), el('th', {}, 'Calls'), el('th', {}, 'Tools'),
          el('th', {}, 'Err %'), el('th', {}, 'Writes'), el('th', {}, 'Appr %'),
          el('th', {}, 'p95'), el('th', {}, 'Last seen'), el('th', {}, ''),
        )),
        el('tbody', {}, ...agentRows),
      ),
  );

  // Auto-approval rules — split into active + suggested.
  const rules = state.insights.autoRules || [];
  const active = rules.filter((r) => r.enabled);
  const suggested = rules.filter((r) => !r.enabled && r.source === 'proposer');

  const ruleSummary = (r) => {
    const parts = [];
    if (r.kind) parts.push(r.kind);
    if (r.tool_name) parts.push('tool=' + r.tool_name);
    if (r.agent_id) parts.push('agent=' + (r.agent_name || agentLabel(r.agent_id)));
    if (r.fingerprint) parts.push('fp=' + r.fingerprint.slice(0, 8));
    return parts.join(' · ');
  };
  const rationale = (r) => {
    if (!r.rationale_json) return null;
    let parsed; try { parsed = JSON.parse(r.rationale_json); } catch { return null; }
    if (!parsed) return null;
    const bits = [];
    if (parsed.approved != null) bits.push(parsed.approved + ' approved');
    if (parsed.denied != null) bits.push(parsed.denied + ' denied');
    if (parsed.calls != null) bits.push(parsed.calls + ' calls');
    if (parsed.approval_ratio != null) bits.push(pct(parsed.approval_ratio) + ' approval rate');
    if (parsed.window_days != null) bits.push('window ' + parsed.window_days + 'd');
    return bits.join(' · ');
  };

  const enabled = !!(state.settings.auto_approval_enabled);
  const autoCard = el('div', { class: 'card' },
    el('div', { class: 'row', style: 'justify-content: space-between;' },
      el('h2', { style: 'margin: 0;' }, 'Auto-approval'),
      el('label', { style: 'display: flex; gap: 6px; align-items: center;' },
        el('input', { type: 'checkbox', checked: enabled, on: { change: async (e) => {
          await api('/v1/settings', { method: 'PATCH', body: { auto_approval_enabled: e.target.checked }});
          state.settings.auto_approval_enabled = e.target.checked;
          toast('Auto-approval ' + (e.target.checked ? 'enabled' : 'disabled'));
          render();
        }}}),
        'Enabled',
      ),
    ),
    el('p', { class: 'meta' },
      'When on, calls that match an active rule skip the human tap and decide as ',
      el('strong', {}, 'auto'), '. Destructive tools and rules in cool-off (after a denial) are always blocked.'),
    el('h3', { style: 'margin-top: 12px; font-size: 14px;' }, `Active rules (${active.length})`),
    active.length === 0 ? el('div', { class: 'empty' }, 'No active rules.') :
      el('table', {},
        el('thead', {}, el('tr', {}, el('th', {}, 'Rule'), el('th', {}, 'Hits'), el('th', {}, 'Last hit'), el('th', {}, ''))),
        el('tbody', {}, ...active.map((r) => el('tr', {},
          el('td', {}, ruleSummary(r), rationale(r) ? el('div', { class: 'meta' }, rationale(r)) : null),
          el('td', {}, r.hit_count || 0),
          el('td', {}, relTime(r.last_hit_ts)),
          el('td', {}, el('button', {
            class: 'danger',
            on: { click: async () => { await api('/v1/insights/auto/rules/' + r.id + '/disable', { method: 'POST' }); toast('Rule revoked'); loadInsights(); }},
          }, 'Revoke')),
        ))),
      ),
    el('h3', { style: 'margin-top: 16px; font-size: 14px;' }, `Suggested rules (${suggested.length})`),
    suggested.length === 0 ? el('div', { class: 'empty' }, 'No suggestions yet — the proposer runs hourly.') :
      el('table', {},
        el('thead', {}, el('tr', {}, el('th', {}, 'Rule'), el('th', {}, 'Why'), el('th', {}, ''))),
        el('tbody', {}, ...suggested.map((r) => el('tr', {},
          el('td', {}, ruleSummary(r)),
          el('td', {}, rationale(r) || ''),
          el('td', {},
            el('button', {
              class: 'primary',
              on: { click: async () => { await api('/v1/insights/auto/rules/' + r.id + '/enable', { method: 'POST' }); toast('Rule enabled'); loadInsights(); }},
            }, 'Enable'),
            ' ',
            el('button', {
              class: 'danger',
              on: { click: async () => { await api('/v1/insights/auto/rules/' + r.id, { method: 'DELETE' }); toast('Dismissed'); loadInsights(); }},
            }, 'Dismiss'),
          ),
        ))),
      ),
  );

  return el('div', {},
    overviewCard,
    toolCard,
    renderPolicyRulesCard(),
    agentCard,
    autoCard,
  );
}

async function purgeAgent(agentID) {
  if (!confirm('Permanently delete all metrics for ' + agentID + '? This cannot be undone.')) return;
  try {
    await api('/v1/insights/purge-agent', { method: 'POST', body: { agent_id: agentID }});
    toast('Purged.');
    loadInsights();
  } catch (e) { toast(e.message, 'error'); }
}

function viewNotifications() {
  const items = state.anomalies || [];
  const sevColor = (s) => s === 'crit' ? 'var(--danger)' : s === 'warn' ? 'var(--warn)' : 'var(--muted)';
  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Notifications'),
      el('p', { class: 'meta' }, 'Anomalies surfaced by the detector — rate spikes, error spikes, oversized arguments, repeated reasons. Dismissing only hides the row; the underlying calls remain in the audit log.'),
      items.length === 0 ? el('div', { class: 'empty' }, 'Nothing to report.') :
        el('table', {},
          el('thead', {}, el('tr', {},
            el('th', {}, 'When'), el('th', {}, 'Kind'), el('th', {}, 'Subject'),
            el('th', {}, 'Summary'), el('th', {}, ''),
          )),
          el('tbody', {}, ...items.map((a) => el('tr', {},
            el('td', {}, relTime(a.ts)),
            el('td', { style: 'color: ' + sevColor(a.severity) + ';' }, a.kind),
            el('td', {}, [a.agent_id ? el('div', { title: a.agent_id }, el('code', {}, a.agent_name || agentLabel(a.agent_id))) : null,
                          a.tool_name ? el('div', {}, el('code', {}, a.tool_name)) : null]),
            el('td', {}, a.summary),
            el('td', {}, el('button', {
              on: { click: async () => { await api('/v1/insights/anomalies/' + a.id + '/dismiss', { method: 'POST' }); loadInsights(); }},
            }, 'Dismiss')),
          ))),
        ),
    ),
  );
}

// ---- shell -----------------------------------------------------------------

function uiIcon(name) {
  const paths = {
    inbox: 'M3 4h14l3 9v7H0v-7z M0 13h6l2 3h4l2-3h6',
    plug: 'M6 2v5 M14 2v5 M4 7h12v3a6 6 0 0 1-12 0z M10 16v4',
    activity: 'M1 10h4l3-7 4 14 3-7h4',
    people: 'M7 10a4 4 0 1 0 0-8 4 4 0 0 0 0 8 M0 19v-2a6 6 0 0 1 12 0v2 M14 3a4 4 0 0 1 0 7 M15 13a5 5 0 0 1 5 5v1',
    tools: 'M7 2H2v5h5z M18 2h-5v5h5z M7 13H2v5h5z M18 13h-5v5h5z',
    sliders: 'M3 2v16 M10 2v16 M17 2v16 M0 7h6 M7 13h6 M14 6h6',
    shield: 'M10 1l8 3v6c0 5-8 9-8 9s-8-4-8-9V4z M6 10l3 3 5-6',
    bell: 'M4 8a6 6 0 0 1 12 0v5l2 3H2l2-3z M8 19h4',
    lock: 'M5 8V5a5 5 0 0 1 10 0v3 M3 8h14v11H3z M10 12v3',
    key: 'M7 12a5 5 0 1 1 4-4l8 8-3 3-3-3v-3h-3z',
    download: 'M10 1v12 M5 8l5 5 5-5 M2 15v4h16v-4',
    info: 'M10 9v6 M10 5h.01 M19 10a9 9 0 1 1-18 0 9 9 0 0 1 18 0',
    plus: 'M10 3v14 M3 10h14',
    menu: 'M2 5h16 M2 10h16 M2 15h16',
    logout: 'M8 2H2v16h6 M7 10h12 M15 6l4 4-4 4',
    chevron: 'M7 4l6 6-6 6',
  };
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  for (const [key, value] of Object.entries({ viewBox: '-1 -1 22 22', width: '18', height: '18', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.5', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true', class: 'ui-icon' })) svg.setAttribute(key, value);
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path'); path.setAttribute('d', paths[name] || paths.tools); svg.appendChild(path);
  return svg;
}

async function logoutWorkspace() {
  const viaClerk = state.user.auth === 'clerk';
  ibClearDrafts(); state.myKey = freshMyKey();
  try { await api('/v1/auth/logout', { method: 'POST' }); } catch (_) {}
  if (evtSrc) try { evtSrc.close(); } catch (_) {}
  if (viaClerk) { location.replace('/login?signout=1'); return; }
  state.user = null; render();
}

const disclosureState = new Map();
function disclosureKey(d) { return d.dataset.disclosureKey || d.querySelector('summary')?.textContent; }
function captureDisclosures(root) {
  const route = root.querySelector('.app-shell')?.dataset.route;
  if (!route) return;
  const values = new Map();
  for (const d of root.querySelectorAll('details')) values.set(disclosureKey(d), d.open);
  disclosureState.set(route, values);
}
function restoreDisclosures(root) {
  const values = disclosureState.get(state.route);
  if (!values) return;
  for (const d of root.querySelectorAll('details')) if (values.has(disclosureKey(d))) d.open = values.get(disclosureKey(d));
}

function workspaceRoute(route) {
  // Retired dashboards are redirected without deleting their stored data or APIs.
  return ({ hooks: 'audit', events: 'audit', memory: 'settings/data', mempalace: 'settings/data', call: 'inbox', sessions: 'inbox', 'inbox/sessions': 'inbox', approvals: 'inbox' })[route] || route;
}

function navigate(route) {
  route = workspaceRoute(route);
  let hashRoute = route;
  if (route.startsWith('settings/')) route = settingsRoute(route);
  else if (route === 'settings') hashRoute = 'settings/' + settingsUI.page;
  if (!routeAllowed(route)) route = defaultRoute();
  if (route === 'inbox' || state.route === 'inbox') {
    if (state.inbox.openId) { ibHalt(); state.inbox.openId = null; state.inbox.detail = null; ibNode = null; }
    if (route === 'inbox') loadInbox();
  }
  state.route = route;
  if (!routeAllowed(hashRoute.split('/')[0])) hashRoute = route;
  if (location.hash !== '#' + hashRoute) history.pushState(null, '', '#' + hashRoute);
  if ((route === 'insights' || route === 'policies' || route === 'notifications') && !state.insights.loading) {
    loadInsights();
  }
  if (route === 'users') loadUsers(true);
  if (route === 'myservers') loadMyServers();
  if (route === 'connections') loadConnections(true);
  if (route === 'agents' || route === 'myservers') loadMyKey();
  render();
}

function shell(content) {
  const selectedNav = key => key === state.route || ({ servers: ['connections'], agents: ['users'], audit: ['insights'], tools: ['policies','approvals'] }[key] || []).includes(state.route);
  const navBtn = (key, label, icon, count) => el('button', {
    class: selectedNav(key) ? 'active' : '', 'aria-current': selectedNav(key) ? 'page' : 'false',
    on: { click: () => navigate(key) }
  }, uiIcon(icon), el('span', { class: 'nav-text' }, label), count ? el('span', { class: 'nav-count' }, count) : null);
  const bottomItem = (key, icon, label, count) => el('button', {
    class: selectedNav(key) ? 'active' : '', 'aria-current': selectedNav(key) ? 'page' : 'false',
    on: { click: () => navigate(key) }
  }, uiIcon(icon), el('span', {}, label), count > 0 ? el('span', { class: 'badge-count' }, String(count)) : null);
  const admin = isAdmin();
  const moreActive = !['inbox','servers','connections','audit','insights'].includes(state.route);
  return el('div', { class: 'app-shell', 'data-route': state.route },
    el('header', { class: 'workspace-sidebar' },
      el('button', { class: 'brand', 'aria-label': 'Toolyard home', on: { click: () => navigate(defaultRoute()) } },
        el('span', { class: 'brand-symbol', 'aria-hidden': 'true' }, 't'), el('span', {}, 'Toolyard')),
      state.authConfig?.environment === 'stage' ? el('div', { class: 'stage-label', title: state.authConfig.version },
        el('span', { class: 'stage-dot' }), 'Stage workspace') : null,
      admin ? el('nav', { 'aria-label': 'Main navigation' },
        el('span', { class: 'nav-label' }, 'Workspace'),
        navBtn('inbox', 'Inbox', 'inbox', inboxBadgeCount()),
        navBtn('servers', 'Connections', 'plug'),
        navBtn('audit', 'Activity', 'activity'),
        el('span', { class: 'nav-label' }, 'Manage'),
        navBtn('agents', 'People & agents', 'people'),
        navBtn('tools', 'Tools', 'tools'),
        navBtn('settings', 'Settings', 'sliders')) : el('nav', { 'aria-label': 'Main navigation' },
        navBtn('inbox', 'Inbox', 'inbox', inboxBadgeCount()), navBtn('agents', 'Agents', 'people'), navBtn('myservers', 'Available services', 'tools'), navBtn('connections', 'Connections', 'plug')),
      el('div', { class: 'sidebar-footer' }, admin ? renderStreamPill() : null,
        el('div', { class: 'sidebar-account' }, el('span', { class: 'user-chip', title: state.user.email || '' },
          userAvatar(state.user), el('span', { class: 'account-copy' }, userLabel(state.user), el('small', {}, admin ? 'Administrator' : 'Member'))),
          el('button', { class: 'icon-button', 'aria-label': 'Sign out', title: 'Sign out', on: { click: logoutWorkspace } }, uiIcon('logout'))))),
    el('main', { id: 'workspace-main' }, content),
    el('nav', { class: 'bottom-nav', 'aria-label': 'Mobile navigation' }, admin ? el('div', { class: 'row' },
      bottomItem('inbox', 'inbox', 'Inbox', inboxBadgeCount()), bottomItem('servers', 'plug', 'Connections'), bottomItem('audit', 'activity', 'Activity'),
      el('button', { class: moreActive ? 'active' : '', 'aria-expanded': String(state.moreSheet), on: { click: () => { state.moreSheet = true; render(); } } }, uiIcon('menu'), el('span', {}, 'More'))) :
      el('div', { class: 'row' }, bottomItem('inbox', 'inbox', 'Inbox', inboxBadgeCount()), bottomItem('agents', 'people', 'Agents'), bottomItem('myservers', 'tools', 'Services'), bottomItem('connections', 'plug', 'Connections'),
        el('button', { on: { click: () => { state.moreSheet = true; render(); } } }, uiIcon('menu'), 'More'))),
    state.moreSheet ? renderMoreSheet() : null);
}

function renderMoreSheet() {
  const item = (key, label, hint) => el('button', {
    class: state.route === key ? 'active primary' : '',
    style: 'width: 100%; justify-content: flex-start; text-align: left; min-height: 52px; padding: 10px 14px;',
    on: { click: () => { state.moreSheet = false; navigate(key); } }
  },
    el('div', {},
      el('div', { style: 'font-weight: 500;' }, label),
      hint ? el('div', { class: 'meta', style: 'font-size: 12px; margin-top: 2px;' }, hint) : null,
    ),
  );
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) { state.moreSheet = false; render(); } }}},
    el('div', { class: 'modal', style: 'display: flex; flex-direction: column; gap: 6px;' },
      el('h3', {}, 'More'),
      isAdmin() ? [
        item('agents', 'People & agents', 'Manage access and enrolled agents'),
        item('tools', 'Tools', 'Browse tools and review access policies'),
        item('settings', 'Settings', 'Workspace preferences'),
      ] : [
        item('agents',    'Agents',     'Manage your enrolled agents'),
        item('myservers', 'My servers', 'Servers your agents may use'),
        item('connections', 'My connections', 'Sign in to servers that act as you'),
      ],
      el('div', { class: 'mobile-account' }, userAvatar(state.user), el('span', {}, userLabel(state.user)), el('button', { on: { click: logoutWorkspace } }, 'Sign out')),
      el('div', { class: 'row', style: 'margin-top: 12px; justify-content: flex-end;' },
        el('button', { on: { click: () => { state.moreSheet = false; render(); } }}, 'Close'),
      ),
    ),
  );
}

// captureFocus/restoreFocus survive the innerHTML='' teardown in render(),
// so a live SSE re-render doesn't yank focus out of the field you're typing
// in or the approval card you're keyboard-driving.
function captureFocus() {
  const a = document.activeElement;
  if (!a || a === document.body) return null;
  const f = {};
  if (a.id) f.id = a.id;
  const card = a.closest && a.closest('[data-approval-id]');
  if (card) f.approvalId = card.getAttribute('data-approval-id');
  if ('selectionStart' in a && a.selectionStart != null) { f.selStart = a.selectionStart; f.selEnd = a.selectionEnd; }
  return (f.id || f.approvalId) ? f : null;
}

function restoreFocus(f) {
  if (!f) return;
  let target = f.id ? document.getElementById(f.id) : null;
  if (!target && f.approvalId) {
    try { target = document.querySelector(`[data-approval-id="${CSS.escape(f.approvalId)}"]`); } catch (_) {}
  }
  if (target && target.focus) {
    target.focus({ preventScroll: true });
    if (f.selStart != null && target.setSelectionRange) {
      try { target.setSelectionRange(f.selStart, f.selEnd); } catch (_) {}
    }
  }
}

// closeTopmostOverlay dismisses the highest-priority open overlay/edit and
// returns true if it handled the Escape.
function closeTopmostOverlay() {
  const lb = document.querySelector('.ib-lightbox');
  if (lb) { lb.remove(); return true; }
  if (state.agentModal) { state.agentModal = null; render(); return true; }
  if (state.userAccessModal) { closeUserAccess(); return true; }
  if (state.serverConnectionsModal) { state.serverConnectionsModal = null; render(); return true; }
  if (state.serverEditModal) { closeServerEdit(); return true; }
  if (state.marketModal) { state.marketModal = null; render(); return true; }
  if (state.oauthFlow) { closeOAuthPanel(); return true; }
  if (settingsUI.secretEdit) { if (!settingsUI.secretEdit.busy) { settingsUI.secretEdit = null; render(); } return true; }
  if (state.jwtPreview || state.pushDiag || state.pushTestResult) {
    state.jwtPreview = null; state.pushDiag = null; state.pushTestResult = null; render(); return true;
  }
  if (state.moreSheet) { state.moreSheet = false; render(); return true; }
  const stale = document.querySelector('.toast.stale');
  if (stale) { stale.remove(); return true; }
  return false;
}

// Global keyboard shortcuts: Escape closes the topmost overlay; a/d decide
// the *focused* approval card (no first-card fallback — that would make an
// accidental keypress approve something).
document.addEventListener('keydown', (e) => {
  if (e.key === 'Tab') {
    const modal = [...document.querySelectorAll('.modal-bg .modal')].at(-1);
    if (modal) {
      const controls = [...modal.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled),a[href],[tabindex="0"]')].filter(n => n.getClientRects().length);
      const first = controls[0], last = controls.at(-1);
      if (!first) { e.preventDefault(); modal.focus(); return; }
      if (e.shiftKey && (document.activeElement === first || document.activeElement === modal)) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  }
  if (e.key === 'Escape') { if (closeTopmostOverlay()) e.preventDefault(); return; }
  const t = e.target;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)) return;
  if (e.key === 'a' || e.key === 'd') {
    const card = document.activeElement && document.activeElement.closest && document.activeElement.closest('[data-approval-id]');
    if (card && !e.metaKey && !e.ctrlKey && !e.altKey) {
      e.preventDefault();
      decideApproval(card.getAttribute('data-approval-id'), e.key === 'a' ? 'allowed' : 'denied');
    }
  }
});

function render() {
  const root = $('app') || document.body;
  const focus = captureFocus();
  const settingsTabScroll = root.querySelector('[aria-label="Settings categories"]')?.scrollLeft || 0;
  const previousSettingsTab = root.querySelector('[aria-label="Settings categories"] [aria-current="page"]')?.id;
  captureDisclosures(root);
  root.innerHTML = '';
  if (state.setupRequired) { root.appendChild(viewSetup()); return; }
  if (!state.user) { root.appendChild(viewLogin()); return; }
  // A member on an admin route (stale hash, old bookmark) lands on Agents.
  if (!routeAllowed(state.route)) {
    state.route = defaultRoute();
    history.replaceState(null, '', '#' + state.route);
  }
  let body;
  switch (state.route) {
    case 'users':         body = viewUsers();         break;
    case 'myservers':     body = viewMyServers();     break;
    case 'connections':   body = viewConnections();   break;
    case 'audit':         body = viewAudit();         break;
    case 'agents':        body = viewAgents();        break;
    case 'servers':       body = viewConnectionHub();       break;
    case 'policies':      body = viewPolicies();       break;
    case 'tools':         body = viewTools();         break;
    case 'settings':      body = viewSettings();      break;
    case 'insights':      body = viewInsights();      break;
    case 'notifications': body = viewNotifications(); break;
    case 'inbox':         body = viewInbox();         break;
    default:              body = viewInbox();
  }
  root.appendChild(shell(withSectionTabs(body)));
  const settingsTabs = root.querySelector('[aria-label="Settings categories"]');
  if (settingsTabs && settingsTabs.scrollWidth > settingsTabs.clientWidth) {
    settingsTabs.scrollLeft = settingsTabScroll;
    const selected = settingsTabs.querySelector('[aria-current="page"]');
    if (selected && selected.id !== previousSettingsTab) {
      const bounds = settingsTabs.getBoundingClientRect(), active = selected.getBoundingClientRect();
      if (active.left < bounds.left) settingsTabs.scrollLeft += active.left - bounds.left;
      else if (active.right > bounds.right) settingsTabs.scrollLeft += active.right - bounds.right;
    }
  }
  restoreDisclosures(root);
  restoreFocus(focus);
  for (const [index, modal] of [...root.querySelectorAll('.modal-bg .modal')].entries()) {
    modal.setAttribute('role', 'dialog'); modal.setAttribute('aria-modal', 'true'); modal.tabIndex = -1;
    const title = modal.querySelector('h3');
    if (title) { title.id = title.id || 'dialog-title-' + index; modal.setAttribute('aria-labelledby', title.id); }
    if (!modal.contains(document.activeElement)) (modal.querySelector('[autofocus],input:not(:disabled),button:not(:disabled)') || modal).focus({ preventScroll: true });
  }
}

// ---- OAuth integration (remote MCPs) ---------------------------------------

function isHTTPUpstream(s) {
  return s.transport === 'http' || s.transport === 'streamable-http' || (!s.transport && s.url);
}

function oauthBadge(s) {
  if (!isHTTPUpstream(s)) return el('span', { class: 'meta' }, '—');
  const st = (state.oauthStatus || {})[s.name];
  if (!st) return el('span', { class: 'meta' }, 'unknown');
  if (isPerUserServer(s)) {
    // The one OAuth client is shared; the tokens are each person's own.
    return st.has_client
      ? el('span', { class: 'badge identity', title: 'Each person connects their own account on My connections' }, 'each person')
      : el('span', { class: 'badge denied', title: 'Register the OAuth client under Auth… (Discover) before people can connect' }, 'setup needed');
  }
  if (st.is_pat) return el('span', { class: 'badge allowed', title: 'personal access token' }, 'PAT');
  if (!st.has_client) return el('span', { class: 'meta' }, 'none');
  if (!st.has_token) return el('span', { class: 'badge', title: 'client registered, no token yet' }, 'no token');
  if (st.state === 'active') {
    let title = 'authorized';
    if (st.access_expires_at) title += ' — expires ' + new Date(st.access_expires_at).toLocaleString();
    // A shared server acts as one account for everyone: say which when
    // the provider told us.
    return el('span', {},
      el('span', { class: 'badge allowed', title }, 'active'),
      el('div', { class: 'meta', title: 'every change through this server goes as this account' },
        st.account_label ? ['acts as ', el('code', {}, st.account_label)] : 'shared account'),
    );
  }
  if (st.state === 'needs_reauth') {
    return el('span', { class: 'badge denied', title: st.last_error || '' }, 'needs reauth');
  }
  return el('span', { class: 'badge', title: st.last_error || st.state }, st.state || 'unknown');
}

async function openOAuthPanel(name) {
  await loadOAuthStatus(name);
  state.oauthFlow = {
    name,
    mode: 'callback',
    state: '',
    authorize_url: '',
    paste: '',
    error: '',
    pat: '',
    busy: false,
  };
  render();
}

function closeOAuthPanel() { state.oauthFlow = null; render(); }

function renderOAuthModal() {
  const f = state.oauthFlow;
  const st = (state.oauthStatus || {})[f.name] || {};
  const supportsDevice = !!st.device_supported;

  const sectionDiscover = el('div', { class: 'card', style: 'margin: 0 0 12px 0;' },
    el('div', { class: 'meta' }, 'No client registered yet for this upstream.'),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', {
        class: 'primary',
        disabled: f.busy,
        on: { click: () => oauthDiscoverClick() }
      }, f.busy ? 'Discovering…' : 'Discover OAuth + register'),
      el('button', {
        on: { click: () => { f.showManual = true; render(); } }
      }, 'Or paste client_id manually'),
    ),
    f.showManual ? renderManualClient() : null,
    el('div', { class: 'meta', style: 'margin-top: 12px;' },
      'Alternative: skip OAuth and store a personal access token.'),
    el('div', { class: 'row' },
      el('input', {
        placeholder: 'pat or api token',
        type: 'password',
        value: f.pat || '',
        on: { input: (e) => { state.oauthFlow.pat = e.target.value; } }
      }),
      el('button', { on: { click: () => oauthSubmitPAT() } }, 'Save PAT'),
    ),
  );

  const modeRow = el('div', { class: 'row', style: 'gap: 16px;' },
    radio('Same browser (recommended)', 'callback', f.mode, (v) => { state.oauthFlow.mode = v; render(); }),
    radio('Different browser → paste URL back', 'paste', f.mode, (v) => { state.oauthFlow.mode = v; render(); }),
    supportsDevice ? radio('Device code', 'device', f.mode, (v) => { state.oauthFlow.mode = v; render(); }) : null,
  );

  const sectionAuthorize = el('div', { class: 'card', style: 'margin: 0 0 12px 0;' },
    el('div', { class: 'meta' }, 'Authorize this upstream:'),
    modeRow,
    !f.authorize_url
      ? el('div', { class: 'row', style: 'margin-top: 8px;' },
          el('button', { class: 'primary', disabled: f.busy, on: { click: () => oauthBeginClick() }},
            f.busy ? 'Working…' : 'Open authorization page'),
        )
      : renderActiveFlow(f),
  );

  const sectionConnected = el('div', { class: 'card', style: 'margin: 0 0 12px 0;' },
    el('div', {}, 'Connected. ',
      st.scope_granted ? el('code', {}, st.scope_granted) : null),
    st.access_expires_at
      ? el('div', { class: 'meta' }, 'Access token expires ' + new Date(st.access_expires_at).toLocaleString())
      : null,
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { on: { click: () => oauthReauthClick() }}, 'Reauthorize'),
      el('button', { class: 'danger', on: { click: () => oauthDisconnectClick() }}, 'Disconnect'),
    ),
  );

  // A per_user server only needs the client registered here; the sign-ins
  // themselves happen on each person's My connections page.
  const perUser = isPerUserServer(state.servers.find((x) => x.name === f.name));
  const sectionPerUser = el('div', { class: 'card', style: 'margin: 0 0 12px 0;' },
    el('div', {}, 'OAuth client registered. This server acts as each person\'s own account: people connect theirs on My connections.'),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { on: { click: () => { closeOAuthPanel(); navigate('connections'); } } }, 'Open My connections'),
      el('button', { on: { click: () => { closeOAuthPanel(); openServerConnections(f.name); } } }, 'Who\'s connected…'),
      el('button', { class: 'danger', title: 'Drop the OAuth client and every person\'s sign-in to this server', on: { click: () => oauthDisconnectClick() } }, 'Remove client'),
    ),
  );

  let body;
  if (!st.has_client && !st.is_pat) body = sectionDiscover;
  else if (perUser) body = sectionPerUser;
  else if (st.state === 'active' && !f.authorize_url) body = sectionConnected;
  else body = sectionAuthorize;

  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeOAuthPanel(); } }},
    el('div', { class: 'modal' },
      el('h3', {}, perUser ? 'OAuth setup for ' : 'Authorize ', f.name),
      st.issuer ? el('div', { class: 'meta' }, 'Provider: ' + st.issuer) : null,
      body,
      f.error ? el('div', { class: 'err' }, f.error) : null,
      el('div', { class: 'row', style: 'margin-top: 12px; justify-content: flex-end;' },
        el('button', { on: { click: closeOAuthPanel }}, 'Close'),
      ),
    ),
  );
}

function radio(label, value, current, onChange) {
  return el('label', { style: 'display: flex; align-items: center; gap: 6px; cursor: pointer;' },
    el('input', {
      type: 'radio', name: 'oauth-mode', value,
      checked: current === value,
      on: { change: (e) => onChange(e.target.value) },
    }),
    label,
  );
}

function renderManualClient() {
  return el('div', { style: 'margin-top: 8px;' },
    el('label', {},
      el('div', { class: 'meta' }, 'client_id'),
      el('input', { id: 'oa-cid' }),
    ),
    el('label', {},
      el('div', { class: 'meta' }, 'client_secret (optional)'),
      el('input', { id: 'oa-csec', type: 'password' }),
    ),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { class: 'primary', on: { click: () => oauthSubmitManual() }}, 'Save client'),
    ),
  );
}

function renderActiveFlow(f) {
  if (f.mode === 'device') {
    return el('div', {},
      el('div', { class: 'meta' }, 'Open ',
        el('a', { href: f.verification_uri, target: '_blank' }, f.verification_uri),
        ' and enter:'),
      el('div', { style: 'font-size: 24px; font-family: monospace; padding: 8px 0;' }, f.user_code || ''),
      el('div', { class: 'meta' }, 'Polling — this dialog auto-updates when you approve.'),
    );
  }
  return el('div', {},
    el('div', { class: 'meta' }, 'Authorization page opened in a new tab.'),
    el('div', { class: 'meta' }, 'If you can return here in the same browser, this dialog updates automatically.'),
    f.mode === 'paste'
      ? el('label', { style: 'margin-top: 8px;' },
          el('div', { class: 'meta' }, 'Paste the post-redirect URL here:'),
          el('textarea', {
            placeholder: 'https://your-callback/cb?code=…&state=…',
            on: { input: (e) => { state.oauthFlow.paste = e.target.value; } },
            rows: 3,
          }),
          el('div', { class: 'row', style: 'margin-top: 4px;' },
            el('button', { class: 'primary', disabled: f.busy, on: { click: () => oauthSubmitPaste() }},
              f.busy ? 'Submitting…' : 'Submit pasted URL'),
          ),
        )
      : null,
  );
}

async function oauthDiscoverClick() {
  const f = state.oauthFlow;
  f.busy = true; f.error = ''; render();
  try {
    await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/discover', {
      method: 'POST', body: {}
    });
    await loadOAuthStatus(f.name);
  } catch (e) { f.error = e.message; }
  f.busy = false; render();
}

async function oauthBeginClick() {
  const f = state.oauthFlow;
  f.busy = true; f.error = ''; render();
  try {
    if (f.mode === 'device') {
      const out = await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/device-begin', {
        method: 'POST', body: {}
      });
      f.state = out.state;
      f.user_code = out.user_code;
      f.verification_uri = out.verification_uri;
      f.authorize_url = '(device)';
      pollDevice(f);
    } else {
      const out = await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/begin', {
        method: 'POST', body: { mode: f.mode }
      });
      f.state = out.state;
      f.authorize_url = out.authorize_url;
      window.open(out.authorize_url, '_blank', 'noopener');
    }
  } catch (e) { f.error = e.message; }
  f.busy = false; render();
}

async function oauthSubmitPaste() {
  const f = state.oauthFlow;
  f.busy = true; f.error = ''; render();
  try {
    await api('/v1/mcp-oauth/paste', {
      method: 'POST', body: { url: f.paste }
    });
    // mcp_oauth_done will fire via SSE.
  } catch (e) { f.error = e.message; }
  f.busy = false; render();
}

async function oauthSubmitPAT() {
  const f = state.oauthFlow;
  if (!f.pat) { f.error = 'token is required'; render(); return; }
  f.busy = true; f.error = ''; render();
  try {
    await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/pat', {
      method: 'POST', body: { token: f.pat }
    });
    await loadOAuthStatus(f.name);
    await reloadServers();
    toast(f.name + ': PAT saved.');
    state.oauthFlow = null;
  } catch (e) { f.error = e.message; }
  if (state.oauthFlow) state.oauthFlow.busy = false;
  render();
}

async function oauthSubmitManual() {
  const f = state.oauthFlow;
  const cid = ($('oa-cid') || {}).value || '';
  const csec = ($('oa-csec') || {}).value || '';
  if (!cid.trim()) { f.error = 'client_id is required'; render(); return; }
  f.busy = true; f.error = ''; render();
  try {
    await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/manual-client', {
      method: 'POST', body: { client_id: cid.trim(), client_secret: csec }
    });
    await loadOAuthStatus(f.name);
    f.showManual = false;
  } catch (e) { f.error = e.message; }
  f.busy = false; render();
}

async function oauthReauthClick() {
  const f = state.oauthFlow;
  try {
    await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/reauth', { method: 'POST', body: {} });
    await loadOAuthStatus(f.name);
    f.authorize_url = '';
  } catch (e) { f.error = e.message; }
  render();
}

async function oauthDisconnectClick() {
  const f = state.oauthFlow;
  try {
    await api('/v1/servers/' + encodeURIComponent(f.name) + '/oauth', { method: 'DELETE' });
    await loadOAuthStatus(f.name);
    await reloadServers();
    toast(f.name + ' disconnected.');
    state.oauthFlow = null;
  } catch (e) { f.error = e.message; }
  render();
}

async function pollDevice(f) {
  // Poll once a second until we either succeed, error, or the user closes the modal.
  const myFlow = f;
  const tick = async () => {
    if (state.oauthFlow !== myFlow) return; // user moved on
    try {
      const resp = await fetch('/v1/servers/' + encodeURIComponent(f.name) + '/oauth/device-poll', {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'toolyard' },
        body: JSON.stringify({ state: f.state }),
      });
      if (resp.status === 200) {
        // mcp_oauth_done SSE will close the modal.
        return;
      }
      if (resp.status === 202) {
        setTimeout(tick, Math.max(1000, 1000 * (f.interval || 1)));
        return;
      }
      const out = await resp.json();
      f.error = out.error || ('HTTP ' + resp.status);
      render();
    } catch (e) {
      f.error = e.message;
      render();
    }
  };
  setTimeout(tick, 1000);
}

// Pre-load OAuth status for any visible upstreams.
async function preloadOAuthStatus() {
  const targets = (state.servers || []).filter(isHTTPUpstream).map((s) => s.name);
  for (const n of targets) await loadOAuthStatus(n);
}



function withSectionTabs(body) {
  const pages = {
    servers: ['Connections', 'Connect services and control the tools they provide.'],
    connections: ['Connections', 'Manage the accounts your agents use on your behalf.'],
    agents: ['People & agents', 'Manage the people and agents with access to your workspace.'],
    users: ['People & agents', 'Manage the people and agents with access to your workspace.'],
    tools: ['Tools', 'Browse available tools, test a call, and manage access policies.'],
    policies: ['Tools', 'Browse available tools, test a call, and manage access policies.'],
    approvals: ['Tools', 'Browse available tools, test a call, and manage access policies.'],
    audit: ['Activity', 'Review tool calls, permission decisions, and results.'],
    insights: ['Activity', 'Review tool calls, permission decisions, and results.'],
    settings: ['Settings', 'Workspace preferences and access controls.'],
    myservers: ['Available services', 'Services your agents can use.'],
  };
  const groups = isAdmin() ? [
    [['servers', 'Services'], ['connections', 'My accounts']],
    [['agents', 'Agents'], ['users', 'People']],
    [['audit', 'Calls'], ['insights', 'Tool activity']],
    [['tools', 'Catalog'], ['policies', 'Policies']],
  ] : [];
  const group = groups.find(g => g.some(([key]) => key === state.route));
  const meta = pages[state.route];
  if (!meta) return body;
  // Move the primary page title out of the legacy card. Keep section titles.
  const primaryTitles = { servers: 'Connections', tools: 'Tool workbench', agents: 'Agents', users: 'People' };
  const title = [...body.querySelectorAll('h2')].find(h => h.textContent === primaryTitles[state.route]);
  if (title) {
    const card = title.closest('.card'); card?.classList.add('page-primary-card');
    const description = title.parentElement.classList.contains('agent-add-bar') ? title.parentElement.nextElementSibling : title.nextElementSibling;
    if (description?.matches('p.meta')) description.remove();
    title.remove();
  }
  if (state.route === 'agents') {
    const keyCard = [...body.querySelectorAll('.card')].find(card => card.querySelector('h2')?.textContent === 'Your Beknown key');
    if (keyCard) {
      const details = el('details', { class: 'connection-setup', 'data-disclosure-key': 'beknown-key' }, el('summary', {}, 'Beknown identity key'));
      keyCard.replaceWith(details); details.appendChild(keyCard); body.appendChild(details);
    }
  }
  // Tables scroll inside their own region, never across the whole page.
  for (const table of body.querySelectorAll('table')) {
    if (table.closest('.table-scroll')) continue;
    const wrap = el('div', { class: 'table-scroll', tabindex: 0, role: 'region', 'aria-label': 'Scrollable ' + meta[0].toLowerCase() + ' table' });
    table.replaceWith(wrap); wrap.appendChild(table);
  }
  return el('div', { class: 'workspace-page page-' + state.route },
    el('div', { class: 'workspace-page-heading' }, el('h1', {}, meta[0]), el('p', { class: 'meta' }, meta[1])),
    group ? el('nav', { class: 'section-tabs', 'aria-label': meta[0] + ' sections' }, ...group.map(([key,label]) => el('button', {
      class: state.route === key ? 'active' : '', 'aria-current': state.route === key ? 'page' : 'false', on: { click: () => navigate(key) }
    }, label))) : null, body);
}
function viewPolicies() {
  return el('div', {}, renderPolicyRulesCard(),
    el('div', { class: 'card' }, el('h3', {}, 'Tool access'), el('p', { class: 'meta' }, 'An explicit policy takes priority over automatic approval rules.'),
      el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, 'Tool'), el('th', {}, 'Service'), el('th', {}, 'Policy'))),
        el('tbody', {}, ...state.tools.map(t => el('tr', {}, el('td', {}, el('code', {}, t.name)), el('td', { class: 'meta' }, t.upstream || 'Built in'), el('td', {}, renderPolicyControl({ tool_name: t.name, ...(state.insights.tools.find(row => row.tool_name === t.name) || {}) }))))))));
}
function viewConnectionHub() {
  const page = viewServers();
  const cards = [...page.children].filter(n => n.classList.contains('card'));
  const connected = cards.find(n => n.querySelector('h2')?.textContent === 'Connected servers');
  if (connected) {
    connected.querySelector('h2').textContent = 'Connections'; page.prepend(connected);
    const table = connected.querySelector('table');
    if (table) {
      const rows = [...table.querySelectorAll('tbody > tr')];
      const list = el('div', { class:'connection-list' }, ...state.servers.map((srv,i) => {
        const cells = rows[i] ? [...rows[i].children] : [];
        const controls = el('div', { class:'connection-controls' });
        for (const [index,label] of [[5,'Account access'],[6,'Permission policy'],[7,'Actions']]) {
          if (cells[index]) controls.appendChild(el('div', {}, el('span', { class:'meta' }, label), ...[...cells[index].childNodes]));
        }
        return el('div', { class:'connection-row' },
          el('div', { class:'connection-name' }, el('strong', {}, srv.name),
            el('span', { class:'meta' }, (srv.auth_mode === 'per_user' ? 'Personal accounts' : 'Shared connection') + ' · ' + (srv.tool_count || 0) + ' tools')),
          serverStatusBadge(srv),
          el('details', { class:'connection-manage', 'data-disclosure-key': 'service-' + srv.name }, el('summary', { 'aria-label':'Manage ' + srv.name }, 'Manage'),
            el('div', { class:'connection-panel' }, el('p', { class:'meta' }, transportLabel(srv)), controls)));
      }));
      table.replaceWith(list);
    }
  }
  for (const card of cards.filter(n => n !== connected)) {
    const title = card.querySelector('h2')?.textContent || 'Add a service';
    const details = el('details', { class: 'connection-setup' }, el('summary', {}, title));
    card.replaceWith(details); details.appendChild(card);
  }
  return page;
}
const ibDrafts = new Map();
function ibDraftKey(r) { return 'toolyard.answer.' + (state.user?.id || '') + '.' + r.id + '.' + (r.revision || 0); }
function ibDraft(r) {
  const key = ibDraftKey(r);
  if (!ibDrafts.has(key)) {
    let d;
    try { d = JSON.parse(sessionStorage.getItem(key)); } catch {}
    if (!d || Date.now() - d.at > 24 * 3600000) d = { selected: [], text: '', submission: crypto.randomUUID(), at: Date.now() };
    ibDrafts.set(key, d);
  }
  return ibDrafts.get(key);
}
function ibSaveDraft(r, d) { d.at = Date.now(); try { sessionStorage.setItem(ibDraftKey(r), JSON.stringify(d)); } catch {} }
function ibDropDraft(r) { const key=ibDraftKey(r); ibDrafts.delete(key); try { sessionStorage.removeItem(key); } catch {} }
const ibAccessDrafts = new Map();
function ibCallID(t, k) { return t.call_id || 'call_' + (k + 1); }
function ibAccessDraftKey(r) { return 'toolyard.decision.' + (state.user?.id || '') + '.' + r.id; }
function ibAccessDraft(r) {
  const key = ibAccessDraftKey(r);
  if (!ibAccessDrafts.has(key)) {
    let d; try { d = JSON.parse(sessionStorage.getItem(key)); } catch {}
    if (!d || !d.allow || !d.reasons) d = { revision: r.revision || 1, allow: {}, reasons: {}, note: '', narrow: {}, ttl: r.ttl_seconds || 1800 };
    ibAccessDrafts.set(key, d);
  }
  const d = ibAccessDrafts.get(key);
  (r.tools || []).forEach((t,k) => { const id=ibCallID(t,k); if (!(id in d.allow)) d.allow[id] = true; });
  state.inbox.allow[r.id] = (r.tools || []).map((t,k) => !!d.allow[ibCallID(t,k)]);
  state.inbox.narrow[r.id] = d.narrow || {}; state.inbox.ttl[r.id] = d.ttl;
  (state.inbox.notes ||= {})[r.id] = d.note;
  return d;
}
function ibSaveAccessDraft(r,d) { d.at=Date.now(); try { sessionStorage.setItem(ibAccessDraftKey(r),JSON.stringify(d)); } catch {} }
function ibSelectCalls(r, allow) { const d=ibAccessDraft(r); r.tools.forEach((t,k)=>{d.allow[ibCallID(t,k)]=!!allow[k];}); state.inbox.allow[r.id]=allow.slice(); ibSaveAccessDraft(r,d); }
function ibDropAccessDraft(r) { const key=ibAccessDraftKey(r);ibAccessDrafts.delete(key);try{sessionStorage.removeItem(key);}catch{} delete state.inbox.allow[r.id];delete state.inbox.notes?.[r.id]; }
function ibClearDrafts() { settingsClearDrafts(); ibDrafts.clear();ibAccessDrafts.clear(); state.inbox.notes={};state.inbox.allow={};state.inbox.narrow={};state.inbox.ttl={}; try {Object.keys(sessionStorage).filter(k=>k.startsWith('toolyard.answer.')||k.startsWith('toolyard.decision.')).forEach(k=>sessionStorage.removeItem(k));}catch{} }

function ibAnswerForm(r) {
  if (r.status !== 'pending') return el('div', { class: 'ib-sec answer-section' }, ibResultEl(r));
  const d = ibDraft(r), q = r.question || { type: 'single_choice', max_selections: 1 };
  const options = (q.options || r.options || []).map((o,i) => ({ ...o, id: o.id || 'option_' + (i+1) }));
  const busy = !!state.inbox.busy, multi = q.type === 'multiple_choice';
  const fieldset = el('fieldset', { class: 'answer-options', disabled: busy },
    el('legend', {}, q.type === 'free_text' ? 'Write your answer' : multi ? 'Select all that apply, or write your own answer' : 'Choose one, or write your own answer'),
    ...options.map((o,i) => el('label', { class: 'answer-option' + (d.selected.includes(o.id) ? ' selected' : '') },
      el('input', { id: 'answer-option-' + i, type: multi ? 'checkbox' : 'radio', name: 'answer-choice', checked: d.selected.includes(o.id), on: { change: e => {
        d.submission = crypto.randomUUID();
        if (!multi || o.exclusive) d.selected = e.target.checked ? [o.id] : [];
        else { const excluded = options.filter(x => x.exclusive).map(x => x.id); d.selected = d.selected.filter(id => !excluded.includes(id) && id !== o.id); if (e.target.checked) d.selected.push(o.id); }
        ibSaveDraft(r,d); ibRegion('decide',ibAnswerForm(r));
      } } }),
      el('span', {}, el('strong', {}, o.label), o.recommended ? el('small', { class: 'option-recommended' }, 'Suggested') : null, o.detail ? el('small', {}, o.detail) : null)
    )));
  const error = el('p', { class: 'answer-error', role: 'alert', id: 'answer-error' });
  const text = el('textarea', { id: 'answer-text', rows: 4, maxlength: 10000, value: d.text, disabled: busy,
    placeholder: 'Write your answer or add context…', 'aria-describedby': 'answer-help answer-error',
    on: { input: e => { d.text = e.target.value; d.submission = crypto.randomUUID(); ibSaveDraft(r,d); error.textContent = ''; } }
  });
  return el('form', { class: 'ib-sec answer-section', on: { submit: e => {
    e.preventDefault(); if (busy) return;
    if (!d.selected.length && !d.text.trim()) { error.textContent = 'Write an answer or choose an option.'; text.focus(); return; }
    if (q.max_selections && d.selected.length > q.max_selections) { error.textContent = 'Choose at most ' + q.max_selections + ' options.'; return; }
    if (d.selected.length && d.selected.length < (q.min_selections || 0)) { error.textContent = 'Choose at least ' + q.min_selections + ' options, or clear the choices and write an answer.'; return; }
    ibSaveDraft(r,d);
    ibDecide(r, { action:'answer', request_revision:r.revision || 0, submission_id:d.submission, response:{ selected_option_ids:[...d.selected], text:d.text } });
  } } },
    el('div', { class: 'ib-eyebrow' }, 'Your answer'), fieldset,
    d.selected.length ? el('button', { type:'button', class:'ib-link', disabled:busy, on:{click:()=>{d.selected=[];d.submission=crypto.randomUUID();ibSaveDraft(r,d);ibRegion('decide',ibAnswerForm(r));}} }, 'Clear choices') : null,
    el('label', { for: 'answer-text' }, options.length ? 'Your answer or additional context' : 'Answer'), text,
    el('p', { id:'answer-help', class:'meta' }, 'Your draft stays on this device until you send it. Selections never submit automatically.'), error,
    el('div', { class:'ib-btns' },
      el('button', { type:'button', disabled:busy, on:{click:()=>ibDecide(r,{action:'snooze',snooze_minutes:60})} }, 'Snooze for 1 hour'),
      el('button', { type:'submit', class:'primary', disabled:busy }, busy ? 'Save answer…' : 'Send answer')));
}

// ---- Inbox: agents ask, toolyard flags, the owner decides at the end --------
//
// The list is ordinary render() output. The open request is a persistent DOM
// node whose regions update in place, so a playing video or voice note isn't
// interrupted when an SSE event re-renders the rest of the page.

state.inbox = {
  loaded: false, loading: false, filter: 'needs', tab: 'inbox', items: [],
  openId: null, detail: null,
  allow: {}, openParams: {}, explain: {}, summary: {}, panel: null, busy: null,
  narrow: {}, ttl: {}, editing: {}, info: null,
};

const IB_KIND = { access: 'Access request', question: 'Question', blocker: 'Blocked', update: 'Update' };
const IB_VERB = { access: 'is asking for access', question: 'has a question', blocker: 'is stuck', update: 'sent an update' };
const IB_URG = { now: 0, soon: 1, digest: 2, fyi: 3 };
const IB_NEEDS = ['access', 'question', 'blocker'];
const IB_FILTERS = ['needs', 'snoozed', 'updates', 'done'];

async function loadInbox() {
  if (state.inbox.loading) return;
  state.inbox.loading = true;
  try {
    const [open, done, info] = await Promise.all([api('/v1/inbox?view=open'), api('/v1/inbox?view=done'),
      api('/v1/inbox/info').catch(() => null)]);
    state.inbox.items = ((open && open.requests) || []).concat((done && done.requests) || []);
    if (info) state.inbox.info = Object.assign({}, info.info, { passkeys: info.passkeys });
    state.inbox.loaded = true;
  } catch (e) {
    if (e.status !== 401) toast('Inbox: ' + e.message, 'error');
  } finally {
    state.inbox.loading = false;
  }
  if (state.route === 'inbox' && !state.inbox.openId) render();
  else renderNavBadges();
}

function renderNavBadges() {
  // The shell re-renders on the next render(); badges are cheap to update
  // through a full render when we're not inside an open request.
  if (!state.inbox.openId) render();
}

function inboxOpenItems() { return state.inbox.items.filter((r) => r.status === 'pending'); }
function inboxNeeds() {
  return inboxOpenItems().filter((r) => IB_NEEDS.includes(r.kind) && !(r.snoozed_until > Date.now()))
    .sort((a, b) => (IB_URG[a.urgency] - IB_URG[b.urgency]) || (b.created_at - a.created_at));
}
function inboxUpdates() { return inboxOpenItems().filter((r) => r.kind === 'update').sort((a, b) => b.created_at - a.created_at); }
function inboxDone() { return state.inbox.items.filter((r) => r.status !== 'pending').sort((a, b) => (b.updated_at || 0) - (a.updated_at || 0)); }
function inboxBadgeCount() { return inboxNeeds().filter((r) => !(r.snoozed_until > Date.now())).length; }

function ibAllFlags(r) {
  const out = (r.flags || []).map((f) => Object.assign({ tool: '' }, f));
  for (const t of (r.tools || [])) for (const f of (t.flags || [])) out.push(Object.assign({ tool: t.tool }, f));
  return out;
}
function ibFlagSummary(r) {
  const m = new Map();
  for (const f of ibAllFlags(r)) {
    const e = m.get(f.label) || { label: f.label, level: f.level, n: 0 };
    e.n++; m.set(f.label, e);
  }
  return [...m.values()].sort((a, b) => (a.level === 'red' ? 0 : 1) - (b.level === 'red' ? 0 : 1));
}
const IB_FLAG_SVG = 'M2.2 1h1.3v10H2.2zM3.5 1.4h6.3L8.4 4l1.4 2.6H3.5z';
function ibSvg(path, box = '0 0 12 12', cls = '') {
  const ns = 'http://www.w3.org/2000/svg';
  const s = document.createElementNS(ns, 'svg');
  s.setAttribute('viewBox', box); s.setAttribute('aria-hidden', 'true');
  if (cls) s.setAttribute('class', cls);
  const p = document.createElementNS(ns, 'path');
  p.setAttribute('d', path); p.setAttribute('fill', 'currentColor');
  s.appendChild(p);
  return s;
}
function ibFlagChip(f) {
  return el('span', { class: 'ib-flag ib-f-' + f.level }, ibSvg(IB_FLAG_SVG), f.label + (f.n > 1 ? ' ×' + f.n : ''));
}
function ibAvatar(name, big) {
  return el('span', { class: 'ib-av' + (big ? ' big' : '') }, (name || '?').trim().charAt(0).toUpperCase());
}
function ibAge(ms) {
  const m = Math.max(0, Math.round((Date.now() - ms) / 60000));
  if (m < 1) return 'now';
  if (m < 60) return m + 'm';
  if (m < 1440) return Math.round(m / 60) + 'h';
  return Math.round(m / 1440) + 'd';
}
function ibClock(ms) { const d = new Date(ms); return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }
function ibStatusPill(r) {
  const map = {
    approved: ['good', 'Approved'], answered: ['good', 'Answered'], read: ['mute', 'Read'],
    denied: ['bad', 'Denied'], returned: ['bad', 'Sent back'], cancelled: ['mute', 'Withdrawn'], expired: ['mute', 'Expired'],
  };
  if (r.status === 'pending') {
    if (r.snoozed_until > Date.now()) return el('span', { class: 'ib-pill mute' }, 'Snoozed');
    return el('span', { class: 'ib-pill u-' + r.urgency }, { now: 'Now', soon: 'Soon', digest: 'Digest', fyi: 'FYI' }[r.urgency] || r.urgency);
  }
  const m = map[r.status] || ['mute', r.status];
  return el('span', { class: 'ib-pill ' + m[0] }, m[1]);
}

// ibAttnPills: what toolyard's attention rules did to this request.
function ibAttnPills(r) {
  if (r.status !== 'pending') return [];
  const out = [];
  if (r.reminded_at) out.push(el('span', { class: 'ib-pill amber', title: 'The agent is blocked on this; toolyard reminded you once.' }, 'Agent blocked'));
  if (r.downgraded) out.push(el('span', { class: 'ib-pill mute', title: 'Lowered from now: ' + r.downgraded }, 'Lowered from now'));
  return out;
}

// ---- voice notes (browser speech stands in for toolyard's TTS) ------------

const ibVoice = { id: null, i: 0, playing: false, rate: 1, sim: false, timer: null, token: 0, t0: 0, est: 0, raf: 0, script: {}, audio: null, audioId: null };
function ibSplit(t) { return (t || '').replace(/([.!?])\s+(?=[A-Z])/g, '$1\u0000').split('\u0000').filter(Boolean); }
function ibSecs(s) { return s.trim().split(/\s+/).length / 2.6 + 0.35; }
function ibReq(id) {
  return state.inbox.items.find((x) => x.id === id) || (state.inbox.detail && state.inbox.detail.request.id === id ? state.inbox.detail.request : null);
}
function ibSents(id) {
  const r = ibReq(id);
  const script = r ? (r.audio && r.audio.script) || '' : (ibVoice.script[id] || '');
  ibVoice.script[id] = script;
  return ibSplit(script);
}
// ibRec is the recorded voice note (toolyard's server-side TTS), if any.
function ibRec(id) { const r = ibReq(id); return r && r.audio && r.audio.blob ? '/v1/inbox/blobs/' + r.audio.blob : ''; }
function ibAudioOn(id) { return !!(ibVoice.audio && ibVoice.audioId === id); }
function ibEstTotal(id) { return ibSents(id).reduce((a, s) => a + ibSecs(s), 0); }
function ibTotal(id) {
  if (ibAudioOn(id) && isFinite(ibVoice.audio.duration) && ibVoice.audio.duration > 0) return ibVoice.audio.duration;
  return ibEstTotal(id);
}
function ibFmt(s) { s = Math.max(0, Math.round(s)); return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0'); }
function ibHalt() {
  ibVoice.token++; clearTimeout(ibVoice.timer); ibVoice.playing = false;
  if (ibVoice.audio) { try { ibVoice.audio.pause(); } catch (_) {} }
  if ('speechSynthesis' in window) { try { speechSynthesis.cancel(); } catch (_) {} }
}
async function ibPlay(id) {
  if (ibVoice.id !== id) { ibHalt(); ibVoice.id = id; ibVoice.i = 0; }
  if (ibVoice.loading === id) return;
  if (!ibRec(id)) {
    const token = ++ibVoice.token;
    ibVoice.loading = id; ibSync();
    try {
      const out = await api('/v1/inbox/' + encodeURIComponent(id) + '/audio', { method: 'POST', body: {} });
      const r = ibReq(id); if (r && out.audio) r.audio = out.audio;
    } catch (e) { toast(e.message + ' Using browser speech.', 'error'); }
    finally { if (ibVoice.loading === id) ibVoice.loading = null; ibSync(); }
    if (token !== ibVoice.token || ibVoice.id !== id) return;
  }
  const rec = ibRec(id);
  if (rec) {
    if (!ibAudioOn(id)) {
      if (ibVoice.audio) { try { ibVoice.audio.pause(); } catch (_) {} }
      const a = new Audio(rec);
      a.preload = 'auto';
      a.addEventListener('ended', ibFinish);
      a.addEventListener('loadedmetadata', ibSyncProgress);
      a.addEventListener('error', () => {
        // Recording unavailable: fall back to the browser's voice.
        if (ibVoice.audio === a) { ibVoice.audio = null; ibVoice.audioId = null; }
        const r = ibReq(id); if (r && r.audio) r.audio.blob = '';
        if (ibVoice.playing && ibVoice.id === id) { ibVoice.i = 0; ibSpeak(); ibLoop(); ibSync(); }
      });
      ibVoice.audio = a; ibVoice.audioId = id;
    }
    const a = ibVoice.audio;
    if (a.ended || (a.duration && a.currentTime >= a.duration)) a.currentTime = 0;
    a.playbackRate = ibVoice.rate;
    ibVoice.playing = true;
    const p = a.play(); if (p && p.catch) p.catch(() => { ibVoice.playing = false; ibSync(); });
    ibLoop(); ibSync();
    return;
  }
  if (ibVoice.i >= ibSents(id).length) ibVoice.i = 0;
  ibVoice.playing = true; ibSpeak(); ibLoop(); ibSync();
}
function ibToggle(id) { if (ibVoice.playing && ibVoice.id === id) { ibHalt(); ibSync(); } else ibPlay(id); }
function ibSetRate(rate) {
  ibVoice.rate = rate;
  if (ibVoice.audio) ibVoice.audio.playbackRate = rate;
  else if (ibVoice.playing) { ibHalt(); ibVoice.playing = true; ibSpeak(); ibLoop(); }
  ibSync();
}
function ibSpeak() {
  const tok = ++ibVoice.token, ss = ibSents(ibVoice.id);
  if (ibVoice.i >= ss.length) { ibFinish(); return; }
  const text = ss[ibVoice.i], est = ibSecs(text) / ibVoice.rate * 1000;
  ibVoice.t0 = performance.now(); ibVoice.est = est;
  const next = () => {
    if (tok !== ibVoice.token) return;
    clearTimeout(ibVoice.timer); ibVoice.i++;
    if (ibVoice.i >= ss.length) ibFinish(); else { ibSpeak(); ibSync(); }
  };
  let synth = false;
  if (!ibVoice.sim && 'speechSynthesis' in window) {
    try {
      const u = new SpeechSynthesisUtterance(text);
      u.rate = ibVoice.rate; u.lang = 'en-US'; u.onend = next;
      speechSynthesis.speak(u); synth = true;
    } catch (_) { ibVoice.sim = true; }
  } else ibVoice.sim = true;
  ibVoice.timer = setTimeout(next, synth ? est * 1.9 + 1500 : est);
}
function ibFinish() { ibVoice.playing = false; ibVoice.token++; clearTimeout(ibVoice.timer); ibVoice.i = ibSents(ibVoice.id).length; ibSync(); }
function ibProgress(id) {
  if (ibVoice.id !== id) return 0;
  if (ibAudioOn(id)) {
    const a = ibVoice.audio;
    if (a.ended) return 1;
    return a.duration > 0 && isFinite(a.duration) ? Math.min(1, a.currentTime / a.duration) : 0;
  }
  const ss = ibSents(id), tot = ibEstTotal(id) || 1;
  let before = 0;
  for (let k = 0; k < Math.min(ibVoice.i, ss.length); k++) before += ibSecs(ss[k]);
  const part = (ibVoice.playing && ibVoice.i < ss.length) ? Math.min(1, (performance.now() - ibVoice.t0) / ibVoice.est) * ibSecs(ss[ibVoice.i]) : 0;
  return Math.min(1, (before + part) / tot);
}
// ibAudioSentence maps recording progress onto the transcript, weighting
// sentences by length.
function ibAudioSentence(id) {
  const ss = ibSents(id), tot = ibEstTotal(id) || 1, p = ibProgress(id);
  if (p >= 1) return ss.length;
  let acc = 0;
  for (let k = 0; k < ss.length; k++) { acc += ibSecs(ss[k]) / tot; if (p < acc) return k; }
  return ss.length;
}
function ibLoop() { cancelAnimationFrame(ibVoice.raf); const f = () => { ibSyncProgress(); if (ibVoice.playing) ibVoice.raf = requestAnimationFrame(f); }; ibVoice.raf = requestAnimationFrame(f); }
function ibSyncProgress() {
  if (ibVoice.id && ibAudioOn(ibVoice.id)) {
    const k = ibAudioSentence(ibVoice.id);
    if (k !== ibVoice.i) { ibVoice.i = k; ibSync(); return; }
  }
  document.querySelectorAll('[data-ib-prog]').forEach((n) => { n.style.width = (ibProgress(n.dataset.ibProg) * 100).toFixed(1) + '%'; });
  document.querySelectorAll('[data-ib-wave]').forEach((n) => {
    const p = ibProgress(n.dataset.ibWave), c = n.children.length;
    for (let k = 0; k < c; k++) n.children[k].classList.toggle('on', k < Math.round(p * c));
  });
  document.querySelectorAll('[data-ib-time]').forEach((n) => {
    const id = n.dataset.ibTime, rate = ibVoice.id === id && !ibAudioOn(id) ? ibVoice.rate : 1;
    const tot = ibTotal(id) / rate, p = ibProgress(id);
    n.textContent = (ibVoice.id === id && (ibVoice.playing || (p > 0 && p < 1))) ? '−' + ibFmt(tot * (1 - p)) : ibFmt(tot);
  });
}
function ibSync() {
  document.querySelectorAll('[data-ib-vsrc]').forEach((n) => {
    const id = n.dataset.ibVsrc;
    n.textContent = ibRec(id) ? 'saved recording' : (ibVoice.loading === id ? 'generating audio' :
      (ibVoice.playing && ibVoice.id === id ? 'read by your browser' : 'not recorded yet'));
  });
  document.querySelectorAll('[data-ib-play]').forEach((n) => {
    const on = ibVoice.playing && ibVoice.id === n.dataset.ibPlay;
    const loading = ibVoice.loading === n.dataset.ibPlay;
    n.disabled = loading;
    n.classList.toggle('playing', on);
    n.setAttribute('aria-label', loading ? 'Generating voice note' : (on ? 'Pause voice note' : 'Play voice note'));
  });
  document.querySelectorAll('[data-ib-sent]').forEach((n) => {
    const [id, k] = n.dataset.ibSent.split(':'); const i = +k, act = ibVoice.id === id;
    n.classList.toggle('cur', act && i === ibVoice.i && ibVoice.i < ibSents(id).length);
    n.classList.toggle('past', act && i < ibVoice.i);
  });
  document.querySelectorAll('[data-ib-rate]').forEach((n) => { n.textContent = ibVoice.rate + '×'; });
  document.querySelectorAll('[data-ib-sim]').forEach((n) => { n.hidden = !ibVoice.sim || !!ibRec(ibVoice.id); });
  ibSyncProgress();
}
function ibPlayIcon() { return el('span', { class: 'ib-playicon' }); }

// ---- list -----------------------------------------------------------------

function viewInbox() {
  if (state.inbox.openId) return inboxDetailNode();
  const lists = { needs: inboxNeeds(), snoozed: inboxOpenItems().filter(r => r.snoozed_until > Date.now()), updates: inboxUpdates(), done: inboxDone() };
  const cur = lists[state.inbox.filter];
  const chip = (k, label) => el('button', {
    class: 'ib-chip' + (state.inbox.filter === k ? ' on' : ''),
    on: { click: () => { state.inbox.filter = k; render(); } },
  }, label, el('span', { class: 'n' }, state.inbox.loaded ? String(lists[k].length) : '·'));
  let body;
  if (!state.inbox.loaded) body = el('div', { class: 'ib-list' }, ...[0, 1, 2].map(() => el('div', { class: 'ib-skcard' },
    el('div', { class: 'ib-sk w55' }), el('div', { class: 'ib-sk h18 w85' }), el('div', { class: 'ib-sk w95' }))));
  else if (!cur.length) body = el('div', { class: 'ib-empty' },
    el('b', {}, state.inbox.filter === 'needs' ? 'Nothing needs you' : 'Nothing here'),
    el('span', {}, state.inbox.filter === 'needs' ? 'Your agents are working. Requests they send will show up here.' : ''));
  else body = el('div', { class: 'ib-list' }, ...cur.map(ibCard));
  let tools = null;
  if (state.inbox.filter === 'updates' && state.inbox.loaded) {
    const info = state.inbox.info || {};
    tools = el('div', { class: 'ib-bar' },
      el('span', { class: 'meta' }, info.next_digest ? 'Next digest ' + new Date(info.next_digest).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' }) : 'Updates never buzz your phone.'),
      cur.length ? el('button', { on: { click: async () => {
        try {
          const out = await api('/v1/inbox/batch', { method: 'POST', body: { ids: cur.map((r) => r.id), action: 'read' } });
          toast(`Marked ${out.done} as read.`);
        } catch (e) { toast(e.message, 'error'); }
        loadInbox();
      } } }, 'Mark all read') : null);
  }
  return el('div', { class: 'ib' },
    el('div', { class: 'ib-head' },
      el('div', {},
        el('h2', {}, 'Inbox'),
        el('div', { class: 'meta' }, state.inbox.loaded ? `${lists.needs.length} need${lists.needs.length === 1 ? 's' : ''} you` : 'Loading…'),
      ),
    ),
    el('div', { class: 'ib-chips' }, chip('needs', 'Needs you'), chip('snoozed', 'Snoozed'), chip('updates', 'Updates'), chip('done', 'Resolved')),
    tools,
    body,
  );
}

function ibCard(r) {
  const fs = ibFlagSummary(r);
  const card = el('div', {
    class: 'ib-card' + (r.reminded_at && r.status === 'pending' ? ' amber' : ''), tabindex: '0', role: 'button',
    on: {
      click: () => openInboxRequest(r.id),
      keydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openInboxRequest(r.id); } },
    },
  },
    el('div', { class: 'ib-sender' }, ibAvatar(r.agent_name), el('b', {}, r.agent_name),
      el('span', { class: 's' }, r.session_title || ''), el('span', { class: 't' }, ibAge(r.created_at))),
    el('div', { class: 'ib-kindrow' }, el('span', { class: 'ib-kind' }, (IB_KIND[r.kind] || r.kind) + (r.kind === 'access' ? ` · ${(r.tools || []).length} tool${(r.tools || []).length === 1 ? '' : 's'}` : '')), ibStatusPill(r),
      ...ibAttnPills(r), r.checked ? null : el('span', { class: 'ib-pill mute' }, 'Checking…')),
    el('h3', {}, r.title),
    r.summary !== r.title ? el('p', { class: 'sum' }, r.summary) : null,
    fs.length ? el('div', { class: 'ib-flags' }, ...fs.map(ibFlagChip)) : null,
    ibDeciderEl(r),
    r.audio?.script ? el('div', { class: 'ib-minirow' },
      el('button', {
        class: 'ib-miniplay', 'data-ib-play': r.id, 'aria-label': 'Play voice note',
        on: { click: (e) => { e.stopPropagation(); ibToggle(r.id); } },
      }, ibPlayIcon(), el('span', { 'data-ib-time': r.id }, ibFmt(ibTotal(r.id)))),
      el('div', { class: 'ib-minibar' }, el('i', { 'data-ib-prog': r.id })),
    ) : null,
  );
  return card;
}

function openInboxRequest(id) {
  ibHalt();
  state.inbox.openId = id;
  state.inbox.detail = null;
  state.inbox.panel = null;
  state.inbox.busy = null;
  state.route = 'inbox';
  if (location.hash !== '#inbox/' + id) history.pushState(null, '', '#inbox/' + id);
  ibNode = null;
  render();
  window.scrollTo(0, 0);
  loadInboxDetail(id, true);
}

function closeInboxRequest() {
  ibHalt();
  state.inbox.openId = null;
  state.inbox.detail = null;
  ibNode = null;
  ibObserver && ibObserver.disconnect();
  if (location.hash !== '#inbox') history.pushState(null, '', '#inbox');
  render();
  loadInbox();
}

async function loadInboxDetail(id, first) {
  try {
    const res = await api('/v1/inbox/' + encodeURIComponent(id));
    if (state.inbox.openId !== id) return;
    const had = !!state.inbox.detail;
    state.inbox.detail = res;
    ibVoice.script[id] = (res.request.audio && res.request.audio.script) || '';
    if (!had || first) ibBuildDetail();
    else ibRefreshRegions();
  } catch (e) {
    if (state.inbox.openId !== id) return;
    toast(e.status === 404 ? 'That request no longer exists.' : e.message, 'error');
    closeInboxRequest();
  }
}

// ---- detail ---------------------------------------------------------------

let ibNode = null;
let ibObserver = null;

function inboxDetailNode() {
  if (ibNode) return ibNode;
  ibNode = el('div', { class: 'ib ib-detail' },
    el('div', { class: 'ib-dtop' },
      el('div', { class: 'row' },
        el('button', { class: 'ib-back', on: { click: closeInboxRequest } }, '‹ Inbox'),
        el('button', { class: 'ib-jumpsm', on: { click: ibJump } }, 'Decision ↓'),
      ),
      el('div', { class: 'ib-readbar' }, el('i', { id: 'ib-readp' })),
    ),
    el('div', { class: 'ib-dbody' },
      el('section', { 'data-region': 'head' }, el('div', { class: 'ib-sk h26 w85' }), el('div', { class: 'ib-sk w55' })),
      el('section', { 'data-region': 'flags' }),
      el('section', { 'data-region': 'voice' }, el('div', { class: 'ib-sk h110' })),
      el('section', { 'data-region': 'msg' }, el('div', { class: 'ib-sk' }), el('div', { class: 'ib-sk w85' })),
      el('section', { 'data-region': 'att' }),
      el('section', { 'data-region': 'decide', class: 'ib-decide', id: 'ib-decide', hidden: true }),
      el('section', { 'data-region': 'activity' }),
    ),
    el('button', { class: 'ib-jump hide', id: 'ib-jump', on: { click: ibJump } }, '↓ Jump to decision'),
  );
  return ibNode;
}

function ibRegion(name, ...children) {
  if (!ibNode) return;
  const r = ibNode.querySelector(`[data-region="${name}"]`);
  if (!r) return;
  const focus = captureFocus();
  r.innerHTML = '';
  for (const c of children.flat()) if (c) r.appendChild(c);
  restoreFocus(focus);
}

function ibBuildDetail() {
  const d = state.inbox.detail; if (!d || !ibNode) return;
  const r = d.request;
  if (r.kind === 'access') ibAccessDraft(r);
  ibRegion('head', ibHeadEl(r));
  ibRegion('flags', ibFlagsEl(r));
  ibRegion('voice', r.audio?.script ? el('details', { class: 'optional-audio' }, el('summary', {}, 'Listen to the summary'), ibVoiceEl(r)) : null);
  ibRegion('msg', ibMsgEl(r));
  ibRegion('att', ibAttEl(r));
  const dec = ibNode.querySelector('#ib-decide'); dec.hidden = false;
  ibRegion('decide', ibDecideEl(r, d.grants || []));
  ibRegion('activity', ibActivityEl(r));
  ibSync();
  ibObserveDecision(r);
}

function ibRefreshRegions() {
  const d = state.inbox.detail; if (!d || !ibNode) return;
  const r = d.request;
  ibRegion('head', ibHeadEl(r));
  ibRegion('flags', ibFlagsEl(r));
  if (!(r.status === 'pending' && ibNode.querySelector('[data-region=decide]')?.contains(document.activeElement))) ibRegion('decide', ibDecideEl(r, d.grants || []));
  ibRegion('activity', ibActivityEl(r));
  ibObserveDecision(r);
}

function ibHeadEl(r) {
  const ses = [r.session_title, r.session_repo, r.session_host].filter(Boolean).join(' · ');
  return el('div', { class: 'ib-dh' },
    el('div', { class: 'ib-from' }, ibAvatar(r.agent_name, true),
      el('div', {}, el('b', {}, r.agent_name), el('span', {}, IB_VERB[r.kind] + (ses ? ' · ' + ses : '')))),
    el('div', { class: 'ib-kindrow' },
      el('span', { class: 'ib-kind' }, (IB_KIND[r.kind] || r.kind) + (r.kind === 'access' ? ` · ${r.tools.length} tool${r.tools.length === 1 ? '' : 's'}` : '')),
      ibStatusPill(r), ...ibAttnPills(r), el('span', { class: 'meta' }, relTime(r.created_at))),
    el('h2', {}, r.title),
    r.task ? el('div', { class: 'meta task-context' }, 'Task · ', r.task.url ? el('a', { href: r.task.url, target: '_blank', rel: 'noopener noreferrer' }, r.task.title || r.task.objective || 'Open task') : (r.task.objective || r.task.title)) : null,
  );
}

function ibFlagsEl(r) {
  const all = ibAllFlags(r);
  if (!r.checked) return el('p', { class: 'ib-clean' }, el('span', { class: 'ib-spin' }), 'Toolyard is checking this in the background…');
  const extra = [];
  if (r.dry_run_count > 0) extra.push(el('span', { class: 'ib-note' }, `${r.dry_run_count} dry run${r.dry_run_count === 1 ? '' : 's'} before sending`));
  if ((r.dropped_flags || []).length) extra.push(el('span', { class: 'ib-note red' }, 'Flags gone since the dry runs: ' + r.dropped_flags.join(', ')));
  if (!all.length && r.kind !== 'access') return el('div');
  if (!all.length) return el('div', {}, el('p', { class: 'ib-clean' }, '✓ Toolyard checked this in the background. Nothing to flag.'), ...extra);
  const open = !!state.inbox.flagsOpen;
  return el('div', { class: 'ib-tystrip' },
    el('button', { class: 'ib-tyhd', 'aria-expanded': String(open), on: { click: () => { state.inbox.flagsOpen = !open; ibRegion('flags', ibFlagsEl(r)); } } },
      el('span', { class: 'ib-tymark' }, 'Toolyard flags'), el('span', { class: 'ib-chev' }, open ? 'Hide' : 'Why?'),
      el('span', { class: 'ib-flags' }, ...ibFlagSummary(r).map(ibFlagChip))),
    open ? el('ul', { class: 'ib-tywhy' }, ...all.map((f) => el('li', {}, ibFlagChip(f),
      el('span', {}, f.tool ? el('code', {}, f.tool) : null, f.tool ? ' · ' : '', f.why, f.source === 'judge' ? ' (judge model)' : '')))) : null,
    extra.length ? el('div', { class: 'ib-tyextra' }, ...extra) : null,
  );
}

function ibVoiceEl(r) {
  const sents = ibSplit(r.audio && r.audio.script);
  const bars = el('div', { class: 'ib-wave', 'data-ib-wave': r.id, 'aria-hidden': 'true' });
  let seed = r.id.split('').reduce((a, c) => a + c.charCodeAt(0), 0);
  for (let k = 0; k < 44; k++) {
    seed = (seed * 9301 + 49297) % 233280;
    const env = 0.35 + 0.65 * Math.sin(Math.PI * (k + 1) / 45);
    const b = el('span'); b.style.height = Math.round((20 + (seed / 233280) * 80) * env) + '%';
    bars.appendChild(b);
  }
  const tx = el('p', { class: 'ib-transcript', hidden: !state.inbox.tx },
    ...sents.map((s, k) => el('span', { 'data-ib-sent': r.id + ':' + k }, s + ' ')));
  return el('div', { class: 'ib-player' },
    el('div', { class: 'ib-plabel' }, el('span', {}, 'Voice note from ' + r.agent_name),
      el('span', { class: 'ib-vsrc', 'data-ib-vsrc': r.id }, r.audio && r.audio.blob ? 'saved recording' : 'not recorded yet'),
      el('button', { class: 'ib-rate', 'data-ib-rate': '1', on: { click: () => {
        ibSetRate(ibVoice.rate === 1 ? 1.25 : ibVoice.rate === 1.25 ? 1.5 : 1);
      } } }, ibVoice.rate + '×')),
    el('div', { class: 'ib-prow' },
      el('button', { class: 'ib-bigplay', 'data-ib-play': r.id, 'aria-label': 'Play voice note', on: { click: () => ibToggle(r.id) } }, ibPlayIcon()),
      bars,
      el('span', { class: 'ib-ptime', 'data-ib-time': r.id }, ibFmt(ibTotal(r.id)))),
    tx,
    el('button', { class: 'ib-link', on: { click: (e) => { state.inbox.tx = !state.inbox.tx; tx.hidden = !state.inbox.tx; e.target.textContent = state.inbox.tx ? 'Hide transcript' : 'Show transcript'; } } },
      state.inbox.tx ? 'Hide transcript' : 'Show transcript'),
    el('p', { class: 'ib-simnote', 'data-ib-sim': '1', hidden: true }, 'Speech isn’t available in this browser, so the transcript highlights at speaking pace.'),
  );
}

function ibMsgEl(r) {
  const facts = r.facts ? el('dl', { class: 'ib-facts' },
    el('dt', {}, 'Why now'), el('dd', {}, r.facts.why_now),
    el('dt', {}, 'If it goes wrong'), el('dd', {}, r.facts.if_it_goes_wrong),
    el('dt', {}, 'Undo'), el('dd', {}, r.facts.undo)) : null;
  const holder = el('div', { class: 'ib-tyholder' });
  const renderTy = () => {
    holder.innerHTML = '';
    const s = state.inbox.summary[r.id];
    if (!s && !state.inbox.info?.judge_available) return;
    if (!s) {
      holder.appendChild(el('button', { class: 'ib-tybtn', on: { click: async () => {
        state.inbox.summary[r.id] = { loading: true }; renderTy();
        try {
          const out = await api('/v1/inbox/' + r.id + '/summarize', { method: 'POST', body: {} });
          state.inbox.summary[r.id] = { text: out.text, source: out.source, shown: '' };
          ibTypewrite(state.inbox.summary[r.id], renderTy);
        } catch (e) { state.inbox.summary[r.id] = null; renderTy(); toast(e.message, 'error'); }
      } } }, '✦ Summarize with toolyard'));
      return;
    }
    if (s.loading) { holder.appendChild(el('div', { class: 'ib-tycard' }, el('span', { class: 'ib-spin' }), ' Toolyard is reading the request…')); return; }
    holder.appendChild(el('div', { class: 'ib-tycard' },
      el('div', { class: 'ib-tyl' }, el('span', {}, '✦ Toolyard summary · ' + (s.source === 'judge' ? 'judge model' : 'from its rules')),
        el('button', { class: 'ib-link', on: { click: () => { state.inbox.summary[r.id] = null; renderTy(); } } }, 'Hide')),
      el('p', {}, s.shown || s.text)));
  };
  renderTy();
  return el('div', { class: 'ib-msg' }, el('p', {}, r.message), facts, holder);
}

function ibTypewrite(obj, rerender) {
  const words = (obj.text || '').split(' ');
  let i = 0;
  const step = () => {
    if (!obj || obj.shown === undefined) return;
    i++; obj.shown = words.slice(0, i).join(' ');
    rerender();
    if (i < words.length) setTimeout(step, 28); else obj.shown = obj.text;
  };
  step();
}

// ---- attachments ----------------------------------------------------------

function ibAttEl(r) {
  const atts = r.attachments || [];
  if (!atts.length) return null;
  return el('div', { class: 'ib-sec' },
    el('div', { class: 'ib-eyebrow' }, 'What I’m attaching · ' + atts.length),
    ...atts.map((a, k) => ibAttachment(r, a, k)));
}

const IB_TYPE_LABEL = { markdown: 'Note', table: 'Table', chart: 'Chart', diff: 'Code change', code: 'Code', log: 'Log', image: 'Screenshot', video: 'Recording', file: 'File', link: 'Link' };

function ibAttachment(r, a, k) {
  const title = a.title || a.name || a.file || a.label || IB_TYPE_LABEL[a.type] || a.type;
  const hd = el('div', { class: 'ib-atthd' }, el('b', {}, title), el('span', { class: 'ib-atype' }, IB_TYPE_LABEL[a.type] || a.type));
  const cap = a.caption ? el('div', { class: 'ib-cap' }, ibAvatar(r.agent_name), el('span', {}, a.caption)) : null;
  const spoken = a.spoken ? el('div', { class: 'ib-spoken' }, 'In the voice note: “', a.spoken, '”') : null;
  let body = null;
  const blob = (sha, name) => '/v1/inbox/blobs/' + sha + (name ? '?name=' + encodeURIComponent(name) : '');
  const notCopied = () => el('div', { class: 'ib-attbody ib-notcopied' },
    el('p', {}, a.fetch_error ? 'Toolyard couldn’t copy this: ' + a.fetch_error : 'Not copied (copying is off in Settings).'),
    a.url ? el('a', { href: a.url, target: '_blank', rel: 'noopener noreferrer' }, 'Open the original link ↗') : null);
  switch (a.type) {
    case 'markdown': body = el('div', { class: 'ib-attbody ib-md' }, ...ibMarkdown(a.body)); break;
    case 'table': body = el('div', { class: 'ib-attbody ib-tbl' }, el('table', {},
      el('thead', {}, el('tr', {}, ...(a.columns || []).map((c) => el('th', {}, String(c))))),
      el('tbody', {}, ...(a.rows || []).map((row) => el('tr', {}, ...row.map((c) => el('td', { class: typeof c === 'number' ? 'num' : '' }, c == null ? '' : String(c)))))))); break;
    case 'chart': body = el('div', { class: 'ib-attbody' }, ibChart(a)); break;
    case 'diff': body = ibCode(a.file, (a.patch || '').split('\n'), true); break;
    case 'code': body = ibCode(a.file || a.language || '', (a.body || '').split('\n'), false); break;
    case 'log': body = el('pre', { class: 'ib-log' }, a.body || ''); break;
    case 'image':
      body = a.blob ? el('button', { class: 'ib-imgbtn', 'aria-label': 'Open image full screen', on: { click: () => ibLightbox(blob(a.blob), a.caption || a.alt || title) } },
        el('img', { src: blob(a.blob), alt: a.alt || title, loading: 'lazy' })) : notCopied();
      break;
    case 'video':
      if (a.blob) {
        body = el('video', { controls: true, playsInline: true, preload: 'metadata', src: blob(a.blob) });
        if (a.poster_blob) body.poster = blob(a.poster_blob);
      } else body = notCopied();
      break;
    case 'file':
      body = a.blob ? el('div', { class: 'ib-file' }, el('span', { class: 'ib-fic' }, (a.name || '').split('.').pop().slice(0, 4).toUpperCase() || 'FILE'),
        el('div', { class: 'fn' }, el('b', {}, a.name), el('span', {}, ibBytes(a.size) + (a.content_type ? ' · ' + a.content_type : ''))),
        el('a', { href: blob(a.blob, a.name), download: a.name }, 'Download')) : notCopied();
      break;
    case 'link': body = el('div', { class: 'ib-attbody' }, el('a', { class: 'ib-extlink', href: a.url, target: '_blank', rel: 'noopener noreferrer' },
      el('span', {}, a.label || a.url, el('small', {}, ibHost(a.url))), '↗')); break;
  }
  const flush = ['image', 'video', 'diff', 'code', 'log'].includes(a.type);
  return el('div', { class: 'ib-att' + (flush ? ' flush' : '') }, hd, body, spoken, cap);
}

function ibHost(u) { try { return new URL(u).host; } catch (_) { return ''; } }
function ibBytes(n) { if (!n) return ''; if (n < 1024) return n + ' B'; if (n < 1 << 20) return (n / 1024).toFixed(0) + ' KB'; return (n / (1 << 20)).toFixed(1) + ' MB'; }

function ibCode(file, lines, diff) {
  const add = diff ? lines.filter((l) => l.startsWith('+') && !l.startsWith('+++')).length : 0;
  const del = diff ? lines.filter((l) => l.startsWith('-') && !l.startsWith('---')).length : 0;
  return el('div', { class: 'ib-code' },
    el('div', { class: 'ib-codehd' }, el('span', {}, file || ''), diff ? el('span', {}, el('b', { class: 'add' }, '+' + add), ' ', el('b', { class: 'del' }, '−' + del)) : null),
    el('pre', {}, ...lines.map((l) => el('span', { class: 'ln' + (diff ? (l.startsWith('@@') ? ' hunk' : l.startsWith('+') ? ' add' : l.startsWith('-') ? ' del' : '') : '') }, l.replace(/\t/g, '  ') || ' '))));
}

// ibMarkdown renders a small, safe subset: paragraphs, "- " lists, "#"
// headings, `code` and **bold**. Everything goes through text nodes.
function ibMarkdown(src) {
  const out = [];
  const inline = (text) => {
    const frag = [];
    const re = /(`[^`]+`|\*\*[^*]+\*\*)/g;
    let last = 0, m;
    while ((m = re.exec(text))) {
      if (m.index > last) frag.push(document.createTextNode(text.slice(last, m.index)));
      const t = m[0];
      frag.push(t.startsWith('`') ? el('code', {}, t.slice(1, -1)) : el('strong', {}, t.slice(2, -2)));
      last = m.index + t.length;
    }
    if (last < text.length) frag.push(document.createTextNode(text.slice(last)));
    return frag;
  };
  const blocks = String(src || '').split(/\n{2,}/);
  for (const b of blocks) {
    const lines = b.split('\n');
    if (lines.every((l) => /^\s*[-*] /.test(l))) {
      out.push(el('ul', {}, ...lines.map((l) => el('li', {}, ...inline(l.replace(/^\s*[-*] /, ''))))));
    } else if (/^#{1,4} /.test(lines[0])) {
      out.push(el('p', {}, el('strong', {}, lines[0].replace(/^#+ /, ''))));
      if (lines.length > 1) out.push(el('p', {}, ...inline(lines.slice(1).join(' '))));
    } else {
      out.push(el('p', {}, ...inline(lines.join(' '))));
    }
  }
  return out;
}

function ibChart(a) {
  const ns = 'http://www.w3.org/2000/svg';
  const mk = (tag, attrs) => { const n = document.createElementNS(ns, tag); for (const k in attrs) n.setAttribute(k, attrs[k]); return n; };
  const W = 340, H = 160, L = 38, R = 8, T = 10, B = 22;
  const series = (a.series || []).filter((s) => (s.values || []).length);
  const all = series.flatMap((s) => s.values);
  if (!all.length) return el('p', { class: 'meta' }, 'No data');
  let lo = Math.min(0, ...all), hi = Math.max(...all);
  if (hi === lo) hi = lo + 1;
  const n = Math.max(...series.map((s) => s.values.length));
  const x = (i) => L + (n === 1 ? (W - L - R) / 2 : i * (W - L - R) / (n - 1));
  const y = (v) => T + (1 - (v - lo) / (hi - lo)) * (H - T - B);
  const svg = mk('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': a.title || 'chart', class: 'ib-chart' });
  const fmt = (v) => (Math.abs(v) >= 1000 ? Math.round(v).toLocaleString() : +v.toFixed(2)) + (a.unit === '%' ? '%' : '');
  [lo, (lo + hi) / 2, hi].forEach((v) => {
    svg.appendChild(mk('line', { class: 'grid', x1: L, x2: W - R, y1: y(v), y2: y(v) }));
    const t = mk('text', { class: 'axis', x: L - 5, y: y(v) + 3, 'text-anchor': 'end' }); t.textContent = fmt(v); svg.appendChild(t);
  });
  const xl = a.x || [];
  [[0, 'start'], [Math.floor((n - 1) / 2), 'middle'], [n - 1, 'end']].forEach(([i, anchor], j) => {
    if (xl[i] === undefined || (j === 1 && n < 3)) return;
    const t = mk('text', { class: 'axis', x: x(i), y: H - 6, 'text-anchor': anchor }); t.textContent = String(xl[i]); svg.appendChild(t);
  });
  if (a.chart === 'bar') {
    const groups = series.length, bw = (W - L - R) / n;
    series.forEach((s, si) => s.values.forEach((v, i) => {
      const w = Math.max(1, (bw - 4) / groups);
      const r = mk('rect', { class: 'bar s' + si, x: (L + i * bw + 2 + si * w).toFixed(1), y: y(Math.max(v, 0)).toFixed(1), width: w.toFixed(1), height: Math.abs(y(v) - y(0)).toFixed(1), rx: 2 });
      r.style.animationDelay = (i * 30) + 'ms';
      svg.appendChild(r);
    }));
  } else {
    series.forEach((s, si) => {
      const d = s.values.map((v, i) => (i ? 'L' : 'M') + x(i).toFixed(1) + ' ' + y(v).toFixed(1)).join(' ');
      if (si === 0) svg.appendChild(mk('path', { class: 'area', d: d + ` L${x(s.values.length - 1)} ${y(lo)} L${x(0)} ${y(lo)} Z` }));
      svg.appendChild(mk('path', { class: 'line s' + si, d }));
    });
  }
  const legend = series.length > 1 ? el('div', { class: 'ib-legend' }, ...series.map((s, si) => el('span', {}, el('i', { class: 's' + si }), s.name))) : null;
  return el('div', {}, svg, legend);
}

function ibLightbox(src, caption) {
  const box = el('div', { class: 'ib-lightbox', role: 'dialog', 'aria-label': 'Image', on: { click: () => box.remove() } },
    el('img', { src, alt: caption || '' }), caption ? el('p', {}, caption) : null, el('button', {}, 'Close'));
  document.body.appendChild(box);
}

// ---- decision (always last) -----------------------------------------------

function ibRedOf(t) { return (t.flags || []).find((f) => f.level === 'red'); }

function ibDecideEl(r, grants) {
  const open = r.status === 'pending';
  const busy = state.inbox.busy;
  const byIdx = {};
  for (const g of grants) byIdx[g.tool_index] = g;
  const result = ibResultEl(r);
  if (r.kind === 'access') {
    const d=ibAccessDraft(r), allow=state.inbox.allow[r.id];
    const n=r.tools.length,nOn=allow.filter(Boolean).length;
    const rows=r.tools.map((t,k)=>ibToolRow(r,t,k,allow,byIdx[k],open&&!busy));
    const setAll=fn=>()=>{ibSelectCalls(r,r.tools.map(fn));ibRegion('decide',ibDecideEl(r,grants));};
    const stale=d.revision!==(r.revision||1);
    const legacy=r.execution_mode==='legacy';
    const delivery=(state.inbox.detail?.callbacks || []).map(c=>el('div',{class:'meta'},'Callback '+c.status+' · '+c.attempts+' attempt(s)',c.terminal_reason?' · '+c.terminal_reason:'',
      ...(c.history||[]).map(a=>el('div',{},'Attempt '+a.attempt+' · '+a.outcome)),
      c.status==='failed'?el('button',{disabled:!!busy,on:{click:()=>ibRetryCallback(r,c)}},'Retry callback'):null));
    return el('div',{class:'ib-sec'},el('div',{class:'ib-eyebrow'},'Your decision'),
      el('h3',{},open?`${r.agent_name} requests ${n} call${n===1?'':'s'}`:'Saved decision'),
      open?el('p',{class:'meta'},'Select calls to accept. Clear calls to reject. Nothing is sent until you submit.'):result,
      legacy?el('p',{class:'ib-warn'},'Legacy execution: Toolyard executes accepted calls after this decision. It does not issue new grants.'):el('p',{class:'meta'},'The agent executes accepted calls with separate single-use grants.'),
      open?el('p',{class:'meta'},'Decision deadline: '+new Date(r.expires_at).toLocaleString()+(legacy?'.':'. Grant lifetime starts after acceptance.')):null,
      stale?el('div',{class:'ib-warn',role:'status'},r.status==='pending'?'The server revision changed. Your draft remains saved. Review the current calls before resubmission.':'The server has a final decision. Your unsent draft remains saved.',
        open?el('button',{on:{click:()=>{d.revision=r.revision||1;delete d.submission;delete d.fingerprint;ibSaveAccessDraft(r,d);ibRegion('decide',ibDecideEl(r,grants));}}},'Use reviewed revision'):null):null,
      open&&!busy?el('div',{class:'ib-quick'},el('button',{class:'ib-link',on:{click:setAll(()=>true)}},'Accept all'),el('button',{class:'ib-link',on:{click:setAll(()=>false)}},'Reject all')):null,
      el('div',{class:'ib-trows'},...rows),
      open?el('div',{class:'ib-actions'},
        el('label',{class:'meta',for:'ib-note'},'Overall note (optional)'),
        el('textarea',{id:'ib-note',value:d.note,rows:3,disabled:!!busy,on:{input:e=>{d.note=e.target.value;(state.inbox.notes||={})[r.id]=d.note;ibSaveAccessDraft(r,d);}}}),
        state.inbox.panel?ibPanelEl(r):el('div',{class:'ib-btns'},
          el('button',{disabled:!!busy,on:{click:()=>{state.inbox.panel='snooze';ibRegion('decide',ibDecideEl(r,grants));}}},'Later'),
          el('button',{class:'primary grow',disabled:!!busy||stale,on:{click:()=>ibApprove(r,allow)}},busy?'Submit decision…':`Accept ${nOn}, reject ${n-nOn}`)),
        !legacy?ibTTLEl(r):null,
        el('p',{class:'meta'},'A required label is a planning hint. You can reject every call.')):null,
      delivery.length?el('details',{class:'connection-setup'},el('summary',{},'Callback delivery'),...delivery):null);
  }
  if (r.kind === 'question' || r.kind === 'blocker') return ibAnswerForm(r);
  return el('div', { class: 'ib-sec' },
    el('div', { class: 'ib-eyebrow' }, 'Done reading?'),
    open ? el('button', { class: 'primary', disabled: !!busy, on: { click: () => ibDecide(r, { action: 'read' }) } }, 'Mark as read') : result);
}

function ibToolRow(r, t, k, allow, grant, editable) {
  const on = allow[k], red = ibRedOf(t);
  const key = r.id + ':' + ibCallID(t,k);
  let state_ = null;
  if (r.status !== 'pending') {
    if (t.decision === 'allowed') {
      const gs = grant ? grant.status : 'active';
      state_ = { active: ['allowed', 'Allowed · unused'], used: ['used', 'Used'], revoked: ['blocked', 'Revoked'], expired: ['blocked', 'Expired'] }[gs] || ['allowed', 'Allowed'];
    } else state_ = ['blocked', 'Not allowed'];
  }
  const ex = state.inbox.explain[key];
  const exEl = !ex ? null : ex.loading ? el('div', { class: 'ib-tycard' }, el('span', { class: 'ib-spin' }), ' Toolyard is reading this call…')
    : el('div', { class: 'ib-tycard' },
      el('div', { class: 'ib-tyl' }, el('span', {}, '✦ Toolyard’s reading · ' + (ex.source === 'judge' ? 'judge model' : 'from its rules')),
        el('button', { class: 'ib-link', on: { click: () => { delete state.inbox.explain[key]; ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || [])); } } }, 'Hide')),
      el('p', {}, ex.shown || ex.text));
  const params = Object.keys(t.params || {}).sort();
  return el('div', { class: 'ib-trow' + (on || state_ ? '' : ' off') + (red && on && editable ? ' red' : '') + (state_ && state_[0] === 'blocked' ? ' off' : '') },
    el('div', { class: 'ib-thd' },
      editable ? el('button', {
        class: 'ib-tick', role: 'checkbox', 'aria-checked': String(!!on), 'aria-label': 'Accept ' + ibCallID(t,k) + ' · ' + t.tool,
        on: { click: () => { allow[k] = !allow[k]; ibSelectCalls(r,allow); ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || [])); } },
      }, '✓') : null,
      el('div', { class: 'ib-tmain' },
        el('div', { class: 'ib-tline' }, el('code', { class: 'ib-tn' }, t.tool),
          el('span', { class: 'ib-tag' + (t.required ? ' req' : '') }, t.required ? 'Required hint' : 'Optional hint'),
          Object.keys(ibNarrowed(r, k)).length || t.requested_params ? el('span', { class: 'ib-tag narrowed' }, 'Narrowed') : null,
          state_ ? el('span', { class: 'ib-tstate ' + state_[0] }, state_[1]) : null),
        el('p', { class: 'ib-tsum' }, t.summary),
        el('div',{class:'meta'},'Call ID: '+ibCallID(t,k)),
        el('dl',{class:'ib-facts'},...Object.entries({Target:t.target,'Expected effects':t.expected_effects,'Affected scope':t.affected_scope,'Material risks':t.material_risks,Undo:t.undo}).filter(([,v])=>v).flatMap(([label,value])=>[el('dt',{},label),el('dd',{},value)])),
        editable?el('label',{class:'meta'},'Reason for this '+(on?'acceptance':'rejection')+' (optional)',el('textarea',{id:'ib-call-reason-'+k,rows:2,value:ibAccessDraft(r).reasons[ibCallID(t,k)]||'',on:{input:e=>{const d=ibAccessDraft(r);d.reasons[ibCallID(t,k)]=e.target.value;ibSaveAccessDraft(r,d);}}})):t.reason?el('p',{class:'meta'},'Your reason: '+t.reason):null,
        r.execution_mode==='legacy'&&r.status!=='pending'?el('p',{class:'meta'},'Decision: '+(t.verdict || (t.decision==='allowed'?'accepted':'rejected'))+'. Legacy execution: '+(r.legacy_execution_state||'outcome unknown')+'.'):null,
        grant?el('p',{class:'meta'},'Decision: '+(t.verdict || (t.decision==='allowed'?'accepted':'rejected'))+'. Execution: '+(grant.execution?.state || (grant.status==='active'?'not started':grant.status==='used'?'outcome unknown':grant.status))+'.'):null,
        state_ && ibGrantBy(grant) ? el('p', { class: 'ib-grantby' }, ibGrantBy(grant)) : null,
        (t.flags || []).length ? el('div', { class: 'ib-flags' }, ...t.flags.map(ibFlagChip)) : null,
        red && r.status === 'pending' ? el('p', { class: 'ib-flagwhy' }, red.why) : null,
        el('div', { class: 'ib-rowlinks' },
          el('button', { class: 'ib-link', on: { click: () => { state.inbox.openParams[key] = !state.inbox.openParams[key]; ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || [])); } } },
            state.inbox.openParams[key] ? 'Hide parameters' : 'Parameters'),
          ex ? null : el('button', { class: 'ib-tylink', on: { click: () => ibExplain(r, k) } }, '✦ Ask toolyard')),
      )),
    exEl,
    state.inbox.openParams[key] ? (r.execution_mode==='legacy' && t.legacy_params_json ? el('pre',{class:'ib-code'},t.legacy_params_json) : ibParamsEl(r, t, k, editable && on && r.execution_mode!=='legacy')) : null,
  );
}

// ---- scope editor: the owner can narrow parameters, never widen them ----

function ibNarrowed(r, k) { return (state.inbox.narrow[r.id] || {})[k] || {}; }
function ibSetNarrow(r, k, p, c) {
  const all = state.inbox.narrow[r.id] = state.inbox.narrow[r.id] || {};
  const tool = all[k] = all[k] || {};
  if (c === null) delete tool[p]; else tool[p] = c;
  if (!Object.keys(tool).length) delete all[k];
  const d=ibAccessDraft(r);d.narrow=all;ibSaveAccessDraft(r,d);
  ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
}
function ibParseVal(v) {
  const t = String(v).trim();
  if (/^-?\d+(\.\d+)?$/.test(t) || t === 'true' || t === 'false') return JSON.parse(t);
  return t;
}
// ibNarrowable: which constraint kinds can be tightened.
function ibNarrowable(c) {
  if (!c || typeof c !== 'object') return false;
  return 'in' in c || 'prefix' in c || 'limit' in c || 'any' in c || 'gte' in c || 'lte' in c;
}
function ibParamsEl(r, t, k, editable) {
  const key = r.id + ':' + k;
  const params = Object.keys(t.params || {}).sort();
  const narrowed = ibNarrowed(r, k);
  const req = t.requested_params || null; // set once the owner narrowed (after approval)
  const rows = params.map((p) => {
    const orig = t.params[p], cur = narrowed[p];
    const editing = state.inbox.editing[key + ':' + p];
    const shown = cur || orig;
    const kind = !shown || typeof shown !== 'object' || 'eq' in shown ? el('span', { class: 'ib-ex' }, 'Exact')
      : 'limit' in shown ? el('span', { class: 'ib-lim' }, 'Limit')
        : 'any' in shown ? el('span', { class: 'ib-lim' }, 'Any value') : el('span', { class: 'ib-ex' }, 'Bounded');
    const value = cur ? el('span', {}, el('b', {}, ibDescribe(cur)), el('small', { class: 'ib-was' }, ' was ' + ibDescribe(orig)))
      : req && req[p] && JSON.stringify(req[p]) !== JSON.stringify(orig) ? el('span', {}, el('b', {}, ibDescribe(orig)), el('small', { class: 'ib-was' }, ' narrowed from ' + ibDescribe(req[p])))
        : ibDescribe(orig);
    const action = editable && ibNarrowable(orig)
      ? (cur ? el('button', { class: 'ib-link', on: { click: () => ibSetNarrow(r, k, p, null) } }, 'Reset')
        : el('button', { class: 'ib-link', on: { click: () => { state.inbox.editing[key + ':' + p] = !editing; ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || [])); } } }, editing ? 'Cancel' : 'Narrow'))
      : null;
    const out = [el('tr', {}, el('td', {}, p), el('td', {}, el('div', {}, value), el('div', { class: 'ib-pmeta' }, kind, action)))];
    if (editing && !cur) out.push(el('tr', { class: 'ib-nrow' }, el('td', { colspan: '2' }, ibNarrowEditor(r, k, p, orig, () => { delete state.inbox.editing[key + ':' + p]; }))));
    return out;
  }).flat();
  return el('div', { class: 'ib-params' }, el('table', {}, el('tbody', {}, ...rows)),
    params.some((p) => t.params[p] && t.params[p].limit !== undefined) ? el('p', { class: 'meta' }, 'Limit values are checked (or, without a pattern, recorded) when the call is made.') : null,
    editable && params.some((p) => ibNarrowable(t.params[p])) ? el('p', { class: 'meta' }, 'You can narrow a parameter (fewer values, an exact value, a tighter range) but not widen it.') : null);
}
function ibNarrowEditor(r, k, p, orig, done) {
  const apply = (c) => { done(); ibSetNarrow(r, k, p, c); };
  const err = el('p', { class: 'ib-nerr', hidden: true });
  const fail = (m) => { err.textContent = m; err.hidden = false; };
  if ('in' in orig) {
    const picked = new Set(orig.in.map((v) => JSON.stringify(v)));
    return el('div', { class: 'ib-narrow' },
      el('div', { class: 'meta' }, 'Allow only:'),
      el('div', { class: 'ib-nchips' }, ...orig.in.map((v) => {
        const id = JSON.stringify(v);
        return el('label', { class: 'ib-check' }, el('input', { type: 'checkbox', checked: true, on: { change: (e) => { if (e.target.checked) picked.add(id); else picked.delete(id); } } }),
          el('span', {}, typeof v === 'string' ? v : id));
      })),
      err,
      el('button', { class: 'primary', on: { click: () => {
        const vals = orig.in.filter((v) => picked.has(JSON.stringify(v)));
        if (!vals.length) return fail('Keep at least one value, or untick the tool instead.');
        apply(vals.length === 1 ? { eq: vals[0] } : { in: vals });
      } } }, 'Use these'));
  }
  if ('gte' in orig || 'lte' in orig) {
    const lo = el('input', { type: 'number', value: orig.gte !== undefined ? String(orig.gte) : '', placeholder: 'min' });
    const hi = el('input', { type: 'number', value: orig.lte !== undefined ? String(orig.lte) : '', placeholder: 'max' });
    return el('div', { class: 'ib-narrow' },
      el('div', { class: 'ib-nrange' }, lo, el('span', {}, 'to'), hi),
      err,
      el('button', { class: 'primary', on: { click: () => {
        const c = {};
        if (lo.value !== '') c.gte = Number(lo.value);
        if (hi.value !== '') c.lte = Number(hi.value);
        if ((orig.gte !== undefined && (c.gte === undefined || c.gte < orig.gte)) || (orig.lte !== undefined && (c.lte === undefined || c.lte > orig.lte)))
          return fail('Stay within ' + ibDescribe(orig) + '.');
        if (c.gte !== undefined && c.lte !== undefined && c.gte > c.lte) return fail('The minimum is above the maximum.');
        apply(c.gte !== undefined && c.gte === c.lte ? { eq: c.gte } : c);
      } } }, 'Use this range'));
  }
  const input = el('input', { type: 'text', value: 'prefix' in orig ? orig.prefix : '', placeholder: 'limit' in orig ? 'the exact value, e.g. a commit SHA' : 'exact value' });
  const asPrefix = 'prefix' in orig;
  let prefixMode = asPrefix;
  return el('div', { class: 'ib-narrow' },
    el('div', { class: 'meta' }, asPrefix ? 'A longer prefix, or an exact value:' : 'Pin it to one exact value:'),
    input,
    asPrefix ? el('label', { class: 'ib-check' }, el('input', { type: 'checkbox', checked: true, on: { change: (e) => { prefixMode = e.target.checked; } } }), el('span', {}, 'Treat as a prefix')) : null,
    err,
    el('button', { class: 'primary', on: { click: () => {
      const v = input.value.trim();
      if (!v) return fail('Enter a value.');
      if (asPrefix && !v.startsWith(orig.prefix)) return fail('It has to start with ' + orig.prefix);
      if ('limit' in orig && orig.pattern) {
        let ok = true; try { ok = new RegExp(orig.pattern).test(v); } catch (_) {}
        if (!ok) return fail('That doesn’t match the pattern the agent gave (' + orig.pattern + ').');
      }
      apply(asPrefix && prefixMode ? { prefix: v } : { eq: asPrefix || 'limit' in orig ? v : ibParseVal(v) });
    } } }, 'Use this'));
}

function ibDescribe(c) {
  if (!c || typeof c !== 'object') return JSON.stringify(c);
  if ('eq' in c) return typeof c.eq === 'string' ? c.eq : JSON.stringify(c.eq);
  if ('in' in c) return 'one of ' + JSON.stringify(c.in);
  if ('prefix' in c) return 'starts with ' + c.prefix;
  if ('limit' in c) return 'not known yet: ' + c.limit + (c.pattern ? ` (must match ${c.pattern})` : '');
  if ('any' in c) return 'any value';
  if ('gte' in c || 'lte' in c) return [c.gte !== undefined ? '≥ ' + c.gte : '', c.lte !== undefined ? '≤ ' + c.lte : ''].filter(Boolean).join(' and ');
  return JSON.stringify(c);
}

async function ibExplain(r, k) {
  const key = r.id + ':' + ibCallID(r.tools[k],k);
  state.inbox.explain[key] = { loading: true };
  ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
  try {
    const out = await api('/v1/inbox/' + r.id + '/explain', { method: 'POST', body: { tool_index: k } });
    const obj = { text: out.text, source: out.source, shown: '' };
    state.inbox.explain[key] = obj;
    ibTypewrite(obj, () => { if (state.inbox.openId === r.id && state.inbox.detail) ibRegion('decide', ibDecideEl(state.inbox.detail.request, state.inbox.detail.grants || [])); });
  } catch (e) {
    delete state.inbox.explain[key];
    toast(e.message, 'error');
    ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
  }
}

function ibNote() { const n = document.getElementById('ib-note'); return n ? n.value.trim() : ''; }

function ibPanelEl(r) {
  const busy = state.inbox.busy;
  const back = () => { state.inbox.panel = null; ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || [])); };
  if (state.inbox.panel === 'deny') {
    return el('div', { class: 'ib-actions' },
      el('label', { class: 'meta', for: 'ib-note' }, `Tell ${r.agent_name} why (optional). It reads this before trying again.`),
      el('textarea', { id: 'ib-note', value: state.inbox.notes?.[r.id] || '', on: { input: e => { (state.inbox.notes ||= {})[r.id] = e.target.value; } }, rows: 3, placeholder: 'e.g. Don’t touch the flag; I’ll roll it out myself' }),
      el('div', { class: 'ib-btns' }, el('button', { on: { click: back } }, 'Cancel'),
        el('button', { class: 'danger', disabled: !!busy, on: { click: () => ibDecide(r, { action: 'deny', note: ibNote() }) } }, busy ? 'Sending…' : (r.kind === 'access' ? 'Deny whole request' : 'Dismiss'))));
  }
  const now = new Date();
  const tonight = new Date(now); tonight.setHours(21, 0, 0, 0); if (tonight <= now) tonight.setDate(tonight.getDate() + 1);
  const tomorrow = new Date(now); tomorrow.setDate(tomorrow.getDate() + 1); tomorrow.setHours(9, 30, 0, 0);
  const mins = (d) => Math.max(1, Math.round((d - now) / 60000));
  return el('div', { class: 'ib-actions' },
    el('div', { class: 'meta' }, 'Remind me…'),
    el('div', { class: 'ib-btns' },
      el('button', { on: { click: () => ibDecide(r, { action: 'snooze', snooze_minutes: 60 }) } }, 'In 1 hour'),
      el('button', { on: { click: () => ibDecide(r, { action: 'snooze', snooze_minutes: mins(tonight) }) } }, 'Tonight 21:00'),
      el('button', { on: { click: () => ibDecide(r, { action: 'snooze', snooze_minutes: mins(tomorrow) }) } }, 'Tomorrow 09:30')),
    el('button', { class: 'ib-link', on: { click: back } }, 'Cancel'));
}

// ibTTLEl: how long the permissions last. The owner can shorten it.
function ibTTLEl(r) {
  const req = r.ttl_seconds || 1800;
  const opts = [...new Set([req, 3600, 1800, 900, 300, 120].filter((v) => v <= req))].sort((a, b) => b - a);
  const cur = state.inbox.ttl[r.id] || req;
  const lbl = (v) => (v >= 3600 ? (v / 3600) + ' h' : Math.round(v / 60) + ' min') + (v === req ? ' (asked for)' : '');
  return el('label', { class: 'ib-ttl' }, el('span', {}, 'Permissions last'),
    el('select', { on: { change: (e) => { state.inbox.ttl[r.id] = +e.target.value;const d=ibAccessDraft(r);d.ttl=+e.target.value;state.inbox.ttl[r.id]=d.ttl;ibSaveAccessDraft(r,d); } } },
      ...opts.map((v) => el('option', { value: String(v), selected: v === cur }, lbl(v)))));
}

function ibApproveBody(r, allow) {
  const d=ibAccessDraft(r),verdicts={};
  r.tools.forEach((t,k)=>{const id=ibCallID(t,k);verdicts[id]={verdict:allow[k]?'accepted':'rejected'};if(d.reasons[id])verdicts[id].reason=d.reasons[id];});
  const body={action:'submit',request_revision:d.revision,verdicts,note:d.note};
  const nar = state.inbox.narrow[r.id] || {};
  const params = {};
  for (const k of Object.keys(nar)) if (allow[+k] && Object.keys(nar[k]).length) params[k] = nar[k];
  if (r.execution_mode!=='legacy' && Object.keys(params).length) body.params = params;
  const ttl = state.inbox.ttl[r.id];
  if (r.execution_mode!=='legacy' && ttl && ttl !== (r.ttl_seconds || 1800)) body.ttl_seconds = ttl;
  const fingerprint=JSON.stringify(body);if(d.fingerprint!==fingerprint){d.fingerprint=fingerprint;d.submission=crypto.randomUUID();}body.submission_id=d.submission;ibSaveAccessDraft(r,d);
  return body;
}

async function ibRetryCallback(r, delivery) {
  state.inbox.busy = true;
  try {
    await api('/v1/inbox/' + encodeURIComponent(r.id) + '/callbacks/' + encodeURIComponent(delivery.event_id) + '/retry', { method: 'POST', body: {} });
    toast('Callback retry queued.');
  } catch (e) {
    toast('Callback retry failed: ' + e.message, 'error');
  } finally {
    state.inbox.busy = false;
    await loadInboxDetail(r.id, false);
  }
}

function ibIsRisky(t) { return (t.flags || []).some((f) => f.level === 'red' || f.label === 'Production'); }

function ibApprove(r, allow) {
  const body = ibApproveBody(r, allow);
  const risky = r.tools.filter((t, k) => allow[k] && ibIsRisky(t));
  if (!risky.length) { ibDecide(r, body); return; }
  const hasPasskey = state.inbox.info && state.inbox.info.passkeys > 0;
  const sheet = el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) sheet.remove(); } } },
    el('div', { class: 'modal ib-confirm' },
      el('h3', {}, 'Allow production or flagged changes?'),
      el('p', {}, `You’re allowing ${allow.filter(Boolean).length} tool(s), including:`),
      el('ul', {}, ...risky.map((t) => el('li', {}, el('code', {}, t.tool), ' · ', (t.flags || []).map((f) => f.label).join(', ')))),
      hasPasskey ? el('p', { class: 'meta' }, 'Your passkey (Face ID) confirms exactly this decision.')
        : el('p', { class: 'meta' }, 'Tip: add a passkey in Settings → Inbox & permissions to confirm these with Face ID.'),
      el('div', { class: 'ib-btns' },
        el('button', { on: { click: () => sheet.remove() } }, 'Cancel'),
        el('button', { class: 'primary grow', on: { click: () => { sheet.remove(); hasPasskey ? ibPasskeyDecide(r, body) : ibDecide(r, body); } } },
          hasPasskey ? 'Confirm with passkey' : 'Yes, approve'))));
  document.body.appendChild(sheet);
}

// ---- passkeys (WebAuthn) ---------------------------------------------------

function pkDec(s) {
  s = s.replace(/-/g, '+').replace(/_/g, '/');
  while (s.length % 4) s += '=';
  const b = atob(s), out = new Uint8Array(b.length);
  for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
  return out.buffer;
}
function pkEnc(buf) {
  const b = new Uint8Array(buf); let s = '';
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function pkSupported() { return !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext); }
async function pkGet(options) {
  const p = Object.assign({}, options.publicKey);
  p.challenge = pkDec(p.challenge);
  if (p.allowCredentials) p.allowCredentials = p.allowCredentials.map((c) => Object.assign({}, c, { id: pkDec(c.id) }));
  const c = await navigator.credentials.get({ publicKey: p });
  return {
    id: c.id, rawId: pkEnc(c.rawId), type: c.type,
    response: {
      clientDataJSON: pkEnc(c.response.clientDataJSON), authenticatorData: pkEnc(c.response.authenticatorData),
      signature: pkEnc(c.response.signature), userHandle: c.response.userHandle ? pkEnc(c.response.userHandle) : null,
    },
  };
}
async function pkCreate(options) {
  const p = Object.assign({}, options.publicKey);
  p.challenge = pkDec(p.challenge);
  p.user = Object.assign({}, p.user, { id: pkDec(p.user.id) });
  if (p.excludeCredentials) p.excludeCredentials = p.excludeCredentials.map((c) => Object.assign({}, c, { id: pkDec(c.id) }));
  const c = await navigator.credentials.create({ publicKey: p });
  return {
    id: c.id, rawId: pkEnc(c.rawId), type: c.type,
    response: {
      clientDataJSON: pkEnc(c.response.clientDataJSON), attestationObject: pkEnc(c.response.attestationObject),
      transports: c.response.getTransports ? c.response.getTransports() : [],
    },
  };
}

async function ibPasskeyDecide(r, body) {
  if (!pkSupported()) { toast('This browser can’t use passkeys here. Open toolyard over https (or localhost).', 'error'); return; }
  state.inbox.busy = 'passkey';
  ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
  let assertion;
  try {
    const begin = await api('/v1/inbox/' + r.id + '/passkey', { method: 'POST', body: { decision: body } });
    const response = await pkGet(begin.options);
    assertion = { session_id: begin.session_id, response };
  } catch (e) {
    state.inbox.busy = null;
    ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
    toast(e && e.name === 'NotAllowedError' ? 'Passkey check cancelled. Nothing was approved.' : (e.message || 'Passkey check failed'), 'error');
    return;
  }
  ibDecide(r, Object.assign({}, body, { passkey: assertion }));
}

async function ibDecide(r, body) {
  state.inbox.busy = body.action;
  ibRegion('decide', ibDecideEl(r, state.inbox.detail.grants || []));
  try {
    await api('/v1/inbox/' + r.id + '/decide', { method: 'POST', body });
    if (body.action === 'answer') ibDropDraft(r);
    if (body.action === 'submit') ibDropAccessDraft(r);
    state.inbox.panel = null;
    delete state.inbox.narrow[r.id]; delete state.inbox.ttl[r.id];
    toast({ submit:'Decision saved. The agent receives your accepted and rejected calls.',approve: 'Approved. The agent can continue.', deny: 'Decision saved.', return: 'Sent back for a new plan.',
      answer: 'Answer saved. The agent can retrieve it.', snooze: 'Snoozed.', read: 'Marked as read.' }[body.action] || 'Done');
    state.inbox.busy = null;
    await loadInboxDetail(r.id, false);
    loadInbox();
  } catch (e) {
    state.inbox.busy = null;
    if (e.status === 428 && !body.passkey) {
      // A passkey became necessary (one was registered elsewhere, or the
      // background check flagged a tool): confirm and retry.
      if (state.inbox.info) state.inbox.info.passkeys = Math.max(1, state.inbox.info.passkeys || 0);
      await loadInboxDetail(r.id, false);
      ibPasskeyDecide(r, body);
      return;
    }
    if (e.status === 409) staleNote('The server revision or decision changed. Your draft remains saved. Review the current server state.');
    else toast(e.message, 'error');
    await loadInboxDetail(r.id, false);
  }
}

function ibResultEl(r) {
  const cls = { approved: 'good', answered: 'good', read: 'mute', denied: 'bad', returned: 'bad', cancelled: 'mute', expired: 'mute' }[r.status] || 'mute';
  const title = {
    approved: `Allowed ${(r.tools || []).filter((t) => t.decision === 'allowed').length} of ${(r.tools || []).length}`,
    answered: (r.retrieved_at ? 'Retrieved by agent: ' : 'Answer saved: ') + r.answer, read: 'Marked as read', denied: r.kind === 'access' ? 'Denied' : 'Dismissed',
    returned: 'Sent back to replan', cancelled: 'Withdrawn by the agent', expired: 'Expired without a decision',
  }[r.status] || r.status;
  const detail = r.status === 'approved' && r.grants_expire_at ? `Permissions expire at ${ibClock(r.grants_expire_at)}.` : '';
  return el('div', { class: 'ib-result ' + cls }, el('b', {}, title),
    r.owner_note ? el('span', {}, 'Your note: “' + r.owner_note + '”') : null, detail ? el('span', {}, detail) : null,
    ibDeciderEl(r));
}

// ibDeciderEl: "Decided by <name> via <label>" on a closed request, or null
// while it's open or when the doc recorded nobody (older docs, expiry).
function ibDeciderEl(r) {
  if (!r || r.status === 'pending') return null;
  const d = inboxDecider(r);
  // The status line already says it expired or the agent withdrew it.
  if (!deciderWho(d) && (d.via === 'expiry' || d.via === 'agent_cancel')) return null;
  const text = deciderText(d);
  return text ? el('span', { class: 'ib-decider', title: deciderTitle(d) }, text) : null;
}

// ibGrantBy: "issued by X · revoked by Y" for a grant, '' when unrecorded.
function ibGrantBy(g) {
  if (!g) return '';
  return [g.issued_by ? 'issued by ' + userNameById(g.issued_by) : '',
    g.revoked_by ? 'revoked by ' + userNameById(g.revoked_by) : ''].filter(Boolean).join(' · ');
}

function ibActivityEl(r) {
  return el('div', { class: 'ib-sec' }, el('div', { class: 'ib-eyebrow' }, 'Activity'),
    el('div', { class: 'ib-timeline' }, ...(r.activity || []).map((a) => el('div', { class: 'ib-tl' }, el('i'),
      el('div', {}, el('time', {}, ibClock(a.at)), a.text)))));
}

function ibJump() { const d = document.getElementById('ib-decide'); if (d) d.scrollIntoView({ behavior: 'smooth', block: 'start' }); }

function ibObserveDecision(r) {
  const pill = document.getElementById('ib-jump'), dec = document.getElementById('ib-decide');
  if (!pill || !dec) return;
  if (ibObserver) ibObserver.disconnect();
  if (r.status !== 'pending') { pill.classList.add('hide'); return; }
  pill.textContent = '↓ Jump to decision' + (r.kind === 'access' ? ` · ${r.tools.length} tool${r.tools.length === 1 ? '' : 's'}` : '');
  ibObserver = new IntersectionObserver((es) => es.forEach((e) => pill.classList.toggle('hide', e.isIntersecting)), { rootMargin: '0px 0px -30% 0px' });
  ibObserver.observe(dec);
}

window.addEventListener('scroll', () => {
  const bar = document.getElementById('ib-readp'), dec = document.getElementById('ib-decide');
  if (!bar || !dec || dec.hidden) return;
  const end = dec.getBoundingClientRect().top + window.scrollY - window.innerHeight * 0.6;
  bar.style.width = Math.max(0, Math.min(100, window.scrollY / Math.max(1, end) * 100)) + '%';
}, { passive: true });

// ---- live updates -----------------------------------------------------------

let ibReloadTimer = null;
function handleInboxEvent(card) {
  clearTimeout(ibReloadTimer);
  ibReloadTimer = setTimeout(loadInbox, 250);
  if (card && state.inbox.openId === card.id) loadInboxDetail(card.id, false);
  if (card && card.status === 'pending' && card.kind !== 'update' && !state.inbox.items.some((x) => x.id === card.id) && state.route !== 'inbox') {
    toast(`${card.agent_name} ${IB_VERB[card.kind] || 'sent a request'}`);
  }
}

function handleGrantEvent() {
  if (state.inbox.openId) loadInboxDetail(state.inbox.openId, false);
}

// ---- settings card --------------------------------------------------------

function ibVoiceSettings(s, patch) {
  const info = state.inbox.info || {};
  const provider = s.inbox_voice_provider || 'gemini';
  const eleven = provider === 'elevenlabs';
  const keyInput = el('input', { type: 'password', autocomplete: 'off', placeholder: 'Enter ElevenLabs API key', 'aria-label': 'ElevenLabs API key' });
  const saveKey = async () => {
    const api_key = keyInput.value.trim();
    if (!api_key) return toast('Enter your ElevenLabs API key.', 'error');
    try {
      const out = await api('/v1/inbox/voice-key', { method: 'POST', body: { api_key } });
      keyInput.value = '';
      Object.assign(state.settings, out);
      state.inbox.info = null;
      toast('ElevenLabs key saved'); render();
    } catch (e) { toast(e.message, 'error'); }
  };
  return el('div', { class: 'ib-set' },
    el('h4', {}, 'Voice notes'),
    el('div', { class: 'ib-setrow' }, el('label', { for: 'ib-voice-provider' }, 'Speech provider'),
      el('select', { id: 'ib-voice-provider', on: { change: (e) => patch({ inbox_voice_provider: e.target.value }) } },
        el('option', { value: 'gemini', selected: !eleven }, 'Gemini'),
        el('option', { value: 'elevenlabs', selected: eleven }, 'ElevenLabs'))),
    eleven ? el('div', {},
      el('p', { class: 'meta' }, (s.elevenlabs_api_key_present || info.voice_available ? 'API key configured. Enter a new key to replace it.' : 'Add an ElevenLabs API key to enable recordings.')),
      el('div', { class: 'ib-setrow' }, keyInput, el('button', { on: { click: saveKey } }, 'Save API key')),
      el('div', { class: 'ib-setrow' }, el('label', { for: 'ib-eleven-voice' }, 'Voice ID'),
        el('input', { id: 'ib-eleven-voice', type: 'text', value: s.inbox_elevenlabs_voice_id || '', placeholder: 'George · JBFqnCBsd6RMkjVDRZzb',
          on: { change: (e) => patch({ inbox_elevenlabs_voice_id: e.target.value.trim() }) } })),
      el('div', { class: 'ib-setrow' }, el('label', { for: 'ib-eleven-model' }, 'Speech model'),
        el('input', { id: 'ib-eleven-model', type: 'text', value: s.inbox_elevenlabs_model || '', placeholder: 'eleven_multilingual_v2',
          on: { change: (e) => patch({ inbox_elevenlabs_model: e.target.value.trim() }) } }))) : null,
    el('label', { class: 'ib-check' }, el('input', { type: 'checkbox', checked: !!s.inbox_voice_enabled, disabled: !info.voice_available && !s.inbox_voice_enabled,
      on: { change: (e) => patch({ inbox_voice_enabled: e.target.checked }) } }),
      el('span', {}, 'Generate voice notes only when I click Listen, then reuse the saved recording. Off: your browser reads the script.' +
        (info.voice_available ? '' : (eleven ? ' Needs an ElevenLabs API key.' : ' Needs GEMINI_API_KEY on the gateway.')))),
    el('p', { class: 'meta' }, 'The first Listen sends that voice-note script to ' + (eleven ? 'ElevenLabs using your API credits.' : 'Gemini.') + ' Replays use the saved audio without another generation. Receiving or opening an inbox item does not generate speech. If generation fails, your browser reads the script.'),
    !eleven && s.inbox_voice_enabled ? el('div', { class: 'ib-setrow' }, el('span', {}, 'Voice'),
      el('select', { on: { change: (e) => patch({ inbox_voice_name: e.target.value }) } },
        ...['Kore', 'Puck', 'Charon', 'Aoede', 'Leda', 'Orus', 'Zephyr', 'Fenrir'].map((v) => el('option', { value: v, selected: (s.inbox_voice_name || 'Kore') === v }, v)))) : null,
  );
}

function ibPasskeySettings() {
  const st = state.passkeys || (state.passkeys = { list: null, busy: false });
  if (st.list === null && !st.loading && !st.error) {
    st.loading = true;
    api('/v1/passkeys').then((out) => { st.list = out.passkeys || []; }).catch(e => { st.error = e.message; })
      .finally(() => { st.loading = false; if (state.route === 'settings') render(); });
  }
  const add = async () => {
    if (!pkSupported()) return toast('Passkeys need toolyard on https (or localhost).', 'error');
    st.busy = true; render();
    try {
      const begin = await api('/v1/passkeys/register/begin', { method: 'POST', body: {} });
      const credential = await pkCreate(begin.options);
      const name = /iPhone/.test(navigator.userAgent) ? 'iPhone' : /iPad/.test(navigator.userAgent) ? 'iPad' : /Mac/.test(navigator.userAgent) ? 'Mac' : 'This device';
      await api('/v1/passkeys/register/finish', { method: 'POST', body: { session_id: begin.session_id, name, credential } });
      toast('Passkey added. High-risk approvals now ask for it.');
      st.list = null;
      if (state.inbox.info) state.inbox.info.passkeys = (state.inbox.info.passkeys || 0) + 1;
    } catch (e) {
      toast(e && e.name === 'NotAllowedError' ? 'Cancelled.' : (e.message || 'Couldn’t add a passkey'), 'error');
    }
    st.busy = false; render();
  };
  const remove = async (pk) => {
    st.busy = true; render();
    try {
      const begin = await api('/v1/passkeys/' + encodeURIComponent(pk.id) + '/remove/begin', { method: 'POST', body: {} });
      const response = await pkGet(begin.options);
      await api('/v1/passkeys/' + encodeURIComponent(pk.id) + '/remove', { method: 'POST', body: { session_id: begin.session_id, response } });
      toast('Passkey removed.');
      st.list = null;
      if (state.inbox.info) state.inbox.info.passkeys = Math.max(0, (state.inbox.info.passkeys || 1) - 1);
    } catch (e) {
      toast(e && e.name === 'NotAllowedError' ? 'Cancelled.' : (e.message || 'Couldn’t remove it'), 'error');
    }
    st.busy = false; render();
  };
  return el('div', { class: 'ib-set' },
    el('h4', {}, 'Passkeys'),
    el('p', { class: 'meta' }, 'With a passkey, allowing production or red-flagged tools needs Face ID (or Touch ID, or your security key), bound to exactly what you approve. Removing a passkey needs a passkey too; if you lose every device, restart the gateway once with -inbox-reset-passkeys.'),
    st.error ? el('div', { class: 'settings-notice error' }, 'Passkeys could not load. ', el('button', { on: { click: () => { st.error = ''; st.list = null; render(); } } }, 'Try again')) : st.list === null ? el('p', { class: 'meta' }, 'Loading…') : st.list.length ? el('div', { class: 'ib-pklist' },
      ...st.list.map((pk) => el('div', { class: 'ib-pkrow' },
        el('span', {}, el('b', {}, pk.name), el('small', { class: 'meta' }, ' added ' + relTime(pk.created_at) + (pk.last_used_at ? ' · used ' + relTime(pk.last_used_at) : ''))),
        el('button', { class: 'danger', disabled: st.busy, on: { click: () => remove(pk) } }, 'Remove')))) : el('p', { class: 'meta' }, 'No passkeys yet.'),
    el('button', { class: 'primary', disabled: st.busy || !!st.error || st.list === null, on: { click: add } }, st.busy ? 'Waiting for your passkey…' : 'Add a passkey on this device'),
  );
}

function agentRulesSnippet(base, tok) {
  return `# Claude Code: install the toolyard-inbox skill
mkdir -p ~/.claude/skills/toolyard-inbox
curl -fsSL -H "Authorization: Bearer ${tok}" ${base}/v1/guide/skill \\
  -o ~/.claude/skills/toolyard-inbox/SKILL.md
# (or, with the toolyard CLI: toolyard skills install toolyard-inbox)

# Codex, Cursor, hermes and others: add this to AGENTS.md / your rules
## Asking for permission (toolyard)
Some toolyard tools are restricted and need the owner's approval.
- Before a task, run inbox.check on the calls you plan to make.
- Ask for every restricted tool in ONE inbox.request: a first-person message,
  facts (why_now, if_it_goes_wrong, undo), a voice-note script of at most
  75 words, and evidence attachments. Run it with dry_run: true first.
- Keep working while you wait; use inbox.wait when you run out of work.
- Call each allowed tool with _grant set to its token, within the
  parameters you asked for. Report back with inbox.post.
- If a call returns permission_required, nothing ran: fill in its draft.
Read the full rules with inbox.guide().`;
}

(async () => {
  if ('serviceWorker' in navigator) {
    try { navigator.serviceWorker.register('/sw.js'); } catch {}
  }
  if (location.hash) {
    state.route = workspaceRoute(location.hash.slice(1) || 'inbox');
    if (state.route !== location.hash.slice(1)) history.replaceState(null, '', '#' + state.route);
  }
  // Retired extension notification links follow the same route redirects.
  const routeParam = new URLSearchParams(location.search).get('route');
  if (routeParam) state.route = workspaceRoute(routeParam);
  let deepInbox = null;
  if (state.route.startsWith('settings/')) state.route = settingsRoute(state.route);
  if (state.route.startsWith('inbox/')) { deepInbox = state.route.slice(6); state.route = 'inbox'; }
  if (IB_FILTERS.includes(deepInbox)) { state.inbox.filter = deepInbox; deepInbox = null; }
  // Push deep link: notifications open /?approval=<id>. Land on the
  // approvals view so the card (or its expired/decided state) is visible.
  const approvalParam = new URLSearchParams(location.search).get('approval') || new URLSearchParams(location.search).get('approval_id');
  if (approvalParam) {
    state.route = 'inbox';deepInbox=approvalParam;history.replaceState(null,'','#inbox/'+encodeURIComponent(approvalParam));
  }
  // /?password=1 (from /login's "Use password instead") opens the password
  // form straight away instead of behind the Google button.
  if (new URLSearchParams(location.search).get('password') === '1') state.showPasswordLogin = true;
  await Promise.all([refreshUser(), loadAuthConfig()]);
  if (state.user && !isAdmin()) {
    // Members: their agents and granted servers only. No event stream and
    // no admin fetches (they'd all be 403 admin_only).
    await loadAll();
    if (state.route === 'connections') loadConnections(true);
    if(deepInbox)openInboxRequest(deepInbox);
  } else if (state.user) {
    await loadAll(); startStream();
    if (state.route === 'users') loadUsers(true);
    if (state.route === 'myservers') loadMyServers();
    if (state.route === 'connections') loadConnections(true);
    if (deepInbox) openInboxRequest(deepInbox);
    if (state.route === 'insights' || state.route === 'policies' || state.route === 'notifications') {
      loadInsights();
    } else {
      // Fetch anomaly count for the navbar badge in the background.
      api('/v1/insights/anomalies?limit=50').then((a) => { state.anomalies = a || []; render(); }).catch(() => {});
    }
  }
  render();

  // Push deep link: focus (and scroll to) the targeted approval card.
  if (state.focusApprovalId) {
    const id = state.focusApprovalId;
    state.focusApprovalId = null;
    setTimeout(() => {
      let card = null;
      try { card = document.querySelector(`[data-approval-id="${CSS.escape(id)}"]`); } catch (_) {}
      if (card) { card.scrollIntoView({ block: 'center' }); card.focus(); }
    }, 0);
  }

  // Refresh owned Inbox state, including members without the admin event stream.
  // Detail updates retain local decision drafts and defer while a field has focus.
  setInterval(() => {
    if (state.user && state.route === 'inbox') {
      loadInbox();
      if (state.inbox.openId) loadInboxDetail(state.inbox.openId, false);
    }
  }, 30000);

  // Deep links while the app is already open (a push tap focuses the open
  // PWA and changes only the hash, which doesn't reload the page).
  window.addEventListener('hashchange', () => {
    if (!state.user) return;
    const raw = location.hash.slice(1), h = workspaceRoute(raw);
    if (h !== raw) { navigate(h); return; }
    if (!routeAllowed(h.split('/')[0])) { navigate(defaultRoute()); return; }
    if (h.startsWith('inbox/')) {
      const id = h.slice(6);
      if (IB_FILTERS.includes(id)) {
        if (state.inbox.openId) closeInboxRequest();
        state.inbox.filter = id; state.inbox.tab = 'inbox'; navigate('inbox');
      } else if (id && state.inbox.openId !== id) openInboxRequest(id);
    } else if (h === 'inbox' && state.inbox.openId) {
      closeInboxRequest();
    } else if (h && h !== state.route) {
      navigate(h);
    }
  });

  // iOS-PWA resume path: WebKit kills the SSE socket on background without
  // firing onerror, so on re-show we proactively reconnect if the stream
  // isn't demonstrably live.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible' || !state.user) return;
    const stale = !state.stream.lastEventAt || Date.now() - state.stream.lastEventAt > 30000;
    if (state.stream.status !== 'live' || stale) reconnectStream();
  });
})();
