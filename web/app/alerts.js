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

// A rendered Alerts view marks evidence read only after it has plausibly held
// the operator's attention: a short continuous dwell, or an explicit
// interaction inside the view. A resume flash must not consume unread — the
// app reopens on the last-used tab, so render-equals-read zeroed the badge on
// every app open that merely passed through Alerts (operator finding,
// 2026-07-19).
const attentionDwellDefaultMs = 2000;
let attentionDwellTimer = null;

function attentionDwellDelayMs() {
  return Number.isSafeInteger(state.attentionDwellMs) && state.attentionDwellMs >= 0 ? state.attentionDwellMs : attentionDwellDefaultMs;
}

function cancelAttentionDwell() {
  if (attentionDwellTimer) {
    clearTimeout(attentionDwellTimer);
    attentionDwellTimer = null;
  }
}

function handleAttentionContextChange() {
  if (!attentionViewReady()) {
    cancelAttentionDwell();
    return refreshAttention();
  }
  if (attentionDwellTimer) return true;
  attentionDwellTimer = setTimeout(() => {
    attentionDwellTimer = null;
    if (attentionViewReady()) acknowledgeAttention();
  }, attentionDwellDelayMs());
  return true;
}

// Deliberate interaction inside the Alerts view (a tap or a scroll) is
// attention now; it skips the remaining dwell.
function acknowledgeAttentionNow() {
  cancelAttentionDwell();
  if (!attentionViewReady()) return false;
  return acknowledgeAttention();
}

function setupAttentionVisibility() {
  if (attentionVisibilityBound) return;
  attentionVisibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") {
      cancelAttentionDwell();
      return;
    }
    refreshPushState();
    handleAttentionContextChange();
  });
  const panel = $("alertsTab");
  if (panel && typeof panel.addEventListener === "function") {
    panel.addEventListener("pointerdown", acknowledgeAttentionNow);
    panel.addEventListener("scroll", acknowledgeAttentionNow, { capture: true, passive: true });
  }
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

