const state = {
  snapshot: null,
  alertSettings: { mode: "watch_and_act" },
  alerts: [],
  vapidPublicKey: "",
  eventSource: null,
  reconnectTimer: null,
  connectionText: "Connecting",
  connectionOK: false,
  accountValueVisible: localStorage.getItem("ibkrAccountValueVisible") === "true",
  canaryDetailOpen: false,
  regimeDetailOpen: false,
  regimeCanaryExpansionInitialized: false,
  detailPreferenceSet: false,
  accountOverviewOpen: false,
  underlyingDetailOpen: false,
  portfolioDetailOpen: false,
  accountExposureOpen: false,
  protectionOpen: false,
  selectedMarket: localStorage.getItem("ibkrSelectedMarket") || "us",
  marketCalendarOverride: null,
  proposalMarketCalendars: {},
  proposalMarketCalendarBusy: {},
  selectedAlertID: null,
  alertFilter: "all",
  clearedAlertFingerprint: localStorage.getItem("ibkrClearedAlertFingerprint") || "",
  ordersOpen: null,
  openOrderEdits: {},
  protectionPreviewBusy: "",
  protectionPreviews: {},
  protectionSubmitBusy: "",
  protectionSubmits: {},
  protectionSnapshotBusy: false,
  protectionSnapshotLastAt: 0,
  protectionSnapshotNotice: "",
  underlyingNotice: "",
  underlyingBusy: "",
  latestPurgeStatus: null,
  fallbackRefreshBusy: false,
  settings: null,
  activeTab: normalizedTab(localStorage.getItem("ibkrActiveTab") || "monitor"),
};

const $ = (id) => document.getElementById(id);

async function main() {
  resetViewportScroll();
  setupBottomTabs();
  await navigator.serviceWorker?.register("/service-worker.js");
  const pair = new URLSearchParams(location.search).get("pair");
  const nonce = new URLSearchParams(location.search).get("nonce");
  if (pair && nonce) {
    try {
      await completePairing(pair, nonce);
      history.replaceState({}, "", "/");
    } catch (err) {
      showPairing("Pairing failed: " + err.message);
      return;
    }
  }
  await bootstrap();
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
    }
    refreshBootstrapIfSSEUnavailable();
  }, 1000);
}

async function bootstrap(options = {}) {
  try {
    const data = await fetchBootstrap();
    if (!data) return false;
    applyBootstrap(data);
    return true;
  } catch (err) {
    if (!options.quiet) {
      showPairing("App bootstrap failed: " + err.message);
    }
    return false;
  }
}

async function fetchBootstrap() {
  let res = await fetch("/api/bootstrap", { credentials: "include" });
  if (res.status === 401) {
    const reauthed = await tryDeviceLogin();
    if (!reauthed) {
      showPairing("Scan a fresh QR code from `ibkr app pair` on the Mac.");
      return null;
    }
    res = await fetch("/api/bootstrap", { credentials: "include" });
    if (res.status === 401) {
      showPairing("Scan a fresh QR code from `ibkr app pair` on the Mac.");
      return null;
    }
  }
  if (!res.ok) {
    throw new Error(await res.text());
  }
  return res.json();
}

function applyBootstrap(data) {
  state.snapshot = data.snapshot;
  state.settings = data.settings || data.snapshot?.settings || state.settings;
  if (state.snapshot && state.settings) state.snapshot.settings = state.settings;
  state.alertSettings = data.alert_settings || state.alertSettings;
  state.alerts = data.alerts || [];
  state.vapidPublicKey = data.vapid_public_key || "";
  $("pairingPanel").hidden = true;
  $("accountPanel").hidden = false;
  $("underlyingPanel").hidden = false;
  $("tabPanels").hidden = false;
  $("bottomTabs").hidden = false;
  $("dashboard").hidden = false;
  $("alertsPanel").hidden = false;
  setConnection("Connected", true);
  renderAll();
  connectEvents();
  refreshOpenOrders();
  refreshPurgeStatus();
  if (state.selectedMarket !== "us") {
    refreshSelectedMarketCalendar();
  }
}

async function completePairing(pairingID, nonce) {
  if (!hasWebCrypto()) {
    return completeHTTPPairing(pairingID, nonce);
  }
  showPairing("Generating a device key and proving QR possession.");
  const keys = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" },
    true,
    ["sign", "verify"]
  );
  const publicKeyJWK = await crypto.subtle.exportKey("jwk", keys.publicKey);
  const signature = await sign(keys.privateKey, nonce);
  const res = await fetch("/api/pairing/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({
      pairing_id: pairingID,
      nonce,
      device_name: navigator.userAgent.includes("iPhone") ? "iPhone" : "Browser",
      public_key_jwk: publicKeyJWK,
      signature,
    }),
  });
  if (!res.ok) {
    showPairing("Pairing failed: " + await res.text());
    throw new Error("pairing failed");
  }
  const body = await res.json();
  localStorage.setItem("ibkrDeviceID", body.device_id);
  await savePrivateKey(keys.privateKey);
  localStorage.removeItem("ibkrDeviceSecret");
}

async function completeHTTPPairing(pairingID, nonce) {
  showPairing("Completing local HTTP pairing.");
  const secret = randomDeviceSecret();
  const res = await fetch("/api/pairing/complete", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({
      pairing_id: pairingID,
      nonce,
      device_name: navigator.userAgent.includes("iPhone") ? "iPhone" : "Browser",
      device_secret: secret,
    }),
  });
  if (!res.ok) {
    showPairing("Pairing failed: " + await res.text());
    throw new Error("pairing failed");
  }
  const body = await res.json();
  localStorage.setItem("ibkrDeviceID", body.device_id);
  localStorage.setItem("ibkrDeviceSecret", secret);
}

async function tryDeviceLogin() {
  const deviceID = localStorage.getItem("ibkrDeviceID");
  const privateKey = hasWebCrypto() ? await loadPrivateKey() : null;
  const deviceSecret = localStorage.getItem("ibkrDeviceSecret") || "";
  if (!deviceID || (!privateKey && !deviceSecret)) return false;
  const ch = await fetch("/api/auth/challenge", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ device_id: deviceID }),
  });
  if (!ch.ok) return false;
  const challenge = await ch.json();
  const body = privateKey
    ? { device_id: deviceID, challenge: challenge.challenge, signature: await sign(privateKey, challenge.challenge) }
    : { device_id: deviceID, challenge: challenge.challenge, device_secret: deviceSecret };
  const session = await fetch("/api/auth/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify(body),
  });
  if (!session.ok && deviceSecret) {
    localStorage.removeItem("ibkrDeviceSecret");
  }
  return session.ok;
}

function connectEvents() {
  if (state.reconnectTimer) {
    clearTimeout(state.reconnectTimer);
    state.reconnectTimer = null;
  }
  if (typeof EventSource === "undefined") {
    setConnection("Polling", false);
    return;
  }
  state.eventSource?.close();
  const es = new EventSource("/api/events", { withCredentials: true });
  state.eventSource = es;
  for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "market_events", "market_quotes", "trading", "auto_trade", "proposals", "settings", "regime", "canary"]) {
    es.addEventListener(type, (event) => {
      const data = JSON.parse(event.data);
      if (type === "snapshot") state.snapshot = data;
      if (type !== "snapshot") state.snapshot = { ...(state.snapshot || {}), [type]: data };
      if (type === "snapshot" || type === "settings") state.settings = type === "settings" ? data : data.settings || state.settings;
      state.lastEventAt = Date.now();
      setConnection("Connected", true);
      renderAll();
      if (type === "canary") {
        setTimeout(refreshAlerts, 500);
      }
    });
  }
  es.onerror = () => scheduleEventRecovery();
}

async function refreshBootstrapIfSSEUnavailable() {
  if (!state.snapshot || state.fallbackRefreshBusy || !sseUnavailable()) return;
  state.fallbackRefreshBusy = true;
  try {
    await bootstrap({ quiet: true });
  } finally {
    state.fallbackRefreshBusy = false;
  }
}

function sseUnavailable() {
  if (!state.eventSource || !state.connectionOK) return true;
  if (typeof EventSource !== "undefined" && state.eventSource.readyState !== EventSource.OPEN) return true;
  return false;
}

function scheduleEventRecovery() {
  setConnection("Reconnecting", false);
  state.eventSource?.close();
  if (state.reconnectTimer) return;
  state.reconnectTimer = setTimeout(async () => {
    state.reconnectTimer = null;
    const recovered = await bootstrap({ quiet: true });
    if (!recovered) {
      scheduleEventRecovery();
    }
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
  renderFreshnessTimestamp("positionsAsOf", positions.as_of, { staleMinutes: 15 });
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = labelize(canary.severity || "--");
  $("canaryAction").textContent = canaryStageLabel(canary);
  $("canarySummary").textContent = canarySummaryText(canary, snap);
  renderCanaryStatus(canary);
  renderCanaryTimestamp(canary);
  renderSelectedAlert();
  renderProtectionPanel(snap.proposals || {}, snap.auto_trade || {}, snap.market_events || {});
  renderOpenOrders();
  renderMarketContext(snap);
  renderRegimePanel(snap);
  renderCanaryDetail(canary);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
  renderSettings();
  renderTabs();
  renderSyncStrip(snap);
}

function normalizedTab(tab) {
  if (tab === "alerts" || tab === "settings") return tab;
  return "monitor";
}

function setupBottomTabs() {
  const tabs = $("bottomTabs");
  if (!tabs) return;
  if (tabs.dataset.bound !== "true") {
    const activate = (event) => {
      const button = event.target.closest("[data-tab]");
      if (!button || !tabs.contains(button)) return;
      if (event.type === "pointerup" && event.pointerType === "mouse") return;
      if (event.type === "click" && Date.now() - Number(tabs.dataset.lastPointerActivation || 0) < 600) return;
      if (event.type === "pointerup") {
        tabs.dataset.lastPointerActivation = String(Date.now());
        event.preventDefault();
      }
      if (button.disabled || button.getAttribute("aria-disabled") === "true") {
        setActiveTab("monitor");
        return;
      }
      setActiveTab(button.dataset.tab || "monitor");
    };
    tabs.addEventListener("click", activate);
    tabs.addEventListener("pointerup", activate);
    tabs.dataset.bound = "true";
  }
  setActiveTab(state.activeTab, { persist: false });
}

function setActiveTab(tab, options = {}) {
  state.activeTab = normalizedTab(tab);
  if (options.persist !== false) {
    localStorage.setItem("ibkrActiveTab", state.activeTab);
  }
  renderTabs();
}

function renderTabs() {
  const active = normalizedTab(state.activeTab);
  if (active !== state.activeTab) {
    state.activeTab = active;
    localStorage.setItem("ibkrActiveTab", active);
  }
  for (const panel of document.querySelectorAll("[data-tab-panel]")) {
    panel.hidden = panel.dataset.tabPanel !== active;
  }
  const accountPanel = $("accountPanel");
  if (accountPanel) accountPanel.hidden = active === "settings";
  for (const button of document.querySelectorAll("[data-tab]")) {
    const selected = button.dataset.tab === active;
    button.classList.toggle("active", selected);
    button.setAttribute("aria-selected", String(selected));
  }
}

function setAccountValueVisible(visible) {
  state.accountValueVisible = Boolean(visible);
  localStorage.setItem("ibkrAccountValueVisible", String(state.accountValueVisible));
  renderAll();
}

function syncAccountPrivacyState() {
  document.body.dataset.accountValues = state.accountValueVisible ? "visible" : "hidden";
}

function resetViewportScroll() {
  const shell = document.querySelector(".shell");
  if (shell && (shell.scrollTop !== 0 || shell.scrollLeft !== 0)) {
    shell.scrollTo(0, 0);
  }
  if (window.scrollX !== 0 || window.scrollY !== 0) {
    window.scrollTo(0, 0);
  }
}

function currentSettings() {
  return state.settings || state.snapshot?.settings || {};
}

function purgeRestoreSettingEnabled() {
  const setting = currentSettings().features?.purge_restore?.enabled;
  return setting?.value !== false;
}

function stockProtectionSettingEnabled() {
  const setting = currentSettings().features?.stock_protection?.enabled;
  return setting?.value !== false;
}

function renderSettings() {
  const settings = currentSettings();
  if (!settings || !settings.kind) return;
  state.settings = settings;
  const purge = settings.features?.purge_restore?.enabled || {};
  const stockProtection = settings.features?.stock_protection?.enabled || {};
  renderFreshnessTimestamp("settingsAsOf", settings.as_of, { staleMinutes: 15 });
  $("purgeRestoreSettingState").textContent = purge.value === false ? "Disabled" : "Enabled";
  $("purgeRestoreSettingMeta").textContent = settingMeta(purge);
  const toggle = $("purgeRestoreToggle");
  toggle.checked = purge.value !== false;
  toggle.disabled = purge.access !== "write";
  toggle.title = purge.reason || "Runtime preference";
  $("stockProtectionSettingState").textContent = stockProtection.value === false ? "Disabled" : "Enabled";
  $("stockProtectionSettingMeta").textContent = settingMeta(stockProtection);
  const stockToggle = $("stockProtectionToggle");
  stockToggle.checked = stockProtection.value !== false;
  stockToggle.disabled = stockProtection.access !== "write";
  stockToggle.title = stockProtection.reason || "Runtime preference";

  const trading = settings.trading || {};
  const status = state.snapshot?.trading || {};
  $("settingsTradingStatus").textContent = tradingStatusSettingsLabel(trading, status);
  $("settingsTradingMeta").textContent = [trading.mode?.value, trading.account?.value].filter(Boolean).join(" / ") || "Config-owned";
  $("settingsTradingLimits").textContent = tradingLimitSummary(trading.limits || {});
  $("settingsTradingLimitsMeta").textContent = tradingLimitMeta(trading.limits || {});
  const quality = settings.market_data?.quality || {};
  $("settingsMarketDataStatus").textContent = labelize(quality.status || "unknown");
  $("settingsMarketDataMeta").textContent = quality.summary || "Observed compact summary";
  $("settingsBuildStatus").textContent = settings.build?.channel?.value || "stable";
  $("settingsBuildMeta").textContent = settings.build?.experimental_trading_note || "Build-controlled capability";
  renderProtectionSettings(settings.auto_trade || {}, state.snapshot?.auto_trade || {});
}

function settingMeta(field = {}) {
  const access = field.access || "read";
  const source = field.source || "observed";
  return field.reason ? `${access}/${source}: ${field.reason}` : `${access}/${source}`;
}

function tradingStatusSettingsLabel(trading = {}, status = {}) {
  if ((status.mode || trading.mode?.value) === "disabled") return "Disabled";
  if (status.blocked) return "Blocked";
  if (status.can_write) return "Write ready";
  if (status.can_preview) return "Preview ready";
  return "Read-only";
}

function tradingLimitSummary(limits = {}) {
  const notional = limits.max_notional?.value;
  const optionQty = limits.max_option_contracts?.value;
  const parts = [];
  if (typeof notional === "number") parts.push(money(notional, "USD"));
  if (typeof optionQty === "number") parts.push(`${optionQty} opt`);
  return parts.join(" / ") || "--";
}

function tradingLimitMeta(limits = {}) {
  const fields = [limits.max_notional, limits.max_option_contracts, limits.allow_stock_short, limits.allow_option_sell_to_open].filter(Boolean);
  const writable = fields.some((field) => field.access === "write");
  const firstReason = fields.map((field) => field.reason).find(Boolean);
  if (writable) return "Runtime overrides writable";
  return firstReason || "Config/build controlled";
}

function renderProtectionSettings(autoTrade = {}, status = {}) {
  const proposals = autoTrade.proposals_enabled || {};
  const fastPath = autoTrade.fast_path_enabled || {};
  const autoEnabled = autoTrade.enabled || {};
  const autoSubmit = autoTrade.auto_submit || {};
  const policy = status.policy || {};
  const hotReload = autoTrade.hot_reload || {};
  const cadence = autoTrade.proposal_cadence?.value || status.proposal_cadence || "";
  const reload = autoTrade.reload_interval?.value || status.reload_interval || "";
  $("settingsProtectionStatus").textContent = proposals.value === false ? "Proposals off" : "Manual proposals on";
  $("settingsProtectionMeta").textContent = [
    fastPath.value === false ? "fast path off" : "fast path on",
    autoEnabled.value ? "auto enabled" : "auto off",
    autoSubmit.value ? "submit on" : "submit off",
    cadence ? `cadence ${cadence}` : "",
  ].filter(Boolean).join(" / ") || "Config-owned";
  $("settingsPolicyStatus").textContent = policy.policy_id
    ? `${policy.policy_id} v${policy.policy_version || "--"}`
    : settingsPolicyFileLabel(autoTrade.policy_file?.value);
  $("settingsPolicyMeta").textContent = [
    policy.status ? `status ${labelize(policy.status)}` : "",
    hotReload.value === false ? "hot reload off" : "hot reload on",
    reload ? `reload ${reload}` : "",
  ].filter(Boolean).join(" / ") || settingMeta(autoTrade.policy_file || {});
}

function settingsPolicyFileLabel(value) {
  const raw = String(value || "").trim();
  if (!raw) return "Policy file";
  const normalized = raw.replaceAll("\\", "/");
  return normalized.split("/").filter(Boolean).pop() || raw;
}

async function setPurgeRestoreEnabled(enabled) {
  const previous = purgeRestoreSettingEnabled();
  state.settings = {
    ...currentSettings(),
    features: {
      ...(currentSettings().features || {}),
      purge_restore: {
        ...(currentSettings().features?.purge_restore || {}),
        enabled: {
          ...(currentSettings().features?.purge_restore?.enabled || {}),
          value: enabled,
        },
      },
    },
  };
  if (state.snapshot) state.snapshot.settings = state.settings;
  renderSettings();
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  try {
    const res = await fetch("/api/settings", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ features: { purge_restore: { enabled } } }),
    });
    if (!res.ok) throw new Error(await res.text());
    state.settings = await res.json();
    if (state.snapshot) state.snapshot.settings = state.settings;
  } catch (err) {
    state.settings = {
      ...currentSettings(),
      features: {
        ...(currentSettings().features || {}),
        purge_restore: {
          ...(currentSettings().features?.purge_restore || {}),
          enabled: {
            ...(currentSettings().features?.purge_restore?.enabled || {}),
            value: previous,
          },
        },
      },
    };
    if (state.snapshot) state.snapshot.settings = state.settings;
    state.underlyingNotice = "Settings update failed: " + err.message;
  }
  renderSettings();
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
}

async function setStockProtectionEnabled(enabled) {
  const previous = stockProtectionSettingEnabled();
  state.settings = {
    ...currentSettings(),
    features: {
      ...(currentSettings().features || {}),
      stock_protection: {
        ...(currentSettings().features?.stock_protection || {}),
        enabled: {
          ...(currentSettings().features?.stock_protection?.enabled || {}),
          value: enabled,
        },
      },
    },
  };
  if (state.snapshot) state.snapshot.settings = state.settings;
  renderSettings();
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/settings", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ features: { stock_protection: { enabled } } }),
    });
    if (!res.ok) throw new Error(await res.text());
    state.settings = await res.json();
    if (state.snapshot) state.snapshot.settings = state.settings;
  } catch (err) {
    state.settings = {
      ...currentSettings(),
      features: {
        ...(currentSettings().features || {}),
        stock_protection: {
          ...(currentSettings().features?.stock_protection || {}),
          enabled: {
            ...(currentSettings().features?.stock_protection?.enabled || {}),
            value: previous,
          },
        },
      },
    };
    if (state.snapshot) state.snapshot.settings = state.settings;
    state.underlyingNotice = "Settings update failed: " + err.message;
  }
  renderSettings();
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
}

function ensureRegimeCanaryExpansion(canary = {}) {
  if (state.detailPreferenceSet || state.regimeCanaryExpansionInitialized) return;
  state.canaryDetailOpen = false;
  state.regimeDetailOpen = false;
  state.regimeCanaryExpansionInitialized = true;
}

function setRegimeCanaryExpansion(which, open) {
  state.detailPreferenceSet = true;
  if (which === "regime") {
    state.regimeDetailOpen = open;
    if (open) state.canaryDetailOpen = false;
  } else {
    state.canaryDetailOpen = open;
    if (open) state.regimeDetailOpen = false;
  }
  renderRegimePanel(state.snapshot || {});
  renderCanaryDetail(state.snapshot?.canary || {});
}

function panelTapIgnored(target) {
  return Boolean(target?.closest?.([
    "button",
    "a",
    "input",
    "select",
    "textarea",
    "label",
    "summary",
    ".detail-panel",
    ".regime-detail-panel",
    ".underlying-book__list-panel",
    ".underlying-bulk-actions",
    ".underlying-action-result",
    ".account-overview-detail",
    ".portfolio-detail-panel",
    ".alert-focus",
  ].join(",")));
}

function handleExpandablePanelTap(event, which) {
  if (panelTapIgnored(event.target)) return;
  const open = which === "regime" ? !state.regimeDetailOpen : !state.canaryDetailOpen;
  setRegimeCanaryExpansion(which, open);
}

function handleUnderlyingPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setUnderlyingExpansion(!state.underlyingDetailOpen);
}

function handlePortfolioPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setPortfolioExpansion(!state.portfolioDetailOpen);
}

function handleProtectionPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setProtectionExpansion(!state.protectionOpen);
}

function handleAccountPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setAccountOverviewExpansion(!state.accountOverviewOpen);
}

function setAccountOverviewExpansion(open) {
  state.accountOverviewOpen = Boolean(open);
  renderAccountPanel(state.snapshot?.account || {}, state.snapshot?.positions || {}, state.snapshot?.canary || {});
}

function setProtectionExpansion(open) {
  state.protectionOpen = Boolean(open);
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {});
}

