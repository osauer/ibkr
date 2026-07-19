import { b64urlToBytes } from "./auth.js";
import { heldStressItems, heldStressSummary } from "./canary.js";
import { $, labelize, shortTime } from "./shared.js";
import { state } from "./state.js";

const alertModes = new Set(["none", "act_only", "watch_and_act"]);
const attentionKinds = new Set(["canary", "governance"]);
const attentionRetryDelayMs = 1500;
let attentionVisibilityBound = false;

function exactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  return actual.length === wanted.length && actual.every((key, index) => key === wanted[index]);
}

function validateAttention(value) {
  if (!exactKeys(value, ["unread_count", "high_water_seq", "read_through_seq", "unread_refs"])) return null;
  for (const key of ["unread_count", "high_water_seq", "read_through_seq"]) {
    if (!Number.isSafeInteger(value[key]) || value[key] < 0) return null;
  }
  if (value.read_through_seq > value.high_water_seq || !Array.isArray(value.unread_refs)) return null;
  const seen = new Set();
  const unreadRefs = [];
  for (const ref of value.unread_refs) {
    if (!exactKeys(ref, ["kind", "id"]) || !attentionKinds.has(ref.kind) || typeof ref.id !== "string" || !ref.id) return null;
    const key = `${ref.kind}\u0000${ref.id}`;
    if (seen.has(key)) return null;
    seen.add(key);
    unreadRefs.push({ kind: ref.kind, id: ref.id });
  }
  if (value.unread_count !== unreadRefs.length) return null;
  return {
    unread_count: value.unread_count,
    high_water_seq: value.high_water_seq,
    read_through_seq: value.read_through_seq,
    unread_refs: unreadRefs,
  };
}

function setAttentionStatus(copy, error = false) {
  state.attentionStatus.state = copy;
  state.attentionStatus.error = error;
  renderAttention();
}

function applyAttention(value, options = {}) {
  const attention = validateAttention(value);
  if (!attention) {
    setAttentionStatus("Unread status unavailable.", true);
    return false;
  }
  state.attention = attention;
  if (options.preserveStatus !== true) {
    state.attentionStatus.state = "";
    state.attentionStatus.error = false;
  }
  renderAttention();
  return true;
}

// Mirror unread onto the installed app icon (Badging API; iOS 16.4+
// home-screen web apps). Follows the in-app badge presentation exactly: a
// known positive count sets the number, anything else clears. Best-effort —
// the API is absent in plain browser tabs and older systems.
function syncAppIconBadge(unread) {
  if (typeof navigator === "undefined" || typeof navigator.setAppBadge !== "function") return;
  const apply = Number.isSafeInteger(unread) && unread > 0
    ? navigator.setAppBadge(unread)
    : typeof navigator.clearAppBadge === "function"
      ? navigator.clearAppBadge()
      : navigator.setAppBadge(0);
  Promise.resolve(apply).catch(() => {});
}

function renderAttention() {
  const attention = state.attention;
  const unread = attention?.unread_count;
  const badge = $("alertUnreadBadge");
  const tab = $("tabAlerts");
  const status = $("attentionStatus");
  const known = Number.isSafeInteger(unread) && unread >= 0;
  badge.hidden = !known || unread === 0;
  badge.textContent = known && unread > 0 ? (unread > 99 ? "99+" : String(unread)) : "";
  badge.setAttribute("aria-hidden", "true");
  tab.setAttribute("aria-label", known && unread > 0 ? `Alerts, ${unread} unread` : "Alerts, no unread alerts");
  syncAppIconBadge(known ? unread : 0);
  status.textContent = state.attentionStatus.state;
  status.classList.toggle("governance-action-status--error", state.attentionStatus.error);
}

function attentionViewReady() {
  const panel = $("alertsTab");
  return state.authenticated === true && state.activeTab === "alerts" && panel && !panel.hidden && document.visibilityState === "visible";
}

function scheduleAttentionRetry() {
  if (!attentionViewReady() || state.attentionRetryTimer) return false;
  state.attentionRetryTimer = setTimeout(() => {
    state.attentionRetryTimer = null;
    acknowledgeAttention({ retry: false });
  }, attentionRetryDelayMs);
  return true;
}

async function refreshAttention(options = {}) {
  if (!state.authenticated) return false;
  const epoch = (state.attentionEpoch || 0) + 1;
  state.attentionEpoch = epoch;
  try {
    const res = await fetch("/api/attention", { credentials: "include" });
    if (!res.ok) throw new Error("attention unavailable");
    if (state.attentionEpoch !== epoch) return false;
    const attention = await res.json();
    if (state.attentionEpoch !== epoch) return false;
    return applyAttention(attention, options);
  } catch {
    if (state.attentionEpoch !== epoch) return false;
    setAttentionStatus(options.failureCopy || "Unread status unavailable.", true);
    return false;
  }
}

function unreadRefsAppear(attention, alerts, governance) {
  const alertIDs = new Set(alerts.map((alert) => typeof alert?.id === "string" ? alert.id : "").filter(Boolean));
  const governanceIDs = new Set(governance.map((occurrence) => typeof occurrence?.display_id === "string" ? occurrence.display_id : "").filter(Boolean));
  return attention.unread_refs.every((ref) => ref.kind === "canary" ? alertIDs.has(ref.id) : governanceIDs.has(ref.id));
}

function validateGovernanceResponse(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  if (!Array.isArray(value.candidates) || !Array.isArray(value.occurrences) || !Array.isArray(value.attempts)) return null;
  for (const field of ["source_health", "poll_source", "attempt_aggregate", "health_aggregate", "delivery_health", "diagnostic"]) {
    if (!value[field] || typeof value[field] !== "object" || Array.isArray(value[field])) return null;
  }
  return value;
}

