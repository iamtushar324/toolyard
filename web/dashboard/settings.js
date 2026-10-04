// Workspace settings. Non-secret drafts survive live updates and tab reloads.
// Credentials stay in memory and are never written to browser storage.
const SETTINGS_PAGES = [
  { id: 'general', title: 'General', icon: 'sliders', description: 'Choose which tools agents see and review automatic model prices.' },
  { id: 'permissions', title: 'Permissions', icon: 'shield', description: 'Control requests, evidence, and restricted tool calls.' },
  { id: 'notifications', title: 'Notifications', icon: 'bell', description: 'Choose when and how Toolyard asks for your attention.' },
  { id: 'security', title: 'Security & access', icon: 'lock', description: 'Manage passkeys and access from the command line.' },
  { id: 'secrets', title: 'Secrets', icon: 'key', description: 'Store credentials for your connections.' },
  { id: 'data', title: 'Data & exports', icon: 'download', description: 'Download workspace records and import memory.' },
  { id: 'about', title: 'About', icon: 'info', description: 'Review this workspace and its available features.' },
];
const settingsUI = { page: 'general', search: '', drafts: {}, errors: {}, saved: {}, busy: {}, userId: null, secret: null, secretEdit: null };

function settingsStorageKey() { return 'toolyard.settings.' + state.user.id; }
function settingsInit() {
  if (settingsUI.userId === state.user.id) return;
  settingsUI.userId = state.user.id;
  settingsUI.drafts = {}; settingsUI.errors = {}; settingsUI.saved = {}; settingsUI.busy = {}; settingsUI.secret = null; settingsUI.secretEdit = null;
  try {
    const saved = JSON.parse(sessionStorage.getItem(settingsStorageKey()));
    if (saved && Date.now() - saved.at < 86400000 && saved.drafts) settingsUI.drafts = saved.drafts;
  } catch (_) {}
  for (const draft of Object.values(settingsUI.drafts)) {
    if (!draft || typeof draft !== 'object') continue;
    delete draft.cost_input_usd_per_m; delete draft.cost_output_usd_per_m;
  }
}
function settingsPersist() {
  try { sessionStorage.setItem(settingsStorageKey(), JSON.stringify({ at: Date.now(), drafts: settingsUI.drafts })); } catch (_) {}
}
function settingsClearDrafts() {
  try { if (settingsUI.userId) sessionStorage.removeItem('toolyard.settings.' + settingsUI.userId); } catch (_) {}
  settingsUI.userId = null; settingsUI.drafts = {}; settingsUI.secret = null; settingsUI.secretEdit = null;
}
function settingsRoute(route) {
  const id = route.split('/')[1] || 'general';
  settingsUI.page = SETTINGS_PAGES.some(p => p.id === id) ? id : 'general';
  return 'settings';
}
function settingsValue(page, key, fallback) {
  const d = settingsUI.drafts[page] || {};
  return Object.hasOwn(d, key) ? d[key] : (state.settings[key] ?? fallback);
}
function settingsChange(page, key, value) {
  const d = settingsUI.drafts[page] || (settingsUI.drafts[page] = {});
  d[key] = value;
  settingsUI.errors[page] = ''; settingsUI.saved[page] = false;
  settingsPersist(); settingsRefreshFooter(page);
}
function settingsDirty(page) { return Object.keys(settingsUI.drafts[page] || {}).length > 0; }
function settingsRefreshFooter(page) {
  const dirty = settingsDirty(page), busy = !!settingsUI.busy[page];
  const status = document.getElementById('settings-status-' + page);
  if (status) {
    status.textContent = settingsUI.errors[page] || (busy ? 'Save in progress…' : dirty ? 'You have unsaved changes.' : settingsUI.saved[page] ? 'Changes saved.' : 'All changes saved.');
    status.className = 'settings-status' + (settingsUI.errors[page] ? ' error' : settingsUI.saved[page] ? ' saved' : '');
  }
  for (const action of ['save', 'discard']) {
    const button = document.getElementById('settings-' + action + '-' + page);
    if (button) button.disabled = !dirty || busy;
  }
  const dot = document.querySelector('[data-settings-draft="' + page + '"]');
  if (dot) dot.hidden = !dirty;
}
function settingsValidate(page, body) {
  if (page === 'notifications') {
    const d = settingsUI.drafts[page] || {};
    if (Object.hasOwn(d, '_quiet_start') || Object.hasOwn(d, '_quiet_end')) {
      const [oldStart, oldEnd] = (state.settings.inbox_quiet_hours || '').split('-');
      const start = d._quiet_start ?? oldStart ?? '', end = d._quiet_end ?? oldEnd ?? '';
      if ((start || end) && (!/^([01]\d|2[0-3]):[0-5]\d$/.test(start) || !/^([01]\d|2[0-3]):[0-5]\d$/.test(end) || start === end)) throw new Error('Set different start and end times, or clear both times.');
      body.inbox_quiet_hours = start ? start + '-' + end : '';
    }
    delete body._quiet_start; delete body._quiet_end;
    if (Object.hasOwn(body, 'inbox_digest_times')) {
      const slots = body.inbox_digest_times.split(',').map(t => t.trim()).filter(Boolean);
      if (slots.length > 8 || slots.some(t => !/^([01]\d|2[0-3]):[0-5]\d$/.test(t))) throw new Error('Use up to eight times in HH:MM format, separated by commas.');
      body.inbox_digest_times = [...new Set(slots)].join(',');
    }
    if (body.inbox_timezone) {
      try { new Intl.DateTimeFormat('en', { timeZone: body.inbox_timezone.trim() }); }
      catch (_) { throw new Error('Enter a valid time zone, such as Asia/Kolkata.'); }
      body.inbox_timezone = body.inbox_timezone.trim();
    }
  }
  for (const [key, min, max] of [['top_n_count', 1, 200], ['top_n_personalize_after', 0, 1000000], ['inbox_now_per_hour', 1, 60]]) {
    if (Object.hasOwn(body, key)) {
      const n = Number(body[key]);
      if (body[key] === '' || !Number.isInteger(n) || n < min || n > max) throw new Error('Enter a whole number from ' + min + ' to ' + max + '.');
      body[key] = n;
    }
  }
  return body;
}
async function settingsSave(page, form) {
  if (settingsUI.busy[page] || !settingsDirty(page) || !form.reportValidity()) return;
  const submitted = { ...settingsUI.drafts[page] };
  let body;
  try { body = settingsValidate(page, { ...submitted }); }
  catch (e) { settingsUI.errors[page] = e.message; settingsRefreshFooter(page); return; }
  settingsUI.busy[page] = true; settingsUI.errors[page] = ''; render();
  try {
    const out = await api('/v1/settings', { method: 'PATCH', body });
    Object.assign(state.settings, out || body);
    // Do not drop a new edit made while a response was in flight.
    for (const [k, v] of Object.entries(submitted)) if (settingsUI.drafts[page]?.[k] === v) delete settingsUI.drafts[page][k];
    settingsUI.saved[page] = true; state.inbox.info = null; settingsPersist();
  } catch (e) { settingsUI.errors[page] = 'Could not save: ' + e.message + ' Your changes are still here.'; }
  finally { settingsUI.busy[page] = false; render(); }
}
function settingsForm(page, ...sections) {
  const form = el('form', { class: 'settings-form', on: {
    submit: e => { e.preventDefault(); settingsSave(page, form); },
    keydown: e => { if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') { e.preventDefault(); form.requestSubmit(); } },
  } },
    el('fieldset', { class: 'settings-fields', disabled: !!settingsUI.busy[page] }, ...sections),
    el('div', { class: 'settings-savebar' },
      el('span', { id: 'settings-status-' + page, class: 'settings-status', role: 'status', 'aria-live': 'polite' },
        settingsUI.errors[page] || (settingsUI.busy[page] ? 'Save in progress…' : settingsDirty(page) ? 'You have unsaved changes.' : settingsUI.saved[page] ? 'Changes saved.' : 'All changes saved.')),
      el('div', { class: 'row' },
        el('button', { id: 'settings-discard-' + page, type: 'button', disabled: !settingsDirty(page) || !!settingsUI.busy[page], on: { click: () => {
          delete settingsUI.drafts[page]; settingsUI.errors[page] = ''; settingsUI.saved[page] = false; settingsPersist(); render();
        } } }, 'Discard'),
        el('button', { id: 'settings-save-' + page, type: 'submit', class: 'primary', disabled: !settingsDirty(page) || !!settingsUI.busy[page] }, settingsUI.busy[page] ? 'Save in progress…' : 'Save changes'))));
  if (settingsUI.errors[page]) form.querySelector('.settings-status').classList.add('error');
  else if (settingsUI.saved[page]) form.querySelector('.settings-status').classList.add('saved');
  return form;
}
function settingsSection(title, description, ...rows) {
  return el('section', { class: 'settings-section' },
    el('div', { class: 'settings-section-head' }, el('h3', {}, title), description ? el('p', { class: 'meta' }, description) : null), ...rows);
}
function settingsRow(title, description, control, id) {
  return el('div', { class: 'settings-row' },
    el('div', { class: 'settings-row-copy' }, el(id ? 'label' : 'div', id ? { for: id, class: 'settings-row-title' } : { class: 'settings-row-title' }, title),
      description ? el('p', { class: 'meta', ...(id ? { id: id + '-hint' } : {}) }, description) : null),
    el('div', { class: 'settings-row-control' }, control));
}
function settingsInput(page, key, title, description, attrs = {}) {
  const id = 'setting-' + key;
  const { multiline, fallback = '', ...inputAttrs } = attrs;
  return settingsRow(title, description, el(multiline ? 'textarea' : 'input', {
    id, type: inputAttrs.type || 'text', value: settingsValue(page, key, fallback),
    'aria-describedby': id + '-hint', ...inputAttrs,
    on: { input: e => settingsChange(page, key, e.target.value) },
  }), id);
}
function settingsToggle(page, key, title, description, fallback = false) {
  const id = 'setting-' + key;
  return settingsRow(title, description, el('label', { class: 'settings-switch', for: id },
    el('input', { id, type: 'checkbox', role: 'switch', checked: !!settingsValue(page, key, fallback), 'aria-describedby': id + '-hint',
      on: { change: e => settingsChange(page, key, e.target.checked) } }), el('span', { class: 'switch-track', 'aria-hidden': 'true' })), id);
}
function settingsChoices(page, key, choices, fallback) {
  return el('div', { class: 'settings-choices', role: 'radiogroup', 'aria-label': key === 'surface_mode' ? 'Tool visibility' : 'Restricted call behavior' },
    ...choices.map(([value, title, description]) => el('label', { class: 'settings-choice' + (settingsValue(page, key, fallback) === value ? ' selected' : '') },
      el('input', { id: 'setting-' + key + '-' + value, name: key, type: 'radio', value, checked: settingsValue(page, key, fallback) === value,
        on: { change: () => { settingsChange(page, key, value); render(); } } }),
      el('span', {}, el('strong', {}, title), el('small', {}, description)))));
}
function settingsLoadInfo() {
  if (state.inbox.info || state.inbox.infoLoading || state.inbox.infoError) return;
  state.inbox.infoLoading = true;
  api('/v1/inbox/info').then(out => { state.inbox.info = { ...out.info, passkeys: out.passkeys }; })
    .catch(e => { state.inbox.infoError = e.message; })
    .finally(() => { state.inbox.infoLoading = false; if (state.route === 'settings') render(); });
}
function viewSettings() {
  settingsInit(); settingsLoadInfo();
  const page = SETTINGS_PAGES.find(p => p.id === settingsUI.page) || SETTINGS_PAGES[0];
  const pages = SETTINGS_PAGES.filter(p => (p.title + ' ' + p.description).toLowerCase().includes(settingsUI.search.toLowerCase()));
  const content = state.settingsError ? el('div', { class: 'settings-notice error', role: 'alert' }, 'Settings could not load. ',
    el('button', { on: { click: async () => { try { Object.assign(state.settings, await api('/v1/settings')); state.settingsError = ''; } catch (e) { state.settingsError = e.message; } render(); } } }, 'Try again')) : ({ general: settingsGeneral, permissions: settingsPermissions, notifications: settingsNotifications,
    security: settingsSecurity, secrets: settingsSecrets, data: settingsData, about: settingsAbout })[page.id]();
  return el('div', { class: 'settings-layout' },
    el('aside', { class: 'settings-nav' },
      el('input', { id: 'settings-search', type: 'search', value: settingsUI.search, placeholder: 'Find a setting…', 'aria-label': 'Find a settings category',
        on: { input: e => { settingsUI.search = e.target.value; render(); } } }),
      el('nav', { 'aria-label': 'Settings categories' }, ...pages.map(p => el('button', { id: 'settings-nav-' + p.id, class: page.id === p.id ? 'active' : '',
        'aria-current': page.id === p.id ? 'page' : 'false', on: { click: () => navigate('settings/' + p.id) } },
        uiIcon(p.icon), el('span', {}, p.title), el('span', { class: 'settings-draft-dot', 'data-settings-draft': p.id, hidden: !settingsDirty(p.id), title: 'Unsaved changes' }))),
        pages.length ? null : el('p', { class: 'meta' }, 'No categories match this search.')),
      state.authConfig?.environment === 'stage' ? el('div', { class: 'settings-stage-note' }, el('span', { class: 'badge' }, 'Stage workspace'), el('p', { class: 'meta' }, 'This workspace uses separate data and test connections.')) : null),
    el('div', { class: 'settings-content', id: 'settings-panel' },
      el('div', { class: 'settings-panel-heading' }, el('h2', {}, page.title), el('p', { class: 'meta' }, page.description)), content));
}
function settingsGeneral() {
  const page = 'general';
  return settingsForm(page,
    settingsSection('Tool visibility', 'Choose the catalog an agent receives when it connects.',
      settingsChoices(page, 'surface_mode', [
        ['full', 'Full catalog', 'Expose every available tool. Best for a small workspace.'],
        ['top_n', 'Most used tools', 'Expose common tools. Agents can search for the rest.'],
        ['router_only', 'Search first', 'Expose the router. Agents search for a tool before a call.'],
      ], 'full'),
      settingsValue(page, 'surface_mode', 'full') === 'top_n' ? el('div', {},
        settingsInput(page, 'top_n_count', 'Visible tool count', 'Extra tools beyond the router and memory tools.', { type: 'number', min: 1, max: 200, step: 1, fallback: 20 }),
        settingsInput(page, 'top_n_personalize_after', 'Personalize after', 'Successful calls before an agent receives its own most used tools.', { type: 'number', min: 0, max: 1000000, step: 1, fallback: 100 })) : null),
    settingsSection('Automatic model prices', 'Toolyard gets provider-specific prices from Models.dev every six hours.',
      pricingStatus(),
      el('p', { class: 'meta' }, 'Prices include separate input, output, and cache rates when the source publishes them.'),
      el('button', { type: 'button', on: { click: () => { navigate('insights'); requestAnimationFrame(() => document.getElementById('model-prices')?.scrollIntoView({ block: 'start' })); } } }, 'View model prices')));
}
function settingsPermissions() {
  const page = 'permissions', info = state.inbox.info || {};
  return settingsForm(page,
    settingsSection('Restricted calls', 'Choose what happens when an agent has no permission for a restricted tool.',
      settingsChoices(page, 'approval_mode', [
        ['inbox', 'Request permission in Inbox', 'Ask the agent to explain its request. The original call does not run.'],
        ['execute', 'Queue the tool call', 'Keep the call in the approval queue. Run it after you approve.'],
      ], 'execute')),
    settingsSection('Request evidence', 'Keep request attachments available after an agent stops.',
      settingsToggle(page, 'inbox_snapshot_enabled', 'Save linked media', 'Copy linked images, videos, and files when a request arrives.', true),
      settingsInput(page, 'inbox_hosting_note', 'File location guidance', 'Tell agents where to upload files for their requests.', { multiline: true, rows: 3, placeholder: 'Upload files to the evidence bucket and include the link.' })),
    info.judge_available || state.settings.inbox_judge_enabled ? settingsSection('Request review', 'An optional model can flag differences between a request and a tool call.',
      settingsToggle(page, 'inbox_judge_enabled', 'Model review', 'Send request text to Gemini to check for contradictions.')) : null,
    state.inbox.infoError ? settingsInfoError() : null);
}
function settingsInfoError() {
  return el('div', { class: 'settings-notice error', role: 'alert' }, 'Some feature details could not load. ', el('button', { type: 'button', on: { click: () => { state.inbox.infoError = ''; settingsLoadInfo(); render(); } } }, 'Try again'));
}
function settingsNotifications() {
  const page = 'notifications';
  const [start, end] = (state.settings.inbox_quiet_hours || '').split('-');
  const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const quiet = el('div', { class: 'settings-time-range' },
    ...[['_quiet_start', 'From', start], ['_quiet_end', 'Until', end]].map(([key, label, value]) => el('label', {},
      el('span', { class: 'meta' }, label), el('input', { id: 'setting-' + key, type: 'time', value: settingsValue(page, key, value || ''),
        on: { input: e => settingsChange(page, key, e.target.value) } }))),
    el('button', { type: 'button', on: { click: () => { settingsChange(page, '_quiet_start', ''); settingsChange(page, '_quiet_end', ''); render(); } } }, 'Clear'));
  const timezone = settingsInput(page, 'inbox_timezone', 'Time zone', 'Quiet hours and digest times use this time zone. Empty uses the server time.', { placeholder: 'Asia/Kolkata' });
  timezone.querySelector('.settings-row-control').appendChild(el('button', { type: 'button', class: 'text-button', on: { click: () => { settingsChange(page, 'inbox_timezone', browserTZ); render(); } } }, 'Use device time zone'));
  const form = settingsForm(page,
    settingsSection('Schedule', 'Urgent requests can notify you at once. Other requests wait for a group or digest.',
      settingsInput(page, 'inbox_now_per_hour', 'Urgent request limit', 'Maximum urgent requests per agent each hour.', { type: 'number', min: 1, max: 60, step: 1, fallback: 3 }),
      settingsRow('Quiet hours', 'Mute requests during this period. Clear both times to turn this off.', quiet),
      timezone,
      settingsInput(page, 'inbox_quiet_allow', 'Quiet hour exceptions', 'Tool names separated by commas. An asterisk matches a prefix.', { placeholder: 'deploy.rollback, pagerduty.*', maxlength: 1000 }),
      settingsInput(page, 'inbox_digest_times', 'Digest times', 'Up to eight times, separated by commas. Empty turns digests off.', { fallback: '09:30,13:30,18:30', placeholder: '09:30, 13:30, 18:30' })),
    settingsSection('Privacy', '', settingsToggle(page, 'inbox_push_details', 'Include request details', 'Show request titles and summaries in device notifications.')));
  const voice = state.inbox.info?.voice_available || state.settings.inbox_voice_enabled;
  return el('div', {}, form, renderPushCard(),
    state.authConfig?.environment !== 'stage' ? renderChatCard() : null,
    voice ? ibVoiceSettings(state.settings, async body => {
      try { Object.assign(state.settings, await api('/v1/settings', { method: 'PATCH', body })); state.inbox.info = null; render(); toast('Saved'); }
      catch (e) { toast(e.message, 'error'); }
    }) : null, state.inbox.infoError ? settingsInfoError() : null);
}
function settingsSecurity() {
  const passkeys = ibPasskeySettings();
  passkeys.className = 'settings-section';
  const heading = passkeys.querySelector('h4'); if (heading) heading.replaceWith(el('h3', {}, 'Passkeys'));
  const desc = passkeys.querySelector('p'); if (desc) desc.textContent = 'Use a passkey to verify high-risk approvals. The verification applies to the exact request you approve.';
  return el('div', {}, passkeys, renderOperatorTokensCard(),
    settingsSection('People and agents', 'Manage workspace roles and enrolled agents from the People page.',
      el('button', { on: { click: () => navigate('users') } }, 'Manage people'), el('button', { class: 'text-button', on: { click: () => navigate('agents') } }, 'Manage agents')));
}
function settingsSecrets() {
  if (!state.secretsLoaded) loadSecrets();
  const d = settingsUI.secret || (settingsUI.secret = { open: false, name: '', value: '', description: '', busy: false, error: '' });
  const create = async e => {
    e.preventDefault(); if (d.busy || !e.currentTarget.reportValidity()) return;
    if (!/^[A-Z][A-Z0-9_]{0,63}$/.test(d.name)) { d.error = 'Use a name such as API_KEY. Start with a letter.'; render(); return; }
    if (!d.value) { d.error = 'Enter a secret value.'; render(); return; }
    d.busy = true; d.error = ''; render();
    try { await api('/v1/secrets', { method: 'POST', body: { name: d.name, value: d.value, description: d.description } });
      settingsUI.secret = null; toast('Secret saved'); await loadSecrets();
    } catch (e) { d.error = e.message; }
    finally { d.busy = false; render(); }
  };
  const field = (key, label, attrs = {}) => el('label', { class: 'settings-secret-field', for: 'secret-' + key }, label,
    el('input', { id: 'secret-' + key, value: d[key], ...attrs, on: { input: e => { d[key] = key === 'name' ? e.target.value.toUpperCase() : e.target.value; if (key === 'name') e.target.value = d[key]; } } }));
  return settingsSection('Connection credentials', 'Secret values are encrypted. Toolyard does not display saved values.',
    el('div', { class: 'settings-section-actions' }, el('button', { class: 'primary', on: { click: () => { d.open = true; render(); document.getElementById('secret-name')?.focus(); } } }, uiIcon('plus'), 'Add secret')),
    state.secretsError ? el('div', { class: 'settings-notice error' }, state.secretsError, el('button', { on: { click: loadSecrets } }, 'Try again')) :
      !state.secretsLoaded || state.secretsLoading ? el('p', { class: 'meta' }, 'Load in progress…') :
      state.secrets.length ? el('div', { class: 'settings-secret-list' }, ...state.secrets.map(s => el('div', { class: 'settings-row' },
        el('div', { class: 'settings-row-copy' }, el('code', {}, s.name), s.pending ? el('span', { class: 'badge pending' }, 'Needs a value') : null,
          el('p', { class: 'meta' }, s.description || ((s.used_by || []).length ? 'Used by ' + s.used_by.join(', ') : 'No connections use this secret.'))),
        el('div', { class: 'row' }, el('button', { on: { click: () => rotateSecret(s.name, s.pending) } }, s.pending ? 'Set value' : 'Replace value'),
          el('button', { class: 'danger', on: { click: () => deleteSecret(s.name, s.used_by || []) } }, 'Delete'))))) :
      el('div', { class: 'settings-empty' }, uiIcon('key'), el('h3', {}, 'No secrets yet'), el('p', { class: 'meta' }, 'Add an API key when a connection requires one.')),
    d.open ? el('form', { class: 'settings-secret-form', on: { submit: create } },
      el('h3', {}, 'Add a secret'), el('fieldset', { disabled: d.busy, class: 'settings-fields' },
        field('name', 'Name', { required: true, maxlength: 64, placeholder: 'API_KEY', autocomplete: 'off' }),
        field('value', 'Value', { required: true, type: 'password', autocomplete: 'new-password' }),
        field('description', 'Description', { placeholder: 'Optional' })),
      d.error ? el('p', { class: 'error', role: 'alert' }, d.error) : null,
      el('div', { class: 'row' }, el('button', { class: 'primary', type: 'submit', disabled: d.busy }, d.busy ? 'Save in progress…' : 'Save secret'),
        el('button', { type: 'button', disabled: d.busy, on: { click: () => { settingsUI.secret = null; render(); } } }, 'Cancel'))) : null,
    el('p', { class: 'meta settings-reference' }, 'Use ', el('code', {}, 'secret://NAME'), ' in connection headers or environment values.'), settingsSecretDialog());
}
function settingsSecretDialog() {
  const d = settingsUI.secretEdit;
  if (!d) return null;
  const close = () => { if (!d.busy) { settingsUI.secretEdit = null; render(); } };
  const submit = async e => {
    e.preventDefault(); if (d.busy || !e.currentTarget.reportValidity()) return;
    d.busy = true; d.error = ''; render();
    try {
      await api('/v1/secrets/' + encodeURIComponent(d.name) + '?reconnect=1', { method: 'PUT', body: { value: d.value } });
      settingsUI.secretEdit = null; toast('Secret value saved'); await loadSecrets();
    } catch (e) { d.error = e.message; }
    finally { d.busy = false; render(); }
  };
  return el('div', { class: 'modal-bg', on: { click: e => { if (e.target === e.currentTarget) close(); } } },
    el('div', { class: 'modal' }, el('h3', {}, d.pending ? 'Set secret value' : 'Replace secret value'),
      el('p', { class: 'meta' }, 'Connections that use ', el('code', {}, d.name), ' reconnect after you save.'),
      el('form', { on: { submit } }, el('label', { for: 'secret-edit-value' }, 'New value'),
        el('input', { id: 'secret-edit-value', type: 'password', required: true, autocomplete: 'new-password', disabled: d.busy, value: d.value,
          on: { input: e => { d.value = e.target.value; } } }),
        d.error ? el('p', { class: 'error', role: 'alert' }, d.error) : null,
        el('div', { class: 'row modal-actions' }, el('button', { type: 'button', disabled: d.busy, on: { click: close } }, 'Cancel'),
          el('button', { class: 'primary', type: 'submit', disabled: d.busy }, d.busy ? 'Save in progress…' : 'Save value')))));
}
function settingsData() {
  const download = (title, description, csv, json) => settingsRow(title, description,
    el('div', { class: 'row' }, csv ? el('a', { class: 'button-link', href: csv, target: '_blank', rel: 'noopener' }, uiIcon('download'), 'CSV') : null,
      el('a', { class: 'button-link', href: json, target: '_blank', rel: 'noopener' }, uiIcon('download'), 'JSON')));
  const memory = state.authConfig?.capabilities?.memory !== false;
  return el('div', {}, settingsSection('Workspace exports', 'Exports include stored records. Approval exports exclude decision tokens.',
    download('Activity log', 'Tool calls, decisions, and workspace changes.', '/v1/audit/export?format=csv', '/v1/audit/export?format=json'),
    download('Approvals', 'Current and previous tool approvals.', '/v1/approvals/export?format=csv', '/v1/approvals/export?format=json'),
    memory ? download('Memory', 'Entries in the workspace memory store.', null, '/v1/memory/export') : null),
    memory ? settingsSection('Import memory', 'Merge entries into the existing store, or replace the store with a file.',
      el('div', { class: 'settings-import' },
        el('label', { for: 'mem-import-mode' }, 'Import mode', el('select', { id: 'mem-import-mode' }, el('option', { value: 'merge' }, 'Merge entries'), el('option', { value: 'replace' }, 'Replace all entries'))),
        el('label', { for: 'mem-import-file' }, 'JSON file', el('input', { id: 'mem-import-file', type: 'file', accept: 'application/json,.json' })),
        el('button', { on: { click: importMemoryFile } }, 'Import file'))) : null);
}
function settingsAbout() {
  const config = state.authConfig || {};
  return settingsSection('Workspace', '',
    settingsRow('Environment', 'The workspace you currently use.', el('span', { class: 'badge' }, config.environment === 'stage' ? 'Stage' : 'Live')),
    settingsRow('Version', 'The deployed Toolyard version.', el('code', {}, config.version || 'Unknown')),
    settingsRow('Connections', 'Services available in this workspace.', el('span', {}, String(state.servers.length))),
    settingsRow('Agents', 'Agents enrolled in this workspace.', el('span', {}, String(state.agents.length))),
    settingsRow('Available features', '', el('div', { class: 'row' }, ...Object.entries(config.capabilities || {}).filter(([, enabled]) => enabled).map(([name]) => el('span', { class: 'badge' }, name)))),
    config.environment === 'stage' ? el('div', { class: 'settings-notice' }, 'Stage uses an isolated database and test connectors. Your feedback stays in this workspace.') : null);
}
