self.addEventListener("push", (event) => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch {
    payload = {};
  }
  const title = payload.title || "ibkr canary";
  const options = {
    body: payload.body || "Open ibkr for details.",
    data: { url: payload.url || "/" },
    tag: payload.alert_id || "ibkr-canary",
    badge: "/icon.svg",
    icon: "/icon.svg",
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = event.notification.data?.url || "/";
  event.waitUntil(clients.openWindow(url));
});