async function fetchAttentionHistories(epoch, attention) {
  const [alertsResponse, governanceResponse] = await Promise.all([
    fetch("/api/alerts", { credentials: "include" }),
    fetch("/api/governance", { credentials: "include" }),
  ]);
  if (!alertsResponse.ok || !governanceResponse.ok) throw new Error("history unavailable");
  const [alerts, governance] = await Promise.all([alertsResponse.json(), governanceResponse.json()]);
  if (state.attentionEpoch !== epoch) return null;
  const validatedGovernance = validateGovernanceResponse(governance);
  if (!Array.isArray(alerts) || !validatedGovernance) throw new Error("history malformed");
  if (state.attentionEpoch !== epoch) return null;
  if (attention.unread_refs.some((ref) => ref.kind === "canary")) state.alertFilter = "all";
  if (state.attentionEpoch !== epoch) return null;
  state.alerts = alerts;
  if (state.attentionEpoch !== epoch) return null;
  state.governance = validatedGovernance;
  if (state.attentionEpoch !== epoch) return null;
  state.governanceRefreshSucceeded = true;
  if (state.attentionEpoch !== epoch) return null;
  if (state.selectedAlertID && !allAlertItems().some((alert) => alert.id === state.selectedAlertID)) state.selectedAlertID = null;
  if (state.attentionEpoch !== epoch) return null;
  renderAlerts();
  if (state.attentionEpoch !== epoch) return null;
  renderSelectedAlert();
  if (state.attentionEpoch !== epoch) return null;
  renderGovernance();
  return { alerts, governance: validatedGovernance.occurrences };
}

async function acknowledgeAttention(options = {}) {
  if (!attentionViewReady()) return false;
  if (state.attentionReadInFlight) return state.attentionReadInFlight;
  state.attentionReadInFlight = (async () => {
    const epoch = (state.attentionEpoch || 0) + 1;
    state.attentionEpoch = epoch;
    try {
      const attentionResponse = await fetch("/api/attention", { credentials: "include" });
      if (!attentionResponse.ok) throw new Error("attention unavailable");
      if (state.attentionEpoch !== epoch) return false;
      const attentionBody = await attentionResponse.json();
      if (state.attentionEpoch !== epoch) return false;
      const attention = validateAttention(attentionBody);
      if (!attention) throw new Error("attention malformed");
      if (state.attentionEpoch !== epoch) return false;
      applyAttention(attention);
      const histories = await fetchAttentionHistories(epoch, attention);
      if (state.attentionEpoch !== epoch || !histories) return false;
      if (!attentionViewReady()) {
        setAttentionStatus("Alerts stayed unread because the view is not visible.", true);
        return false;
      }
      if (!unreadRefsAppear(attention, histories.alerts, histories.governance)) {
        setAttentionStatus("Unread alerts could not be matched to retained history.", true);
        if (options.retry !== false) scheduleAttentionRetry();
        return false;
      }
      try {
        const readResponse = await fetch("/api/attention/read", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ through_seq: attention.high_water_seq }),
        });
        if (state.attentionEpoch !== epoch) return false;
        if (!readResponse.ok) throw new Error("attention read unavailable");
        const readBody = await readResponse.json();
        if (state.attentionEpoch !== epoch) return false;
        const readAttention = validateAttention(readBody);
        if (!readAttention) throw new Error("attention read malformed");
        if (state.attentionEpoch !== epoch) return false;
        if (!applyAttention(readAttention)) throw new Error("attention read malformed");
      } catch {
        if (state.attentionEpoch !== epoch) return false;
        const reconciled = await refreshAttention({ preserveStatus: true, failureCopy: "Alerts were not marked read; unread status could not be reconciled." });
        setAttentionStatus(reconciled ? "Alerts were not marked read; unread status was reconciled." : "Alerts were not marked read; unread status could not be reconciled.", true);
        return false;
      }
      return true;
    } catch {
      if (state.attentionEpoch !== epoch) return false;
      setAttentionStatus("Unread status unavailable.", true);
      if (options.retry !== false) scheduleAttentionRetry();
      return false;
    } finally {
      state.attentionReadInFlight = null;
    }
  })();
  return state.attentionReadInFlight;
}

function handleAttentionContextChange() {
  if (attentionViewReady()) return acknowledgeAttention();
  return refreshAttention();
}

function setupAttentionVisibility() {
  if (attentionVisibilityBound) return;
  attentionVisibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") return;
    refreshPushState();
    handleAttentionContextChange();
  });
}

function validateAlertSettings(value) {
  return exactKeys(value, ["mode"]) && alertModes.has(value.mode) ? { mode: value.mode } : null;
}

async function setAlertMode(mode) {
  if (!alertModes.has(mode) || state.alertSettingsUpdate.busy) return false;
  const previous = validateAlertSettings(state.alertSettings) || { mode: "watch_and_act" };
  state.alertSettingsUpdate.busy = true;
  state.alertSettingsUpdate.state = "Saving notification level…";
  state.alertSettingsUpdate.error = false;
  renderAlertMode();
  try {
    const res = await fetch("/api/alerts/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ mode }),
    });
    if (!res.ok) throw new Error("update unavailable");
    const updated = validateAlertSettings(await res.json());
    if (!updated || updated.mode !== mode) throw new Error("update malformed");
    state.alertSettings = updated;
    state.alertSettingsUpdate.state = "Delivery level saved for this app host.";
    return true;
  } catch {
    state.alertSettings = previous;
    state.alertSettingsUpdate.state = "Delivery level was not changed.";
    state.alertSettingsUpdate.error = true;
    return false;
  } finally {
    state.alertSettingsUpdate.busy = false;
    renderAlertMode();
  }
}

function renderAlertMode() {
  document.querySelectorAll("#alertSegments button").forEach((button) => {
    button.classList.toggle("active", button.dataset.mode === state.alertSettings.mode);
    button.disabled = state.alertSettingsUpdate.busy;
  });
  $("pushState").textContent = notificationStateLabel();
  $("alertSettingsStatus").textContent = state.alertSettingsUpdate.state;
  $("alertSettingsStatus").classList.toggle("governance-action-status--error", state.alertSettingsUpdate.error);
}

