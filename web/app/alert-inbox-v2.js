import { state } from "./state.js";

const $ = (id) => globalThis.document?.getElementById(id) || null;

const SCHEMA_VERSION = "alert-inbox-v2";
const SHADOW_AUTHORITY = "shadow";

const SOURCES = new Set([
  "canary", "regime", "rulebook", "risk_policy", "protection", "order_integrity",
  "reconciliation", "governance", "data_health", "delivery",
]);
const KINDS = new Set([
  "market_state", "portfolio_risk", "margin_safety", "drawdown", "protection_gap",
  "order_integrity", "reconciliation_exception", "governance", "policy_drift",
  "data_health", "delivery_health",
]);
const EPISODE_STATES = new Set(["open", "escalated", "recovered"]);
const SEVERITIES = new Set(["observe", "watch", "act", "urgent"]);
const DELIVERY_PREFERENCES = new Set(["unapproved", "record_only", "inbox", "digest", "page"]);
const EVIDENCE_HEALTH = new Set(["current", "partial", "stale", "unavailable", "error"]);
const DESTINATIONS = new Set(["monitor", "alerts", "brief"]);
const COVERAGE_STATES = new Set(["complete", "partial", "unavailable"]);
const COVERAGE_FRESHNESS = new Set(["current", "stale", "unknown"]);
const CURRENT_STATES = new Set(["clear", "active", "unknown"]);
const END_REASONS = new Set(["recovered", "authoritative_omission", "qualified_escalation", "authority_scope_changed"]);
const DELIVERY_HEALTH_STATES = new Set(["shadow", "healthy", "degraded", "unavailable", "overflow"]);
const DELIVERY_HEALTH_CLASSES = new Set([
  "", "policy_unapproved", "retry_pending", "transport_rejected", "interrupted_uncertain",
  "state_write_failure", "capacity_overflow", "retry_exhausted", "no_active_subscription",
  "signing_keys_unavailable", "sender_unavailable", "invalid_persisted_state",
]);
const MAX_RENDERED_OCCURRENCES = 24;

const TOP_LEVEL_KEYS = [
  "schema_version", "authority", "initialized", "generation", "as_of", "current_state",
  "coverage", "occurrences", "attention", "delivery_health",
];
const COVERAGE_KEYS = ["state", "freshness", "as_of", "expected_sources", "covered_sources"];
const OCCURRENCE_KEYS = [
  "display_id", "source", "kind", "state", "severity", "delivery_preference",
  "evidence_health", "destination", "evidence_as_of", "state_changed_at", "first_seen_at",
  "last_seen_at", "ended_at", "end_reason", "attention_seq",
];
const ATTENTION_KEYS = ["unread_count", "high_water_seq", "read_through_seq", "unread_refs"];
const ATTENTION_REF_KEYS = ["display_id", "source", "kind"];
const DELIVERY_HEALTH_KEYS = ["state", "class", "updated_at"];
const DISPLAY_ID_PATTERN = /^alert-[0-9a-f]{16}$/;
const RFC3339_UTC_PATTERN = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z$/;

class AlertInboxV2ContractError extends Error {
  constructor(message) {
    super(message);
    this.name = "AlertInboxV2ContractError";
  }
}

function fail(path, reason) {
  throw new AlertInboxV2ContractError(`${path} ${reason}`);
}

function exactObject(value, keys, path) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail(path, "must be an object");
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(path, "has unexpected or missing keys");
  }
  return value;
}

function exactArray(value, path) {
  if (!Array.isArray(value)) fail(path, "must be an array");
  return value;
}

function safeInteger(value, path, { positive = false } = {}) {
  if (!Number.isSafeInteger(value) || value < (positive ? 1 : 0)) fail(path, "must be a safe unsigned integer");
  return value;
}

function enumValue(value, allowed, path) {
  if (typeof value !== "string" || !allowed.has(value)) fail(path, "has an invalid enum value");
  return value;
}

