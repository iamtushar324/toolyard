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

  event.waitUntil(self.registration.showNotification(title, {
    body,
    tag,
    icon: '/icon-192.svg',
    badge: '/icon-192.svg',
    data: { url, approval_id: data.approval_id, decision_token: data.decision_token },
    actions: [
      { action: 'allow', title: 'Allow' },
      { action: 'deny',  title: 'Deny'  },
    ],
    requireInteraction: true,
  }));
});

self.addEventListener('notificationclick', (event) => {
  const data = event.notification.data || {};
  event.notification.close();

  if (event.action === 'allow' || event.action === 'deny') {
    if (data.decision_token) {
      const action = event.action === 'allow' ? 'allowed' : 'denied';
      event.waitUntil(fetch('/v1/approvals/decide-by-token', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: data.decision_token, action }),
      }));
      return;
    }
  }
  event.waitUntil(self.clients.openWindow(data.url || '/'));
});