function renderAlerts() {
  const currentItems = filterAlertItems(liveAlertPreviewsSuppressed() ? [] : currentAlertPreviewItems());
  const historyItems = filterAlertItems(currentHistoryAlertItems());
  const previousItems = filterAlertItems(previousContextAlertItems());
  const activeItems = [...currentItems, ...historyItems];
  const clearableLivePreview = currentAlertPreviewItems().length > 0 && !liveAlertPreviewsSuppressed();
  const staleCount = previousContextAlertItems().length;
  const activeHistoryCount = currentHistoryAlertItems().length;
  const activePreviewCount = liveAlertPreviewsSuppressed() ? 0 : currentAlertPreviewItems().length;
  const count = $("alertCount");
  const activeTones = [...currentItems, ...historyItems].map(alertTone);
  count.textContent = activePreviewCount > 0 || activeHistoryCount > 0
    ? `${activePreviewCount} current / ${activeHistoryCount} stored`
    : "0 active";
  count.classList.toggle("is-zero", activeHistoryCount === 0 && activePreviewCount === 0);
  count.classList.toggle("has-risk", activeTones.includes("risk"));
  count.classList.toggle("has-warn", !activeTones.includes("risk") && activeTones.includes("warn"));
  $("currentSignalCount").textContent = String(activePreviewCount);
  $("alertHistoryCount").textContent = String(activeHistoryCount);
  $("previousContextCount").textContent = String(staleCount);
  $("alertsHint").textContent = state.alerts.length === 0
    ? liveAlertPreviewsSuppressed() ? "Current canary signals dismissed for this snapshot." : currentCanaryHasPortfolioAlert()
      ? "Current canary signals from the live snapshot; no alert history recorded yet."
      : "No portfolio alerts for the current low-exposure snapshot."
    : staleCount > 0 ? `${staleCount} previous-context alert${staleCount === 1 ? "" : "s"} hidden. Clear history to reset.`
      : "Tap an alert to inspect it in Canary.";
  $("clearAlertsButton").textContent = state.alerts.length === 0 && clearableLivePreview ? "Dismiss current" : "Clear alerts";
  $("clearAlertsButton").disabled = state.alerts.length === 0 && !clearableLivePreview;
  document.querySelectorAll("[data-alert-filter]").forEach((button) => {
    button.classList.toggle("active", button.dataset.alertFilter === state.alertFilter);
  });
  renderAlertList("currentSignalList", currentItems, "No current canary signal.");
  renderAlertList("alertHistoryList", historyItems, "No stored alert history for the current context.");
  renderAlertList("previousContextList", previousItems, "No previous-context alerts.");
  $("previousContextAlerts").hidden = staleCount === 0;
}

const governanceInputNames = ["policy", "reconciliation", "capital", "pins", "cadence", "confirmed_flow"];
const governanceSnapshotRefreshMinInterval = 15000;
const governanceRecentAttemptLimit = 6;
const governanceTransportClasses = new Set([
  "push_service_accepted", "partial_acceptance", "all_failed", "no_subscription", "missing_keys",
  "sender_unavailable", "attempt_reserved", "interrupted_uncertain", "target_retired", "deadline_retry",
  "canceled_retry", "transport_retry", "http_retry", "http_rejected", "timeout_retry", "rejected",
  "dead_subscription", "state_write_failure", "recovery", "suppressed", "overflow",
]);

function renderGovernance() {
  const snapshot = state.snapshot || {};
  const nudges = snapshot.nudges || null;
  const pollSource = snapshot.sources?.nudges || {};
  const governance = state.governance;
  const pollState = safeGovernancePollState(pollSource.state);
  const current = pollState === "current";
  const candidates = current && Array.isArray(nudges?.candidates) ? nudges.candidates : [];
  const aggregate = current ? safeGovernanceAggregate(nudges?.source_health?.aggregate) : "unavailable";

  $("governanceCurrentState").textContent = current ? aggregate : pollState;
  $("governanceCurrentCount").textContent = current ? String(candidates.length) : "--";
  if (!current) {
    renderGovernanceEmpty("governanceCurrentList", "Current risk & process nudges are unavailable.");
  } else if (candidates.length === 0 && aggregate === "ready") {
    renderGovernanceEmpty("governanceCurrentList", "No current risk & process nudges.");
  } else if (candidates.length === 0) {
    renderGovernanceEmpty("governanceCurrentList", "Current eligibility is suppressed by source health.");
  } else {
    $("governanceCurrentList").replaceChildren(...candidates.map(governanceCandidateElement));
  }

  renderGovernanceSourceHealth(pollSource, nudges?.source_health, current);
  renderGovernanceContext(nudges?.context, current);
  renderGovernanceCoverage(nudges?.confirmed_flow_coverage, current);
  renderGovernanceHistory(governance?.occurrences);
  renderGovernanceDelivery(governance);
  renderGovernanceAttempts(governance?.attempts);
  renderGovernanceControlStatus();
}

function governanceCandidateElement(candidate = {}) {
  const row = document.createElement("div");
  row.className = `governance-row governance-row--${candidate.severity === "act" ? "act" : candidate.severity === "watch" ? "watch" : "unknown"}`;
  const copy = document.createElement("div");
  const title = document.createElement("b");
  title.textContent = typeof candidate.title === "string" ? candidate.title : "";
  const body = document.createElement("p");
  body.textContent = typeof candidate.body === "string" ? candidate.body : "";
  const meta = document.createElement("span");
  const severity = ["act", "watch"].includes(candidate.severity) ? candidate.severity : "";
  const destination = ["monitor", "alerts"].includes(candidate.destination) ? candidate.destination : "";
  meta.textContent = [severity, destination].filter(Boolean).join(" · ");
  copy.append(title, body);
  row.append(copy, meta);
  return row;
}