function strictTimestamp(value, path) {
  if (typeof value !== "string") fail(path, "must be an RFC3339 UTC timestamp");
  const match = RFC3339_UTC_PATTERN.exec(value);
  if (!match) fail(path, "must be an RFC3339 UTC timestamp");
  const [, yearRaw, monthRaw, dayRaw, hourRaw, minuteRaw, secondRaw] = match;
  const year = Number(yearRaw);
  const month = Number(monthRaw);
  const day = Number(dayRaw);
  const hour = Number(hourRaw);
  const minute = Number(minuteRaw);
  const second = Number(secondRaw);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (month < 1 || month > 12 || day < 1 || day > days[month - 1] || hour > 23 || minute > 59 || second > 59) {
    fail(path, "must be a real RFC3339 UTC timestamp");
  }
  if (!Number.isFinite(Date.parse(value))) fail(path, "must be a representable RFC3339 UTC timestamp");
  return value;
}

function timestampOrderKey(value) {
  const match = RFC3339_UTC_PATTERN.exec(value);
  if (!match) return "";
  return `${match[1]}${match[2]}${match[3]}${match[4]}${match[5]}${match[6]}${(match[7] || "").padEnd(9, "0")}`;
}

function timestampAfter(left, right) {
  return timestampOrderKey(left) > timestampOrderKey(right);
}

function uniqueEnums(values, allowed, path, { nonempty = false } = {}) {
  exactArray(values, path);
  if (nonempty && values.length === 0) fail(path, "must not be empty");
  const seen = new Set();
  values.forEach((value, index) => {
    enumValue(value, allowed, `${path}[${index}]`);
    if (seen.has(value)) fail(path, "must contain unique values");
    seen.add(value);
  });
  return seen;
}

function validateCoverage(coverage, snapshotAsOf) {
  exactObject(coverage, COVERAGE_KEYS, "coverage");
  enumValue(coverage.state, COVERAGE_STATES, "coverage.state");
  enumValue(coverage.freshness, COVERAGE_FRESHNESS, "coverage.freshness");
  strictTimestamp(coverage.as_of, "coverage.as_of");
  if (timestampAfter(coverage.as_of, snapshotAsOf)) fail("coverage.as_of", "must not be after as_of");
  if (coverage.freshness === "current" && coverage.as_of !== snapshotAsOf) {
    fail("coverage.as_of", "must equal as_of when current");
  }
  const expected = uniqueEnums(coverage.expected_sources, SOURCES, "coverage.expected_sources", { nonempty: true });
  const covered = uniqueEnums(coverage.covered_sources, SOURCES, "coverage.covered_sources");
  for (const source of covered) {
    if (!expected.has(source)) fail("coverage.covered_sources", "must be a subset of expected_sources");
  }
  if (coverage.state === "complete") {
    if (covered.size !== expected.size || coverage.freshness === "unknown") {
      fail("coverage", "complete coverage must cover every source with known freshness");
    }
  } else if (coverage.state === "partial") {
    if (covered.size === 0 || covered.size === expected.size || coverage.freshness === "unknown") {
      fail("coverage", "partial coverage must be a non-empty proper subset with known freshness");
    }
  } else if (covered.size !== 0 || coverage.freshness !== "unknown") {
    fail("coverage", "unavailable coverage must be empty with unknown freshness");
  }
}

