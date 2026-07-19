import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const workerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");

function loadWorker(options = {}) {
  const listeners = new Map();
  const notifications = [];
  const opened = [];
  const clients = options.clients || [];
  const self = {
    addEventListener(type, listener) {
      listeners.set(type, listener);
    },
    skipWaiting: async () => {},
    registration: {
      async showNotification(title, notificationOptions) {
        notifications.push({ title, options: notificationOptions });
      },
    },
    clients: {
      claim: async () => {},
      async matchAll(matchOptions) {
        assert.equal(matchOptions.type, "window");
        assert.equal(matchOptions.includeUncontrolled, true);
        return clients;
      },
      async openWindow(route) {
        opened.push(route);
      },
    },
  };
  const context = vm.createContext({ self, console });
  vm.runInContext(workerSource, context, { filename: "service-worker.js" });
  return { listeners, notifications, opened };
}

async function dispatch(listener, event) {
  let pending;
  listener({
    ...event,
    waitUntil(promise) {
      pending = Promise.resolve(promise);
    },
  });
  await pending;
}

test("push payload uses governance display id before legacy Canary alert id", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), {
    data: { json: () => ({
      title: "Safe title", body: "Safe body", kind: "policy_drift",
      display_id: "gov-1111111111111111", alert_id: "legacy-canary", destination: "alerts",
      url: "https://evil.example/private?token=sentinel",
    }) },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(worker.notifications)), [{
    title: "Safe title",
    options: {
      body: "Safe body",
      data: { destination: "alerts" },
      tag: "gov-1111111111111111",
      badge: "/favicon-64.png",
      icon: "/icon-192.png",
    },
  }]);
  assert.equal(JSON.stringify(worker.notifications).includes("evil.example"), false);
});

test("distinct governance occurrence ids remain distinct while Canary keeps alert-id coalescing", async () => {
  const worker = loadWorker();
  for (const display_id of ["gov-aaaaaaaaaaaaaaaa", "gov-bbbbbbbbbbbbbbbb"]) {
    await dispatch(worker.listeners.get("push"), { data: { json: () => ({ display_id, kind: "monthly_pulse" }) } });
  }
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({ alert_id: "canary-stable" }) } });
  assert.deepEqual(worker.notifications.map((item) => item.options.tag), [
    "gov-aaaaaaaaaaaaaaaa", "gov-bbbbbbbbbbbbbbbb", "canary-stable",
  ]);
});

test("malformed payload and unknown destination fail closed to monitor with a fixed fallback tag", async () => {
  const worker = loadWorker();
  await dispatch(worker.listeners.get("push"), { data: { json: () => { throw new Error("malformed"); } } });
  await dispatch(worker.listeners.get("push"), { data: { json: () => "https://evil.example" } });
  await dispatch(worker.listeners.get("push"), { data: { json: () => ({ destination: "javascript:alert(1)", url: "/admin" }) } });
  for (const notification of worker.notifications) {
    assert.equal(notification.options.data.destination, "monitor");
    assert.deepEqual(Object.keys(notification.options.data), ["destination"]);
    assert.equal(notification.options.tag, "ibkr-canary");
    assert.equal(JSON.stringify(notification).includes("evil.example"), false);
    assert.equal(JSON.stringify(notification).includes("/admin"), false);
  }
});

test("notification click navigates and focuses an existing client", async () => {
  const navigated = [];
  let focused = 0;
  const worker = loadWorker({ clients: [{
    async navigate(route) { navigated.push(route); },
    async focus() { focused++; },
  }] });
  let closed = 0;
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "alerts", url: "https://evil.example" }, close() { closed++; } },
  });
  await dispatch(worker.listeners.get("notificationclick"), {
    notification: { data: { destination: "javascript:alert(1)", url: "https://evil.example/private" }, close() { closed++; } },
  });
  assert.deepEqual(navigated, ["/?tab=alerts", "/?tab=monitor"]);
  assert.equal(focused, 2);
  assert.equal(closed, 2);
  assert.deepEqual(worker.opened, []);
});

test("notification click opens only fixed monitor and alerts routes when no client exists", async () => {
  const worker = loadWorker();
  for (const destination of ["alerts", "monitor", "https://evil.example", null]) {
    await dispatch(worker.listeners.get("notificationclick"), {
      notification: { data: { destination, url: "file:///private/sentinel" }, close() {} },
    });
  }
  assert.deepEqual(worker.opened, ["/?tab=alerts", "/?tab=monitor", "/?tab=monitor", "/?tab=monitor"]);
  assert.equal(worker.opened.some((route) => /evil|file:|private/.test(route)), false);
});
