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
  event.waitUntil(self.registration.showNotification(title, {
    body,
    data: { destination },
    tag,
    badge: "/favicon-64.png",
    icon: "/icon-192.png",
  }));
});

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
