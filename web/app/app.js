const state = {
  snapshot: null,
  alertSettings: { mode: "watch_and_act" },
  alerts: [],
  vapidPublicKey: "",
  eventSource: null,
};

const $ = (id) => document.getElementById(id);

async function main() {
  await navigator.serviceWorker?.register("/service-worker.js");
  const pair = new URLSearchParams(location.search).get("pair");
  const nonce = new URLSearchParams(location.search).get("nonce");
  if (pair && nonce) {
    await completePairing(pair, nonce);
    history.replaceState({}, "", "/");
  }
  await bootstrap();
}

async function bootstrap() {
  const res = await fetch("/api/bootstrap", { credentials: "include" });
  if (res.status === 401) {
    await tryDeviceLogin();
    const retry = await fetch("/api/bootstrap", { credentials: "include" });
    if (retry.status === 401) {
      showPairing("Scan a fresh QR code from `ibkr app pair` on the Mac.");
      return;
    }
    return applyBootstrap(await retry.json());
  }
  if (!res.ok) {
    showPairing("App bootstrap failed: " + await res.text());
    return;
  }
  applyBootstrap(await res.json());
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
}

async function tryDeviceLogin() {
  const deviceID = localStorage.getItem("ibkrDeviceID");
  const privateKey = await loadPrivateKey();
  if (!deviceID || !privateKey) return;
  const ch = await fetch("/api/auth/challenge", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ device_id: deviceID }),
  });
  if (!ch.ok) return;
  const challenge = await ch.json();
  const signature = await sign(privateKey, challenge.challenge);
  await fetch("/api/auth/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ device_id: deviceID, challenge: challenge.challenge, signature }),
  });
}

function connectEvents() {
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
  es.onerror = () => setConnection("Reconnecting", false);
}

function renderAll() {
  const snap = state.snapshot || {};
  const account = snap.account || {};
  const positions = snap.positions || {};
  const canary = snap.canary || {};
  $("netLiquidation").textContent = money(account.net_liquidation, account.base_currency);
  $("dailyPnl").textContent = account.daily_pnl == null ? "--" : money(account.daily_pnl, account.base_currency);
  $("cushion").textContent = account.cushion ? (account.cushion * 100).toFixed(1) + "%" : "--";
  $("accountAsOf").textContent = shortTime(account.as_of);
  $("positionsAsOf").textContent = shortTime(positions.as_of);
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = canary.severity || "--";
  $("canaryAction").textContent = (canary.action || "--").replaceAll("_", " ");
  $("canarySummary").textContent = canary.summary || "Waiting for canary snapshot.";
  renderAlertMode();
  renderAlerts();
}

function renderAlertMode() {
  document.querySelectorAll("#alertSegments button").forEach((button) => {
    button.classList.toggle("active", button.dataset.mode === state.alertSettings.mode);
  });
  $("pushState").textContent = Notification.permission === "granted" ? "push on" : "push off";
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

document.querySelectorAll("[data-tool]").forEach((button) => {
  button.addEventListener("click", async () => {
    const res = await fetch("/api/tools/" + button.dataset.tool, { method: "POST", credentials: "include" });
    $("toolOutput").textContent = JSON.stringify(await res.json(), null, 2);
  });
});

async function enablePush() {
  if (!("PushManager" in window)) {
    $("pushState").textContent = "push unsupported";
    return;
  }
  const registration = await navigator.serviceWorker.ready;
  const permission = await Notification.requestPermission();
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
  const sig = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" },
    privateKey,
    new TextEncoder().encode(value)
  );
  return bytesToB64url(new Uint8Array(sig));
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
