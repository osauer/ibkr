import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const alertsSource = await readFile(new URL("../alerts.js", import.meta.url), "utf8");
const serviceWorkerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");

class FakeClassList {
  constructor() { this.values = new Set(); }
  add(...values) { values.forEach((value) => this.values.add(value)); }
  remove(...values) { values.forEach((value) => this.values.delete(value)); }
  toggle(value, force) {
    const enabled = force === undefined ? !this.values.has(value) : Boolean(force);
    if (enabled) this.values.add(value); else this.values.delete(value);
    return enabled;
  }
  contains(value) { return this.values.has(value); }
}

class FakeElement {
  constructor() {
    this.attributes = new Map();
    this.children = [];
    this.classList = new FakeClassList();
    this.className = "";
    this.dataset = {};
    this.disabled = false;
    this.hidden = false;
    this.open = false;
    this.textContent = "";
    this.title = "";
  }
  append(...children) { this.children.push(...children); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = children; }
  addEventListener() {}
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  scrollIntoView() {}
}

function loadAlerts({ visibility = "visible", alertsPanelHidden = false } = {}) {
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, new FakeElement());
    return elements.get(id);
  };
  element("alertsTab").hidden = alertsPanelHidden;
  const modeButtons = ["none", "act_only", "watch_and_act"].map((mode) => {
    const button = new FakeElement();
    button.dataset.mode = mode;
    return button;
  });
  const storageWrites = [];
  const badgeCalls = [];
  const state = {
    activeTab: "monitor",
    alertFilter: "all",
    alertSettings: { mode: "watch_and_act" },
    alertSettingsUpdate: { busy: false, state: "", error: false },
    alerts: [],
    attention: null,
    attentionEpoch: 0,
    attentionReadInFlight: null,
    attentionRetryTimer: null,
    attentionStatus: { state: "", error: false },
    authenticated: true,
    clearedAlertFingerprint: "",
    governance: null,
    governanceCutoverReceipt: null,
    governanceCutoverReview: { busy: false, state: "", error: false },
    governanceRefreshAfterFlight: false,
    governanceRefreshDueAt: 0,
    governanceRefreshInFlight: null,
    governanceRefreshSucceeded: null,
    governanceRefreshTimer: null,
    governanceRefreshTimerEnsureTrailing: false,
    governanceLastRefreshAt: 0,
    pushInspection: { state: "status unavailable", busy: false },
    safeNotificationTest: { busy: false, state: "", error: false },
    selectedAlertID: null,
    snapshot: {
      canary: { fingerprint: { key: "canary-current" }, portfolio_fit: "low", portfolio: {} },
      sources: { nudges: { state: "current" } },
      nudges: {
        as_of: "2026-07-02T00:00:00Z",
        candidates: [],
        source_health: { aggregate: "ready" },
        confirmed_flow_coverage: { coverage_from: "2026-07-01T00:00:00Z", pre_cutover_flows_unreviewed: false },
      },
    },
    vapidPublicKey: "",
  };
  const document = {
    visibilityState: visibility,
    addEventListener() {},
    createElement: () => new FakeElement(),
    getElementById: element,
    querySelectorAll(selector) {
      if (selector === "#alertSegments button") return modeButtons;
      return [];
    },
  };
  const registration = { pushManager: { async getSubscription() { return null; } } };
  function Notification() {}
  Notification.permission = "default";
  const context = vm.createContext({
    console,
    Date,
    JSON,
    clearTimeout,
    setTimeout,
    document,
    state,
    $: element,
    b64urlToBytes: () => new Uint8Array(),
    heldStressItems: () => [],
    heldStressSummary: () => "",
    labelize: (value) => String(value),
    shortTime: () => "12:00",
    localStorage: {
      getItem: () => "",
      setItem(key, value) { storageWrites.push([key, value]); },
    },
    navigator: {
      serviceWorker: { ready: Promise.resolve(registration) },
      setAppBadge(count) { badgeCalls.push(["set", count]); return Promise.resolve(); },
      clearAppBadge() { badgeCalls.push(["clear"]); return Promise.resolve(); },
    },
    Notification,
    PushManager: function PushManager() {},
  });
  const executable = alertsSource
    .replace(/^import .*;\n/gm, "")
    .replace(/export \{([^}]+)\};\s*$/m, "globalThis.__exports = {$1};");
  vm.runInContext(executable, context, { filename: "alerts.js" });
  return { badgeCalls, context, document, elements, exports: context.__exports, modeButtons, registration, state, storageWrites };
}

