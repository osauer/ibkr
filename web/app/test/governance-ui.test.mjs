import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const alertsSource = await readFile(new URL("../alerts.js", import.meta.url), "utf8");
const briefSource = await readFile(new URL("../brief.js", import.meta.url), "utf8");

class FakeElement {
  constructor() {
    this.children = [];
    this.className = "";
    this.dataset = {};
    this.disabled = false;
    this.hidden = false;
    this.textContent = "";
    this.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
  }
  append(...children) { this.children.push(...children); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = children; }
  addEventListener() {}
  setAttribute() {}
  scrollIntoView() {}
}

function loadAlerts() {
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, new FakeElement());
    return elements.get(id);
  };
  const state = {
    authenticated: true,
    governance: null,
    governanceRefreshInFlight: null,
    governanceRefreshTimer: null,
    governanceRefreshDueAt: 0,
    governanceRefreshTimerEnsureTrailing: false,
    governanceRefreshAfterFlight: false,
    governanceLastRefreshAt: 0,
    governanceRefreshSucceeded: null,
    governanceCutoverReceipt: null,
    safeNotificationTest: { busy: false, state: "", error: false },
    governanceCutoverReview: { busy: false, state: "", error: false },
    snapshot: {
      sources: { nudges: { state: "current" } },
      nudges: {
        as_of: "2026-07-01T12:00:00Z",
        candidates: [],
        source_health: { aggregate: "ready" },
        confirmed_flow_coverage: {
          coverage_from: "2026-07-01T00:00:00Z",
          pre_cutover_flows_unreviewed: true,
        },
      },
    },
  };
  const document = {
    createElement: () => new FakeElement(),
    getElementById: element,
    querySelectorAll: () => [],
  };
  const context = vm.createContext({
    console,
    Date,
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
    localStorage: { getItem: () => "", setItem() {} },
    navigator: {},
  });
  const executable = alertsSource
    .replace(/^import .*;\n/gm, "")
    .replace(/export \{([^}]+)\};\s*$/m, "globalThis.__exports = {$1};");
  vm.runInContext(executable, context, { filename: "alerts.js" });
  return { context, elements, exports: context.__exports, state };
}

function loadBrief() {
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, new FakeElement());
    return elements.get(id);
  };
  const state = {
    authenticated: true,
    activeTab: "monitor",
    snapshot: null,
  };
  const context = vm.createContext({
    console,
    Date,
    clearTimeout,
    setTimeout,
    requestAnimationFrame: (callback) => setTimeout(callback, 0),
    document: {
      visibilityState: "visible",
      addEventListener() {},
      createElement: () => new FakeElement(),
    },
    MutationObserver: undefined,
    state,
    $: element,
    money: () => "",
    readJSONOrText: async (res) => res.json(),
  });
  const executable = briefSource
    .replace(/^import .*;\n/gm, "")
    .replace(/export \{([^}]+)\};\s*$/m, "globalThis.__exports = {$1};");
  vm.runInContext(executable, context, { filename: "brief.js" });
  return { context, elements, exports: context.__exports, state };
}