function validateOccurrence(occurrence, index) {
  const path = `occurrences[${index}]`;
  exactObject(occurrence, OCCURRENCE_KEYS, path);
  if (typeof occurrence.display_id !== "string" || !DISPLAY_ID_PATTERN.test(occurrence.display_id)) {
    fail(`${path}.display_id`, "has an invalid public display id");
  }
  enumValue(occurrence.source, SOURCES, `${path}.source`);
  enumValue(occurrence.kind, KINDS, `${path}.kind`);
  enumValue(occurrence.state, EPISODE_STATES, `${path}.state`);
  enumValue(occurrence.severity, SEVERITIES, `${path}.severity`);
  enumValue(occurrence.delivery_preference, DELIVERY_PREFERENCES, `${path}.delivery_preference`);
  enumValue(occurrence.evidence_health, EVIDENCE_HEALTH, `${path}.evidence_health`);
  enumValue(occurrence.destination, DESTINATIONS, `${path}.destination`);
  strictTimestamp(occurrence.evidence_as_of, `${path}.evidence_as_of`);
  strictTimestamp(occurrence.state_changed_at, `${path}.state_changed_at`);
  strictTimestamp(occurrence.first_seen_at, `${path}.first_seen_at`);
  strictTimestamp(occurrence.last_seen_at, `${path}.last_seen_at`);
  if (timestampAfter(occurrence.first_seen_at, occurrence.last_seen_at)) {
    fail(path, "first_seen_at must not be after last_seen_at");
  }
  if (timestampAfter(occurrence.evidence_as_of, occurrence.last_seen_at) || timestampAfter(occurrence.state_changed_at, occurrence.last_seen_at)) {
    fail(path, "evidence and state timestamps must not be after last_seen_at");
  }
  if (occurrence.ended_at === null) {
    if (occurrence.end_reason !== null) fail(`${path}.end_reason`, "must be null without ended_at");
  } else {
    strictTimestamp(occurrence.ended_at, `${path}.ended_at`);
    enumValue(occurrence.end_reason, END_REASONS, `${path}.end_reason`);
    if (timestampAfter(occurrence.state_changed_at, occurrence.ended_at)) {
      fail(`${path}.ended_at`, "must not precede occurrence lifecycle timestamps");
    }
  }
  if (occurrence.state === "recovered") {
    if (occurrence.evidence_health !== "current" || occurrence.ended_at === null || !["recovered", "authoritative_omission"].includes(occurrence.end_reason)) {
      fail(path, "recovered state requires current evidence and a coherent recovery end");
    }
  } else if (occurrence.ended_at !== null && !["qualified_escalation", "authority_scope_changed"].includes(occurrence.end_reason)) {
    fail(path, "active lifecycle state can end only through qualified escalation or authority scope change");
  }
  safeInteger(occurrence.attention_seq, `${path}.attention_seq`, { positive: true });
}

function validateOccurrenceAuthority(occurrence, coverage, snapshotAsOf, index) {
  const path = `occurrences[${index}]`;
  if (timestampAfter(occurrence.last_seen_at, snapshotAsOf) || (occurrence.ended_at !== null && timestampAfter(occurrence.ended_at, snapshotAsOf))) {
    fail(path, "lifecycle timestamps must not be after as_of");
  }
}

function validateDeliveryHealth(health, initialized) {
  exactObject(health, DELIVERY_HEALTH_KEYS, "delivery_health");
  enumValue(health.state, DELIVERY_HEALTH_STATES, "delivery_health.state");
  enumValue(health.class, DELIVERY_HEALTH_CLASSES, "delivery_health.class");
  const validClass = (
    (health.state === "shadow" && health.class === "policy_unapproved") ||
    (health.state === "healthy" && health.class === "") ||
    (health.state === "degraded" && ["retry_pending", "transport_rejected", "interrupted_uncertain", "retry_exhausted"].includes(health.class)) ||
    (health.state === "unavailable" && [
      "state_write_failure", "no_active_subscription", "signing_keys_unavailable", "sender_unavailable", "invalid_persisted_state",
    ].includes(health.class)) ||
    (health.state === "overflow" && health.class === "capacity_overflow")
  );
  if (!validClass) fail("delivery_health", "has an invalid state/class combination");
  if (initialized && health.class === "invalid_persisted_state") {
    fail("delivery_health", "invalid persisted state must remain uninitialized");
  }
  if (!initialized) {
    const coldShadow = health.state === "shadow" && health.class === "policy_unapproved" && health.updated_at === null;
    const coldFailure = health.state === "unavailable" && health.class === "state_write_failure" && health.updated_at !== null;
    const invalidPersisted = health.state === "unavailable" && health.class === "invalid_persisted_state" && health.updated_at !== null;
    if (!coldShadow && !coldFailure && !invalidPersisted) {
      fail("delivery_health", "must use a typed uninitialized posture");
    }
    if (coldFailure || invalidPersisted) strictTimestamp(health.updated_at, "delivery_health.updated_at");
  } else {
    strictTimestamp(health.updated_at, "delivery_health.updated_at");
  }
}