// The Alerts page is severity-first: the header counts conditions that need
// attention (never all-clear rows), passed checks collapse into one disclosure
// line, and history lives behind a collapsed section. Presence of a row must
// mean something needs the operator.
function renderAlerts() {
  // Conditions are always computed from the full preview set: dismissing rows
  // hides watch/data noise from the list, never the facts — the header counter
  // stays truthful, and act-severity rows are never dismissible.
  const allPreviews = currentAlertPreviewItems();
  const suppressed = liveAlertPreviewsSuppressed();
  const attentionItems = allPreviews.filter((item) => alertTone(item) !== "info");
  const passedItems = allPreviews.filter((item) => alertTone(item) === "info");
  const historyItems = currentHistoryAlertItems();
  const firstSeen = firstSeenForCurrentSignal(historyItems);
  const visibleHistory = historyItems.filter((item) => !firstSeen.ids.has(item.id));
  const previousItems = previousContextAlertItems();
  const hasCanary = Boolean(state.snapshot?.canary?.as_of);
  // History conditions still current (e.g. a protective-stop mismatch) count as
  // attention when no live preview already covers the same condition. One title
  // is one condition regardless of how many records it produced, and before the
  // first canary snapshot arrives no staleness verdict is possible, so history
  // stays history.
  const attentionTitles = new Set(attentionItems.map((item) => String(item.title || "").toLowerCase()));
  const historyConditions = [];
  if (hasCanary) {
    const seenTitles = new Set();
    for (const item of visibleHistory) {
      const title = String(item.title || "").toLowerCase();
      if (alertTone(item) === "info" || attentionTitles.has(title) || seenTitles.has(title)) continue;
      seenTitles.add(title);
      historyConditions.push(item);
    }
  }
  const conditions = [...attentionItems, ...historyConditions];
  const acts = conditions.filter((item) => alertTone(item) === "risk").length;
  const dataIssues = conditions.filter(isDataQualityItem).length;
  const watches = conditions.filter((item) => alertTone(item) === "warn").length - dataIssues;

  const count = $("alertCount");
  count.textContent = acts > 0 || watches > 0 || dataIssues > 0
    ? [acts > 0 ? `${acts} act` : "", watches > 0 ? `${watches} watch` : "", dataIssues > 0 ? `${dataIssues} data` : ""].filter(Boolean).join(" · ")
    : hasCanary ? "All clear" : "--";
  count.classList.toggle("is-zero", acts === 0 && watches === 0 && dataIssues === 0);
  count.classList.toggle("has-risk", acts > 0);
  count.classList.toggle("has-warn", acts === 0 && (watches > 0 || dataIssues > 0));

  renderAlertsStatusLine(firstSeen.at);
  // The section number counts decisions (act + watch); data caveats render in
  // their own quieter band below and never inflate it.
  $("currentSignalCount").textContent = String(acts + watches);
  const listed = suppressed
    ? conditions.filter((item) => alertTone(item) === "risk")
    : conditions;
  const toneRank = { risk: 0, warn: 1 };
  const decisionRows = listed.filter((item) => !isDataQualityItem(item))
    .sort((a, b) => (toneRank[alertTone(a)] ?? 2) - (toneRank[alertTone(b)] ?? 2));
  const dataRows = listed.filter(isDataQualityItem);
  renderAttentionList(decisionRows, dataRows,
    !hasCanary ? "Waiting for the first canary snapshot."
      : suppressed && conditions.length > 0 ? "Watch and data rows are hidden until the signal changes."
        : "Nothing needs your attention right now.");
  renderPassedChecks(suppressed ? [] : passedItems);

  const historyCount = visibleHistory.length + previousItems.length;
  $("alertsHistorySection").hidden = historyCount === 0;
  $("alertHistoryCount").textContent = String(historyCount);
  renderAlertList("alertHistoryList", visibleHistory, "No recorded alerts for the current context.");
  $("previousContextAlerts").hidden = previousItems.length === 0;
  $("previousContextCount").textContent = String(previousItems.length);
  renderAlertList("previousContextList", previousItems, "No alerts from a previous context.");
  $("alertsHint").textContent = state.alerts.length > 0
    ? "Clearing removes read history only; live signals are unaffected."
    : "No alert history recorded yet.";
  $("clearAlertsButton").disabled = state.alerts.length === 0;

  const dismiss = $("dismissCurrentButton");
  const dismissible = attentionItems.some((item) => alertTone(item) !== "risk") || passedItems.length > 0;
  dismiss.hidden = suppressed || !dismissible;
  dismiss.title = "Hides watch and data rows for this signal. Act rows and the counter are unaffected; rows return when the canary signal changes.";
  // The banner reads snapshot session state, which arrives on this render
  // path; the governance path re-renders it when delivery health changes.
  renderDeliveryBanner();
}

function renderAttentionList(decisionRows, dataRows, emptyText) {
  const list = $("currentSignalList");
  const children = decisionRows.map(alertRowElement);
  if (dataRows.length > 0) {
    const divider = document.createElement("div");
    divider.className = "alert-section__subhead";
    divider.textContent = `Data caveats (${dataRows.length}) — reasons to discount signals, not decisions`;
    children.push(divider, ...dataRows.map(alertRowElement));
  }
  if (children.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = emptyText;
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...children);
}

