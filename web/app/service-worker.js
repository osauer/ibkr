self.addEventListener("push", (event) => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch {
    payload = {};
  }
  const title = payload.title || "ibkr canary";
  const options = {
    body: payload.body || "Open ibkr canary for details.",
    data: { url: payload.url || "/" },
    tag: payload.alert_id || "ibkr-canary",
    badge: "/favicon-64.png",
    icon: "/icon-192.png",
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = event.notification.data?.url || "/";
  event.waitUntil(clients.openWindow(url));
});