function validateAttention(attention, occurrences) {
  exactObject(attention, ATTENTION_KEYS, "attention");
  safeInteger(attention.unread_count, "attention.unread_count");
  safeInteger(attention.high_water_seq, "attention.high_water_seq");
  safeInteger(attention.read_through_seq, "attention.read_through_seq");
  if (attention.read_through_seq > attention.high_water_seq) fail("attention", "read cursor exceeds high water");
  exactArray(attention.unread_refs, "attention.unread_refs");
  if (attention.unread_count !== attention.unread_refs.length) fail("attention", "unread_count does not match unread_refs");

  const displayIDs = new Set();
  const sequences = new Set();
  const occurrencesByDisplay = new Map();
  const expectedUnread = [];
  occurrences.forEach((occurrence) => {
    if (displayIDs.has(occurrence.display_id)) fail("occurrences", "must have unique display ids");
    if (sequences.has(occurrence.attention_seq)) fail("occurrences", "must have unique attention sequences");
    if (occurrence.attention_seq > attention.high_water_seq) fail("occurrences", "attention sequence exceeds high water");
    displayIDs.add(occurrence.display_id);
    sequences.add(occurrence.attention_seq);
    occurrencesByDisplay.set(occurrence.display_id, occurrence);
    if (occurrence.attention_seq > attention.read_through_seq) expectedUnread.push(occurrence);
  });
  expectedUnread.sort((left, right) => left.attention_seq - right.attention_seq);
  if (attention.high_water_seq - attention.read_through_seq !== expectedUnread.length) {
    fail("attention", "unread occurrence sequence range is incomplete");
  }
  attention.unread_refs.forEach((ref, index) => {
    const path = `attention.unread_refs[${index}]`;
    exactObject(ref, ATTENTION_REF_KEYS, path);
    if (typeof ref.display_id !== "string" || !DISPLAY_ID_PATTERN.test(ref.display_id)) fail(`${path}.display_id`, "is invalid");
    enumValue(ref.source, SOURCES, `${path}.source`);
    enumValue(ref.kind, KINDS, `${path}.kind`);
    const expected = expectedUnread[index];
    const occurrence = occurrencesByDisplay.get(ref.display_id);
    if (!expected || !occurrence || expected.display_id !== ref.display_id || occurrence.source !== ref.source || occurrence.kind !== ref.kind) {
      fail(path, "does not correspond to the ordered unread occurrence");
    }
  });
}

function validateAlertInboxV2(value) {
  exactObject(value, TOP_LEVEL_KEYS, "alert_inbox_v2");
  if (value.schema_version !== SCHEMA_VERSION) fail("schema_version", "is unsupported");
  if (value.authority !== SHADOW_AUTHORITY) fail("authority", "is not the fixed shadow authority");
  if (typeof value.initialized !== "boolean") fail("initialized", "must be boolean");
  safeInteger(value.generation, "generation");
  exactArray(value.occurrences, "occurrences");
  value.occurrences.forEach(validateOccurrence);
  validateAttention(value.attention, value.occurrences);
  validateDeliveryHealth(value.delivery_health, value.initialized);

  if (!value.initialized) {
    const coldFailure = value.delivery_health.state === "unavailable" &&
      ["state_write_failure", "invalid_persisted_state"].includes(value.delivery_health.class);
    if ((coldFailure ? value.generation < 1 : value.generation !== 0) || value.as_of !== null || value.current_state !== null || value.coverage !== null || value.occurrences.length !== 0 ||
        value.attention.unread_count !== 0 || value.attention.high_water_seq !== 0 || value.attention.read_through_seq !== 0 || value.attention.unread_refs.length !== 0) {
      fail("alert_inbox_v2", "has data while uninitialized");
    }
    return value;
  }

  if (value.generation === 0) fail("generation", "must advance after initialization");
  strictTimestamp(value.as_of, "as_of");
  enumValue(value.current_state, CURRENT_STATES, "current_state");
  validateCoverage(value.coverage, value.as_of);
  value.occurrences.forEach((occurrence, index) => validateOccurrenceAuthority(occurrence, value.coverage, value.as_of, index));
  if (value.current_state === "clear") {
    const hasRetainedActive = value.occurrences.some((occurrence) => occurrence.ended_at === null && (occurrence.state === "open" || occurrence.state === "escalated"));
    if (value.coverage.state !== "complete" || value.coverage.freshness !== "current" || hasRetainedActive) {
      fail("current_state", "clear requires complete current coverage and no retained active occurrence");
    }
  }
  return value;
}