// The calendar's not-open umbrella covers phases with different price
// stories: pre-open on a trading date shows live indicative pre-market prints
// anchored to the last close, after-close shows that session's closing
// prints, and weekend/holiday closures show older last-session prints.
// Classify with the same open/close bounds the header ticker uses so this
// page never calls live pre-market prints "last-session". An absent calendar
// or no-coverage state is "unknown": claim nothing about the session.
function marketSessionPhase() {
  const session = state.snapshot?.market_calendar?.session;
  if (!session || session.state === "unknown") return "unknown";
  if (session.is_open === true) return "open";
  const now = Date.now();
  const open = Date.parse(session.open ?? "");
  const close = Date.parse(session.close ?? "");
  if (Number.isFinite(open) && now < open) return "pre-open";
  if (Number.isFinite(close) && now >= close) return "after-close";
  if (session.state === "holiday") return "holiday";
  return session.reason === "weekend" ? "weekend" : "closed";
}

// Push delivery failing while the operator may be trading unalerted belongs
// at the top of the page in plain English, not inside the governance evidence
// disclosure. That window is the open session plus pre-open on a trading
// date — pre-market alerts are exactly the ones a phone must deliver before
// the bell. Known non-trading phases (after close, weekend, holiday) keep the
// quiet disclosure-only presentation; an unknown session state fails visible.
// Suppressed delivery (operator chose Off) is not a failure and never banners.
function renderDeliveryBanner() {
  const banner = $("alertsDeliveryBanner");
  const health = state.governance?.delivery_health || {};
  const failing = ["degraded", "unavailable"].includes(health.state);
  const phase = marketSessionPhase();
  const attentive = phase === "open" || phase === "pre-open" || phase === "unknown";
  if (!failing || !attentive) {
    banner.hidden = true;
    banner.textContent = "";
    return;
  }
  const when = phase === "pre-open" ? " ahead of the open"
    : phase === "open" ? " while the market is open"
      : "";
  banner.textContent = health.state === "degraded"
    ? `Alerts may not be reaching your phone — push delivery is degraded${when}. Rely on this page until delivery recovers.`
    : `Alerts are not reaching your phone — push delivery is down${when}. Rely on this page and check notification settings.`;
  banner.hidden = false;
}

// The one-sentence page lede: the daemon-authored canary summary plus when the
// condition was first recorded and how fresh the snapshot is.
function renderAlertsStatusLine(firstSeenAt) {
  const status = $("alertsStatusLine");
  const canary = state.snapshot?.canary || {};
  if (!canary.summary) {
    status.hidden = true;
    return;
  }
  const parts = [canary.summary];
  const firstSeen = firstSeenAt ? alertDayTime(firstSeenAt) : "";
  const asOf = canary.as_of ? alertDayTime(canary.as_of) : "";
  if (firstSeen && firstSeen !== asOf) parts.push(`First seen ${firstSeen}.`);
  if (asOf) parts.push(sessionPriceStamp(asOf));
  status.textContent = parts.join(" ");
  status.hidden = false;
  const tone = alertTone({ severity: canary.severity, action: canary.action });
  status.classList.toggle("alerts-status--risk", tone === "risk");
  status.classList.toggle("alerts-status--warn", tone === "warn");
}

// The stamp tells the price story for the session phase: a closed market must
// not launder frozen prints as fresh, and pre-market must not call live
// indicative prints "last-session" — pre-open, prints are live and only the
// daily-change anchors come from the last close. An unknown phase claims
// nothing about the session.
function sessionPriceStamp(asOf) {
  switch (marketSessionPhase()) {
    case "pre-open":
      return `Snapshot ${asOf} · pre-market — live indicative prints; daily changes anchor to the last close.`;
    case "after-close":
      return `Snapshot ${asOf} · after close — prices are last-session prints.`;
    case "weekend":
      return `Snapshot ${asOf} · weekend — prices are last-session prints.`;
    case "holiday":
      return `Snapshot ${asOf} · market holiday — prices are last-session prints.`;
    case "closed":
      return `Snapshot ${asOf} · market closed — prices are last-session prints.`;
    default:
      return `Data as of ${asOf}.`;
  }
}