function response(body, ok = true) {
  return { ok, async json() { return body; }, async text() { return JSON.stringify(body); } };
}

function delayedJSONResponse() {
  let markStarted;
  let releaseBody;
  const started = new Promise((resolve) => { markStarted = resolve; });
  const body = new Promise((resolve) => { releaseBody = resolve; });
  return {
    response: {
      ok: true,
      async json() {
        markStarted();
        return body;
      },
    },
    started,
    release: releaseBody,
  };
}

function attention(unreadCount, highWaterSeq, readThroughSeq, unreadRefs) {
  return { unread_count: unreadCount, high_water_seq: highWaterSeq, read_through_seq: readThroughSeq, unread_refs: unreadRefs };
}

function governanceDTO(overrides = {}) {
  return {
    candidates: [],
    source_health: {},
    poll_source: {},
    occurrences: [],
    attempts: [],
    attempt_aggregate: {},
    health_aggregate: {},
    delivery_health: {},
    diagnostic: {},
    ...overrides,
  };
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

function visibleText(element) {
  return [element?.textContent || "", ...(element?.children || []).map(visibleText)].join(" ").trim();
}

function assertExactJSONCall(call, { method, url, body }) {
  assert.equal(call.url, url);
  assert.equal(call.init.method, method);
  assert.equal(call.init.credentials, "include");
  assert.deepEqual(Object.keys(call.init.headers), ["Content-Type"]);
  assert.equal(call.init.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(call.init.body), body);
}

test("unread state mirrors onto the app icon badge and clears with it", () => {
  const harness = loadAlerts();
  harness.exports.applyAttention(attention(2, 5, 3, [{ kind: "canary", id: "a" }, { kind: "governance", id: "g" }]));
  assert.deepEqual(harness.badgeCalls.at(-1), ["set", 2]);
  harness.exports.applyAttention(attention(0, 5, 5, []));
  assert.deepEqual(harness.badgeCalls.at(-1), ["clear"]);
});

test("alerts entry marks read only after a continuous dwell", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  harness.state.attentionDwellMs = 30;
  const posts = [];
  harness.context.fetch = async (url, init = {}) => {
    if (url === "/api/attention/read") {
      posts.push(JSON.parse(init.body));
      return response(attention(0, 7, 7, []));
    }
    if (url === "/api/alerts") return response([{ id: "alert-7" }]);
    if (url === "/api/governance") return response(governanceDTO());
    return response(attention(1, 7, 6, [{ kind: "canary", id: "alert-7" }]));
  };
  assert.equal(harness.exports.handleAttentionContextChange(), true);
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.deepEqual(posts, [], "a resume flash must not read");
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.deepEqual(posts, [{ through_seq: 7 }], "a held view reads after the dwell");
});

test("leaving alerts before the dwell cancels the pending read", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  harness.state.attentionDwellMs = 30;
  const posts = [];
  harness.context.fetch = async (url) => {
    if (url === "/api/attention/read") {
      posts.push(1);
      return response(attention(0, 7, 7, []));
    }
    return response(attention(1, 7, 6, [{ kind: "canary", id: "alert-7" }]));
  };
  assert.equal(harness.exports.handleAttentionContextChange(), true);
  harness.state.activeTab = "monitor";
  await harness.exports.handleAttentionContextChange();
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.deepEqual(posts, [], "a pass-through visit never reads");
});