function deepEqualJSON(left, right) {
  if (left === right) return true;
  if (left === null || right === null || typeof left !== "object" || typeof right !== "object") return false;
  if (Array.isArray(left) !== Array.isArray(right)) return false;
  if (Array.isArray(left)) {
    return left.length === right.length && left.every((item, index) => deepEqualJSON(item, right[index]));
  }
  const leftKeys = Object.keys(left).sort();
  const rightKeys = Object.keys(right).sort();
  return leftKeys.length === rightKeys.length && leftKeys.every((key, index) => key === rightKeys[index] && deepEqualJSON(left[key], right[key]));
}

function clonedJSON(value) {
  return JSON.parse(JSON.stringify(value));
}

function markAlertInboxV2Invalid(message) {
  state.alertInboxV2FeedValid = false;
  state.alertInboxV2FeedError = message;
}

// ingestAlertInboxV2 is deliberately fail-last-good. Rewinds are harmless,
// equal-generation equivocation is rejected, and a malformed advance cannot
// replace the last fully validated generation.
function ingestAlertInboxV2(value) {
  const current = state.alertInboxV2;
  const generation = value?.generation;
  if (current && Number.isSafeInteger(generation) && generation >= 0) {
    if (generation < current.generation) return { status: "ignored", value: current };
    if (generation === current.generation) {
      if (deepEqualJSON(value, current)) return { status: "noop", value: current };
      markAlertInboxV2Invalid("equal-generation alert inbox v2 equivocation");
      return { status: "rejected", value: current };
    }
  }

  try {
    validateAlertInboxV2(value);
  } catch (error) {
    markAlertInboxV2Invalid(error instanceof Error ? error.message : "invalid alert inbox v2 feed");
    return { status: "rejected", value: current };
  }
  const accepted = clonedJSON(value);
  state.alertInboxV2 = accepted;
  state.alertInboxV2FeedValid = true;
  state.alertInboxV2FeedError = "";
  return { status: "applied", value: accepted };
}

function ingestAlertInboxV2Event(raw) {
  try {
    return ingestAlertInboxV2(JSON.parse(raw));
  } catch {
    markAlertInboxV2Invalid("malformed alert inbox v2 event");
    return { status: "rejected", value: state.alertInboxV2 };
  }
}

// Shadow observations may report that their measured snapshot is clear, but
// they never authorize a user-facing all-clear assertion during this rollout.
function alertInboxV2CanAssertClear(value = state.alertInboxV2) {
  if (!value) return false;
  try {
    validateAlertInboxV2(value);
  } catch {
    return false;
  }
  return value.authority !== SHADOW_AUTHORITY && value.initialized && value.current_state === "clear" &&
    value.coverage?.state === "complete" && value.coverage?.freshness === "current";
}

function alertInboxV2OccurrenceView(occurrence = {}) {
  const timestamps = [
    { label: "Evidence", value: occurrence.evidence_as_of },
    { label: "State changed", value: occurrence.state_changed_at },
    { label: "First seen", value: occurrence.first_seen_at },
    { label: "Last seen", value: occurrence.last_seen_at },
  ];
  if (occurrence.ended_at) timestamps.push({ label: "Ended", value: occurrence.ended_at });
  return {
    source: occurrence.source,
    kind: occurrence.kind,
    severity: occurrence.severity,
    evidenceHealth: occurrence.evidence_health,
    previousContext: occurrence.end_reason === "authority_scope_changed",
    timestamps,
  };
}

function alertInboxV2RenderedOccurrences(occurrences = [], limit = MAX_RENDERED_OCCURRENCES) {
  const bounded = Number.isSafeInteger(limit) && limit >= 0 ? limit : MAX_RENDERED_OCCURRENCES;
  return occurrences
    .map((occurrence, index) => ({ occurrence, index }))
    .sort((left, right) => {
      const byLastSeen = timestampOrderKey(right.occurrence.last_seen_at).localeCompare(timestampOrderKey(left.occurrence.last_seen_at));
      return byLastSeen || right.index - left.index;
    })
    .slice(0, bounded)
    .map(({ occurrence }) => alertInboxV2OccurrenceView(occurrence));
}

