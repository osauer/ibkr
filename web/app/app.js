import { clearAlerts, enablePush, renderAlertMode, renderAlerts, renderSelectedAlert } from "./alerts.js";
import { completePairing } from "./auth.js";
import { canaryStageLabel, canarySummaryText, firstClause, renderCanaryDetail, renderCanaryStatus, renderCanaryTimestamp, renderMarketContext, renderRegimePanel, renderRulesCard } from "./canary.js";
import { ensureRegimeCanaryExpansion, handleAccountPanelTap, handleExpandablePanelTap, handleOpportunitiesPanelTap, handlePortfolioPanelTap, handleProtectionPanelTap, handleUnderlyingPanelTap, renderTabs, resetViewportScroll, setAccountOverviewExpansion, setAccountValueVisible, setOpportunitiesExpansion, setProtectionExpansion, setRegimeCanaryExpansion, setupBottomTabs, syncAccountPrivacyState } from "./chrome.js";
import { bootstrap, bootstrapWithRetry, refreshBootstrapIfSSEUnavailable, showPairing } from "./lifecycle.js";
import { refreshOpportunities, renderOpportunitiesPanel } from "./opportunities.js";
import { renderOpenOrders } from "./orders.js";
import { renderPortfolioRisk, setPortfolioExpansion } from "./portfolio.js";
import { cancelProtectionDerisk, previewProtectionDerisk, renderProtectionPanel } from "./protection.js";
import { renderSettings, setPurgeRestoreEnabled, setStockProtectionEnabled } from "./settings.js";
import { $, labelize, pct, renderFreshnessTimestamp, renderSensitiveText } from "./shared.js";
import { renderSourceBanners, renderSyncStrip, renderTopbar, setupMarketSelect } from "./shell.js";
import { state } from "./state.js";
import { renderAccountPanel, renderUnderlyings, runUnderlyingAction, setUnderlyingExpansion } from "./underlyings.js";

installSmokeHooks();

function installSmokeHooks() {
  const smoke = globalThis.__ibkrSmoke;
  if (!smoke || smoke.applySnapshotPatch) return;
  smoke.applySnapshotPatch = (patch = {}, ui = {}) => {
    const current = state.snapshot || {};
    state.snapshot = {
      ...current,
      ...patch,
      account: patch.account ? { ...(current.account || {}), ...patch.account } : current.account,
      positions: patch.positions ? {
        ...(current.positions || {}),
        ...patch.positions,
        portfolio: patch.positions.portfolio ? { ...(current.positions?.portfolio || {}), ...patch.positions.portfolio } : current.positions?.portfolio,
      } : current.positions,
      canary: patch.canary ? {
        ...(current.canary || {}),
        ...patch.canary,
        portfolio: patch.canary.portfolio ? { ...(current.canary?.portfolio || {}), ...patch.canary.portfolio } : current.canary?.portfolio,
      } : current.canary,
      proposals: patch.proposals ? { ...(current.proposals || {}), ...patch.proposals } : current.proposals,
      opportunities: patch.opportunities ? { ...(current.opportunities || {}), ...patch.opportunities } : current.opportunities,
    };
    for (const key of ["protectionOpen", "portfolioDetailOpen", "canaryDetailOpen", "opportunitiesOpen"]) {
      if (Object.prototype.hasOwnProperty.call(ui, key)) state[key] = Boolean(ui[key]);
    }
    renderAll();
    return true;
  };
}

async function main() {
  resetViewportScroll();
  setupBottomTabs();
  await navigator.serviceWorker?.register("/service-worker.js");
  const params = new URLSearchParams(location.search);
  const pair = params.get("pair");
  const nonce = params.get("nonce");
  const remote = params.get("remote");
  if (remote) {
    // The relay addresses this phone's route by an HttpOnly cookie; mirror
    // the route id so the relay's recovery page can rebuild the cookie
    // after eviction instead of forcing a re-pair.
    localStorage.setItem("ibkrRemoteRoute", remote);
  }
  let bootstrapped = false;
  if (pair && nonce) {
    try {
      await completePairing(pair, nonce);
      history.replaceState({}, "", "/");
    } catch (err) {
      history.replaceState({}, "", "/");
      showPairing("Pairing link expired; opening paired app.");
    }
  }
  if (!bootstrapped) {
    bootstrapped = await bootstrapWithRetry();
  }
  if (!bootstrapped) {
    return;
  }
  resetViewportScroll();
  setupMarketSelect();
  setupBottomTabs();
  setupLiveRefreshLoop();
}

function setupLiveRefreshLoop() {
  setInterval(() => {
    const snap = state.snapshot || {};
    renderTopbar(snap);
    renderSyncStrip(snap);
    if (state.snapshot) {
      renderAccountPanel(snap.account || {}, snap.positions || {}, snap.canary || {});
      renderUnderlyings(snap.positions || {}, snap.account || {}, snap.market_events || {});
      renderPortfolioRisk(snap.positions || {}, snap.account || {});
      renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
      renderOpportunitiesPanel(snap.opportunities || {});
    }
    refreshBootstrapIfSSEUnavailable();
  }, 1000);
}