function renderPassedChecks(passedItems) {
  const details = $("alertsPassedChecks");
  details.hidden = passedItems.length === 0;
  if (passedItems.length === 0) return;
  $("alertsPassedSummary").textContent = `${passedItems.length} check${passedItems.length === 1 ? "" : "s"} passed — no action from these`;
  $("alertsPassedList").replaceChildren(...passedItems.map((item) => {
    const row = document.createElement("div");
    row.className = "passed-row";
    const title = document.createElement("b");
    title.textContent = item.title;
    const fact = document.createElement("p");
    fact.textContent = [item.body, item.evidence].filter(Boolean).join(" · ");
    row.append(title, fact);
    return row;
  }));
}

// A stored canary alert whose fingerprint matches the live snapshot is the
// same condition the page already shows — surface its timestamp as
// "first seen" instead of rendering a duplicate row.
function firstSeenForCurrentSignal(historyItems) {
  const current = currentCanaryFingerprint();
  const ids = new Set();
  let at = "";
  if (!current) return { ids, at };
  for (const item of historyItems) {
    if (!isCanarySourceAlert(item) || item.fingerprint !== current) continue;
    ids.add(item.id);
    if (!at || (item.created_at && item.created_at < at)) at = item.created_at || at;
  }
  return { ids, at };
}

function isCanarySourceAlert(alert) {
  return typeof alert?.id === "string" && alert.id.startsWith("canary-");
}

// Within the last two days a weekday reads naturally; anything older needs a
// real date — "Sun 09:23 PM" is ambiguous in a history spanning weeks.
function alertDayTime(value) {
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "--";
  const options = Date.now() - at.getTime() > 48 * 3600 * 1000
    ? { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }
    : { weekday: "short", hour: "2-digit", minute: "2-digit" };
  return at.toLocaleString(undefined, options);
}

// One plain sentence carries the whole section state; the chip is a matching
// single word. Raw aggregate/poll enum values never render outside the
// evidence disclosure.
function governanceStateChip(current, aggregate, pollState) {
  if (!current) return pollState === "not_observed" ? "waiting" : pollState;
  if (aggregate === "ready") return "active";
  if (aggregate === "degraded") return "degraded";
  return "paused";
}

