import { b64urlToBytes } from "./auth.js";
import { heldStressItems, heldStressSummary } from "./canary.js";
import { $, labelize, shortTime } from "./shared.js";
import { state } from "./state.js";

function renderAlertMode() {
  document.querySelectorAll("#alertSegments button").forEach((button) => {
    button.classList.toggle("active", button.dataset.mode === state.alertSettings.mode);
  });
  $("pushState").textContent = notificationStateLabel();
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
    $("pushState").textContent = "push unsupported";
    return;
  }
  const registration = await navigator.serviceWorker.ready;
  const permission = await globalThis.Notification.requestPermission();
  if (permission !== "granted") {
    renderAlertMode();
    return;
  }
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: b64urlToBytes(state.vapidPublicKey),
  });
  await fetch("/api/push/subscribe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify(subscription),
  });
  renderAlertMode();
}

function notificationStateLabel() {
  if (!hasNotifications()) return "push unsupported";
  return globalThis.Notification.permission === "granted" ? "push on" : "push off";
}

function hasNotifications() {
  return typeof globalThis.Notification === "function";
}

function canUseWebPush() {
  return hasNotifications() && "PushManager" in globalThis && !!navigator.serviceWorker;
}

export { alertIsStale, alertItems, alertRowElement, alertSourceLabel, alertSourceTitle, alertTone, allAlertItems, canUseWebPush, canaryHasPortfolioAlert, canaryPreviewRows, clearAlerts, currentAlertPreviewItems, currentCanaryFingerprint, currentCanaryHasPortfolioAlert, currentHistoryAlertItems, enablePush, filterAlertItems, hasNotifications, liveAlertPreviewsSuppressed, notificationStateLabel, previousContextAlertItems, refreshAlerts, renderAlertList, renderAlertMode, renderAlerts, renderSelectedAlert, staleAlertReason, warningMessages };