function response(body, ok = true) {
  return { ok, async json() { return body; } };
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

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function visibleText(element) {
  return [element?.textContent || "", ...(element?.children || []).map(visibleText)].join(" ").trim();
}

function assertExactPost(call, path, body) {
  assert.equal(call.url, path);
  assert.equal(call.init.method, "POST");
  assert.equal(call.init.credentials, "include");
  assert.equal(call.init.headers["Content-Type"], "application/json");
  assert.deepEqual(Object.keys(call.init.headers), ["Content-Type"]);
  assert.deepEqual(JSON.parse(call.init.body), body);
  assert.deepEqual(Object.keys(JSON.parse(call.init.body)).sort(), Object.keys(body).sort());
}

test("a stale first governance GET is followed by a bounded refresh and concurrent triggers coalesce", async () => {
  const harness = loadAlerts();
  assert.equal(typeof harness.exports.scheduleGovernanceRefresh, "function");
  let calls = 0;
  let releaseFirst;
  const first = new Promise((resolve) => { releaseFirst = resolve; });
  harness.context.fetch = async () => {
    calls++;
    if (calls === 1) {
      await first;
      return response(governanceDTO({ marker: "stale" }));
    }
    return response(governanceDTO({ marker: "fresh" }));
  };

  const inFlight = harness.exports.refreshGovernance();
  harness.exports.scheduleGovernanceRefresh({ delayMs: 1, minIntervalMs: 0, ensureTrailing: true });
  harness.exports.scheduleGovernanceRefresh({ delayMs: 1, minIntervalMs: 0, ensureTrailing: true });
  await wait(5);
  assert.equal(calls, 1, "concurrent delayed triggers must not start a second in-flight GET");
  releaseFirst();
  await inFlight;
  await wait(10);
  assert.equal(calls, 2, "the first stale GET must not be the final governance refresh");
  assert.equal(harness.state.governance.marker, "fresh");
});

test("poll-source rendering preserves allowlisted state and fails candidates closed", () => {
  for (const source of [
    { state: "stale", reason: "poll_stale", updated_at: "2026-07-02T10:00:00Z", last_success_at: "2026-07-02T09:00:00Z" },
    { state: "not_observed", reason: "not_observed" },
    { state: "unavailable", reason: "transport_unavailable", updated_at: "2026-07-02T10:00:00Z", last_success_at: "2026-07-02T09:00:00Z" },
  ]) {
    const harness = loadAlerts();
    harness.state.snapshot.sources.nudges = source;
    harness.state.snapshot.nudges.candidates = [{ title: "Retained candidate", body: "must stay hidden", severity: "act", destination: "alerts" }];
    harness.exports.renderGovernance();
    assert.equal(harness.elements.get("governanceCurrentState").textContent, source.state);
    const sourceCopy = harness.elements.get("governanceSourceHealth").textContent;
    assert.match(sourceCopy, new RegExp(source.state));
    assert.match(sourceCopy, new RegExp(source.reason));
    if (source.updated_at) assert.match(sourceCopy, /updated/);
    if (source.last_success_at) assert.match(sourceCopy, /last successful/);
    assert.equal(visibleText(harness.elements.get("governanceCurrentList")).includes("Retained candidate"), false);
  }

  const unknown = loadAlerts();
  unknown.state.snapshot.sources.nudges = { state: "hostile-state", reason: "hostile-reason" };
  unknown.exports.renderGovernance();
  assert.equal(unknown.elements.get("governanceCurrentState").textContent, "unavailable");
  assert.match(unknown.elements.get("governanceSourceHealth").textContent, /unavailable.*invalid_health/);

  const unknownReason = loadAlerts();
  unknownReason.state.snapshot.sources.nudges = { state: "current", reason: "hostile-reason" };
  unknownReason.exports.renderGovernance();
  assert.match(unknownReason.elements.get("governanceSourceHealth").textContent, /poll current.*invalid_health/);
});

test("delivery health is aged and a failed refresh labels retained evidence", async () => {
  const harness = loadAlerts();
  harness.state.governance = governanceDTO({
    delivery_health: { state: "healthy", class: "push_service_accepted", updated_at: "2026-07-02T10:00:00Z" },
  });
  harness.state.governanceRefreshSucceeded = true;
  harness.exports.renderGovernance();
  assert.match(harness.elements.get("governanceDeliveryHealth").textContent, /healthy.*push_service_accepted.*updated/);

  harness.context.fetch = async () => response({}, false);
  assert.equal(await harness.exports.refreshGovernance(), false);
  assert.equal(harness.state.governanceRefreshSucceeded, false);
  assert.match(harness.elements.get("governanceDeliveryHealth").textContent, /retained.*refresh unavailable.*last known healthy.*updated/);

  harness.state.governanceRefreshSucceeded = true;
  harness.state.governance.delivery_health.updated_at = "invalid";
  harness.exports.renderGovernance();
  assert.match(harness.elements.get("governanceDeliveryHealth").textContent, /unavailable.*updated not observed/);
  assert.equal(harness.elements.get("governanceDeliveryHealth").textContent.includes("healthy"), false);

  harness.state.governance.delivery_health.updated_at = "2026-07-02T10:00:00Z";
  const retained = JSON.stringify(harness.state.governance);
  harness.context.fetch = async () => response({});
  assert.equal(await harness.exports.refreshGovernance(), false);
  assert.equal(JSON.stringify(harness.state.governance), retained, "malformed HTTP 200 must retain last-known governance evidence");
  assert.equal(harness.state.governanceRefreshSucceeded, false);
  assert.match(harness.elements.get("governanceDeliveryHealth").textContent, /retained.*refresh unavailable.*last known healthy/);

  harness.context.fetch = async () => response({ occurrences: [], attempts: [] });
  assert.equal(await harness.exports.refreshGovernance(), false);
  assert.equal(JSON.stringify(harness.state.governance), retained, "arrays-only HTTP 200 must retain last-known governance evidence");
  assert.equal(harness.state.governanceRefreshSucceeded, false);
  assert.match(harness.elements.get("governanceDeliveryHealth").textContent, /retained.*refresh unavailable.*last known healthy/);
});

test("safe notification POST is fixed and renders accepted, partial, suppressed, failed, and unavailable outcomes", async () => {
  for (const fixture of [
    { response: response({ state: "push_service_accepted", push_service_accepted: true }), copy: "Push-service accepted.", error: false },
    { response: response({ state: "partial_acceptance", push_service_accepted: true }), copy: "Partial push-service acceptance.", error: false },
    { response: response({ state: "suppressed", push_service_accepted: false }), copy: "Safe notification test suppressed.", error: false },
    { response: response({ state: "all_failed", push_service_accepted: false }), copy: "Safe notification test failed · all_failed.", error: true },
    { response: response({}, false), copy: "Safe notification test unavailable.", error: true },
  ]) {
    const harness = loadAlerts();
    const calls = [];
    harness.context.fetch = async (url, init = {}) => {
      if (url === "/api/push/test") {
        calls.push({ url, init });
        return fixture.response;
      }
      if (url === "/api/governance") return response(governanceDTO());
      throw new Error(`unintercepted request ${url}`);
    };
    await harness.exports.sendSafeNotificationTest();
    assert.equal(calls.length, 1);
    assertExactPost(calls[0], "/api/push/test", {});
    assert.equal(harness.state.safeNotificationTest.state, fixture.copy);
    assert.equal(harness.state.safeNotificationTest.error, fixture.error);
    assert.equal(harness.elements.get("safeNotificationTestStatus").textContent, fixture.copy);
  }
});

test("cutover receipt overlays stale snapshots until authority catches up while failed responses do not", async () => {
  const interceptCutover = (harness, cutoverResponse) => {
    const calls = [];
    harness.context.fetch = async (url, init = {}) => {
      if (url !== "/api/governance/cutover-review") throw new Error(`unintercepted request ${url}`);
      calls.push({ url, init });
      return cutoverResponse;
    };
    return calls;
  };
  const success = loadAlerts();
  assert.equal(typeof success.exports.applyGovernanceCutoverReceipt, "function");
  const receipt = {
    ok: true,
    already_reviewed: false,
    reviewed_at: "2026-07-02T00:00:00Z",
    coverage_from: "2026-07-01T00:00:00Z",
    evidence: "paired_device_foreground_render_review",
  };
  const successCalls = interceptCutover(success, response(receipt));
  await success.exports.sendGovernanceCutoverReview();
  clearTimeout(success.state.governanceRefreshTimer);
  success.state.governanceRefreshTimer = null;
  assert.equal(successCalls.length, 1);
  assertExactPost(successCalls[0], "/api/governance/cutover-review", {});
  assert.equal(success.state.governanceCutoverReview.state, "foreground render recorded");
  assert.deepEqual(JSON.parse(JSON.stringify(success.state.snapshot.nudges.confirmed_flow_coverage)), {
    coverage_from: receipt.coverage_from,
    pre_cutover_flows_unreviewed: false,
  });
  const staleSnapshot = success.exports.applyGovernanceCutoverOverlay({
    nudges: {
      as_of: "2026-07-01T23:59:00Z",
      confirmed_flow_coverage: { coverage_from: "2026-06-01T00:00:00Z", pre_cutover_flows_unreviewed: true },
    },
  });
  assert.equal(staleSnapshot.nudges.confirmed_flow_coverage.pre_cutover_flows_unreviewed, false);
  assert.equal(success.state.governanceCutoverReceipt.reviewed_at, receipt.reviewed_at);
  const caughtUp = success.exports.applyGovernanceCutoverOverlay({
    nudges: {
      as_of: "2026-07-02T00:00:01Z",
      confirmed_flow_coverage: { coverage_from: receipt.coverage_from, pre_cutover_flows_unreviewed: false },
    },
  });
  assert.equal(caughtUp.nudges.confirmed_flow_coverage.pre_cutover_flows_unreviewed, false);
  assert.equal(success.state.governanceCutoverReceipt, null);
  const reopened = success.exports.applyGovernanceCutoverOverlay({
    nudges: {
      as_of: "2026-07-02T00:01:00Z",
      confirmed_flow_coverage: { coverage_from: receipt.coverage_from, pre_cutover_flows_unreviewed: true },
    },
  });
  assert.equal(reopened.nudges.confirmed_flow_coverage.pre_cutover_flows_unreviewed, true);

  const invalidTiming = loadAlerts();
  assert.equal(invalidTiming.exports.applyGovernanceCutoverReceipt(receipt), true);
  const failClosed = invalidTiming.exports.applyGovernanceCutoverOverlay({
    nudges: {
      as_of: "invalid",
      confirmed_flow_coverage: { coverage_from: receipt.coverage_from, pre_cutover_flows_unreviewed: true },
    },
  });
  assert.equal(failClosed.nudges.confirmed_flow_coverage.pre_cutover_flows_unreviewed, true);
  assert.equal(invalidTiming.state.governanceCutoverReceipt, null);

  const already = loadAlerts();
  const alreadyCalls = interceptCutover(already, response({ ...receipt, already_reviewed: true }));
  await already.exports.sendGovernanceCutoverReview();
  clearTimeout(already.state.governanceRefreshTimer);
  already.state.governanceRefreshTimer = null;
  assert.equal(alreadyCalls.length, 1);
  assertExactPost(alreadyCalls[0], "/api/governance/cutover-review", {});
  assert.equal(already.state.governanceCutoverReview.state, "already recorded");
  assert.equal(already.state.snapshot.nudges.confirmed_flow_coverage.pre_cutover_flows_unreviewed, false);

  const failed = loadAlerts();
  const before = JSON.stringify(failed.state.snapshot.nudges.confirmed_flow_coverage);
  const failedCalls = interceptCutover(failed, response({ ok: false }, false));
  await failed.exports.sendGovernanceCutoverReview();
  assert.equal(failedCalls.length, 1);
  assertExactPost(failedCalls[0], "/api/governance/cutover-review", {});
  assert.equal(failed.state.governanceCutoverReview.error, true);
  assert.equal(JSON.stringify(failed.state.snapshot.nudges.confirmed_flow_coverage), before);

  const malformed = loadAlerts();
  const malformedBefore = JSON.stringify(malformed.state.snapshot.nudges.confirmed_flow_coverage);
  const malformedCalls = interceptCutover(malformed, response({ ok: true }));
  await malformed.exports.sendGovernanceCutoverReview();
  assert.equal(malformedCalls.length, 1);
  assertExactPost(malformedCalls[0], "/api/governance/cutover-review", {});
  assert.equal(malformed.state.governanceCutoverReview.error, true, "HTTP success without a typed receipt is not authority");
  assert.equal(JSON.stringify(malformed.state.snapshot.nudges.confirmed_flow_coverage), malformedBefore);

  assert.equal(failed.exports.applyGovernanceCutoverReceipt({ ok: false }), false);
  assert.equal(JSON.stringify(failed.state.snapshot.nudges.confirmed_flow_coverage), before);
  assert.equal(failed.state.governanceCutoverReceipt, null);
  assert.equal(malformed.state.governanceCutoverReceipt, null);
});

test("recent attempt rows expose allowlisted delivery facts without opaque identities", () => {
  const harness = loadAlerts();
  assert.equal(typeof harness.exports.governanceAttemptRows, "function");
  const rows = harness.exports.governanceAttemptRows([
    {
      occurrence_id: "private-occurrence", target_ref: "private-target-a", receipt_key: "private-receipt",
      class: "push_service_accepted", at: "2026-07-01T10:00:00Z", completed_at: "2026-07-01T10:00:01Z",
      transport_count: 1, raw_error: "private-error", endpoint: "https://evil.example",
    },
    {
      occurrence_id: "private-occurrence", target_ref: "private-target-b", class: "timeout_retry",
      at: "2026-07-01T10:01:00Z", retry_at: "2026-07-01T10:06:00Z", transport_count: 2,
    },
    {
      target_ref: "private-target-b", class: "target_retired", at: "2026-07-01T10:02:00Z",
      target_retired_at: "2026-07-01T10:03:00Z",
    },
    { target_ref: "private-target-c", class: "partial_acceptance", at: "2026-07-01T10:03:00Z", transport_count: 3 },
    { class: "no_subscription", at: "2026-07-01T10:04:00Z" },
    { class: "interrupted_uncertain", at: "2026-07-01T10:05:00Z", retry_at: "2026-07-01T10:10:00Z" },
  ]);
  const visible = JSON.stringify(rows);
  for (const expected of ["push_service_accepted", "timeout_retry", "target_retired", "partial_acceptance", "no_subscription", "interrupted_uncertain", "target 1", "target 2", "target 3", "retry", "retired", "transport count 2", "transport count 3"]) {
    assert.match(visible, new RegExp(expected));
  }

  const closed = JSON.stringify(harness.exports.governanceAttemptRows([
    { target_ref: "private-target-d", class: "http_rejected", at: "2026-07-01T10:06:00Z" },
    { target_ref: "private-target-e", class: "suppressed", at: "2026-07-01T10:07:00Z" },
    { target_ref: "private-target-f", class: "hostile-private-class", at: "2026-07-01T10:08:00Z" },
  ]));
  for (const expected of ["http_rejected", "suppressed", "unknown"]) {
    assert.match(closed, new RegExp(expected));
  }
  for (const forbidden of ["private-occurrence", "private-target", "private-receipt", "private-error", "evil.example", "hostile-private-class"]) {
    assert.equal(`${visible}${closed}`.includes(forbidden), false, `attempt display leaked ${forbidden}`);
  }
});

test("monthly mapper exposes all four dedicated visible states", () => {
  const brief = loadBrief().exports;
  assert.deepEqual([
    brief.monthlyPulseStatus({ status: "not_due" }),
    brief.monthlyPulseStatus({ status: "due" }),
    brief.monthlyPulseStatus({ status: "blocked" }),
    brief.monthlyPulseStatus({ status: "completed" }),
  ], ["not due", "due", "blocked by policy evidence", "completed this month"]);
});

test("monthly foreground-render scheduling sends one exact authenticated acknowledgement and keeps result copy distinct", async () => {
  const monthlyBrief = (fingerprint) => ({
    stamp_target: "monthly",
    brief_fingerprint: fingerprint,
    process: { monthly_pulse: { month: "2026-07", status: "due" } },
  });
  for (const fixture of [
    { body: { ok: true, kind: "monthly", already_stamped: false }, ok: true, copy: "foreground render recorded" },
    { body: { ok: true, kind: "monthly", already_stamped: true }, ok: true, copy: "foreground render already recorded" },
    { body: { error: "private-backend-error" }, ok: false, copy: "Monthly foreground render unavailable." },
  ]) {
    const harness = loadBrief();
    assert.equal(typeof harness.exports.scheduleBriefStamp, "function");
    const brief = monthlyBrief(`sha256:${fixture.copy}`);
    harness.state.snapshot = { brief };
    const calls = [];
    harness.context.fetch = async (url, init = {}) => {
      if (url !== "/api/brief/seen") throw new Error(`unintercepted request ${url}`);
      calls.push({ url, init });
      return response(fixture.body, fixture.ok);
    };
    harness.exports.scheduleBriefStamp(brief);
    harness.exports.scheduleBriefStamp(brief);
    await wait(10);
    assert.equal(calls.length, 1);
    assertExactPost(calls[0], "/api/brief/seen", {
      kind: "monthly",
      brief_fingerprint: brief.brief_fingerprint,
      month: "2026-07",
      evidence: "render",
    });
    assert.equal(harness.elements.get("briefAckStatus").textContent, fixture.copy);
    assert.equal(harness.elements.get("briefAckStatus").textContent.includes("private-backend-error"), false);
  }

  const morning = loadBrief();
  const brief = { stamp_target: "morning", brief_fingerprint: "sha256:morning", process: {} };
  morning.state.snapshot = { brief };
  const calls = [];
  morning.context.fetch = async (url, init = {}) => {
    if (url !== "/api/brief/seen") throw new Error(`unintercepted request ${url}`);
    calls.push({ url, init });
    return response({ ok: true, kind: "morning", day: "2026-07-02", already_stamped: false });
  };
  morning.exports.scheduleBriefStamp(brief);
  await wait(10);
  assert.equal(calls.length, 1);
  assertExactPost(calls[0], "/api/brief/seen", { kind: "morning", brief_fingerprint: brief.brief_fingerprint });
  assert.match(morning.elements.get("briefAckStatus").textContent, /morning artefact stamped/);
});