function governanceSummaryCopy(current, aggregate, pollState, candidateCount) {
  if (!current) {
    if (pollState === "stale") return "Process check status is stale; showing the last known state.";
    if (pollState === "not_observed") return "Process check status not observed yet.";
    return "Process check status is unavailable.";
  }
  if (candidateCount > 0) {
    return candidateCount === 1 ? "1 process reminder needs review below." : `${candidateCount} process reminders need review below.`;
  }
  if (aggregate === "ready") return "No process reminders; process data sources are healthy.";
  if (aggregate === "degraded") return "Process data sources are degraded; reminders may be incomplete.";
  return "Process checks are paused — data sources are not ready (normal outside market hours), so no reminders can be evaluated.";
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

  $("governanceCurrentState").textContent = governanceStateChip(current, aggregate, pollState);
  $("governanceCurrentCount").textContent = current ? String(candidates.length) : "--";
  $("governanceSummary").textContent = governanceSummaryCopy(current, aggregate, pollState, candidates.length);
  $("governanceCurrentBlock").hidden = !current || candidates.length === 0;
  if (!current) {
    renderGovernanceEmpty("governanceCurrentList", "Current risk & process nudges are unavailable.");
  } else if (candidates.length === 0 && aggregate === "ready") {
    renderGovernanceEmpty("governanceCurrentList", "No current risk & process nudges.");
  } else if (candidates.length === 0) {
    renderGovernanceEmpty("governanceCurrentList", "Paused by source health — an empty list is not a clean result.");
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
  renderDeliveryBanner();
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
  // Healthy inputs collapse to one line; only inputs that are not ok earn a row.
  const unhealthy = [];
  for (const name of governanceInputNames) {
    const input = sourceHealth?.[name] || {};
    const status = ["ok", "unapproved", "stale", "unavailable", "error"].includes(input.status) ? input.status : "error";
    if (status === "ok") continue;
    const reason = safeGovernanceReason(input.reason, "invalid_health");
    const asOf = input.as_of ? ` · ${governanceTime(input.as_of)}` : "";
    unhealthy.push(`${name}: ${status}${reason ? ` · ${reason}` : ""}${asOf}`);
  }
  parts.push(unhealthy.length === 0 ? "all inputs ok" : unhealthy.join("\n"));
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

// The coverage block surfaces inline only while it needs the operator (the
// one-time cutover review); the reviewed state stays available in the
// evidence disclosure.
function renderGovernanceCoverage(coverage, current) {
  const block = $("governanceCoverageBlock");
  const target = $("governanceCoverage");
  const detail = $("governanceCoverageDetail");
  const button = $("governanceCutoverReviewButton");
  const unresolved = current && coverage?.pre_cutover_flows_unreviewed === true;
  button.hidden = !unresolved;
  if (!current || !coverage?.coverage_from) {
    block.hidden = true;
    target.textContent = "Confirmed-flow coverage unavailable.";
    detail.textContent = "Confirmed-flow coverage unavailable.";
    return;
  }
  block.hidden = !unresolved && !state.governanceCutoverReview.state;
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
  // Counter walls carry no information at zero: render only nonzero facts.
  const attemptFacts = [
    ["cumulative", attempts.cumulative_attempts], ["push_service_accepted", attempts.push_service_accepted],
    ["retryable_failures", attempts.retryable_failures], ["rejected", attempts.rejected],
    ["retry_pending", attempts.retry_pending], ["dead_subscription", attempts.dead_subscription],
    ["missed", attempts.missed], ["suppressed", attempts.suppressed],
    ["interrupted_uncertain", attempts.interrupted_uncertain], ["target_retired", attempts.target_retired],
  ].filter(([, value]) => safeCount(value) > 0).map(([label, value]) => `${label} ${safeCount(value)}`);
  const healthFacts = [
    ["partial_episodes", healthTotals.partial_episodes], ["state_write_failures", healthTotals.state_write_failures],
    ["recoveries", healthTotals.recoveries], ["overflows", healthTotals.overflows],
  ].filter(([, value]) => safeCount(value) > 0).map(([label, value]) => `${label} ${safeCount(value)}`);
  $("governanceDeliveryDetail").textContent = [
    lastAccepted,
    attemptFacts.length > 0 ? `attempts ${attemptFacts.join(" · ")}` : "no delivery attempts recorded",
    healthFacts.length > 0 ? `health ${healthFacts.join(" · ")}` : "",
    `diagnostic ${diagnosticState}${diagnostic.at ? ` · ${governanceTime(diagnostic.at)}` : ""}`,
  ].filter(Boolean).join("\n");
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
  const tone = alertTone(alert);
  const row = document.createElement("div");
  row.className = "alert-row alert-row--" + tone;
  row.classList.toggle("alert-row--stale", alertIsStale(alert));
  row.classList.toggle("active", alert.id === state.selectedAlertID);
  row.addEventListener("click", () => {
    state.selectedAlertID = alert.id;
    renderAlerts();
    renderSelectedAlert();
  });
  const text = document.createElement("div");
  text.className = "alert-row__copy";
  const head = document.createElement("div");
  head.className = "alert-row__head";
  const chip = document.createElement("span");
  chip.className = `alert-chip alert-chip--${isDataQualityItem(alert) ? "data" : tone}`;
  chip.textContent = alertIsStale(alert) ? "EXPIRED" : alertChipLabel(alert);
  const title = document.createElement("b");
  title.textContent = alert.title;
  head.append(chip, title);
  const body = document.createElement("p");
  body.textContent = alert.body;
  text.append(head, body);
  if (alert.preview && alert.evidence) {
    const detail = document.createElement("details");
    detail.className = "alert-row__details";
    detail.addEventListener("click", (event) => event.stopPropagation());
    const summary = document.createElement("summary");
    summary.textContent = "Details";
    const evidence = document.createElement("p");
    evidence.textContent = alert.evidence;
    detail.append(summary, evidence);
    text.append(detail);
  }
  const sourceLabel = alertSourceLabel(alert);
  if (sourceLabel) {
    const at = document.createElement("span");
    at.className = "alert-row__source";
    at.textContent = sourceLabel;
    row.append(text, at);
  } else {
    row.classList.add("alert-row--nosource");
    row.append(text);
  }
  return row;
}

function alertSourceLabel(alert) {
  // Preview alerts already sit under the "Needs attention" header; a per-row
  // chip would restate it.
  if (alert.preview) return "";
  if (alertIsStale(alert)) return staleAlertReason(alert);
  return alert.created_at ? `recorded ${alertDayTime(alert.created_at)}` : "recorded";
}

function renderSelectedAlert() {
  const alert = allAlertItems().find((item) => item.id === state.selectedAlertID);
  const panel = $("selectedAlertPanel");
  panel.hidden = !alert;
  if (!alert) return;
  $("selectedAlertTitle").textContent = alert.title || "Canary alert";
  const stale = alertIsStale(alert);
  $("selectedAlertBody").textContent = stale
    ? `From ${staleAlertReason(alert).startsWith("different") ? "a" : "an"} ${staleAlertReason(alert)} — no longer applies. ${alert.body || ""}`.trim()
    : alert.body || "Open detail for the current canary context.";
  $("selectedAlertTime").textContent = stale
    ? "no longer applies to the current context"
    : alert.preview ? "current canary snapshot"
    : alert.created_at ? `recorded ${alertDayTime(alert.created_at)}` : "recorded --";
}

function currentCanaryFingerprint() {
  return state.snapshot?.canary?.fingerprint?.key || "";
}

// Fingerprint staleness applies only to canary-source alerts: other sources
// (e.g. protective-stop mismatches) carry their own fingerprint scheme and
// must stay current until their account or mode context changes.
function alertIsStale(alert) {
  const current = currentCanaryFingerprint();
  const canaryChanged = isCanarySourceAlert(alert) && Boolean(alert?.fingerprint && current && alert.fingerprint !== current);
  const trading = state.snapshot?.trading || {};
  const accountChanged = Boolean(alert?.account && trading.account && alert.account !== trading.account);
  const modeChanged = Boolean(alert?.mode && trading.mode && alert.mode !== trading.mode);
  return canaryChanged || accountChanged || modeChanged;
}

function staleAlertReason(alert) {
  const current = currentCanaryFingerprint();
  if (isCanarySourceAlert(alert) && alert?.fingerprint && current && alert.fingerprint !== current) return "earlier signal";
  const trading = state.snapshot?.trading || {};
  if (alert?.account && trading.account && alert.account !== trading.account) return "different account";
  if (alert?.mode && trading.mode && alert.mode !== trading.mode) return "different mode";
  return "earlier context";
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
    body: row.guidance || canary.summary || "Current canary context.",
    evidence: row.evidence || "",
    created_at: canary.as_of,
    fingerprint: currentCanaryFingerprint(),
    severity: row.severity || canary.severity,
    direction: row.direction || "",
    preview: true,
  }));
}