test("interaction inside alerts reads immediately without waiting out the dwell", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  harness.state.attentionDwellMs = 5000;
  const posts = [];
  harness.context.fetch = async (url, init = {}) => {
    if (url === "/api/attention/read") {
      posts.push(JSON.parse(init.body));
      return response(attention(0, 7, 7, []));
    }
    if (url === "/api/alerts") return response([{ id: "alert-7" }]);
    if (url === "/api/governance") return response(governanceDTO());
    return response(attention(1, 7, 6, [{ kind: "canary", id: "alert-7" }]));
  };
  assert.equal(harness.exports.handleAttentionContextChange(), true, "dwell armed");
  assert.equal(await harness.exports.acknowledgeAttentionNow(), true);
  assert.deepEqual(posts, [{ through_seq: 7 }]);
});

test("attention is validated as exact opaque server state and badge rendering never recounts histories", () => {
  const harness = loadAlerts();
  assert.equal(typeof harness.exports.validateAttention, "function");
  assert.equal(typeof harness.exports.applyAttention, "function");
  const one = attention(1, 4, 3, [{ kind: "canary", id: "alert-1" }]);
  assert.deepEqual(plain(harness.exports.validateAttention(one)), one);
  assert.equal(harness.exports.applyAttention(one), true);
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "1");
  assert.equal(harness.elements.get("alertUnreadBadge").hidden, false);
  assert.equal(harness.elements.get("alertUnreadBadge").getAttribute("aria-hidden"), "true");
  assert.equal(harness.elements.get("tabAlerts").getAttribute("aria-label"), "Alerts, 1 unread");

  harness.state.alerts = Array.from({ length: 100 }, (_, index) => ({ id: `history-${index}` }));
  harness.state.governance = governanceDTO({ occurrences: Array.from({ length: 100 }, (_, index) => ({ display_id: `gov-${index}` })) });
  harness.exports.renderAttention();
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "1", "history arrays are not unread authority");

  assert.equal(harness.exports.applyAttention(attention(100, 200, 100, Array.from({ length: 100 }, (_, index) => ({ kind: "governance", id: `gov-${index}` })))), true);
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "99+");
  assert.equal(harness.elements.get("tabAlerts").getAttribute("aria-label"), "Alerts, 100 unread");
  assert.equal(harness.exports.applyAttention(attention(0, 200, 200, [])), true);
  assert.equal(harness.elements.get("alertUnreadBadge").hidden, true);
  assert.equal(harness.elements.get("tabAlerts").getAttribute("aria-label"), "Alerts, no unread alerts");

  for (const malformed of [
    null,
    {},
    attention(-1, 1, 0, []),
    attention(0, 1.5, 0, []),
    attention(0, 1, 2, []),
    attention(1, 1, 0, []),
    attention(1, 1, 0, [{ kind: "unknown", id: "x" }]),
    attention(1, 1, 0, [{ kind: "canary", id: "" }]),
    attention(2, 2, 0, [{ kind: "canary", id: "same" }, { kind: "canary", id: "same" }]),
    attention(1, 1, 0, [{ kind: "canary", id: "x", account: "private-account" }]),
    { ...one, receipt: "private-receipt" },
  ]) {
    assert.equal(harness.exports.validateAttention(malformed), null);
  }
  assert.deepEqual(harness.storageWrites, [], "attention state must never use browser storage");
});