function alertInboxV2EvidenceView(value) {
  const total = value?.initialized && Array.isArray(value.occurrences) ? value.occurrences.length : 0;
  const occurrences = total > 0 ? alertInboxV2RenderedOccurrences(value.occurrences) : [];
  return {
    occurrenceCount: total,
    shownOccurrenceCount: occurrences.length,
    occurrences,
  };
}

function alertInboxV2CoverageView(coverage, retained = false) {
  if (!coverage) return null;
  return {
    state: coverage.state,
    freshness: coverage.freshness,
    asOf: coverage.as_of,
    expectedSources: [...coverage.expected_sources],
    coveredSources: [...coverage.covered_sources],
    summary: `${retained ? "Last-valid " : ""}${alertInboxV2Label(coverage.state)} coverage · ${alertInboxV2Label(coverage.freshness)} · ${coverage.covered_sources.length}/${coverage.expected_sources.length} sources`,
  };
}

function alertInboxV2View(value = state.alertInboxV2, feedValid = state.alertInboxV2FeedValid) {
  let accepted = null;
  if (value) {
    try {
      accepted = validateAlertInboxV2(value);
    } catch {
      // A caller can ask for a view before lifecycle ingestion. The visible
      // result still fails closed without echoing validation or payload text.
    }
  }

  const persistedInvalid = accepted?.delivery_health?.class === "invalid_persisted_state";
  if (feedValid === false || (value && !accepted) || persistedInvalid) {
    const retained = accepted?.initialized === true;
    const evidence = retained ? alertInboxV2EvidenceView(accepted) : alertInboxV2EvidenceView(null);
    return {
      state: "invalid",
      label: "Invalid",
      tone: "warn",
      summary: "The latest shadow measurement is invalid. No delivery is active; retained evidence is last-valid context only.",
      asOf: retained ? accepted.as_of : null,
      coverage: retained ? alertInboxV2CoverageView(accepted.coverage, true) : null,
      ...evidence,
    };
  }

  if (!accepted || !accepted.initialized) {
    const unavailable = accepted?.delivery_health?.class === "state_write_failure";
    return {
      state: "uninitialized",
      label: "Uninitialized",
      tone: unavailable ? "warn" : "neutral",
      summary: unavailable
        ? "Shadow measurement could not initialize its record-only state. No delivery is active."
        : "Shadow measurement has not initialized yet. No delivery is active.",
      asOf: null,
      coverage: null,
      occurrenceCount: 0,
      shownOccurrenceCount: 0,
      occurrences: [],
    };
  }

  const evidence = alertInboxV2EvidenceView(accepted);
  const expectedShadowHealth = accepted.delivery_health.state === "shadow" && accepted.delivery_health.class === "policy_unapproved";
  if (!expectedShadowHealth) {
    return {
      state: "unknown",
      label: "Unknown",
      tone: "warn",
      summary: "Shadow delivery health is outside the expected record-only state. This view cannot assert current advisory state or operator all-clear; no delivery is active.",
      asOf: accepted.as_of,
      coverage: alertInboxV2CoverageView(accepted.coverage, true),
      ...evidence,
    };
  }

  const currentState = accepted.current_state;
  const summaries = {
    active: "Shadow measurement observed active advisory evidence. No delivery is active; established alert delivery is unchanged.",
    unknown: "Shadow measurement cannot determine the current advisory state from available coverage. No delivery is active.",
    clear: "Shadow measurement found no active advisory evidence, but this shadow measurement cannot assert operator all-clear. No delivery is active.",
  };
  const labels = { active: "Active measurement", unknown: "Unknown", clear: "Clear measurement" };
  return {
    state: currentState,
    label: labels[currentState],
    tone: currentState === "clear" ? "neutral" : "warn",
    summary: summaries[currentState],
    asOf: accepted.as_of,
    coverage: alertInboxV2CoverageView(accepted.coverage),
    ...evidence,
  };
}

function alertInboxV2TimestampLabel(value) {
  if (!value) return "--";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "--";
  return parsed.toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  });
}

function alertInboxV2Label(value) {
  return String(value || "")
    .split("_")
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(" ") || "--";
}