function renderAll() {
  const snap = state.snapshot || {};
  const account = snap.account || {};
  const positions = snap.positions || {};
  const canary = snap.canary || {};
  syncAccountPrivacyState();
  ensureRegimeCanaryExpansion(canary);
  renderTopbar(snap);
  renderAccountPanel(account, positions, canary);
  renderUnderlyings(positions, account, snap.market_events || {});
  renderSensitiveText("cushion", typeof account.cushion === "number" ? pct(account.cushion * 100) : "--", typeof account.cushion === "number");
  renderFreshnessTimestamp("positionsAsOf", positions.as_of, { staleMinutes: 15, quietWhenFresh: true });
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = labelize(canary.severity || "--");
  $("canaryAction").textContent = canaryStageLabel(canary);
  // The hero clamps to 2 lines; cutting at the first clause reads cleaner
  // than a mid-word ellipsis, and the full text stays one tap away in detail.
  const canarySummaryFull = canarySummaryText(canary, snap);
  const canarySummaryEl = $("canarySummary");
  canarySummaryEl.textContent = firstClause(canarySummaryFull);
  canarySummaryEl.title = canarySummaryFull;
  renderCanaryStatus(canary);
  renderRulesCard(snap.rules);
  renderCanaryTimestamp(canary);
  renderSelectedAlert();
  renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
  renderOpportunitiesPanel(snap.opportunities || {});
  renderOpenOrders();
  renderMarketContext(snap);
  renderRegimePanel(snap);
  renderCanaryDetail(canary, snap);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
  renderSettings();
  renderTabs();
  renderSyncStrip(snap);
}

document.querySelectorAll("#alertSegments button").forEach((button) => {
  button.addEventListener("click", async () => {
    const res = await fetch("/api/alerts/settings", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ mode: button.dataset.mode }),
    });
    if (res.ok) {
      state.alertSettings = await res.json();
      renderAlertMode();
    }
  });
});

document.querySelectorAll("[data-alert-filter]").forEach((button) => {
  button.addEventListener("click", (event) => {
    event.preventDefault();
    event.stopPropagation();
    state.alertFilter = button.dataset.alertFilter || "all";
    renderAlerts();
  });
});

$("enablePushButton").addEventListener("click", enablePush);
$("retryAuthButton").addEventListener("click", bootstrap);
$("accountPrivacyToggle").addEventListener("click", () => {
  setAccountValueVisible(!state.accountValueVisible);
});
$("accountLargestExposureToggle").addEventListener("click", () => {
  state.accountExposureOpen = !state.accountExposureOpen;
  renderAccountPanel(state.snapshot?.account || {}, state.snapshot?.positions || {}, state.snapshot?.canary || {});
});
$("accountOverviewToggle").addEventListener("click", () => {
  setAccountOverviewExpansion(!state.accountOverviewOpen);
});
$("accountPanel").addEventListener("click", (event) => handleAccountPanelTap(event));
$("canaryDetailToggle").addEventListener("click", () => {
  setRegimeCanaryExpansion("canary", !state.canaryDetailOpen);
});
$("canaryRulesToggle").addEventListener("click", () => {
  state.rulesDetailOpen = !state.rulesDetailOpen;
  renderRulesCard(state.snapshot?.rules);
});
$("protectionToggle").addEventListener("click", () => {
  setProtectionExpansion(!state.protectionOpen);
});
$("protectionPanel").addEventListener("click", (event) => handleProtectionPanelTap(event));
$("opportunitiesToggle").addEventListener("click", () => {
  setOpportunitiesExpansion(!state.opportunitiesOpen);
});
$("opportunitiesPanel").addEventListener("click", (event) => handleOpportunitiesPanelTap(event));
$("opportunitiesRefreshButton").addEventListener("click", (event) => {
  event.stopPropagation();
  refreshOpportunities();
});
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
  $("regimeDetailToggle").addEventListener("click", () => {
  setRegimeCanaryExpansion("regime", !state.regimeDetailOpen);
});
$("regimeSummaryCard").addEventListener("click", (event) => {
  handleExpandablePanelTap(event, "regime");
});
$("canaryHero").addEventListener("click", (event) => {
  handleExpandablePanelTap(event, "canary");
});
$("underlyingDetailToggle").addEventListener("click", () => {
  setUnderlyingExpansion(!state.underlyingDetailOpen);
});
$("underlyingPanel").addEventListener("click", handleUnderlyingPanelTap);
$("buildAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("build", { all: true });
});
$("purgeAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("purge", { all: true });
});
$("restoreAllUnderlyingsButton").addEventListener("click", () => {
  runUnderlyingAction("restore", { all: true });
});
$("portfolioDetailToggle").addEventListener("click", () => {
  setPortfolioExpansion(!state.portfolioDetailOpen);
});
$("portfolioPanel").addEventListener("click", handlePortfolioPanelTap);
$("clearAlertsButton").addEventListener("click", clearAlerts);
$("purgeRestoreToggle").addEventListener("change", (event) => {
  setPurgeRestoreEnabled(event.currentTarget.checked);
});
$("stockProtectionToggle").addEventListener("change", (event) => {
  setStockProtectionEnabled(event.currentTarget.checked);
});
$("protectionDeriskPercent").addEventListener("change", (event) => {
  state.protectionDerisk.percent = Number(event.currentTarget.value) || 25;
  // A different percentage is a different sweep: abandon any in-flight
  // preview and rendered basket rather than letting a stale-percent result
  // land later.
  cancelProtectionDerisk();
});
$("protectionDeriskPreview").addEventListener("click", previewProtectionDerisk);
$("protectionDeriskCancel").addEventListener("click", cancelProtectionDerisk);

window.addEventListener("storage", (event) => {
  if (event.key !== "ibkrAccountValueVisible") return;
  state.accountValueVisible = event.newValue === "true";
  renderAll();
});
window.addEventListener("resize", resetViewportScroll);
window.addEventListener("orientationchange", resetViewportScroll);

main().catch((err) => {
  console.error(err);
  showPairing(err.message);
});

export { installSmokeHooks, main, renderAll, setupLiveRefreshLoop };