function renderGovernanceSourceHealth(pollSource = {}, sourceHealth = {}, current) {
  const target = $("governanceSourceHealth");
  const pollStateKnown = ["current", "stale", "not_observed", "unavailable"].includes(pollSource.state);
  const pollState = safeGovernancePollState(pollSource.state);
  const pollReason = !pollStateKnown
    ? "invalid_health"
    : typeof pollSource.reason === "string" && pollSource.reason
      ? safeGovernanceReason(pollSource.reason, "invalid_health")
      : "";
  const pollFacts = [pollState];
  if (pollReason) pollFacts.push(pollReason);
  if (pollSource.updated_at) pollFacts.push(`updated ${governanceTime(pollSource.updated_at)}`);
  if (pollSource.last_success_at) pollFacts.push(`last successful ${governanceTime(pollSource.last_success_at)}`);
  if (!current) {
    target.textContent = pollFacts.join(" · ");
    return;
  }
  const aggregate = safeGovernanceAggregate(sourceHealth?.aggregate);
  const parts = [`${aggregate} · poll ${pollFacts.join(" · ")}`];
  for (const name of governanceInputNames) {
    const input = sourceHealth?.[name] || {};
    const status = ["ok", "unapproved", "stale", "unavailable", "error"].includes(input.status) ? input.status : "error";
    const reason = safeGovernanceReason(input.reason, status === "ok" ? "" : "invalid_health");
    const asOf = input.as_of ? ` · ${governanceTime(input.as_of)}` : "";
    parts.push(`${name}: ${status}${reason ? ` · ${reason}` : ""}${asOf}`);
  }
  target.textContent = parts.join("\n");
}

function renderGovernanceContext(context, current) {
  const target = $("governanceContext");
  if (!current || !context) {
    target.textContent = "Context unavailable.";
    return;
  }
  const parts = [];
  if (context.shadow && Number.isFinite(context.shadow.count)) {
    parts.push(`Shadow count ${Math.trunc(context.shadow.count)}`);
  }
  if (context.drawdown) {
    const tier = context.drawdown.tier === "block" ? "block" : "unavailable";
    const consumed = context.drawdown.consumed_pct === null || !Number.isFinite(context.drawdown.consumed_pct)
      ? "measurement unavailable"
      : `${context.drawdown.consumed_pct.toFixed(1)}% consumed`;
    parts.push(`Drawdown ${tier} · ${consumed}`);
  }
  target.textContent = parts.length > 0 ? parts.join(" · ") : "No typed governance context.";
}

function renderGovernanceCoverage(coverage, current) {
  const target = $("governanceCoverage");
  const detail = $("governanceCoverageDetail");
  const button = $("governanceCutoverReviewButton");
  const unresolved = current && coverage?.pre_cutover_flows_unreviewed === true;
  button.hidden = !unresolved;
  if (!current || !coverage?.coverage_from) {
    target.textContent = "Confirmed-flow coverage unavailable.";
    detail.textContent = "Confirmed-flow coverage unavailable.";
    return;
  }
  target.textContent = unresolved ? "Pre-cutover flows need foreground review." : "Confirmed-flow coverage is reviewed.";
  detail.textContent = `Coverage from ${governanceTime(coverage.coverage_from)} · pre-cutover flows ${unresolved ? "unreviewed" : "reviewed"}`;
}

function renderGovernanceHistory(occurrences) {
  const rows = Array.isArray(occurrences) ? occurrences : [];
  $("governanceHistoryCount").textContent = String(rows.length);
  if (rows.length === 0) {
    renderGovernanceEmpty("governanceHistoryList", state.governance === null ? "Governance history not observed." : "No governance history recorded.");
    return;
  }
  $("governanceHistoryList").replaceChildren(...rows.map(governanceOccurrenceElement));
}

function governanceOccurrenceElement(occurrence = {}) {
  const row = governanceCandidateElement(occurrence);
  row.classList.add("governance-row--history");
  const status = document.createElement("small");
  const lifecycle = governanceOccurrenceLifecycle(occurrence);
  const at = lifecycle === "resolved" ? occurrence.resolved_at : lifecycle === "expired" ? occurrence.expires_at : occurrence.occurred_at;
  status.textContent = `${lifecycle} · ${governanceTime(at)}`;
  row.append(status);
  return row;
}

function governanceOccurrenceLifecycle(occurrence = {}, now = Date.now()) {
  if (occurrence.resolved_at) return "resolved";
  const expiresAt = Date.parse(occurrence.expires_at || "");
  if (Number.isFinite(expiresAt) && expiresAt <= now) return "expired";
  return "active";
}

function renderGovernanceDelivery(governance) {
  const health = governance?.delivery_health || {};
  const healthState = ["healthy", "suppressed", "degraded", "unavailable", "overflow"].includes(health.state) ? health.state : "unavailable";
  const healthClass = safeGovernanceTransportClass(health.class);
  const healthAt = governanceTimestamp(health.updated_at);
  const lastKnown = healthClass ? `${healthState} · ${healthClass}` : healthState;
  if (!healthAt) {
    $("governanceDeliveryHealth").textContent = "unavailable · updated not observed";
  } else if (state.governanceRefreshSucceeded === false) {
    $("governanceDeliveryHealth").textContent = `retained · refresh unavailable · last known ${lastKnown} · updated ${governanceTime(health.updated_at)}`;
  } else {
    $("governanceDeliveryHealth").textContent = `${lastKnown} · updated ${governanceTime(health.updated_at)}`;
  }

  const attempts = governance?.attempt_aggregate || {};
  const healthTotals = governance?.health_aggregate || {};
  const diagnostic = governance?.diagnostic || {};
  const diagnosticState = safeGovernanceTransportClass(diagnostic.state) || "not_observed";
  const lastAccepted = health.last_push_service_acceptance_at
    ? `last push-service acceptance ${governanceTime(health.last_push_service_acceptance_at)}`
    : "last push-service acceptance not observed";
  $("governanceDeliveryDetail").textContent = [
    lastAccepted,
    `attempts cumulative ${safeCount(attempts.cumulative_attempts)} · push_service_accepted ${safeCount(attempts.push_service_accepted)} · retryable_failures ${safeCount(attempts.retryable_failures)} · rejected ${safeCount(attempts.rejected)} · retry_pending ${safeCount(attempts.retry_pending)} · dead_subscription ${safeCount(attempts.dead_subscription)} · missed ${safeCount(attempts.missed)} · suppressed ${safeCount(attempts.suppressed)} · interrupted_uncertain ${safeCount(attempts.interrupted_uncertain)} · target_retired ${safeCount(attempts.target_retired)}`,
    `health partial_episodes ${safeCount(healthTotals.partial_episodes)} · state_write_failures ${safeCount(healthTotals.state_write_failures)} · recoveries ${safeCount(healthTotals.recoveries)} · overflows ${safeCount(healthTotals.overflows)}`,
    `diagnostic ${diagnosticState}${diagnostic.at ? ` · ${governanceTime(diagnostic.at)}` : ""}`,
  ].join("\n");
}