function alertInboxV2OccurrenceElement(occurrence) {
  const row = document.createElement("article");
  row.className = `shadow-advisory-row shadow-advisory-row--${occurrence.severity}`;

  const head = document.createElement("div");
  head.className = "shadow-advisory-row__head";
  const kind = document.createElement("b");
  kind.textContent = alertInboxV2Label(occurrence.kind);
  const source = document.createElement("span");
  source.textContent = alertInboxV2Label(occurrence.source);
  head.append(kind, source);

  const evidence = document.createElement("div");
  evidence.className = "shadow-advisory-row__evidence";
  const severity = document.createElement("span");
  severity.textContent = `Severity · ${alertInboxV2Label(occurrence.severity)}`;
  const health = document.createElement("span");
  health.textContent = `Evidence · ${alertInboxV2Label(occurrence.evidenceHealth)}`;
  evidence.append(severity, health);
  row.append(head, evidence);

  if (occurrence.previousContext) {
    const context = document.createElement("p");
    context.className = "shadow-advisory-row__context";
    context.textContent = "Previous account/mode context";
    row.appendChild(context);
  }

  const timestamps = document.createElement("div");
  timestamps.className = "shadow-advisory-row__times";
  for (const timestamp of occurrence.timestamps) {
    const item = document.createElement("time");
    item.dateTime = timestamp.value;
    item.title = timestamp.value;
    item.textContent = `${timestamp.label} · ${alertInboxV2TimestampLabel(timestamp.value)}`;
    timestamps.appendChild(item);
  }
  row.appendChild(timestamps);
  return row;
}

function renderAlertInboxV2() {
  const view = alertInboxV2View();
  const section = $("alertInboxV2Section");
  if (!section) return view;
  section.dataset.state = view.state;
  section.classList.remove("shadow-advisory--warn", "shadow-advisory--neutral");
  section.classList.add(`shadow-advisory--${view.tone}`);
  $("alertInboxV2State").textContent = view.label;
  $("alertInboxV2Summary").textContent = view.summary;

  const coverage = $("alertInboxV2Coverage");
  const coverageSources = $("alertInboxV2CoverageSources");
  const asOf = $("alertInboxV2AsOf");
  const occurrenceCount = $("alertInboxV2OccurrenceCount");
  coverage.hidden = !view.coverage;
  coverage.textContent = view.coverage?.summary || "";
  coverageSources.hidden = !view.coverage;
  coverageSources.textContent = view.coverage
    ? (view.coverage.coveredSources.length > 0
      ? `Observed sources · ${view.coverage.coveredSources.map(alertInboxV2Label).join(" · ")}`
      : "No sources currently covered")
    : "";
  asOf.hidden = !view.asOf;
  asOf.dateTime = view.asOf || "";
  asOf.title = view.asOf || "";
  asOf.textContent = view.asOf ? `Measurement · ${alertInboxV2TimestampLabel(view.asOf)}` : "";
  occurrenceCount.hidden = view.occurrenceCount === 0;
  occurrenceCount.textContent = view.occurrenceCount > 0
    ? `Showing ${view.shownOccurrenceCount} of ${view.occurrenceCount} ${view.shownOccurrenceCount < view.occurrenceCount ? "latest " : ""}public occurrences`
    : "";

  const list = $("alertInboxV2List");
  list.replaceChildren(...view.occurrences.map(alertInboxV2OccurrenceElement));
  const empty = $("alertInboxV2Empty");
  empty.hidden = view.occurrences.length > 0;
  empty.textContent = view.state === "clear"
    ? "No active shadow occurrence evidence in this measurement."
    : view.state === "uninitialized"
      ? "No shadow evidence has been measured yet."
      : "No public shadow occurrence evidence is available.";
  return view;
}

export {
  AlertInboxV2ContractError,
  alertInboxV2CanAssertClear,
  alertInboxV2CoverageView,
  alertInboxV2Label,
  alertInboxV2OccurrenceElement,
  alertInboxV2OccurrenceView,
  alertInboxV2RenderedOccurrences,
  alertInboxV2TimestampLabel,
  alertInboxV2View,
  ingestAlertInboxV2,
  ingestAlertInboxV2Event,
  renderAlertInboxV2,
  validateAlertInboxV2,
};