function currentCanaryHasPortfolioAlert() {
  return canaryHasPortfolioAlert(state.snapshot?.canary || {});
}

// The daemon stamps portfolio_alert_relevant on every canary snapshot; the
// single policy copy lives in internal/canary (a low-fit flat book is market
// weather, not a portfolio alert). An unstamped snapshot — an older daemon —
// fails open to relevant: version skew may add market-weather rows but must
// never hide alerts.
function canaryHasPortfolioAlert(canary) {
  return canary.portfolio_alert_relevant !== false;
}

function liveAlertPreviewsSuppressed() {
  const current = currentCanaryFingerprint();
  return Boolean(current && state.clearedAlertFingerprint === current);
}

// Every per-topic canary row is a candidate; the "Portfolio canary" overall
// row feeds the status line instead. A synthesized held-name row is appended
// only when no daemon row already covers held stress.
function canaryPreviewRows(canary) {
  const rows = (Array.isArray(canary.rows) ? canary.rows : [])
    .filter((row) => String(row.title || "") !== "Portfolio canary");
  const heldStress = heldStressItems(canary);
  if (heldStress.length === 0) return rows;
  const hasHeldRow = rows.some((row) => String(row.title || "") === "Held-name stress");
  if (hasHeldRow) return rows;
  return [...rows, {
    title: "Held-name stress",
    severity: "watch",
    guidance: "Review material held underlyings before acting.",
    evidence: heldStressSummary(heldStress, 2),
  }];
}