function renderGovernanceAttempts(attempts) {
  const target = $("governanceAttemptList");
  const rows = governanceAttemptRows(attempts);
  if (rows.length === 0) {
    renderGovernanceEmpty("governanceAttemptList", state.governance === null ? "Delivery attempts not observed." : "No recent delivery attempts.");
    return;
  }
  target.replaceChildren(...rows.map((attempt) => {
    const row = document.createElement("div");
    row.className = `governance-attempt governance-attempt--${attempt.class === "unknown" ? "unknown" : "known"}`;
    const className = document.createElement("b");
    className.textContent = attempt.class;
    const facts = document.createElement("p");
    facts.textContent = attempt.facts.join(" · ");
    row.append(className, facts);
    return row;
  }));
}

function governanceAttemptRows(attempts) {
  if (!Array.isArray(attempts)) return [];
  const recent = attempts
    .map((attempt, index) => ({ attempt: attempt || {}, index }))
    .sort((left, right) => governanceTimestamp(right.attempt.at) - governanceTimestamp(left.attempt.at) || right.index - left.index)
    .slice(0, governanceRecentAttemptLimit);
  const targets = new Map();
  return recent.map(({ attempt }) => {
    const opaqueTarget = typeof attempt.target_ref === "string" ? attempt.target_ref : "";
    if (opaqueTarget && !targets.has(opaqueTarget)) targets.set(opaqueTarget, targets.size + 1);
    const transportClass = safeGovernanceTransportClass(attempt.class) || "unknown";
    const facts = [];
    if (opaqueTarget) facts.push(`target ${targets.get(opaqueTarget)}`);
    facts.push(`at ${governanceTime(attempt.at)}`);
    if (attempt.completed_at) facts.push(`completed ${governanceTime(attempt.completed_at)}`);
    if (attempt.retry_at) facts.push(`retry ${governanceTime(attempt.retry_at)}`);
    if (attempt.target_retired_at) facts.push(`retired ${governanceTime(attempt.target_retired_at)}`);
    if (Number.isFinite(attempt.transport_count) && attempt.transport_count >= 0) facts.push(`transport count ${Math.trunc(attempt.transport_count)}`);
    return { class: transportClass, facts };
  });
}

function renderGovernanceControlStatus() {
  const safeTest = state.safeNotificationTest;
  $("safeNotificationTestButton").disabled = safeTest.busy;
  $("safeNotificationTestStatus").textContent = safeTest.busy ? "Safe notification test pending." : safeTest.state;
  $("safeNotificationTestStatus").classList.toggle("governance-action-status--error", safeTest.error);

  const cutover = state.governanceCutoverReview;
  $("governanceCutoverReviewButton").disabled = cutover.busy;
  $("governanceCutoverReviewStatus").textContent = cutover.busy ? "Cutover review pending authoritative refresh." : cutover.state;
  $("governanceCutoverReviewStatus").classList.toggle("governance-action-status--error", cutover.error);
}

function scheduleGovernanceRefresh(options = {}) {
  if (!state.authenticated) return false;
  const delayMs = Math.max(0, Number(options.delayMs) || 0);
  const minIntervalMs = options.minIntervalMs === undefined
    ? governanceSnapshotRefreshMinInterval
    : Math.max(0, Number(options.minIntervalMs) || 0);
  const now = Date.now();
  const throttleDelay = Math.max(0, minIntervalMs - (now - state.governanceLastRefreshAt));
  const dueAt = now + Math.max(delayMs, throttleDelay);
  const ensureTrailing = options.ensureTrailing === true;
  let timerEnsureTrailing = ensureTrailing;
  if (state.governanceRefreshTimer) {
    timerEnsureTrailing ||= state.governanceRefreshTimerEnsureTrailing;
    state.governanceRefreshTimerEnsureTrailing = timerEnsureTrailing;
    if (state.governanceRefreshDueAt <= dueAt) return true;
    clearTimeout(state.governanceRefreshTimer);
  }
  state.governanceRefreshDueAt = dueAt;
  state.governanceRefreshTimerEnsureTrailing = timerEnsureTrailing;
  state.governanceRefreshTimer = setTimeout(() => {
    const trailing = state.governanceRefreshTimerEnsureTrailing;
    state.governanceRefreshTimer = null;
    state.governanceRefreshDueAt = 0;
    state.governanceRefreshTimerEnsureTrailing = false;
    refreshGovernance({ ensureTrailing: trailing });
  }, Math.max(0, dueAt - Date.now()));
  return true;
}

