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
  enrollment: null,
  errors: {},
  notice: '',
  streamLive: false,
  vapidKey: null,
  pushReady: false,
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
  if (init.body && typeof init.body !== 'string') {
    init.body = JSON.stringify(init.body);
    init.headers = { 'Content-Type': 'application/json', ...(init.headers || {}) };
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
    const [pendings, audits, agents, memos, vapid] = await Promise.all([
      api('/v1/approvals?status=pending'),
      api('/v1/audit?limit=50'),
      api('/v1/agents'),
      api('/v1/memory'),
      api('/v1/push/vapid_key').catch(() => null),
    ]);
    state.approvals = pendings || [];
    state.audit = audits || [];
    state.agents = agents || [];
    state.memory = memos || [];
    state.vapidKey = vapid && vapid.public_key ? vapid.public_key : null;
  } catch (e) {
    toast(e.message, 'error');
  }
}

let evtSrc = null;
function startStream() {
  if (evtSrc) try { evtSrc.close(); } catch {}
  evtSrc = new EventSource('/v1/events/stream');
  evtSrc.addEventListener('open', () => { state.streamLive = true; render(); });
  evtSrc.onerror = () => { state.streamLive = false; render(); };
  evtSrc.addEventListener('approval', (e) => { handleApprovalEvent(JSON.parse(e.data)); });
  evtSrc.addEventListener('audit', (e) => { handleAuditEvent(JSON.parse(e.data)); });
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
  return el('div', {}, state.approvals.map((a) => el('div', { class: 'card' },
    el('div', { class: 'row' },
      el('span', { class: 'grow', style: 'font-weight: 600;' },
        `${a.upstream_name} · ${a.tool_name}`),
      badge(a.status),
    ),
    el('div', { class: 'meta', style: 'margin: 4px 0 8px;' },
      `agent: ${a.agent_id || '—'} · created ${relTime(a.created_at)} · expires ${relTime(a.expires_at)}`),
    el('div', { style: 'background: var(--bg); border: 1px solid var(--border); border-radius: 4px; padding: 8px; margin: 4px 0;' },
      el('div', { class: 'meta' }, 'reasoning'),
      a.reason || '(none provided)'),
    a.intent_category ? el('div', { class: 'meta' }, 'intent: ', el('code', {}, a.intent_category)) : null,
    el('details', {},
      el('summary', {}, 'arguments'),
      el('pre', { class: 'json' }, JSON.stringify(a.arguments || {}, null, 2)),
    ),
    el('div', { class: 'row', style: 'margin-top: 10px;' },
      el('button', { class: 'primary', on: { click: () => decideApproval(a.id, 'allowed') } }, 'Allow'),
      el('button', { class: 'danger',  on: { click: () => decideApproval(a.id, 'denied')  } }, 'Deny'),
    ),
  )));
}

async function decideApproval(id, action) {
  try {
    await api(`/v1/approvals/${id}/decide`, { method: 'POST', body: { Action: action } });
    state.approvals = state.approvals.filter((a) => a.id !== id);
    toast(action === 'allowed' ? 'Approved' : 'Denied');
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
    el('div', { class: 'card' },
      el('h2', {}, 'Enroll a new agent'),
      el('p', { class: 'meta' },
        'Generate a one-time enrollment code, then exchange it from your agent host with: ',
        el('code', {}, 'curl -sX POST http://localhost:8787/v1/agents/exchange -H "content-type: application/json" -d \'{"Code":"<code>"}\'')),
      el('div', { class: 'row' },
        el('input', { id: 'enroll-name', placeholder: 'agent name (e.g. claude-code)', class: 'grow' }),
        el('button', { class: 'primary', on: { click: async () => {
          try {
            const r = await api('/v1/agents/enroll', { method: 'POST', body: { Name: $('enroll-name').value.trim() || 'agent' }});
            state.enrollment = r;
            render();
          } catch (e) { toast(e.message, 'error'); }
        }}}, 'Generate code'),
      ),
      state.enrollment ? el('div', { class: 'card', style: 'margin-top: 12px; background: var(--bg);' },
        el('div', { class: 'meta' }, `code (expires in ${Math.round(state.enrollment.expires_in/60)}m)`),
        el('pre', { class: 'json' }, state.enrollment.enrollment_code),
      ) : null,
    ),
    el('div', { class: 'card' },
      el('h2', {}, 'Enrolled agents'),
      state.agents.length === 0
        ? el('div', { class: 'empty' }, 'No agents yet.')
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

function viewSettings() {
  return el('div', {},
    el('div', { class: 'card' },
      el('h2', {}, 'Web Push'),
      el('p', { class: 'meta' },
        'Subscribe this browser to push notifications. On iOS, install this dashboard to your home screen first.'),
      el('div', { class: 'row' },
        state.vapidKey
          ? el('button', { class: 'primary', on: { click: enablePush }}, state.pushReady ? 'Push enabled' : 'Enable push')
          : el('span', { class: 'meta' }, 'No VAPID key on the server.'),
      ),
    ),
    el('div', { class: 'card' },
      el('h2', {}, 'About'),
      el('p', {}, 'toolyard v0.1.0 — Apache-2.0.'),
      el('p', { class: 'meta' }, 'Single Go binary + SQLite. Source: ', el('code', {}, 'github.com/tusharbhardwaj/toolyard')),
    ),
  );
}

async function enablePush() {
  try {
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      throw new Error('this browser does not support Web Push');
    }
    const reg = await navigator.serviceWorker.register('/sw.js');
    await navigator.serviceWorker.ready;
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') throw new Error('notification permission denied');
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

// ---- shell -----------------------------------------------------------------

function navigate(route) {
  state.route = route;
  history.replaceState(null, '', '#' + route);
  render();
}

function shell(content) {
  const navBtn = (key, label) => el('button', {
    class: state.route === key ? 'active' : '',
    on: { click: () => navigate(key) }
  }, label);
  return el('div', {},
    el('header', {},
      el('div', { class: 'brand' }, el('span', { class: 'dot' }), 'toolyard'),
      el('nav', {},
        navBtn('approvals', 'Approvals'),
        navBtn('audit',     'Audit'),
        navBtn('memory',    'Memory'),
        navBtn('agents',    'Agents'),
        navBtn('settings',  'Settings'),
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
  );
}

function render() {
  const root = $('app') || document.body;
  root.innerHTML = '';
  if (state.setupRequired) { root.appendChild(viewSetup()); return; }
  if (!state.user) { root.appendChild(viewLogin()); return; }
  let body;
  switch (state.route) {
    case 'audit':    body = viewAudit();    break;
    case 'memory':   body = viewMemory();   break;
    case 'agents':   body = viewAgents();   break;
    case 'settings': body = viewSettings(); break;
    default:         body = viewApprovals();
  }
  root.appendChild(shell(body));
}

(async () => {
  if ('serviceWorker' in navigator) {
    try { navigator.serviceWorker.register('/sw.js'); } catch {}
  }
  if (location.hash) state.route = location.hash.slice(1) || 'approvals';
  await refreshUser();
  if (state.user) { await loadAll(); startStream(); }
  render();
})();