test("visible rendered Alerts acknowledges only the coherent matched watermark after full histories render", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  const coherent = attention(2, 12, 10, [
    { kind: "canary", id: "alert-12" },
    { kind: "governance", id: "gov-12" },
  ]);
  const calls = [];
  harness.context.fetch = async (url, init = {}) => {
    calls.push({ url, init });
    if (url === "/api/attention") return response(coherent);
    if (url === "/api/alerts") return response([{ id: "alert-12", title: "Watch", body: "Review", severity: "watch" }]);
    if (url === "/api/governance") return response(governanceDTO({ occurrences: [{ display_id: "gov-12", title: "Review", body: "Process", severity: "act" }] }));
    if (url === "/api/attention/read") return response(attention(0, 12, 12, []));
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await harness.exports.acknowledgeAttention(), true);
  assert.deepEqual(calls.slice(0, 3).map((call) => call.url).sort(), ["/api/alerts", "/api/attention", "/api/governance"]);
  assertExactJSONCall(calls[3], { method: "POST", url: "/api/attention/read", body: { through_seq: 12 } });
  assert.equal(harness.elements.get("alertHistoryList").children.length, 1);
  assert.equal(harness.elements.get("governanceHistoryList").children.length, 1);
  assert.equal(harness.elements.get("alertUnreadBadge").hidden, true);
  assert.deepEqual(harness.storageWrites, []);
});

test("missing unread refs fail closed without POST and hidden or unrendered navigation never reads", async () => {
  const missing = loadAlerts();
  missing.state.activeTab = "alerts";
  const calls = [];
  missing.context.fetch = async (url, init = {}) => {
    calls.push({ url, init });
    if (url === "/api/attention") return response(attention(1, 9, 8, [{ kind: "governance", id: "missing-gov" }]));
    if (url === "/api/alerts") return response([]);
    if (url === "/api/governance") return response(governanceDTO());
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await missing.exports.acknowledgeAttention({ retry: false }), false);
  assert.equal(calls.some((call) => call.url === "/api/attention/read"), false);
  assert.equal(missing.elements.get("alertUnreadBadge").textContent, "1");
  assert.match(missing.elements.get("attentionStatus").textContent, /could not be matched/i);

  for (const options of [
    { visibility: "hidden", alertsPanelHidden: false },
    { visibility: "visible", alertsPanelHidden: true },
  ]) {
    const harness = loadAlerts(options);
    harness.state.activeTab = "alerts";
    let fetches = 0;
    harness.context.fetch = async () => { fetches++; throw new Error("must not fetch"); };
    assert.equal(await harness.exports.acknowledgeAttention(), false);
    assert.equal(fetches, 0);
  }
});

