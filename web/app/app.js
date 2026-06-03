const state = {
  snapshot: null,
  alertSettings: { mode: "watch_and_act" },
  alerts: [],
  vapidPublicKey: "",
  eventSource: null,
  reconnectTimer: null,
  accountValueVisible: localStorage.getItem("ibkrAccountValueVisible") === "true",
  canaryDetailOpen: false,
};

const $ = (id) => document.getElementById(id);

async function main() {
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
  state.alertSettings = data.alert_settings || state.alertSettings;
  state.alerts = data.alerts || [];
  state.vapidPublicKey = data.vapid_public_key || "";
  $("pairingPanel").hidden = true;
  $("dashboard").hidden = false;
  $("alertsPanel").hidden = false;
  $("toolsPanel").hidden = false;
  setConnection("Connected", true);
  renderAll();
  connectEvents();
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
  state.eventSource?.close();
  const es = new EventSource("/api/events", { withCredentials: true });
  state.eventSource = es;
  for (const type of ["snapshot", "status", "account", "positions", "canary"]) {
    es.addEventListener(type, (event) => {
      const data = JSON.parse(event.data);
      if (type === "snapshot") state.snapshot = data;
      if (type !== "snapshot") state.snapshot = { ...(state.snapshot || {}), [type]: data };
      setConnection("Live", true);
      renderAll();
    });
  }
  es.onerror = () => scheduleEventRecovery();
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
  renderAccountValue(account);
  $("dailyPnl").textContent = account.daily_pnl == null ? "--" : money(account.daily_pnl, account.base_currency);
  $("cushion").textContent = typeof account.cushion === "number" ? pct(account.cushion * 100) : "--";
  $("accountAsOf").textContent = shortTime(account.as_of);
  $("positionsAsOf").textContent = shortTime(positions.as_of);
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = canary.severity || "--";
  $("canaryAction").textContent = (canary.action || "--").replaceAll("_", " ");
  $("canarySummary").textContent = canary.summary || "Waiting for canary snapshot.";
  renderMarketContext(canary);
  renderCanaryDetail(canary);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
}

function renderAccountValue(account) {
  const hasValue = typeof account.net_liquidation === "number";
  const value = $("netLiquidation");
  value.textContent = state.accountValueVisible || !hasValue
    ? money(account.net_liquidation, account.base_currency)
    : "******";
  value.classList.toggle("is-private", !state.accountValueVisible && hasValue);

  const button = $("accountPrivacyToggle");
  button.classList.toggle("is-visible", state.accountValueVisible);
  button.setAttribute("aria-pressed", String(state.accountValueVisible));
  const label = state.accountValueVisible ? "Hide net liquidation" : "Show net liquidation";
  button.setAttribute("aria-label", label);
  button.title = label;
}

function renderCanaryDetail(canary) {
  const panel = $("canaryDetailPanel");
  const button = $("canaryDetailToggle");
  panel.hidden = !state.canaryDetailOpen;
  button.textContent = state.canaryDetailOpen ? "Hide detail" : "Show detail";
  button.setAttribute("aria-expanded", String(state.canaryDetailOpen));
  if (!state.canaryDetailOpen) return;

  const detailItems = [
    ["Market", cleanDetail(canary.market_confirmation || canary.market?.regime_verdict)],
    ["Portfolio", cleanDetail(canary.portfolio_fit || canary.portfolio?.largest_exposure)],
    ["Inputs", cleanDetail(canary.input_health)],
    ["Readiness", cleanDetail(canary.planner_readiness || canary.planner_mode_hint)],
  ];
  $("canaryDetailGrid").replaceChildren(...detailItems.map(([label, value]) => detailCard(label, value)));

  const rows = (canary.rows || []).slice(0, 3);
  $("canaryDrivers").replaceChildren(...rows.map((row) => {
    const item = document.createElement("div");
    item.className = "driver-row";
    const label = document.createElement("span");
    label.textContent = row.severity ? String(row.severity).replaceAll("_", " ") : "driver";
    const title = document.createElement("b");
    title.textContent = row.title || "Canary driver";
    const body = document.createElement("p");
    body.textContent = [row.guidance, row.evidence].filter(Boolean).join(" ");
    item.append(label, title, body);
    return item;
  }));
}

function renderMarketContext(canary) {
  const market = canary.market || {};
  $("marketAsOf").textContent = shortTime(canary.as_of);
  renderSignedPercent("spyChange", market.spy_change_pct, false);
  renderSignedPercent("vixChange", market.vix_change_pct, true);
  $("marketRegime").textContent = cleanDetail(market.regime_verdict);
}

function renderSignedPercent(id, value, positiveIsRisk) {
  const el = $(id);
  el.classList.remove("ok", "risk");
  if (typeof value !== "number") {
    el.textContent = "--";
    return;
  }
  el.textContent = signedPct(value);
  const isRisk = positiveIsRisk ? value > 0 : value < 0;
  const isOk = positiveIsRisk ? value < 0 : value > 0;
  if (isRisk) el.classList.add("risk");
  if (isOk) el.classList.add("ok");
}

