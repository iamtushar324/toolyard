// Focused behavior checks. Run with: node --test scripts/settings-ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, '../web/dashboard/settings.js'), 'utf8');

function harness() {
  const storage = new Map();
  const context = vm.createContext({
    state: { user: { id: 'test-user' }, settings: { inbox_quiet_hours: '', inbox_timezone: '', surface_mode: 'full' }, inbox: {} },
    sessionStorage: { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value), removeItem: key => storage.delete(key) },
    document: { getElementById: () => null, querySelector: () => null }, render() {},
  });
  vm.runInContext(source, context);
  const ui = vm.runInContext('({ settingsUI, settingsInit, settingsChange, settingsValue, settingsValidate, settingsSave, settingsClearDrafts })', context);
  ui.settingsInit();
  return { context, storage, ...ui };
}

test('a non-secret draft survives a refresh and stays separate for each user', () => {
  const h = harness();
  h.settingsChange('permissions', 'inbox_hosting_note', 'Keep my exact draft.');
  h.settingsUI.userId = null;
  h.settingsInit();
  assert.equal(h.settingsValue('permissions', 'inbox_hosting_note', ''), 'Keep my exact draft.');
  h.context.state.user.id = 'second-user'; h.settingsInit();
  assert.equal(h.settingsValue('permissions', 'inbox_hosting_note', ''), '');
});

test('quiet hours reject partial and invalid ranges before a request', () => {
  const h = harness();
  h.settingsChange('notifications', '_quiet_start', '22:00');
  assert.throws(() => h.settingsValidate('notifications', { _quiet_start: '22:00' }), /different start and end/);
  h.settingsChange('notifications', '_quiet_end', '07:00');
  const body = h.settingsValidate('notifications', { _quiet_start: '22:00', _quiet_end: '07:00' });
  assert.equal(body.inbox_quiet_hours, '22:00-07:00');
  assert.equal(Object.hasOwn(body, '_quiet_start'), false);
  assert.throws(() => h.settingsValidate('notifications', { inbox_digest_times: '09:30,25:00' }), /HH:MM/);
  assert.throws(() => h.settingsValidate('notifications', { inbox_timezone: 'invalid-zone' }), /valid time zone/);
});

test('numbers reject empty, fractional, and out-of-range values', () => {
  const h = harness();
  for (const value of ['', 0, 201, 2.5]) assert.throws(() => h.settingsValidate('general', { top_n_count: value }), /whole number/);
  assert.equal(h.settingsValidate('general', { top_n_count: '25' }).top_n_count, 25);
});

test('obsolete manual prices are removed from saved drafts', () => {
  const h = harness();
  h.storage.set('toolyard.settings.test-user', JSON.stringify({ at: Date.now(), drafts: {
    general: { cost_input_usd_per_m: 2, cost_output_usd_per_m: 8, top_n_count: 25 },
  } }));
  h.settingsUI.userId = null; h.settingsInit();
  assert.deepEqual(Object.keys(h.settingsUI.drafts.general), ['top_n_count']);
});

test('a failed save keeps the draft and the previous stored value', async () => {
  const h = harness();
  h.settingsChange('permissions', 'inbox_hosting_note', 'unsaved');
  h.context.api = async () => { throw new Error('offline'); };
  await h.settingsSave('permissions', { reportValidity: () => true });
  assert.equal(h.settingsUI.drafts.permissions.inbox_hosting_note, 'unsaved');
  assert.equal(h.context.state.settings.inbox_hosting_note, undefined);
  assert.match(h.settingsUI.errors.permissions, /Your changes are still here/);
  assert.equal(h.settingsUI.busy.permissions, false);
});

test('a save sends only edited keys and does not discard a newer edit', async () => {
  const h = harness(); let finish, payload;
  h.settingsChange('permissions', 'inbox_hosting_note', 'submitted');
  h.context.api = async (_, options) => {
    payload = options.body;
    return new Promise(resolve => { finish = resolve; });
  };
  const save = h.settingsSave('permissions', { reportValidity: () => true });
  h.settingsChange('permissions', 'inbox_hosting_note', 'newer draft');
  finish({ inbox_hosting_note: 'submitted', surface_mode: 'full' }); await save;
  assert.deepEqual(Object.keys(payload), ['inbox_hosting_note']);
  assert.equal(h.context.state.settings.inbox_hosting_note, 'submitted');
  assert.equal(h.settingsUI.drafts.permissions.inbox_hosting_note, 'newer draft');
});

test('credentials stay out of storage and logout clears drafts', () => {
  const h = harness();
  h.settingsChange('general', 'top_n_count', '25');
  h.settingsUI.secret = { value: 'private-test-value' };
  assert.equal([...h.storage.values()].some(value => value.includes('private-test-value')), false);
  h.settingsClearDrafts();
  assert.equal(h.settingsUI.secret, null);
  assert.equal(h.storage.size, 0);
});