test("an older matched watermark cannot swallow a newer event returned after history rendering", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  const calls = [];
  harness.context.fetch = async (url, init = {}) => {
    calls.push({ url, init });
    if (url === "/api/attention") return response(attention(1, 5, 4, [{ kind: "canary", id: "alert-5" }]));
    if (url === "/api/alerts") return response([{ id: "alert-5" }, { id: "alert-6" }]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") return response(attention(1, 6, 5, [{ kind: "canary", id: "alert-6" }]));
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await harness.exports.acknowledgeAttention(), true);
  assertExactJSONCall(calls.find((call) => call.url === "/api/attention/read"), { method: "POST", url: "/api/attention/read", body: { through_seq: 5 } });
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "1");
  assert.equal(harness.state.attention.high_water_seq, 6);

  const race = loadAlerts();
  let releaseOld;
  const oldResponse = new Promise((resolve) => { releaseOld = resolve; });
  let attentionGets = 0;
  race.context.fetch = async (url) => {
    if (url === "/api/attention") {
      attentionGets++;
      if (attentionGets === 1) return oldResponse;
      return response(attention(0, 8, 8, []));
    }
    if (url === "/api/alerts") return response([]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") return response(attention(0, 8, 8, []));
    throw new Error(`unintercepted request ${url}`);
  };
  const oldRefresh = race.exports.refreshAttention();
  race.state.activeTab = "alerts";
  assert.equal(await race.exports.acknowledgeAttention(), true);
  releaseOld(response(attention(1, 7, 6, [{ kind: "canary", id: "stale-alert" }])));
  assert.equal(await oldRefresh, false);
  assert.equal(race.elements.get("alertUnreadBadge").hidden, true, "an older GET must not resurrect unread after acknowledgement");

  const delayedRefresh = loadAlerts();
  const staleRefreshBody = delayedJSONResponse();
  let refreshGets = 0;
  delayedRefresh.context.fetch = async (url) => {
    if (url !== "/api/attention") throw new Error(`unintercepted request ${url}`);
    refreshGets++;
    return refreshGets === 1 ? staleRefreshBody.response : response(attention(0, 9, 9, []));
  };
  const staleRefresh = delayedRefresh.exports.refreshAttention();
  await staleRefreshBody.started;
  assert.equal(await delayedRefresh.exports.refreshAttention(), true);
  staleRefreshBody.release(attention(1, 7, 6, [{ kind: "canary", id: "stale-body" }]));
  assert.equal(await staleRefresh, false);
  assert.equal(delayedRefresh.state.attention.high_water_seq, 9, "a delayed refresh JSON body must not overwrite newer attention");
  assert.equal(delayedRefresh.elements.get("alertUnreadBadge").hidden, true);

  const delayedAcknowledgement = loadAlerts();
  delayedAcknowledgement.state.activeTab = "alerts";
  const staleAttentionBody = delayedJSONResponse();
  let acknowledgementGets = 0;
  let staleHistoryFetches = 0;
  let staleMarkReadCalls = 0;
  delayedAcknowledgement.context.fetch = async (url) => {
    if (url === "/api/attention") {
      acknowledgementGets++;
      return acknowledgementGets === 1 ? staleAttentionBody.response : response(attention(0, 10, 10, []));
    }
    if (url === "/api/alerts" || url === "/api/governance") {
      staleHistoryFetches++;
      throw new Error("stale acknowledgement must not fetch histories");
    }
    if (url === "/api/attention/read") {
      staleMarkReadCalls++;
      throw new Error("stale acknowledgement must not mark read");
    }
    throw new Error(`unintercepted request ${url}`);
  };
  const staleAcknowledgement = delayedAcknowledgement.exports.acknowledgeAttention({ retry: false });
  await staleAttentionBody.started;
  assert.equal(await delayedAcknowledgement.exports.refreshAttention(), true);
  staleAttentionBody.release(attention(1, 8, 7, [{ kind: "canary", id: "stale-ack-body" }]));
  assert.equal(await staleAcknowledgement, false);
  assert.equal(delayedAcknowledgement.state.attention.high_water_seq, 10);
  assert.equal(staleHistoryFetches, 0);
  assert.equal(staleMarkReadCalls, 0, "a delayed stale attention body must not produce a read attempt");

  const delayedReadReceipt = loadAlerts();
  delayedReadReceipt.state.activeTab = "alerts";
  const staleReadBody = delayedJSONResponse();
  let readAttentionGets = 0;
  delayedReadReceipt.context.fetch = async (url) => {
    if (url === "/api/attention") {
      readAttentionGets++;
      return response(readAttentionGets === 1
        ? attention(1, 11, 10, [{ kind: "canary", id: "alert-11" }])
        : attention(1, 12, 11, [{ kind: "canary", id: "alert-12" }]));
    }
    if (url === "/api/alerts") return response([{ id: "alert-11" }]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") return staleReadBody.response;
    throw new Error(`unintercepted request ${url}`);
  };
  const delayedRead = delayedReadReceipt.exports.acknowledgeAttention({ retry: false });
  await staleReadBody.started;
  assert.equal(await delayedReadReceipt.exports.refreshAttention(), true);
  staleReadBody.release(attention(0, 11, 11, []));
  assert.equal(await delayedRead, false);
  assert.equal(delayedReadReceipt.state.attention.high_water_seq, 12, "a delayed read-receipt body must not overwrite newer attention");
  assert.equal(delayedReadReceipt.elements.get("alertUnreadBadge").textContent, "1");

  const delayedHistories = loadAlerts();
  delayedHistories.state.activeTab = "alerts";
  const staleAlertsBody = delayedJSONResponse();
  const staleGovernanceBody = delayedJSONResponse();
  let historyMarkReadCalls = 0;
  delayedHistories.context.fetch = async (url) => {
    if (url === "/api/attention") return response(attention(1, 13, 12, [{ kind: "canary", id: "stale-history" }]));
    if (url === "/api/alerts") return staleAlertsBody.response;
    if (url === "/api/governance") return staleGovernanceBody.response;
    if (url === "/api/attention/read") {
      historyMarkReadCalls++;
      return response(attention(0, 13, 13, []));
    }
    throw new Error(`unintercepted request ${url}`);
  };
  const staleHistoryAcknowledgement = delayedHistories.exports.acknowledgeAttention({ retry: false });
  await Promise.all([staleAlertsBody.started, staleGovernanceBody.started]);
  delayedHistories.state.attentionEpoch += 1;
  delayedHistories.state.alerts = [{ id: "new-history" }];
  delayedHistories.state.governance = governanceDTO({ occurrences: [{ display_id: "new-governance" }] });
  staleAlertsBody.release([{ id: "stale-history" }]);
  staleGovernanceBody.release(governanceDTO());
  assert.equal(await staleHistoryAcknowledgement, false);
  assert.equal(delayedHistories.state.alerts[0].id, "new-history", "delayed stale Canary history must not replace newer state");
  assert.equal(delayedHistories.state.governance.occurrences[0].display_id, "new-governance", "delayed stale governance must not replace newer state");
  assert.equal(historyMarkReadCalls, 0);
});

test("acknowledgement exposes complete unread evidence instead of silently reading a filtered-out row", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  harness.state.alertFilter = "warnings";
  let markReadCalls = 0;
  harness.context.fetch = async (url) => {
    if (url === "/api/attention") return response(attention(1, 14, 13, [{ kind: "canary", id: "info-14" }]));
    if (url === "/api/alerts") return response([{ id: "info-14", title: "Informational evidence", body: "Visible before acknowledgement", severity: "info" }]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") {
      markReadCalls++;
      return response(attention(0, 14, 14, []));
    }
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await harness.exports.acknowledgeAttention({ retry: false }), true);
  assert.equal(harness.state.alertFilter, "all", "complete unread evidence must render through the All filter");
  assert.match(visibleText(harness.elements.get("alertHistoryList")), /Informational evidence/);
  assert.equal(markReadCalls, 1);
});