function detailCard(label, value) {
  const item = document.createElement("div");
  const labelEl = document.createElement("span");
  labelEl.textContent = label;
  const valueEl = document.createElement("b");
  valueEl.textContent = value || "--";
  item.append(labelEl, valueEl);
  return item;
}

function renderPortfolioRisk(positions, account) {
  const portfolio = positions.portfolio || {};
  const baseCurrency = portfolio.base_currency || account.base_currency || "USD";
  $("portfolioDollarDelta").textContent = money(
    portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
    portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
  );
  $("portfolioDailyTheta").textContent = money(
    portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
    portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
  );
  $("portfolioGreeksCoverage").textContent = greeksCoverage(portfolio, positions);
  $("portfolioFxSensitivity").textContent = money(
    portfolio.fx_sensitivity_per_pct,
    portfolio.fx_base_currency || baseCurrency,
  );

  const exposures = (portfolio.exposure_base || []).slice(0, 3);
  const list = $("portfolioExposureList");
  list.hidden = exposures.length === 0;
  list.replaceChildren(...exposures.map((exposure) => {
    const row = document.createElement("div");
    row.className = "metric-row";
    const label = document.createElement("span");
    const pctText = typeof exposure.market_value_pct_nlv === "number" ? ` ${pct(exposure.market_value_pct_nlv)}` : "";
    label.textContent = exposure.underlying + pctText;
    const value = document.createElement("b");
    value.textContent = money(exposure.market_value_base, exposure.base_currency || baseCurrency);
    row.append(label, value);
    return row;
  }));
}

function renderSourceBanners(snap) {
  const sourceErrors = Object.entries(snap.sources || {})
    .filter(([, meta]) => meta?.error)
    .map(([source, meta]) => `${source}: ${meta.error}`);
  setBanner("sourceErrorBanner", "sourceErrorText", sourceErrors.join(" | "));

  const snapshotErrors = (snap.errors || []).map((err) => `${err.source}: ${err.message}`);
  setBanner("snapshotErrorBanner", "snapshotErrorText", snapshotErrors.join(" | "));
}

function setBanner(bannerID, textID, text) {
  const banner = $(bannerID);
  if (!banner) return;
  banner.hidden = !text;
  $(textID).textContent = text || "--";
}

function greeksCoverage(portfolio, positions) {
  if (portfolio.greeks_total > 0) {
    return `${portfolio.greeks_coverage || 0}/${portfolio.greeks_total}`;
  }
  if ((positions.options || []).length === 0) {
    return "none";
  }
  return "--";
}

function renderAlertMode() {
  document.querySelectorAll("#alertSegments button").forEach((button) => {
    button.classList.toggle("active", button.dataset.mode === state.alertSettings.mode);
  });
  $("pushState").textContent = notificationStateLabel();
}

function renderAlerts() {
  $("alertCount").textContent = state.alerts.length;
  $("alertsList").replaceChildren(...state.alerts.map((alert) => {
    const row = document.createElement("div");
    row.className = "alert-row";
    const text = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = alert.title;
    const body = document.createElement("p");
    body.textContent = alert.body;
    text.append(title, body);
    const at = document.createElement("span");
    at.textContent = shortTime(alert.created_at);
    row.append(text, at);
    return row;
  }));
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

$("enablePushButton").addEventListener("click", enablePush);
$("retryAuthButton").addEventListener("click", bootstrap);
$("accountPrivacyToggle").addEventListener("click", () => {
  state.accountValueVisible = !state.accountValueVisible;
  localStorage.setItem("ibkrAccountValueVisible", String(state.accountValueVisible));
  renderAccountValue(state.snapshot?.account || {});
});
$("canaryDetailToggle").addEventListener("click", () => {
  state.canaryDetailOpen = !state.canaryDetailOpen;
  renderCanaryDetail(state.snapshot?.canary || {});
});

document.querySelectorAll("[data-tool]").forEach((button) => {
  button.addEventListener("click", async () => {
    const res = await fetch("/api/tools/" + button.dataset.tool, { method: "POST", credentials: "include" });
    $("toolOutput").textContent = JSON.stringify(await res.json(), null, 2);
  });
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
  if (typeof value !== "number") return "--";
  return new Intl.NumberFormat(undefined, { style: "currency", currency: currency || "USD" }).format(value);
}

function pct(value) {
  if (typeof value !== "number") return "--";
  return value.toFixed(1) + "%";
}

function signedPct(value) {
  if (typeof value !== "number") return "--";
  const sign = value > 0 ? "+" : "";
  return sign + value.toFixed(1) + "%";
}

function cleanDetail(value) {
  if (!value) return "--";
  return String(value).replaceAll("_", " ");
}

function shortTime(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function setConnection(text, ok) {
  $("connectionLine").textContent = text;
  $("statusDot").className = "status-dot " + (ok ? "ok" : "risk");
}

function showPairing(text) {
  $("pairingPanel").hidden = false;
  $("dashboard").hidden = true;
  $("alertsPanel").hidden = true;
  $("toolsPanel").hidden = true;
  $("pairingText").textContent = text;
  setConnection("Locked", false);
}

main().catch((err) => {
  console.error(err);
  showPairing(err.message);
});