async function refreshGovernance(options = {}) {
  if (!state.authenticated) return false;
  if (state.governanceRefreshInFlight) {
    if (options.ensureTrailing === true) state.governanceRefreshAfterFlight = true;
    return state.governanceRefreshInFlight;
  }
  state.governanceLastRefreshAt = Date.now();
  state.governanceRefreshInFlight = (async () => {
    try {
      const res = await fetch("/api/governance", { credentials: "include" });
      if (!res.ok) {
        state.governanceRefreshSucceeded = false;
        renderGovernance();
        return false;
      }
      const governance = validateGovernanceResponse(await res.json());
      if (!governance) {
        state.governanceRefreshSucceeded = false;
        renderGovernance();
        return false;
      }
      state.governance = governance;
      state.governanceRefreshSucceeded = true;
      renderGovernance();
      return true;
    } catch {
      state.governanceRefreshSucceeded = false;
      renderGovernance();
      return false;
    } finally {
      state.governanceRefreshInFlight = null;
      if (state.governanceRefreshAfterFlight) {
        state.governanceRefreshAfterFlight = false;
        scheduleGovernanceRefresh({ minIntervalMs: 0 });
      }
    }
  })();
  return state.governanceRefreshInFlight;
}

async function sendSafeNotificationTest() {
  const outcome = state.safeNotificationTest;
  if (outcome.busy) return;
  outcome.busy = true;
  outcome.state = "";
  outcome.error = false;
  renderGovernanceControlStatus();
  try {
    const res = await fetch("/api/push/test", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({}),
    });
    const body = res.ok ? await res.json() : {};
    if (!res.ok) throw new Error("safe test unavailable");
    const transportState = safeGovernanceTransportClass(body.state);
    if (body.push_service_accepted === true && transportState === "push_service_accepted") {
      outcome.state = "Push-service accepted.";
    } else if (body.push_service_accepted === true && transportState === "partial_acceptance") {
      outcome.state = "Partial push-service acceptance.";
    } else if (transportState === "suppressed") {
      outcome.state = "Safe notification test suppressed.";
    } else {
      outcome.state = `Safe notification test failed${transportState ? ` · ${transportState}` : ""}.`;
      outcome.error = true;
    }
  } catch {
    outcome.state = "Safe notification test unavailable.";
    outcome.error = true;
  } finally {
    outcome.busy = false;
    renderGovernanceControlStatus();
    refreshGovernance();
  }
}

async function sendGovernanceCutoverReview() {
  const outcome = state.governanceCutoverReview;
  if (outcome.busy) return;
  outcome.busy = true;
  outcome.state = "";
  outcome.error = false;
  renderGovernanceControlStatus();
  try {
    const res = await fetch("/api/governance/cutover-review", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({}),
    });
    const body = res.ok ? await res.json() : {};
    if (!res.ok || !applyGovernanceCutoverReceipt(body)) throw new Error("cutover review unavailable");
    outcome.state = body.already_reviewed === true ? "already recorded" : "foreground render recorded";
    scheduleGovernanceRefresh({ delayMs: 1500, minIntervalMs: 0, ensureTrailing: true });
  } catch {
    outcome.state = "Cutover review unavailable.";
    outcome.error = true;
  } finally {
    outcome.busy = false;
    renderGovernance();
  }
}

function applyGovernanceCutoverReceipt(receipt) {
  if (!receipt || receipt.ok !== true || typeof receipt.already_reviewed !== "boolean" || receipt.evidence !== "paired_device_foreground_render_review") return false;
  const reviewedAt = governanceTimestamp(receipt.reviewed_at);
  const coverageFrom = governanceTimestamp(receipt.coverage_from);
  if (!reviewedAt || !coverageFrom || coverageFrom > reviewedAt || !state.snapshot?.nudges) return false;
  state.governanceCutoverReceipt = {
    reviewed_at: receipt.reviewed_at,
    coverage_from: receipt.coverage_from,
  };
  state.snapshot = {
    ...state.snapshot,
    nudges: {
      ...state.snapshot.nudges,
      confirmed_flow_coverage: {
        coverage_from: receipt.coverage_from,
        pre_cutover_flows_unreviewed: false,
      },
    },
  };
  return true;
}