test("failed mark-read retains server unread state and reconciles with a fresh GET", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  let attentionGets = 0;
  harness.context.fetch = async (url) => {
    if (url === "/api/attention") {
      attentionGets++;
      return response(attentionGets === 1
        ? attention(1, 7, 6, [{ kind: "canary", id: "alert-7" }])
        : attention(2, 8, 6, [{ kind: "canary", id: "alert-7" }, { kind: "governance", id: "gov-8" }]));
    }
    if (url === "/api/alerts") return response([{ id: "alert-7" }]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") return response({}, false);
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await harness.exports.acknowledgeAttention({ retry: false }), false);
  assert.equal(attentionGets, 2);
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "2");
  assert.match(harness.elements.get("attentionStatus").textContent, /not marked read/i);

  const rejectedJSON = loadAlerts();
  rejectedJSON.state.activeTab = "alerts";
  let rejectedJSONAttentionGets = 0;
  rejectedJSON.context.fetch = async (url) => {
    if (url === "/api/attention") {
      rejectedJSONAttentionGets++;
      return response(rejectedJSONAttentionGets === 1
        ? attention(1, 17, 16, [{ kind: "canary", id: "alert-17" }])
        : attention(2, 18, 16, [{ kind: "canary", id: "alert-17" }, { kind: "governance", id: "gov-18" }]));
    }
    if (url === "/api/alerts") return response([{ id: "alert-17" }]);
    if (url === "/api/governance") return response(governanceDTO());
    if (url === "/api/attention/read") return { ok: true, async json() { throw new Error("non-JSON read receipt"); } };
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await rejectedJSON.exports.acknowledgeAttention({ retry: false }), false);
  assert.equal(rejectedJSONAttentionGets, 2, "a rejected read-receipt body must immediately reconcile with a fresh GET");
  assert.equal(rejectedJSON.state.attention.high_water_seq, 18);
  assert.equal(rejectedJSON.elements.get("alertUnreadBadge").textContent, "2", "the last server unread state must be retained");
  assert.match(rejectedJSON.elements.get("attentionStatus").textContent, /not marked read.*reconciled/i);
});

