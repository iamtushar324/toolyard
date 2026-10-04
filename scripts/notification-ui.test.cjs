const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../web/dashboard/sw.js'), 'utf8');

function harness({ existingClient = false, response = { ok: true, status: 200 } } = {}) {
  const handlers = {}, notifications = [], requests = [], destinations = [];
  let focused = 0;
  const client = { async navigate(url) { destinations.push(url); }, async focus() { focused++; } };
  const context = vm.createContext({
    self: {
      addEventListener(name, callback) { handlers[name] = callback; },
      registration: { async showNotification(title, options) { notifications.push(JSON.parse(JSON.stringify({ title, ...options }))); } },
      clients: {
        async matchAll() { return existingClient ? [client] : []; },
        async openWindow(url) { destinations.push(url); },
      },
    },
    async fetch(url, options) {
      requests.push({ url, ...options, body: JSON.parse(options.body) });
      if (response instanceof Error) throw response;
      return response;
    },
    encodeURIComponent,
  });
  vm.runInContext(source, context);
  async function dispatch(name, fields) {
    const waits = [];
    handlers[name]({ ...fields, waitUntil(promise) { waits.push(promise); } });
    await Promise.all(waits);
  }
  return {
    notifications, requests, destinations,
    get focused() { return focused; },
    push(data) { return dispatch('push', { data: { json: () => data } }); },
    click(action, data) { return dispatch('notificationclick', { action, notification: { data, close() {} } }); },
  };
}

test('legacy push offers only an Inbox handoff and discards approval capability', async () => {
  const h = harness();
  await h.push({ approval_id: 'ap_legacy', decision_token: 'legacy-capability', url: '/?approval=ap_legacy' });
  const card = h.notifications[0];
  assert.deepEqual(card.actions, []);
  assert.equal(card.data.url, '/#inbox/ap_legacy');
  assert.equal(card.data.decision_token, undefined);
  assert.equal(h.requests.length, 0);
});

test('already-visible legacy Allow and Deny taps focus the original Inbox item without a decision', async () => {
  for (const action of ['allow', 'deny', '']) {
    const h = harness({ existingClient: true });
    await h.click(action, { approval_id: 'ap_legacy', decision_token: 'old-capability', url: '/?approval=ap_legacy' });
    assert.deepEqual(h.destinations, ['/#inbox/ap_legacy']);
    assert.equal(h.focused, 1);
    assert.equal(h.requests.length, 0);
  }
});

test('new migrated request notifications open Inbox without any approval token', async () => {
  const h = harness();
  await h.push({ kind: 'inbox', approval_id: 'ap_migrated', url: '/#inbox/ap_migrated' });
  const card = h.notifications[0];
  assert.deepEqual(card.actions, []);
  await h.click('', card.data);
  assert.deepEqual(h.destinations, ['/#inbox/ap_migrated']);
  assert.equal(h.requests.length, 0);
});

test('Inbox deny, snooze, and question options retain their signed action and result', async () => {
  for (const [action, title, outcome] of [['deny', 'Deny', 'Denied'], ['snooze', 'Snooze 1h', 'Snoozed'], ['opt1', 'Later', 'Answered: Later']]) {
    const h = harness();
    await h.push({ kind: 'inbox', request_id: 'rq_test', url: '/#inbox/rq_test', inbox_token: 'signed-inbox-tap', actions: [{ action, title }] });
    const card = h.notifications[0];
    assert.equal(card.actions[0].action, action);
    await h.click(action, card.data);
    assert.equal(h.requests.length, 1);
    assert.equal(h.requests[0].url, '/v1/inbox/decide-by-token');
    assert.deepEqual(h.requests[0].body, { token: 'signed-inbox-tap', action });
    assert.ok(h.notifications[1].title.includes(outcome));
    assert.equal(h.notifications[1].data.url, '/#inbox/rq_test');
  }
});

test('approval actions cannot enter the Inbox tap path even in stale or malformed notification data', async () => {
  const h = harness();
  await h.push({ kind: 'inbox', url: '/#inbox/rq_test', inbox_token: 'signed-tap', actions: [{ action: 'allow', title: 'Allow' }, { action: 'approve', title: 'Approve' }, { action: 'deny', title: 'Deny' }] });
  assert.deepEqual(h.notifications[0].actions, [{ action: 'deny', title: 'Deny' }]);
  await h.click('allow', { kind: 'inbox', url: '/#inbox/rq_test', inbox_token: 'signed-tap' });
  assert.deepEqual(h.destinations, ['/#inbox/rq_test']);
  assert.equal(h.requests.length, 0);
});

test('failed and expired Inbox taps expose the real outcome with a review link', async () => {
  for (const [response, outcome] of [[new Error('offline'), 'Couldn’t deliver'], [{ ok: false, status: 409 }, 'Already decided or expired']]) {
    const h = harness({ response });
    await h.click('deny', { kind: 'inbox', inbox_token: 'tap', url: '/#inbox/rq_test' });
    assert.ok(h.notifications[0].title.includes(outcome));
    assert.equal(h.notifications[0].data.url, '/#inbox/rq_test');
  }
});

test('other notifications keep their destination and do not acquire permission actions', async () => {
  const h = harness();
  await h.push({ title: 'Reconnect the server', url: '/#connections' });
  assert.deepEqual(h.notifications[0].actions, []);
  await h.click('', h.notifications[0].data);
  assert.deepEqual(h.destinations, ['/#connections']);
  assert.equal(h.requests.length, 0);
});