function renderAccountPanel(account = {}, positions = {}, canary = {}) {
  const detail = $("accountOverviewDetail");
  const detailToggle = $("accountOverviewToggle");
  detail.hidden = !state.accountOverviewOpen;
  detailToggle.textContent = state.accountOverviewOpen ? "Hide detail" : "Detail";
  detailToggle.setAttribute("aria-expanded", String(state.accountOverviewOpen));
  $("accountPanel").dataset.open = String(state.accountOverviewOpen);

  const hasSnapshot = Boolean(account.as_of || account.account_id || account.base_currency);
  const hasValue = hasSnapshot && hasNumericValue(account.net_liquidation);
  const accountContext = currentAccountContext(account);
  const value = $("netLiquidation");
  value.textContent = state.accountValueVisible || !hasValue
    ? compactMoney(account.net_liquidation, account.base_currency)
    : privacyMask();
  value.classList.toggle("is-private", !state.accountValueVisible && hasValue);
  renderSensitiveText("buyingPower", compactMoney(account.buying_power, account.base_currency), hasSnapshot && hasNumericValue(account.buying_power));
  renderSensitiveSignedMoney("dailyPnl", account.daily_pnl, account.base_currency);
  renderAccountDailyPnlPct(account);
  $("accountLabel").textContent = accountContext.accountLabel;
  $("tradingEnvPill").textContent = accountContext.modeLabel;
  $("tradingEnvPill").className = "trading-env-pill " + accountContext.modeClass;
  renderFreshnessTimestamp("accountAsOf", account.as_of, { staleMinutes: 15 });

  const button = $("accountPrivacyToggle");
  button.classList.toggle("is-visible", state.accountValueVisible);
  button.setAttribute("aria-pressed", String(state.accountValueVisible));
  const label = state.accountValueVisible ? "Hide account values" : "Show account values";
  button.setAttribute("aria-label", label);
  button.title = label;

  const portfolio = positions.portfolio || {};
  const baseCurrency = portfolio.base_currency || account.base_currency || "USD";
  renderSensitiveText("accountRiskDelta", riskMoney(
    portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
    portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy));
  renderSensitiveText("accountRiskTheta", riskMoney(
    portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
    portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy));
  renderSensitiveText("accountRiskFx", riskMoney(
    portfolio.fx_sensitivity_per_pct,
    portfolio.fx_base_currency || baseCurrency,
  ), hasNumericValue(portfolio.fx_sensitivity_per_pct));
  renderAccountLargestExposure(portfolio, canary, baseCurrency);
}

function renderAccountDailyPnlPct(account = {}) {
  const el = $("dailyPnlPct");
  if (!el) return;
  const value = accountDailyPnlPct(account);
  el.className = "account-pnl-pct " + signedClass(value);
  el.textContent = typeof value === "number" ? `${signedPct(value)} today` : "--";
  el.title = "Daily P/L as a percentage of estimated start-of-day net liquidation";
}

function accountDailyPnlPct(account = {}) {
  if (typeof account.daily_pnl !== "number") return null;
  const startOfDay = firstNumber(
    account.net_liquidation_start_of_day,
    account.previous_net_liquidation,
    typeof account.net_liquidation === "number" ? account.net_liquidation - account.daily_pnl : null,
  );
  const denominator = typeof startOfDay === "number" && startOfDay > 0
    ? startOfDay
    : account.net_liquidation;
  if (typeof denominator !== "number" || denominator <= 0) return null;
  return (account.daily_pnl / denominator) * 100;
}

function renderAccountLargestExposure(portfolio = {}, canary = {}, baseCurrency = "USD") {
  const panel = $("accountLargestExposurePanel");
  const button = $("accountLargestExposureToggle");
  const list = $("accountLargestExposureList");
  const exposures = (portfolio.exposure_base || []).slice(0, 5);
  const largest = exposures[0];
  const label = largest?.underlying
    ? `${largest.underlying}${typeof largest.market_value_pct_nlv === "number" ? ` ${pct(largest.market_value_pct_nlv)}` : ""}`
    : "--";
  $("accountLargestExposureLabel").textContent = label;
  panel.hidden = !state.accountExposureOpen;
  button.setAttribute("aria-expanded", String(state.accountExposureOpen));
  button.disabled = exposures.length === 0 && heldStressItems(canary).length === 0;
  button.title = button.disabled ? "No exposure rows in this snapshot" : "Show largest exposure detail";
  if (panel.hidden) return;

  const rows = exposures.map((exposure) => exposureMetricRow(exposure, baseCurrency));
  const stress = heldStressItems(canary).slice(0, 3);
  for (const item of stress) {
    rows.push(heldStressMetricRow(item));
  }
  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No exposure rows available for this snapshot.";
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...rows);
}

function exposureMetricRow(exposure, baseCurrency) {
  const row = document.createElement("div");
  row.className = "metric-row";
  const label = document.createElement("span");
  const pctText = typeof exposure.market_value_pct_nlv === "number" ? ` ${pct(exposure.market_value_pct_nlv)}` : "";
  label.textContent = `${exposure.underlying || "--"}${pctText}`;
  const value = document.createElement("b");
  value.textContent = sensitiveDisplayMoney(exposure.market_value_base, exposure.base_currency || baseCurrency);
  value.className = sensitiveMoneyHidden(exposure.market_value_base) ? "is-private" : "";
  row.append(label, value);
  return row;
}

function heldStressMetricRow(stress) {
  const row = document.createElement("div");
  row.className = "metric-row";
  const label = document.createElement("span");
  label.textContent = `${stress.underlying || "Held name"} stress`;
  const value = document.createElement("b");
  value.textContent = heldStressEvidence(stress);
  row.append(label, value);
  return row;
}

function renderUnderlyings(positions = {}, account = {}, marketEvents = {}) {
  const list = $("underlyingBookList");
  if (!list) return;

  const baseCurrency = normalizeCurrency(account.base_currency || positions.portfolio?.base_currency || "USD") || "USD";
  const rows = underlyingBookRows(positions, baseCurrency, marketEvents);
  const heldCount = rows.filter((row) => !row.virtual).length;
  const virtualCount = rows.length - heldCount;
  const count = $("underlyingBookCount");
  const status = $("underlyingBookStatus");
  const freshness = $("underlyingBookFreshness");
  const heldSymbols = rows.filter((row) => !row.virtual).slice(0, 3).map((row) => row.symbol);
  const heldLabel = heldSymbols.length > 0 ? ` · ${heldSymbols.join(", ")}${heldCount > heldSymbols.length ? ` +${heldCount - heldSymbols.length}` : ""}` : "";
  const quoteSummary = underlyingQuoteSummary(rows);
  renderUnderlyingPnlSummary(underlyingHeldDailyPnlTotals(rows, baseCurrency));
  renderMarketFlagRail("underlyingFlagRail", underlyingHeroMarketFlags(rows, marketEvents));
  if (count) {
    count.textContent = rows.length === 0
      ? "No underlyings"
      : `${heldCount} held / ${virtualCount} purged${heldLabel}`;
  }
  if (status) {
    status.textContent = state.underlyingNotice
      || quoteSummary
      || (virtualCount > 0 ? "Includes virtual purge-book records" : heldCount > 0 ? "Current held underlyings" : "Waiting for positions or purge book");
  }
  if (freshness) {
    renderFreshnessTimestamp(freshness, positions.as_of, { staleMinutes: 15 });
  }
  const panel = $("underlyingPanel");
  if (panel && (state.underlyingBusy || state.underlyingNotice)) {
    state.underlyingDetailOpen = true;
  }
  renderUnderlyingBulkActions(rows);

  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "underlying-book__empty";
    empty.textContent = "No held or virtual underlyings.";
    list.replaceChildren(empty);
    renderUnderlyingExpansion();
    return;
  }

  list.replaceChildren(...rows.map((row) => underlyingBookRow(row, baseCurrency)));
  renderUnderlyingExpansion();
}

function renderUnderlyingBulkActions(rows) {
  const heldCount = rows.filter((row) => !row.virtual).length;
  const virtualCount = rows.length - heldCount;
  const trading = state.snapshot?.trading || {};
  setUnderlyingActionButtonState("buildAllUnderlyingsButton", virtualCount > 0 && !state.underlyingBusy, virtualCount > 0 ? "Build a non-executing restore draft for all purged rows" : "No purged rows to build");
  setUnderlyingActionButtonState("purgeAllUnderlyingsButton", heldCount > 0 && canWriteUnderlyings(trading) && !state.underlyingBusy, underlyingWriteReason("Purge all held underlyings", heldCount > 0, trading));
  setUnderlyingActionButtonState("restoreAllUnderlyingsButton", virtualCount > 0 && canWriteUnderlyings(trading) && !state.underlyingBusy, underlyingWriteReason("Restore all purged rows", virtualCount > 0, trading));
}

function setUnderlyingActionButtonState(id, enabled, reason) {
  const button = $(id);
  if (!button) return;
  button.disabled = !enabled;
  button.title = enabled ? reason : reason || "Unavailable";
}

function renderUnderlyingPnlSummary(totals) {
  setUnderlyingSummaryPnl("underlyingWinnerPnl", totals.winner, totals.winnerCurrency);
  setUnderlyingSummaryPnl("underlyingLoserPnl", totals.loser, totals.loserCurrency);
}

function setUnderlyingSummaryPnl(id, value, currency) {
  const el = $(id);
  if (!el) return;
  if (!hasNumericValue(value)) {
    el.className = "signed";
    el.textContent = "--";
    return;
  }
  if (sensitiveMoneyHidden(value)) {
    el.className = "signed is-private";
    el.textContent = privacyMask();
    return;
  }
  el.className = signedClass(value);
  el.textContent = displayMoney(value, currency);
}

function underlyingHeldDailyPnlTotals(rows, baseCurrency) {
  const totals = {
    winner: null,
    winnerCurrency: "",
    loser: null,
    loserCurrency: "",
  };
  for (const row of rows) {
    if (row.virtual || typeof row.pnl !== "number" || row.pnl === 0) continue;
    if (row.pnl > 0) {
      totals.winner = (totals.winner || 0) + row.pnl;
      totals.winnerCurrency = mergeCurrency(totals.winnerCurrency, row.pnlCurrency || baseCurrency);
    } else {
      totals.loser = (totals.loser || 0) + row.pnl;
      totals.loserCurrency = mergeCurrency(totals.loserCurrency, row.pnlCurrency || baseCurrency);
    }
  }
  return {
    ...totals,
    winnerCurrency: totals.winnerCurrency || baseCurrency,
    loserCurrency: totals.loserCurrency || baseCurrency,
  };
}

function setUnderlyingExpansion(open) {
  state.underlyingDetailOpen = Boolean(open);
  renderUnderlyingExpansion();
}

function renderUnderlyingExpansion() {
  const panel = $("underlyingPanel");
  const listPanel = $("underlyingBookListPanel");
  const button = $("underlyingDetailToggle");
  if (!panel || !listPanel || !button) return;
  panel.dataset.open = String(state.underlyingDetailOpen);
  listPanel.hidden = !state.underlyingDetailOpen;
  button.textContent = state.underlyingDetailOpen ? "Hide underlyings" : "Show underlyings";
  button.setAttribute("aria-expanded", String(state.underlyingDetailOpen));
}

function canWriteUnderlyings(trading = {}) {
  return Boolean(purgeRestoreSettingEnabled() && trading.mode && trading.mode !== "disabled" && trading.can_write && trading.account);
}

function underlyingWriteReason(action, hasRows, trading = {}) {
  if (!hasRows) return "No matching underlying rows";
  if (!purgeRestoreSettingEnabled()) return "Purge/restore is disabled in Settings";
  if (!trading.mode || trading.mode === "disabled") return "Trading is disabled";
  if (!trading.can_write) return protectionWriteUnavailableReason(trading);
  if (!trading.account) return "Broker-write account unavailable";
  if (!trading.mode) return "Trading mode unavailable";
  return `${action} after confirming ${trading.mode}/${trading.account}`;
}

function underlyingBookRows(positions, baseCurrency, marketEvents = {}) {
  const rows = new Map();
  for (const row of heldUnderlyingRows(positions, baseCurrency, marketEvents)) {
    rows.set(row.symbol, row);
  }
  for (const row of purgedUnderlyingRows(positions, baseCurrency, marketEvents)) {
    const existing = rows.get(row.symbol);
    if (existing) {
      existing.hasPurgeRecord = true;
      existing.purgeLabel = row.purgeLabel;
      continue;
    }
    rows.set(row.symbol, row);
  }
  return [...rows.values()].sort(compareUnderlyingRows);
}

function compareUnderlyingRows(a, b) {
  if (a.virtual !== b.virtual) return a.virtual ? 1 : -1;
  const aPnl = underlyingSortPnl(a);
  const bPnl = underlyingSortPnl(b);
  const aRank = underlyingPnlSortRank(aPnl);
  const bRank = underlyingPnlSortRank(bPnl);
  if (aRank !== bRank) return aRank - bRank;
  if (aRank === 0) return aPnl - bPnl || a.symbol.localeCompare(b.symbol);
  if (aRank === 1) return bPnl - aPnl || a.symbol.localeCompare(b.symbol);
  return a.symbol.localeCompare(b.symbol);
}

function underlyingSortPnl(row) {
  return row.virtual ? row.pnl : row.dailyPnl;
}

function underlyingPnlSortRank(value) {
  if (typeof value !== "number" || value === 0) return 2;
  return value < 0 ? 0 : 1;
}

function heldUnderlyingRows(positions, baseCurrency, marketEvents = {}) {
  return (positions.by_underlying || []).map((group) => {
    const symbol = normalizeSymbol(group.underlying || group.stock?.symbol || group.options?.[0]?.symbol);
    if (!symbol) return null;
    const quoteState = underlyingMarketQuote(symbol);
    const quote = quoteState.quote;
    const price = heldUnderlyingPrice(group, quote);
    const currency = heldUnderlyingCurrency(group, quote, baseCurrency);
    const pnl = heldUnderlyingDailyPnl(group, baseCurrency, currency);
    const stockCount = group.stock ? 1 : 0;
    const optionCount = (group.options || []).length;
    const row = {
      symbol,
      currency,
      price: price.value,
      priceSource: price.source,
      priceAt: price.at,
      change: heldUnderlyingChange(group, quote, price.value),
      changePct: heldUnderlyingChangePct(group, quote, price.value),
      pnl: pnl.value,
      pnlCurrency: pnl.currency,
      pnlSource: pnl.source,
      dailyPnl: pnl.value,
      dailyPnlCurrency: pnl.currency,
      quote,
      quoteError: quoteState.error,
      held: true,
      virtual: false,
      purged: false,
      stockCount,
      optionCount,
      detail: underlyingPositionDetail(stockCount, optionCount),
      marketFlags: marketEventFlagsForSymbol(symbol, marketEvents),
    };
    row.quoteStatus = underlyingQuoteStatus(row);
    return row;
  }).filter(Boolean);
}

function heldUnderlyingPrice(group, quote) {
  const marketPrice = quotePrice(quote);
  if (typeof marketPrice === "number") {
    return { value: marketPrice, source: quoteSourceLabel(quote, "IBKR quote"), at: quoteTimestamp(quote) };
  }
  const stockPrice = firstNumber(group.stock?.quote_price, group.stock?.mark, group.stock?.valuation_mark);
  if (typeof stockPrice === "number") {
    const source = typeof group.stock?.quote_price === "number" ? "stock quote" : "account mark";
    return { value: stockPrice, source, at: group.stock?.quote_price_at || group.stock?.price_at || "" };
  }
  const optionUnderlying = firstNumber(...(group.options || []).map((option) => option.underlying));
  if (typeof optionUnderlying === "number") {
    return { value: optionUnderlying, source: "option model spot", at: "" };
  }
  return { value: null, source: "no price" };
}

function heldUnderlyingChangePct(group, quote, price) {
  const marketChange = quoteChangePct(quote);
  if (typeof marketChange === "number") return marketChange;
  const stockChange = firstNumber(group.stock?.quote_change_pct, group.stock?.regular_change_pct, group.stock?.day_change_pct);
  if (typeof stockChange === "number") return stockChange;
  const prevClose = firstNumber(...(group.options || []).map((option) => option.prev_close));
  if (typeof price === "number" && typeof prevClose === "number" && prevClose !== 0) {
    return (price - prevClose) / prevClose * 100;
  }
  return null;
}

function heldUnderlyingChange(group, quote, price) {
  const marketChange = quoteChange(quote);
  if (typeof marketChange === "number") return marketChange;
  const stockChange = firstNumber(group.stock?.quote_change, group.stock?.regular_change, group.stock?.day_change);
  if (typeof stockChange === "number") return stockChange;
  const prevClose = heldUnderlyingPrevClose(group, quote);
  if (typeof price === "number" && typeof prevClose === "number") {
    return price - prevClose;
  }
  return null;
}

function heldUnderlyingPrevClose(group, quote) {
  const marketPrevClose = quotePrevClose(quote);
  if (typeof marketPrevClose === "number") return marketPrevClose;
  const stockPrevClose = firstNumber(group.stock?.prev_close, group.stock?.regular_close, group.stock?.prior_regular_close);
  if (typeof stockPrevClose === "number") return stockPrevClose;
  return firstNumber(...(group.options || []).map((option) => option.prev_close));
}

function heldUnderlyingCurrency(group, quote, baseCurrency) {
  const quoteCurrency = normalizeCurrency(quote?.currency || quote?.contract?.currency);
  if (quoteCurrency) return quoteCurrency;
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  const currencies = [...new Set(rows.map((row) => normalizeCurrency(row.currency)).filter(Boolean))];
  if (currencies.length === 1) return currencies[0];
  if (currencies.length > 1) return "MIX";
  return baseCurrency;
}

function heldUnderlyingDailyPnl(group, baseCurrency, currency) {
  if (typeof group.group_daily_pnl_base === "number") {
    return { value: group.group_daily_pnl_base, currency: baseCurrency, source: "daily P/L" };
  }
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  if (rows.length > 0 && rows.every((row) => typeof row.daily_pnl_base === "number")) {
    return { value: rows.reduce((sum, row) => sum + row.daily_pnl_base, 0), currency: baseCurrency, source: "daily P/L" };
  }
  if (rows.length > 0 && rows.every((row) => typeof row.daily_pnl_ccy === "number")) {
    return { value: rows.reduce((sum, row) => sum + row.daily_pnl_ccy, 0), currency, source: "daily P/L" };
  }
  return { value: null, currency: baseCurrency, source: "daily P/L pending" };
}

function purgedUnderlyingRows(positions, baseCurrency, marketEvents = {}) {
  const rows = new Map();
  for (const entry of purgeBookEntries(positions)) {
    const symbol = normalizeSymbol(entry.underlying || entry.symbol || entry.ticker || entry.contract?.symbol);
    if (!symbol) continue;
    const quoteState = underlyingMarketQuote(symbol);
    const row = rows.get(symbol) || {
      symbol,
      currency: "",
      price: null,
      priceSource: "",
      priceAt: "",
      change: null,
      changePct: null,
      pnl: null,
      pnlCurrency: "",
      pnlSource: "shadow P/L",
      quote: quoteState.quote,
      quoteError: quoteState.error,
      virtual: true,
      purged: true,
      held: false,
      legCount: 0,
      purgeIDs: new Set(),
      detail: "",
      marketFlags: marketEventFlagsForSymbol(symbol, marketEvents),
    };
    const currency = normalizeCurrency(entry.currency || entry.trading_currency || entry.contract?.currency || entry.base_currency);
    if (currency) {
      row.currency = mergeCurrency(row.currency, currency);
    }
    if (quoteState.quote) {
      row.quote = quoteState.quote;
      const marketPrice = quotePrice(quoteState.quote);
      if (typeof marketPrice === "number") {
        row.price = marketPrice;
        row.priceSource = quoteSourceLabel(quoteState.quote, "IBKR quote");
        row.priceAt = quoteTimestamp(quoteState.quote);
      }
      const quotePct = quoteChangePct(quoteState.quote);
      if (typeof quotePct === "number") {
        row.changePct = quotePct;
      }
      const marketChange = quoteChange(quoteState.quote);
      if (typeof marketChange === "number") {
        row.change = marketChange;
      }
      const quoteCurrency = normalizeCurrency(quoteState.quote.currency || quoteState.quote.contract?.currency);
      if (quoteCurrency) {
        row.currency = mergeCurrency(row.currency, quoteCurrency);
      }
    }
    if (quoteState.error) {
      row.quoteError = quoteState.error;
    }
    const price = firstNumber(entry.current_price, entry.quote_price, entry.price, entry.last_price, entry.mark, entry.underlying, entry.reference_price);
    if (typeof price === "number" && row.price === null) {
      row.price = price;
      row.priceSource = entry.current_price_source || entry.quote_price_source || entry.price_source || "purge book";
    }
    const change = firstNumber(entry.quote_change_pct, entry.change_pct, entry.day_change_pct, entry.regular_change_pct);
    if (typeof change === "number" && row.changePct === null) {
      row.changePct = change;
    }
    const absoluteChange = firstNumber(entry.quote_change, entry.change, entry.day_change, entry.regular_change);
    if (typeof absoluteChange === "number" && row.change === null) {
      row.change = absoluteChange;
    }
    const pnl = purgeEntryPnl(entry);
    if (typeof pnl.value === "number") {
      row.pnl = (row.pnl || 0) + pnl.value;
      row.pnlCurrency = mergeCurrency(row.pnlCurrency, pnl.currency || currency || baseCurrency);
      row.pnlSource = pnl.source;
    }
    if (entry.purge_id) row.purgeIDs.add(String(entry.purge_id));
    row.legCount += Number(entry.leg_count || 1);
    rows.set(symbol, row);
  }
  return [...rows.values()].map((row) => {
    const out = {
      ...row,
      currency: row.currency || row.pnlCurrency || baseCurrency,
      pnlCurrency: row.pnlCurrency || row.currency || baseCurrency,
      priceSource: row.priceSource || "purge book",
      purgeLabel: row.purgeIDs.size > 0 ? [...row.purgeIDs].slice(0, 2).join(", ") : "purge book",
      detail: `${row.legCount} purged ${row.legCount === 1 ? "leg" : "legs"}`,
    };
    out.marketFlags = marketEventFlagsForSymbol(out.symbol, marketEvents);
    out.quoteStatus = underlyingQuoteStatus(out);
    return out;
  });
}

function underlyingMarketQuote(symbol) {
  const marketQuotes = state.snapshot?.market_quotes || {};
  return {
    quote: quoteBySymbol(marketQuotes.quotes || {}, symbol),
    error: quoteErrorBySymbol(marketQuotes.errors || {}, symbol),
    marketQuotes,
  };
}

function quoteErrorBySymbol(errors, symbol) {
  if (!errors) return "";
  const target = normalizeSymbol(symbol);
  if (!target) return "";
  for (const [key, value] of Object.entries(errors)) {
    if (normalizeSymbol(key) === target) return String(value || "");
  }
  return "";
}

function underlyingQuoteSummary(rows) {
  const quoteRows = rows.filter((row) => row.held || row.quote);
  const interrupted = quoteRows.filter((row) => row.quoteError).map((row) => row.symbol);
  if (interrupted.length > 0) {
    return `Quote feed interrupted for ${humanList(interrupted, 3)}; showing frozen values`;
  }
  const quoted = quoteRows.filter((row) => typeof quotePrice(row.quote) === "number").length;
  if (quoted > 0) {
    return `Quotes updating for ${quoted}/${quoteRows.length} rows`;
  }
  return "";
}

