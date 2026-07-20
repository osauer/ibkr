import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

globalThis.localStorage = {
  getItem() { return null; },
  setItem() {},
};

const { state } = await import("../state.js");
const {
  AlertInboxV2ContractError,
  alertInboxV2CanAssertClear,
  ingestAlertInboxV2,
  ingestAlertInboxV2Event,
  validateAlertInboxV2,
} = await import("../alert-inbox-v2.js");
const lifecycleSource = await readFile(new URL("../lifecycle.js", import.meta.url), "utf8");
const stateSource = await readFile(new URL("../state.js", import.meta.url), "utf8");

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
  assert.doesNotMatch(lifecycleSource, /fetch\("\/api\/alert-inbox-v2/);
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
