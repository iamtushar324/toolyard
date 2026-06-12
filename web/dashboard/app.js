// toolyard dashboard — vanilla SPA. No build step.
//
// Single mutable `state` object. `render()` redraws the screen for the current
// route. SSE feed reconciles approvals + audit live.

const state = {
  user: null,
  setupRequired: false,
  route: 'approvals',
  approvals: [],
  audit: [],
  agents: [],
  memory: [],
  servers: [],
  tools: [],
  marketplace: [],
  marketModal: null,
  agentModal: null,        // { stage: 'name'|'done', name, agent, snippetTab }
  toolFilter: '',
  // Audit tab: client-side filters + pagination cursor state.
  auditFilter: { q: '', event_type: '', decision: '', agent: '' },
  auditPaged: false,      // true once "Load older" has pulled extra rows
  auditEnd: false,        // true when a "Load older" returned nothing
  hooks: { events: [], loading: false, end: false, loaded: false },
  hookFilter: { q: '', source: '', event_name: '', agent: '', session_id: '' },
  // Memory tab: filter text + which row is being inline-edited.
  memoryFilter: '',
  memEdit: null,          // { scope, key, value } while editing a row
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
    cost: { rows: [], input_usd_per_m: 0, output_usd_per_m: 0 },
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
  // Events Hub: feed rows + source configs + filters + unacked badge.
  events: { rows: [], unacked: 0, sources: [], loaded: false, nextBefore: 0 },
  eventFilter: { source_id: '', type: '', q: '', unacked: false },
  eventSourceModal: null,  // { kind, name, ... } while adding a source
  // Personal data lake (TUS-104). Manifest + per-panel cached results so
  // a tab switch is instant after first visit. Errors per panel are kept
  // local so one bad query doesn't break the rest of the page.
  lake: {
    manifest: null,        // { tabs: [...] } from /v1/lake/manifest
    activeTab: null,       // tab id
    panelData: {},         // "<tab>/<panelId>" -> result object
    panelErrors: {},       // "<tab>/<panelId>" -> error string
    chartInstances: {},    // "<tab>/<panelId>" -> echarts instance (for resize)
    loading: false,
    error: null,
    explorerSQL: '',       // last SQL the user typed in the explorer tab
    explorerResult: null,
    explorerError: null,
    echartsLoaded: false,
  },
  // Voice "live call" panel. The actual WS, AudioContext, MediaStream
  // live in module-scope handles (see voiceClient below) — they aren't
  // serialisable and must survive re-renders, so they can't sit in this
  // state object. This block is the renderable mirror.
  call: {
    active: false,
    callId: null,
    phase: 'idle',    // 'idle' | 'connecting' | 'listening' | 'thinking' | 'speaking' | 'error'
    error: '',
    mute: false,
    transcript: [],   // [{ role: 'user'|'assistant', text }]
    toolCalls: [],    // [{ name, args }]
    permission: 'unknown', // 'unknown' | 'granted' | 'denied'
    activeCallElsewhere: null, // {call_id} when server returns 409
    // hardwareReady reflects whether we've successfully claimed the OS
    // Now Playing slot. If false, BTR11 play/pause falls through to
    // Siri / system handling instead of toggling mute.
    hardwareReady: false,
    hardwareWhy: '',
    // Music ducking via Path A: a low-level pink-noise loop in our
    // audio output trips the OS audio session, so cooperating apps
    // (Spotify, Music with auto-pause prefs on) voluntarily pause.
    // Persisted in localStorage; default on. No server involvement.
    duck: (localStorage.getItem('toolyard.call.duck') ?? '1') === '1',
  },
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

// ---- bootstrap -------------------------------------------------------------

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
  try {
    const [pendings, audits, agents, memos, servers, tools, market, settingsRes, usageRes, vapid, policies] = await Promise.all([
      api('/v1/approvals?status=pending'),
      api('/v1/audit?limit=50'),
      api('/v1/agents'),
      api('/v1/memory'),
      api('/v1/servers').catch(() => []),
      api('/v1/tools').catch(() => []),
      api('/v1/marketplace').catch(() => null),
      api('/v1/settings').catch(() => ({})),
      api('/v1/usage').catch(() => ({ per_tool: {}, rows: [] })),
      api('/v1/push/vapid_key').catch(() => null),
      api('/v1/policies').catch(() => []),
    ]);
    state.policies = policies || [];
    state.settings = Object.assign({}, state.settings, settingsRes || {});
    state.usage = usageRes || state.usage;
    state.approvals = pendings || [];
    state.audit = audits || [];
    state.agents = agents || [];
    state.memory = memos || [];
    state.servers = servers || [];
    state.tools = tools || [];
    // /v1/marketplace returns { entries, oauth_redirect_uri }; tolerate the
    // legacy bare-array shape so a stale dashboard doesn't blank the tab.
    state.marketplace = Array.isArray(market) ? market : ((market && market.entries) || []);
    state.oauthRedirectURI = (market && market.oauth_redirect_uri) || '';
    state.vapidKey = vapid && vapid.public_key ? vapid.public_key : null;
    // Fire-and-forget — the badges fill in once the responses land.
    preloadOAuthStatus().then(() => render()).catch(() => {});
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
  evtSrc.addEventListener('event', (e) => { markStreamEvent(); handleHubEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_done', (e) => { markStreamEvent(); handleOAuthDone(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_refreshed', () => { markStreamEvent(); reloadServers(); render(); });
  evtSrc.addEventListener('mcp_oauth_needs_reauth', (e) => { markStreamEvent(); handleOAuthReauth(JSON.parse(e.data)); });

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
      api('/v1/audit?limit=50'),
    ]);
    state.approvals = pendings || [];
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
  }
  render();
}

// handleHubEvent receives a live Events Hub event. When the operator is on the
// Events route we prepend it; otherwise we just bump the unacked badge so the
// nav label reflects the new arrival without a full reload.
function handleHubEvent(ev) {
  state.events.unacked = (state.events.unacked || 0) + 1;
  if (state.route === 'events') {
    state.events.rows.unshift(ev);
    if (state.events.rows.length > 300) state.events.rows.length = 300;
  }
  render();
}

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
    const [overview, tools, agents, cost, rules, anomalies, policies] = await Promise.all([
      api('/v1/insights/overview?range=' + range).catch(() => null),
      api('/v1/insights/tools?range=' + range).catch(() => []),
      api('/v1/insights/agents?range=' + range).catch(() => []),
      api('/v1/insights/cost?range=' + range).catch(() => ({ rows: [] })),
      api('/v1/insights/auto/rules').catch(() => []),
      api('/v1/insights/anomalies?limit=50').catch(() => []),
      api('/v1/policies').catch(() => []),
    ]);
    state.insights.overview = overview;
    state.insights.tools = tools || [];
    state.insights.agents = agents || [];
    state.insights.cost = cost || { rows: [] };
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

function viewLogin() {
  return el('div', { class: 'login-wrap' },
    el('div', { class: 'card' },
      el('h2', {}, 'toolyard'),
      el('label', {}, 'Username'),
      el('input', { id: 'lg-user', autocomplete: 'username' }),
      el('label', {}, 'Password'),
      el('input', { id: 'lg-pass', type: 'password', autocomplete: 'current-password' }),
      el('div', { class: 'err' }, state.errors.login || ''),
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', {
          class: 'primary',
          on: { click: async () => {
            state.errors.login = '';
            try {
              await api('/v1/auth/login', { method: 'POST', body: {
                Username: $('lg-user').value.trim(),
                Password: $('lg-pass').value,
              }});
              await refreshUser(); await loadAll(); startStream();
              navigate('approvals');
            } catch (e) { state.errors.login = e.message; render(); }
          }},
        }, 'Sign in'),
      ),
    ),
  );
}

function viewApprovals() {
  if (!state.approvals.length) {
    return el('div', { class: 'card empty' }, 'No pending approvals.');
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
  }));
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
  const labelAgent = agentKey === '__anon__' ? 'anonymous (no agent token)' : agentKey;

  // Compact summary line: "fs.write_file · github.create_issue · …"
  const toolSummary = items.map((a) => `${a.upstream_name}·${a.tool_name}`).join('  ·  ');

  const header = el('div', { class: 'row', style: 'margin-bottom: 8px;' },
    el('div', { class: 'grow' },
      el('div', { style: 'font-weight: 600; font-size: 14px;' },
        isBatch ? `Batch: ${items.length} pending writes` : `${items[0].upstream_name} · ${items[0].tool_name}`),
      el('div', { class: 'meta', style: 'margin-top: 2px;' },
        `agent: `, el('code', {}, labelAgent),
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
    await api(`/v1/approvals/${id}/decide`, { method: 'POST', body: { Action: action } });
    state.approvals = state.approvals.filter((a) => a.id !== id);
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
    if (!q) return true;
    return [e.tool_name, e.upstream_name, e.reason, e.agent_id, e.result_summary, e.event_type]
      .some((v) => (v || '').toLowerCase().includes(q));
  });
}

function uniqueSorted(vals) {
  return Array.from(new Set(vals.filter(Boolean))).sort();
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
  const p = new URLSearchParams({ format });
  if (f.event_type) p.set('event_type', f.event_type);
  if (f.decision) p.set('decision', f.decision);
  if (f.agent) p.set('agent_id', f.agent);
  return '/v1/audit/export?' + p.toString();
}

function hookQueryParams(extra = {}) {
  const f = state.hookFilter || {};
  const p = new URLSearchParams(extra);
  if (f.agent) p.set('agent_id', f.agent);
  if (f.source) p.set('source', f.source);
  if (f.event_name) p.set('event_name', f.event_name);
  if (f.session_id) p.set('session_id', f.session_id);
  if (f.q) p.set('q', f.q);
  return p;
}

async function loadHooks(reset = false) {
  if (state.hooks.loading) return;
  state.hooks.loading = true;
  if (reset) {
    state.hooks.events = [];
    state.hooks.end = false;
  }
  render();
  try {
    const p = hookQueryParams({ limit: '100' });
    if (!reset && state.hooks.events.length) {
      p.set('before', String(state.hooks.events[state.hooks.events.length - 1].ts));
    }
    const rows = await api('/v1/hooks/events?' + p.toString());
    if (!rows || rows.length === 0) {
      state.hooks.end = true;
    } else if (reset) {
      state.hooks.events = rows;
    } else {
      const seen = new Set(state.hooks.events.map((e) => e.id));
      state.hooks.events = state.hooks.events.concat(rows.filter((e) => !seen.has(e.id)));
    }
    state.hooks.loaded = true;
  } catch (e) { toast(e.message, 'error'); }
  state.hooks.loading = false;
  render();
}

function hooksExportURL(format) {
  return '/v1/hooks/export?' + hookQueryParams({ format }).toString();
}

