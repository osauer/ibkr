import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

globalThis.localStorage = {
  getItem() { return null; },
  setItem() {},
};

const { state } = await import("../state.js");
const {
  AlertInboxV2ContractError,
  alertInboxV2CanAssertClear,
  alertInboxV2OccurrenceView,
  alertInboxV2RenderedOccurrences,
  alertInboxV2View,
  ingestAlertInboxV2,
  ingestAlertInboxV2Event,
  validateAlertInboxV2,
} = await import("../alert-inbox-v2.js");
const lifecycleSource = await readFile(new URL("../lifecycle.js", import.meta.url), "utf8");
const stateSource = await readFile(new URL("../state.js", import.meta.url), "utf8");
const serviceWorkerSource = await readFile(new URL("../service-worker.js", import.meta.url), "utf8");
const canarySource = await readFile(new URL("../canary.js", import.meta.url), "utf8");

function sourceFunction(source, name) {
  const marker = `function ${name}(`;
  const start = source.indexOf(marker);
  assert.notEqual(start, -1, `missing ${name}`);
  const next = source.indexOf("\nfunction ", start + marker.length);
  return source.slice(start, next === -1 ? source.length : next);
}

const regimeViewContext = vm.createContext({
  cleanDetail(value) { return value ? String(value).replaceAll("_", " ") : "--"; },
  labelize(value) { return String(value || "--"); },
});
vm.runInContext([
  sourceFunction(canarySource, "regimeAuthorityReasonLabel"),
  sourceFunction(canarySource, "regimeAuthorityView"),
  sourceFunction(canarySource, "regimePresentationPosture"),
  sourceFunction(canarySource, "marketRegimeLabel"),
  sourceFunction(canarySource, "regimeAuthorityLabel"),
  sourceFunction(canarySource, "regimeAuthorityStatusLine"),
  sourceFunction(canarySource, "regimeWeatherClass"),
  "globalThis.__regimeViews = { regimeAuthorityLabel, regimeAuthorityStatusLine, regimeAuthorityView, regimePresentationPosture, regimeWeatherClass };",
].join("\n"), regimeViewContext);
const {
  regimeAuthorityLabel,
  regimeAuthorityStatusLine,
  regimeAuthorityView,
  regimePresentationPosture,
  regimeWeatherClass,
} = regimeViewContext.__regimeViews;

const at = "2026-07-20T18:00:00Z";

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function occurrence(overrides = {}) {
  return {
    display_id: "alert-0123456789abcdef",
    source: "canary",
    kind: "portfolio_risk",
    state: "open",
    severity: "watch",
    delivery_preference: "unapproved",
    evidence_health: "current",
    destination: "alerts",
    evidence_as_of: at,
    state_changed_at: at,
    first_seen_at: at,
    last_seen_at: at,
    ended_at: null,
    end_reason: null,
    attention_seq: 1,
    ...overrides,
  };
}

function inbox(overrides = {}) {
  const item = occurrence();
  return {
    schema_version: "alert-inbox-v2",
    authority: "shadow",
    initialized: true,
    generation: 1,
    as_of: at,
    current_state: "active",
    coverage: {
      state: "complete",
      freshness: "current",
      as_of: at,
      expected_sources: ["canary"],
      covered_sources: ["canary"],
    },
    occurrences: [item],
    attention: {
      unread_count: 1,
      high_water_seq: 1,
      read_through_seq: 0,
      unread_refs: [{ display_id: item.display_id, source: item.source, kind: item.kind }],
    },
    delivery_health: { state: "shadow", class: "policy_unapproved", updated_at: at },
    ...overrides,
  };
}

function uninitialized(overrides = {}) {
  return {
    schema_version: "alert-inbox-v2",
    authority: "shadow",
    initialized: false,
    generation: 0,
    as_of: null,
    current_state: null,
    coverage: null,
    occurrences: [],
    attention: { unread_count: 0, high_water_seq: 0, read_through_seq: 0, unread_refs: [] },
    delivery_health: { state: "shadow", class: "policy_unapproved", updated_at: null },
    ...overrides,
  };
}

function quarantined(overrides = {}) {
  return uninitialized({
    generation: Number.MAX_SAFE_INTEGER,
    delivery_health: { state: "unavailable", class: "invalid_persisted_state", updated_at: at },
    ...overrides,
  });
}