function underlyingQuoteStatus(row) {
  const quote = row.quote || null;
  const error = String(row.quoteError || "").trim();
  const at = quoteTimestamp(quote) || row.priceAt || "";
  const atLabel = at ? quoteTime(at) : "";
  const dataType = String(quote?.data_type || "").toLowerCase();
  const quality = String(quote?.quote_quality || "").toLowerCase();
  const hasQuotePrice = typeof quotePrice(quote) === "number";
  const source = row.priceSource || quoteSourceLabel(quote, "IBKR quote");
  const sourceDetail = [source, atLabel].filter(Boolean).join(" · ");
  const frozenLabel = atLabel ? `Frozen · ${atLabel}` : "Frozen";
  const showSource = sourceDetail || "last available value";

  if (error) {
    return {
      tone: "error",
      label: typeof row.price === "number"
        ? atLabel ? `Frozen · ${atLabel}` : "Frozen"
        : "Feed issue",
      title: `${marketQuoteErrorLabel(error)}; showing ${showSource}`,
    };
  }
  if (quote?.stale || quality === "stale" || quality === "missing") {
    return {
      tone: "warn",
      label: atLabel ? `Stale · ${atLabel}` : "Stale",
      title: `${cleanDetail(quote.stale_reason || quality || "stale quote")}; showing ${showSource}`,
    };
  }
  if (dataType.includes("frozen")) {
    return {
      tone: "warn",
      label: frozenLabel,
      title: `Gateway is in ${labelize(dataType)} mode; showing ${showSource}`,
    };
  }
  if (dataType.includes("delayed")) {
    return {
      tone: "warn",
      label: atLabel ? `Delayed · ${atLabel}` : "Delayed",
      title: `Delayed market-data feed; showing ${showSource}`,
    };
  }
  if (quality && quality !== "firm") {
    return {
      tone: "warn",
      label: atLabel ? `${labelize(quality)} · ${atLabel}` : labelize(quality),
      title: `Quote quality ${labelize(quality)}; showing ${showSource}`,
    };
  }
  if (quote && hasQuotePrice) {
    return {
      tone: "ok",
      label: atLabel ? `Live · ${atLabel}` : "Live",
      title: `IBKR quote feed; showing ${showSource}`,
    };
  }
  if (typeof row.price === "number") {
    return {
      tone: "fallback",
      label: cleanDetail(source || "Position mark"),
      title: quote ? "Underlying quote has no current price yet; showing the latest position mark." : "No live underlying quote yet; showing the latest position mark.",
    };
  }
  return {
    tone: "error",
    label: "No price",
    title: "No quote or position mark is available for this underlying.",
  };
}

function quoteTimestamp(quote) {
  return quote?.quote_price_at || quote?.price_at || quote?.as_of || "";
}

function purgeBookEntries(positions = {}) {
  const out = [];
  const candidates = [
    state.snapshot?.purge_book,
    state.snapshot?.purge_books,
    state.snapshot?.purged_underlyings,
    state.snapshot?.purged_positions,
    state.latestPurgeStatus,
    positions.purge_book,
    positions.purge_books,
    positions.purged_underlyings,
    positions.purged_positions,
    readLocalPurgeBook(),
  ];
  for (const candidate of candidates) {
    collectPurgeEntries(candidate, out, {});
  }
  return out;
}

function collectPurgeEntries(candidate, out, context) {
  if (!candidate) return;
  if (Array.isArray(candidate)) {
    candidate.forEach((item) => collectPurgeEntries(item, out, context));
    return;
  }
  if (typeof candidate !== "object") return;

  const next = {
    purge_id: candidate.purge_id || context.purge_id,
    base_currency: candidate.base_currency || context.base_currency,
  };
  for (const key of ["books", "underlyings", "positions", "rows"]) {
    if (Array.isArray(candidate[key])) {
      candidate[key].forEach((item) => collectPurgeEntries(item, out, next));
    }
  }
  if (Array.isArray(candidate.legs)) {
    candidate.legs.forEach((leg) => out.push({ ...leg, ...next }));
  }
  if (candidate.symbol || candidate.underlying || candidate.ticker || candidate.contract?.symbol) {
    out.push({ ...candidate, ...next });
  }
}

function readLocalPurgeBook() {
  for (const key of ["ibkrPurgeBook", "ibkrPurgeBooks"]) {
    const raw = localStorage.getItem(key);
    if (!raw) continue;
    try {
      return JSON.parse(raw);
    } catch {
      continue;
    }
  }
  return null;
}

function purgeEntryPnl(entry) {
  const direct = firstNumber(entry.current_shadow_pnl, entry.shadow_pnl, entry.group_unrealized_pnl_base, entry.unrealized_pnl_base, entry.pnl_base, entry.pnl, entry.shadow_saved);
  const currency = normalizeCurrency(entry.pnl_currency || entry.base_currency || entry.currency || entry.contract?.currency);
  if (typeof direct === "number") {
    const shadow = typeof entry.current_shadow_pnl === "number" || typeof entry.shadow_pnl === "number" || typeof entry.shadow_saved === "number";
    return { value: direct, currency, source: shadow ? "shadow P/L" : "unrealized P/L" };
  }
  const restore = firstNumber(entry.current_restore_value, entry.estimated_value);
  const exit = firstNumber(entry.exit_value);
  if (typeof restore === "number" && typeof exit === "number") {
    return { value: exit - restore, currency, source: "shadow P/L" };
  }
  return { value: null, currency, source: "no P/L" };
}

function underlyingBookRow(row, baseCurrency) {
  const item = document.createElement("div");
  item.className = "underlying-row" + (row.virtual ? " underlying-row--virtual" : "") + (row.hasPurgeRecord ? " underlying-row--book" : "");
  if (row.quoteError) item.classList.add("underlying-row--quote-error");
  item.dataset.symbol = row.symbol;

  const identity = document.createElement("div");
  identity.className = "underlying-row__identity";
  const title = document.createElement("div");
  title.className = "underlying-row__title";
  const symbol = document.createElement("strong");
  symbol.textContent = row.symbol;
  title.append(symbol, ...underlyingMarkers(row));
  const detail = document.createElement("small");
  detail.textContent = row.detail;
  identity.append(title, detail);
  const flagRow = marketFlagRow(row.marketFlags || []);
  if (flagRow) identity.append(flagRow);

  const price = document.createElement("div");
  const quoteStatus = row.quoteStatus || underlyingQuoteStatus(row);
  price.className = "underlying-row__metric underlying-row__metric--quote quote-" + quoteStatus.tone;
  const priceValue = document.createElement("b");
  priceValue.textContent = displayMoney(row.price, row.currency);
  const priceNote = document.createElement("small");
  priceNote.className = "underlying-quote-status " + quoteStatus.tone;
  priceNote.textContent = quoteStatus.label;
  priceNote.title = quoteStatus.title;
  price.append(priceValue, priceNote);

  const change = document.createElement("div");
  change.className = "underlying-row__metric underlying-row__metric--change";
  const changeValue = document.createElement("b");
  const changeTone = typeof row.change === "number" ? row.change : row.changePct;
  changeValue.className = signedClass(changeTone);
  changeValue.textContent = signedDisplayMoney(row.change, row.currency);
  const changeNote = document.createElement("small");
  changeNote.className = signedClass(row.changePct);
  changeNote.textContent = typeof row.changePct === "number" ? `${signedPct(row.changePct)} day` : "% change";
  change.append(changeValue, changeNote);

  const pnl = document.createElement("div");
  pnl.className = "underlying-row__metric underlying-row__metric--pnl";
  const pnlValue = document.createElement("b");
  pnlValue.className = sensitiveMoneyHidden(row.pnl) ? "is-private" : signedClass(row.pnl);
  pnlValue.textContent = sensitiveDisplayMoney(row.pnl, row.pnlCurrency || baseCurrency);
  const pnlNote = document.createElement("small");
  pnlNote.textContent = row.pnlSource || "P/L";
  pnl.append(pnlValue, pnlNote);

  const actions = document.createElement("div");
  actions.className = "underlying-row__actions";
  actions.append(
    underlyingActionButton("Purge", !row.virtual, row, "purge"),
    underlyingActionButton("Restore", row.virtual, row, "restore"),
    underlyingActionButton("Build", row.virtual, row, "build"),
  );

  item.append(identity, price, change, pnl, actions);
  return item;
}

function underlyingMarkers(row) {
  const markers = [];
  if (row.virtual) {
    markers.push(underlyingMarker("Virtual", "virtual"));
    markers.push(underlyingMarker("Purged", "purged"));
  } else if (row.hasPurgeRecord) {
    markers.push(underlyingMarker("Book", "book"));
  }
  return markers;
}

function underlyingMarker(label, tone) {
  const marker = document.createElement("span");
  marker.className = "underlying-marker underlying-marker--" + tone;
  marker.textContent = label;
  return marker;
}

function renderMarketFlagRail(id, items) {
  const rail = $(id);
  if (!rail) return;
  const chips = (items || []).map((item) => item.sourceHealth ? marketSourceHealthChip(item.sourceHealth) : marketFlagChip(item.flag, item.options || {})).filter(Boolean);
  rail.hidden = chips.length === 0;
  rail.replaceChildren(...chips);
}

function marketFlagRow(flags) {
  const active = (flags || []).filter(marketEventFlagVisible);
  if (active.length === 0) return null;
  const row = document.createElement("div");
  row.className = "market-flag-row";
  row.replaceChildren(...active.map((flag) => marketFlagChip(flag, { compact: true })));
  return row;
}

function marketFlagChip(flag = {}, options = {}) {
  if (!flag || !flag.id) return null;
  const chip = document.createElement("span");
  chip.className = `market-flag-chip market-flag-chip--${marketEventTone(flag)}`;
  chip.textContent = options.label || marketEventLabel(flag, options);
  chip.title = marketEventTitle(flag);
  return chip;
}

function marketSourceHealthChip(health = {}) {
  if (!marketEventHealthVisible(health)) return null;
  const chip = document.createElement("span");
  chip.className = "market-flag-chip market-flag-chip--muted";
  chip.textContent = `${marketEventSourceLabel(health.source)} ${labelize(health.status || "unknown")}`;
  chip.title = [
    health.source,
    health.as_of ? `as of ${shortTimeWithZone(health.as_of)}` : "",
    ...(health.notes || []),
  ].filter(Boolean).join(" · ");
  return chip;
}

function marketEventFlagsForSymbol(symbol, events = {}) {
  const target = normalizeSymbol(symbol);
  if (!target) return [];
  const bySymbol = events.by_symbol || {};
  for (const [key, flags] of Object.entries(bySymbol)) {
    if (normalizeSymbol(key) === target) {
      return (flags || []).filter(marketEventFlagVisible);
    }
  }
  return [];
}

function marketEventFlagVisible(flag = {}) {
  const status = String(flag.status || "").toLowerCase();
  return status === "active" || status === "recent" || status === "stale" || status === "unknown" || status === "degraded";
}