test("malformed attention fails closed and notification click routing alone has no read side effect", async () => {
  const harness = loadAlerts();
  harness.state.activeTab = "alerts";
  harness.exports.applyAttention(attention(1, 2, 1, [{ kind: "canary", id: "alert-2" }]));
  harness.context.fetch = async (url) => {
    if (url === "/api/attention") return response({ unread_count: 0 });
    throw new Error(`unintercepted request ${url}`);
  };
  assert.equal(await harness.exports.acknowledgeAttention({ retry: false }), false);
  assert.equal(harness.elements.get("alertUnreadBadge").textContent, "1", "last valid server state is retained");
  assert.match(harness.elements.get("attentionStatus").textContent, /unavailable/i);
  // The worker may GET /api/attention (icon-badge truth after a push) but
  // must never reference the read-acknowledge mutation; the click path is
  // additionally proven fetch-free in service-worker.test.mjs.
  assert.equal(serviceWorkerSource.includes("/api/attention/read"), false);
  assert.match(serviceWorkerSource, /fetch\("\/api\/attention", \{ credentials: "include" \}\)/);
  assert.match(serviceWorkerSource, /\?tab=alerts/);
});

test("each notification level makes one exact app-owned PUT and strict failures roll back with safe copy", async () => {
  for (const mode of ["none", "act_only", "watch_and_act"]) {
    const harness = loadAlerts();
    const calls = [];
    harness.context.fetch = async (url, init = {}) => {
      calls.push({ url, init });
      return response({ mode });
    };
    assert.equal(await harness.exports.setAlertMode(mode), true);
    assert.equal(calls.length, 1);
    assertExactJSONCall(calls[0], { method: "PUT", url: "/api/alerts/settings", body: { mode } });
    assert.equal(calls.some((call) => call.url === "/api/settings"), false);
    assert.equal(harness.state.alertSettings.mode, mode);
    assert.match(harness.elements.get("alertSettingsStatus").textContent, /saved/i);
  }

  for (const fixture of [response({}, false), response({ mode: "hostile" }), response({ mode: "none", private: "receipt" })]) {
    const harness = loadAlerts();
    harness.state.alertSettings = { mode: "act_only" };
    harness.context.fetch = async (url) => {
      assert.equal(url, "/api/alerts/settings");
      return fixture;
    };
    assert.equal(await harness.exports.setAlertMode("none"), false);
    assert.equal(harness.state.alertSettings.mode, "act_only");
    assert.equal(harness.elements.get("alertSettingsStatus").textContent, "Delivery level was not changed.");
    assert.equal(harness.elements.get("alertSettingsStatus").textContent.includes("private"), false);
  }
});

test("device status distinguishes permission from an actual browser subscription", async () => {
  const harness = loadAlerts();
  assert.equal(await harness.exports.refreshPushState(), "permission not granted");
  harness.context.Notification.permission = "denied";
  assert.equal(await harness.exports.refreshPushState(), "permission blocked");
  harness.context.Notification.permission = "granted";
  assert.equal(await harness.exports.refreshPushState(), "permission granted but not subscribed");
  harness.registration.pushManager.getSubscription = async () => ({ endpoint: "private-endpoint" });
  assert.equal(await harness.exports.refreshPushState(), "browser subscribed");
  assert.equal(harness.elements.get("pushState").textContent, "browser subscribed");
  assert.equal(harness.elements.get("pushState").textContent.includes("private-endpoint"), false);
});
