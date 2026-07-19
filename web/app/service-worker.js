const notificationRoutes = Object.freeze({
  monitor: "/?tab=monitor",
  alerts: "/?tab=alerts",
});

self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch {
    payload = {};
  }
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) payload = {};
  const destination = payload.destination === "alerts" ? "alerts" : "monitor";
  const title = typeof payload.title === "string" && payload.title ? payload.title : "ibkr canary";
  const body = typeof payload.body === "string" && payload.body ? payload.body : "Open ibkr canary for details.";
  const tag = notificationTag(payload);
  event.waitUntil((async () => {
    await self.registration.showNotification(title, {
      body,
      data: { destination },
      tag,
      badge: "/favicon-64.png",
      icon: "/icon-192.png",
    });
    await refreshAppIconBadge();
  })());
});

// After showing a notification, mirror the server's unread truth onto the
// installed app icon (Badging API). Best-effort: an unsupported runtime or a
// failed fetch leaves the icon untouched — the notification itself already
// displayed. notificationclick deliberately never touches the badge: a tap
// navigates, it does not mark anything read.
async function refreshAppIconBadge() {
  const nav = self.navigator;
  if (!nav || typeof nav.setAppBadge !== "function" || typeof self.fetch !== "function") return;
  try {
    const res = await self.fetch("/api/attention", { credentials: "include" });
    if (!res.ok) return;
    const attention = await res.json();
    const unread = attention?.unread_count;
    if (Number.isSafeInteger(unread) && unread > 0) await nav.setAppBadge(unread);
    else if (typeof nav.clearAppBadge === "function") await nav.clearAppBadge();
    else await nav.setAppBadge(0);
  } catch {
    // Best-effort only.
  }
}

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const destination = event.notification.data?.destination === "alerts" ? "alerts" : "monitor";
  const route = notificationRoutes[destination];
  event.waitUntil(openNotificationRoute(route));
});

function notificationTag(payload) {
  if (typeof payload.display_id === "string" && payload.display_id) return payload.display_id;
  if (typeof payload.alert_id === "string" && payload.alert_id) return payload.alert_id;
  return "ibkr-canary";
}

async function openNotificationRoute(route) {
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  for (const client of windows) {
    try {
      if (typeof client.navigate === "function") await client.navigate(route);
      if (typeof client.focus === "function") await client.focus();
      return;
    } catch {
      // Try another controlled or uncontrolled app window before opening one.
    }
  }
  await self.clients.openWindow(route);
}