function exportAuditCSV() {
  const rows = filteredAudit();
  const header = ['when_iso', 'event_type', 'upstream', 'tool', 'decision', 'agent_id', 'reason', 'result_summary'];
  const lines = [header.join(',')];
  for (const e of rows) {
    lines.push([
      new Date(e.ts).toISOString(), e.event_type, e.upstream_name, e.tool_name,
      e.decision, e.agent_id, e.reason, e.result_summary,
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
    const older = await api(`/v1/audit?before=${oldest}&limit=100`);
    if (!older || !older.length) { state.auditEnd = true; render(); return; }
    const seen = new Set(state.audit.map((a) => a.id));
    state.audit = state.audit.concat(older.filter((a) => !seen.has(a.id)));
    state.auditPaged = true;
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function viewAudit() {
  if (!state.audit.length) {
    return el('div', { class: 'card empty' }, 'No events yet.');
  }
  const f = state.auditFilter;
  const setF = (k) => (ev) => { f[k] = ev.target.value; render(); };
  const eventTypes = uniqueSorted(state.audit.map((e) => e.event_type));
  const decisions = uniqueSorted(state.audit.map((e) => e.decision));
  const agents = uniqueSorted(state.audit.map((e) => e.agent_id));

  const opt = (v, label) => el('option', { value: v, selected: false }, label);
  const sel = (key, all, vals) => {
    const s = el('select', { on: { change: setF(key) } }, opt('', all), ...vals.map((v) => opt(v, v)));
    s.value = f[key] || '';
    return s;
  };

  const filterBar = el('div', { class: 'row audit-filters', style: 'gap: 8px; margin-bottom: 12px; flex-wrap: wrap;' },
    (() => { const i = el('input', { id: 'audit-q', type: 'search', placeholder: 'Filter tool / reason / agent / result…', class: 'grow', on: { input: setF('q') } }); i.value = f.q || ''; return i; })(),
    sel('event_type', 'All events', eventTypes),
    sel('decision', 'All decisions', decisions),
    sel('agent', 'All agents', agents),
    el('button', { on: { click: exportAuditCSV }, title: 'Export the rows currently shown (includes the text filter)' }, 'Export shown'),
    el('a', { href: auditExportURL('csv'), target: '_blank', title: 'Server-side export of the full log honoring the dropdown filters' }, 'Full CSV'),
    el('a', { href: auditExportURL('json'), target: '_blank' }, 'Full JSON'),
  );

  const rows = filteredAudit();
  const tbl = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', {}, 'When'),
      el('th', {}, 'Event'),
      el('th', {}, 'Tool'),
      el('th', {}, 'Agent'),
      el('th', {}, 'Decision'),
      el('th', {}, 'Reasoning / Result'),
    )),
    el('tbody', {}, rows.map((e) => el('tr', {},
      el('td', { class: 'meta' }, relTime(e.ts)),
      el('td', {}, e.event_type),
      el('td', {}, e.tool_name ? `${e.upstream_name || ''} · ${e.tool_name}` : '—'),
      el('td', { class: 'meta' }, e.agent_id || '—'),
      el('td', {}, e.decision || '—'),
      el('td', {}, e.reason ? el('div', {}, el('div', {}, e.reason),
        e.result_summary ? el('div', { class: 'meta', style: 'margin-top: 4px;' }, e.result_summary) : null)
        : (e.result_summary || '—')),
    ))),
  );

  const footer = el('div', { class: 'row', style: 'margin-top: 12px; align-items: center; gap: 12px;' },
    el('span', { class: 'meta' }, `${rows.length} of ${state.audit.length} loaded`),
    state.auditEnd
      ? el('span', { class: 'meta' }, 'No older events.')
      : el('button', { on: { click: loadOlderAudit } }, 'Load older'),
  );

  return el('div', { class: 'card' }, filterBar, tbl, footer);
}

function viewHooks() {
  const rows = state.hooks.events || [];
  const f = state.hookFilter;
  const setF = (k) => (ev) => {
    f[k] = ev.target.value;
    state.hooks.loaded = false;
    loadHooks(true);
  };
  const opt = (v, label) => el('option', { value: v }, label);
  const sel = (key, all, vals) => {
    const s = el('select', { on: { change: setF(key) } }, opt('', all), ...vals.map((v) => opt(v, v)));
    s.value = f[key] || '';
    return s;
  };
  const agents = uniqueSorted([...(state.agents || []).map((a) => a.id), ...rows.map((e) => e.agent_id)]);
  const sources = uniqueSorted(rows.map((e) => e.source).concat(['claude_code', 'codex', 'cursor', 'generic']));
  const events = uniqueSorted(rows.map((e) => e.event_name));
  const sessions = uniqueSorted(rows.map((e) => e.session_id)).slice(0, 100);
  const counts = rows.reduce((m, e) => {
    if (e.agent_id) m[e.agent_id] = (m[e.agent_id] || 0) + 1;
    return m;
  }, {});
  const countLine = Object.entries(counts).slice(0, 4).map(([agent, n]) => `${agent.slice(0, 10)}: ${n}`).join(' · ');

  const filterBar = el('div', { class: 'row audit-filters', style: 'gap: 8px; margin-bottom: 12px; flex-wrap: wrap;' },
    (() => { const i = el('input', { id: 'hooks-q', type: 'search', placeholder: 'Search text / tool / event / session…', class: 'grow', on: { change: setF('q'), keydown: (e) => { if (e.key === 'Enter') setF('q')(e); } } }); i.value = f.q || ''; return i; })(),
    sel('source', 'All sources', sources),
    sel('event_name', 'All events', events),
    sel('agent', 'All agents', agents),
    sel('session_id', 'All sessions', sessions),
    el('button', { on: { click: () => loadHooks(true) }, disabled: state.hooks.loading }, state.hooks.loading ? 'Loading…' : 'Refresh'),
    el('a', { href: hooksExportURL('csv'), target: '_blank' }, 'CSV'),
    el('a', { href: hooksExportURL('json'), target: '_blank' }, 'JSON'),
  );

  const recipes = el('div', { class: 'card' },
    el('h2', {}, 'Hook recipes'),
    el('p', { class: 'meta' },
      'Use the same ingest endpoint for interaction logging, memory ingest, approval resume, tool-call auditing, notifications, guardrails, and turn summaries.'),
    el('div', { class: 'meta' }, countLine || 'No per-agent hook counts loaded yet.'),
  );

  const table = rows.length === 0
    ? el('div', { class: 'card empty' }, state.hooks.loading ? 'Loading hook events…' : 'No hook events match these filters yet.')
    : el('div', { class: 'card' },
        filterBar,
        el('table', {},
          el('thead', {}, el('tr', {},
            el('th', {}, 'When'),
            el('th', {}, 'Source'),
            el('th', {}, 'Event'),
            el('th', {}, 'Agent'),
            el('th', {}, 'Session'),
            el('th', {}, 'Text / Tool'),
          )),
          el('tbody', {}, rows.map((e) => el('tr', {},
            el('td', { class: 'meta' }, relTime(e.ts)),
            el('td', {}, e.source || 'generic'),
            el('td', {}, e.event_name || 'unknown'),
            el('td', { class: 'meta' }, e.agent_id || '—'),
            el('td', { class: 'meta' }, e.session_id ? el('code', {}, e.session_id.slice(0, 18)) : '—'),
            el('td', {}, [
              e.tool_name ? el('div', {}, el('code', {}, e.tool_name)) : null,
              e.text ? el('div', { class: 'meta', style: 'white-space: pre-wrap; margin-top: 4px;' }, e.text.slice(0, 500)) : null,
              e.memory_ingested ? el('div', { class: 'badge allowed', style: 'margin-top: 4px;' }, 'memory') : null,
              e.payload ? el('details', { style: 'margin-top: 6px;' },
                el('summary', {}, 'payload'),
                el('pre', { class: 'mem-value', style: 'max-height: 220px; overflow: auto; white-space: pre-wrap;' },
                  JSON.stringify(e.payload, null, 2).slice(0, 4000)),
              ) : null,
            ]),
          ))),
        ),
        el('div', { class: 'row', style: 'margin-top: 12px; align-items: center; gap: 12px;' },
          el('span', { class: 'meta' }, `${rows.length} loaded`),
          state.hooks.end
            ? el('span', { class: 'meta' }, 'No older events.')
            : el('button', { on: { click: () => loadHooks(false) }, disabled: state.hooks.loading }, state.hooks.loading ? 'Loading…' : 'Load older'),
        ),
      );

  return el('div', {}, rows.length === 0 ? el('div', { class: 'card' }, filterBar) : null, table, recipes);
}

function viewAgents() {
  return el('div', {},
    state.agentModal ? renderAgentModal() : null,
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
            el('tbody', {}, state.agents.map((a) => el('tr', { style: a.disabled ? 'opacity: 0.6;' : '' },
              el('td', {}, a.name),
              el('td', {}, el('code', {}, a.id)),
              el('td', { class: 'meta' }, a.last_seen ? relTime(a.last_seen) : 'never'),
              el('td', {}, a.disabled
                ? el('span', { class: 'badge denied' }, 'disabled')
                : el('span', { class: 'badge allowed' }, 'active')),
              el('td', {}, el('div', { class: 'row' },
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
    { id: 'hook-claude', label: 'Claude hooks' },
    { id: 'hook-codex',  label: 'Codex hooks' },
    { id: 'hook-cursor', label: 'Cursor hooks' },
    { id: 'hook-conductor', label: 'Conductor' },
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

  const hookURL = baseUrlNoMcp + '/v1/hooks/ingest?source=claude_code';
  const claudeHooks = JSON.stringify({
    hooks: {
      UserPromptSubmit: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
      PostToolUse: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
      PostToolUseFailure: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
      Stop: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
      SubagentStart: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
      SubagentStop: [{ hooks: [{ type: 'http', url: hookURL, headers: { Authorization: `Bearer ${tok}` } }] }],
    },
  }, null, 2);

  const forwarderInstall =
`mkdir -p ~/.toolyard/hooks
cp scripts/toolyard-hook-forwarder.sh ~/.toolyard/hooks/
chmod +x ~/.toolyard/hooks/toolyard-hook-forwarder.sh`;

  const codexHooks = forwarderInstall + '\n\n# ~/.codex/hooks.json\n' + JSON.stringify({
    hooks: {
      UserPromptSubmit: [{ hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
      PreToolUse: [{ matcher: '*', hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
      PostToolUse: [{ matcher: '*', hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
      Stop: [{ hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
      SubagentStart: [{ matcher: '*', hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
      SubagentStop: [{ matcher: '*', hooks: [{ type: 'command', command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh codex ${baseUrlNoMcp} ${tok}` }] }],
    },
  }, null, 2);

  const cursorHooks = forwarderInstall + '\n\n# ~/.cursor/hooks.json or <project>/.cursor/hooks.json\n' + JSON.stringify({
    version: 1,
    hooks: {
      beforeSubmitPrompt: [{ command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor ${baseUrlNoMcp} ${tok}` }],
      beforeMCPExecution: [{ command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor ${baseUrlNoMcp} ${tok}` }],
      afterMCPExecution: [{ command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor ${baseUrlNoMcp} ${tok}` }],
      afterFileEdit: [{ command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor ${baseUrlNoMcp} ${tok}` }],
      stop: [{ command: `~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor ${baseUrlNoMcp} ${tok}` }],
    },
  }, null, 2);

  const conductorHooks =
`Conductor runs Claude Code or Codex inside each workspace.

For a Conductor Claude Code workspace:
1. Use the "Claude hooks" snippet in the workspace or user Claude settings.
2. Keep the MCP snippet above for Toolyard tools and approvals.

For a Conductor Codex workspace:
1. Install scripts/toolyard-hook-forwarder.sh with the command shown in the Codex tab.
2. Use the "Codex hooks" hooks.json shape in the workspace .codex/ layer or user ~/.codex/hooks.json.

There is no separate Conductor hook endpoint for v1; the selected agent client emits the lifecycle events.`;

  const snippet = m.snippetTab === 'cli' ? cli :
                  m.snippetTab === 'project' ? project :
                  m.snippetTab === 'global' ? global :
                  m.snippetTab === 'hook-claude' ? claudeHooks :
                  m.snippetTab === 'hook-codex' ? codexHooks :
                  m.snippetTab === 'hook-cursor' ? cursorHooks : conductorHooks;
  const snippetLang = (m.snippetTab === 'cli' || m.snippetTab === 'hook-codex' || m.snippetTab === 'hook-cursor') ? 'bash' :
                      m.snippetTab === 'hook-conductor' ? 'text' : 'json';

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
                : m.snippetTab === 'hook-claude'
                  ? 'Merge into Claude Code settings.json'
                  : m.snippetTab === 'hook-codex'
                    ? 'Install the forwarder, then merge into Codex hooks.json'
                    : m.snippetTab === 'hook-cursor'
                      ? 'Install the forwarder, then merge into Cursor hooks.json'
                      : 'Use the hook snippet for the agent type Conductor runs'),
          el('button', { class: 'copy-btn', on: { click: (e) => copyToButton(e.target, snippet) }}, 'Copy'),
        ),
        el('pre', { 'data-lang': snippetLang }, snippet),
      ),
      m.snippetTab === 'cli' ? el('div', { class: 'meta', style: 'margin-top: 8px;' },
        'After running, restart your Claude Code session and run ', el('code', {}, '/mcp'),
        '. You should see ', el('code', {}, 'toolyard ✔ connected'), ' with ',
        el('code', {}, String(state.tools.length || '...')), ' tools.',
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

function viewMemory() {
  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Set value'),
      el('div', { class: 'row' },
        el('input', { id: 'mem-scope', placeholder: 'scope (default: global)' }),
        el('input', { id: 'mem-key', placeholder: 'key', class: 'grow' }),
      ),
      el('textarea', { id: 'mem-val', placeholder: 'value', style: 'margin-top: 8px;' }),
      el('div', { class: 'row', style: 'margin-top: 8px;' },
        el('button', { class: 'primary', on: { click: async () => {
          try {
            await api('/v1/memory', { method: 'POST', body: {
              Scope: $('mem-scope').value, Key: $('mem-key').value, Value: $('mem-val').value }});
            await loadAll(); render();
          } catch (e) { toast(e.message, 'error'); }
        }}}, 'Save'),
      ),
    ),
    el('div', { class: 'card' },
      el('div', { class: 'row', style: 'align-items: center;' },
        el('h2', { class: 'grow', style: 'margin: 0;' }, 'Entries'),
        (() => {
          const i = el('input', { id: 'mem-filter', type: 'search', placeholder: 'Filter memories…', on: { input: (e) => { state.memoryFilter = e.target.value; render(); } } });
          i.value = state.memoryFilter || '';
          return i;
        })(),
      ),
      renderMemoryEntries(),
    ),
  );
}

function renderMemoryEntries() {
  const q = (state.memoryFilter || '').toLowerCase();
  const rows = (state.memory || []).filter((m) => !q ||
    [m.scope, m.key, m.value].some((v) => (v || '').toLowerCase().includes(q)));
  if (!rows.length) {
    return el('div', { class: 'empty', style: 'margin-top: 12px;' },
      state.memory.length ? 'No memories match your filter.' : 'No memories yet.');
  }
  return el('table', { style: 'margin-top: 12px;' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Scope'), el('th', {}, 'Key'), el('th', {}, 'Value'), el('th', {}, 'Updated'), el('th', {}, ''))),
    el('tbody', {}, rows.map((m) => {
      const editing = state.memEdit && state.memEdit.scope === m.scope && state.memEdit.key === m.key;
      const valueCell = editing
        ? (() => {
            const ta = el('textarea', {
              style: 'width: 100%; min-height: 60px;',
              on: { keydown: (e) => {
                if (e.key === 'Escape') { state.memEdit = null; render(); }
                else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { saveMemoryEdit(m, ta.value); }
              } },
            }, state.memEdit.value);
            return el('div', {}, ta,
              el('div', { class: 'row', style: 'margin-top: 6px;' },
                el('button', { class: 'primary', on: { click: () => saveMemoryEdit(m, ta.value) } }, 'Save'),
                el('button', { on: { click: () => { state.memEdit = null; render(); } } }, 'Cancel'),
                el('span', { class: 'meta' }, '⌘/Ctrl+Enter saves · Esc cancels'),
              ),
            );
          })()
        : el('div', { class: 'mem-value', title: 'Tap to edit',
            on: { click: () => { state.memEdit = { scope: m.scope, key: m.key, value: m.value || '' }; render(); } } },
            m.value || el('span', { class: 'meta' }, '(empty)'));
      return el('tr', {},
        el('td', {}, m.scope),
        el('td', {}, el('code', {}, m.key)),
        el('td', {}, valueCell),
        el('td', { class: 'meta' }, relTime(m.updated_at)),
        el('td', {}, el('button', { class: 'danger', on: { click: async () => {
          try {
            await api(`/v1/memory?scope=${encodeURIComponent(m.scope)}&key=${encodeURIComponent(m.key)}`, { method: 'DELETE' });
            await loadAll(); render();
          } catch (e) { toast(e.message, 'error'); }
        }}}, 'Delete')),
      );
    })),
  );
}

async function saveMemoryEdit(m, value) {
  try {
    await api('/v1/memory', { method: 'POST', body: { Scope: m.scope, Key: m.key, Value: value } });
    state.memEdit = null;
    await loadAll();
    render();
    toast('Saved');
  } catch (e) { toast(e.message, 'error'); }
}

function viewServers() {
  const transport = (state._serverDraft && state._serverDraft.transport) || 'stdio';
  const draft = state._serverDraft || (state._serverDraft = { transport: 'stdio' });

  const setDraft = (k, v) => { draft[k] = v; render(); };

  const transportRow = el('div', { class: 'row' },
    el('label', { style: 'flex: 1;' },
      el('div', { class: 'meta' }, 'Transport'),
      el('select', {
        on: { change: (e) => setDraft('transport', e.target.value) }
      },
        el('option', { value: 'stdio',           selected: transport === 'stdio'           }, 'stdio (subprocess)'),
        el('option', { value: 'http',            selected: transport === 'http'            }, 'streamable HTTP'),
      ),
    ),
    el('label', { style: 'flex: 1;' },
      el('div', { class: 'meta' }, 'Server name'),
      el('input', { id: 'srv-name', placeholder: 'e.g. github', value: draft.name || '' }),
    ),
  );

  const transportFields = transport === 'stdio'
    ? el('div', {},
        el('div', { class: 'row' },
          el('label', { style: 'flex: 1;' },
            el('div', { class: 'meta' }, 'Command'),
            el('input', { id: 'srv-cmd', placeholder: 'e.g. uvx', value: draft.command || '' }),
          ),
          el('label', { style: 'flex: 2;' },
            el('div', { class: 'meta' }, 'Args (one per line)'),
            el('textarea', { id: 'srv-args', placeholder: 'mcp-server-github', value: (draft.args || []).join('\n') }),
          ),
        ),
      )
    : el('label', {},
        el('div', { class: 'meta' }, 'URL'),
        el('input', { id: 'srv-url', placeholder: 'https://example.com/mcp', value: draft.url || '' }),
      );

  const envRow = el('label', {},
    el('div', { class: 'meta' }, 'Environment (KEY=VALUE per line, optional). Use ', el('code', {}, 'KEY=secret://NAME'), ' to reference a stored secret.'),
    el('textarea', { id: 'srv-env', placeholder: 'GITHUB_PERSONAL_ACCESS_TOKEN=secret://GITHUB_TOKEN', value: draftEnvAsText(draft) }),
  );

  const headersRow = transport !== 'stdio' ? el('label', {},
    el('div', { class: 'meta' }, 'HTTP headers (Header: value per line, optional). Values may be ', el('code', {}, 'secret://NAME'), '.'),
    el('textarea', { id: 'srv-headers', placeholder: 'X-Api-Key: secret://MY_API_KEY', value: '' }),
  ) : null;

  const installedByName = new Map(state.servers.map((s) => [s.name, s]));

  return el('div', {},
    state.marketModal ? renderMarketModal() : null,
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
      el('h2', {}, 'Add custom MCP server'),
      el('p', { class: 'meta' },
        'Connect an upstream MCP server. Its tools are wrapped with the required _reason field, fed through the policy engine, and surface in the catalog as ',
        el('code', {}, '<server>.<tool-name>'), '.'),
      transportRow,
      transportFields,
      envRow,
      headersRow,
      el('div', { class: 'row', style: 'margin-top: 12px;' },
        el('button', { class: 'primary', on: { click: () => addServer() }}, 'Add server'),
        el('button', { on: { click: () => { state._serverDraft = { transport: 'stdio' }; render(); } } }, 'Reset'),
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
            el('th', {}, 'Policy'),
            el('th', {}, ''))),
            el('tbody', {}, state.servers.map((s) => el('tr', {},
              el('td', {}, el('code', {}, s.name),
                serverUsesSecret(s) ? el('span', { title: 'references a stored secret', style: 'margin-left:6px;' }, '🔒') : null),
              el('td', {}, transportLabel(s)),
              el('td', {}, String(s.tool_count || 0)),
              el('td', {}, s.last_status === 'ok'
                ? el('span', { class: 'badge allowed' }, 'connected')
                : el('span', { class: 'badge denied', title: s.last_error || '' }, s.last_status || 'error')),
              el('td', {}, oauthBadge(s)),
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
  if (s.transport === 'stdio') return s.command + (s.args && s.args.length ? ' ' + s.args.join(' ') : '');
  return s.url || '';
}

// serverUsesSecret reports whether any env/header value on a (masked) server
// is a secret:// reference, so the list can show a lock badge.
function serverUsesSecret(s) {
  const refs = (m) => Object.values(m || {}).some((v) => typeof v === 'string' && v.startsWith('secret://'));
  return refs(s.env) || refs(s.headers);
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
        'Search across every connected MCP server and try a tool right here. Calls run through the gateway exactly like an agent would: writes hold for approval, reads pass through. Stuck-pending calls show up in the Approvals tab — approve in another tab and the result lands here.'),
      el('input', {
        placeholder: 'filter by name, description, or upstream (e.g. "github", "search", "context7")…',
        value: filter,
        on: { input: (e) => { state.toolFilter = e.target.value; render(); } },
      }),
      el('div', { class: 'meta', style: 'margin-top: 8px;' },
        `${tools.length} of ${state.tools.length} tools`),
    ),
    tools.length === 0
      ? el('div', { class: 'card empty' }, state.tools.length === 0 ? 'No tools yet — install an MCP server in the Servers tab.' : 'No tools match your filter.')
      : el('div', { class: 'workbench' },
          el('div', { class: 'tool-list' },
            tools.map((t) => {
              const c = counts[t.name] || 0;
              return el('div', {
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
        disabled: wb.running ? '' : null,
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
              : isPending ? 'Approval pending — approve in the Approvals tab, then re-run with _approval_id'
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

function viewSettings() {
  const mode = state.settings.surface_mode || 'full';
  const N    = Number(state.settings.top_n_count || 20);
  const T    = Number(state.settings.top_n_personalize_after || 100);

  // Pinned set (must mirror gateway.PinnedTools).
  const pinned = new Set(['tools.search', 'tools.execute', 'memory.get', 'memory.set', 'memory.list', 'memory.delete']);
  const totalTools = state.tools.length;
  const pinnedCount = state.tools.filter((t) => pinned.has(t.name)).length;

  // Approximate "what each NEW agent currently sees" — pinned + top-N
  // selected globally because no agent has crossed the personalisation
  // threshold for them.
  const globalTop = Object.entries(state.usage.per_tool || {})
    .filter(([name]) => !pinned.has(name))
    .sort((a, b) => b[1] - a[1])
    .slice(0, N)
    .map(([name]) => name);
  const exposedInTopN = pinnedCount + globalTop.length;

  const exposedCount =
    mode === 'full' ? totalTools :
    mode === 'router_only' ? pinnedCount :
    exposedInTopN;

  const setMode = async (next) => {
    try {
      await api('/v1/settings', { method: 'PATCH', body: { surface_mode: next }});
      state.settings.surface_mode = next;
      toast('Mode: ' + next);
      render();
    } catch (err) { toast(err.message, 'error'); }
  };

  const radio = (val, label, sub) => el('label', {
    style: 'display: flex; gap: 12px; align-items: flex-start; cursor: pointer; padding: 8px 0; border-top: 1px solid var(--border);',
  },
    el('input', {
      type: 'radio',
      name: 'surface_mode',
      checked: mode === val,
      style: 'margin-top: 4px;',
      on: { change: () => setMode(val) },
    }),
    el('div', {},
      el('div', { style: 'font-weight: 500;' }, label),
      el('div', { class: 'meta', style: 'margin-top: 4px;' }, sub),
    ),
  );

  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Agent surface'),
      el('p', { class: 'meta' },
        'Controls which tools each agent sees in its tools/list. Pinned tools (',
        el('code', {}, 'tools.search'), ', ', el('code', {}, 'tools.execute'), ', ', el('code', {}, 'memory.*'),
        ') are always visible regardless of mode. Approximate exposure for a new agent: ',
        el('code', {}, exposedCount + ' / ' + totalTools + ' tools'), '.'),
      radio('full', 'Full catalog',
        'Every wrapped tool. Simple, but the agent\'s prompt carries every schema — pricey when you have many servers connected.'),
      radio('top_n', 'Top-N most used (recommended)',
        'Pinned tools + the agent\'s top-N most-used real tools. New agents fall back to the overall top-N until they\'ve made enough calls of their own to personalise.'),
      radio('router_only', 'Router only',
        'Pinned tools only. The model uses tools.search to discover anything else and tools.execute to invoke. Smallest possible prompt; one extra hop per call.'),
      mode === 'top_n' ? el('div', { style: 'margin-top: 12px; display: grid; grid-template-columns: 1fr 1fr; gap: 12px;' },
        el('label', {},
          el('div', { class: 'meta' }, 'Top-N count'),
          el('input', {
            type: 'number', min: 1, max: 200,
            value: String(N),
            on: { change: async (e) => {
              const v = Math.max(1, Math.min(200, parseInt(e.target.value, 10) || 20));
              try {
                await api('/v1/settings', { method: 'PATCH', body: { top_n_count: v }});
                state.settings.top_n_count = v;
                toast('top_n_count = ' + v);
                render();
              } catch (err) { toast(err.message, 'error'); }
            }},
          }),
          el('div', { class: 'meta', style: 'margin-top: 4px;' }, 'Extra tools beyond the pinned set.'),
        ),
        el('label', {},
          el('div', { class: 'meta' }, 'Personalise after (calls)'),
          el('input', {
            type: 'number', min: 0, max: 1000000,
            value: String(T),
            on: { change: async (e) => {
              const v = Math.max(0, parseInt(e.target.value, 10) || 0);
              try {
                await api('/v1/settings', { method: 'PATCH', body: { top_n_personalize_after: v }});
                state.settings.top_n_personalize_after = v;
                toast('top_n_personalize_after = ' + v);
                render();
              } catch (err) { toast(err.message, 'error'); }
            }},
          }),
          el('div', { class: 'meta', style: 'margin-top: 4px;' }, 'Until an agent has made this many successful calls, it sees the overall top-N rather than its own.'),
        ),
      ) : null,
    ),
    renderLakeIntegrationCard(),
    renderPushCard(),
    renderChatCard(),
    renderSecretsCard(),
    el('div', { class: 'card' },
      el('h2', {}, 'About'),
      el('p', {}, 'toolyard v0.1.0 — Apache-2.0.'),
      el('p', { class: 'meta' }, 'Single Go binary + SQLite. Source: ', el('code', {}, 'github.com/tusharbhardwaj/toolyard')),
    ),
    renderBackupCard(),
  );
}

// renderBackupCard offers full-store exports and a memory import.
function renderBackupCard() {
  return el('div', { class: 'card' },
    el('h2', {}, 'Backup & export'),
    el('p', { class: 'meta' }, 'Download the full audit log, approvals (decision tokens excluded), and memory store. Memory can be re-imported.'),
    el('div', { class: 'row', style: 'flex-wrap: wrap; gap: 8px;' },
      el('a', { href: '/v1/audit/export?format=csv', target: '_blank' }, 'Audit CSV'),
      el('a', { href: '/v1/audit/export?format=json', target: '_blank' }, 'Audit JSON'),
      el('a', { href: '/v1/approvals/export?format=csv', target: '_blank' }, 'Approvals CSV'),
      el('a', { href: '/v1/approvals/export?format=json', target: '_blank' }, 'Approvals JSON'),
      el('a', { href: '/v1/memory/export', target: '_blank' }, 'Memory JSON'),
    ),
    el('div', { class: 'row', style: 'margin-top: 12px; align-items: center; gap: 8px;' },
      el('span', { class: 'meta' }, 'Import memory:'),
      (() => {
        const sel = el('select', { id: 'mem-import-mode' },
          el('option', { value: 'merge' }, 'Merge'),
          el('option', { value: 'replace' }, 'Replace all'));
        return sel;
      })(),
      el('input', { id: 'mem-import-file', type: 'file', accept: 'application/json,.json' }),
      el('button', { class: 'primary', on: { click: importMemoryFile } }, 'Import'),
    ),
  );
}

async function importMemoryFile() {
  const fileEl = $('mem-import-file');
  const mode = ($('mem-import-mode') || {}).value || 'merge';
  const file = fileEl && fileEl.files && fileEl.files[0];
  if (!file) { toast('Choose a memory JSON file first.', 'error'); return; }
  if (mode === 'replace' && !confirm('Replace ALL memory entries with the imported file? This deletes everything not in the file.')) return;
  try {
    const text = await file.text();
    const doc = JSON.parse(text);
    // Accept either the versioned envelope or a bare entries array.
    const body = Array.isArray(doc) ? { mode, entries: doc } : Object.assign({}, doc, { mode });
    const resp = await api('/v1/memory/import', { method: 'POST', body });
    toast(`Imported ${resp.imported} memories (${mode}).`);
    await loadAll();
    render();
  } catch (e) { toast('Import failed: ' + e.message, 'error'); }
}

// renderLakeIntegrationCard shows the Lake API token + Grafana origin
// controls. The token's actual value never lands in any GET response —
// it's auto-generated on first gateway boot and only revealed via the
// explicit one-shot reveal endpoint. Rotation issues a fresh value and
// returns it once for copy. The Grafana origin field updates the CSP
// frame-src directive live (no gateway restart) so /lake/ can iframe
// the named Grafana instance.
function renderLakeIntegrationCard() {
  const tokenSet = !!state.settings.lake_api_token_present;
  const grafanaOrigin = state.settings.grafana_origin || '';

  // Reveal flow: POST /v1/settings/reveal {key: lake_api_token}, show
  // the value in a toast-like box with a one-tap copy button. The DOM
  // node clears itself after 60s so a logged-in laptop left unattended
  // doesn't keep the token on screen.
  const reveal = async () => {
    try {
      const r = await api('/v1/settings/reveal', { method: 'POST', body: { key: 'lake_api_token' }});
      showSecretBox('Lake API Token', r.value);
    } catch (err) { toast(err.message, 'error'); }
  };

  const rotate = async () => {
    if (!confirm('Rotate the lake API token? Existing Grafana datasources keep using the old value until you restart the Grafana container.')) {
      return;
    }
    try {
      const r = await api('/v1/settings/rotate', { method: 'POST', body: { key: 'lake_api_token' }});
      state.settings.lake_api_token_present = true;
      if (r.warning) toast(r.warning, 'error');
      showSecretBox('New lake API token', r.value, 'Run: sudo docker compose -f deploy/grafana/docker-compose.yaml restart grafana');
      render();
    } catch (err) { toast(err.message, 'error'); }
  };

  let originDraft = grafanaOrigin;
  const saveOrigin = async () => {
    try {
      await api('/v1/settings', { method: 'PATCH', body: { grafana_origin: originDraft }});
      state.settings.grafana_origin = originDraft;
      toast('Grafana origin saved');
      render();
    } catch (err) { toast(err.message, 'error'); }
  };

  return el('div', { class: 'card' },
    el('h2', {}, 'Lake & Grafana integration'),
    el('p', { class: 'meta' },
      'Configures how external read-only consumers (Grafana Infinity, scripts) reach ',
      el('code', {}, '/v1/lake/*'), ', and which origin the toolyard ',
      el('code', {}, '/lake/'), ' page is allowed to iframe.',
    ),
    // --- Lake API token row ---
    el('div', { style: 'display: grid; grid-template-columns: 1fr auto auto; gap: 8px; align-items: center; margin-top: 8px; padding-top: 12px; border-top: 1px solid var(--border);' },
      el('div', {},
        el('div', { style: 'font-weight: 500;' }, 'Lake API token'),
        el('div', { class: 'meta', style: 'margin-top: 4px;' },
          tokenSet
            ? 'Set. Reveal once to copy into Grafana, or rotate to invalidate the existing value.'
            : 'Not set — toolyard will mint one on the next gateway start.',
        ),
      ),
      el('button', {
        class: 'btn',
        disabled: !tokenSet,
        on: { click: reveal },
      }, 'Reveal once'),
      el('button', {
        class: 'btn',
        on: { click: rotate },
      }, tokenSet ? 'Rotate' : 'Generate'),
    ),
    // --- Grafana origin row ---
    el('div', { style: 'display: grid; grid-template-columns: 1fr auto; gap: 8px; align-items: center; margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--border);' },
      el('label', {},
        el('div', { style: 'font-weight: 500;' }, 'Grafana origin'),
        el('div', { class: 'meta', style: 'margin: 4px 0;' },
          'Added to the CSP ', el('code', {}, 'frame-src'),
          ' so the toolyard ', el('code', {}, '/lake/'),
          ' page can embed Grafana panels. Empty keeps the strict default.',
        ),
        el('input', {
          type: 'url',
          placeholder: 'http://localhost:3030',
          value: grafanaOrigin,
          style: 'width: 100%;',
          on: { input: (e) => { originDraft = e.target.value; } },
        }),
      ),
      el('button', { class: 'btn', on: { click: saveOrigin } }, 'Save'),
    ),
  );
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
      'Get pending approvals in Telegram with inline Approve / Deny buttons — usable from anywhere, no public URL needed (the bot uses outbound long-polling).'),
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

// renderSecretsCard is the secrets-broker settings card: a write-only store of
// credentials referenced as secret://NAME in upstream env/headers. Values are
// never shown.
function renderSecretsCard() {
  if (!state.secretsLoaded) { loadSecrets(); }
  let nName = '', nVal = '', nDesc = '';
  const create = async () => {
    if (!/^[A-Z][A-Z0-9_]{0,63}$/.test(nName)) { toast('Name must be UPPER_SNAKE (e.g. API_KEY)', 'error'); return; }
    if (!nVal) { toast('Value required', 'error'); return; }
    try {
      await api('/v1/secrets', { method: 'POST', body: { name: nName, value: nVal, description: nDesc } });
      toast('Stored ' + nName);
      loadSecrets();
    } catch (e) { toast(e.message, 'error'); }
  };
  const rows = (state.secrets || []).map((s) => el('tr', {},
    el('td', {}, el('code', {}, s.name)),
    el('td', { class: 'meta' }, s.description || '—'),
    el('td', { class: 'meta' }, (s.used_by && s.used_by.length) ? s.used_by.join(', ') : '—'),
    el('td', {},
      el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => rotateSecret(s.name) } }, 'Rotate'),
      ' ',
      el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => deleteSecret(s.name, s.used_by || []) } }, 'Delete'),
    ),
  ));
  return el('div', { class: 'card' },
    el('h2', {}, 'Secrets'),
    el('p', { class: 'meta' },
      'Store API keys once, encrypted. Reference them in a server\'s env/headers as ',
      el('code', {}, 'secret://NAME'), '. Values are resolved only at dial time and never returned by the API.'),
    state.secrets && state.secrets.length ? el('table', { class: 'tbl', style: 'width:100%; margin-top:8px;' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Name'), el('th', {}, 'Description'), el('th', {}, 'Used by'), el('th', {}, ''))),
      el('tbody', {}, ...rows),
    ) : el('p', { class: 'meta' }, 'No secrets yet.'),
    el('div', { style: 'margin-top:12px; display:grid; grid-template-columns: 1fr 1fr; gap:8px;' },
      el('input', { placeholder: 'NAME', style: 'padding:6px 8px;', on: { input: (e) => { nName = e.target.value.toUpperCase(); e.target.value = nName; } } }),
      el('input', { type: 'password', placeholder: 'value', style: 'padding:6px 8px;', on: { input: (e) => { nVal = e.target.value; } } }),
    ),
    el('div', { style: 'margin-top:8px; display:flex; gap:8px;' },
      el('input', { placeholder: 'description (optional)', style: 'flex:1; padding:6px 8px;', on: { input: (e) => { nDesc = e.target.value; } } }),
      el('button', { class: 'btn', on: { click: create } }, 'Add secret'),
    ),
  );
}

async function loadSecrets() {
  state.secretsLoaded = true;
  try { state.secrets = await api('/v1/secrets'); render(); } catch (_) {}
}
async function rotateSecret(name) {
  const v = prompt('New value for ' + name + ' (rotates + reconnects referencing servers):');
  if (v == null || v === '') return;
  try { await api('/v1/secrets/' + encodeURIComponent(name) + '?reconnect=1', { method: 'PUT', body: { value: v } }); toast('Rotated ' + name); loadSecrets(); }
  catch (e) { toast(e.message, 'error'); }
}
async function deleteSecret(name, usedBy) {
  if (usedBy.length && !confirm(name + ' is used by ' + usedBy.join(', ') + '. Force delete anyway?')) return;
  const q = usedBy.length ? '?force=1' : '';
  try { await api('/v1/secrets/' + encodeURIComponent(name) + q, { method: 'DELETE' }); toast('Deleted ' + name); loadSecrets(); }
  catch (e) { toast(e.message, 'error'); }
}

// ---- Events Hub ------------------------------------------------------------

async function loadEvents() {
  state.events.loaded = true;
  const f = state.eventFilter;
  const p = new URLSearchParams();
  if (f.source_id) p.set('source_id', f.source_id);
  if (f.type) p.set('type', f.type);
  if (f.q) p.set('q', f.q);
  if (f.unacked) p.set('unacked', '1');
  p.set('limit', '100');
  try {
    const [feed, sources] = await Promise.all([
      api('/v1/events?' + p.toString()),
      api('/v1/event-sources').catch(() => []),
    ]);
    state.events.rows = feed.events || [];
    state.events.unacked = feed.unacked || 0;
    state.events.nextBefore = feed.next_before || 0;
    state.events.sources = sources || [];
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function eventRelTime(ms) {
  const d = (Date.now() - ms) / 1000;
  if (d < 60) return 'just now';
  if (d < 3600) return Math.floor(d / 60) + 'm ago';
  if (d < 86400) return Math.floor(d / 3600) + 'h ago';
  return Math.floor(d / 86400) + 'd ago';
}

async function ackEvents(ids) {
  if (!ids.length) return;
  try {
    await api('/v1/events/ack', { method: 'POST', body: { ids } });
    loadEvents();
  } catch (e) { toast(e.message, 'error'); }
}

function viewEvents() {
  if (!state.events.loaded) loadEvents();
  const f = state.eventFilter;
  const ev = state.events;

  const filterBar = el('div', { class: 'card', style: 'display:flex; gap:8px; flex-wrap:wrap; align-items:center;' },
    el('select', { style: 'padding:6px 8px;', on: { change: (e) => { f.source_id = e.target.value; loadEvents(); } } },
      el('option', { value: '' }, 'All sources'),
      ...(ev.sources || []).map((s) => el('option', { value: s.id, selected: f.source_id === s.id }, s.name)),
    ),
    el('input', { placeholder: 'type', value: f.type, style: 'padding:6px 8px; width:120px;', on: { change: (e) => { f.type = e.target.value; loadEvents(); } } }),
    el('input', { placeholder: 'search summary…', value: f.q, style: 'padding:6px 8px; flex:1;', on: { change: (e) => { f.q = e.target.value; loadEvents(); } } }),
    el('label', { style: 'display:flex; gap:6px; align-items:center;' },
      el('input', { type: 'checkbox', checked: f.unacked, on: { change: (e) => { f.unacked = e.target.checked; loadEvents(); } } }),
      'Unacked only',
    ),
    el('button', { class: 'btn', on: { click: () => ackEvents((ev.rows || []).filter((r) => !r.acked_at).map((r) => r.id)) } }, 'Ack all visible'),
  );

  const rows = (ev.rows || []).map((r) => el('div', {
    class: 'card', style: 'padding:10px 14px; ' + (r.acked_at ? 'opacity:0.6;' : ''),
  },
    el('div', { style: 'display:flex; gap:10px; align-items:baseline; flex-wrap:wrap;' },
      el('span', { class: 'meta', style: 'min-width:64px;' }, eventRelTime(r.received_at)),
      el('span', { style: 'font-weight:500;' }, r.source_name),
      el('span', { style: 'padding:1px 7px; border-radius:9px; background:var(--bg); border:1px solid var(--border); font-size:12px;' }, r.type),
      el('span', { style: 'flex:1;' }, r.summary),
      r.acked_at ? el('span', { class: 'meta' }, '✓ acked')
        : el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => ackEvents([r.id]) } }, 'Ack'),
    ),
    r.payload ? el('details', { style: 'margin-top:6px;' },
      el('summary', { class: 'meta', style: 'cursor:pointer;' }, 'payload'),
      el('pre', { style: 'white-space:pre-wrap; word-break:break-all; font-size:12px; margin:6px 0 0;' },
        (() => { try { return JSON.stringify(JSON.parse(r.payload), null, 2); } catch { return String(r.payload); } })()),
    ) : null,
  ));

  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Events',
        ev.unacked ? el('span', { style: 'margin-left:8px; font-size:14px; color:var(--muted);' }, '(' + ev.unacked + ' unacked)') : null),
      el('p', { class: 'meta' }, 'The shared activity feed across agents and external systems. Webhooks push events in, pollers watch URLs, and agents publish to each other. Every event has a natural-language summary; agents read them with the events.brief tool.')),
    filterBar,
    rows.length ? el('div', {}, ...rows) : el('div', { class: 'card' }, el('p', { class: 'meta' }, 'No events. Create a source below and push one in.')),
    renderEventSourcesCard(),
    state.eventSourceModal ? renderEventSourceModal() : null,
  );
}

function renderEventSourcesCard() {
  const sources = state.events.sources || [];
  const rows = sources.map((s) => el('tr', {},
    el('td', {}, s.name, ' ', el('span', { class: 'meta' }, '(' + s.kind + ')')),
    el('td', {},
      el('label', { style: 'display:inline-flex; gap:4px; align-items:center; margin-right:10px;' },
        el('input', { type: 'checkbox', checked: s.enabled, on: { change: (e) => patchSource(s.id, { enabled: e.target.checked }) } }), 'on'),
      el('label', { style: 'display:inline-flex; gap:4px; align-items:center;' },
        el('input', { type: 'checkbox', checked: s.notify, on: { change: (e) => patchSource(s.id, { notify: e.target.checked }) } }), 'notify'),
    ),
    el('td', { class: 'meta' }, s.last_error ? ('⚠ ' + s.last_error) : (s.poller_state && s.poller_state.last_polled_at ? 'polled ' + eventRelTime(s.poller_state.last_polled_at) : '—')),
    el('td', {},
      s.kind === 'webhook' ? el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => rotateSourceToken(s.id) } }, 'Token') : null,
      ' ',
      el('button', { class: 'btn', style: 'font-size:12px;', on: { click: () => deleteSource(s.id, s.name) } }, 'Delete'),
    ),
  ));
  return el('div', { class: 'card' },
    el('h2', {}, 'Sources',
      el('button', { class: 'btn', style: 'float:right; font-size:12px;', on: { click: () => { state.eventSourceModal = { kind: 'webhook', name: '', url: '', interval: 300, mode: 'hash', json_path: '' }; render(); } } }, '+ Add source')),
    sources.length ? el('table', { class: 'tbl', style: 'width:100%; margin-top:8px;' },
      el('thead', {}, el('tr', {}, el('th', {}, 'Name'), el('th', {}, 'State'), el('th', {}, 'Last'), el('th', {}, ''))),
      el('tbody', {}, ...rows),
    ) : el('p', { class: 'meta' }, 'No sources yet. Add a webhook to receive events or a poller to watch a URL.'),
  );
}

function renderEventSourceModal() {
  const m = state.eventSourceModal;
  const close = () => { state.eventSourceModal = null; render(); };
  const create = async () => {
    const body = { name: m.name, kind: m.kind, notify: false };
    if (m.kind === 'poller') {
      body.poller_config = { url: m.url, interval_sec: Number(m.interval) || 300, mode: m.mode, json_path: m.json_path };
    }
    try {
      const r = await api('/v1/event-sources', { method: 'POST', body });
      close();
      loadEvents();
      if (r.token) showSecretBox('Webhook token for ' + m.name, r.token, r.curl_example || 'Send events with: Authorization: Bearer <token>');
    } catch (e) { toast(e.message, 'error'); }
  };
  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) close(); } } },
    el('div', { class: 'modal' },
      el('h3', {}, 'Add event source'),
      el('div', { style: 'display:flex; gap:8px; margin:8px 0;' },
        ...['webhook', 'poller', 'agent'].map((k) => el('label', { style: 'display:flex; gap:4px; align-items:center;' },
          el('input', { type: 'radio', name: 'evkind', checked: m.kind === k, on: { change: () => { m.kind = k; render(); } } }), k)),
      ),
      el('input', { placeholder: 'source name', value: m.name, style: 'width:100%; padding:6px 8px; margin-bottom:8px;', on: { input: (e) => { m.name = e.target.value; } } }),
      m.kind === 'poller' ? el('div', {},
        el('input', { placeholder: 'https://url-to-watch', value: m.url, style: 'width:100%; padding:6px 8px; margin-bottom:8px;', on: { input: (e) => { m.url = e.target.value; } } }),
        el('div', { style: 'display:flex; gap:8px; margin-bottom:8px;' },
          el('input', { type: 'number', min: 60, placeholder: 'interval (s)', value: m.interval, style: 'width:120px; padding:6px 8px;', on: { input: (e) => { m.interval = e.target.value; } } }),
          el('select', { style: 'padding:6px 8px;', on: { change: (e) => { m.mode = e.target.value; render(); } } },
            el('option', { value: 'hash', selected: m.mode === 'hash' }, 'whole-page hash'),
            el('option', { value: 'json_field', selected: m.mode === 'json_field' }, 'JSON field'),
          ),
        ),
        m.mode === 'json_field' ? el('input', { placeholder: 'json path e.g. data.price', value: m.json_path, style: 'width:100%; padding:6px 8px; margin-bottom:8px;', on: { input: (e) => { m.json_path = e.target.value; } } }) : null,
      ) : null,
      el('div', { style: 'display:flex; gap:8px; justify-content:flex-end; margin-top:8px;' },
        el('button', { class: 'btn', on: { click: close } }, 'Cancel'),
        el('button', { class: 'btn primary', on: { click: create } }, 'Create'),
      ),
    ),
  );
}

async function patchSource(id, body) {
  try { await api('/v1/event-sources/' + encodeURIComponent(id), { method: 'PATCH', body }); loadEvents(); }
  catch (e) { toast(e.message, 'error'); }
}
async function rotateSourceToken(id) {
  try {
    const r = await api('/v1/event-sources/' + encodeURIComponent(id) + '/rotate-token', { method: 'POST', body: {} });
    showSecretBox('New webhook token', r.token, r.curl_example);
  } catch (e) { toast(e.message, 'error'); }
}
async function deleteSource(id, name) {
  if (!confirm('Delete source ' + name + ' and all its events?')) return;
  try { await api('/v1/event-sources/' + encodeURIComponent(id), { method: 'DELETE' }); toast('Deleted'); loadEvents(); }
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
    body = el('div', { class: 'meta' }, 'No VAPID key on the server.');
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
      el('div', { class: 'row' },
        el('button', { class: 'primary', disabled: !!state.pushEnabling, on: { click: enablePush }},
          state.pushEnabling ? 'Enabling…' : (state.pushReady ? 'Push enabled ✓ (re-enroll)' : 'Enable push')),
        el('button', { on: { click: testPush }}, 'Send test push'),
        el('button', { on: { click: showPushDiag }}, 'Diagnostics'),
        el('button', { class: 'danger', on: { click: wipeAndReenroll }}, 'Wipe & re-enroll'),
      ),
      el('div', { class: 'row', style: 'margin-top: 6px;' },
        el('button', { on: { click: showJWTPreview }}, 'Show JWT details'),
        el('button', { class: 'danger', on: { click: rotateVapidKeypair }}, 'Rotate VAPID keys'),
      ),
      el('div', { class: 'meta', style: 'margin-top: 4px;' },
        'BadJwtToken from Apple? Tap "Rotate VAPID keys" — that regenerates the keypair using the upstream library (eliminating any format ambiguity), wipes all subscriptions, and lets you re-enroll fresh. Tap "Show JWT details" to inspect the exact claims toolyard signs into the Authorization header.'),
      state.jwtPreview ? renderJWTPreview() : null,
      state.pushDiag ? renderPushDiag() : null,
      state.pushTestResult ? renderPushTestResult() : null,
      ios ? iosSteps : null,
    );
  }

  return el('div', { class: 'card' },
    el('h2', {}, 'Web Push'),
    el('p', { class: 'meta' },
      'Get a notification on this device when an approval is pending. The notification has Allow / Deny actions tied to the approval\'s signed token, so you can decide right from the lock screen.'),
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
    return el('tr', {},
      el('td', {}, el('code', {}, idShort), a.agent_name ? el('div', { class: 'meta' }, a.agent_name) : null),
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
    if (r.agent_id) parts.push('agent=' + r.agent_id.slice(0, 10));
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

  // Cost panel
  const costRows = state.insights.cost.rows || [];
  const totalUsd = costRows.reduce((acc, r) => acc + (r.usd_estimated || 0), 0);
  const costCard = el('div', { class: 'card' },
    el('h2', {}, 'Cost (estimated)'),
    el('p', { class: 'meta' },
      'Token estimate: 1 token ≈ 4 bytes. Edit the per-million USD rates in Settings — they default to 0 (i.e. cost panel is dark) so we don\'t pretend to know what your stack costs.',
    ),
    el('div', { class: 'row', style: 'gap: 6px; margin-bottom: 8px;' },
      el('span', { class: 'meta' }, 'Rates: input $' + state.insights.cost.input_usd_per_m + '/M · output $' + state.insights.cost.output_usd_per_m + '/M · estimated total: '),
      el('strong', {}, '$' + totalUsd.toFixed(4)),
    ),
    costRows.length === 0 ? el('div', { class: 'empty' }, 'No cost data in this range yet.') :
      el('table', {},
        el('thead', {}, el('tr', {}, el('th', {}, 'Tool'), el('th', {}, 'Tokens in'), el('th', {}, 'Tokens out'), el('th', {}, 'USD'))),
        el('tbody', {}, ...costRows.slice(0, 30).map((r) => el('tr', {},
          el('td', {}, el('code', {}, r.tool_name)),
          el('td', {}, fmtNum(r.tokens_in)),
          el('td', {}, fmtNum(r.tokens_out)),
          el('td', {}, '$' + r.usd_estimated.toFixed(4)),
        ))),
      ),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('a', { href: '/v1/insights/export?range=' + state.insights.range, target: '_blank' }, 'Download CSV (range)'),
    ),
  );

  return el('div', {},
    overviewCard,
    toolCard,
    renderPolicyRulesCard(),
    agentCard,
    autoCard,
    costCard,
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
            el('td', {}, [a.agent_id ? el('div', {}, el('code', {}, (a.agent_id || '').slice(0, 12))) : null,
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

function navigate(route) {
  state.route = route;
  history.replaceState(null, '', '#' + route);
  if ((route === 'insights' || route === 'notifications') && !state.insights.loading) {
    loadInsights();
  }
  if (route === 'hooks' && !state.hooks.loaded && !state.hooks.loading) {
    loadHooks(true);
  }
  if (route === 'lake' && !state.lake.manifest && !state.lake.loading) {
    loadLakeManifest();
  }
  render();
}

function shell(content) {
  const navBtn = (key, label) => el('button', {
    class: state.route === key ? 'active' : '',
    on: { click: () => navigate(key) }
  }, label);

  // Bottom-nav routes for mobile. We surface the 5 most-used routes
  // directly and put the rest behind a "More" sheet so the bar isn't
  // cramped. Approvals + Tools are the bread-and-butter; Servers and
  // Notifications get badges when there's something to act on.
  const pendingCount = (state.approvals || []).filter((a) => a.status === 'pending').length;
  const alertCount   = (state.anomalies || []).length;
  const bottomItem = (key, icon, label, badge) => el('button', {
    class: state.route === key ? 'active' : '',
    on: { click: () => navigate(key) }
  },
    el('span', { class: 'icon' }, icon),
    el('span', {}, label),
    badge > 0 ? el('span', { class: 'badge-count' }, String(badge)) : null,
  );

  return el('div', {},
    el('header', {},
      el('div', { class: 'brand' }, el('span', { class: 'dot' }), 'toolyard'),
      el('nav', {},
        navBtn('approvals',    'Approvals'),
        navBtn('call',         'Call' + (state.call.active ? ' ●' : '')),
        navBtn('lake',         'Lake'),
        navBtn('events',       'Events' + (state.events.unacked ? ' (' + state.events.unacked + ')' : '')),
        navBtn('insights',     'Insights'),
        navBtn('notifications', 'Alerts' + (alertCount ? ' (' + alertCount + ')' : '')),
        navBtn('audit',        'Audit'),
        navBtn('hooks',        'Hooks'),
        navBtn('servers',      'Servers'),
        navBtn('tools',        'Tools'),
        navBtn('memory',       'Memory'),
        navBtn('agents',       'Agents'),
        navBtn('settings',     'Settings'),
      ),
      el('span', { class: 'user' },
        renderStreamPill(),
        ' ', state.user ? state.user.username : '',
      ),
      state.user ? el('button', { on: { click: async () => {
        try { await api('/v1/auth/logout', { method: 'POST' }); } catch {}
        if (evtSrc) try { evtSrc.close(); } catch {}
        state.user = null; render();
      }}}, 'Logout') : null,
    ),
    el('main', {}, content),
    // Bottom nav is rendered for everyone but CSS hides it above 768px.
    // The "More" item opens a sheet rather than navigating, so its active
    // state mirrors whatever the current route is when it isn't one of
    // the four primary routes.
    el('div', { class: 'bottom-nav' }, el('div', { class: 'row' },
      bottomItem('approvals', '✓', 'Approvals', pendingCount),
      bottomItem('tools',     '⚙', 'Tools'),
      bottomItem('servers',   '⌘', 'Servers'),
      bottomItem('notifications', '◔', 'Alerts', alertCount),
      el('button', {
        class: ['audit','hooks','memory','agents','settings','insights'].includes(state.route) ? 'active' : '',
        on: { click: () => { state.moreSheet = true; render(); } }
      },
        el('span', { class: 'icon' }, '☰'),
        el('span', {}, 'More'),
      ),
    )),
    state.moreSheet ? renderMoreSheet() : null,
  );
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
      item('call',     'Call',     'Talk to Toolyard through Gemini Live'),
      item('lake',     'Lake',     'Personal data warehouse — finance, ops, daily memory'),
      item('insights', 'Insights', 'Per-tool, per-agent, cost breakdowns'),
      item('audit',    'Audit',    'Append-only event log'),
      item('hooks',    'Hooks',    'Agent lifecycle events and memory ingest'),
      item('memory',   'Memory',   'Scope/key-value store'),
      item('agents',   'Agents',   'Manage enrolled agents'),
      item('settings', 'Settings', 'Surface mode, auto-approval, retention'),
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
    target.focus();
    if (f.selStart != null && target.setSelectionRange) {
      try { target.setSelectionRange(f.selStart, f.selEnd); } catch (_) {}
    }
  }
}

// closeTopmostOverlay dismisses the highest-priority open overlay/edit and
// returns true if it handled the Escape.
function closeTopmostOverlay() {
  if (state.memEdit) { state.memEdit = null; render(); return true; }
  if (state.agentModal) { state.agentModal = null; render(); return true; }
  if (state.marketModal) { state.marketModal = null; render(); return true; }
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
  root.innerHTML = '';
  if (state.setupRequired) { root.appendChild(viewSetup()); return; }
  if (!state.user) { root.appendChild(viewLogin()); return; }
  let body;
  switch (state.route) {
    case 'audit':         body = viewAudit();         break;
    case 'hooks':         body = viewHooks();         break;
    case 'memory':        body = viewMemory();        break;
    case 'agents':        body = viewAgents();        break;
    case 'servers':       body = viewServers();       break;
    case 'tools':         body = viewTools();         break;
    case 'settings':      body = viewSettings();      break;
    case 'insights':      body = viewInsights();      break;
    case 'lake':          body = viewLake();          break;
    case 'call':          body = viewCall();          break;
    case 'events':        body = viewEvents();        break;
    case 'notifications': body = viewNotifications(); break;
    default:              body = viewApprovals();
  }
  root.appendChild(shell(body));
  restoreFocus(focus);
}

// ---- Lake (personal data warehouse, TUS-104) -------------------------------
//
// The lake lives at /v1/lake/* on this same gateway. We render its manifest
// inline as a `lake` route inside this dashboard so login state, push, and
// nav are all shared. ECharts is loaded lazily on first visit so users who
// never click Lake don't pay the 1MB bundle cost.

async function loadLakeManifest() {
  state.lake.loading = true; state.lake.error = null; render();
  try {
    const m = await api('/v1/lake/manifest');
    state.lake.manifest = m;
    if (!state.lake.activeTab && m.tabs && m.tabs.length) {
      state.lake.activeTab = m.tabs[0].id;
    }
  } catch (e) {
    state.lake.error = e.message;
  }
  state.lake.loading = false;
  render();
  // Kick the panels for the active tab and lazy-load ECharts.
  if (state.lake.manifest && state.lake.activeTab) {
    ensureECharts().then(() => loadLakeTabData(state.lake.activeTab));
  }
}

function ensureECharts() {
  if (state.lake.echartsLoaded || (typeof window !== 'undefined' && window.echarts)) {
    state.lake.echartsLoaded = true;
    return Promise.resolve();
  }
  return new Promise((resolve) => {
    const s = document.createElement('script');
    s.src = '/lake/vendor/echarts.min.js';
    s.async = true;
    s.onload = () => { state.lake.echartsLoaded = true; resolve(); };
    s.onerror = () => {
      // ECharts not vendored — chart panels will fall back to a clean
      // error state but tables / KPIs / SQL editor still work.
      state.lake.echartsLoaded = false;
      resolve();
    };
    document.head.appendChild(s);
  });
}

async function loadLakeTabData(tabId) {
  const tab = (state.lake.manifest && state.lake.manifest.tabs || []).find((t) => t.id === tabId);
  if (!tab) return;
  for (const p of tab.panels || []) {
    const key = tabId + '/' + p.id;
    if (state.lake.panelData[key]) continue; // cached
    if (p.type === 'sql_editor') continue;   // no remote fetch for editor
    if (!p.query) continue;
    const qid = p.query.split('/').pop().replace(/\.sql$/, '');
    try {
      const res = await api('/v1/lake/run/' + encodeURIComponent(tabId) + '/' + encodeURIComponent(qid));
      state.lake.panelData[key] = res;
      delete state.lake.panelErrors[key];
    } catch (e) {
      state.lake.panelErrors[key] = e.message;
    }
    render();
  }
}

function lakeSetTab(id) {
  state.lake.activeTab = id;
  render();
  ensureECharts().then(() => loadLakeTabData(id));
}

function viewLake() {
  if (state.lake.loading && !state.lake.manifest) {
    return el('div', { class: 'card' }, el('div', { class: 'empty' }, 'Loading lake…'));
  }
  if (state.lake.error) {
    return el('div', { class: 'card' },
      el('h2', {}, 'Lake'),
      el('div', { class: 'err' }, 'Failed to load: ' + state.lake.error),
      el('button', { on: { click: () => loadLakeManifest() } }, 'Retry'),
    );
  }
  if (!state.lake.manifest) {
    // First-paint hit when navigation fired but loadLakeManifest hasn't returned.
    setTimeout(() => loadLakeManifest(), 0);
    return el('div', { class: 'card' }, el('div', { class: 'empty' }, 'Loading lake…'));
  }
  const tabs = state.lake.manifest.tabs || [];
  const active = tabs.find((t) => t.id === state.lake.activeTab) || tabs[0];

  const tabBar = el('div', { class: 'row', style: 'gap: 4px; margin-bottom: 12px;' },
    ...tabs.map((t) => el('button', {
      class: t.id === active.id ? 'primary' : '',
      on: { click: () => lakeSetTab(t.id) },
    }, t.label || t.id)),
  );

  const grid = el('div', { class: 'lake-grid' },
    ...((active.panels || []).map((p) => renderLakePanel(active.id, p))),
  );

  return el('div', {}, tabBar, grid);
}

function renderLakePanel(tabId, panel) {
  const key = tabId + '/' + panel.id;
  const sizeCls = 'lake-panel-' + (panel.size || 'medium');
  const head = el('div', { class: 'row', style: 'justify-content: space-between; margin-bottom: 8px;' },
    el('span', {}, panel.title || panel.id),
    el('span', { class: 'meta' }, panel.type || ''),
  );
  let body;
  if (panel.type === 'sql_editor') {
    body = renderLakeSqlEditor();
  } else if (state.lake.panelErrors[key]) {
    body = el('div', { class: 'err' }, state.lake.panelErrors[key]);
  } else if (!state.lake.panelData[key]) {
    body = el('div', { class: 'meta' }, 'Loading…');
  } else {
    const res = state.lake.panelData[key];
    switch (panel.type) {
      case 'table':       body = renderLakeTable(res); break;
      case 'kpi':         body = renderLakeKPI(res, panel); break;
      case 'line_chart':  body = renderLakeChartHolder(key, res, panel, 'line'); break;
      case 'bar_chart':   body = renderLakeChartHolder(key, res, panel, 'bar'); break;
      case 'pie_chart':   body = renderLakeChartHolder(key, res, panel, 'pie'); break;
      default:            body = el('div', { class: 'err' }, 'Unknown panel type: ' + panel.type);
    }
  }
  return el('div', { class: 'card lake-panel ' + sizeCls }, head, body);
}

function renderLakeTable(res) {
  if (!res.rows || res.rows.length === 0) return el('div', { class: 'empty' }, 'No rows.');
  const thead = el('thead', {}, el('tr', {}, ...res.columns.map((c) => el('th', {}, c.name))));
  const tbody = el('tbody', {}, ...res.rows.map((row) => el('tr', {},
    ...row.map((v) => el('td', {}, lakeFormatCell(v))))));
  const meta = res.truncated
    ? el('div', { class: 'meta', style: 'margin-top: 6px;' }, 'Truncated at ' + res.row_count + ' rows.')
    : null;
  return el('div', {}, el('table', {}, thead, tbody), meta);
}

function renderLakeKPI(res, panel) {
  const valCol = res.columns.findIndex((c) => c.name === panel.value);
  const row = res.rows[0] || [];
  const raw = valCol >= 0 ? row[valCol] : null;
  const fmt = panel.format || lakeInferFormat(panel.value);
  return el('div', { style: 'padding: 10px 4px;' },
    el('div', { style: 'font-size: 28px; font-weight: 600;' }, lakeFormatWithKind(raw, fmt)),
    el('div', { class: 'meta', style: 'margin-top: 4px;' }, panel.value),
  );
}

// renderLakeChartHolder mounts a div + queues an ECharts setOption call
// after the DOM is in. We schedule via requestAnimationFrame because el()
// nodes aren't in the live DOM until shell() appends them.
function renderLakeChartHolder(key, res, panel, kind) {
  const holder = el('div', { class: 'lake-chart-holder', style: 'height: 280px;' });
  requestAnimationFrame(() => {
    if (!window.echarts) {
      holder.innerHTML = '<div class="meta" style="padding:10px;">ECharts vendor missing — chart unavailable. Tables and KPIs are unaffected.</div>';
      return;
    }
    // Reuse existing instance for this panel if present, else init.
    let chart = state.lake.chartInstances[key];
    if (!chart || chart.isDisposed && chart.isDisposed()) {
      chart = echarts.init(holder, null, { renderer: 'canvas' });
      state.lake.chartInstances[key] = chart;
    }
    const opt = lakeChartOption(res, panel, kind);
    chart.setOption(opt, true);
    // ResizeObserver keeps the chart sharp on container resize.
    if (!holder._lakeResizeObs) {
      const ro = new ResizeObserver(() => chart.resize());
      ro.observe(holder);
      holder._lakeResizeObs = ro;
    }
  });
  return holder;
}

function lakeChartOption(res, panel, kind) {
  const cssText = (cssVar, fallback) => {
    try {
      const v = getComputedStyle(document.body).getPropertyValue(cssVar).trim();
      return v || fallback;
    } catch { return fallback; }
  };
  const fg = cssText('--fg', '#e8eaed');
  const muted = cssText('--muted', '#8a93a3');
  if (kind === 'pie') {
    const labelCol = res.columns.findIndex((c) => c.name === panel.label);
    const valueCol = res.columns.findIndex((c) => c.name === panel.value);
    const data = res.rows.map((r) => ({
      name: String(r[labelCol]),
      value: r[valueCol],
    }));
    return {
      backgroundColor: 'transparent',
      textStyle: { color: fg },
      tooltip: { trigger: 'item' },
      series: [{ type: 'pie', radius: '70%', data, label: { color: fg } }],
    };
  }
  // line/bar share xs+series shape
  const xCol = res.columns.findIndex((c) => c.name === panel.x);
  const yKeys = Array.isArray(panel.y) ? panel.y : [panel.y];
  const xs = res.rows.map((r) => String(r[xCol]));
  const series = yKeys.map((y) => {
    const yi = res.columns.findIndex((c) => c.name === y);
    return {
      name: y,
      type: kind,
      smooth: kind === 'line',
      stack: kind === 'bar' && panel.stacked ? 'total' : null,
      data: res.rows.map((r) => yi < 0 ? null : r[yi]),
    };
  });
  return {
    backgroundColor: 'transparent',
    textStyle: { color: fg },
    tooltip: { trigger: 'axis' },
    legend: { data: yKeys, textStyle: { color: fg } },
    grid: { left: 50, right: 20, top: 30, bottom: 40 },
    xAxis: { type: 'category', data: xs, axisLine: { lineStyle: { color: muted } } },
    yAxis: { type: 'value', axisLine: { lineStyle: { color: muted } } },
    series,
  };
}

function renderLakeSqlEditor() {
  const ta = el('textarea', {
    style: 'width: 100%; min-height: 120px;',
    placeholder: 'SELECT 1; -- read-only. SELECT/WITH/SHOW/PRAGMA only.',
  });
  ta.value = state.lake.explorerSQL || '';
  ta.addEventListener('input', () => { state.lake.explorerSQL = ta.value; });

  const runBtn = el('button', {
    class: 'primary',
    on: { click: async () => {
      state.lake.explorerError = null;
      state.lake.explorerResult = null;
      try {
        const r = await api('/v1/lake/exec', { method: 'POST', body: { sql: ta.value, max_rows: 500 } });
        state.lake.explorerResult = r;
      } catch (e) {
        state.lake.explorerError = e.message;
      }
      render();
    } },
  }, 'Run');

  const out = el('div', { style: 'margin-top: 10px;' });
  if (state.lake.explorerError) {
    out.appendChild(el('div', { class: 'err' }, state.lake.explorerError));
  } else if (state.lake.explorerResult) {
    const res = state.lake.explorerResult;
    out.appendChild(el('div', { class: 'meta' },
      res.row_count + ' rows · ' + res.elapsed_ms + ' ms' + (res.truncated ? ' · TRUNCATED' : '')));
    out.appendChild(renderLakeTable(res));
  }
  return el('div', {}, ta, el('div', { class: 'row', style: 'margin-top: 6px;' }, runBtn), out);
}

function lakeFormatCell(v) {
  if (v == null) return '';
  if (typeof v === 'number') return v.toLocaleString();
  return String(v);
}

function lakeInferFormat(name) {
  if (!name) return 'plain';
  const n = String(name).toLowerCase();
  if (/(balance|worth|asset|liabilit|amount|paid|billed|value|pnl|outstanding)/.test(n)) return 'currency_inr';
  if (/(count|n_|num_|qty)/.test(n)) return 'count';
  return 'plain';
}

function lakeFormatWithKind(v, kind) {
  if (v == null) return '—';
  if (kind === 'currency_inr') {
    const n = Number(v); if (!isFinite(n)) return '—';
    try {
      return new Intl.NumberFormat('en-IN', {
        style: 'currency', currency: 'INR', maximumFractionDigits: 0,
      }).format(n);
    } catch { return '₹' + Math.round(n).toLocaleString(); }
  }
  if (kind === 'count') return Number(v).toLocaleString();
  return lakeFormatCell(v);
}

// ---- OAuth integration (remote MCPs) ---------------------------------------

function isHTTPUpstream(s) {
  return s.transport === 'http' || s.transport === 'streamable-http' || (!s.transport && s.url);
}

function oauthBadge(s) {
  if (!isHTTPUpstream(s)) return el('span', { class: 'meta' }, '—');
  const st = (state.oauthStatus || {})[s.name];
  if (!st) return el('span', { class: 'meta' }, 'unknown');
  if (st.is_pat) return el('span', { class: 'badge allowed', title: 'personal access token' }, 'PAT');
  if (!st.has_client) return el('span', { class: 'meta' }, 'none');
  if (!st.has_token) return el('span', { class: 'badge', title: 'client registered, no token yet' }, 'no token');
  if (st.state === 'active') {
    let title = 'authorized';
    if (st.access_expires_at) title += ' — expires ' + new Date(st.access_expires_at).toLocaleString();
    return el('span', { class: 'badge allowed', title }, 'active');
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

  let body;
  if (!st.has_client && !st.is_pat) body = sectionDiscover;
  else if (st.state === 'active' && !f.authorize_url) body = sectionConnected;
  else body = sectionAuthorize;

  return el('div', { class: 'modal-bg', on: { click: (e) => { if (e.target === e.currentTarget) closeOAuthPanel(); } }},
    el('div', { class: 'modal' },
      el('h3', {}, 'Authorize ', f.name),
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

// ---- Call (voice live agent) ------------------------------------------------
//
// Browser side of the /v1/voice/ws WebSocket. Module-scope handles
// (voiceClient) hold the WS, AudioContext, MediaStream and AudioWorklet
// node so they survive re-render. state.call mirrors what the panel
// needs to show.

const voiceClient = {
  ws: null,
  ctx: null,                // AudioContext (output, 24 kHz to match Gemini)
  micCtx: null,             // AudioContext (input — separate, runs at hardware rate)
  micStream: null,          // MediaStream from getUserMedia
  workletNode: null,
  micSource: null,
  nextPlaybackAt: 0,        // scheduling clock for AudioBufferSource chain
  // BTR11 / Bluetooth-headset hardware button support. macOS Now
  // Playing only recognises HTMLMediaElement playback from a real file
  // source (URL/Blob), not a MediaStream — so we synthesize a silent
  // WAV blob and loop it through a hidden <audio> tag. That puts us on
  // the OS media-key bus where BTR11's play/pause posts events.
  silentAudioEl: null,      // <audio> tag playing the silent blob loop
  silentAudioURL: null,     // object URL we created — revoked on teardown
  // Music ducking ("Path A"): an audible pink-noise loop routed through
  // the AudioContext destination during mic-live. The level is well
  // below speech (~-50 dBFS) so it sits under conversation, but it's
  // measurable enough that the OS treats us as "playing media" and
  // cooperative apps (Spotify, Apple Music with their auto-pause prefs
  // on) voluntarily pause themselves while we're hot. Muted → gain
  // ramps to 0; unmuted → ramps back up.
  duckNoiseSrc: null,       // AudioBufferSourceNode (looping)
  duckGain: null,           // GainNode whose .gain we ramp on mute
  // Downstream audio arrives as 24 kHz mono int16 (Gemini's native rate).
  // We schedule chunks back-to-back on the AudioContext clock so playback
  // never gaps; the engine resamples to whatever the output hardware
  // wants (LDAC over BTR11 typically targets 96 kHz).
};

async function startCall() {
  if (state.call.active) return;
  state.call.error = '';
  state.call.activeCallElsewhere = null;
  state.call.phase = 'connecting';
  state.call.transcript = [];
  state.call.toolCalls = [];
  render();

  // 0) Preflight: the WebSocket constructor swallows HTTP response
  //    bodies, so a 409 from the WS upgrade can't carry the active
  //    call ID back to us. Hit /v1/voice/sessions first; if anything
  //    is open, surface the "hang up the other one?" prompt without
  //    even touching the mic.
  try {
    const r = await api('/v1/voice/sessions');
    if (r && r.sessions && r.sessions.length > 0) {
      state.call.phase = 'idle';
      state.call.activeCallElsewhere = { call_id: r.sessions[0].id };
      render();
      return;
    }
  } catch (e) {
    // 503 from voice-disabled or auth failure — surface the message and
    // bail without prompting for mic.
    state.call.phase = 'error';
    state.call.error = e.message;
    render();
    return;
  }

  // 1) Mic permission + capture.
  //
  // BTR11 (and any closed-back Bluetooth amp/DAC) has no acoustic
  // feedback path — output goes to wired headphones plugged into the
  // amp, mic is the device's built-in MEMS or the user's inline mic.
  // So echoCancellation actively hurts: it adds latency and can carve
  // out frequencies that aren't echoing in the first place. We ask the
  // browser to skip it, keeping NS+AGC because those still help voice.
  // sampleRate hint nudges Chrome toward 16 kHz capture (matches what
  // Gemini wants on the wire); browsers free to ignore.
  //
  // navigator.mediaDevices is undefined on iOS Safari (and stricter
  // Chrome builds) when the page isn't a "secure context" — i.e. not
  // HTTPS and not localhost. We detect that explicitly so the user
  // sees an actionable hint instead of the cryptic stock error.
  if (!window.isSecureContext || !navigator.mediaDevices ||
      typeof navigator.mediaDevices.getUserMedia !== 'function') {
    state.call.phase = 'error';
    state.call.permission = 'denied';
    state.call.error =
      'microphone API unavailable on this origin (' + location.origin + '). ' +
      'iOS Safari and most browsers refuse mic access unless the page is ' +
      'served over HTTPS or from localhost. Open the dashboard over an ' +
      'HTTPS tunnel (Tailscale Serve, Cloudflare Tunnel, or a local cert) ' +
      'and try again.';
    render();
    return;
  }
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: {
      channelCount: 1,
      echoCancellation: false,
      noiseSuppression: true,
      autoGainControl: true,
      sampleRate: { ideal: 16000 },
      sampleSize: 16,
    }, video: false });
  } catch (e) {
    state.call.phase = 'error';
    state.call.permission = 'denied';
    state.call.error = 'microphone permission denied: ' + e.message;
    render();
    return;
  }
  voiceClient.micStream = stream;
  state.call.permission = 'granted';

  // 2) AudioContext for capture + worklet. Native rate (usually 48 kHz);
  //    the worklet downsamples to 16 kHz.
  const InputCtx = window.AudioContext || window.webkitAudioContext;
  voiceClient.micCtx = new InputCtx();
  try {
    await voiceClient.micCtx.audioWorklet.addModule('/voice-worklet.js');
  } catch (e) {
    state.call.phase = 'error';
    state.call.error = 'audio worklet load failed: ' + e.message;
    hangUpLocal();
    render();
    return;
  }
  voiceClient.micSource = voiceClient.micCtx.createMediaStreamSource(stream);
  voiceClient.workletNode = new AudioWorkletNode(voiceClient.micCtx, 'voice-capture');
  voiceClient.micSource.connect(voiceClient.workletNode);
  // Worklet doesn't have an audio output we care about, but Chrome
  // requires a sink for the graph to actually pump. Connect to a muted
  // gain so the loop spins without echoing the mic back.
  const muted = voiceClient.micCtx.createGain();
  muted.gain.value = 0;
  voiceClient.workletNode.connect(muted);
  muted.connect(voiceClient.micCtx.destination);

  // 3) Output AudioContext. 24 kHz matches Gemini's native output rate
  //    so we can hand AudioBuffers in unchanged; the engine resamples up
  //    to whatever the output device wants (BTR11 over LDAC typically
  //    runs 96 kHz/24-bit, so we hand off as much upstream fidelity as
  //    possible).
  const OutputCtx = window.AudioContext || window.webkitAudioContext;
  voiceClient.ctx = new OutputCtx({ sampleRate: 24000, latencyHint: 'interactive' });
  voiceClient.nextPlaybackAt = 0;

  // 3a) Anchor a MediaSession so the BTR11's play/pause button (and any
  //     other Bluetooth headset's transport buttons) route to us. We
  //     attach a silent looping AudioBufferSource to an <audio> element
  //     via MediaStreamDestination — the browser sees a media element
  //     "playing audio" and registers our app on the OS media key bus.
  setupMediaSessionAnchor();
  // 3b) Audible duck anchor: low-level pink noise routed through the
  //     output graph. Tickles the OS audio session so cooperating apps
  //     auto-pause. Honors state.call.duck (default on).
  if (state.call.duck) setupDuckAnchor();

  // 4) WebSocket — same origin, same cookie auth as the rest of the API.
  const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(wsProto + '//' + location.host + '/v1/voice/ws');
  ws.binaryType = 'arraybuffer';
  voiceClient.ws = ws;

  // Mic frames from the worklet → WS binary frames. Drop frames if the
  // socket isn't open yet (a few are normal during the handshake).
  voiceClient.workletNode.port.onmessage = (e) => {
    if (state.call.mute) return;
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(e.data);
    }
  };

  ws.onopen = () => {
    ws.send(JSON.stringify({ type: 'start' }));
  };
  ws.onmessage = (e) => {
    if (e.data instanceof ArrayBuffer) {
      scheduleVoicePlayback(e.data);
      return;
    }
    try {
      const msg = JSON.parse(e.data);
      handleVoiceServerMsg(msg);
    } catch (_) { /* ignore */ }
  };
  ws.onerror = () => {
    state.call.error = 'websocket error';
  };
  ws.onclose = (e) => {
    // Server hangs up after 409 with code 1008 (policy violation). The
    // ws.onmessage handler will already have populated activeCallElsewhere.
    if (e.code === 1008 && !state.call.activeCallElsewhere) {
      state.call.error = e.reason || 'duplicate session';
    }
    hangUpLocal();
    render();
  };

  state.call.active = true;
}

function handleVoiceServerMsg(msg) {
  switch (msg.type) {
    case 'state':
      state.call.phase = msg.value || 'listening';
      if (msg.call_id) state.call.callId = msg.call_id;
      break;
    case 'transcript':
      state.call.transcript.push({ role: msg.role || 'assistant', text: msg.text || '' });
      // Cap transcript so a long call doesn't bloat the DOM.
      if (state.call.transcript.length > 200) {
        state.call.transcript = state.call.transcript.slice(-200);
      }
      break;
    case 'tool_call':
      state.call.toolCalls.push({ name: msg.name, args: msg.args });
      if (state.call.toolCalls.length > 50) {
        state.call.toolCalls = state.call.toolCalls.slice(-50);
      }
      break;
    case 'error':
      state.call.error = msg.message || 'unknown error';
      state.call.phase = 'error';
      break;
    case 'hangup':
      state.call.error = msg.reason || '';
      break;
  }
  render();
}

// setupMediaSessionAnchor anchors a Now Playing session so the BTR11's
// hardware play/pause routes to our MediaSession handlers instead of
// falling through to Siri / the OS media-key default. Async because we
// only want to claim the anchor was successful after the <audio>
// element actually fires 'playing' — the macOS Now Playing service is
// registered at that point, not at .play() invocation. Idempotent.
//
// macOS specifics that bit us:
//   * Stream-sourced media (createMediaStreamDestination) is NOT enough
//     — Safari and Chrome both refuse to register Now Playing for it.
//   * Pure-zero silent WAVs sometimes register, sometimes don't —
//     Chrome's media-focus tracker uses a "really playing audio?"
//     heuristic. We dither the buffer at -78 dBFS so it's literally
//     inaudible (one LSB worth of signal) but unambiguously non-silent.
//   * The metadata must be set BEFORE play() on Safari; setting it
//     after play() works in Chrome but Safari silently no-ops.
//   * Some macOS releases keep routing media keys to the previously-
//     active media app (Music, Spotify) until *they* are paused. If
//     hardwareReady stays false even after the anchor reports playing,
//     that's the path to check.
async function setupMediaSessionAnchor() {
  if (voiceClient.silentAudioEl) return;

  // 1) Tell the OS who we are *before* play() — Safari requirement.
  //    Register action handlers in the same gesture frame so the
  //    Now Playing slot is fully armed the instant the audio starts.
  if ('mediaSession' in navigator) {
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title:  'toolyard live call',
        artist: 'toolyard',
        album:  'voice control',
      });
      navigator.mediaSession.setActionHandler('pause', () => toggleMuteFromHardware('muted'));
      navigator.mediaSession.setActionHandler('play',  () => toggleMuteFromHardware('live'));
      navigator.mediaSession.setActionHandler('stop',  () => endCall());
    } catch { /* older browsers — fall back silently */ }
  }

  // 2) Synthesize a 5-second 8 kHz mono WAV with 1-LSB dither so it
  //    reads as "real audio" to every browser's media-focus heuristic.
  const blob = createSilentWavBlob(5.0);
  const url = URL.createObjectURL(blob);
  const audio = document.createElement('audio');
  audio.src = url;
  audio.loop = true;
  audio.preload = 'auto';
  audio.autoplay = true;
  audio.controls = false;
  audio.style.display = 'none';
  // Inaudible to a human but non-zero amplitude. Setting volume=0 or
  // .muted=true makes Safari skip Now Playing registration.
  audio.volume = 0.02;
  audio.disableRemotePlayback = true;
  document.body.appendChild(audio);
  voiceClient.silentAudioEl = audio;
  voiceClient.silentAudioURL = url;

  // 3) Listen for the actual 'playing' event so we know the OS has
  //    registered our Now Playing slot. If we don't see it within
  //    1.5 s, surface that in the UI so the user can investigate
  //    (autoplay blocked, another app holding focus, etc.).
  let playingFired = false;
  audio.addEventListener('playing', () => {
    playingFired = true;
    state.call.hardwareReady = true;
    state.call.hardwareWhy = '';
    if ('mediaSession' in navigator) {
      try { navigator.mediaSession.playbackState = 'playing'; } catch {}
    }
    render();
  });

  try {
    await audio.play();
  } catch (e) {
    state.call.hardwareReady = false;
    state.call.hardwareWhy = 'autoplay blocked: ' + (e.message || e.name || 'unknown');
    render();
    return;
  }
  // If play() resolved but 'playing' didn't fire within 1.5s, something
  // else is in the way (e.g. another app owns Now Playing on macOS).
  setTimeout(() => {
    if (!playingFired) {
      state.call.hardwareReady = false;
      state.call.hardwareWhy = 'Now Playing not claimed — quit Music/Spotify if open, or try Cmd-Shift-R to hard-refresh.';
      render();
    }
  }, 1500);
}

function teardownMediaSessionAnchor() {
  if ('mediaSession' in navigator) {
    try {
      navigator.mediaSession.setActionHandler('play',  null);
      navigator.mediaSession.setActionHandler('pause', null);
      navigator.mediaSession.setActionHandler('stop',  null);
      navigator.mediaSession.playbackState = 'none';
      navigator.mediaSession.metadata = null;
    } catch {}
  }
  if (voiceClient.silentAudioEl) {
    try { voiceClient.silentAudioEl.pause(); } catch {}
    try { voiceClient.silentAudioEl.removeAttribute('src'); } catch {}
    try { voiceClient.silentAudioEl.load(); } catch {}
    try { voiceClient.silentAudioEl.remove(); } catch {}
    voiceClient.silentAudioEl = null;
  }
  if (voiceClient.silentAudioURL) {
    try { URL.revokeObjectURL(voiceClient.silentAudioURL); } catch {}
    voiceClient.silentAudioURL = null;
  }
}

// createSilentWavBlob returns a tiny mono 8 kHz 16-bit silent WAV. Used
// solely as the Now Playing anchor — content is 0-valued samples so it's
// inaudible even at volume 1.0; we still play it at volume ~0.001 for
// extra paranoia on browsers that scan the buffer.
function createSilentWavBlob(durationSec) {
  const rate = 8000;
  const numFrames = Math.max(1, Math.floor(rate * durationSec));
  const dataBytes = numFrames * 2;
  const buf = new ArrayBuffer(44 + dataBytes);
  const view = new DataView(buf);
  let p = 0;
  const wstr = (s) => { for (let i = 0; i < s.length; i++) view.setUint8(p++, s.charCodeAt(i)); };
  const u32 = (n) => { view.setUint32(p, n, true); p += 4; };
  const u16 = (n) => { view.setUint16(p, n, true); p += 2; };
  wstr('RIFF'); u32(36 + dataBytes); wstr('WAVE');
  wstr('fmt '); u32(16); u16(1); u16(1); u32(rate); u32(rate * 2); u16(2); u16(16);
  wstr('data'); u32(dataBytes);
  // Dither the samples at 1 LSB so the buffer reads as "real audio" to
  // browser media-focus heuristics without being audible (1 LSB at 16-
  // bit is ~-96 dBFS; combined with our 2% gain that's ~-130 dBFS at
  // the speakers — well below the noise floor of any DAC).
  for (let i = 0; i < numFrames; i++) {
    view.setInt16(44 + i * 2, (i & 1) ? 1 : -1, true);
  }
  return new Blob([buf], { type: 'audio/wav' });
}

// toggleMuteFromHardware is the MediaSession-side handler. The optional
// `intent` argument matches what the user pressed: 'muted' means "they
// pressed pause", 'live' means "they pressed play". On a BTR11 single-
// button toggle, only one of the two fires per press depending on the
// browser's current playbackState — we flip state.call.mute accordingly
// and play the appropriate cue.
function toggleMuteFromHardware(intent) {
  if (!state.call.active) return;
  const wantMuted = intent === 'muted' ? true : intent === 'live' ? false : !state.call.mute;
  if (state.call.mute === wantMuted) return;
  state.call.mute = wantMuted;
  if (voiceClient.ws && voiceClient.ws.readyState === WebSocket.OPEN) {
    try { voiceClient.ws.send(JSON.stringify({ type: 'mute', mute: wantMuted })); } catch {}
  }
  // Ramp the duck anchor down on mute → music auto-resumes; back up
  // on unmute → music auto-pauses.
  rampDuckGain(wantMuted ? 0 : DUCK_LIVE_GAIN);
  playMuteCue(wantMuted ? 'muted' : 'live');
  // Keep MediaSession state coherent so the next button press fires the
  // opposite handler (the BTR11's single button toggles).
  if ('mediaSession' in navigator) {
    try {
      navigator.mediaSession.playbackState = wantMuted ? 'paused' : 'playing';
    } catch {}
  }
  render();
}

// playMuteCue emits a short two-tone chirp through the output graph so
// the user gets unambiguous audible confirmation that the mic state
// changed — important on a hardware button press where there's no
// visual cue if the dashboard isn't in front.
//
//   'muted': descending (880 → 440 Hz) — "going to sleep"
//   'live' : ascending  (440 → 880 Hz) — "waking up, you're hot"
function playMuteCue(state) {
  const ctx = voiceClient.ctx;
  if (!ctx) return;
  const tones = state === 'muted' ? [880, 440] : [440, 880];
  const each = 0.06; // 60 ms per tone
  const now = ctx.currentTime;
  tones.forEach((freq, i) => {
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = 'sine';
    osc.frequency.value = freq;
    const t0 = now + i * each;
    // Short attack + release envelope so the cue doesn't click.
    gain.gain.setValueAtTime(0, t0);
    gain.gain.linearRampToValueAtTime(0.18, t0 + 0.008);
    gain.gain.setValueAtTime(0.18, t0 + each - 0.012);
    gain.gain.linearRampToValueAtTime(0, t0 + each);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start(t0);
    osc.stop(t0 + each + 0.02);
  });
}

// Target gain for the duck anchor when mic is live. -50 dBFS ≈ 0.003.
// Subjectively well below speech but unambiguously non-silent so the OS
// audio session activates. Bumping this higher makes more apps notice
// (some have a higher threshold) at the cost of perceptible hiss.
const DUCK_LIVE_GAIN = 0.003;

// setupDuckAnchor creates the pink-noise source + gain node and wires
// it into ctx.destination. Idempotent; bails if already running or the
// AudioContext isn't ready.
function setupDuckAnchor() {
  const ctx = voiceClient.ctx;
  if (!ctx || voiceClient.duckNoiseSrc) return;

  // 3 seconds of pink noise (Voss-McCartney approximation) baked into a
  // looping buffer. 3 s is long enough that the loop seam isn't a
  // perceptible click; short enough that buffer alloc is instant.
  const seconds = 3;
  const buf = ctx.createBuffer(1, ctx.sampleRate * seconds, ctx.sampleRate);
  const data = buf.getChannelData(0);
  let b0 = 0, b1 = 0, b2 = 0, b3 = 0, b4 = 0, b5 = 0, b6 = 0;
  for (let i = 0; i < data.length; i++) {
    const w = Math.random() * 2 - 1;
    b0 = 0.99886 * b0 + w * 0.0555179;
    b1 = 0.99332 * b1 + w * 0.0750759;
    b2 = 0.96900 * b2 + w * 0.1538520;
    b3 = 0.86650 * b3 + w * 0.3104856;
    b4 = 0.55000 * b4 + w * 0.5329522;
    b5 = -0.7616 * b5 - w * 0.0168980;
    data[i] = (b0 + b1 + b2 + b3 + b4 + b5 + b6 + w * 0.5362) * 0.11;
    b6 = w * 0.115926;
  }

  const src = ctx.createBufferSource();
  src.buffer = buf;
  src.loop = true;
  const gain = ctx.createGain();
  gain.gain.value = state.call.mute ? 0 : DUCK_LIVE_GAIN;
  src.connect(gain);
  gain.connect(ctx.destination);
  src.start();
  voiceClient.duckNoiseSrc = src;
  voiceClient.duckGain = gain;
}

function teardownDuckAnchor() {
  if (voiceClient.duckNoiseSrc) {
    try { voiceClient.duckNoiseSrc.stop(); } catch {}
    try { voiceClient.duckNoiseSrc.disconnect(); } catch {}
    voiceClient.duckNoiseSrc = null;
  }
  if (voiceClient.duckGain) {
    try { voiceClient.duckGain.disconnect(); } catch {}
    voiceClient.duckGain = null;
  }
}

// rampDuckGain transitions the duck noise volume smoothly. Linear over
// 50 ms — long enough to avoid a click, short enough that the cue +
// gain change feel simultaneous from the user's perspective.
function rampDuckGain(target) {
  if (!voiceClient.duckGain || !voiceClient.ctx) return;
  const t = voiceClient.ctx.currentTime;
  const g = voiceClient.duckGain.gain;
  try {
    g.cancelScheduledValues(t);
    g.setValueAtTime(g.value, t);
    g.linearRampToValueAtTime(target, t + 0.05);
  } catch {
    g.value = target;
  }
}

function scheduleVoicePlayback(buf) {
  const ctx = voiceClient.ctx;
  if (!ctx) return;
  const i16 = new Int16Array(buf);
  if (i16.length === 0) return;
  // int16 → float32 [-1, 1]
  const f32 = new Float32Array(i16.length);
  for (let i = 0; i < i16.length; i++) {
    f32[i] = i16[i] < 0 ? i16[i] / 0x8000 : i16[i] / 0x7fff;
  }
  const ab = ctx.createBuffer(1, f32.length, 24000);
  ab.copyToChannel(f32, 0);
  const src = ctx.createBufferSource();
  src.buffer = ab;
  src.connect(ctx.destination);
  // Schedule back-to-back. If we've slipped behind real time (network
  // hiccup), restart from "now" so we don't lag forever; the user will
  // hear a tiny gap once.
  const now = ctx.currentTime;
  if (voiceClient.nextPlaybackAt < now) voiceClient.nextPlaybackAt = now;
  src.start(voiceClient.nextPlaybackAt);
  voiceClient.nextPlaybackAt += f32.length / 24000;
}

async function endCall() {
  if (voiceClient.ws && voiceClient.ws.readyState === WebSocket.OPEN) {
    try { voiceClient.ws.send(JSON.stringify({ type: 'hangup' })); } catch {}
    try { voiceClient.ws.close(1000, 'user'); } catch {}
  } else {
    // No live WS but flag may be stale (server-side); call hangup REST
    // so a leftover server-side session is cleaned up cleanly.
    try { await api('/v1/voice/hangup', { method: 'POST', body: {} }); } catch {}
  }
  hangUpLocal();
  render();
}

function hangUpLocal() {
  teardownMediaSessionAnchor();
  teardownDuckAnchor();
  if (voiceClient.workletNode) {
    try { voiceClient.workletNode.port.onmessage = null; } catch {}
    try { voiceClient.workletNode.disconnect(); } catch {}
    voiceClient.workletNode = null;
  }
  if (voiceClient.micSource) {
    try { voiceClient.micSource.disconnect(); } catch {}
    voiceClient.micSource = null;
  }
  if (voiceClient.micStream) {
    voiceClient.micStream.getTracks().forEach((t) => t.stop());
    voiceClient.micStream = null;
  }
  if (voiceClient.micCtx) {
    try { voiceClient.micCtx.close(); } catch {}
    voiceClient.micCtx = null;
  }
  if (voiceClient.ctx) {
    try { voiceClient.ctx.close(); } catch {}
    voiceClient.ctx = null;
  }
  voiceClient.ws = null;
  voiceClient.nextPlaybackAt = 0;
  state.call.active = false;
  state.call.phase = 'idle';
  state.call.callId = null;
  state.call.mute = false;
  state.call.hardwareReady = false;
  state.call.hardwareWhy = '';
}

async function hangUpOtherAndStart() {
  try { await api('/v1/voice/hangup', { method: 'POST', body: {} }); } catch (e) {
    state.call.error = e.message; render(); return;
  }
  state.call.activeCallElsewhere = null;
  await startCall();
}

function viewCall() {
  const c = state.call;
  const phaseColor = {
    idle:        '#888',
    connecting:  '#f0c75e',
    listening:   '#4caf50',
    thinking:    '#5b8def',
    speaking:    '#b87bff',
    error:       '#e06060',
  }[c.phase] || '#888';

  const intro = el('p', { class: 'meta' },
    'Talk to Toolyard through Gemini Live. Mic audio streams from this device ' +
    'to the gateway, and tool calls run through the same policy, approval, and ' +
    'audit path as enrolled agents. Bluetooth headset play/pause (e.g. FiiO BTR11) toggles mute; you\'ll hear ' +
    'a descending chirp when muted, ascending when live again. With music auto-' +
    'pause on, a faint masking tone plays while you\'re live so cooperating ' +
    'apps (Spotify, Music) auto-pause themselves; muting silences the tone ' +
    'and they resume.');

  const phasePill = el('div', { class: 'row', style: 'align-items: center; gap: 8px; margin: 8px 0; flex-wrap: wrap;' },
    el('span', { class: 'mic-dot', style: 'background:' + phaseColor }),
    el('strong', {}, c.phase.toUpperCase()),
    c.callId ? el('span', { class: 'meta' }, '· ' + c.callId.slice(0, 8)) : null,
    c.active && c.duck ? el('span', { class: 'meta' }, '🎵 masking on') : null,
    c.active ? el('span', { class: 'meta', style: 'margin-left: auto;' },
      c.hardwareReady
        ? '🎛 BTR11 button armed'
        : (c.hardwareWhy ? '⚠ ' + c.hardwareWhy : '⚠ BTR11 button not armed yet')
    ) : null,
  );

  // Duck preference toggle. Honored at next call start — toggling
  // mid-call won't switch the anchor on/off (cheap restart by ending
  // and re-calling if you really want to).
  const duckRow = c.active ? null : el('label', {
    class: 'row',
    style: 'gap: 8px; align-items: center; margin: 6px 0 0;',
  },
    el('input', {
      type: 'checkbox',
      checked: !!c.duck,
      on: { change: (e) => {
        state.call.duck = e.target.checked;
        try { localStorage.setItem('toolyard.call.duck', e.target.checked ? '1' : '0'); } catch {}
      }},
    }),
    el('span', {}, 'Auto-pause background music while my mic is live (plays a soft masking tone)'),
  );

  const errBox = c.error
    ? el('div', { class: 'err', style: 'margin: 8px 0;' }, c.error)
    : null;

  // 409 — server says another call is active for this user.
  const dupBox = c.activeCallElsewhere
    ? el('div', { class: 'card', style: 'border-color: #c2853f;' },
        el('h3', {}, 'A call is already active'),
        el('p', { class: 'meta' }, 'You\'ve got an open call elsewhere (call ' + c.activeCallElsewhere.call_id.slice(0,8) + '). End it and start fresh?'),
        el('div', { class: 'row' },
          el('button', { class: 'primary', on: { click: hangUpOtherAndStart } }, 'Hang up & start new'),
          el('button', { on: { click: () => { state.call.activeCallElsewhere = null; render(); }}}, 'Cancel'),
        ),
      )
    : null;

  const buttons = c.active
    ? el('div', { class: 'row' },
        el('button', {
          class: c.mute ? 'primary' : '',
          on: { click: () => toggleMuteFromHardware(c.mute ? 'live' : 'muted') },
        }, c.mute ? 'Unmute' : 'Mute'),
        el('button', { class: 'danger', on: { click: endCall } }, 'End call'),
      )
    : el('div', { class: 'row' },
        el('button', {
          class: 'primary',
          style: 'min-height: 56px; min-width: 180px; font-size: 16px;',
          on: { click: startCall },
        }, '🎙 Start call'),
      );

  const transcriptBlock = c.transcript.length === 0
    ? null
    : el('div', { class: 'card' },
        el('h3', {}, 'Transcript'),
        el('div', { class: 'transcript' },
          c.transcript.map((row) => el('div', { class: 'transcript-row ' + (row.role === 'user' ? 'user' : 'assistant') },
            el('span', { class: 'meta' }, row.role + ': '),
            el('span', {}, row.text),
          )),
        ),
      );

  const toolsBlock = c.toolCalls.length === 0
    ? null
    : el('div', { class: 'card' },
        el('h3', {}, 'Tool calls'),
        el('ul', {}, c.toolCalls.map((t) => el('li', {},
          el('code', {}, t.name),
          t.args ? el('span', { class: 'meta' }, ' ' + t.args) : null,
        ))),
      );

  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Live call'),
      intro,
      phasePill,
      errBox,
      buttons,
      duckRow,
    ),
    dupBox,
    transcriptBlock,
    toolsBlock,
  );
}


// Pre-load OAuth status for any visible upstreams.
async function preloadOAuthStatus() {
  const targets = (state.servers || []).filter(isHTTPUpstream).map((s) => s.name);
  for (const n of targets) await loadOAuthStatus(n);
}

(async () => {
  if ('serviceWorker' in navigator) {
    try { navigator.serviceWorker.register('/sw.js'); } catch {}
  }
  if (location.hash) state.route = location.hash.slice(1) || 'approvals';
  // Push deep link: notifications open /?approval=<id>. Land on the
  // approvals view so the card (or its expired/decided state) is visible.
  const approvalParam = new URLSearchParams(location.search).get('approval');
  if (approvalParam) {
    state.route = 'approvals';
    state.focusApprovalId = approvalParam;
  }
  // Events push deep link: notifications open /?route=events.
  const routeParam = new URLSearchParams(location.search).get('route');
  if (routeParam) state.route = routeParam;
  await refreshUser();
  if (state.user) {
    await loadAll(); startStream();
    if (state.route === 'insights' || state.route === 'notifications') {
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

  // While approvals are on screen, re-render every 30s so countdowns stay
  // fresh and cards that cross their expiry get disabled even if no SSE
  // event has arrived yet.
  setInterval(() => {
    if (state.user && state.route === 'approvals' && (state.approvals || []).length) {
      render();
    }
  }, 30000);

  // iOS-PWA resume path: WebKit kills the SSE socket on background without
  // firing onerror, so on re-show we proactively reconnect if the stream
  // isn't demonstrably live.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible' || !state.user) return;
    const stale = !state.stream.lastEventAt || Date.now() - state.stream.lastEventAt > 30000;
    if (state.stream.status !== 'live' || stale) reconnectStream();
  });
})();
