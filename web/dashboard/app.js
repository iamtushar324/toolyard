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
  enrollment: null,
  errors: {},
  notice: '',
  streamLive: false,
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
    throw new Error((body && body.error) || `HTTP ${r.status}`);
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
    const [pendings, audits, agents, memos, servers, tools, market, settingsRes, usageRes, vapid] = await Promise.all([
      api('/v1/approvals?status=pending'),
      api('/v1/audit?limit=50'),
      api('/v1/agents'),
      api('/v1/memory'),
      api('/v1/servers').catch(() => []),
      api('/v1/tools').catch(() => []),
      api('/v1/marketplace').catch(() => []),
      api('/v1/settings').catch(() => ({})),
      api('/v1/usage').catch(() => ({ per_tool: {}, rows: [] })),
      api('/v1/push/vapid_key').catch(() => null),
    ]);
    state.settings = Object.assign({}, state.settings, settingsRes || {});
    state.usage = usageRes || state.usage;
    state.approvals = pendings || [];
    state.audit = audits || [];
    state.agents = agents || [];
    state.memory = memos || [];
    state.servers = servers || [];
    state.tools = tools || [];
    state.marketplace = market || [];
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
function startStream() {
  if (evtSrc) try { evtSrc.close(); } catch {}
  evtSrc = new EventSource('/v1/events/stream');
  evtSrc.addEventListener('open', () => { state.streamLive = true; render(); });
  evtSrc.onerror = () => { state.streamLive = false; render(); };
  evtSrc.addEventListener('approval', (e) => { handleApprovalEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('audit', (e) => { handleAuditEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_done', (e) => { handleOAuthDone(JSON.parse(e.data)); });
  evtSrc.addEventListener('mcp_oauth_refreshed', () => { reloadServers(); render(); });
  evtSrc.addEventListener('mcp_oauth_needs_reauth', (e) => { handleOAuthReauth(JSON.parse(e.data)); });
}

function handleOAuthDone(payload) {
  const name = payload && payload.upstream;
  if (!name) return;
  if (state.oauthFlow && state.oauthFlow.name === name) {
    state.oauthFlow = null;
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

function handleAuditEvent(ev) {
  state.audit.unshift(ev);
  if (state.audit.length > 200) state.audit.length = 200;
  render();
}

async function loadInsights() {
  state.insights.loading = true;
  render();
  const range = state.insights.range;
  try {
    const [overview, tools, agents, cost, rules, anomalies] = await Promise.all([
      api('/v1/insights/overview?range=' + range).catch(() => null),
      api('/v1/insights/tools?range=' + range).catch(() => []),
      api('/v1/insights/agents?range=' + range).catch(() => []),
      api('/v1/insights/cost?range=' + range).catch(() => ({ rows: [] })),
      api('/v1/insights/auto/rules').catch(() => []),
      api('/v1/insights/anomalies?limit=50').catch(() => []),
    ]);
    state.insights.overview = overview;
    state.insights.tools = tools || [];
    state.insights.agents = agents || [];
    state.insights.cost = cost || { rows: [] };
    state.insights.autoRules = rules || [];
    state.anomalies = anomalies || [];
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

function renderApprovalBatch(agentKey, items) {
  const ids = items.map((a) => a.id);
  const earliest = Math.min(...items.map((a) => a.expires_at));
  const isBatch = items.length > 1;
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
      el('button', { class: 'primary', on: { click: () => decideBatch(ids, 'allowed') }}, `Allow all (${items.length})`),
      el('button', { class: 'danger',  on: { click: () => decideBatch(ids, 'denied')  }}, `Deny all`),
    ) : null,
  );

  const rows = items.map((a) => el('div', {
    style: 'border-top: 1px solid var(--border); padding: 12px 0; margin-top: 8px;',
  },
    el('div', { class: 'row' },
      el('span', { class: 'grow', style: 'font-weight: 500;' }, `${a.upstream_name} · ${a.tool_name}`),
      a.intent_category ? el('span', { class: 'badge' }, a.intent_category) : null,
      badge(a.status),
    ),
    el('div', { class: 'meta', style: 'margin: 4px 0 6px;' },
      `created ${relTime(a.created_at)} · expires ${relTime(a.expires_at)}`),
    el('div', { style: 'background: var(--bg); border: 1px solid var(--border); border-radius: 4px; padding: 8px; margin: 4px 0;' },
      el('div', { class: 'meta' }, 'reasoning'),
      a.reason || '(none provided)'),
    el('details', {},
      el('summary', {}, 'arguments'),
      el('pre', { class: 'json' }, JSON.stringify(a.arguments || {}, null, 2)),
    ),
    el('div', { class: 'row', style: 'margin-top: 8px;' },
      el('button', { class: 'primary', on: { click: () => decideApproval(a.id, 'allowed') } }, 'Allow'),
      el('button', { class: 'danger',  on: { click: () => decideApproval(a.id, 'denied')  } }, 'Deny'),
    ),
  ));

  return el('div', { class: 'card' }, header, ...rows);
}

async function decideApproval(id, action) {
  try {
    await api(`/v1/approvals/${id}/decide`, { method: 'POST', body: { Action: action } });
    state.approvals = state.approvals.filter((a) => a.id !== id);
    toast(action === 'allowed' ? 'Approved' : 'Denied');
    render();
  } catch (e) { toast(e.message, 'error'); }
}

async function decideBatch(ids, action) {
  if (!ids.length) return;
  try {
    const out = await api('/v1/approvals/decide-batch', { method: 'POST', body: { ids, action }});
    const flipped = (out || []).filter((r) => r.status === action).length;
    state.approvals = state.approvals.filter((a) => !ids.includes(a.id));
    toast(`${action === 'allowed' ? 'Approved' : 'Denied'} ${flipped} of ${ids.length}`);
    render();
  } catch (e) { toast(e.message, 'error'); }
}

function viewAudit() {
  if (!state.audit.length) {
    return el('div', { class: 'card empty' }, 'No events yet.');
  }
  const tbl = el('table', {},
    el('thead', {}, el('tr', {},
      el('th', {}, 'When'),
      el('th', {}, 'Event'),
      el('th', {}, 'Tool'),
      el('th', {}, 'Decision'),
      el('th', {}, 'Reasoning / Result'),
    )),
    el('tbody', {}, state.audit.map((e) => el('tr', {},
      el('td', { class: 'meta' }, relTime(e.ts)),
      el('td', {}, e.event_type),
      el('td', {}, e.tool_name ? `${e.upstream_name || ''} · ${e.tool_name}` : '—'),
      el('td', {}, e.decision || '—'),
      el('td', {}, e.reason ? el('div', {}, el('div', {}, e.reason),
        e.result_summary ? el('div', { class: 'meta', style: 'margin-top: 4px;' }, e.result_summary) : null)
        : (e.result_summary || '—')),
    ))),
  );
  return el('div', { class: 'card' }, tbl);
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
            el('th', {}, 'Name'), el('th', {}, 'ID'), el('th', {}, 'Last seen'))),
            el('tbody', {}, state.agents.map((a) => el('tr', {},
              el('td', {}, a.name),
              el('td', {}, el('code', {}, a.id)),
              el('td', { class: 'meta' }, a.last_seen ? relTime(a.last_seen) : 'never'),
            )))),
    ),
  );
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

  const snippet = m.snippetTab === 'cli' ? cli :
                  m.snippetTab === 'project' ? project : global;
  const snippetLang = m.snippetTab === 'cli' ? 'bash' : 'json';

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
              : 'Open ~/.claude.json and merge under mcpServers'),
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
      el('h2', {}, 'Entries'),
      state.memory.length === 0
        ? el('div', { class: 'empty' }, 'Memory is empty.')
        : el('table', {}, el('thead', {}, el('tr', {},
            el('th', {}, 'Scope'), el('th', {}, 'Key'), el('th', {}, 'Value'), el('th', {}, 'Updated'), el('th', {}, ''))),
            el('tbody', {}, state.memory.map((m) => el('tr', {},
              el('td', {}, m.scope),
              el('td', {}, el('code', {}, m.key)),
              el('td', {}, m.value),
              el('td', { class: 'meta' }, relTime(m.updated_at)),
              el('td', {}, el('button', { class: 'danger', on: { click: async () => {
                try {
                  await api(`/v1/memory?scope=${encodeURIComponent(m.scope)}&key=${encodeURIComponent(m.key)}`, { method: 'DELETE' });
                  await loadAll(); render();
                } catch (e) { toast(e.message, 'error'); }
              }}}, 'Delete')),
            )))),
    ),
  );
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
    el('div', { class: 'meta' }, 'Environment (KEY=VALUE per line, optional)'),
    el('textarea', { id: 'srv-env', placeholder: 'GITHUB_PERSONAL_ACCESS_TOKEN=ghp_…', value: draftEnvAsText(draft) }),
  );

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
                      : el('span', { class: 'badge allowed' }, 'installed'))
                  : el('button', {
                      class: 'primary',
                      on: { click: () => openMarketAdd(m, needsEnv) },
                    }, needsEnv ? 'Configure & add' : 'Add'),
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
            el('th', {}, ''))),
            el('tbody', {}, state.servers.map((s) => el('tr', {},
              el('td', {}, el('code', {}, s.name)),
              el('td', {}, transportLabel(s)),
              el('td', {}, String(s.tool_count || 0)),
              el('td', {}, s.last_status === 'ok'
                ? el('span', { class: 'badge allowed' }, 'connected')
                : el('span', { class: 'badge denied', title: s.last_error || '' }, s.last_status || 'error')),
              el('td', {}, oauthBadge(s)),
              el('td', {},
                el('div', { class: 'row' },
                  isHTTPUpstream(s) ? el('button', { on: { click: () => openOAuthPanel(s.name) }}, 'Auth…') : null,
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

function openMarketAdd(entry, needsEnv) {
  if (!needsEnv) {
    installFromMarket(entry, {});
    return;
  }
  state.marketModal = { entry, env: {}, name: entry.suggested_name, error: '' };
  for (const v of entry.env || []) {
    state.marketModal.env[v.name] = v.default || '';
  }
  render();
}

function closeMarketModal() {
  state.marketModal = null;
  render();
}

function renderMarketModal() {
  const { entry, env, name, error } = state.marketModal;
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
      error ? el('div', { class: 'err' }, error) : null,
      el('div', { class: 'row', style: 'margin-top: 16px; justify-content: flex-end;' },
        el('button', { on: { click: closeMarketModal } }, 'Cancel'),
        el('button', { class: 'primary', on: { click: () => {
          // Validate required env now.
          for (const v of entry.env || []) {
            if (v.required && !((state.marketModal.env[v.name] || '').trim())) {
              state.marketModal.error = v.name + ' is required';
              render();
              return;
            }
          }
          installFromMarket(entry, state.marketModal.env, state.marketModal.name);
        }}}, 'Install'),
      ),
    ),
  );
}

async function installFromMarket(entry, env, overrideName) {
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
      toast(entry.name + ' installed.');
    }
    state.marketModal = null;
    await reloadServers();
    render();
  } catch (e) {
    if (state.marketModal) { state.marketModal.error = e.message; render(); }
    else toast(e.message, 'error');
  }
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
    el('div', { class: 'card' },
      el('h2', {}, 'About'),
      el('p', {}, 'toolyard v0.1.0 — Apache-2.0.'),
      el('p', { class: 'meta' }, 'Single Go binary + SQLite. Source: ', el('code', {}, 'github.com/tusharbhardwaj/toolyard')),
    ),
  );
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
        el('button', { class: 'primary', on: { click: enablePush }},
          state.pushReady ? 'Push enabled ✓ (re-enroll)' : 'Enable push'),
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
    render();
  } catch (e) { toast(e.message, 'error'); }
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
  const renderPolicyToggle = (t) => {
    const on = !!t.auto_approve;
    const destructive = !!t.is_destructive;
    return el('label', {
      class: 'policy-toggle' + (on ? ' on' : '') + (destructive ? ' veto' : ''),
      title: destructive
        ? 'Destructive tools always require human review — the engine ignores the rule.'
        : (on ? 'Auto-approves on every call.' : 'Holds for human review.'),
    },
      el('input', { type: 'checkbox', checked: on, on: { change: async (e) => {
        const want = e.target.checked;
        try {
          const resp = await api('/v1/insights/tools/' + encodeURIComponent(t.tool_name) + '/policy', {
            method: 'POST', body: { auto_approve: want },
          });
          if (resp && resp.destructive_veto) {
            toast(t.tool_name + ': rule saved, but destructive tools still require human review.', 'warn');
          } else {
            toast(t.tool_name + ' → ' + (want ? 'auto-approve' : 'needs approval'));
          }
          loadInsights();
        } catch (err) {
          toast(err.message, 'error');
          render(); // revert the checkbox visually
        }
      }}}),
      el('span', { class: 'policy-pill' }, on ? 'Auto' : 'Ask'),
    );
  };

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
    costRows.length === 0 ? el('div', { class: 'empty' }, 'No data.') :
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
        navBtn('insights',     'Insights'),
        navBtn('notifications', 'Alerts' + (alertCount ? ' (' + alertCount + ')' : '')),
        navBtn('audit',        'Audit'),
        navBtn('servers',      'Servers'),
        navBtn('tools',        'Tools'),
        navBtn('memory',       'Memory'),
        navBtn('agents',       'Agents'),
        navBtn('settings',     'Settings'),
      ),
      el('span', { class: 'user' },
        el('span', { class: 'stream-pill ' + (state.streamLive ? 'live' : '') }),
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
        class: ['audit','memory','agents','settings','insights'].includes(state.route) ? 'active' : '',
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
      item('call',     'Call',     'Talk to Claude through Maestro — voice in, voice out'),
      item('lake',     'Lake',     'Personal data warehouse — finance, ops, daily memory'),
      item('insights', 'Insights', 'Per-tool, per-agent, cost breakdowns'),
      item('audit',    'Audit',    'Append-only event log'),
      item('memory',   'Memory',   'Scope/key-value store'),
      item('agents',   'Agents',   'Manage enrolled agents'),
      item('settings', 'Settings', 'Surface mode, auto-approval, retention'),
      el('div', { class: 'row', style: 'margin-top: 12px; justify-content: flex-end;' },
        el('button', { on: { click: () => { state.moreSheet = false; render(); } }}, 'Close'),
      ),
    ),
  );
}

function render() {
  const root = $('app') || document.body;
  root.innerHTML = '';
  if (state.setupRequired) { root.appendChild(viewSetup()); return; }
  if (!state.user) { root.appendChild(viewLogin()); return; }
  let body;
  switch (state.route) {
    case 'audit':         body = viewAudit();         break;
    case 'memory':        body = viewMemory();        break;
    case 'agents':        body = viewAgents();        break;
    case 'servers':       body = viewServers();       break;
    case 'tools':         body = viewTools();         break;
    case 'settings':      body = viewSettings();      break;
    case 'insights':      body = viewInsights();      break;
    case 'lake':          body = viewLake();          break;
    case 'call':          body = viewCall();          break;
    case 'notifications': body = viewNotifications(); break;
    default:              body = viewApprovals();
  }
  root.appendChild(shell(body));
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
        artist: 'maestro',
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
    'Talk to Claude through Maestro. Mic goes from this device to the gateway, ' +
    'through Gemini Live, into a tmux+claude session named mae-*. End the call ' +
    'and the session keeps running — `tmux attach -t mae-<name>` to take over. ' +
    'Bluetooth headset play/pause (e.g. FiiO BTR11) toggles mute; you\'ll hear ' +
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
})();