function applyGovernanceCutoverOverlay(snapshot) {
  const receipt = state.governanceCutoverReceipt;
  if (!receipt || !snapshot?.nudges) return snapshot;
  const reviewedAt = governanceTimestamp(receipt.reviewed_at);
  const authorityAt = governanceTimestamp(snapshot.nudges.as_of);
  if (!reviewedAt || !authorityAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  if (snapshot.nudges.confirmed_flow_coverage?.pre_cutover_flows_unreviewed === false && authorityAt >= reviewedAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  if (authorityAt > reviewedAt) {
    state.governanceCutoverReceipt = null;
    return snapshot;
  }
  return {
    ...snapshot,
    nudges: {
      ...snapshot.nudges,
      confirmed_flow_coverage: {
        coverage_from: receipt.coverage_from,
        pre_cutover_flows_unreviewed: false,
      },
    },
  };
}

function safeGovernanceAggregate(value) {
  return ["ready", "suppressed", "degraded"].includes(value) ? value : "suppressed";
}

function safeGovernancePollState(value) {
  return ["current", "stale", "not_observed", "unavailable"].includes(value) ? value : "unavailable";
}

function safeGovernanceReason(value, fallback = "invalid_health") {
  const reasons = new Set([
    "", "not_observed", "poll_stale", "transport_unavailable", "policy_unapproved", "cadence_unapproved",
    "evidence_stale", "source_unavailable", "evaluation_error", "coverage_unavailable",
    "cutover_review_required", "invalid_health",
  ]);
  return reasons.has(value) ? value : fallback;
}

function safeGovernanceTransportClass(value) {
  return governanceTransportClasses.has(value) ? value : "";
}

function safeCount(value) {
  return Number.isFinite(value) && value >= 0 ? Math.trunc(value) : 0;
}

function governanceTime(value) {
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "not observed";
  return `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, "0")}-${String(at.getDate()).padStart(2, "0")} ${shortTime(value)}`;
}

function governanceTimestamp(value) {
  const timestamp = Date.parse(value || "");
  return Number.isFinite(timestamp) ? timestamp : 0;
}

function renderGovernanceEmpty(id, copy) {
  const empty = document.createElement("div");
  empty.className = "empty-row";
  empty.textContent = copy;
  $(id).replaceChildren(empty);
}

function renderAlertList(id, items, emptyText) {
  const list = $(id);
  list.replaceChildren(...items.map(alertRowElement));
  if (items.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = emptyText;
    list.replaceChildren(empty);
  }
}

function alertRowElement(alert) {
  const row = document.createElement("button");
  row.className = "alert-row alert-row--" + alertTone(alert);
  row.classList.toggle("alert-row--stale", alertIsStale(alert));
  row.type = "button";
  row.classList.toggle("active", alert.id === state.selectedAlertID);
  row.addEventListener("click", () => {
    state.selectedAlertID = alert.id;
    renderAlerts();
    renderSelectedAlert();
    $("selectedAlertPanel").scrollIntoView({ block: "nearest" });
  });
  const text = document.createElement("div");
  text.className = "alert-row__copy";
  const title = document.createElement("b");
  title.textContent = alert.title;
  const body = document.createElement("p");
  body.textContent = alert.body;
  text.append(title, body);
  const sourceLabel = alertSourceLabel(alert);
  if (sourceLabel) {
    const at = document.createElement("span");
    at.className = "alert-row__source";
    at.textContent = sourceLabel;
    at.title = alertSourceTitle(alert);
    row.append(text, at);
  } else {
    row.classList.add("alert-row--nosource");
    row.title = alertSourceTitle(alert);
    row.append(text);
  }
  return row;
}

function alertSourceLabel(alert) {
  // Preview alerts only appear under the "Current signal" section header, so
  // a per-row "current signal" chip would restate it.
  if (alert.preview) return "";
  if (alertIsStale(alert)) return `stale: ${staleAlertReason(alert)}`;
  return alert.created_at ? `stored ${shortTime(alert.created_at)}` : "stored history";
}

function alertSourceTitle(alert) {
  if (alert.preview) return "Synthetic current Canary preview from the live snapshot";
  if (alertIsStale(alert)) return `Persisted alert from ${staleAlertReason(alert)}`;
  return "Persisted alert history for the current Canary context";
}

function renderSelectedAlert() {
  const alert = allAlertItems().find((item) => item.id === state.selectedAlertID);
  const panel = $("selectedAlertPanel");
  panel.hidden = !alert;
  if (!alert) return;
  $("selectedAlertTitle").textContent = alert.title || "Canary alert";
  const stale = alertIsStale(alert);
  $("selectedAlertBody").textContent = stale
    ? `Stale alert from a previous canary/account context. ${alert.body || ""}`.trim()
    : alert.body || "Open detail for the current canary context.";
  $("selectedAlertTime").textContent = stale
    ? "not valid for current daemon context"
    : alert.preview ? "current canary snapshot"
    : alert.created_at ? `recorded ${shortTime(alert.created_at)}` : "recorded --";
}

function currentCanaryFingerprint() {
  return state.snapshot?.canary?.fingerprint?.key || "";
}

function alertIsStale(alert) {
  const current = currentCanaryFingerprint();
  const canaryChanged = Boolean(alert?.fingerprint && current && alert.fingerprint !== current);
  const trading = state.snapshot?.trading || {};
  const accountChanged = Boolean(alert?.account && trading.account && alert.account !== trading.account);
  const modeChanged = Boolean(alert?.mode && trading.mode && alert.mode !== trading.mode);
  return canaryChanged || accountChanged || modeChanged;
}

function staleAlertReason(alert) {
  const current = currentCanaryFingerprint();
  if (alert?.fingerprint && current && alert.fingerprint !== current) return "previous signal";
  const trading = state.snapshot?.trading || {};
  if (alert?.account && trading.account && alert.account !== trading.account) return "previous account";
  if (alert?.mode && trading.mode && alert.mode !== trading.mode) return "previous mode";
  return "previous context";
}

function warningMessages(warnings = []) {
  return warnings.map((warning) => {
    if (!warning) return "";
    if (typeof warning === "string") return warning;
    return warning.message || warning.code || JSON.stringify(warning);
  }).filter(Boolean);
}

async function refreshAlerts() {
  try {
    const res = await fetch("/api/alerts", { credentials: "include" });
    if (!res.ok) return;
    state.alerts = await res.json();
    if (state.selectedAlertID && !allAlertItems().some((alert) => alert.id === state.selectedAlertID)) {
      state.selectedAlertID = null;
    }
    renderAlerts();
    renderSelectedAlert();
  } catch {
    // Alert history is secondary; SSE recovery handles app connectivity.
  }
}

function alertItems() {
  const history = currentHistoryAlertItems();
  const previews = liveAlertPreviewsSuppressed() ? [] : currentAlertPreviewItems();
  if (history.length === 0) return previews;
  const historyTitles = new Set(history.map((item) => String(item.title || "").toLowerCase()));
  return [
    ...history,
    ...previews.filter((item) => !historyTitles.has(String(item.title || "").toLowerCase())),
  ].slice(0, 3);
}

function allAlertItems() {
  return [
    ...(liveAlertPreviewsSuppressed() ? [] : currentAlertPreviewItems()),
    ...currentHistoryAlertItems(),
    ...previousContextAlertItems(),
  ];
}

function currentHistoryAlertItems() {
  return state.alerts
    .map((alert) => ({ ...alert, preview: false }))
    .filter((alert) => !alertIsStale(alert));
}

function previousContextAlertItems() {
  return state.alerts
    .map((alert) => ({ ...alert, preview: false }))
    .filter((alert) => alertIsStale(alert));
}

function currentAlertPreviewItems() {
  const canary = state.snapshot?.canary || {};
  if (!canaryHasPortfolioAlert(canary)) return [];
  const rows = canaryPreviewRows(canary);
  return rows.map((row, index) => ({
    id: `preview-${index}`,
    title: row.title || labelize(row.severity || "canary"),
    body: [row.guidance, row.evidence].filter(Boolean).join(" ") || canary.summary || "Current canary context.",
    created_at: canary.as_of,
    fingerprint: currentCanaryFingerprint(),
    severity: row.severity || canary.severity,
    preview: true,
  }));
}

function currentCanaryHasPortfolioAlert() {
  return canaryHasPortfolioAlert(state.snapshot?.canary || {});
}

function canaryHasPortfolioAlert(canary) {
  const fit = String(canary.portfolio_fit || "").toLowerCase();
  if (fit !== "low") return true;
  const portfolio = canary.portfolio || {};
  if ((portfolio.held_stress || []).length > 0) return true;
  const exposureValues = [
    portfolio.gross_exposure_pct_nlv,
    portfolio.net_delta_pct_nlv,
    portfolio.gross_delta_pct_nlv,
    portfolio.largest_exposure_pct_nlv,
    portfolio.largest_delta_pct_nlv,
  ];
  return exposureValues.some((value) => typeof value === "number" && Math.abs(value) >= 0.5);
}

function liveAlertPreviewsSuppressed() {
  const current = currentCanaryFingerprint();
  return Boolean(current && state.clearedAlertFingerprint === current);
}

function filterAlertItems(items) {
  if (state.alertFilter === "warnings") {
    return items.filter((item) => ["risk", "warn"].includes(alertTone(item)));
  }
  if (state.alertFilter === "info") {
    return items.filter((item) => alertTone(item) === "info");
  }
  return items;
}

function canaryPreviewRows(canary) {
  const rows = Array.isArray(canary.rows) ? canary.rows : [];
  const heldStress = heldStressItems(canary);
  if (heldStress.length === 0) return rows.slice(0, 3);
  const heldRow = {
    title: "Held-name stress",
    severity: "watch",
    guidance: "Review material held underlyings before acting.",
    evidence: heldStressSummary(heldStress, 2),
  };
  const hasHeldRow = rows.some((row) => {
    const text = `${row.title || ""} ${row.evidence || ""} ${row.guidance || ""}`.toLowerCase();
    return text.includes("held") && text.includes("stress");
  });
  if (hasHeldRow) return rows.slice(0, 3);
  return [...rows.slice(0, 2), heldRow];
}

function alertTone(alert) {
  const severity = String(alert.severity || "").toLowerCase();
  const action = String(alert.action || "").toLowerCase();
  if (["act", "risk", "high", "critical"].includes(severity) || ["defend", "rebalance"].includes(action)) return "risk";
  if (["watch", "warn", "warning", "medium"].includes(severity)) return "warn";
  if (["observe", "ok", "info", "low"].includes(severity)) return "info";

  const text = `${alert.title || ""} ${alert.body || ""}`.toLowerCase();
  if (text.includes("act now") || text.includes("defend now") || text.includes("high severity")) return "risk";
  if (text.includes("watch") || text.includes("warn") || text.includes("spike") || text.includes("down")) return "warn";
  return "info";
}

async function clearAlerts() {
  const res = await fetch("/api/alerts", { method: "DELETE", credentials: "include" });
  if (!res.ok) return;
  state.alerts = [];
  state.selectedAlertID = null;
  const fp = currentCanaryFingerprint();
  if (fp) {
    state.clearedAlertFingerprint = fp;
    localStorage.setItem("ibkrClearedAlertFingerprint", fp);
  }
  renderAlerts();
  renderSelectedAlert();
}

async function enablePush() {
  if (!canUseWebPush()) {
    state.pushInspection.state = "unsupported";
    renderAlertMode();
    return;
  }
  state.pushInspection.busy = true;
  try {
    const permission = await globalThis.Notification.requestPermission();
    if (permission !== "granted") return;
    const registration = await navigator.serviceWorker.ready;
    const existing = await registration.pushManager.getSubscription();
    const subscription = existing || await registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: b64urlToBytes(state.vapidPublicKey),
    });
    const res = await fetch("/api/push/subscribe", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(subscription),
    });
    if (!res.ok) throw new Error("subscription unavailable");
  } catch {
    state.pushInspection.state = "status unavailable";
  } finally {
    state.pushInspection.busy = false;
    await refreshPushState();
  }
}

