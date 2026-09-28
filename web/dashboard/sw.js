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
  const title = data.title || 'toolyard approval';
  const body  = data.body  || 'A tool call needs your decision.';
  const tag   = data.tag   || data.approval_id || 'toolyard';
  const url   = data.url   || (data.approval_id ? `/?approval=${data.approval_id}` : '/');
  // Inbox pushes name their own actions (never "approve": allowing tools
  // happens on the review page). Legacy approval pushes get Allow / Deny.
  const isInbox = data.kind === 'inbox';
  const actions = isInbox
    ? (Array.isArray(data.actions) ? data.actions.slice(0, 2) : [])
    : [{ action: 'allow', title: 'Allow' }, { action: 'deny', title: 'Deny' }];

  event.waitUntil(self.registration.showNotification(title, {
    body,
    tag,
    icon: '/icon-192.svg',
    badge: '/icon-192.svg',
    data: { url, tag, kind: data.kind, approval_id: data.approval_id, decision_token: data.decision_token,
      inbox_token: data.inbox_token, tool: data.tool, actions },
    actions,
    renotify: isInbox,
    requireInteraction: !isInbox || data.reason === 'reminder',
  }));
});

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

  const deepLink = data.url || (data.approval_id ? `/?approval=${data.approval_id}` : '/');
  if (data.kind === 'inbox') {
    event.waitUntil(inboxTap(event.action, data, deepLink));
    return;
  }
  const isDecision = (event.action === 'allow' || event.action === 'deny') && data.decision_token;
  if (!isDecision) {
    // Plain tap (or an action with no token) → focus/open the dashboard.
    event.waitUntil(focusOrOpen(deepLink));
    return;
  }

  const action = event.action === 'allow' ? 'allowed' : 'denied';
  const verb = event.action === 'allow' ? 'Approved' : 'Denied';
  const tag = data.tag || data.approval_id || 'toolyard';
  const toolLabel = data.tool ? ` — ${data.tool}` : '';
  const note = (title, body, extra) => self.registration.showNotification(title, Object.assign({
    tag, body, icon: '/icon-192.svg', badge: '/icon-192.svg',
    data: { url: deepLink },
  }, extra || {}));

  // Deliver the decision and replace the card (same tag) with the outcome,
  // so a missed/expired tap no longer reads as a silent success.
  event.waitUntil((async () => {
    try {
      const res = await fetch('/v1/approvals/decide-by-token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: data.decision_token, action }),
      });
      if (res.ok) {
        await note(`✓ ${verb}${toolLabel}`, 'Decision delivered.');
      } else if (res.status === 404 || res.status === 409 || res.status === 410) {
        await note('Already decided or expired', 'Tap to open the dashboard.');
      } else {
        throw new Error('http ' + res.status);
      }
    } catch (_) {
      await note('Couldn’t deliver decision', 'Network error — tap to decide on the dashboard.', { requireInteraction: true });
    }
  })());
});

// inboxTap delivers an inbox notification action (deny, snooze, or an
// answer option) and replaces the notification with the outcome.
async function inboxTap(action, data, deepLink) {
  if (!action || !data.inbox_token) return focusOrOpen(deepLink);
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