function clearInbox(overrides = {}) {
  return inbox({
    current_state: "clear",
    occurrences: [],
    attention: { unread_count: 0, high_water_seq: 1, read_through_seq: 1, unread_refs: [] },
    ...overrides,
  });
}

function resetState() {
  state.alertInboxV2 = null;
  state.alertInboxV2FeedValid = null;
  state.alertInboxV2FeedError = "";
}

test("bootstrap and SSE are the only lifecycle ingestion wires", () => {
  assert.match(stateSource, /alertInboxV2: null/);
  assert.match(stateSource, /alertInboxV2FeedValid: null/);
  assert.match(lifecycleSource, /ingestAlertInboxV2\(data\.alert_inbox_v2\)/);
  assert.match(lifecycleSource, /"alert_inbox_v2"/);
  assert.match(lifecycleSource, /if \(type === "alert_inbox_v2"\) \{\s*ingestAlertInboxV2Event\(event\.data\);/);
  assert.match(lifecycleSource, /ingestAlertInboxV2Event\(event\.data\);\s*renderAlertInboxV2\(\);/);
  assert.doesNotMatch(lifecycleSource, /fetch\("\/api\/alert-inbox-v2/);
  assert.doesNotMatch(serviceWorkerSource, /alert_inbox_v2|alert-inbox-v2/i);
});

test("regime authority health immediately overrides only presentation tone", () => {
  const posture = { label: "Constructive regime", tone: "normal", severity: "observe" };
  const lastSuccess = "2026-07-20T17:59:59Z";
  const stale = {
    regime: {
      authority_health: {
        status: "stale",
        refreshing: true,
        last_success_at: lastSuccess,
        last_success_age_seconds: 1,
        failure_code: "refresh_failed",
      },
    },
    sources: { regime: { state: "current", last_success_at: at } },
  };
  const authority = regimeAuthorityView(stale);
  assert.equal(authority.status, "stale", "typed stale must not wait for an indicator-age budget");
  assert.equal(authority.lastSuccessAt, lastSuccess);
  const presented = regimePresentationPosture(posture, authority);
  assert.equal(presented.label, posture.label, "the daemon-authored verdict is retained");
  assert.equal(presented.severity, posture.severity);
  assert.equal(presented.tone, "data_quality");
  assert.equal(regimeAuthorityLabel(posture, authority), "Last known · Constructive regime");
  assert.equal(regimeWeatherClass(presented.tone), "amber");
  const line = regimeAuthorityStatusLine(stale, posture);
  assert.match(line.summary, /Last-known regime · stale/);
  assert.match(line.detail, /canonical last-good verdict is retained as context/i);
  assert.match(line.detail, /refresh failed/);

  const rollback = clone(stale);
  rollback.regime.authority_health.failure_code = "clock_invalid";
  assert.match(regimeAuthorityStatusLine(rollback, posture).detail, /daemon clock is behind the last successful Regime commit/i);
});

test("unavailable app source outranks cached fresh authority without replacing its verdict", () => {
  const posture = { label: "Confirmed stress", tone: "stress" };
  const unavailable = {
    regime: {
      authority_health: {
        status: "fresh",
        refreshing: false,
        last_success_at: at,
        last_success_age_seconds: 0,
      },
    },
    sources: { regime: { state: "unavailable", reason: "transport_unavailable", last_success_at: at } },
  };
  const authority = regimeAuthorityView(unavailable);
  assert.equal(authority.status, "unavailable");
  assert.equal(regimePresentationPosture(posture, authority).label, posture.label);
  assert.equal(regimeAuthorityLabel(posture, authority), "Last known · Confirmed stress");
  assert.equal(regimeWeatherClass(regimePresentationPosture(posture, authority).tone), "amber");
  const line = regimeAuthorityStatusLine(unavailable, posture);
  assert.match(line.summary, /Last-known regime · authority unavailable/);
  assert.match(line.detail, /context only/);
  assert.match(line.detail, /daemon transport is unavailable/);

  const fresh = regimeAuthorityView({
    regime: { authority_health: { status: "fresh", refreshing: false, last_success_at: at, last_success_age_seconds: 0 } },
    sources: { regime: { state: "current", last_success_at: at } },
  });
  assert.equal(fresh.degraded, false);
  assert.equal(regimePresentationPosture(posture, fresh), posture);
  assert.equal(regimeAuthorityLabel(posture, fresh), posture.label);
});

test("validator accepts only the recursive exact public contract", () => {
  assert.equal(validateAlertInboxV2(inbox()).generation, 1);
  assert.equal(validateAlertInboxV2(uninitialized()).initialized, false);

  const cases = [
    ["top-level extra key", (value) => { value.private = true; }],
    ["coverage extra key", (value) => { value.coverage.source_watermarks = {}; }],
    ["occurrence private key", (value) => { value.occurrences[0].occurrence_key = "private"; }],
    ["attention ref extra key", (value) => { value.attention.unread_refs[0].attention_seq = 1; }],
    ["delivery health acceptance", (value) => { value.delivery_health.last_push_service_acceptance_at = at; }],
    ["invalid source enum", (value) => { value.occurrences[0].source = "broker"; }],
    ["invalid timestamp", (value) => { value.as_of = "2026-02-30T18:00:00Z"; }],
    ["unsafe generation", (value) => { value.generation = Number.MAX_SAFE_INTEGER + 1; }],
  ];
  for (const [label, mutate] of cases) {
    const value = inbox();
    mutate(value);
    assert.throws(() => validateAlertInboxV2(value), AlertInboxV2ContractError, label);
  }
});

test("validator accepts only allowlisted unavailable delivery prerequisites", () => {
  for (const healthClass of ["no_active_subscription", "signing_keys_unavailable", "sender_unavailable"]) {
    const value = inbox({ delivery_health: { state: "unavailable", class: healthClass, updated_at: at } });
    assert.equal(validateAlertInboxV2(value).delivery_health.class, healthClass);
  }
  const invalid = inbox({ delivery_health: { state: "unavailable", class: "transport_rejected", updated_at: at } });
  assert.throws(() => validateAlertInboxV2(invalid), /invalid state.class combination/);
});

test("validator enforces unique display ids and attention sequences", () => {
  const duplicateDisplay = inbox();
  duplicateDisplay.occurrences.push(occurrence({ attention_seq: 2 }));
  duplicateDisplay.attention.high_water_seq = 2;
  duplicateDisplay.attention.unread_count = 2;
  duplicateDisplay.attention.unread_refs.push({ display_id: duplicateDisplay.occurrences[1].display_id, source: "canary", kind: "portfolio_risk" });
  assert.throws(() => validateAlertInboxV2(duplicateDisplay), /unique display ids/);

  const duplicateSequence = inbox();
  const second = occurrence({ display_id: "alert-fedcba9876543210" });
  duplicateSequence.occurrences.push(second);
  duplicateSequence.attention.unread_count = 2;
  duplicateSequence.attention.unread_refs.push({ display_id: second.display_id, source: second.source, kind: second.kind });
  assert.throws(() => validateAlertInboxV2(duplicateSequence), /unique attention sequences/);
});

test("validator enforces coverage subset and exact unread correspondence", () => {
  const outsideCoverage = inbox();
  outsideCoverage.coverage.state = "partial";
  outsideCoverage.coverage.expected_sources = ["canary", "regime"];
  outsideCoverage.coverage.covered_sources = ["delivery"];
  assert.throws(() => validateAlertInboxV2(outsideCoverage), /subset/);

  const wrongRef = inbox();
  wrongRef.attention.unread_refs[0].kind = "market_state";
  assert.throws(() => validateAlertInboxV2(wrongRef), /does not correspond/);

  const missingSequence = inbox();
  missingSequence.attention.high_water_seq = 2;
  assert.throws(() => validateAlertInboxV2(missingSequence), /sequence range is incomplete/);
});

test("validator treats occurrence source authority as retained cross-generation history", () => {
  const expectedSetShrank = inbox({ current_state: "unknown" });
  expectedSetShrank.occurrences[0].source = "regime";
  expectedSetShrank.attention.unread_refs[0].source = "regime";
  assert.equal(validateAlertInboxV2(expectedSetShrank).current_state, "unknown");

  const retainedUncovered = inbox({ current_state: "unknown" });
  retainedUncovered.coverage = {
    state: "partial",
    freshness: "stale",
    as_of: at,
    expected_sources: ["canary", "regime"],
    covered_sources: ["regime"],
  };
  assert.equal(validateAlertInboxV2(retainedUncovered).current_state, "unknown");
  assert.equal(alertInboxV2CanAssertClear(retainedUncovered), false);
});

test("validator enforces recovered-state and timestamp coherence", () => {
  const badRecovery = inbox({ current_state: "clear" });
  badRecovery.occurrences[0] = occurrence({
    state: "recovered",
    evidence_health: "stale",
    ended_at: at,
    end_reason: "recovered",
  });
  badRecovery.attention.unread_refs[0] = {
    display_id: badRecovery.occurrences[0].display_id,
    source: badRecovery.occurrences[0].source,
    kind: badRecovery.occurrences[0].kind,
  };
  assert.throws(() => validateAlertInboxV2(badRecovery), /recovered state requires current evidence/);

  const evidenceAfterObservation = inbox();
  evidenceAfterObservation.occurrences[0].evidence_as_of = "2026-07-20T18:00:01Z";
  assert.throws(() => validateAlertInboxV2(evidenceAfterObservation), /must not be after last_seen_at/);

  const endedBeforeLifecycle = inbox();
  endedBeforeLifecycle.occurrences[0].ended_at = "2026-07-20T17:59:59Z";
  endedBeforeLifecycle.occurrences[0].end_reason = "qualified_escalation";
  assert.throws(() => validateAlertInboxV2(endedBeforeLifecycle), /must not precede occurrence lifecycle/);

  const delayedRecovery = inbox({ current_state: "clear" });
  delayedRecovery.occurrences[0] = occurrence({
    state: "recovered",
    state_changed_at: "2026-07-20T17:59:59Z",
    evidence_as_of: "2026-07-20T18:00:02Z",
    last_seen_at: "2026-07-20T18:00:02Z",
    ended_at: "2026-07-20T17:59:59Z",
    end_reason: "recovered",
  });
  delayedRecovery.as_of = "2026-07-20T18:00:02Z";
  delayedRecovery.coverage.as_of = delayedRecovery.as_of;
  delayedRecovery.attention.unread_refs[0] = {
    display_id: delayedRecovery.occurrences[0].display_id,
    source: delayedRecovery.occurrences[0].source,
    kind: delayedRecovery.occurrences[0].kind,
  };
  assert.equal(validateAlertInboxV2(delayedRecovery).occurrences[0].end_reason, "recovered");
});

test("authority scope retirement remains active history and renders only generic previous context", () => {
  const scoped = inbox({ current_state: "unknown" });
  scoped.occurrences[0] = occurrence({
    ended_at: at,
    end_reason: "authority_scope_changed",
  });
  scoped.attention.unread_refs[0] = {
    display_id: scoped.occurrences[0].display_id,
    source: scoped.occurrences[0].source,
    kind: scoped.occurrences[0].kind,
  };
  assert.equal(validateAlertInboxV2(scoped).occurrences[0].state, "open");
  const view = alertInboxV2OccurrenceView(scoped.occurrences[0]);
  assert.deepEqual(Object.keys(view).sort(), ["evidenceHealth", "kind", "previousContext", "severity", "source", "timestamps"]);
  assert.equal(view.previousContext, true);
  assert.equal(JSON.stringify(view).includes("authority_scope_changed"), false);
  assert.equal(JSON.stringify(view).includes(scoped.occurrences[0].display_id), false);

  const falseRecovery = clone(scoped);
  falseRecovery.occurrences[0].state = "recovered";
  assert.throws(() => validateAlertInboxV2(falseRecovery), /recovered state requires current evidence and a coherent recovery end/);
});

test("validator treats occurrences as retained history and fail-closes clear claims", () => {
  const activeClaimedUnknown = inbox({ current_state: "unknown" });
  assert.equal(validateAlertInboxV2(activeClaimedUnknown).current_state, "unknown");

  const clearWithRetainedActive = inbox({ current_state: "clear" });
  assert.throws(() => validateAlertInboxV2(clearWithRetainedActive), /clear requires complete current coverage/);
});

test("lower generation is ignored and equal identical is a no-op", () => {
  resetState();
  const current = inbox({ generation: 2 });
  assert.equal(ingestAlertInboxV2(current).status, "applied");
  const accepted = state.alertInboxV2;

  assert.equal(ingestAlertInboxV2(inbox({ generation: 1 })).status, "ignored");
  assert.equal(state.alertInboxV2, accepted);
  assert.equal(ingestAlertInboxV2(clone(current)).status, "noop");
  assert.equal(state.alertInboxV2, accepted);
  assert.equal(state.alertInboxV2FeedValid, true);
});

test("equal-generation equivocation is rejected without replacing last-good", () => {
  resetState();
  const current = inbox({ generation: 2 });
  ingestAlertInboxV2(current);
  const accepted = clone(state.alertInboxV2);
  const equivocation = inbox({ generation: 2 });
  equivocation.occurrences[0].severity = "act";

  assert.equal(ingestAlertInboxV2(equivocation).status, "rejected");
  assert.deepEqual(state.alertInboxV2, accepted);
  assert.equal(state.alertInboxV2FeedValid, false);
  assert.match(state.alertInboxV2FeedError, /equivocation/);
});

test("invalid newer state retains last-good until a valid newer generation replaces it", () => {
  resetState();
  ingestAlertInboxV2(inbox({ generation: 2 }));
  const accepted = clone(state.alertInboxV2);
  const invalid = inbox({ generation: 3 });
  invalid.coverage.covered_sources = ["delivery"];

  assert.equal(ingestAlertInboxV2(invalid).status, "rejected");
  assert.deepEqual(state.alertInboxV2, accepted);
  assert.equal(state.alertInboxV2FeedValid, false);

  const newer = inbox({ generation: 3 });
  assert.equal(ingestAlertInboxV2(newer).status, "applied");
  assert.equal(state.alertInboxV2.generation, 3);
  assert.equal(state.alertInboxV2FeedValid, true);
  assert.equal(state.alertInboxV2FeedError, "");
});

test("malformed SSE event marks feed invalid and retains last-good", () => {
  resetState();
  ingestAlertInboxV2(inbox({ generation: 2 }));
  const accepted = clone(state.alertInboxV2);
  const result = ingestAlertInboxV2Event('{"generation":');
  assert.equal(result.status, "rejected");
  assert.deepEqual(result.value, accepted);
  assert.deepEqual(state.alertInboxV2, accepted);
  assert.equal(state.alertInboxV2FeedValid, false);
  assert.equal(state.alertInboxV2FeedError, "malformed alert inbox v2 event");
});

test("shadow ingestion never changes legacy unread or foreground attention state", () => {
  resetState();
  const legacyAttention = { unread_count: 7, high_water_seq: 11, read_through_seq: 4, unread_refs: [{ kind: "canary", id: "legacy" }] };
  state.attention = legacyAttention;
  state.attentionEpoch = 23;
  ingestAlertInboxV2(inbox());
  assert.equal(state.attention, legacyAttention);
  assert.equal(state.attentionEpoch, 23);
});

test("pure shadow advisory view exposes every state without granting all-clear", () => {
  const cold = alertInboxV2View(uninitialized(), true);
  assert.equal(cold.state, "uninitialized");
  assert.match(cold.summary, /No delivery is active/);

  const active = alertInboxV2View(inbox(), true);
  assert.equal(active.state, "active");
  assert.equal(active.occurrences.length, 1);
  assert.equal("attention" in active, false);
  assert.equal("destination" in active.occurrences[0], false);
  assert.equal("deliveryPreference" in active.occurrences[0], false);

  const unknown = alertInboxV2View(inbox({ current_state: "unknown" }), true);
  assert.equal(unknown.state, "unknown");
  assert.match(unknown.summary, /cannot determine/);

  const clear = alertInboxV2View(clearInbox(), true);
  assert.equal(clear.state, "clear");
  assert.match(clear.summary, /cannot assert operator all-clear/);
  assert.equal(clear.tone, "neutral", "shadow clear must not render as a green operator all-clear");

  const invalid = alertInboxV2View(inbox(), false);
  assert.equal(invalid.state, "invalid");
  assert.match(invalid.summary, /invalid/);
  assert.match(invalid.coverage.summary, /Last-valid/);
});

test("unhealthy or unexpected initialized delivery health can never render shadow clear", () => {
  const healthCases = [
    { state: "degraded", class: "retry_pending", updated_at: at },
    { state: "unavailable", class: "no_active_subscription", updated_at: at },
    { state: "overflow", class: "capacity_overflow", updated_at: at },
    { state: "healthy", class: "", updated_at: at },
  ];
  for (const deliveryHealth of healthCases) {
    const value = clearInbox({ delivery_health: deliveryHealth });
    assert.equal(validateAlertInboxV2(value).current_state, "clear");
    const view = alertInboxV2View(value, true);
    assert.equal(view.state, "unknown", JSON.stringify(deliveryHealth));
    assert.equal(view.tone, "warn");
    assert.match(view.summary, /cannot assert current advisory state or operator all-clear/);
  }
});

test("shadow evidence rendering is bounded to the deterministic latest 24 occurrences", () => {
  const occurrences = Array.from({ length: 30 }, (_, index) => occurrence({
    last_seen_at: `2026-07-20T18:00:${String(index).padStart(2, "0")}Z`,
    evidence_as_of: `2026-07-20T18:00:${String(index).padStart(2, "0")}Z`,
    state_changed_at: `2026-07-20T18:00:${String(index).padStart(2, "0")}Z`,
  }));
  const rendered = alertInboxV2RenderedOccurrences(occurrences);
  assert.equal(rendered.length, 24);
  assert.equal(rendered[0].timestamps.find((item) => item.label === "Last seen").value, "2026-07-20T18:00:29Z");
  assert.equal(rendered.at(-1).timestamps.find((item) => item.label === "Last seen").value, "2026-07-20T18:00:06Z");
});

test("clear measurement requires complete current coverage but shadow never asserts all-clear", () => {
  const completeCurrent = clearInbox();
  assert.equal(validateAlertInboxV2(completeCurrent).current_state, "clear");
  assert.equal(alertInboxV2CanAssertClear(completeCurrent), false);
  assert.equal(alertInboxV2CanAssertClear(uninitialized()), false);

  const partial = clearInbox();
  partial.coverage = {
    state: "partial",
    freshness: "current",
    as_of: at,
    expected_sources: ["canary", "regime"],
    covered_sources: ["canary"],
  };
  assert.throws(() => validateAlertInboxV2(partial), /clear requires complete current coverage/);

  const stale = clearInbox();
  stale.coverage.freshness = "stale";
  assert.throws(() => validateAlertInboxV2(stale), /clear requires complete current coverage/);
});

test("uninitialized feed cannot smuggle a clear state or delivery timestamp", () => {
  const claimedClear = uninitialized({ current_state: "clear" });
  assert.throws(() => validateAlertInboxV2(claimedClear), /data while uninitialized/);
  const stamped = uninitialized();
  stamped.delivery_health.updated_at = at;
  assert.throws(() => validateAlertInboxV2(stamped), /typed uninitialized posture/);

  const coldFailure = uninitialized({
    generation: 1,
    delivery_health: { state: "unavailable", class: "state_write_failure", updated_at: at },
  });
  assert.equal(validateAlertInboxV2(coldFailure).generation, 1);
  assert.equal(alertInboxV2CanAssertClear(coldFailure), false);
});

test("invalid persisted state is accepted only as uninitialized and can never assert clear", () => {
  const value = quarantined();
  assert.equal(validateAlertInboxV2(value).delivery_health.class, "invalid_persisted_state");
  assert.equal(alertInboxV2CanAssertClear(value), false);

  const initialized = inbox({
    delivery_health: { state: "unavailable", class: "invalid_persisted_state", updated_at: at },
  });
  assert.throws(() => validateAlertInboxV2(initialized), /must remain uninitialized/);

  const claimedClear = quarantined({ current_state: "clear" });
  assert.throws(() => validateAlertInboxV2(claimedClear), /data while uninitialized/);
});

test("quarantine generation supersedes last-good state and is stable on replay", () => {
  resetState();
  assert.equal(ingestAlertInboxV2(inbox({ generation: 41 })).status, "applied");
  const quarantine = quarantined();
  assert.equal(ingestAlertInboxV2(quarantine).status, "applied");
  assert.equal(state.alertInboxV2.initialized, false);
  assert.equal(state.alertInboxV2.delivery_health.class, "invalid_persisted_state");
  assert.equal(ingestAlertInboxV2(clone(quarantine)).status, "noop");
  assert.equal(state.alertInboxV2FeedValid, true);
  assert.equal(alertInboxV2CanAssertClear(state.alertInboxV2), false);
});