function notificationStateLabel() {
  return state.pushInspection.state;
}

async function refreshPushState() {
  let label = "status unavailable";
  if (!canUseWebPush()) {
    label = "unsupported";
  } else if (globalThis.Notification.permission === "denied") {
    label = "permission blocked";
  } else if (globalThis.Notification.permission !== "granted") {
    label = "permission not granted";
  } else {
    try {
      const registration = await navigator.serviceWorker.ready;
      const subscription = await registration.pushManager.getSubscription();
      label = subscription ? "browser subscribed" : "permission granted but not subscribed";
    } catch {
      label = "status unavailable";
    }
  }
  state.pushInspection.state = label;
  renderAlertMode();
  return label;
}

function hasNotifications() {
  return typeof globalThis.Notification === "function";
}

function canUseWebPush() {
  return hasNotifications() && "PushManager" in globalThis && !!navigator.serviceWorker;
}

export { acknowledgeAttention, alertIsStale, alertItems, alertRowElement, alertSourceLabel, alertSourceTitle, alertTone, allAlertItems, applyAttention, applyGovernanceCutoverOverlay, applyGovernanceCutoverReceipt, attentionViewReady, canUseWebPush, canaryHasPortfolioAlert, canaryPreviewRows, clearAlerts, currentAlertPreviewItems, currentCanaryFingerprint, currentCanaryHasPortfolioAlert, currentHistoryAlertItems, enablePush, fetchAttentionHistories, filterAlertItems, governanceAttemptRows, governanceOccurrenceLifecycle, handleAttentionContextChange, hasNotifications, liveAlertPreviewsSuppressed, notificationStateLabel, previousContextAlertItems, refreshAlerts, refreshAttention, refreshGovernance, refreshPushState, renderAlertList, renderAlertMode, renderAlerts, renderAttention, renderGovernance, renderSelectedAlert, scheduleGovernanceRefresh, sendGovernanceCutoverReview, sendSafeNotificationTest, setAlertMode, setupAttentionVisibility, staleAlertReason, unreadRefsAppear, validateAlertSettings, validateAttention, validateGovernanceResponse, warningMessages };