// Severity maps exactly onto the daemon ladder (observe < watch < act <
// urgent, with order-mismatch "critical"); direction-style actions are the
// only fallback. No text sniffing.
function alertTone(alert) {
  const severity = String(alert.severity || "").toLowerCase();
  if (["urgent", "act", "critical"].includes(severity)) return "risk";
  if (["watch", "warn", "warning"].includes(severity)) return "warn";
  if (["observe", "ok", "info"].includes(severity)) return "info";
  const action = String(alert.action || "").toLowerCase();
  if (["defend", "rebalance", "order_mismatch"].includes(action)) return "risk";
  return "info";
}

const alertToneChip = { risk: "ACT", warn: "WATCH", info: "INFO" };

// Data-quality caveats need eyes but are not market conditions; the chip
// says which world the row lives in.
function alertChipLabel(alert) {
  if (String(alert.direction || "") === "data_quality" && alertTone(alert) === "warn") return "DATA";
  return alertToneChip[alertTone(alert)];
}

function isDataQualityItem(alert) {
  return String(alert.direction || "") === "data_quality" && alertTone(alert) === "warn";
}

// Clearing history and dismissing the live previews are separate, honestly
// labeled actions: clear removes read history records only; dismiss suppresses
// the current snapshot's previews until the canary signal changes.
async function clearAlerts() {
  const res = await fetch("/api/alerts", { method: "DELETE", credentials: "include" });
  if (!res.ok) return;
  state.alerts = [];
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
}

function dismissCurrentSignals() {
  const fp = currentCanaryFingerprint();
  if (!fp) return;
  state.clearedAlertFingerprint = fp;
  localStorage.setItem("ibkrClearedAlertFingerprint", fp);
  renderAlerts();
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

export { acknowledgeAttention, acknowledgeAttentionNow, alertIsStale, alertRowElement, alertSourceLabel, alertTone, allAlertItems, applyAttention, applyGovernanceCutoverOverlay, applyGovernanceCutoverReceipt, attentionViewReady, canUseWebPush, canaryHasPortfolioAlert, canaryPreviewRows, clearAlerts, currentAlertPreviewItems, currentCanaryFingerprint, currentCanaryHasPortfolioAlert, currentHistoryAlertItems, dismissCurrentSignals, enablePush, fetchAttentionHistories, firstSeenForCurrentSignal, governanceAttemptRows, governanceOccurrenceLifecycle, handleAttentionContextChange, hasNotifications, liveAlertPreviewsSuppressed, notificationStateLabel, previousContextAlertItems, refreshAlerts, refreshAttention, refreshGovernance, refreshPushState, renderAlertList, renderAlertMode, renderAlerts, renderAttention, renderDeliveryBanner, renderGovernance, renderSelectedAlert, scheduleGovernanceRefresh, sendGovernanceCutoverReview, sendSafeNotificationTest, setAlertMode, setupAttentionVisibility, staleAlertReason, unreadRefsAppear, validateAlertSettings, validateAttention, validateGovernanceResponse, warningMessages };