function protectionEffectiveMarketFlags(proposal = {}, events = {}) {
  const out = [];
  const seen = new Set();
  const add = (flag = {}) => {
    if (!marketEventFlagVisible(flag)) return;
    const key = `${flag.id || ""}|${flag.symbol || ""}|${flag.status || ""}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push(flag);
  };
  for (const flag of proposal.market_flags || []) add(flag);
  for (const flag of marketEventFlagsForSymbol(proposal.symbol || proposal.contract?.symbol, events)) add(flag);
  return out;
}

function protectionEffectiveBlockers(proposal = {}, events = {}) {
  const blockers = [...(proposal.blockers || [])];
  const snapshotBlocker = proposalSnapshotBlocker();
  if (snapshotBlocker) blockers.unshift(snapshotBlocker);
  const eventBlocker = protectionMarketEventBlocker(proposal, events);
  if (eventBlocker) blockers.unshift(eventBlocker);
  return blockers;
}

function proposalSnapshotBlocker() {
  return (state.snapshot?.proposals?.blockers || [])[0] || null;
}

function protectionMarketEventBlocker(proposal = {}, events = {}) {
  for (const flag of protectionEffectiveMarketFlags(proposal, events)) {
    const id = String(flag.id || "");
    const status = String(flag.status || "").toLowerCase();
    if (status !== "active") continue;
    if (id === "halt_regulatory_or_news" || id === "luld_pause" || flag.role === "hard_blocker" || flag.severity === "block") {
      return {
        code: `market_event_${id || "blocker"}`,
        message: `${flag.label || marketEventIDLabel(id)} is active for ${flag.symbol || proposal.symbol || "this symbol"}; refresh proposals after it clears`,
      };
    }
  }
  return null;
}

function marketEventHealthVisible(health = {}) {
  const status = String(health.status || "").toLowerCase();
  return status === "unknown" || status === "stale" || status === "degraded" || status === "partial" || status === "error" || status === "unavailable";
}

function underlyingHeroMarketFlags(rows, events = {}) {
  const heldSymbols = new Set(rows.filter((row) => !row.virtual).map((row) => row.symbol));
  const counts = new Map();
  for (const row of rows) {
    if (row.virtual || !heldSymbols.has(row.symbol)) continue;
    for (const flag of row.marketFlags || []) {
      if (!marketEventFlagVisible(flag)) continue;
      const key = flag.id;
      const existing = counts.get(key) || { flag, count: 0 };
      existing.count += 1;
      counts.set(key, existing);
    }
  }
  const items = [...counts.values()].map(({ flag, count }) => ({
    flag,
    options: { label: `${flag.label || marketEventIDLabel(flag.id)} ${count}` },
  }));
  if (items.length > 0) return items;
  return marketEventHealthItems(events);
}

function marketEventHealthItems(events = {}) {
  const includeBorrow = bookHasShortStock();
  return (events.source_health || [])
    .filter(marketEventHealthVisible)
    .filter((health) => includeBorrow || !borrowSourceHealth(health))
    .map((sourceHealth) => ({ sourceHealth }));
}

// Borrow-inventory / borrow-fee feed health only changes a decision when
// the book can be forced to cover — i.e. it holds short stock (the only
// daemon consumer is buy-to-cover proposal friction). For an all-long
// book a permanently unreachable borrow feed is noise, not risk
// disclosure, so those health chips stay hidden until a short stock
// position exists. Active borrow flags on held names still render.
function borrowSourceHealth(health = {}) {
  const source = String(health.source || "").toLowerCase();
  return source.includes("borrow_inventory") || source.includes("borrow_fee");
}

function bookHasShortStock() {
  const groups = state.snapshot?.positions?.by_underlying || [];
  return groups.some((group) => typeof group.stock?.quantity === "number" && group.stock.quantity < 0);
}

function marketEventLabel(flag = {}, options = {}) {
  const base = flag.label || marketEventIDLabel(flag.id);
  if (options.compact) return base;
  return base;
}

function marketEventIDLabel(id = "") {
  switch (id) {
    case "borrow_inventory_tight": return "Borrow tight";
    case "borrow_fee_extreme": return "Fee extreme";
    case "reg_sho_threshold": return "Reg SHO";
    case "luld_pause":
    case "luld_pause_recent": return "LULD";
    case "halt_regulatory_or_news": return "Halt";
    default: return labelize(id || "flag");
  }
}

function marketEventTone(flag = {}) {
  const status = String(flag.status || "").toLowerCase();
  if (status === "unknown" || status === "stale" || status === "degraded") return "muted";
  const severity = String(flag.severity || "").toLowerCase();
  if (severity === "block") return "hard";
  if (severity === "act" || severity === "watch") return "friction";
  if (severity === "context") return "context";
  return "muted";
}

function marketEventTitle(flag = {}) {
  return [
    flag.symbol,
    flag.status ? labelize(flag.status) : "",
    flag.source || "",
    flag.as_of ? `as of ${shortTimeWithZone(flag.as_of)}` : "",
    ...(flag.details || []),
  ].filter(Boolean).join(" · ");
}

function marketEventSourceLabel(source = "") {
  const normalized = String(source || "").toLowerCase();
  if (normalized.includes("borrow_inventory")) return "Borrow";
  if (normalized.includes("borrow_fee")) return "Fee";
  if (normalized.includes("reg_sho")) return "Reg SHO";
  if (normalized.includes("halt")) return "Halts";
  if (normalized.includes("market_events")) return "Flags";
  return labelize(source || "Source");
}

function underlyingActionButton(label, enabled, row, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "underlying-action underlying-action--" + action;
  button.textContent = label;
  const trading = state.snapshot?.trading || {};
  const writeAction = action === "purge" || action === "restore";
  const available = enabled && !state.underlyingBusy && (!writeAction || canWriteUnderlyings(trading));
  button.disabled = !available;
  const disabledReason = row.virtual
    ? "Already in the purge book; restore or build is available."
    : "Available after this underlying has been purged.";
  button.title = available
    ? underlyingActionTitle(label, row, action)
    : writeAction ? underlyingWriteReason(`${label} ${row.symbol}`, enabled, trading) : disabledReason;
  button.setAttribute("aria-label", `${label} ${row.symbol}`);
  if (available) {
    button.addEventListener("click", () => {
      runUnderlyingAction(action, { symbols: [row.symbol] });
    });
  }
  return button;
}

function underlyingActionTitle(label, row, action) {
  if (action === "build") return `Build a non-executing restore draft for ${row.symbol}`;
  return `${label} ${row.symbol} after account/mode confirmation`;
}

function quoteSourceLabel(quote, fallback) {
  const dataType = String(quote?.data_type || "").trim();
  if (!dataType || dataType === "live") return fallback;
  return labelize(dataType) + " quote";
}

function underlyingPositionDetail(stockCount, optionCount) {
  const parts = [];
  if (stockCount > 0) parts.push(`${stockCount} stock ${stockCount === 1 ? "leg" : "legs"}`);
  if (optionCount > 0) parts.push(`${optionCount} option ${optionCount === 1 ? "leg" : "legs"}`);
  return parts.length ? parts.join(" / ") : "Held position";
}

function normalizeSymbol(value) {
  return String(value || "").trim().toUpperCase();
}

function normalizeCurrency(value) {
  return String(value || "").trim().toUpperCase();
}

function mergeCurrency(left, right) {
  const a = normalizeCurrency(left);
  const b = normalizeCurrency(right);
  if (!a) return b;
  if (!b || a === b) return a;
  return "MIX";
}

function displayMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const ccy = normalizeCurrency(currency);
  if (/^[A-Z]{3}$/.test(ccy) && ccy !== "MIX") {
    return money(value, ccy);
  }
  const amount = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
  return ccy ? `${amount} ${ccy}` : amount;
}

function signedDisplayMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const sign = value > 0 ? "+" : value < 0 ? "-" : "";
  return sign + displayMoney(Math.abs(value), currency);
}

function currentAccountContext(account = {}) {
  const trading = state.snapshot?.trading || {};
  const status = state.snapshot?.status || {};
  const rawTradingAccount = String(trading.account || "").trim();
  const rawAccount = String(account.account_id || "").trim();
  const rawPositionsAccount = String(state.snapshot?.positions?.account_id || "").trim();
  const rawStatusAccount = String(status.connected_account || status.account || "").trim();
  const concreteTradingAccount = rawTradingAccount && rawTradingAccount.toLowerCase() !== "all" ? rawTradingAccount : "";
  const concreteAccount = rawAccount && rawAccount.toLowerCase() !== "all" ? rawAccount : "";
  const concretePositionsAccount = rawPositionsAccount && rawPositionsAccount.toLowerCase() !== "all" ? rawPositionsAccount : "";
  const concreteStatusAccount = rawStatusAccount && rawStatusAccount.toLowerCase() !== "all" ? rawStatusAccount : "";
  const accountLabel = concreteTradingAccount || concreteAccount || concretePositionsAccount || concreteStatusAccount || "";
  const modeSource = [
    status.account_mode,
    account.account_mode,
    account.mode,
    account.environment,
    trading.mode,
    status.trading?.mode,
  ].map((value) => String(value || "").trim()).find((value) => /paper|live/i.test(value));
  const modeLabel = modeSource
    ? modeSource.toLowerCase().includes("paper") ? "Paper" : "Live"
    : "IBKR";
  const aggregate = rawTradingAccount.toLowerCase() === "all" ||
    rawAccount.toLowerCase() === "all" ||
    rawPositionsAccount.toLowerCase() === "all" ||
    rawStatusAccount.toLowerCase() === "all";
  const visibleAccountLabel = accountLabel || (aggregate ? "Aggregate account" : "Account pending");
  return {
    accountLabel: visibleAccountLabel,
    modeClass: String(modeLabel).toLowerCase().includes("paper") ? "paper" : String(modeLabel).toLowerCase().includes("live") ? "live" : "neutral",
    modeLabel,
    hasAccount: Boolean(accountLabel || aggregate),
  };
}

function renderCanaryDetail(canary) {
  const panel = $("canaryDetailPanel");
  const button = $("canaryDetailToggle");
  panel.hidden = !state.canaryDetailOpen;
  button.textContent = state.canaryDetailOpen ? "Hide detail" : "Show detail";
  button.setAttribute("aria-expanded", String(state.canaryDetailOpen));
  if (!state.canaryDetailOpen) return;

  $("canaryDetailGrid").replaceChildren(...canaryExplanationCards(canary).map(detailCard));
  renderHeldStress(canary);

  const rows = (canary.rows || []).slice(0, 3);
  $("canaryDrivers").replaceChildren(...rows.map((row) => {
    const item = document.createElement("div");
    item.className = "driver-row";
    const label = document.createElement("span");
    label.textContent = row.severity ? String(row.severity).replaceAll("_", " ") : "driver";
    const title = document.createElement("b");
    title.textContent = row.title || "Canary driver";
    const body = document.createElement("p");
    body.textContent = [row.guidance, row.evidence].filter(Boolean).join(" ") || "No extra detail for this driver.";
    item.append(label, title, body);
    return item;
  }));
}

function renderProtectionPanel(proposals = {}, autoTrade = {}, marketEvents = {}) {
  const panel = $("protectionPanel");
  const detail = $("protectionDetailPanel");
  const toggle = $("protectionToggle");
  const rows = proposals.proposals || [];
  const counts = proposals.counts || {};
  panel.dataset.open = String(state.protectionOpen);
  detail.hidden = !state.protectionOpen;
  toggle.textContent = state.protectionOpen ? "Hide proposals" : "Show proposals";
  toggle.setAttribute("aria-expanded", String(state.protectionOpen));
  renderProtectionTimestamp(proposals);
  $("protectionTheta").textContent = typeof counts.theta_per_day === "number" ? money(counts.theta_per_day, "") : "--";
  $("protectionRiskExcess").textContent = typeof counts.risk_reduction_excess_notional === "number"
    ? money(counts.risk_reduction_excess_notional, protectionRiskExcessCurrency(counts))
    : "--";
  $("protectionActions").textContent = String(counts.actionable ?? rows.length ?? 0);
  renderMarketFlagRail("protectionFlagRail", protectionHeroMarketFlags(rows, marketEvents));
  const autoButton = $("protectionAutoButton");
  autoButton.disabled = true;
  autoButton.title = autoTrade.auto_submit ? "Autonomous submit is not available in MVP" : "Manual confirmation required";
  const reason = protectionReason(proposals, autoTrade);
  const reasonEl = $("protectionReason");
  const refreshReason = protectionSnapshotRefreshReason();
  const reasonText = [reason, refreshReason].filter(Boolean).join(" · ");
  reasonEl.textContent = reasonText;
  reasonEl.hidden = !reasonText;
  if (!state.protectionOpen) return;
  $("protectionRows").replaceChildren(...rows.map(protectionRow));
  if (protectionNeedsSnapshotSync(proposals, autoTrade)) {
    queueProtectionSnapshotSync();
  }
}

function renderProtectionTimestamp(proposals = {}) {
  // Proposal refresh cadence is 15m daemon-side ([auto_trade].proposal_cadence);
  // a 15m badge threshold brushed "stale" in healthy operation, so allow one
  // full cycle plus grace before flagging.
  renderFreshnessTimestamp("protectionAsOf", proposals.as_of, { staleMinutes: 20 });
}

function protectionReason(proposals = {}, autoTrade = {}) {
  const blocker = (proposals.blockers || autoTrade.blockers || [])[0];
  if (blocker) return `${blocker.code}: ${blocker.message}`;
  if (autoTrade.policy?.status && autoTrade.policy.status !== "active" && autoTrade.policy.status !== "default") {
    return `Policy ${autoTrade.policy.status}`;
  }
  return autoTrade.fast_path_enabled === false ? "Fast path disabled" : "";
}

function protectionSnapshotRefreshReason() {
  if (state.protectionSnapshotBusy) return "Refreshing proposals";
  return state.protectionSnapshotNotice || "";
}

function protectionNeedsSnapshotSync(proposals = {}, autoTrade = {}) {
  if (!state.protectionOpen || state.protectionSnapshotBusy) return false;
  if (autoTrade.proposals_enabled === false) return false;
  const policyStatus = String(proposals.policy_status?.status || autoTrade.policy?.status || "").toLowerCase();
  if (policyStatus === "disabled") return false;
  const revision = String(proposals.revision || "");
  const blockerCodes = [
    ...(proposals.blockers || []),
    ...(autoTrade.blockers || []),
  ].map((blocker) => String(blocker.code || ""));
  if (blockerCodes.some(protectionTransientSnapshotBlocker)) return true;
  if ((proposals.proposals || []).length > 0) return false;
  return !revision || revision === "empty";
}

function protectionTransientSnapshotBlocker(code = "") {
  return [
    "account_unavailable",
    "positions_unavailable",
    "trading_status_unavailable",
    "market_events_unavailable",
  ].includes(code);
}

function queueProtectionSnapshotSync() {
  const now = Date.now();
  if (state.protectionSnapshotBusy || now - state.protectionSnapshotLastAt < 10000) return;
  state.protectionSnapshotBusy = true;
  state.protectionSnapshotLastAt = now;
  state.protectionSnapshotNotice = "";
  setTimeout(() => {
    syncProtectionSnapshot();
  }, 0);
}

async function syncProtectionSnapshot() {
  try {
    const res = await fetch("/api/proposals", { credentials: "include", cache: "no-store" });
    if (!res.ok) throw new Error(await res.text());
    const proposals = await res.json();
    applyProtectionSnapshot(proposals);
    const proposalCount = proposals.counts?.total ?? (proposals.proposals || []).length;
    state.protectionSnapshotNotice = proposalCount > 0 ? "" : "No protection proposals available yet";
  } catch (err) {
    state.protectionSnapshotNotice = "Proposal refresh failed: " + shortPreviewMessage(err.message);
  } finally {
    state.protectionSnapshotBusy = false;
    renderAll();
  }
}

function protectionRow(proposal) {
  const row = document.createElement("div");
  row.className = "protection-row";
  const marketEvents = state.snapshot?.market_events || {};
  const effectiveBlockers = protectionEffectiveBlockers(proposal, marketEvents);
  const blocked = effectiveBlockers.length > 0;
  const previewFlow = protectionUsesPreviewFlow(proposal);
  const tradability = previewFlow ? protectionPreviewGate(proposal) : protectionSubmitGate(proposal);
  const previewKey = protectionPreviewStateKey(proposal);
  const previewBusy = state.protectionPreviewBusy === previewKey;
  const previewResult = state.protectionPreviews[previewKey] || null;
  const finalSubmitGate = previewFlow ? protectionPreviewSubmitGate(proposal, previewResult) : null;
  const submitBusy = state.protectionSubmitBusy === previewKey;
  const submitResult = state.protectionSubmits[previewKey] || null;
  const copy = document.createElement("div");
  copy.className = "protection-row__copy";
  const bucket = document.createElement("span");
  bucket.className = "protection-row__bucket";
  bucket.textContent = protectionBucketLabel(proposal);
  const title = document.createElement("b");
  title.className = "protection-row__title";
  title.textContent = protectionProposalTitle(proposal);
  copy.append(bucket, title);
  const trailText = protectionTrailText(proposal);
  if (trailText) {
    const trail = document.createElement("small");
    trail.className = "protection-row__trail";
    trail.textContent = trailText;
    copy.append(trail);
  }
  const blockerText = blocked ? protectionBlockerText({ ...proposal, blockers: effectiveBlockers }) : "";
  if (blockerText) {
    const blocker = document.createElement("small");
    blocker.className = "protection-row__blocker";
    blocker.textContent = blockerText;
    copy.append(blocker);
  }
  const previewText = protectionPreviewText(previewResult, proposal);
  if (previewText) {
    const preview = document.createElement("small");
    preview.className = "protection-row__preview";
    preview.textContent = previewText;
    copy.append(preview);
  }
  const submitStateText = previewFlow ? protectionSubmitStateText({
    result: submitResult,
    gate: finalSubmitGate,
    busy: submitBusy,
    previewResult,
    proposal,
  }) : "";
  if (submitStateText) {
    const submitState = document.createElement("small");
    submitState.className = protectionSubmitStateClass({ result: submitResult, gate: finalSubmitGate, busy: submitBusy });
    submitState.textContent = submitStateText;
    copy.append(submitState);
  }
  const reason = document.createElement("small");
  reason.className = "protection-row__reason";
  reason.textContent = protectionReasonText(proposal);
  copy.append(reason);
  const flagRow = marketFlagRow(protectionEffectiveMarketFlags(proposal, marketEvents));
  if (flagRow) copy.append(flagRow);
  const actions = document.createElement("div");
  actions.className = "protection-row__actions";
  const primary = document.createElement("button");
  primary.type = "button";
  primary.className = previewFlow ? "protection-preview" : proposal.action === "BUY" ? "protection-buy" : "protection-sell";
  primary.textContent = previewBusy ? "Previewing" : protectionSubmitLabel(proposal);
  primary.disabled = blocked || previewBusy || submitBusy || !tradability.ready;
  primary.title = protectionButtonTitle(proposal, { blocked, previewBusy, tradability });
  primary.addEventListener("click", () => {
    if (previewFlow) {
      previewProtectionProposal(proposal);
      return;
    }
    submitProtectionProposal(proposal);
  });
  actions.append(primary);
  if (previewFlow && (submitResult || submitBusy || (previewResult && !previewResult.pending))) {
    const finalSubmit = document.createElement("button");
    finalSubmit.type = "button";
    finalSubmit.className = "protection-submit";
    finalSubmit.textContent = submitBusy ? "Submitting" : "Submit stop";
    finalSubmit.disabled = blocked || previewBusy || submitBusy || !finalSubmitGate.ready;
    finalSubmit.title = protectionSubmitButtonTitle({ blocked, previewBusy, submitBusy, gate: finalSubmitGate });
    finalSubmit.addEventListener("click", () => submitProtectionProposal(proposal));
    actions.append(finalSubmit);
  }
  const ignore = document.createElement("button");
  ignore.type = "button";
  ignore.className = "protection-ignore";
  ignore.textContent = "Ignore";
  ignore.title = "Ignore this proposal; no market order is sent";
  ignore.addEventListener("click", () => ignoreProtectionProposal(proposal));
  actions.append(ignore);
  row.append(copy, actions);
  return row;
}

function protectionProposalTitle(proposal = {}) {
  return [
    protectionSideLabel(proposal),
    proposal.quantity || 0,
    proposal.symbol || "--",
    protectionContractLabel(proposal.contract || {}),
  ].filter(Boolean).join(" ");
}

function protectionSubmitLabel(proposal = {}) {
  if (proposal.bucket === "trailing_stop") return "Preview stop";
  return protectionActionLabel(proposal);
}

function protectionUsesPreviewFlow(proposal = {}) {
  return proposal.bucket === "trailing_stop";
}

function protectionButtonTitle(proposal = {}, gate = {}) {
  if (gate.blocked) return protectionBlockerText(proposal);
  if (gate.previewBusy) return "Broker WhatIf preview is running; no order has been placed";
  if (!gate.tradability?.ready) return gate.tradability?.reason || "Protection action is unavailable";
  return protectionActionTitle(proposal, gate.tradability.reason);
}

function protectionSideLabel(proposal = {}) {
  if (proposal.bucket !== "trailing_stop") return protectionActionLabel(proposal);
  if (proposalIsBuyToCover(proposal)) return "Buy to cover stop";
  return String(proposal.action || "--").toUpperCase() === "BUY" ? "Buy stop" : "Sell stop";
}

function protectionBucketLabel(proposal = {}) {
  if (proposal.bucket === "trailing_stop") return "Broker stop";
  return labelize(proposal.bucket || "--");
}

function protectionActionLabel(proposal = {}) {
  if (proposalIsBuyToCover(proposal)) return "Buy to cover";
  return String(proposal.action || "--").toUpperCase() === "BUY" ? "Buy" : "Sell";
}

function protectionActionTitle(proposal = {}, fallback = "") {
  if (proposal.bucket === "trailing_stop" && String(proposal.action || "").toUpperCase() === "SELL") {
    return "Preview a broker trailing stop sell order. Once submitted, IBKR maintains the stop and raises it as the instrument price rises above the submission reference.";
  }
  if (proposal.bucket === "trailing_stop" && String(proposal.action || "").toUpperCase() === "BUY") {
    return "Preview a broker trailing stop buy-to-close order. Once submitted, IBKR maintains the stop as the instrument price moves in favor of the short position.";
  }
  return fallback || "Preview this protection proposal";
}

function protectionReasonText(proposal = {}) {
  if (proposal.bucket === "trailing_stop") {
    const policy = protectionTrailPolicyLabel(proposal);
    if (String(proposal.action || "").toUpperCase() === "SELL") {
      return policy
        ? `Broker-managed stop; IBKR lifts it as price rises above submission reference (policy ${policy})`
        : "Broker-managed stop; IBKR lifts it as price rises above submission reference";
    }
    return policy
      ? `Broker-managed stop; IBKR adjusts it as price moves in favor (policy ${policy})`
      : "Broker-managed stop; IBKR adjusts it as price moves in favor";
  }
  return proposal.reason || "";
}

function protectionTrailText(proposal = {}) {
  const trail = proposal.trail || null;
  if (!trail) return "";
  const orderType = String(proposal.order_type || "TRAIL").toUpperCase();
  const offset = protectionTrailOffsetLabel(trail);
  const live = protectionLiveTrailStop(proposal, trail);
  const parts = [
    orderType === "TRAIL LIMIT" ? "TRAIL LIMIT order" : "TRAIL order",
    live ? `quote ${live.quoteLabel} ${numberRead(live.reference)}` : "",
    live?.quoteInfo || "",
    offset,
  ].filter(Boolean);
  if (live && protectionStopChanged(trail.initial_stop_price, live.stop)) {
    parts.push(`stop ${numberRead(live.stop)}`);
  } else if (hasNumericValue(trail.initial_stop_price) && trail.initial_stop_price > 0) {
    parts.push(`stop ${numberRead(trail.initial_stop_price)}`);
  } else if (live) {
    parts.push(`stop ${numberRead(live.stop)}`);
  }
  if (hasNumericValue(trail.limit_offset)) {
    parts.push(`limit offset ${numberRead(trail.limit_offset)}`);
  }
  return parts.join(" · ");
}

function protectionTrailOffsetLabel(trail = {}) {
  if (hasNumericValue(trail.trailing_percent)) return `offset ${pct(trail.trailing_percent)}`;
  if (hasNumericValue(trail.trailing_amount)) return `offset ${numberRead(trail.trailing_amount)}`;
  return "";
}

function protectionTrailPolicyLabel(proposal = {}) {
  for (const value of proposal.details || []) {
    const match = String(value || "").match(/trail=([0-9.]+%)/i);
    if (match) return match[1];
  }
  const reason = String(proposal.reason || "");
  const match = reason.match(/([0-9.]+%)/);
  return match ? match[1] : "";
}

function protectionLiveTrailStop(proposal = {}, trail = {}) {
  const symbol = proposal.symbol || proposal.contract?.symbol;
  const action = String(proposal.action || "").toUpperCase();
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  const quote = secType === "OPT" ? null : quoteBySymbol(state.snapshot?.market_quotes?.quotes || {}, symbol);
  const quoteLabel = protectionReferenceLabel(proposal);
  const inferredReference = protectionInferredReference(proposal, trail, action);
  const reference = quote
    ? action === "BUY"
      ? firstNumber(quote.ask, quote.ask_price, inferredReference)
      : firstNumber(quote.bid, quote.bid_price, inferredReference)
    : inferredReference;
  if (!hasNumericValue(reference) || reference <= 0) return null;
  const offset = hasNumericValue(trail.trailing_percent)
    ? reference * trail.trailing_percent / 100
    : trail.trailing_amount;
  if (!hasNumericValue(offset) || offset <= 0) return null;
  const stop = quote
    ? action === "BUY" ? reference + offset : Math.max(reference - offset, 0.0001)
    : hasNumericValue(trail.initial_stop_price) ? trail.initial_stop_price : action === "BUY" ? reference + offset : Math.max(reference - offset, 0.0001);
  return { reference, quoteLabel, stop, quoteInfo: protectionQuoteStatusLabel(quote) };
}

function protectionReferenceLabel(proposal = {}) {
  const action = String(proposal.action || "").toUpperCase();
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  if (secType === "OPT") return action === "BUY" ? "ask premium" : "bid premium";
  return action === "BUY" ? "ask" : "bid";
}

function protectionInferredReference(proposal = {}, trail = {}, action = "") {
  const amount = hasNumericValue(trail.trailing_amount) ? trail.trailing_amount : null;
  const percent = hasNumericValue(trail.trailing_percent) ? trail.trailing_percent : null;
  const stop = hasNumericValue(trail.initial_stop_price) ? trail.initial_stop_price : null;
  if (amount && stop) {
    return action === "BUY" ? Math.max(stop - amount, 0.0001) : stop + amount;
  }
  if (percent && stop) {
    const ratio = percent / 100;
    if (action === "BUY") return Math.max(stop / (1 + ratio), 0.0001);
    if (ratio < 1) return Math.max(stop / (1 - ratio), 0.0001);
  }
  return null;
}

function protectionQuoteStatusLabel(quote = null) {
  if (!quote) return "";
  const parts = [];
  const dataType = String(quote.data_type || "").toLowerCase();
  if (quote.stale || quote.stale_reason) parts.push("stale");
  else if (dataType.includes("delayed")) parts.push("delayed");
  else if (dataType.includes("frozen")) parts.push("frozen");
  if (quote.price_as_of) parts.push(quote.price_as_of);
  else if (quote.price_at) parts.push(shortTimeWithZone(quote.price_at));
  return parts.join(" ");
}

function protectionStopChanged(snapshotStop, liveStop) {
  if (!hasNumericValue(snapshotStop) || !hasNumericValue(liveStop)) return false;
  return Math.abs(snapshotStop - liveStop) >= Math.max(0.01, Math.abs(snapshotStop) * 0.0025);
}

function proposalIsBuyToCover(proposal = {}) {
  const action = String(proposal.action || "").toUpperCase();
  const effect = String(proposal.position_effect || "").toLowerCase();
  return action === "BUY" &&
    Number(proposal.position_quantity || 0) < 0 &&
    (effect === "close" || effect === "reduce");
}

function protectionHeroMarketFlags(rows = [], marketEvents = {}) {
  const counts = new Map();
  for (const proposal of rows) {
    for (const flag of proposal.market_flags || []) {
      if (!marketEventFlagVisible(flag)) continue;
      const key = flag.id;
      const existing = counts.get(key) || { flag, count: 0 };
      existing.count += 1;
      counts.set(key, existing);
    }
  }
  const items = [...counts.values()].map(({ flag, count }) => ({
    flag,
    options: { label: `${flag.label || marketEventIDLabel(flag.id)} ${count}` },
  }));
  if (items.length > 0) return items;
  return marketEventHealthItems(marketEvents);
}

function protectionContractLabel(contract = {}) {
  if (String(contract.sec_type || "").toUpperCase() !== "OPT") {
    const currency = String(contract.currency || "").trim().toUpperCase();
    const market = proposalMarketLabel(proposalMarketKey({ contract }));
    const primary = String(contract.primary_exchange || contract.primary_exch || contract.exchange || "").trim().toUpperCase();
    if (currency || primary) return [currency, market === "US market" ? "" : market, primary && primary !== "SMART" ? primary : ""].filter(Boolean).join(" ");
    return "";
  }
  const right = String(contract.right || "").trim().toUpperCase();
  const strike = typeof contract.strike === "number" && contract.strike > 0 ? formatStrike(contract.strike) : "";
  const expiry = formatExpiry(contract.expiry || "");
  const optionSide = strike && right ? `${strike}${right}` : right || strike;
  const currency = String(contract.currency || "").trim().toUpperCase();
  return [expiry, optionSide, currency].filter(Boolean).join(" ");
}

function protectionPreviewGate(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  const blocker = protectionEffectiveBlockers(proposal, state.snapshot?.market_events || {})[0];
  if (blocker) return { ready: false, reason: `${blocker.code}: ${blocker.message}` };
  if (!trading.can_preview) return { ready: false, reason: "Broker preview is not enabled by trading.status" };
  return { ready: true, reason: "Preview this protection proposal with broker WhatIf; no order is placed" };
}

function protectionSubmitGate(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  const blocker = protectionEffectiveBlockers(proposal, state.snapshot?.market_events || {})[0];
  if (blocker) return { ready: false, reason: `${blocker.code}: ${blocker.message}` };
  if (!trading.can_write) return { ready: false, reason: protectionWriteUnavailableReason(trading) };
  const calendar = protectionMarketCalendar(proposal);
  const session = calendar?.session;
  if (!session) {
    return { ready: true, reason: `${proposalMarketLabel(proposalMarketKey(proposal))} calendar unavailable; broker WhatIf remains the submit authority` };
  }
  if (session.is_open) {
    return { ready: true, reason: `${proposalMarketLabel(proposalMarketKey(proposal))} is currently tradable` };
  }
  const label = marketSessionLabel(calendar);
  const market = label.phase || label.text || `${proposalMarketLabel(proposalMarketKey(proposal))} is closed`;
  return { ready: true, reason: `${market}; broker may queue the stop after fresh WhatIf` };
}

function protectionPreviewSubmitGate(proposal = {}, previewResult = null) {
  if (!previewResult) return { ready: false, reason: "Run preview first" };
  if (previewResult.pending) return { ready: false, reason: "Broker WhatIf preview is still running" };
  const blocker = (previewResult.blockers || [])[0];
  if (blocker) return { ready: false, reason: `${blocker.code}: ${blocker.message}` };
  if (!protectionPreviewSubmitEligible(previewResult)) {
    return { ready: false, reason: protectionPreviewSubmitBlockedReason(previewResult) };
  }
  if (protectionPreviewStale(previewResult, proposal)) {
    return { ready: false, reason: "Live suggestion changed; preview again before submitting" };
  }
  const writeGate = protectionSubmitGate(proposal);
  if (!writeGate.ready) return writeGate;
  return { ready: true, reason: "Submit the stop after confirmation; the daemon runs a fresh broker WhatIf before placing it" };
}

function protectionWriteUnavailableReason(trading = {}) {
  const blocker = (trading.write_blockers || trading.blockers || [])[0];
  if (blocker?.code || blocker?.message) {
    return `${blocker.code || "write_blocked"}: ${blocker.message || "broker writes are not enabled"}`;
  }
  if (trading.mode === "paper") return "Paper preview is enabled, but broker writes are not enabled for this build/session";
  return "Broker writes are not enabled by trading.status";
}

function protectionPreviewStateKey(proposal = {}) {
  return `${proposal.key || ""}@${proposal.revision || ""}`;
}

function protectionPreviewText(result = null, proposal = {}) {
  if (!result) return "";
  if (result.local && result.pending) {
    return `Stop draft ready; broker WhatIf running · ${protectionStopDraftSummary(proposal)}`;
  }
  if (result.pending) return "Previewing broker WhatIf; no order is placed";
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Preview blocked; no order placed · ${blocker.code}: ${blocker.message}`;
  const preview = result.preview || {};
  const whatIfStatus = String(preview.what_if?.status || "").trim();
  const submitEligible = result.submit_eligible || preview.submit_eligible;
  const whatIfAccepted = whatIfStatus.toLowerCase() === "accepted";
  const parts = [
    protectionPreviewOutcomeLabel({ submitEligible, whatIfAccepted, whatIfStatus, accepted: result.accepted }),
    submitEligible ? "submit eligible" : "not submit eligible",
  ];
  const tokenID = result.preview_token_id || preview.preview_token_id || "";
  if (tokenID) parts.push(`token ${shortPreviewTokenID(tokenID)}`);
  const expiresAt = result.preview_token_expires_at || preview.preview_token_expires_at || "";
  if (expiresAt) parts.push(`expires ${shortTimeWithZone(expiresAt)}`);
  const whatIfDetails = protectionWhatIfDetails(preview.what_if || {});
  if (whatIfDetails) parts.push(whatIfDetails);
  if (!submitEligible && whatIfStatus && whatIfAccepted) parts.push("WhatIf accepted");
  if (!submitEligible && preview.what_if?.message) parts.push(shortPreviewMessage(preview.what_if.message));
  if (protectionPreviewStale(result, proposal)) parts.push("live suggestion changed");
  return parts.filter(Boolean).join(" · ");
}

function protectionPreviewOutcomeLabel({ submitEligible = false, whatIfAccepted = false, whatIfStatus = "", accepted = false } = {}) {
  if (submitEligible) return "Broker WhatIf accepted; no order placed";
  if (whatIfStatus) return `Broker WhatIf ${labelize(whatIfStatus)}; no order placed`;
  if (accepted) return "Draft previewed; no order placed";
  return "Preview returned; no order placed";
}

function protectionPreviewSubmitEligible(result = {}) {
  return Boolean(result.submit_eligible || result.preview?.submit_eligible);
}

function protectionPreviewSubmitBlockedReason(result = {}) {
  const preview = result.preview || {};
  const whatIf = preview.what_if || {};
  if (whatIf.message) return shortPreviewMessage(whatIf.message);
  if (whatIf.status) return `Broker WhatIf ${labelize(whatIf.status)}`;
  if (preview.token_minted === false) return "Broker preview did not mint a submit token";
  return "Broker WhatIf is not submit eligible";
}

function protectionWhatIfDetails(whatIf = {}) {
  const margin = whatIf.margin || {};
  const currency = margin.currency || margin.commission_currency || "USD";
  const parts = [];
  if (hasNumericValue(margin.commission)) {
    parts.push(`commission ${compactMoney(margin.commission, margin.commission_currency || currency)}`);
  }
  if (hasNumericValue(margin.initial_margin_after)) {
    parts.push(`init margin ${compactMoney(margin.initial_margin_after, currency)}`);
  }
  if (margin.warning_text) parts.push(shortPreviewMessage(margin.warning_text));
  return parts.join(" · ");
}

function protectionSubmitStateText({ result = null, gate = {}, busy = false, previewResult = null } = {}) {
  if (busy) return "Submitting stop; fresh broker WhatIf running";
  if (result) return protectionSubmitResultText(result);
  if (!previewResult) return "";
  if (previewResult.pending) return "";
  if (!gate.ready) return `Submit blocked · ${gate.reason}`;
  if (!protectionPreviewSubmitEligible(previewResult)) return `Submit unavailable · ${protectionPreviewSubmitBlockedReason(previewResult)}`;
  return `Ready; Submit stop sends the broker write to ${protectionWriteConfirmationLabel()}`;
}

function protectionSubmitStateClass({ result = null, gate = {}, busy = false } = {}) {
  const classes = ["protection-row__submit-state"];
  if (busy) {
    classes.push("protection-row__submit-state--pending");
  } else if (result?.accepted || result?.place?.accepted) {
    classes.push("protection-row__submit-state--ready");
  } else if (result?.blockers?.length || (gate && gate.ready === false)) {
    classes.push("protection-row__submit-state--blocked");
  } else {
    classes.push("protection-row__submit-state--ready");
  }
  return classes.join(" ");
}

function protectionSubmitResultText(result = {}) {
  if (result.local && result.pending) return "Submitting stop; fresh broker WhatIf running";
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Submit blocked · ${blocker.code}: ${blocker.message}`;
  const orderRef = result.order_ref || result.place?.order_ref || "";
  const placeStatus = result.place?.lifecycle_status || result.place?.status || result.place?.send_state || "";
  if (result.accepted || result.place?.accepted) {
    return ["Submitted to broker", orderRef ? `order ${orderRef}` : "", placeStatus].filter(Boolean).join(" · ");
  }
  const message = result.message || result.place?.message || "";
  return message ? `Submit returned · ${message}` : "Submit returned without an accepted broker order";
}

function protectionSubmitButtonTitle({ blocked = false, previewBusy = false, submitBusy = false, gate = {} } = {}) {
  if (blocked) return "Proposal is blocked";
  if (previewBusy) return "Broker WhatIf preview is still running";
  if (submitBusy) return "Submitting stop order";
  if (!gate.ready) return gate.reason || "Submit unavailable";
  return gate.reason || `Submit the previewed stop to ${protectionWriteConfirmationLabel()}; the daemon runs a fresh broker WhatIf first`;
}

function protectionPreviewStale(result = {}, proposal = {}) {
  const boundTrail = result.preview?.draft?.trail || result.proposal?.trail || null;
  const liveTrail = proposal.trail || null;
  if (!boundTrail || !liveTrail) return false;
  const live = protectionLiveTrailStop(proposal, liveTrail);
  if (!live) return false;
  return protectionStopChanged(boundTrail.initial_stop_price, live.stop);
}

function shortPreviewTokenID(tokenID = "") {
  const value = String(tokenID || "");
  return value.length > 18 ? `${value.slice(0, 18)}...` : value;
}

function shortPreviewMessage(message = "") {
  const value = String(message || "").replace(/\s+/g, " ").trim();
  return value.length > 80 ? `${value.slice(0, 77)}...` : value;
}

function protectionStopDraftSummary(proposal = {}) {
  const parts = [
    protectionProposalTitle(proposal),
    protectionTrailText(proposal),
  ].filter(Boolean);
  return parts.join(" · ");
}

function protectionBlockerText(proposal = {}) {
  const blockers = proposal.blockers || [];
  if (blockers.length === 0) return "Proposal is blocked";
  return blockers.map((blocker) => `${blocker.code}: ${blocker.message}`).join("; ");
}

function protectionRiskExcessCurrency(counts = {}) {
  const currency = String(counts.risk_reduction_excess_currency || "").trim().toUpperCase();
  if (currency && currency !== "MIX") return currency;
  return "USD";
}

function formatStrike(value) {
  if (typeof value !== "number") return "";
  return Number.isInteger(value) ? String(value) : value.toFixed(2).replace(/\.?0+$/, "");
}

function formatExpiry(value) {
  const raw = String(value || "").trim();
  if (/^\d{8}$/.test(raw)) {
    return `${raw.slice(0, 4)}-${raw.slice(4, 6)}-${raw.slice(6, 8)}`;
  }
  return raw;
}

async function submitProtectionProposal(proposal) {
  const previewKey = protectionPreviewStateKey(proposal);
  const previewResult = state.protectionPreviews[previewKey] || null;
  const gate = protectionUsesPreviewFlow(proposal) ? protectionPreviewSubmitGate(proposal, previewResult) : protectionSubmitGate(proposal);
  if (!gate.ready) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: { blockers: [{ code: "submit_gate_blocked", message: gate.reason }], as_of: new Date().toISOString() },
    };
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    return;
  }
  const confirmation = protectionWriteConfirmation(proposal);
  if (!confirmation) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: { blockers: [{ code: "confirmation_cancelled", message: "broker submit confirmation was cancelled" }], as_of: new Date().toISOString() },
    };
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    return;
  }
  state.protectionSubmitBusy = previewKey;
  state.protectionSubmits = {
    ...state.protectionSubmits,
    [previewKey]: { local: true, pending: true, proposal, as_of: new Date().toISOString() },
  };
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/proposals/submit", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        key: proposal.key,
        revision: proposal.revision,
        quantity: proposal.quantity,
        fast_path: true,
        timeout_ms: protectionPreviewTimeoutMs(proposal),
        confirm_account: confirmation.account,
        confirm_mode: confirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: body,
    };
    await refreshOpenOrders();
  } catch (err) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: {
        blockers: [{ code: "submit_failed", message: err.message }],
        as_of: new Date().toISOString(),
      },
    };
  } finally {
    if (state.protectionSubmitBusy === previewKey) state.protectionSubmitBusy = "";
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  }
}

function protectionWriteConfirmation(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  if (!trading.mode || !trading.account) return null;
  return { account: trading.account, mode: trading.mode };
}

function protectionWriteConfirmationLabel() {
  const trading = state.snapshot?.trading || {};
  return [trading.mode, trading.account].filter(Boolean).join("/") || "broker account";
}

async function previewProtectionProposal(proposal) {
  const previewKey = protectionPreviewStateKey(proposal);
  state.protectionPreviewBusy = previewKey;
  state.protectionPreviews = {
    ...state.protectionPreviews,
    [previewKey]: {
      local: true,
      pending: true,
      proposal,
      as_of: new Date().toISOString(),
    },
  };
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/proposals/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        key: proposal.key,
        revision: proposal.revision,
        quantity: proposal.quantity,
        timeout_ms: protectionPreviewTimeoutMs(proposal),
        fast_path: proposal.bucket === "trailing_stop",
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    const result = await res.json();
    state.protectionPreviews = {
      ...state.protectionPreviews,
      [previewKey]: result,
    };
  } catch (err) {
    state.protectionPreviews = {
      ...state.protectionPreviews,
      [previewKey]: {
        blockers: [{ code: "preview_failed", message: err.message }],
        as_of: new Date().toISOString(),
      },
    };
  } finally {
    if (state.protectionPreviewBusy === previewKey) state.protectionPreviewBusy = "";
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  }
}

function protectionPreviewTimeoutMs(proposal = {}) {
  return proposal.bucket === "trailing_stop" ? 5000 : 10000;
}

async function ignoreProtectionProposal(proposal) {
  const res = await fetch("/api/proposals/ignore", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ key: proposal.key, revision: proposal.revision }),
  });
  if (!res.ok) throw new Error(await res.text());
  await refreshProtectionProposals();
}

async function refreshProtectionProposals() {
  const res = await fetch("/api/proposals/refresh", { method: "POST", credentials: "include" });
  if (res.ok) {
    const proposals = await res.json();
    applyProtectionSnapshot(proposals);
    renderAll();
  }
}

function applyProtectionSnapshot(proposals = {}) {
  state.snapshot = {
    ...(state.snapshot || {}),
    proposals,
    auto_trade: proposals.auto_trade || state.snapshot?.auto_trade,
    trading: proposals.trading || state.snapshot?.trading,
    market_events: proposals.market_events || state.snapshot?.market_events,
  };
}

function canaryExplanationCards(canary) {
  return [
    marketExplanation(canary),
    portfolioExplanation(canary),
    inputExplanation(canary),
    readinessExplanation(canary),
  ];
}

function renderCanaryStatus(canary) {
  const severity = String(canary.severity || "").toLowerCase();
  const hero = $("canaryHero");
  const pill = $("canarySeverity");
  hero.classList.remove("severity-act", "severity-watch", "severity-observe");
  pill.classList.remove("severity-act", "severity-watch", "severity-observe");
  if (severity === "act") {
    hero.classList.add("severity-act");
    pill.classList.add("severity-act");
  } else if (severity === "watch") {
    hero.classList.add("severity-watch");
    pill.classList.add("severity-watch");
  } else if (severity === "observe") {
    hero.classList.add("severity-observe");
    pill.classList.add("severity-observe");
  }
}

function canaryStageLabel(canary) {
  const action = String(canary.action || "").toLowerCase();
  if (action === "defend") return "Defend";
  if (action === "rebalance") return "Rebalance";
  if (action === "confirm_inputs") return "Check data";
  const severity = String(canary.severity || "").toLowerCase();
  if (severity === "act") return "Defend";
  if (severity === "watch") return "Watch";
  if (severity === "observe") return "Steady";
  return labelize(canary.action || "--");
}

function canarySummaryText(canary, snap = {}) {
  const fallback = canary.summary || "Waiting for canary snapshot.";
  if (!canaryInputCheckBlocksAction(canary)) return fallback;

  const verdict = cleanDetail(canary.market?.regime_posture?.label || canary.market?.regime_verdict);
  const prefix = verdict === "--" ? "Market read" : verdict;
  const issues = canaryInputIssueSummary(canary, snap);
  const issueLine = issues ? `check ${issues}` : "check input health";
  const confirmation = String(canary.market_confirmation || "").toLowerCase();
  const actionLine = confirmation === "confirmed"
    ? "verify before escalation."
    : "no market-stress action.";
  return `${prefix}; ${issueLine} before treating canary as a market signal; ${actionLine}`;
}

function canaryNeedsInputCheck(canary) {
  const inputHealth = String(canary.input_health || "").toLowerCase();
  return canaryInputCheckBlocksAction(canary) ||
    ["warming", "degraded", "failed"].includes(inputHealth);
}

function canaryInputCheckBlocksAction(canary) {
  const action = String(canary.action || "").toLowerCase();
  const direction = String(canary.direction || "").toLowerCase();
  const planner = String(canary.planner_mode_hint || "").toLowerCase();
  const readiness = String(canary.planner_readiness || "").toLowerCase();
  return action === "confirm_inputs" ||
    planner === "confirm_data" ||
    direction === "data_quality" ||
    readiness === "blocked";
}

function marketExplanation(canary) {
  const confirmation = String(canary.market_confirmation || "").toLowerCase();
  if (confirmation === "confirmed") {
    return {
      label: "Market",
      title: "Stress is confirmed",
      body: "Independent market signals agree. Treat this as real pressure, not one noisy input.",
      tone: "risk",
    };
  }
  if (confirmation === "partial") {
    return {
      label: "Market",
      title: "Pressure is developing",
      body: "Some signals are warning, but confirmation is incomplete. Watch before taking major action.",
      tone: "warn",
    };
  }
  const posture = normalizeRegimePosture(canary.market?.regime_posture) || {
    label: cleanDetail(canary.market?.regime_verdict),
    tone: legacyRegimeTone(canary.market?.regime_verdict),
  };
  const verdict = cleanDetail(posture.label || canary.market?.regime_verdict);
  const readiness = String(posture.readiness || "").toLowerCase();
  const hasGaps = marketHasDataGaps(canary.market || {}) ||
    ["blocked", "degraded", "failed", "partial", "warming"].includes(readiness) ||
    String(posture.tone || "").toLowerCase() === "data_quality";
  const postureTone = regimePostureDetailTone(posture);
  const tone = hasGaps && (postureTone === "ok" || postureTone === "neutral") ? "warn" : postureTone;
  const body = tone === "warn" || hasGaps
    ? "Market stress is not confirmed, but the regime read has watch or data-quality warnings."
    : "The broad-market regime is not giving a fully confirmed canary trigger.";
  return {
    label: "Market",
    title: verdict === "--" ? "No clear market stress" : verdict,
    body,
    tone,
  };
}

function regimePostureDetailTone(posture = {}) {
  switch (regimeWeatherClass(posture.tone)) {
    case "red":
      return "risk";
    case "amber":
      return "warn";
    case "green":
      return "ok";
    default:
      return "neutral";
  }
}

function portfolioExplanation(canary) {
  const fit = String(canary.portfolio_fit || "").toLowerCase();
  const heldStress = heldStressItems(canary);
  const heldStressLine = heldStress.length > 0 ? ` Held stress: ${heldStressSummary(heldStress, 2)}.` : "";
  if (fit === "high") {
    return {
      label: "Portfolio",
      title: "Portfolio is exposed",
      body: "The current portfolio shape is vulnerable if this market pressure continues." + heldStressLine,
      tone: "risk",
    };
  }
  if (fit === "medium") {
    return {
      label: "Portfolio",
      title: "Exposure is meaningful",
      body: "The portfolio has some sensitivity to the current stress. Size changes carefully." + heldStressLine,
      tone: "warn",
    };
  }
  if (heldStress.length > 0) {
    return {
      label: "Portfolio",
      title: "Held-name stress",
      body: heldStressSummary(heldStress, 2),
      tone: "warn",
    };
  }
  return {
    label: "Portfolio",
    title: fit === "low" ? "Exposure looks contained" : cleanDetail(canary.portfolio?.largest_exposure),
    body: "The current portfolio shape is not the main reason for a defensive canary action.",
    tone: "ok",
  };
}

function inputExplanation(canary) {
  const health = String(canary.input_health || "").toLowerCase();
  if (health === "ok") {
    return {
      label: "Inputs",
      title: "Data looks usable",
      body: "The snapshot has enough current account, portfolio, and market data for this canary read.",
      tone: "ok",
    };
  }
  return {
    label: "Inputs",
    title: "Check data quality",
    body: "Some inputs are stale, missing, or degraded. " + canaryInputCheckSentence(canary),
    tone: "warn",
  };
}

function readinessExplanation(canary) {
  const readiness = String(canary.planner_readiness || canary.planner_mode_hint || "").toLowerCase();
  if (canaryInputCheckBlocksAction(canary)) {
    return {
      label: "Readiness",
      title: "Data check blocked",
      body: "Risk-plan readiness is blocked until the missing or stale inputs are refreshed.",
      tone: "warn",
    };
  }
  if (readiness.includes("ready")) {
    return {
      label: "Readiness",
      title: "Risk plan can proceed",
      body: "The canary has enough evidence to support the next risk-management step.",
      tone: "ok",
    };
  }
  if (readiness.includes("confirm")) {
    return {
      label: "Readiness",
      title: "Confirm before acting",
      body: "The app is asking for one more data-quality or intent check before a major move.",
      tone: "warn",
    };
  }
  return {
    label: "Readiness",
    title: cleanDetail(readiness || canary.action),
    body: "Use this as a prompt for review, not an automatic trading instruction.",
    tone: "warn",
  };
}

function renderCanaryTimestamp(canary) {
  renderFreshnessTimestamp("canaryAsOf", canary.as_of, { staleMinutes: 5 });
}

function renderMarketContext(snap) {
  const canary = snap.canary || {};
  const market = canary.market || {};
  const quotes = snap.market_quotes?.quotes || {};
  const strip = $("marketQuoteStrip");
  const symbols = ["SPY", "VIX", "QQQ", "IWM", "HYG", "TLT"];
  strip.replaceChildren(...symbols.map((symbol) => marketQuoteCell(symbol, quoteBySymbol(quotes, symbol), market, snap.market_quotes)));
}

function marketQuoteCell(symbol, quote, market, marketQuotes) {
  const fallback = marketQuoteFallback(symbol, market);
  const price = quotePrice(quote) ?? fallback.price;
  const change = quoteChangePct(quote) ?? fallback.changePct;
  const error = marketQuotes?.errors?.[symbol] || "";
  const hasPrice = typeof price === "number";
  const cell = document.createElement("div");
  cell.className = "market-quote-cell";
  cell.classList.toggle("market-quote-cell--missing", !hasPrice);
  if (error) cell.classList.add("market-quote-cell--error");
  cell.setAttribute("aria-label", `${symbol} ${hasPrice ? numberRead(price) : "price pending"} ${typeof change === "number" ? signedPct(change) : "change pending"}`);

  const head = document.createElement("div");
  head.className = "market-quote-cell__head";
  const label = document.createElement("b");
  label.textContent = symbol;
  head.append(label);

  const valueLine = document.createElement("div");
  valueLine.className = "market-quote-cell__value";
  const value = document.createElement("strong");
  value.textContent = hasPrice ? numberRead(price) : "--";
  const changeEl = document.createElement("span");
  changeEl.className = "market-change " + marketQuoteChangeClass(symbol, change);
  changeEl.textContent = typeof change === "number" ? signedPct(change) : "--";
  valueLine.append(value, changeEl);

  const source = document.createElement("small");
  source.className = "market-quote-cell__source" + (error ? " error" : "");
  source.textContent = error
    ? marketQuoteInterruptedLine(quote, marketQuotes, hasPrice)
    : marketQuoteSourceLine(quote, marketQuotes, fallback.source);
  source.title = error
    ? `${marketQuoteErrorLabel(error)}; ${hasPrice ? "showing last available quote" : "no frozen quote available yet"}`
    : source.textContent;
  cell.append(head, valueLine, source);
  return cell;
}

function marketQuoteChangeClass(symbol, change) {
  return signedClass(normalizeSymbol(symbol) === "VIX" && typeof change === "number" ? -change : change);
}

function marketQuoteInterruptedLine(quote, marketQuotes, hasPrice) {
  const at = quoteTimestamp(quote) || marketQuotes?.as_of || "";
  const atLabel = at ? ` · ${quoteTime(at)}` : "";
  return hasPrice ? `Frozen${atLabel}` : "Feed issue";
}

function marketQuoteFallback(symbol, market = {}) {
  switch (symbol) {
    case "SPY":
      return { price: market.spy_price, changePct: market.spy_change_pct, source: "canary market read" };
    case "QQQ":
      return {
        price: firstNumber(market.qqq_price, market.ndx_price, market.nasdaq_price, market.nasdaq_100_price),
        changePct: firstNumber(market.qqq_change_pct, market.ndx_change_pct, market.nasdaq_change_pct, market.nasdaq_100_change_pct),
        source: "canary market read",
      };
    case "VIX":
      return { price: market.vix, changePct: market.vix_change_pct, source: "canary market read" };
    default:
      return { price: null, changePct: null, source: "IBKR quote pending" };
  }
}

function marketQuoteSourceLine(quote, marketQuotes, fallback) {
  const parts = [];
  const quality = String(quote?.quote_quality || "").trim();
  const dataType = String(quote?.data_type || "").trim();
  if (quality && quality !== "firm") parts.push(labelize(quality));
  if (dataType && dataType !== "live") parts.push(labelize(dataType));
  const uniqueParts = [...new Set(parts)];
  if (uniqueParts.length === 0) uniqueParts.push(quote ? "IBKR quote" : fallback || "Quote pending");
  const at = quote?.quote_price_at || quote?.price_at || quote?.as_of || marketQuotes?.as_of;
  if (at) uniqueParts.push(quoteTime(at));
  return uniqueParts.join(" · ");
}

function quoteTime(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

function renderRegimePanel(snap) {
  const canary = snap.canary || {};
  const market = canary.market || {};
  const indicators = canary.market_indicators || [];
  const posture = regimePosture(snap, canary, market);
  const regimeStatus = marketRegimeStatusLine(snap, canary, market, indicators);
  $("marketRegime").textContent = marketRegimeLabel(posture);
  $("marketRegimeSummary").textContent = regimeStatus.summary;
  $("marketRegimeMix").textContent = regimeStatus.detail;
  $("marketRegimeMix").title = regimeStatus.title;
  renderFreshnessTimestamp("regimeAsOf", latestRegimeTimestamp(canary, indicators), {
    staleMinutes: 60,
  });
  renderMarketWeather(posture);
  renderRegimeDetail(indicators, snap, canary);
}

function marketSourceIssueLabels(snap = {}) {
  const labels = [];
  const add = (label) => {
    label = String(label || "").trim();
    if (label && !labels.includes(label)) labels.push(label);
  };

  for (const [symbol, error] of Object.entries(snap.market_quotes?.errors || {})) {
    add(`${normalizeSymbol(symbol)} ${marketQuoteErrorLabel(error)}`);
  }

  const marketSourceError = String(snap.sources?.market_quotes?.error || "").trim();
  if (marketSourceError) {
    for (const part of marketSourceError.split("|")) {
      add(marketSourceErrorLabel(part));
    }
  }

  return labels;
}

function marketSourceErrorLabel(error) {
  const text = String(error || "").trim();
  const match = text.match(/^([A-Za-z0-9._-]+):\s*(.+)$/);
  if (!match) return marketQuoteErrorLabel(text);
  return `${normalizeSymbol(match[1])} ${marketQuoteErrorLabel(match[2])}`;
}

function marketQuoteErrorLabel(error) {
  const text = String(error || "").trim();
  if (!text) return "";
  const withoutPrefix = text.replace(/^quote\.snapshot:\s*/i, "").trim();
  const lower = withoutPrefix.toLowerCase();
  if (lower.includes("gateway_unavailable") || lower.includes("connection unavailable") || lower.includes("ibkr connection unavailable")) return "feed interrupted";
  if (lower.includes("symbol_inactive")) return "quote unavailable";
  if (lower.includes("timeout")) return "quote timeout";
  return withoutPrefix;
}

function quoteBySymbol(quotes, symbol) {
  if (!quotes) return null;
  return quotes[symbol] || quotes[symbol.toLowerCase()] || null;
}

function quotePrice(quote) {
  if (!quote) return null;
  return firstNumber(quote.quote_price, quote.price, quote.last, quote.mark);
}

function quotePrevClose(quote) {
  if (!quote) return null;
  return firstNumber(quote.prev_close, quote.regular_close, quote.prior_regular_close);
}

function quoteChangePct(quote) {
  if (!quote) return null;
  const explicit = firstNumber(quote.quote_change_pct, quote.change_pct, quote.regular_change_pct);
  if (typeof explicit === "number") return explicit;
  const price = quotePrice(quote);
  const prev = quotePrevClose(quote);
  if (typeof price === "number" && typeof prev === "number" && prev !== 0) {
    return (price - prev) / prev * 100;
  }
  return null;
}

function quoteChange(quote) {
  if (!quote) return null;
  const explicit = firstNumber(quote.quote_change, quote.change, quote.regular_change);
  if (typeof explicit === "number") return explicit;
  const price = quotePrice(quote);
  const prev = quotePrevClose(quote);
  if (typeof price === "number" && typeof prev === "number") {
    return price - prev;
  }
  return null;
}

function regimePosture(snap = {}, canary = {}, market = {}) {
  for (const candidate of [snap.regime?.posture, market.regime_posture, canary.market?.regime_posture]) {
    const normalized = normalizeRegimePosture(candidate);
    if (normalized) return normalized;
  }
  const label = cleanDetail(snap.regime?.summary?.label || snap.regime?.composite?.verdict || market.regime_verdict);
  if (label === "--") return { label: "--", tone: "na" };
  return { label, tone: legacyRegimeTone(label) };
}

function normalizeRegimePosture(candidate) {
  if (!candidate || typeof candidate !== "object") return null;
  const label = cleanDetail(candidate.label);
  const tone = String(candidate.tone || "").trim().toLowerCase();
  if (label === "--" && !tone) return null;
  return {
    label,
    tone: tone || legacyRegimeTone(label),
    stage: candidate.stage || "",
    severity: candidate.severity || "",
    readiness: candidate.readiness || "",
    confidence: candidate.confidence || "",
    evidence: candidate.evidence || "",
  };
}

function legacyRegimeTone(label) {
  const lower = String(label || "").toLowerCase();
  if (!lower || lower === "--") return "na";
  if (lower.includes("full risk-off")) return "risk_off";
  if (lower.includes("broad stress")) return "stress";
  if (lower.includes("stress signal") || lower.includes("elevated stress") || lower.includes("watch")) return "watch";
  if (lower.includes("insufficient") || lower.includes("no usable") || lower.includes("no ranked")) return "data_quality";
  if (lower.includes("normal") || lower.includes("constructive")) return "normal";
  return "watch";
}

function marketRegimeLabel(posture = {}) {
  const label = cleanDetail(posture.label);
  return label === "--" ? "--" : labelize(label);
}

function marketRegimeStatusLine(snap, canary, market, indicators) {
  const latest = latestRegimeRead(canary, indicators);
  const ranked = Number(market.ranked_clusters || 0);
  const unranked = Number(market.unranked_clusters || 0);
  const total = ranked + unranked;
  if (!canaryNeedsInputCheck(canary) && !marketHasDataGaps(market)) {
    return { summary: "Regime read", detail: latest, title: latest };
  }

  const issues = canaryInputIssueSummary(canary, snap);
  const coverage = total > 0 ? `${ranked}/${total} ranked` : "ranked inputs pending";
  const summary = issues ? `${coverage}; data gaps` : `${coverage}; degraded`;
  const gateway = gatewayDataStatus(snap);
  const detail = issues ? `${gateway}; check ${issues}` : `${gateway}; check regime sources`;
  return { summary, detail, title: `${detail}; regime updated ${latest}` };
}

function marketRegimeMix(market, indicators) {
  if (indicators.length === 0) return "Waiting for market evidence";
  const counts = indicators.reduce((out, indicator) => {
    const status = indicatorStatusClass(indicator.status);
    out[status] = (out[status] || 0) + 1;
    return out;
  }, {});
  const risk = (counts.red || 0) + Number(market.red_clusters || 0);
  const neutral = (counts.amber || 0) + (counts.context || 0) + (counts.na || 0);
  const normal = counts.green || 0;
  return `${risk} Risk · ${neutral} Neutral · ${normal} Normal`;
}

function latestRegimeRead(canary, indicators) {
  const latest = latestRegimeTimestamp(canary, indicators);
  if (latest) return shortTimeWithZone(latest.toISOString());
  return latestRegimeTimestampFallback(canary, indicators) || "Waiting for regime timestamp";
}

function latestRegimeTimestamp(canary, indicators) {
  const sourceAsOf = canary.source_as_of || {};
  const candidates = [
    sourceAsOf.regime,
    sourceAsOf.market_regime,
    canary.regime_as_of,
    canary.market?.regime_as_of,
    canary.as_of,
    ...indicators.map((indicator) => indicator.as_of),
  ].filter(Boolean);
  let latest = null;
  for (const candidate of candidates) {
    const parsed = parseDate(candidate);
    if (parsed && (!latest || parsed > latest)) {
      latest = parsed;
    }
  }
  return latest;
}

function latestRegimeTimestampFallback(canary, indicators) {
  const sourceAsOf = canary.source_as_of || {};
  return [
    sourceAsOf.regime,
    sourceAsOf.market_regime,
    canary.regime_as_of,
    canary.market?.regime_as_of,
    canary.as_of,
    ...indicators.map((indicator) => indicator.as_of),
  ].map((candidate) => String(candidate || "").trim()).find(Boolean) || "";
}

function renderMarketWeather(posture = {}) {
  const tone = regimeWeatherClass(posture.tone);
  const card = $("regimeSummaryCard");
  const panel = $("regimePanel");
  card.classList.remove("weather-green", "weather-amber", "weather-red", "weather-na");
  panel.classList.remove("weather-green", "weather-amber", "weather-red", "weather-na");
  card.classList.add("weather-" + tone);
  panel.classList.add("weather-" + tone);
}

function regimeWeatherClass(tone) {
  switch (String(tone || "").toLowerCase()) {
    case "normal":
      return "green";
    case "stress":
    case "risk_off":
      return "red";
    case "watch":
    case "data_quality":
      return "amber";
    default:
      return "na";
  }
}

function marketHasDataGaps(market = {}) {
  const lists = [
    market.ambiguous_clusters,
    market.partial_clusters,
    market.computing_clusters,
    market.degraded_clusters,
    market.stale_clusters,
  ];
  return lists.some((items) => Array.isArray(items) && items.length > 0) ||
    Number(market.unranked_clusters || 0) > 0;
}

function canaryInputCheckSentence(canary) {
  const issues = canaryInputIssueSummary(canary, state.snapshot || {});
  return issues
    ? `Refresh or verify ${issues} before treating the canary as a market signal.`
    : "Use the detail rows before acting.";
}

function canaryInputIssueSummary(canary, snap = {}) {
  return humanList(canaryInputIssueLabels(canary, snap), 4);
}

function canaryInputIssueLabels(canary, snap = {}) {
  const labels = [];
  const add = (label) => {
    label = String(label || "").trim();
    if (label && !labels.includes(label)) labels.push(label);
  };

  const market = canary.market || {};
  for (const cluster of [
    ...(market.partial_clusters || []),
    ...(market.ambiguous_clusters || []),
    ...(market.computing_clusters || []),
    ...(market.degraded_clusters || []),
    ...(market.stale_clusters || []),
  ]) {
    add(clusterInputLabel(cluster));
  }

  for (const item of snap.status?.data_quality || []) {
    for (const cluster of [
      ...(item.partial_clusters || []),
      ...(item.degraded_clusters || []),
      ...(item.stale_clusters || []),
    ]) {
      add(clusterInputLabel(cluster));
    }
  }

  for (const source of canary.source_health || []) {
    const status = String(source.status || "").toLowerCase();
    if (!status || status === "ok") continue;
    if (source.source === "account") add("account snapshot");
    if (source.source === "positions") add("positions snapshot");
  }

  for (const warning of canary.warnings || []) {
    const text = String(warning || "").toLowerCase();
    if (text.includes("hyg") || text.includes("50dma") || text.includes("50-day")) add("HYG 50-DMA");
    if (text.includes("usd.jpy") || text.includes("usd/jpy") || text.includes("weekly") || text.includes("7d")) add("USD/JPY baseline");
    if (text.includes("gamma")) add("gamma cache");
  }
  return labels;
}

function clusterInputLabel(cluster) {
  switch (String(cluster || "").trim().toLowerCase()) {
    case "credit":
      return "HYG 50-DMA";
    case "fx":
      return "USD/JPY baseline";
    case "gamma":
      return "gamma cache";
    case "breadth":
      return "breadth compute";
    case "vol":
    case "volatility":
      return "volatility feed";
    case "funding":
      return "funding series";
    default:
      return labelize(cluster);
  }
}

function gatewayDataStatus(snap = {}) {
  const status = snap.status || {};
  const mode = String(status.account_mode || snap.trading?.mode || "").toLowerCase();
  const quoteReady = (status.subsystems || []).some((subsystem) =>
    String(subsystem.name || "").toLowerCase() === "quote" &&
    String(subsystem.status || "").toLowerCase() === "ready"
  );
  if (status.connected && quoteReady && mode.includes("paper")) return "Paper gateway live quotes OK";
  if (status.connected && quoteReady) return "Gateway live quotes OK";
  if (status.connected) return "Gateway connected";
  return "Gateway status pending";
}

function humanList(items, limit = 3) {
  items = (items || []).filter(Boolean);
  if (items.length === 0) return "";
  const shown = items.slice(0, limit);
  if (items.length > limit) {
    shown[shown.length - 1] = `${shown[shown.length - 1]} +${items.length - limit} more`;
  }
  if (shown.length === 1) return shown[0];
  if (shown.length === 2) return `${shown[0]} and ${shown[1]}`;
  return `${shown.slice(0, -1).join(", ")}, and ${shown[shown.length - 1]}`;
}

function renderSignedPercent(id, value, positiveIsRisk) {
  const el = $(id);
  el.classList.remove("signed", "ok", "risk", "neutral", "is-empty");
  if (typeof value !== "number") {
    el.textContent = "";
    el.classList.add("is-empty");
    return "neutral";
  }
  el.textContent = signedPct(value);
  el.classList.add("signed");
  const isRisk = positiveIsRisk ? value > 0 : value < 0;
  const isOk = positiveIsRisk ? value < 0 : value > 0;
  if (isRisk) el.classList.add("risk");
  if (isOk) el.classList.add("ok");
  if (!isRisk && !isOk) el.classList.add("neutral");
  return isRisk ? "risk" : isOk ? "ok" : "neutral";
}

function renderRegimeDetail(indicators, snap = {}, canary = {}) {
  const panel = $("regimeDetailPanel");
  const button = $("regimeDetailToggle");
  panel.hidden = !state.regimeDetailOpen;
  button.textContent = state.regimeDetailOpen ? "Hide detail" : "Show detail";
  button.setAttribute("aria-expanded", String(state.regimeDetailOpen));
  if (!state.regimeDetailOpen) return;
  const rows = indicators.length > 0 ? indicators : regimeFallbackIndicators(snap, canary);
  $("regimeIndicators").replaceChildren(...rows.map((indicator) => {
    const row = document.createElement("div");
    row.className = "indicator-row";
    const dot = document.createElement("span");
    dot.className = "indicator-status " + indicatorStatusClass(indicator.status);
    const body = document.createElement("div");
    body.className = "indicator-body";
    const head = document.createElement("div");
    head.className = "indicator-head";
    const title = document.createElement("b");
    title.textContent = indicator.name || "Indicator";
    const at = document.createElement("span");
    at.textContent = indicator.as_of || "--";
    head.append(title, at);
    const reading = document.createElement("p");
    reading.textContent = indicator.reading || "--";
    body.append(head, reading);
    if (indicator.comment) {
      const comment = document.createElement("small");
      comment.textContent = indicator.comment;
      body.append(comment);
    }
    row.append(dot, body);
    return row;
  }));
  renderRegimeQualityRemarks(snap, canary);
}

function regimeFallbackIndicators(snap = {}, canary = {}) {
  const market = canary.market || {};
  const status = marketRegimeStatusLine(snap, canary, market, []);
  const tone = regimeWeatherClass(regimePosture(snap, canary, market).tone);
  const rows = [{
    name: "Regime status",
    status: tone === "red" ? "red" : tone === "green" ? "green" : tone === "amber" ? "amber" : "na",
    as_of: latestRegimeRead(canary, []),
    reading: status.summary,
    comment: status.detail,
  }, {
    name: "Gateway",
    status: state.connectionOK ? "green" : "amber",
    as_of: snap.updated_at ? shortTimeWithZone(snap.updated_at) : "--",
    reading: gatewayDataStatus(snap),
    comment: state.connectionOK ? "Live app stream connected." : "App stream is reconnecting.",
  }];
  const issues = [...marketSourceIssueLabels(snap), ...canaryInputIssueLabels(canary, snap)];
  if (issues.length > 0) {
    rows.push({
      name: "Data quality",
      status: "amber",
      as_of: canary.as_of ? shortTimeWithZone(canary.as_of) : "--",
      reading: humanList([...new Set(issues)], 4),
      comment: "Fine-print data gaps are kept inside the Regime panel.",
    });
  }
  return rows;
}

function renderRegimeQualityRemarks(snap = {}, canary = {}) {
  const panel = $("regimeQualityRemarks");
  const text = $("regimeQualityText");
  if (!panel || !text) return;
  const issues = [...marketSourceIssueLabels(snap), ...canaryInputIssueLabels(canary, snap)];
  const unique = [...new Set(issues.filter(Boolean))];
  panel.hidden = unique.length === 0;
  text.textContent = unique.length === 0 ? "--" : humanList(unique, 4);
}

function indicatorStatusClass(status) {
  status = String(status || "").toLowerCase();
  if (["green", "amber", "red", "context"].includes(status)) return status;
  return "na";
}

function detailCard(card) {
  const item = document.createElement("div");
  item.className = "detail-card " + (card.tone || "neutral");
  const labelEl = document.createElement("span");
  labelEl.textContent = card.label;
  const valueEl = document.createElement("b");
  valueEl.textContent = card.title || "--";
  const body = document.createElement("p");
  body.textContent = card.body || "";
  item.append(labelEl, valueEl, body);
  return item;
}

function renderHeldStress(canary) {
  const panel = $("heldStressPanel");
  if (!panel) return;
  const stresses = heldStressItems(canary);
  panel.hidden = stresses.length === 0;
  if (stresses.length === 0) {
    $("heldStressSummary").textContent = "--";
    $("heldStressList").replaceChildren();
    return;
  }
  $("heldStressSummary").textContent = heldStressSummary(stresses, 2);
  $("heldStressList").replaceChildren(...stresses.slice(0, 5).map(heldStressRow));
}

function heldStressRow(stress) {
  const row = document.createElement("div");
  row.className = "held-stress-row " + heldStressTone(stress);
  const title = document.createElement("b");
  title.textContent = stress.underlying || "Held name";
  const body = document.createElement("p");
  body.textContent = heldStressEvidence(stress);
  const reasons = document.createElement("div");
  reasons.className = "held-stress-row__reasons";
  for (const reason of heldStressReasonLabels(stress)) {
    const pill = document.createElement("span");
    pill.textContent = reason;
    reasons.append(pill);
  }
  row.append(title, body, reasons);
  return row;
}

function heldStressItems(canary) {
  const items = canary?.portfolio?.held_stress;
  return Array.isArray(items) ? items : [];
}

function heldStressTone(stress) {
  const daily = stress.daily_pnl_pct_nlv;
  if (typeof daily === "number" && daily <= -2) return "risk";
  if ((stress.liquidity_flags || []).length > 0 || typeof stress.near_expiry_delta_pct_nlv === "number") return "warn";
  return "neutral";
}

function heldStressSummary(stresses, limit) {
  const shown = stresses.slice(0, limit).map((stress) => {
    const evidence = heldStressEvidence(stress);
    return `${stress.underlying || "Held name"} ${evidence}`;
  });
  if (stresses.length > shown.length) {
    shown.push(`+${stresses.length - shown.length} more`);
  }
  return shown.join("; ");
}

function heldStressEvidence(stress) {
  const parts = [];
  if (typeof stress.daily_pnl_pct_nlv === "number") {
    parts.push(`daily P/L ${signedPct(stress.daily_pnl_pct_nlv)} NLV`);
  }
  if (typeof stress.near_expiry_delta_pct_nlv === "number") {
    let text = `near-expiry delta ${pct(stress.near_expiry_delta_pct_nlv)} NLV`;
    if (typeof stress.near_expiry_min_dte === "number") {
      text += ` at ${stress.near_expiry_min_dte} DTE`;
    }
    parts.push(text);
  }
  if ((stress.liquidity_flags || []).length > 0) {
    parts.push("liquidity " + stress.liquidity_flags.map(heldStressFlagLabel).join(", "));
  }
  if (typeof stress.market_value_pct_nlv === "number") {
    parts.push(`market value ${pct(stress.market_value_pct_nlv)} NLV`);
  }
  if (typeof stress.delta_pct_nlv === "number") {
    parts.push(`delta ${pct(stress.delta_pct_nlv)} NLV`);
  }
  if (parts.length === 0 && (stress.material_reasons || []).length > 0) {
    parts.push(stress.material_reasons.map(labelize).join(", "));
  }
  return parts.join(" / ") || "Material held-name stress";
}

function heldStressReasonLabels(stress) {
  const labels = (stress.material_reasons || []).map(heldStressReasonLabel);
  if ((stress.liquidity_flags || []).length > 0) labels.push("Liquidity");
  if (labels.length === 0 && (stress.signal_ids || []).length > 0) {
    labels.push(...stress.signal_ids.map(heldStressReasonLabel));
  }
  return [...new Set(labels)].slice(0, 4);
}

function heldStressReasonLabel(value) {
  const key = String(value || "").toLowerCase();
  if (key === "daily_pnl" || key === "held_underlying_pnl_shock") return "Daily P/L";
  if (key === "near_expiry_option_delta" || key === "held_option_expiry_concentration") return "Near-expiry options";
  if (key === "market_value") return "Market value";
  if (key === "delta") return "Delta";
  if (key === "held_liquidity_degraded") return "Liquidity";
  return labelize(value);
}

function heldStressFlagLabel(value) {
  const key = String(value || "").toLowerCase();
  if (key === "mark_outside_bid_ask") return "mark outside bid/ask";
  if (key === "options_closed") return "options closed";
  if (key === "stale_quote") return "stale quote";
  if (key === "wide_spread") return "wide spread";
  return cleanDetail(value);
}

function renderPortfolioRisk(positions, account) {
  const portfolio = positions.portfolio || {};
  const baseCurrency = portfolio.base_currency || account.base_currency || "USD";
  renderPortfolioDeltaPosture(portfolio, account);
  renderSensitiveText("portfolioDailyTheta", riskMoney(
    portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
    portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy));
  $("portfolioGreeksCoverage").textContent = greeksCoverage(portfolio, positions);
  $("portfolioGreeksMeaning").textContent = greeksMeaning(portfolio, positions);
  renderSensitiveText("portfolioFxSensitivity", riskMoney(
    portfolio.fx_sensitivity_per_pct,
    portfolio.fx_base_currency || baseCurrency,
  ), hasNumericValue(portfolio.fx_sensitivity_per_pct));
  $("portfolioDetailSummary").textContent = portfolioDetailSummary(portfolio, positions);
  renderPortfolioDetail(portfolio, positions, baseCurrency);

  const exposures = (portfolio.exposure_base || []).slice(0, 3);
  renderExposureVisual(exposureComposition(positions, account, portfolio, baseCurrency));
  const list = $("portfolioExposureList");
  list.hidden = exposures.length === 0;
  list.replaceChildren(...exposures.map((exposure) => {
    const row = document.createElement("div");
    row.className = "metric-row";
    const label = document.createElement("span");
    const pctText = typeof exposure.market_value_pct_nlv === "number" ? ` ${pct(exposure.market_value_pct_nlv)}` : "";
    label.textContent = exposure.underlying + pctText;
    const value = document.createElement("b");
    value.textContent = sensitiveDisplayMoney(exposure.market_value_base, exposure.base_currency || baseCurrency);
    value.className = "exposure-value" + (sensitiveMoneyHidden(exposure.market_value_base) ? " is-private" : "");
    row.append(label, value);
    const pnl = exposure.daily_pnl_base ?? exposure.unrealized_pnl_base;
    if (state.accountValueVisible && typeof pnl === "number") {
      const detail = document.createElement("small");
      detail.className = signedClass(pnl);
      detail.textContent = "P/L " + money(pnl, exposure.base_currency || baseCurrency);
      value.append(detail);
    }
    return row;
  }));
}

function renderPortfolioDeltaPosture(portfolio, account) {
  const posture = portfolioDeltaPosture(portfolio, account);
  const value = $("portfolioDollarDelta");
  if (!value) return;
  value.textContent = posture.label;
  value.className = "portfolio-delta-posture " + posture.tone;
  const meaning = $("portfolioDeltaMeaning");
  if (meaning) {
    meaning.textContent = posture.detail;
  }
}

function portfolioDeltaPosture(portfolio = {}, account = {}) {
  const delta = portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy;
  const nlv = portfolio.net_liquidation_base ?? account.net_liquidation;
  if (typeof delta !== "number") {
    return {
      label: "Delta unavailable",
      detail: "Waiting for portfolio Greeks or stock exposure.",
      tone: "neutral",
    };
  }
  const ratio = typeof nlv === "number" && nlv > 0 ? Math.abs(delta) / nlv : null;
  const direction = delta > 0 ? "Long-biased" : delta < 0 ? "Short-biased" : "Flat";
  if (ratio === null) {
    return {
      label: direction,
      detail: "Market sensitivity is available in detail.",
      tone: "neutral",
    };
  }
  if (ratio >= 1) {
    return {
      label: "High delta risk",
      detail: `${direction}; detail has the private estimate.`,
      tone: "risk",
    };
  }
  if (ratio >= 0.35) {
    return {
      label: "Moderate delta",
      detail: `${direction}; watch broad-market moves.`,
      tone: "warn",
    };
  }
  return {
    label: "Low delta",
    detail: `${direction}; broad-market sensitivity is contained.`,
    tone: "ok",
  };
}

function exposureComposition(positions, account, portfolio, baseCurrency) {
  const netLiquidation = portfolio.net_liquidation_base ?? account.net_liquidation;
  const stocks = sumAbsBase(positions.stocks || [], baseCurrency);
  const options = sumAbsBase(positions.options || [], baseCurrency);
  const cash = typeof account.total_cash === "number" ? Math.max(0, account.total_cash) : 0;
  if (typeof netLiquidation === "number" && netLiquidation > 0) {
    const raw = [
      { label: "Equity", pct: stocks / netLiquidation * 100 },
      { label: "Options", pct: options / netLiquidation * 100 },
      { label: "Cash", pct: cash / netLiquidation * 100 },
    ].filter((item) => item.pct > 0.1);
    const used = raw.reduce((sum, item) => sum + item.pct, 0);
    if (used < 99) raw.push({ label: "Other", pct: 100 - used, other: true });
    return normalizeComposition(raw);
  }
  return normalizeComposition((portfolio.exposure_base || []).slice(0, 3).map((exposure) => ({
    label: exposure.underlying || "--",
    pct: Math.abs(Number(exposure.market_value_pct_nlv || 0)),
  })));
}

function sumAbsBase(rows, baseCurrency) {
  return rows.reduce((sum, row) => {
    if (typeof row.market_value_base === "number") return sum + Math.abs(row.market_value_base);
    if (row.currency === baseCurrency && typeof row.market_value_ccy === "number") return sum + Math.abs(row.market_value_ccy);
    return sum;
  }, 0);
}

function normalizeComposition(items) {
  const filtered = items.filter((item) => item.pct > 0);
  const total = filtered.reduce((sum, item) => sum + item.pct, 0);
  if (total <= 0) return [];
  if (total <= 100) return filtered;
  return filtered.map((item) => ({ ...item, pct: item.pct / total * 100 }));
}

function renderExposureVisual(exposures) {
  const visual = $("portfolioExposureVisual");
  if (!visual) return;
  if (exposures.length === 0) {
    visual.hidden = true;
    visual.replaceChildren();
    return;
  }

  const normalized = exposures.filter((exposure) => exposure.pct > 0);
  if (normalized.length === 0) {
    visual.hidden = true;
    visual.replaceChildren();
    return;
  }

  const totalShown = normalized.reduce((sum, exposure) => sum + exposure.pct, 0);
  const remainder = Math.max(0, 100 - totalShown);
  const trackBase = totalShown + remainder || totalShown;

  const track = document.createElement("div");
  track.className = "exposure-visual__track";
  for (const exposure of normalized) {
    const segment = document.createElement("div");
    segment.className = "exposure-visual__segment" + (exposure.other ? " exposure-visual__segment--other" : "");
    segment.style.width = `${(exposure.pct / trackBase) * 100}%`;
    segment.title = `${exposure.label} ${pct(exposure.pct)}`;
    if (exposure.pct >= 5) {
      segment.textContent = wholePct(exposure.pct);
    }
    track.append(segment);
  }
  if (remainder > 0) {
    const other = document.createElement("div");
    other.className = "exposure-visual__segment exposure-visual__segment--other";
    other.style.width = `${(remainder / trackBase) * 100}%`;
    other.title = `Other ${pct(remainder)}`;
    track.append(other);
  }

  const legend = document.createElement("div");
  legend.className = "exposure-visual__legend";
  legend.replaceChildren(...normalized.map((exposure) => exposureLegendItem(exposure.label, exposure.pct)));
  if (remainder > 0) {
    const otherItem = exposureLegendItem("Other", remainder);
    otherItem.classList.add("exposure-visual__item--other");
    legend.append(otherItem);
  }

  visual.hidden = false;
  visual.replaceChildren(track, legend);
}

function exposureLegendItem(label, value) {
  const item = document.createElement("div");
  item.className = "exposure-visual__item";
  const swatch = document.createElement("span");
  swatch.className = "exposure-visual__swatch";
  const itemLabel = document.createElement("span");
  itemLabel.className = "exposure-visual__label";
  itemLabel.textContent = label;
  const itemValue = document.createElement("span");
  itemValue.className = "exposure-visual__value";
  itemValue.textContent = wholePct(value);
  item.append(swatch, itemLabel, itemValue);
  return item;
}

function renderPortfolioDetail(portfolio, positions, baseCurrency) {
  const panel = $("portfolioDetailPanel");
  const button = $("portfolioDetailToggle");
  const wrapper = $("portfolioPanel");
  wrapper.dataset.open = String(state.portfolioDetailOpen);
  panel.hidden = !state.portfolioDetailOpen;
  button.setAttribute("aria-expanded", String(state.portfolioDetailOpen));
  button.textContent = state.portfolioDetailOpen ? "Hide detail" : "Detail";
  if (!state.portfolioDetailOpen) return;
  $("portfolioDetailList").replaceChildren(...portfolioDetailRows(portfolio, positions, baseCurrency).map(detailFact));
}

function setPortfolioExpansion(open) {
  state.portfolioDetailOpen = Boolean(open);
  renderPortfolioDetail(
    state.snapshot?.positions?.portfolio || {},
    state.snapshot?.positions || {},
    state.snapshot?.account?.base_currency || "USD",
  );
}

function portfolioDetailRows(portfolio, positions, baseCurrency) {
  const total = portfolio.greeks_total || 0;
  const covered = portfolio.greeks_coverage || 0;
  const greeksTitle = total > 0 ? `${covered}/${total} option legs covered` : "No option legs";
  const greeksBody = total === 0
    ? "There are no option legs that need model Greeks in this snapshot."
    : covered === total
      ? "Delta, theta, gamma, and vega aggregates are complete for the current option legs."
      : "Some option legs are missing model Greeks; treat portfolio Greeks as partial.";
  const rows = [
    {
      label: "Greeks",
      title: greeksTitle,
      body: greeksBody,
      tone: total > 0 && covered < total ? "warn" : "ok",
    },
    {
      label: "Market risk (delta)",
      title: sensitiveMoney(
        portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
        portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
      ),
      body: state.accountValueVisible
        ? "Approximate portfolio move for a one-point move in the underlyings, converted to account base when possible."
        : "Hidden while account privacy is on. Dollar delta estimates how fast the held book moves with the market.",
      tone: "neutral",
    },
    {
      label: "Theta/day",
      title: sensitiveMoney(
        portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
        portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
      ),
      body: state.accountValueVisible
        ? "Estimated option time decay per day. Negative values mean expected decay cost."
        : "Hidden while account privacy is on. Theta/day estimates option time decay across the book.",
      tone: signedTone(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy, true),
    },
    {
      label: "FX 1%",
      title: sensitiveMoney(portfolio.fx_sensitivity_per_pct, portfolio.fx_base_currency || baseCurrency),
      body: state.accountValueVisible
        ? "Estimated base-currency P/L from a 1% move in non-base contract currencies."
        : "Hidden while account privacy is on. FX 1% estimates currency sensitivity across non-base exposures.",
      tone: "neutral",
    },
  ];
  if ((portfolio.exposure_base || []).length > 0) {
    rows.push({
      label: "Largest exposure",
      title: portfolio.exposure_base[0].underlying || "--",
      body: "The exposure rows below show dominant underlyings by absolute base-currency market value.",
      tone: "neutral",
    });
  } else if ((positions.stocks || []).length === 0 && (positions.options || []).length === 0) {
    rows.push({
      label: "Positions",
      title: "No open positions",
      body: "The portfolio-risk panel has no position rows to aggregate.",
      tone: "neutral",
    });
  }
  return rows;
}

function portfolioDetailSummary(portfolio, positions) {
  if (portfolio.greeks_total > 0) {
    return (portfolio.greeks_coverage || 0) >= portfolio.greeks_total ? "Greeks ready" : "Partial Greeks";
  }
  if ((positions.options || []).length === 0) {
    return "No option Greeks needed";
  }
  return "details";
}

function detailFact(fact) {
  const row = document.createElement("div");
  row.className = "detail-fact " + (fact.tone || "neutral");
  const label = document.createElement("span");
  label.textContent = labelize(fact.label);
  const title = document.createElement("b");
  title.textContent = cleanDetail(fact.title || "--");
  const body = document.createElement("p");
  body.textContent = cleanDetail(fact.body || "");
  row.append(label, title, body);
  return row;
}

function renderSourceBanners(snap) {
  const snapshotErrors = (snap.errors || []).filter((err) => err.source !== "market_quotes");
  const summary = snapshotIssueSummary(snapshotErrors, snap);
  setBanner("snapshotErrorBanner", "snapshotErrorText", summary.text, summary.title);
  $("bannerStack").hidden = snapshotErrors.length === 0;
}

function snapshotIssueSummary(errors, snap = {}) {
  if (!errors.length) return { text: "", title: "" };
  const sources = [...new Set(errors.map((err) => snapshotSourceLabel(err.source)).filter(Boolean))];
  const sourceText = humanList(sources, 3);
  const title = errors.map((err) => `${err.source}: ${err.message}`).join(" | ");
  const gateway = gatewayIssueText(snap);
  if (gateway) {
    return { text: gateway, title };
  }
  return {
    text: `${sourceText || "Data"} feed interrupted; showing last good snapshot.`,
    title,
  };
}

function gatewayIssueText(snap = {}) {
  const direct = String(snap.status?.last_error || "").trim();
  const source = direct || (snap.errors || []).map((err) => err.message).find((msg) => /client id .*already in use/i.test(String(msg || ""))) || "";
  if (!source) return "";
  let text = String(source)
    .replace(/^gateway_unavailable:\s*/i, "")
    .replace(/^ibkr connection unavailable:\s*/i, "")
    .replace(/^ibkr:\s*client id already in use:\s*/i, "")
    .trim();
  if (!/client id .*already in use/i.test(text)) return "";
  text = text.charAt(0).toUpperCase() + text.slice(1);
  if (!/[.!?]$/.test(text)) text += ".";
  return text;
}

function snapshotSourceLabel(source) {
  switch (String(source || "").toLowerCase()) {
    case "account":
      return "account";
    case "positions":
      return "positions";
    case "status":
      return "gateway status";
    case "calendar":
      return "market calendar";
    case "trading":
      return "trading status";
    case "canary":
      return "canary";
    case "regime":
      return "regime";
    default:
      return cleanDetail(source);
  }
}

function setBanner(bannerID, textID, text, title = "") {
  const banner = $(bannerID);
  if (!banner) return;
  banner.hidden = !text;
  const target = $(textID);
  target.textContent = text || "--";
  target.title = title || text || "";
}

function renderTopbar(snap) {
  const label = marketSessionLabel(currentMarketCalendar(snap));
  const line = $("connectionLine");
  const strip = document.querySelector(".market-strip");
  line.textContent = label.side || label.text || state.connectionText;
  line.classList.remove("market-open", "market-closed", "market-warn");
  strip?.classList.remove("market-open", "market-closed", "market-warn");
  if (label.tone) {
    line.classList.add(label.tone);
    strip?.classList.add(label.tone);
  }
  $("sessionPhase").textContent = label.phase;
  const marketDot = $("marketStateDot");
  if (marketDot) {
    const dotLabel = label.dotTitle || label.text || "Market session status";
    marketDot.setAttribute("aria-label", dotLabel);
    marketDot.title = dotLabel;
  }
}

function currentMarketCalendar(snap) {
  return state.marketCalendarOverride || snap.market_calendar;
}

function protectionMarketCalendar(proposal = {}) {
  const market = proposalMarketKey(proposal);
  const current = currentMarketCalendar(state.snapshot || {});
  if (marketCalendarMatches(current, market)) return current;
  if (Object.hasOwn(state.proposalMarketCalendars, market)) return state.proposalMarketCalendars[market];
  queueProposalMarketCalendarSync(market);
  return null;
}

function proposalMarketKey(proposal = {}) {
  const contract = proposal.contract || {};
  const explicit = String(contract.market || "").trim().toLowerCase();
  if (explicit) return explicit;
  const secType = String(contract.sec_type || proposal.sec_type || "").trim().toUpperCase();
  if (secType === "OPT") return "us-options";
  const primary = String(contract.primary_exchange || contract.primary_exch || contract.exchange || "").trim().toUpperCase();
  if (primary === "IBIS" || primary === "XETRA") return "de";
  return "us";
}

function proposalMarketLabel(market = "") {
  switch (String(market || "").toLowerCase()) {
    case "de":
      return "Xetra";
    case "us-options":
      return "US options";
    default:
      return "US market";
  }
}

function marketCalendarMatches(calendar, market = "") {
  if (!calendar) return false;
  const got = String(calendar.market || calendar.session?.market || "").toLowerCase();
  const want = String(market || "us").toLowerCase();
  const aliases = {
    us: ["us", "us_equity", "us-equity"],
    "us-options": ["us-options", "us_options", "us_option", "us-options"],
    de: ["de", "xetra", "de_xetra", "de-xetra"],
  };
  return (aliases[want] || [want]).includes(got);
}

function queueProposalMarketCalendarSync(market = "") {
  const key = String(market || "us").toLowerCase();
  if (Object.hasOwn(state.proposalMarketCalendars, key) || state.proposalMarketCalendarBusy[key]) return;
  state.proposalMarketCalendarBusy = { ...state.proposalMarketCalendarBusy, [key]: true };
  fetch(`/api/market-calendar?market=${encodeURIComponent(key)}`, { credentials: "include" })
    .then((res) => {
      if (!res.ok) throw new Error("market calendar unavailable");
      return res.json();
    })
    .then((calendar) => {
      state.proposalMarketCalendars = { ...state.proposalMarketCalendars, [key]: calendar };
    })
    .catch(() => {
      state.proposalMarketCalendars = { ...state.proposalMarketCalendars, [key]: null };
    })
    .finally(() => {
      const busy = { ...state.proposalMarketCalendarBusy };
      delete busy[key];
      state.proposalMarketCalendarBusy = busy;
      renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    });
}

function setupMarketSelect() {
  const select = $("marketSelect");
  if (!select) return;
  select.value = state.selectedMarket;
  select.addEventListener("change", () => {
    state.selectedMarket = select.value || "us";
    localStorage.setItem("ibkrSelectedMarket", state.selectedMarket);
    if (state.selectedMarket === "us") {
      state.marketCalendarOverride = null;
      renderTopbar(state.snapshot || {});
      return;
    }
    refreshSelectedMarketCalendar();
  });
}

async function refreshSelectedMarketCalendar() {
  const select = $("marketSelect");
  const market = state.selectedMarket || "us";
  if (select) select.disabled = true;
  try {
    const res = await fetch(`/api/market-calendar?market=${encodeURIComponent(market)}`, { credentials: "include" });
    if (!res.ok) throw new Error(await res.text());
    state.marketCalendarOverride = await res.json();
  } catch {
    state.marketCalendarOverride = null;
  } finally {
    if (select) select.disabled = false;
    renderTopbar(state.snapshot || {});
  }
}

function renderSyncStrip(snap) {
  const strip = $("syncStrip");
  if (!strip) return;
  const updatedAt = parseDate(snap.updated_at);
  if (!updatedAt) {
    strip.hidden = true;
    return;
  }

  const ageMinutes = Math.max(0, Math.floor((Date.now() - updatedAt.getTime()) / 60000));
  const sourceIssues = Object.values(snap.sources || {}).filter((meta) => meta?.error).length;
  const stateLabel = !state.connectionOK
    ? "syncing"
    : sourceIssues > 0
      ? "degraded"
      : ageMinutes >= 5
        ? "stale"
        : "live";
  $("syncStatusLabel").textContent = sourceIssues > 0 ? "Data gaps" : "Last sync:";
  $("syncStatusTime").textContent = `${shortTimeWithZone(snap.updated_at)} · ${state.connectionOK ? "SSE connected" : "SSE reconnecting"}`;
  $("syncStatusState").textContent = labelize(stateLabel);
  strip.hidden = false;
}

function marketSessionLabel(calendar) {
  const session = calendar?.session;
  if (!session) {
    return {
      text: state.connectionOK ? "Waiting for official market calendar" : "App connection offline",
      tone: state.connectionOK ? "market-warn" : "market-closed",
      phase: state.connectionOK ? "syncing" : "offline",
      countdownVerb: "opening in",
      countdown: "--",
      side: state.connectionOK ? "Calendar pending" : "Offline",
      dotTitle: state.connectionOK ? "Market calendar is loading" : "App stream is reconnecting",
    };
  }
  const now = Date.now();
  const stateText = String(session.state || "").toLowerCase();
  const reason = session.reason ? ` (${session.reason})` : "";
  const open = parseDate(session.open);
  const close = parseDate(session.close);
  const nextOpen = parseDate(session.next_open);
  if (session.is_open) {
    const timeLeft = countdownLabel(close);
    const phase = stateText === "early_close" ? "early close" : "open";
    return {
      text: session.reason || "Regular cash session",
      tone: "market-open",
      phase: marketStatusPhrase(phase, "closing in", timeLeft || "live"),
      countdownVerb: "closing in",
      countdown: timeLeft || "live",
      side: marketSessionNow(session),
      dotTitle: stateText === "early_close" ? "Selected market is open in an early-close session" : "Selected market is open",
    };
  }

  if (open && now < open.getTime()) {
    const untilOpen = countdownLabel(open);
    return {
      text: session.state === "early_close" ? session.reason || "Shortened session ahead" : "Regular cash session",
      tone: "market-warn",
      phase: marketStatusPhrase("pre-open", "opening in", untilOpen || "--"),
      countdownVerb: "opening in",
      countdown: untilOpen || "--:--:--",
      side: marketSessionNow(session),
      dotTitle: "Selected market is pre-open",
    };
  }

  if (close && nextOpen && now >= close.getTime()) {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Next regular cash session",
      tone: "market-closed",
      phase: marketStatusPhrase(stateText === "early_close" ? "after early close" : "after close", "opening in", untilOpen || "--"),
      countdownVerb: "opening in",
      countdown: untilOpen || "--:--:--",
      side: marketSessionNow(session),
      dotTitle: stateText === "early_close" ? "Selected market has closed after an early-close session" : "Selected market is closed",
    };
  }

  if (stateText === "holiday") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Official market holiday",
      tone: "market-closed",
      phase: marketStatusPhrase("holiday", "opening in", untilOpen || "--"),
      countdownVerb: "opening in",
      countdown: untilOpen || "--:--:--",
      side: marketSessionNow(session),
      dotTitle: "Selected market is closed for a holiday",
    };
  }

  if (stateText === "closed") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason === "weekend" ? "Weekend closure" : `Outside regular cash session${reason}`,
      tone: "market-closed",
      phase: marketStatusPhrase(session.reason === "weekend" ? "weekend" : "closed", "opening in", untilOpen || "--"),
      countdownVerb: "opening in",
      countdown: untilOpen || "--:--:--",
      side: marketSessionNow(session),
      dotTitle: session.reason === "weekend" ? "Selected market is closed for the weekend" : "Selected market is closed",
    };
  }

  if (stateText === "unknown") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: `Calendar coverage unavailable${reason}`,
      tone: "market-warn",
      phase: marketStatusPhrase("unknown", "opening in", untilOpen || "--"),
      countdownVerb: "opening in",
      countdown: untilOpen || "--:--:--",
      side: marketSessionNow(session),
      dotTitle: "Selected market calendar status is unknown",
    };
  }

  const untilOpen = countdownLabel(nextOpen);
  return {
    text: session.reason || `Official calendar${reason}`,
    tone: "market-warn",
    phase: marketStatusPhrase(cleanDetail(session.state), "opening in", untilOpen || "--"),
    countdownVerb: "opening in",
    countdown: untilOpen || "--:--:--",
    side: marketSessionNow(session),
    dotTitle: "Selected market calendar status needs attention",
  };
}

function marketStatusPhrase(phase, verb, countdown) {
  return [phase, `${verb} ${countdown || "--"}`].filter(Boolean).join(" · ");
}

function marketSessionNow(session) {
  const formatted = new Date().toLocaleString("en-US", {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZoneName: "short",
    timeZone: session?.timezone || undefined,
  }).replaceAll(",", "");
  const parts = formatted.split(/\s+/).filter(Boolean);
  if (parts.length >= 5) {
    return `${parts[1]} ${parts[0].toUpperCase()} ${parts[2]} ${parts[3]} ${parts[4]}`;
  }
  return formatted.toUpperCase();
}

function countdownLabel(target) {
  if (!target) return "";
  const ms = target.getTime() - Date.now();
  if (ms <= 0) return "";
  const totalSeconds = Math.ceil(ms / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  const clock = [hours, minutes, seconds].map((value, index) => index === 0 ? String(value) : String(value).padStart(2, "0")).join(":");
  return days > 0 ? `${days}d ${clock}` : clock;
}

function greeksCoverage(portfolio, positions) {
  if (portfolio.greeks_total > 0) {
    return `${portfolio.greeks_coverage || 0} of ${portfolio.greeks_total}`;
  }
  if ((positions.options || []).length === 0) {
    return "No options";
  }
  return "--";
}

function greeksMeaning(portfolio, positions) {
  const total = portfolio.greeks_total || 0;
  const covered = portfolio.greeks_coverage || 0;
  if (total > 0 && covered >= total) {
    return "All option legs have model Greeks for risk totals.";
  }
  if (total > 0) {
    return "Some option legs are missing model Greeks; totals are partial.";
  }
  if ((positions.options || []).length === 0) {
    return "No option legs need model Greeks in this snapshot.";
  }
  return "Model Greeks unavailable for this option snapshot.";
}

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
  const at = document.createElement("span");
  at.className = "alert-row__source";
  at.textContent = alertSourceLabel(alert);
  at.title = alertSourceTitle(alert);
  row.append(text, at);
  return row;
}

function alertSourceLabel(alert) {
  if (alert.preview) return "current signal";
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

function renderOpenOrders() {
  const panel = $("ordersPanel");
  const list = $("ordersOpenList");
  const orders = state.ordersOpen?.orders || [];
  if (panel) panel.hidden = orders.length === 0;
  renderFreshnessTimestamp("ordersAsOf", state.ordersOpen?.as_of, { staleMinutes: 15, fallback: "--" });
  if (orders.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No open orders available for this view.";
    list.replaceChildren(empty);
    return;
  }
  list.replaceChildren(...orders.map(openOrderRowElement));
}

function openOrderRowElement(order) {
  const row = document.createElement("div");
  row.className = "open-order-row";
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const modifyGate = orderModifyGate(order, trading);
  const cancelGate = orderCancelGate(order, trading);

  const main = document.createElement("div");
  main.className = "open-order-row__main";
  const title = document.createElement("b");
  title.textContent = `${order.action || "--"} ${order.quantity || "--"} ${order.symbol || order.order_ref || "--"}`;
  const meta = document.createElement("span");
  meta.textContent = [
    order.lifecycle_status,
    order.send_state,
    order.order_ref,
    order.account,
    order.endpoint,
  ].filter(Boolean).join(" / ") || "journal view";
  main.append(title, meta);

  const editBox = document.createElement("div");
  editBox.className = "open-order-row__edit";

  const qty = document.createElement("input");
  qty.type = "number";
  qty.min = "1";
  qty.max = String(orderReductionMax(order) || 1);
  qty.step = "1";
  qty.value = String(edit.quantity || orderReductionMax(order) || 1);
  qty.setAttribute("aria-label", `Reduction quantity for ${order.symbol || id}`);
  qty.disabled = !modifyGate.ready || edit.busy;
  qty.addEventListener("change", () => {
    const maxQty = orderReductionMax(order) || 1;
    edit.quantity = Math.min(maxQty, Math.max(1, Math.trunc(Number(qty.value || 1))));
    edit.preview = null;
    edit.result = null;
    edit.error = "";
    renderOpenOrders();
  });

  const priceInputs = orderIsTrail(order)
    ? [
        order.trail?.trailing_amount > 0
          ? orderEditNumberInput(order, edit, modifyGate, "trailing_amount", "Trailing amount", "Trail amt")
          : orderEditNumberInput(order, edit, modifyGate, "trailing_percent", "Trailing percent", "Trail %"),
        orderEditNumberInput(order, edit, modifyGate, "initial_stop", "Initial stop price", "Stop"),
        ...(String(order.order_type || "").toUpperCase() === "TRAIL LIMIT"
          ? [orderEditNumberInput(order, edit, modifyGate, "limit_offset", "Limit offset", "Offset")]
          : []),
      ]
    : [orderEditNumberInput(order, edit, modifyGate, "limit_price", "Limit price", "Limit")];

  const fixed = document.createElement("span");
  fixed.className = "open-order-row__fixed";
  fixed.textContent = `${order.order_type || "LMT"} / ${order.tif || "DAY"} / ${order.action || "--"}`;
  editBox.append(qty, ...priceInputs, fixed);

  const controls = document.createElement("div");
  controls.className = "open-order-row__controls";
  const previewButton = orderActionButton("Preview change", modifyGate.ready && !edit.busy, modifyGate.reason);
  previewButton.addEventListener("click", () => previewOrderModify(order));
  const applyButton = orderActionButton("Apply change", modifyGate.ready && modifyPreviewReady(edit.preview) && !edit.busy, modifyApplyDisabledReason(modifyGate, edit.preview));
  applyButton.addEventListener("click", () => applyOrderModify(order));
  const cancelButton = orderActionButton("Cancel order", cancelGate.ready && !edit.busy, cancelGate.reason);
  cancelButton.addEventListener("click", () => cancelOpenOrder(order));
  controls.append(previewButton, applyButton, cancelButton);

  const status = document.createElement("small");
  status.className = "open-order-row__status";
  status.textContent = openOrderStatusLine(order, edit, modifyGate, cancelGate);

  row.append(main, editBox, controls, status);
  return row;
}

function orderEditNumberInput(order, edit, modifyGate, field, label, placeholder) {
  const input = document.createElement("input");
  input.type = "number";
  input.min = "0";
  input.step = "0.01";
  input.value = typeof edit[field] === "number" ? String(edit[field]) : "";
  input.placeholder = placeholder;
  input.setAttribute("aria-label", `${label} for ${order.symbol || orderIdentity(order)}`);
  input.disabled = !modifyGate.ready || edit.busy;
  input.addEventListener("change", () => {
    const next = Number(input.value || 0);
    edit[field] = Number.isFinite(next) && next > 0 ? next : null;
    edit.preview = null;
    edit.result = null;
    edit.error = "";
    renderOpenOrders();
  });
  return input;
}

function orderActionButton(label, enabled, reason) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "text-button";
  button.textContent = label;
  button.disabled = !enabled;
  button.title = enabled ? label : reason || `${label} unavailable`;
  return button;
}

async function previewOrderModify(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderModifyGate(order, trading);
  if (!gate.ready) {
    edit.error = gate.reason;
    renderOpenOrders();
    return;
  }
  edit.busy = "preview";
  edit.error = "";
  edit.result = null;
  edit.cancelResult = null;
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/preview-modify`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(modifyPreviewBody(order, edit)),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.preview = body;
  } catch (err) {
    edit.preview = null;
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function applyOrderModify(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderModifyGate(order, trading);
  if (!gate.ready || !modifyPreviewReady(edit.preview)) {
    edit.error = modifyApplyDisabledReason(gate, edit.preview);
    renderOpenOrders();
    return;
  }
  const modifyConfirmation = protectionWriteConfirmation();
  if (!modifyConfirmation) {
    edit.error = "Trading account/mode unavailable; cannot confirm broker write.";
    renderOpenOrders();
    return;
  }
  edit.busy = "modify";
  edit.error = "";
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/modify`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        preview_token: edit.preview.preview_token,
        confirm_account: modifyConfirmation.account,
        confirm_mode: modifyConfirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.result = body;
    edit.preview = null;
    await refreshOpenOrders();
  } catch (err) {
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function cancelOpenOrder(order) {
  const id = orderIdentity(order);
  const edit = openOrderEdit(order);
  const trading = state.snapshot?.trading || {};
  const gate = orderCancelGate(order, trading);
  if (!gate.ready) {
    edit.error = gate.reason;
    renderOpenOrders();
    return;
  }
  const cancelConfirmation = protectionWriteConfirmation();
  if (!cancelConfirmation) {
    edit.error = "Trading account/mode unavailable; cannot confirm broker write.";
    renderOpenOrders();
    return;
  }
  edit.busy = "cancel";
  edit.error = "";
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        confirm_account: cancelConfirmation.account,
        confirm_mode: cancelConfirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    edit.cancelResult = body;
    await refreshOpenOrders();
  } catch (err) {
    edit.error = err.message;
  } finally {
    edit.busy = "";
    renderOpenOrders();
  }
}

async function refreshOpenOrders() {
  try {
    const res = await fetch("/api/orders/open", { credentials: "include" });
    if (!res.ok) return;
    state.ordersOpen = await res.json();
    renderOpenOrders();
  } catch {
    // Open orders are read-only context; the live snapshot remains primary.
  }
}

function orderIdentity(order) {
  return String(order.order_ref || order.reserved_order_id || order.perm_id || order.preview_token_id || order.symbol || "").trim();
}

function openOrderEdit(order) {
  const id = orderIdentity(order);
  if (!state.openOrderEdits[id]) {
    state.openOrderEdits[id] = {
      quantity: orderReductionMax(order) || 1,
      limit_price: order.limit_price > 0 ? order.limit_price : null,
      trailing_percent: order.trail?.trailing_percent > 0 ? order.trail.trailing_percent : null,
      trailing_amount: order.trail?.trailing_amount > 0 ? order.trail.trailing_amount : null,
      initial_stop: order.trail?.initial_stop_price > 0 ? order.trail.initial_stop_price : null,
      limit_offset: order.trail?.limit_offset > 0 ? order.trail.limit_offset : null,
      preview: null,
      result: null,
      cancelResult: null,
      error: "",
      busy: "",
    };
  }
  return state.openOrderEdits[id];
}

function orderReductionMax(order) {
  const remaining = Number(order.remaining || 0);
  const quantity = Number(order.quantity || 0);
  return Math.max(0, Math.floor(remaining > 0 ? remaining : quantity));
}

function orderModifyGate(order, trading) {
  if (!orderIdentity(order)) return { ready: false, reason: "Order id unavailable" };
  if (!trading.can_write) return { ready: false, reason: "Broker writes are not enabled by trading.status" };
  if ("modify_eligible" in order && order.modify_eligible !== true) return { ready: false, reason: "This order is not modify eligible" };
  if (order.open === false) return { ready: false, reason: "Only open orders can be modified" };
  const orderType = String(order.order_type || "LMT").toUpperCase();
  if (orderType !== "LMT" && orderType !== "TRAIL" && orderType !== "TRAIL LIMIT") {
    return { ready: false, reason: "Canary mitigation UI supports LMT, TRAIL, and TRAIL LIMIT changes" };
  }
  if (orderReductionMax(order) <= 0) return { ready: false, reason: "No remaining quantity available to reduce" };
  if (orderIsTrail(order)) return { ready: true, reason: "Preview a reduction-only quantity or trail re-price; the broker order ID is kept" };
  return { ready: true, reason: "Preview a reduction-only quantity or LMT price change" };
}

function orderIsTrail(order) {
  const orderType = String(order.order_type || "").toUpperCase();
  return orderType === "TRAIL" || orderType === "TRAIL LIMIT";
}

function orderCancelGate(order, trading) {
  if (!orderIdentity(order)) return { ready: false, reason: "Order id unavailable" };
  if (!trading.can_write) return { ready: false, reason: "Broker writes are not enabled by trading.status" };
  if ("cancel_eligible" in order && order.cancel_eligible !== true) return { ready: false, reason: "This order is not cancel eligible" };
  if (order.open === false) return { ready: false, reason: "Only open orders can be cancelled" };
  return { ready: true, reason: "Cancel this journal-backed open order after confirmation" };
}

function modifyPreviewBody(order, edit) {
  const body = {
    action: order.action || "",
    quantity: Math.min(orderReductionMax(order) || 1, Math.max(1, Math.trunc(Number(edit.quantity || 1)))),
    order_type: order.order_type || "LMT",
    tif: order.tif || "DAY",
  };
  if (orderIsTrail(order)) {
    const trail = {};
    if (edit.trailing_amount > 0) trail.trailing_amount = edit.trailing_amount;
    else if (edit.trailing_percent > 0) trail.trailing_percent = edit.trailing_percent;
    if (edit.initial_stop > 0) trail.initial_stop_price = edit.initial_stop;
    if (String(order.order_type || "").toUpperCase() === "TRAIL LIMIT" && edit.limit_offset > 0) trail.limit_offset = edit.limit_offset;
    body.trail = trail;
  } else {
    const limit = Number(edit.limit_price || 0);
    body.limit_price = Number.isFinite(limit) && limit > 0 ? limit : undefined;
  }
  return body;
}

function modifyPreviewReady(preview) {
  return Boolean(preview?.submit_eligible && previewToken(preview));
}

function modifyApplyDisabledReason(gate, preview) {
  if (!gate.ready) return gate.reason;
  if (!preview) return "Preview change first";
  if (!preview.submit_eligible) return "Modify preview is not submit eligible";
  if (!previewToken(preview)) return "Modify preview did not mint a preview token";
  return "Apply previewed change after confirmation";
}

function openOrderStatusLine(order, edit, modifyGate, cancelGate) {
  if (edit.busy === "preview") return "Previewing change.";
  if (edit.busy === "modify") return "Applying previewed change.";
  if (edit.busy === "cancel") return "Cancelling order.";
  if (edit.error) return edit.error;
  if (edit.result) return `Modify result: ${edit.result.accepted ? "accepted" : "not accepted"}${edit.result.message ? " / " + edit.result.message : ""}`;
  if (edit.cancelResult) return `Cancel result: ${edit.cancelResult.accepted ? "accepted" : "not accepted"}${edit.cancelResult.message ? " / " + edit.cancelResult.message : ""}`;
  if (edit.preview) return modifyPreviewLine(edit.preview);
  const reasons = [modifyGate.ready ? "" : modifyGate.reason, cancelGate.ready ? "" : cancelGate.reason].filter(Boolean);
  return reasons.length ? reasons.join(" / ") : `Open ${order.action || "--"} ${order.quantity || "--"} ${order.symbol || "--"}`;
}

function modifyPreviewLine(preview) {
  const parts = [
    preview.submit_eligible ? "submit eligible" : "not submit eligible",
    preview.what_if?.status ? `WhatIf ${preview.what_if.status}` : "",
    preview.preview_token_id ? `token ${preview.preview_token_id}` : "no submit token",
    preview.what_if?.message || "",
    warningMessages(preview.warnings).join(" / "),
  ].filter(Boolean);
  return "Preview change: " + parts.join(" / ");
}

function previewToken(preview) {
  return String(preview?.preview_token || "").trim();
}

function readJSONOrText(res) {
  return res.text().then((text) => {
    if (!text) return {};
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  });
}

async function refreshPurgeStatus() {
  try {
    const res = await fetch("/api/purge/status", { credentials: "include" });
    if (!res.ok) return;
    state.latestPurgeStatus = await res.json();
    renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  } catch {
    // Purge status is secondary context; live positions and trading remain primary.
  }
}

async function runUnderlyingAction(action, target = {}) {
  const all = Boolean(target.all);
  const symbols = (target.symbols || []).map(normalizeSymbol).filter(Boolean);
  const label = underlyingActionLabel(action, all, symbols);
  const body = { all, symbols };
  const writeAction = action === "purge" || action === "restore";
  if (writeAction) {
    const confirmation = underlyingWriteConfirmation(action, label);
    if (!confirmation) return;
    body.confirm_account = confirmation.account;
    body.confirm_mode = confirmation.mode;
  }

  state.underlyingBusy = action;
  state.underlyingNotice = `${label}: running.`;
  renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  try {
    const res = await fetch(underlyingActionEndpoint(action), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(body),
    });
    const result = await readJSONOrText(res);
    if (!res.ok) throw new Error(result.error || result.message || String(result));
    state.underlyingNotice = `${label}: ${purgeResultSummary(result)}`;
    renderUnderlyingActionResult(result);
    await refreshPurgeStatus();
    await refreshOpenOrders();
  } catch (err) {
    state.underlyingNotice = `${label}: ${err.message}`;
    renderUnderlyingActionResult({ status: "error", message: err.message });
  } finally {
    state.underlyingBusy = "";
    renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
  }
}

function underlyingActionEndpoint(action) {
  if (action === "build") return "/api/purge/restore/preview";
  if (action === "restore") return "/api/purge/restore/execute";
  return "/api/purge/execute";
}

function underlyingActionLabel(action, all, symbols) {
  const target = all ? "all" : symbols.join(", ") || "selection";
  if (action === "build") return `Build ${target}`;
  if (action === "restore") return `Restore ${target}`;
  return `Purge ${target}`;
}

function underlyingWriteConfirmation(action, label) {
  const trading = state.snapshot?.trading || {};
  if (!canWriteUnderlyings(trading)) {
    state.underlyingNotice = underlyingWriteReason(label, true, trading);
    renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
    return null;
  }
  const expected = `${trading.mode}/${trading.account}`;
  const verb = action === "restore" ? "restore purged rows" : "purge held positions";
  const got = window.prompt([
    `${label} is a broker-write action.`,
    `Type ${expected} to ${verb}.`,
  ].join("\n"));
  if (got !== expected) {
    state.underlyingNotice = `${label}: confirmation cancelled.`;
    renderUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
    return null;
  }
  return { account: trading.account, mode: trading.mode };
}

function purgeResultSummary(result = {}) {
  const status = result.status || "ok";
  const selected = Number(result.selected_legs || 0);
  const submitted = Number(result.submitted_legs || 0);
  const skipped = Number(result.skipped_legs || 0);
  const errors = Number(result.error_legs || 0);
  const message = result.message ? ` / ${result.message}` : "";
  const preview = result.kind === "ibkr.purge_restore_preview" ? "draft" : status;
  return `${preview}; ${selected} selected, ${submitted} submitted, ${skipped} skipped, ${errors} errors${message}`;
}

function renderUnderlyingActionResult(result = {}) {
  const panel = $("underlyingActionResult");
  if (!panel) return;
  panel.hidden = false;
  panel.className = "underlying-action-result " + (result.status === "error" || result.error_legs > 0 ? "risk" : "neutral");
  const lines = [];
  if (result.message) lines.push(result.message);
  if ((result.blockers || []).length > 0) {
    lines.push(...result.blockers.map((blocker) => blocker.message || blocker.code).filter(Boolean));
  }
  if ((result.skipped || []).length > 0) {
    lines.push(...result.skipped.slice(0, 3).map((row) => `${row.symbol || row.leg_id}: ${row.reason}`));
  }
  panel.textContent = lines.join(" / ") || purgeResultSummary(result);
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
$("protectionToggle").addEventListener("click", () => {
  setProtectionExpansion(!state.protectionOpen);
});
$("protectionPanel").addEventListener("click", (event) => handleProtectionPanelTap(event));
$("quickReviewBlockersButton").addEventListener("click", () => {
  setRegimeCanaryExpansion("canary", true);
  $("canaryDetailPanel").scrollIntoView({ block: "nearest" });
});
  $("quickHeldActionsButton").addEventListener("click", () => {
  setUnderlyingExpansion(true);
  $("underlyingPanel").scrollIntoView({ block: "nearest" });
});
$("quickAlertsButton").addEventListener("click", () => {
  setActiveTab("alerts");
});
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
  $("regimeDetailToggle").addEventListener("click", () => {
  setRegimeCanaryExpansion("regime", !state.regimeDetailOpen);
});
$("regimePanel").addEventListener("click", (event) => {
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

async function sign(privateKey, value) {
  if (!hasWebCrypto()) {
    throw new Error("WebCrypto is unavailable on this origin");
  }
  const sig = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" },
    privateKey,
    new TextEncoder().encode(value)
  );
  return bytesToB64url(new Uint8Array(sig));
}

function hasWebCrypto() {
  return !!globalThis.crypto?.subtle;
}

function randomDeviceSecret() {
  const bytes = new Uint8Array(32);
  if (!globalThis.crypto?.getRandomValues) {
    throw new Error("secure random is unavailable in this browser");
  }
  globalThis.crypto.getRandomValues(bytes);
  return bytesToB64url(bytes);
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

async function savePrivateKey(key) {
  const db = await openDB();
  return new Promise((resolve, reject) => {
    const tx = db.transaction("keys", "readwrite");
    tx.objectStore("keys").put(key, "device");
    tx.oncomplete = resolve;
    tx.onerror = () => reject(tx.error);
  });
}

async function loadPrivateKey() {
  const db = await openDB();
  return new Promise((resolve) => {
    const tx = db.transaction("keys", "readonly");
    const req = tx.objectStore("keys").get("device");
    req.onsuccess = () => resolve(req.result || null);
    req.onerror = () => resolve(null);
  });
}

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open("ibkr-app", 1);
    req.onupgradeneeded = () => req.result.createObjectStore("keys");
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function b64urlToBytes(input) {
  const pad = "=".repeat((4 - (input.length % 4)) % 4);
  const raw = atob((input + pad).replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

function bytesToB64url(bytes) {
  let raw = "";
  bytes.forEach((b) => raw += String.fromCharCode(b));
  return btoa(raw).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function money(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return new Intl.NumberFormat(undefined, { style: "currency", currency: currency || "USD" }).format(value);
}

function compactMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const abs = Math.abs(value);
  if (abs >= 1000000) {
    return `${currency || "USD"} ${(value / 1000000).toFixed(abs >= 10000000 ? 1 : 2)}m`;
  }
  if (abs >= 100000) {
    return `${currency || "USD"} ${(value / 1000).toFixed(0)}k`;
  }
  if (abs >= 10000) {
    return `${currency || "USD"} ${(value / 1000).toFixed(1)}k`;
  }
  return money(value, currency);
}

function renderSignedMoney(id, value, currency) {
  const el = $(id);
  el.className = signedClass(value);
  el.textContent = hasNumericValue(value) ? money(value, currency) : "--";
}

function renderSensitiveSignedMoney(id, value, currency) {
  const el = $(id);
  if (!hasNumericValue(value)) {
    el.className = "signed";
    el.textContent = "--";
    return;
  }
  if (sensitiveMoneyHidden(value)) {
    el.className = "signed is-private";
    el.textContent = privacyMask();
    return;
  }
  el.className = signedClass(value);
  el.textContent = money(value, currency);
}

function renderSensitiveText(id, value, hasValue) {
  const el = $(id);
  if (!hasValue) {
    el.classList.remove("is-private");
    el.textContent = "--";
    return;
  }
  if (!state.accountValueVisible) {
    el.classList.add("is-private");
    el.textContent = privacyMask();
    return;
  }
  el.classList.remove("is-private");
  el.textContent = value;
}

function sensitiveMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? money(value, currency) : privacyMask();
}

function sensitiveDisplayMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? displayMoney(value, currency) : privacyMask();
}

function sensitiveMoneyHidden(value) {
  return hasNumericValue(value) && !state.accountValueVisible;
}

function privacyMask() {
  return "******";
}

function hasNumericValue(value) {
  return Number.isFinite(value);
}

function firstNumber(...values) {
  return values.find((value) => typeof value === "number");
}

function signedClass(value) {
  if (!hasNumericValue(value) || value === 0) return "signed";
  return "signed " + (value > 0 ? "ok" : "risk");
}

function signedTone(value, inverse = false) {
  if (!hasNumericValue(value) || value === 0) return "neutral";
  const good = inverse ? value > 0 : value >= 0;
  return good ? "ok" : "risk";
}

function numberRead(value) {
  if (!hasNumericValue(value)) return "--";
  return new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}

function riskMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const amount = new Intl.NumberFormat(undefined, {
    maximumFractionDigits: 0,
    minimumFractionDigits: 0,
  }).format(value);
  return `${amount} ${currency || "USD"}`;
}

function pct(value) {
  if (!hasNumericValue(value)) return "--";
  return value.toFixed(1) + "%";
}

function wholePct(value) {
  if (!hasNumericValue(value)) return "--";
  return Math.round(value) + "%";
}

function signedPct(value) {
  if (!hasNumericValue(value)) return "--";
  const sign = value > 0 ? "+" : "";
  return sign + value.toFixed(2) + "%";
}

function cleanDetail(value) {
  if (!value) return "--";
  return String(value).replaceAll("_", " ");
}

function labelize(value) {
  const words = cleanDetail(value).split(/\s+/).filter(Boolean);
  if (words.length === 0) return "--";
  return words.map((word) => word.charAt(0).toUpperCase() + word.slice(1)).join(" ");
}

function ageLabel(minutes) {
  if (minutes < 1) return "now";
  if (minutes < 60) return `${minutes}m old`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest === 0 ? `${hours}h old` : `${hours}h ${rest}m old`;
}

function renderFreshnessTimestamp(target, value, options = {}) {
  const el = typeof target === "string" ? $(target) : target;
  if (!el) return;
  const at = value instanceof Date ? value : parseDate(value);
  if (!at) {
    el.textContent = options.fallback || "no timestamp";
    el.classList.add("stale");
    return;
  }
  const ageMS = Date.now() - at.getTime();
  const ageMinutes = Math.max(0, Math.floor(ageMS / 60000));
  const staleMinutes = typeof options.staleMinutes === "number" ? options.staleMinutes : 15;
  const stale = ageMinutes >= staleMinutes;
  el.textContent = `${stale ? "stale" : "updated"} ${shortTime(at.toISOString())} · ${ageLabel(ageMinutes)}`;
  el.classList.toggle("stale", stale);
}

function parseDate(value) {
  if (!value) return null;
  const at = new Date(value);
  return Number.isNaN(at.getTime()) ? null : at;
}

function shortTime(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function shortTimeWithZone(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  });
}

function setConnection(text, ok) {
  state.connectionText = text;
  state.connectionOK = ok;
  $("statusDot").className = "status-dot " + (ok ? "ok" : "risk");
  const statusLabel = ok ? "App data stream connected" : "App data stream reconnecting";
  $("statusDot").setAttribute("aria-label", statusLabel);
  $("statusDot").title = statusLabel;
  renderTopbar(state.snapshot || {});
}

function showPairing(text) {
  $("pairingPanel").hidden = false;
  $("tabPanels").hidden = true;
  $("bottomTabs").hidden = true;
  $("accountPanel").hidden = true;
  $("bannerStack").hidden = true;
  $("syncStrip").hidden = true;
  $("pairingText").textContent = text;
  setConnection("Locked", false);
}

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
