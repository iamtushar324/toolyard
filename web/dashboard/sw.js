// toolyard service worker — handles Web Push notifications and one-tap actions.

self.addEventListener('install', (e) => {
  self.skipWaiting();
});

self.addEventListener('activate', (e) => {
  e.waitUntil(self.clients.claim());
});

self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (_) {}
  const title = data.title || 'toolyard Inbox';
  const body  = data.body  || 'A tool call needs your decision.';
  const tag   = data.tag   || data.request_id || data.approval_id || 'toolyard';
  const url   = notificationLink(data);
  // Accepted calls always require the complete review in Inbox. Legacy
  // notifications only open that same item; they never offer approval.
  const isInbox = data.kind === 'inbox';
  const actions = isInbox
    ? (Array.isArray(data.actions) ? data.actions.filter(a => a && isInboxTap(a.action)).slice(0, 2) : [])
    : [];

  event.waitUntil(self.registration.showNotification(title, {
    body,
    tag,
    icon: '/icon-192.svg',
    badge: '/icon-192.svg',
    data: { url, tag, kind: data.kind, approval_id: data.approval_id,
      inbox_token: data.inbox_token, tool: data.tool, actions },
    actions,
    renotify: isInbox,
    requireInteraction: !isInbox || data.reason === 'reminder',
  }));
});

function notificationLink(data) {
  if (data.approval_id) return `/#inbox/${encodeURIComponent(data.approval_id)}`;
  return data.url || '/#inbox';
}

function isInboxTap(action) {
  return /^(deny|snooze|opt[0-9]+)$/.test(action || '');
}

// focusOrOpen brings an existing dashboard tab forward (best-effort
// navigating it to url) or opens a new one.
async function focusOrOpen(url) {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const c of all) {
    if ('focus' in c) {
      if (c.navigate) { try { await c.navigate(url); } catch (_) {} }
      return c.focus();
    }
  }
  return self.clients.openWindow(url);
}

self.addEventListener('notificationclick', (event) => {
  const data = event.notification.data || {};
  event.notification.close();

  const deepLink = notificationLink(data);
  if (data.kind === 'inbox') {
    event.waitUntil(inboxTap(event.action, data, deepLink));
    return;
  }
  // Also applies to legacy cards already visible before this worker
  // update: their old Allow/Deny buttons become a handoff to Inbox.
  event.waitUntil(focusOrOpen(deepLink));
});

// inboxTap delivers an inbox notification action (deny, snooze, or an
// answer option) and replaces the notification with the outcome.
async function inboxTap(action, data, deepLink) {
  if (!isInboxTap(action) || !data.inbox_token) return focusOrOpen(deepLink);
  const label = ((data.actions || []).find((a) => a.action === action) || {}).title || action;
  const note = (title, body, extra) => self.registration.showNotification(title, Object.assign({
    tag: data.tag || 'toolyard', body, icon: '/icon-192.svg', badge: '/icon-192.svg', data: { url: deepLink },
  }, extra || {}));
  try {
    const res = await fetch('/v1/inbox/decide-by-token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token: data.inbox_token, action }),
    });
    if (res.ok) {
      const done = action === 'snooze' ? 'Snoozed for an hour' : action === 'deny' ? 'Denied' : 'Answered: ' + label;
      await note('✓ ' + done, 'The agent has been told.');
    } else if (res.status === 404 || res.status === 409 || res.status === 410) {
      await note('Already decided or expired', 'Tap to open the inbox.');
    } else {
      throw new Error('http ' + res.status);
    }
  } catch (_) {
    await note('Couldn’t deliver that', 'Network error — tap to open the inbox.', { requireInteraction: true });
  }
}
