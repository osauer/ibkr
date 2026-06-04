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
  portfolioDetailOpen: false,
  accountMenuOpen: false,
  selectedMarket: localStorage.getItem("ibkrSelectedMarket") || "us",
  marketCalendarOverride: null,
  selectedAlertID: null,
  alertFilter: "all",
  clearedAlertFingerprint: localStorage.getItem("ibkrClearedAlertFingerprint") || "",
  orderReviewSets: [],
  activeOrderReviewSetID: null,
  orderReviewEdits: {},
  orderReviewLoading: false,
  orderReviewError: "",
  orderPreview: null,
  orderTransmitLoading: false,
  orderTransmitResult: null,
  ordersOpen: null,
  openOrderEdits: {},
  underlyingActionNotice: "",
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
  setupMarketSelect();
  setInterval(() => {
    const snap = state.snapshot || {};
    renderTopbar(snap);
    renderSyncStrip(snap);
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
  state.alertSettings = data.alert_settings || state.alertSettings;
  state.alerts = data.alerts || [];
  state.orderReviewSets = data.order_review_sets || [];
  state.activeOrderReviewSetID = state.orderReviewSets[0]?.id || null;
  state.vapidPublicKey = data.vapid_public_key || "";
  $("pairingPanel").hidden = true;
  $("dashboard").hidden = false;
  $("alertsPanel").hidden = false;
  $("ordersPanel").hidden = false;
  $("toolsPanel").hidden = false;
  setConnection("Connected", true);
  renderAll();
  connectEvents();
  refreshOpenOrders();
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
  state.eventSource?.close();
  const es = new EventSource("/api/events", { withCredentials: true });
  state.eventSource = es;
  for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "market_quotes", "trading", "canary"]) {
    es.addEventListener(type, (event) => {
      const data = JSON.parse(event.data);
      if (type === "snapshot") state.snapshot = data;
      if (type !== "snapshot") state.snapshot = { ...(state.snapshot || {}), [type]: data };
      setConnection("Connected", true);
      renderAll();
      if (type === "canary") {
        setTimeout(refreshAlerts, 500);
      }
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
  renderTopbar(snap);
  renderAccountValue(account);
  renderSensitiveSignedMoney("dailyPnl", account.daily_pnl, account.base_currency);
  renderCanaryUnderlyings(positions, account);
  renderSensitiveText("cushion", typeof account.cushion === "number" ? pct(account.cushion * 100) : "--", typeof account.cushion === "number");
  $("accountAsOf").textContent = shortTime(account.as_of);
  $("positionsAsOf").textContent = shortTime(positions.as_of);
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = labelize(canary.severity || "--");
  $("canaryAction").textContent = canaryStageLabel(canary);
  $("canarySummary").textContent = canarySummaryText(canary, snap);
  renderCanaryStatus(canary);
  renderCanaryTimestamp(canary);
  renderSelectedAlert();
  renderOrderReview();
  renderOpenOrders();
  renderMarketContext(snap);
  renderCanaryDetail(canary);
  renderPortfolioRisk(positions, account);
  renderSourceBanners(snap);
  renderAlertMode();
  renderAlerts();
  renderSyncStrip(snap);
}

function renderAccountValue(account) {
  const hasSnapshot = Boolean(account.as_of || account.account_id || account.base_currency);
  const hasValue = hasSnapshot && typeof account.net_liquidation === "number";
  const accountContext = currentAccountContext(account);
  const value = $("netLiquidation");
  value.textContent = state.accountValueVisible || !hasValue
    ? compactMoney(account.net_liquidation, account.base_currency)
    : "******";
  value.classList.toggle("is-private", !state.accountValueVisible && hasValue);
  renderSensitiveText("buyingPower", compactMoney(account.buying_power, account.base_currency), hasSnapshot && typeof account.buying_power === "number");
  $("accountContextLine").textContent = accountContext.contextLine;
  $("tradingEnvPill").textContent = accountContext.modeLabel;
  $("tradingEnvPill").className = "trading-env-pill " + accountContext.modeClass;
  $("accountStatusCard").className = "account-status-card " + accountContext.modeClass;
  $("accountEnvironment").textContent = accountContext.modeLabel;
  $("orderAccountLabel").textContent = accountContext.accountLabel;

  const button = $("accountPrivacyToggle");
  button.classList.toggle("is-visible", state.accountValueVisible);
  button.setAttribute("aria-pressed", String(state.accountValueVisible));
  const label = state.accountValueVisible ? "Hide account values" : "Show account values";
  button.setAttribute("aria-label", label);
  button.title = label;
  renderAccountMenu(account);
}

function renderAccountMenu(account) {
  const panel = $("accountMenu");
  const button = $("accountMenuToggle");
  if (!panel || !button) return;

  const accountContext = currentAccountContext(account);
  panel.hidden = !state.accountMenuOpen;
  button.setAttribute("aria-expanded", String(state.accountMenuOpen));
  button.className = "account-chip " + accountContext.modeClass;
  $("accountChipText").textContent = accountContext.chipLabel;
}

function renderCanaryUnderlyings(positions = {}, account = {}) {
  const list = $("underlyingBookList");
  if (!list) return;

  const baseCurrency = normalizeCurrency(account.base_currency || positions.portfolio?.base_currency || "USD") || "USD";
  const rows = underlyingBookRows(positions, baseCurrency);
  const heldCount = rows.filter((row) => !row.virtual).length;
  const virtualCount = rows.length - heldCount;
  const count = $("underlyingBookCount");
  const status = $("underlyingBookStatus");
  count.textContent = rows.length === 0
    ? "No underlyings"
    : `${heldCount} held${virtualCount > 0 ? ` / ${virtualCount} virtual` : ""}`;
  status.textContent = state.underlyingActionNotice
    || (virtualCount > 0 ? "Includes virtual purge-book records" : heldCount > 0 ? "Current held underlyings" : "Waiting for positions or purge book");

  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "underlying-book__empty";
    empty.textContent = "No held or virtual underlyings.";
    list.replaceChildren(empty);
    return;
  }

  list.replaceChildren(...rows.map((row) => underlyingBookRow(row, baseCurrency)));
}

function underlyingBookRows(positions, baseCurrency) {
  const rows = new Map();
  for (const row of heldUnderlyingRows(positions, baseCurrency)) {
    rows.set(row.symbol, row);
  }
  for (const row of purgedUnderlyingRows(positions, baseCurrency)) {
    const existing = rows.get(row.symbol);
    if (existing) {
      existing.hasPurgeRecord = true;
      existing.purgeLabel = row.purgeLabel;
      continue;
    }
    rows.set(row.symbol, row);
  }
  return [...rows.values()].sort((a, b) => {
    if (a.virtual !== b.virtual) return a.virtual ? 1 : -1;
    return a.symbol.localeCompare(b.symbol);
  });
}

function heldUnderlyingRows(positions, baseCurrency) {
  return (positions.by_underlying || []).map((group) => {
    const symbol = normalizeSymbol(group.underlying || group.stock?.symbol || group.options?.[0]?.symbol);
    if (!symbol) return null;
    const quote = quoteBySymbol(state.snapshot?.market_quotes?.quotes || {}, symbol);
    const price = heldUnderlyingPrice(group, quote);
    const currency = heldUnderlyingCurrency(group, quote, baseCurrency);
    const pnl = heldUnderlyingPnl(group, baseCurrency, currency);
    const stockCount = group.stock ? 1 : 0;
    const optionCount = (group.options || []).length;
    return {
      symbol,
      currency,
      price: price.value,
      priceSource: price.source,
      changePct: heldUnderlyingChangePct(group, quote, price.value),
      pnl: pnl.value,
      pnlCurrency: pnl.currency,
      pnlSource: pnl.source,
      held: true,
      virtual: false,
      purged: false,
      stockCount,
      optionCount,
      detail: underlyingPositionDetail(stockCount, optionCount),
    };
  }).filter(Boolean);
}

function heldUnderlyingPrice(group, quote) {
  const marketPrice = quotePrice(quote);
  if (typeof marketPrice === "number") {
    return { value: marketPrice, source: quoteSourceLabel(quote, "market quote") };
  }
  const stockPrice = firstNumber(group.stock?.quote_price, group.stock?.mark, group.stock?.valuation_mark);
  if (typeof stockPrice === "number") {
    const source = typeof group.stock?.quote_price === "number" ? "stock quote" : "account mark";
    return { value: stockPrice, source };
  }
  const optionUnderlying = firstNumber(...(group.options || []).map((option) => option.underlying));
  if (typeof optionUnderlying === "number") {
    return { value: optionUnderlying, source: "option model spot" };
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

function heldUnderlyingCurrency(group, quote, baseCurrency) {
  const quoteCurrency = normalizeCurrency(quote?.currency || quote?.contract?.currency);
  if (quoteCurrency) return quoteCurrency;
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  const currencies = [...new Set(rows.map((row) => normalizeCurrency(row.currency)).filter(Boolean))];
  if (currencies.length === 1) return currencies[0];
  if (currencies.length > 1) return "MIX";
  return baseCurrency;
}

function heldUnderlyingPnl(group, baseCurrency, currency) {
  if (typeof group.group_unrealized_pnl_base === "number") {
    return { value: group.group_unrealized_pnl_base, currency: baseCurrency, source: "unrealized P/L" };
  }
  const rows = [group.stock, ...(group.options || [])].filter(Boolean);
  if (rows.length > 0 && rows.every((row) => typeof row.unrealized_pnl_base === "number")) {
    return { value: rows.reduce((sum, row) => sum + row.unrealized_pnl_base, 0), currency: baseCurrency, source: "unrealized P/L" };
  }
  if (typeof group.group_unrealized_pnl_ccy === "number") {
    return { value: group.group_unrealized_pnl_ccy, currency, source: "unrealized P/L" };
  }
  return { value: null, currency: baseCurrency, source: "no P/L" };
}

function purgedUnderlyingRows(positions, baseCurrency) {
  const rows = new Map();
  for (const entry of purgeBookEntries(positions)) {
    const symbol = normalizeSymbol(entry.underlying || entry.symbol || entry.ticker || entry.contract?.symbol);
    if (!symbol) continue;
    const row = rows.get(symbol) || {
      symbol,
      currency: "",
      price: null,
      priceSource: "",
      changePct: null,
      pnl: null,
      pnlCurrency: "",
      pnlSource: "shadow P/L",
      virtual: true,
      purged: true,
      held: false,
      legCount: 0,
      purgeIDs: new Set(),
      detail: "",
    };
    const currency = normalizeCurrency(entry.currency || entry.trading_currency || entry.contract?.currency || entry.base_currency);
    if (currency) {
      row.currency = mergeCurrency(row.currency, currency);
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
  return [...rows.values()].map((row) => ({
    ...row,
    currency: row.currency || row.pnlCurrency || baseCurrency,
    pnlCurrency: row.pnlCurrency || row.currency || baseCurrency,
    priceSource: row.priceSource || "purge book",
    purgeLabel: row.purgeIDs.size > 0 ? [...row.purgeIDs].slice(0, 2).join(", ") : "purge book",
    detail: `${row.legCount} purged ${row.legCount === 1 ? "leg" : "legs"}`,
  }));
}

function purgeBookEntries(positions = {}) {
  const out = [];
  const candidates = [
    state.snapshot?.purge_book,
    state.snapshot?.purge_books,
    state.snapshot?.purged_underlyings,
    state.snapshot?.purged_positions,
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
  const direct = firstNumber(entry.group_unrealized_pnl_base, entry.unrealized_pnl_base, entry.pnl_base, entry.pnl, entry.shadow_saved);
  const currency = normalizeCurrency(entry.pnl_currency || entry.base_currency || entry.currency || entry.contract?.currency);
  if (typeof direct === "number") {
    return { value: direct, currency, source: typeof entry.shadow_saved === "number" ? "shadow P/L" : "unrealized P/L" };
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

  const price = document.createElement("div");
  price.className = "underlying-row__metric";
  const priceValue = document.createElement("b");
  priceValue.textContent = displayMoney(row.price, row.currency);
  const priceNote = document.createElement("small");
  priceNote.textContent = row.priceSource || "price";
  price.append(priceValue, priceNote);

  const change = document.createElement("div");
  change.className = "underlying-row__metric";
  const changeValue = document.createElement("b");
  changeValue.className = signedClass(row.changePct);
  changeValue.textContent = signedPct(row.changePct);
  const changeNote = document.createElement("small");
  changeNote.textContent = "% change";
  change.append(changeValue, changeNote);

  const pnl = document.createElement("div");
  pnl.className = "underlying-row__metric underlying-row__metric--pnl";
  const pnlValue = document.createElement("b");
  pnlValue.className = signedClass(row.pnl) + (!state.accountValueVisible && typeof row.pnl === "number" ? " is-private" : "");
  pnlValue.textContent = typeof row.pnl === "number" && !state.accountValueVisible ? "******" : displayMoney(row.pnl, row.pnlCurrency || baseCurrency);
  const pnlNote = document.createElement("small");
  pnlNote.textContent = row.pnlSource || "P/L";
  pnl.append(pnlValue, pnlNote);

  const actions = document.createElement("div");
  actions.className = "underlying-row__actions";
  actions.append(
    underlyingActionButton("Purge", !row.virtual, row, "purge"),
    underlyingActionButton("Restore", row.virtual, row, "restore"),
    underlyingActionButton("Rebuild", row.virtual, row, "rebuild"),
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

function underlyingActionButton(label, enabled, row, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "underlying-action underlying-action--" + action;
  button.textContent = label;
  button.disabled = !enabled;
  const disabledReason = row.virtual
    ? "Already in the purge book; restore or rebuild is available."
    : "Available after this underlying has been purged.";
  button.title = enabled ? `${label} placeholder for ${row.symbol}` : disabledReason;
  button.setAttribute("aria-label", `${label} ${row.symbol}`);
  if (enabled) {
    button.addEventListener("click", () => {
      state.underlyingActionNotice = `${label} placeholder for ${row.symbol}; backend wiring pending.`;
      renderCanaryUnderlyings(state.snapshot?.positions || {}, state.snapshot?.account || {});
    });
  }
  return button;
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
  if (typeof value !== "number") return "--";
  const ccy = normalizeCurrency(currency);
  if (/^[A-Z]{3}$/.test(ccy) && ccy !== "MIX") {
    return money(value, ccy);
  }
  const amount = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
  return ccy ? `${amount} ${ccy}` : amount;
}

function currentAccountContext(account = {}) {
  const trading = state.snapshot?.trading || {};
  const rawTradingAccount = String(trading.account || "").trim();
  const rawAccount = String(account.account_id || "").trim();
  const rawPositionsAccount = String(state.snapshot?.positions?.account_id || "").trim();
  const concreteTradingAccount = rawTradingAccount && rawTradingAccount.toLowerCase() !== "all" ? rawTradingAccount : "";
  const concreteAccount = rawAccount && rawAccount.toLowerCase() !== "all" ? rawAccount : "";
  const concretePositionsAccount = rawPositionsAccount && rawPositionsAccount.toLowerCase() !== "all" ? rawPositionsAccount : "";
  const accountLabel = concreteTradingAccount || concreteAccount || concretePositionsAccount || "";
  const mode = String(trading.mode || "").trim();
  const modeLabel = mode ? labelize(mode) : account.account_type || account.type || (accountLabel ? "IBKR" : "Local PWA");
  const aggregate = rawTradingAccount.toLowerCase() === "all" || rawAccount.toLowerCase() === "all" || rawPositionsAccount.toLowerCase() === "all";
  const contextLine = accountLabel
    ? `${accountLabel} / ${modeLabel}`
    : aggregate ? `No concrete order account / ${modeLabel}` : "No account synced";
  return {
    accountLabel: accountLabel || "No concrete account",
    chipLabel: accountLabel ? `${accountLabel} / ${modeLabel}` : mode ? modeLabel : "Account",
    contextLine,
    modeClass: String(modeLabel).toLowerCase().includes("paper") ? "paper" : String(modeLabel).toLowerCase().includes("live") ? "live" : "neutral",
    modeLabel,
    hasAccount: Boolean(accountLabel || aggregate),
    hasConcreteAccount: Boolean(accountLabel),
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

  const verdict = cleanDetail(canary.market?.regime_verdict);
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
  const verdict = cleanDetail(canary.market?.regime_verdict);
  return {
    label: "Market",
    title: verdict === "--" ? "No clear market stress" : verdict,
    body: "The broad-market regime is not giving a fully confirmed canary trigger.",
    tone: "ok",
  };
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
  const el = $("canaryAsOf");
  const at = parseDate(canary.as_of);
  if (!at) {
    el.textContent = "no timestamp";
    el.classList.add("stale");
    return;
  }
  const ageMS = Date.now() - at.getTime();
  const ageMinutes = Math.max(0, Math.floor(ageMS / 60000));
  const stale = ageMinutes >= 5;
  el.textContent = `${stale ? "stale" : "updated"} ${shortTime(canary.as_of)} · ${ageLabel(ageMinutes)}`;
  el.classList.toggle("stale", stale);
}

function renderMarketContext(snap) {
  const canary = snap.canary || {};
  const market = canary.market || {};
  const indicators = canary.market_indicators || [];
  const quotes = snap.market_quotes?.quotes || {};
  const spyQuote = quoteBySymbol(quotes, "SPY");
  const qqqQuote = quoteBySymbol(quotes, "QQQ");
  const vixQuote = quoteBySymbol(quotes, "VIX");
  const spyPrice = quotePrice(spyQuote) ?? market.spy_price;
  const spyChange = quoteChangePct(spyQuote) ?? market.spy_change_pct;
  const vixPrice = quotePrice(vixQuote) ?? market.vix;
  const vixChange = quoteChangePct(vixQuote) ?? market.vix_change_pct;
  const nasdaqPrice = quotePrice(qqqQuote) ?? firstNumber(
    market.qqq_price,
    market.ndx_price,
    market.nasdaq_price,
    market.nasdaq_100_price,
  );
  const nasdaqChange = quoteChangePct(qqqQuote) ?? firstNumber(
    market.qqq_change_pct,
    market.ndx_change_pct,
    market.nasdaq_change_pct,
    market.nasdaq_100_change_pct,
  );
  const hasSpyData = typeof spyPrice === "number";
  const hasVIXData = typeof vixPrice === "number";
  const hasNasdaqData = typeof nasdaqPrice === "number";
  $("marketAsOf").textContent = marketQuoteFreshnessLabel(snap.market_quotes, [spyQuote, qqqQuote, vixQuote], canary.as_of);
  $("spyLevel").textContent = numberRead(spyPrice);
  $("vixLevel").textContent = numberRead(vixPrice);
  $("nasdaqLevel").textContent = numberRead(nasdaqPrice);
  const spyTone = renderSignedPercent("spyChange", spyChange, false);
  const vixTone = renderSignedPercent("vixChange", vixChange, true);
  const nasdaqTone = renderSignedPercent("nasdaqChange", nasdaqChange, false);
  setMarketTileTone(".market-tile--spy", spyTone);
  setMarketTileTone(".market-tile--vix", vixTone);
  setMarketTileTone(".market-tile--nasdaq", nasdaqTone);
  setQuoteRangeTone("#spyBaseline", spyTone);
  setQuoteRangeTone("#vixBaseline", vixTone);
  setQuoteRangeTone("#nasdaqBaseline", nasdaqTone);
  renderQuoteRange("#spyBaseline", quotePrevClose(spyQuote) ?? market.spy_prev_close, spyPrice, spyChange, "SPY", !hasSpyData);
  renderQuoteRange("#vixBaseline", quotePrevClose(vixQuote) ?? market.vix_prev_close, vixPrice, vixChange, "VIX", !hasVIXData);
  renderQuoteRange("#nasdaqBaseline", quotePrevClose(qqqQuote) ?? market.qqq_prev_close ?? market.ndx_prev_close ?? market.nasdaq_prev_close, nasdaqPrice, nasdaqChange, "QQQ", !hasNasdaqData);
  document.querySelector(".market-tile--spy")?.classList.toggle("market-tile--missing", !hasSpyData);
  document.querySelector(".market-tile--vix")?.classList.toggle("market-tile--missing", !hasVIXData);
  document.querySelector(".market-tile--nasdaq")?.classList.toggle("market-tile--missing", !hasNasdaqData);
  const spyChangePending = hasSpyData && typeof spyChange !== "number";
  const vixChangePending = hasVIXData && typeof vixChange !== "number";
  const nasdaqChangePending = hasNasdaqData && typeof nasdaqChange !== "number";
  setQuoteTileNote("spyNote", spyQuote, spyChangePending ? "Change pending" : "S&P 500 ETF", "SPY", !hasSpyData, quoteChangePendingTitle(spyChangePending));
  setQuoteTileNote("vixNote", vixQuote, vixChangePending ? "Change pending" : "VIX index", "VIX", !hasVIXData, quoteChangePendingTitle(vixChangePending));
  setQuoteTileNote("nasdaqNote", qqqQuote, nasdaqChangePending ? "Change pending" : "Nasdaq 100 ETF", "QQQ", !hasNasdaqData, quoteChangePendingTitle(nasdaqChangePending));
  const regimeStatus = marketRegimeStatusLine(snap, canary, market, indicators);
  $("marketRegime").textContent = marketRegimeLabel(market, indicators, canary);
  $("marketRegimeSummary").textContent = regimeStatus.summary;
  $("marketRegimeMix").textContent = regimeStatus.detail;
  $("marketRegimeMix").title = regimeStatus.title;
  renderMarketWeather(market, indicators);
  renderRegimeDetail(indicators);
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

function setQuoteTileNote(id, quote, fallback, symbol, missing, title = "") {
  const el = $(id);
  el.textContent = quoteTileNote(quote, fallback, symbol, missing);
  el.title = missing ? missingQuoteReason(quote) : title;
}

function quoteTileNote(quote, fallback, symbol, missing) {
  if (missing) return `No ${symbol} price`;
  const quality = String(quote?.quote_quality || "").trim();
  if (quality && quality !== "firm") return labelize(quality) + " quote";
  const dataType = String(quote?.data_type || "").trim();
  if (dataType && dataType !== "live") return labelize(dataType) + " quote";
  return fallback;
}

function missingQuoteReason(quote) {
  const staleReason = String(quote?.stale_reason || "").trim();
  if (staleReason) return labelize(staleReason);
  const quality = String(quote?.quote_quality || "").trim().toLowerCase();
  if (quality && quality !== "missing") return `${labelize(quality)} quote`;
  return "IBKR returned no price";
}

function quoteChangePendingTitle(pending) {
  return pending ? "IBKR quote has no previous-close baseline yet." : "";
}

function marketQuoteFreshnessLabel(marketQuotes, quotes, fallbackTime) {
  const present = (quotes || []).filter(Boolean);
  const at = marketQuotes?.as_of || latestQuoteTime(present) || fallbackTime;
  if (present.length === 0) {
    return at ? `Quote pending ${shortTime(at)}` : "Quote pending";
  }
  const priced = present.filter((quote) => typeof quotePrice(quote) === "number");
  if (priced.length === 0) {
    return at ? `IBKR quote pending ${shortTime(at)}` : "IBKR quote pending";
  }
  const dataTypes = present.map((quote) => String(quote.data_type || "").toLowerCase()).filter(Boolean);
  const delayed = dataTypes.some((value) => value.includes("delayed"));
  const frozen = dataTypes.some((value) => value.includes("frozen"));
  const live = dataTypes.includes("live");
  const prefix = delayed ? "Delayed quote" : live ? "Live quote" : frozen ? "Frozen quote" : "IBKR quote";
  return at ? `${prefix} ${shortTime(at)}` : prefix;
}

function latestQuoteTime(quotes) {
  let latest = null;
  for (const quote of quotes) {
    const at = parseDate(quote?.quote_price_at || quote?.price_at || quote?.as_of);
    if (at && (!latest || at > latest)) {
      latest = at;
    }
  }
  return latest?.toISOString() || "";
}

function marketRegimeLabel(market, indicators, canary = {}) {
  const tone = marketWeatherTone(market, indicators);
  if (tone === "red") return "Risk-off";
  if (tone === "amber" && marketHasDataGaps(market) && canaryNeedsInputCheck(canary)) {
    const verdict = cleanDetail(market.regime_verdict);
    if (verdict.toLowerCase().includes("normal")) return "Normal + gaps";
    return "Data gaps";
  }
  if (tone === "green") return "Support";
  if (tone === "amber") return "Mixed";
  const verdict = cleanDetail(market.regime_verdict);
  return verdict === "--" ? "--" : labelize(verdict);
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
  const support = counts.green || 0;
  return `${risk} Risk · ${neutral} Neutral · ${support} Support`;
}

function latestRegimeRead(canary, indicators) {
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
  let fallback = "";
  for (const candidate of candidates) {
    const parsed = parseDate(candidate);
    if (parsed && (!latest || parsed > latest)) {
      latest = parsed;
      continue;
    }
    if (!fallback) fallback = String(candidate);
  }
  if (latest) return shortTimeWithZone(latest.toISOString());
  return fallback || "Waiting for regime timestamp";
}

function renderMarketWeather(market, indicators) {
  const tone = marketWeatherTone(market, indicators);
  const button = $("marketRegimeToggle");
  button.classList.remove("weather-green", "weather-amber", "weather-red", "weather-na");
  button.classList.add("weather-" + tone);
}

function marketWeatherTone(market, indicators) {
  const redClusters = Number(market.red_clusters || 0);
  const yellowClusters = Number(market.yellow_clusters || 0);
  const rankedClusters = Number(market.ranked_clusters || 0);
  const cautionLists = [
    market.ambiguous_clusters,
    market.partial_clusters,
    market.computing_clusters,
    market.degraded_clusters,
    market.stale_clusters,
    market.unconfirmed_red_cluster_names,
  ];
  if (redClusters > 0) return "red";
  if (yellowClusters > 0 || cautionLists.some((items) => Array.isArray(items) && items.length > 0)) return "amber";
  if (rankedClusters > 0) return "green";

  const statuses = (indicators || []).map((indicator) => indicatorStatusClass(indicator.status));
  if (statuses.includes("red")) return "red";
  if (statuses.some((status) => ["amber", "context", "na"].includes(status))) return "amber";
  if (statuses.includes("green")) return "green";

  const verdict = String(market.regime_verdict || "").toLowerCase();
  if (!verdict) return "na";
  if (verdict.includes("broad stress") || verdict.includes("stress signal") || verdict.includes("red")) return "red";
  if (verdict.includes("normal") || verdict.includes("green") || verdict.includes("constructive")) return "green";
  return "amber";
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

function setMarketTileTone(selector, tone) {
  const el = document.querySelector(selector);
  if (!el) return;
  el.classList.remove("market-tile--ok", "market-tile--risk", "market-tile--neutral");
  el.classList.add("market-tile--" + (tone || "neutral"));
}

function setQuoteRangeTone(selector, tone) {
  const el = document.querySelector(selector);
  if (!el) return;
  el.classList.remove("quote-range--ok", "quote-range--risk", "quote-range--neutral");
  el.classList.add("quote-range--" + (tone || "neutral"));
}

function renderQuoteRange(selector, previousClose, currentPrice, changePct, label, showMissingGuide = false) {
  const el = document.querySelector(selector);
  if (!el) return;
  const inferredPreviousClose = previousClose ?? previousCloseFromChange(currentPrice, changePct);
  const hasGuide = typeof inferredPreviousClose === "number" && typeof currentPrice === "number";
  el.textContent = "";
  el.classList.toggle("quote-range--missing", !hasGuide);
  if (hasGuide) {
    const movePct = typeof changePct === "number" ? changePct : ((currentPrice - inferredPreviousClose) / inferredPreviousClose) * 100;
    const labelText = `${label} ${numberRead(currentPrice)} vs previous close ${numberRead(inferredPreviousClose)} (${signedPct(movePct)})`;
    el.style.setProperty("--quote-pos", quoteRangePosition(movePct) + "%");
    el.title = labelText;
    el.setAttribute("aria-label", labelText);
    return;
  }
  el.style.removeProperty("--quote-pos");
  const missingText = showMissingGuide ? `${label} quote unavailable` : `${label} previous-close marker pending`;
  el.title = missingText;
  el.setAttribute("aria-label", missingText);
}

function quoteRangePosition(changePct) {
  if (typeof changePct !== "number") return 50;
  return Math.max(6, Math.min(94, 50 + changePct * 10));
}

function previousCloseFromChange(currentPrice, changePct) {
  if (typeof currentPrice !== "number" || typeof changePct !== "number" || changePct <= -100) return null;
  return currentPrice / (1 + changePct / 100);
}

function renderRegimeDetail(indicators) {
  const panel = $("regimeDetailPanel");
  const button = $("marketRegimeToggle");
  panel.hidden = !state.regimeDetailOpen;
  button.setAttribute("aria-expanded", String(state.regimeDetailOpen));
  if (!state.regimeDetailOpen) return;
  $("regimeIndicators").replaceChildren(...indicators.map((indicator) => {
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
  renderSensitiveText("portfolioDollarDelta", riskMoney(
    portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
    portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
  ), hasNumericValue(portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy));
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
    value.textContent = state.accountValueVisible
      ? money(exposure.market_value_base, exposure.base_currency || baseCurrency)
      : "******";
    value.className = "exposure-value";
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
  panel.hidden = !state.portfolioDetailOpen;
  button.setAttribute("aria-expanded", String(state.portfolioDetailOpen));
  if (!state.portfolioDetailOpen) return;
  $("portfolioDetailList").replaceChildren(...portfolioDetailRows(portfolio, positions, baseCurrency).map(detailFact));
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
  label.textContent = fact.label;
  const title = document.createElement("b");
  title.textContent = fact.title || "--";
  const body = document.createElement("p");
  body.textContent = fact.body || "";
  row.append(label, title, body);
  return row;
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
  $("syncStatusLabel").textContent = sourceIssues > 0 ? "Source issues" : "Last sync:";
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
  const items = filteredAlertItems();
  const allItems = alertItems();
  const activeItems = allItems.filter((alert) => !alertIsStale(alert));
  const clearableLivePreview = currentAlertPreviewItems().length > 0 && !liveAlertPreviewsSuppressed();
  const staleCount = state.alerts.filter((alert) => alertIsStale(alert)).length;
  const count = $("alertCount");
  const activeTones = activeItems.map(alertTone);
  count.textContent = `${activeItems.length} Active`;
  count.classList.toggle("is-zero", activeItems.length === 0);
  count.classList.toggle("has-risk", activeTones.includes("risk"));
  count.classList.toggle("has-warn", !activeTones.includes("risk") && activeTones.includes("warn"));
  $("alertsHint").textContent = state.alerts.length === 0
    ? liveAlertPreviewsSuppressed() ? "Current canary alerts cleared for this snapshot." : currentCanaryHasPortfolioAlert()
      ? "Live canary preview; no alert history recorded yet."
      : "No portfolio alerts for the current low-exposure snapshot."
    : staleCount > 0 ? `${staleCount} previous-context alert${staleCount === 1 ? "" : "s"} hidden. Clear history to reset.`
      : "Tap an alert to inspect it in Canary.";
  $("clearAlertsButton").disabled = state.alerts.length === 0 && !clearableLivePreview;
  document.querySelectorAll("[data-alert-filter]").forEach((button) => {
    button.classList.toggle("active", button.dataset.alertFilter === state.alertFilter);
  });
  $("alertsList").replaceChildren(...items.map((alert) => {
    const row = document.createElement("button");
    row.className = "alert-row alert-row--" + alertTone(alert);
    row.classList.toggle("alert-row--stale", alertIsStale(alert));
    row.type = "button";
    row.classList.toggle("active", alert.id === state.selectedAlertID);
    row.addEventListener("click", () => {
      state.selectedAlertID = alert.id;
      renderAlerts();
      renderSelectedAlert();
      if (!activeOrderReviewSet()) refreshOrderReviewSet();
      $("selectedAlertPanel").scrollIntoView({ block: "nearest" });
    });
    const text = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = alert.title;
    const body = document.createElement("p");
    body.textContent = alert.body;
    text.append(title, body);
    const at = document.createElement("span");
    at.textContent = alertIsStale(alert) ? "stale" : shortTime(alert.created_at);
    row.append(text, at);
    return row;
  }));
  if (items.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No matching canary alerts.";
    $("alertsList").replaceChildren(empty);
  }
}

function renderSelectedAlert() {
  const alert = alertItems().find((item) => item.id === state.selectedAlertID);
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
    : alert.created_at ? `recorded ${shortTime(alert.created_at)}` : "recorded --";
}

function activeOrderReviewSet() {
  const currentSets = state.orderReviewSets.filter((set) => !reviewSetIsStale(set));
  return currentSets.find((set) => set.id === state.activeOrderReviewSetID) || currentSets[0] || null;
}

function activeOrderPreview(set) {
  if (!set) return null;
  for (const preview of [state.orderPreview, set.latest_preview]) {
    if (preview?.set_id === set.id && preview?.set_revision === set.revision) {
      return preview;
    }
  }
  return null;
}

function selectedReviewRows(set) {
  return (set?.rows || []).filter((row) => {
    const edit = rowEdit(row);
    return edit.included && Number(edit.quantity || 0) > 0;
  });
}

function previewRowsByID(preview) {
  const out = new Map();
  for (const row of preview?.rows || []) {
    out.set(row.row_id, row);
  }
  return out;
}

function reviewTransmitGate(set, trading, selected) {
  if (!set) return { ready: false, reason: "Create or refresh a mitigation plan first" };
  if (reviewSetIsStale(set)) return { ready: false, reason: "Refresh the stale mitigation plan before transmitting" };
  if (!trading.can_transmit) return { ready: false, reason: "Transmit is not enabled by trading.status" };
  if ((selected || []).length === 0) return { ready: false, reason: "Select at least one row before transmitting" };
  const preview = activeOrderPreview(set);
  if (!preview) return { ready: false, reason: "Preview selected rows first" };
  if (!preview.submit_ready) return { ready: false, reason: "Latest preview is not submit-ready" };
  const previewRows = previewRowsByID(preview);
  for (const row of selected) {
    const edit = rowEdit(row);
    const previewRow = previewRows.get(row.row_id);
    if (!previewRow || !previewRow.included || previewRow.quantity !== Number(edit.quantity || 0)) {
      return { ready: false, reason: `Preview ${contractLabel(row.contract)} again after selection changes` };
    }
    if (!previewRow.submit_eligible) {
      return { ready: false, reason: `${contractLabel(row.contract)} is not submit eligible` };
    }
    if (!previewToken(previewRow.preview)) {
      return { ready: false, reason: `${contractLabel(row.contract)} has no preview token` };
    }
  }
  return { ready: true, reason: "Transmit selected rows after confirmation" };
}

function renderOrderReview() {
  const panel = $("orderReviewPanel");
  const set = activeOrderReviewSet();
  const shouldShow = Boolean(set || state.orderReviewLoading || state.orderReviewError || state.orderPreview);
  panel.hidden = !shouldShow;
  if (!shouldShow) return;

  const trading = state.snapshot?.trading || set?.capabilities || {};
  $("orderReviewTitle").textContent = "Mitigation plan";
  $("orderReviewMeta").textContent = set
    ? `${(set.rows || []).length} review row${(set.rows || []).length === 1 ? "" : "s"} · updated ${shortTime(set.updated_at || set.created_at)}`
    : "Create a review set from the latest canary context";
  $("orderReviewStatus").textContent = capabilityLine(trading, set);
  $("refreshOrderReviewButton").disabled = state.orderReviewLoading;

  const rows = set?.rows || [];
  $("orderReviewRows").replaceChildren(...rows.map(orderReviewRowElement));
  if (rows.length === 0) {
    const empty = document.createElement("div");
    empty.className = "order-review__empty";
    empty.textContent = state.orderReviewLoading ? "Refreshing risk plan." : "No review rows yet.";
    $("orderReviewRows").replaceChildren(empty);
  }

  const selected = selectedReviewRows(set);
  const totalQty = selected.reduce((sum, row) => sum + rowEdit(row).quantity, 0);
  $("orderReviewSummary").textContent = selected.length === 0
    ? "No selected rows"
    : `${selected.length} selected / ${totalQty} units`;
  $("resetOrderReviewButton").disabled = !set || state.orderReviewLoading || state.orderTransmitLoading;
  $("previewOrdersButton").disabled = !set || !trading.can_preview || selected.length === 0 || state.orderReviewLoading || state.orderTransmitLoading;
  $("previewOrdersButton").title = !set ? "Create a review set first" : !trading.can_preview ? "Preview is not enabled by trading.status" : selected.length === 0 ? "Select at least one row" : "Preview selected rows";
  const transmitGate = reviewTransmitGate(set, trading, selected);
  $("transmitSelectedButton").disabled = !transmitGate.ready || state.orderReviewLoading || state.orderTransmitLoading;
  $("transmitSelectedButton").title = state.orderTransmitLoading ? "Transmit in progress" : transmitGate.reason;

  renderOrderPreview();
}

function orderReviewRowElement(row) {
  const edit = rowEdit(row);
  const item = document.createElement("div");
  item.className = "order-review-row";
  if (!edit.included || edit.quantity === 0) item.classList.add("excluded");

  const include = document.createElement("input");
  include.type = "checkbox";
  include.checked = edit.included;
  include.setAttribute("aria-label", `Include ${row.contract?.symbol || row.row_id}`);
  include.addEventListener("change", () => {
    state.orderReviewEdits[row.row_id] = { ...edit, included: include.checked, quantity: include.checked ? Math.max(1, edit.quantity || row.editable_quantity || row.proposed_quantity || 1) : 0 };
    state.orderPreview = null;
    state.orderTransmitResult = null;
    renderOrderReview();
  });

  const main = document.createElement("div");
  main.className = "order-review-row__main";
  const title = document.createElement("b");
  title.textContent = `${row.action || "--"} ${contractLabel(row.contract)}`;
  const rationale = document.createElement("p");
  rationale.textContent = row.rationale || "Risk-plan row";
  main.append(title, rationale);

  const meta = document.createElement("div");
  meta.className = "order-review-row__meta";
  meta.append(metaPill(`Proposed ${row.proposed_quantity || 0}`));
  meta.append(metaPill(`${row.order_type || "LMT"} ${priceLabel(row.limit_price)}`));
  meta.append(metaPill(row.tif || "DAY"));
  if (row.position_effect) meta.append(metaPill(labelize(row.position_effect)));

  const qty = document.createElement("input");
  qty.type = "number";
  qty.min = "0";
  qty.max = String(row.max_quantity ?? row.proposed_quantity ?? 0);
  qty.step = "1";
  qty.value = String(edit.quantity ?? 0);
  qty.addEventListener("input", () => {
    const next = Math.max(0, Math.trunc(Number(qty.value || 0)));
    state.orderReviewEdits[row.row_id] = { ...edit, included: next > 0 && edit.included, quantity: next };
    state.orderPreview = null;
    state.orderTransmitResult = null;
    renderOrderReview();
  });

  const blockers = document.createElement("div");
  blockers.className = "order-review-row__blockers";
  blockers.textContent = (row.blockers || []).join(" / ");
  blockers.hidden = (row.blockers || []).length === 0;

  item.append(include, main, meta, qty, blockers);
  return item;
}

function renderOrderPreview() {
  const panel = $("orderPreviewPanel");
  const set = activeOrderReviewSet();
  const preview = activeOrderPreview(set);
  if (!preview && !state.orderReviewError && !state.orderTransmitResult) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }
  panel.hidden = false;
  const children = [];
  if (state.orderReviewError) {
    const banner = document.createElement("div");
    banner.className = "order-preview__banner";
    banner.textContent = state.orderReviewError;
    children.push(banner);
  }
  if (preview) {
    const head = document.createElement("div");
    head.className = "order-preview__head";
    head.innerHTML = `<b>${preview.submit_ready ? "Submit-ready preview" : "Preview needs attention"}</b><span>${shortTime(preview.as_of)}</span>`;
    children.push(head);
    for (const row of preview.rows || []) {
      const item = document.createElement("div");
      item.className = "order-preview-row";
      item.classList.toggle("order-preview-row--blocked", row.included && !row.submit_eligible);
      const title = document.createElement("b");
      title.textContent = row.draft
        ? `${row.draft.action} ${row.quantity} ${contractLabel(row.draft.contract)}`
        : `${row.row_id} / ${row.quantity}`;
      const detail = document.createElement("p");
      detail.textContent = previewRowLine(row);
      const token = document.createElement("small");
      token.textContent = row.preview?.preview_token_id ? `token ${row.preview.preview_token_id}` : "no submit token";
      item.append(title, detail, token);
      const transmit = transmitRowResult(row.row_id);
      if (transmit) {
        const result = document.createElement("small");
        result.className = "order-result-line " + (transmit.failure || transmit.result?.accepted === false ? "risk" : "ok");
        result.textContent = transmit.failure
          ? `Transmit failed: ${transmit.failure}`
          : `Transmit row result: ${transmit.result?.accepted ? "accepted" : "not accepted"}${transmit.result?.message ? " / " + transmit.result.message : ""}`;
        item.append(result);
      }
      children.push(item);
    }
  }
  if (state.orderTransmitResult) {
    const summary = document.createElement("div");
    summary.className = "order-preview__banner order-preview__banner--result";
    summary.textContent = transmitResultSummary(state.orderTransmitResult);
    children.push(summary);
  }
  panel.replaceChildren(...children);
}

function renderOpenOrders() {
  const list = $("ordersOpenList");
  const orders = state.ordersOpen?.orders || [];
  $("ordersAsOf").textContent = state.ordersOpen?.as_of ? shortTime(state.ordersOpen.as_of) : "--";
  if (orders.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No open journal-backed orders.";
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

  const price = document.createElement("input");
  price.type = "number";
  price.min = "0";
  price.step = "0.01";
  price.value = typeof edit.limit_price === "number" ? String(edit.limit_price) : "";
  price.placeholder = "Limit";
  price.setAttribute("aria-label", `Limit price for ${order.symbol || id}`);
  price.disabled = !modifyGate.ready || edit.busy;
  price.addEventListener("change", () => {
    const next = Number(price.value || 0);
    edit.limit_price = Number.isFinite(next) && next > 0 ? next : null;
    edit.preview = null;
    edit.result = null;
    edit.error = "";
    renderOpenOrders();
  });

  const fixed = document.createElement("span");
  fixed.className = "open-order-row__fixed";
  fixed.textContent = `${order.order_type || "LMT"} / ${order.tif || "DAY"} / ${order.action || "--"}`;
  editBox.append(qty, price, fixed);

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

function orderActionButton(label, enabled, reason) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "text-button";
  button.textContent = label;
  button.disabled = !enabled;
  button.title = enabled ? label : reason || `${label} unavailable`;
  return button;
}

function rowEdit(row) {
  return state.orderReviewEdits[row.row_id] || {
    included: row.included,
    quantity: row.editable_quantity ?? row.proposed_quantity ?? 0,
  };
}

function resetOrderReviewEdits() {
  state.orderReviewEdits = {};
  state.orderPreview = null;
  state.orderTransmitResult = null;
  state.orderReviewError = "";
  renderOrderReview();
}

async function refreshOrderReviewSet() {
  state.orderReviewLoading = true;
  state.orderReviewError = "";
  state.orderTransmitResult = null;
  renderOrderReview();
  try {
    const res = await fetch("/api/order-review-sets", { method: "POST", credentials: "include" });
    if (!res.ok) throw new Error(await res.text());
    const set = await res.json();
    upsertOrderReviewSet(set);
    state.activeOrderReviewSetID = set.id;
    resetOrderReviewEdits();
  } catch (err) {
    state.orderReviewError = err.message;
    renderOrderReview();
  } finally {
    state.orderReviewLoading = false;
    renderOrderReview();
  }
}

async function previewOrderReviewSet() {
  const set = activeOrderReviewSet();
  if (!set) return;
  if (reviewSetIsStale(set)) {
    state.orderReviewError = "Review set is stale for the current canary/account context. Refresh before previewing.";
    renderOrderReview();
    return;
  }
  state.orderReviewLoading = true;
  state.orderReviewError = "";
  renderOrderReview();
  const rows = (set.rows || []).map((row) => {
    const edit = rowEdit(row);
    return { row_id: row.row_id, included: Boolean(edit.included), quantity: Number(edit.quantity || 0) };
  });
  try {
    const res = await fetch(`/api/order-review-sets/${encodeURIComponent(set.id)}/preview`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ revision: set.revision, rows }),
    });
    const body = await res.json();
    if (res.status === 409 && body.code === "rebase_required") {
      upsertOrderReviewSet(body.current_set);
      state.activeOrderReviewSetID = body.current_set.id;
      state.orderReviewEdits = {};
      state.orderPreview = null;
      state.orderReviewError = "Proposal changed. Review the refreshed rows before previewing.";
      return;
    }
    if (!res.ok) throw new Error(body.error || JSON.stringify(body));
    upsertOrderReviewSet(body.set);
    state.activeOrderReviewSetID = body.set.id;
    state.orderPreview = body.preview;
    state.orderTransmitResult = null;
    await refreshOpenOrders();
  } catch (err) {
    state.orderReviewError = err.message;
  } finally {
    state.orderReviewLoading = false;
    renderOrderReview();
  }
}

async function transmitSelectedOrders() {
  const set = activeOrderReviewSet();
  if (!set) return;
  const trading = state.snapshot?.trading || set.capabilities || {};
  const selected = selectedReviewRows(set);
  const gate = reviewTransmitGate(set, trading, selected);
  if (!gate.ready) {
    state.orderReviewError = gate.reason;
    renderOrderReview();
    return;
  }
  if (!window.confirm(transmitConfirmationText(set, activeOrderPreview(set), selected, trading))) {
    return;
  }
  state.orderTransmitLoading = true;
  state.orderReviewError = "";
  state.orderTransmitResult = null;
  renderOrderReview();
  try {
    const res = await fetch(`/api/order-review-sets/${encodeURIComponent(set.id)}/transmit`, {
      method: "POST",
      credentials: "include",
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    state.orderTransmitResult = body;
    if (body.orders_open) {
      state.ordersOpen = body.orders_open;
      renderOpenOrders();
    }
    await refreshOpenOrders();
  } catch (err) {
    state.orderReviewError = err.message;
  } finally {
    state.orderTransmitLoading = false;
    renderOrderReview();
  }
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
  if (!window.confirm(modifyConfirmationText(order, edit.preview))) {
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
      body: JSON.stringify({ preview_token: edit.preview.preview_token }),
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
  if (!window.confirm(cancelConfirmationText(order, trading))) {
    return;
  }
  edit.busy = "cancel";
  edit.error = "";
  renderOpenOrders();
  try {
    const res = await fetch(`/api/orders/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
      credentials: "include",
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

function upsertOrderReviewSet(set) {
  if (!set?.id) return;
  const next = state.orderReviewSets.filter((item) => item.id !== set.id);
  state.orderReviewSets = [set, ...next].slice(0, 10);
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
  if (!trading.can_modify) return { ready: false, reason: "Modify is not enabled by trading.status" };
  if ("modify_eligible" in order && order.modify_eligible !== true) return { ready: false, reason: "This order is not modify eligible" };
  if (order.open === false) return { ready: false, reason: "Only open orders can be modified" };
  if (String(order.order_type || "LMT").toUpperCase() !== "LMT") return { ready: false, reason: "Canary mitigation UI only supports LMT price changes" };
  if (orderReductionMax(order) <= 0) return { ready: false, reason: "No remaining quantity available to reduce" };
  return { ready: true, reason: "Preview a reduction-only quantity or LMT price change" };
}

function orderCancelGate(order, trading) {
  if (!orderIdentity(order)) return { ready: false, reason: "Order id unavailable" };
  if (!trading.can_cancel) return { ready: false, reason: "Cancel is not enabled by trading.status" };
  if ("cancel_eligible" in order && order.cancel_eligible !== true) return { ready: false, reason: "This order is not cancel eligible" };
  if (order.open === false) return { ready: false, reason: "Only open orders can be cancelled" };
  return { ready: true, reason: "Cancel this journal-backed open order after confirmation" };
}

function modifyPreviewBody(order, edit) {
  const limit = Number(edit.limit_price || 0);
  return {
    action: order.action || "",
    quantity: Math.min(orderReductionMax(order) || 1, Math.max(1, Math.trunc(Number(edit.quantity || 1)))),
    limit_price: Number.isFinite(limit) && limit > 0 ? limit : undefined,
    tif: order.tif || "DAY",
  };
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

function transmitRowResult(rowID) {
  return (state.orderTransmitResult?.rows || []).find((row) => row.row_id === rowID) || null;
}

function transmitResultSummary(result) {
  const rows = result?.rows || [];
  const accepted = rows.filter((row) => row.result?.accepted).length;
  const failed = rows.filter((row) => row.failure || row.result?.accepted === false).length;
  return `Transmit returned per-row results: ${accepted} accepted, ${failed} failed. This is not all-or-nothing.`;
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

function capabilityLine(trading, set) {
  if (!set) return "Create or refresh the review set before preview.";
  if (reviewSetIsStale(set)) return "Stale review set for a previous canary/account context. Refresh before preview.";
  const bits = [
    trading.can_preview ? "preview ready" : "preview blocked",
    trading.can_transmit ? "transmit ready" : "transmit disabled",
    trading.open_orders ? `${trading.open_orders} open` : "",
  ].filter(Boolean);
  const blockers = (trading.blockers || []).map((blocker) => blocker.message || blocker.code).filter(Boolean);
  return [...bits, ...blockers].join(" / ") || "Trading status unavailable";
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

function reviewSetIsStale(set) {
  const current = currentCanaryFingerprint();
  const canaryChanged = Boolean(set?.canary_fingerprint && current && set.canary_fingerprint !== current);
  const trading = state.snapshot?.trading || {};
  const setCaps = set?.capabilities || {};
  const accountChanged = Boolean(setCaps.account && trading.account && setCaps.account !== trading.account);
  const modeChanged = Boolean(setCaps.mode && trading.mode && setCaps.mode !== trading.mode);
  return canaryChanged || accountChanged || modeChanged;
}

function transmitConfirmationText(set, preview, selected, trading) {
  const previewRows = previewRowsByID(preview);
  const firstPreview = selected.map((row) => previewRows.get(row.row_id)?.preview).find(Boolean);
  const lines = [
    "Transmit selected orders?",
    "",
    `Mode: ${labelize(firstPreview?.mode || trading.mode || set.capabilities?.mode || "--")}`,
    `Account: ${firstPreview?.account || trading.account || set.capabilities?.account || "--"}`,
    `Endpoint: ${venueLabel(firstPreview || trading)}`,
    "",
    "Selected rows:",
  ];
  for (const row of selected) {
    const previewRow = previewRows.get(row.row_id) || {};
    lines.push(" - " + orderConfirmationLine(row, previewRow));
  }
  lines.push("", "Rows transmit independently; this is not all-or-nothing.");
  return lines.join("\n");
}

function modifyConfirmationText(order, preview) {
  const warnings = warningMessages(preview.warnings).join(" / ") || "--";
  return [
    "Apply previewed order change?",
    "",
    `Mode: ${labelize(preview.mode || order.mode || "--")}`,
    `Account: ${preview.account || order.account || "--"}`,
    `Endpoint: ${venueLabel(preview || order)}`,
    `Order: ${preview.draft?.action || order.action || "--"} ${preview.draft?.quantity || "--"} ${preview.draft?.contract?.symbol || order.symbol || "--"} ${preview.draft?.order_type || order.order_type || "LMT"} ${priceLabel(preview.draft?.limit_price || order.limit_price)} ${preview.draft?.tif || order.tif || "DAY"}`,
    `WhatIf: ${preview.what_if?.status || "--"}${preview.what_if?.message ? " / " + preview.what_if.message : ""}`,
    `Broker warning/message: ${warnings}`,
    `Submit eligibility: ${preview.submit_eligible ? "eligible" : "not eligible"} / ${preview.preview_token_id ? "token " + preview.preview_token_id : "no token"}`,
  ].join("\n");
}

function cancelConfirmationText(order, trading) {
  return [
    "Cancel order?",
    "",
    `Mode: ${labelize(order.mode || trading.mode || "--")}`,
    `Account: ${order.account || trading.account || "--"}`,
    `Endpoint: ${venueLabel(order.endpoint ? order : trading)}`,
    `Order: ${order.action || "--"} ${order.quantity || "--"} ${order.symbol || order.order_ref || "--"} ${order.order_type || "LMT"} ${priceLabel(order.limit_price)} ${order.tif || "DAY"}`,
    `Status: ${[order.lifecycle_status, order.send_state, order.last_message].filter(Boolean).join(" / ") || "--"}`,
  ].join("\n");
}

function orderConfirmationLine(row, previewRow) {
  const preview = previewRow.preview || {};
  const draft = previewRow.draft || preview.draft || {};
  const warnings = [
    ...warningMessages(preview.warnings),
    ...(previewRow.warnings || []),
    preview.what_if?.message || "",
  ].filter(Boolean).join(" / ") || "--";
  return [
    `${draft.action || row.action || "--"} ${previewRow.quantity || rowEdit(row).quantity || "--"} ${contractLabel(draft.contract || row.contract)}`,
    `${draft.order_type || row.order_type || "LMT"} ${priceLabel(draft.limit_price || row.limit_price)} ${draft.tif || row.tif || "DAY"}`,
    `WhatIf ${previewRow.what_if_status || preview.what_if?.status || "--"}`,
    `warning/message ${warnings}`,
    `submit ${previewRow.submit_eligible ? "eligible" : "not eligible"} / ${preview.preview_token_id ? "token " + preview.preview_token_id : "no token"}`,
  ].join(" / ");
}

function warningMessages(warnings = []) {
  return warnings.map((warning) => {
    if (!warning) return "";
    if (typeof warning === "string") return warning;
    return warning.message || warning.code || JSON.stringify(warning);
  }).filter(Boolean);
}

function venueLabel(value = {}) {
  if (value.endpoint) return value.endpoint;
  if (value.gateway_host && value.gateway_port) return `${value.gateway_host}:${value.gateway_port}`;
  return "broker endpoint unavailable";
}

function metaPill(text) {
  const pill = document.createElement("span");
  pill.textContent = text || "--";
  return pill;
}

function contractLabel(contract = {}) {
  if (contract.local_symbol) return contract.local_symbol;
  if (!contract.symbol) return "--";
  if (contract.sec_type === "OPT") return `${contract.symbol} ${contract.expiry || ""} ${contract.right || ""}${contract.strike || ""}`.trim();
  return contract.symbol;
}

function priceLabel(value) {
  return typeof value === "number" ? value.toFixed(2) : "auto";
}

function previewRowLine(row) {
  const parts = [];
  parts.push(row.submit_eligible ? "submit eligible" : "not submit eligible");
  if (row.what_if_status) parts.push(`WhatIf ${row.what_if_status}`);
  if (row.blockers?.length) parts.push(row.blockers.join(" / "));
  if (row.failure) parts.push(row.failure);
  return parts.join(" / ");
}

async function refreshAlerts() {
  try {
    const res = await fetch("/api/alerts", { credentials: "include" });
    if (!res.ok) return;
    state.alerts = await res.json();
    if (state.selectedAlertID && !alertItems().some((alert) => alert.id === state.selectedAlertID)) {
      state.selectedAlertID = null;
    }
    renderAlerts();
    renderSelectedAlert();
  } catch {
    // Alert history is secondary; SSE recovery handles app connectivity.
  }
}

function alertItems() {
  const history = state.alerts
    .map((alert) => ({ ...alert, preview: false }))
    .filter((alert) => !alertIsStale(alert));
  const previews = liveAlertPreviewsSuppressed() ? [] : currentAlertPreviewItems();
  if (history.length === 0) return previews;
  const historyTitles = new Set(history.map((item) => String(item.title || "").toLowerCase()));
  return [
    ...history,
    ...previews.filter((item) => !historyTitles.has(String(item.title || "").toLowerCase())),
  ].slice(0, 3);
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

function filteredAlertItems() {
  const items = alertItems();
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
  state.accountValueVisible = !state.accountValueVisible;
  localStorage.setItem("ibkrAccountValueVisible", String(state.accountValueVisible));
  renderAll();
});
$("accountMenuToggle").addEventListener("click", () => {
  state.accountMenuOpen = !state.accountMenuOpen;
  renderAccountMenu(state.snapshot?.account || {});
});
$("canaryDetailToggle").addEventListener("click", () => {
  state.canaryDetailOpen = !state.canaryDetailOpen;
  renderCanaryDetail(state.snapshot?.canary || {});
});
$("quickReviewBlockersButton").addEventListener("click", () => {
  state.canaryDetailOpen = true;
  renderCanaryDetail(state.snapshot?.canary || {});
  $("canaryDetailPanel").scrollIntoView({ block: "nearest" });
});
$("quickRiskPlanButton").addEventListener("click", async () => {
  await refreshOrderReviewSet();
  $("orderReviewPanel").scrollIntoView({ block: "nearest" });
});
$("quickHeldActionsButton").addEventListener("click", () => {
  $("underlyingBook").scrollIntoView({ block: "nearest" });
});
$("quickAlertsButton").addEventListener("click", () => {
  $("alertsPanel").open = true;
  $("alertsPanel").scrollIntoView({ block: "nearest" });
});
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
$("refreshOrderReviewButton").addEventListener("click", refreshOrderReviewSet);
$("resetOrderReviewButton").addEventListener("click", resetOrderReviewEdits);
$("previewOrdersButton").addEventListener("click", previewOrderReviewSet);
$("transmitSelectedButton").addEventListener("click", transmitSelectedOrders);
$("marketRegimeToggle").addEventListener("click", () => {
  state.regimeDetailOpen = !state.regimeDetailOpen;
  renderRegimeDetail(state.snapshot?.canary?.market_indicators || []);
});
$("portfolioDetailToggle").addEventListener("click", () => {
  state.portfolioDetailOpen = !state.portfolioDetailOpen;
  renderPortfolioDetail(state.snapshot?.positions?.portfolio || {}, state.snapshot?.positions || {}, state.snapshot?.account?.base_currency || "USD");
});
$("clearAlertsButton").addEventListener("click", clearAlerts);

document.querySelectorAll("[data-tool]").forEach((button) => {
  button.addEventListener("click", async () => {
    const res = await fetch("/api/tools/" + button.dataset.tool, { method: "POST", credentials: "include" });
    $("toolOutput").hidden = false;
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

function compactMoney(value, currency) {
  if (typeof value !== "number") return "--";
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
  el.textContent = typeof value === "number" ? money(value, currency) : "--";
}

function renderSensitiveSignedMoney(id, value, currency) {
  const el = $(id);
  if (!hasNumericValue(value)) {
    el.className = "signed";
    el.textContent = "--";
    return;
  }
  if (!state.accountValueVisible) {
    el.className = "signed is-private";
    el.textContent = "******";
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
    el.textContent = "******";
    return;
  }
  el.classList.remove("is-private");
  el.textContent = value;
}

function sensitiveMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? money(value, currency) : "******";
}

function hasNumericValue(value) {
  return typeof value === "number";
}

function firstNumber(...values) {
  return values.find((value) => typeof value === "number");
}

function signedClass(value) {
  if (typeof value !== "number" || value === 0) return "signed";
  return "signed " + (value > 0 ? "ok" : "risk");
}

function signedTone(value, inverse = false) {
  if (typeof value !== "number" || value === 0) return "neutral";
  const good = inverse ? value > 0 : value >= 0;
  return good ? "ok" : "risk";
}

function numberRead(value) {
  if (typeof value !== "number") return "--";
  return new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}

function riskMoney(value, currency) {
  if (typeof value !== "number") return "--";
  const amount = new Intl.NumberFormat(undefined, {
    maximumFractionDigits: 0,
    minimumFractionDigits: 0,
  }).format(value);
  return `${amount} ${currency || "USD"}`;
}

function pct(value) {
  if (typeof value !== "number") return "--";
  return value.toFixed(1) + "%";
}

function wholePct(value) {
  if (typeof value !== "number") return "--";
  return Math.round(value) + "%";
}

function signedPct(value) {
  if (typeof value !== "number") return "--";
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
  $("dashboard").hidden = true;
  $("alertsPanel").hidden = true;
  $("accountMenu").hidden = true;
  $("ordersPanel").hidden = true;
  $("toolsPanel").hidden = true;
  $("syncStrip").hidden = true;
  $("pairingText").textContent = text;
  setConnection("Locked", false);
}

main().catch((err) => {
  console.error(err);
  showPairing(err.message);
});
