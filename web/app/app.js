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
  selectedAlertID: null,
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
  setInterval(() => renderTopbar(state.snapshot || {}), 30000);
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
  for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "canary"]) {
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
  renderSignedMoney("dailyPnl", account.daily_pnl, account.base_currency);
  $("cushion").textContent = typeof account.cushion === "number" ? pct(account.cushion * 100) : "--";
  $("accountAsOf").textContent = shortTime(account.as_of);
  $("positionsAsOf").textContent = shortTime(positions.as_of);
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = canary.severity || "--";
  $("canaryAction").textContent = (canary.action || "--").replaceAll("_", " ");
  $("canarySummary").textContent = canary.summary || "Waiting for canary snapshot.";
  renderCanaryTimestamp(canary);
  renderSelectedAlert();
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

  $("canaryDetailGrid").replaceChildren(...canaryExplanationCards(canary).map(detailCard));

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
  if (fit === "high") {
    return {
      label: "Portfolio",
      title: "Portfolio is exposed",
      body: "The current portfolio shape is vulnerable if this market pressure continues.",
      tone: "risk",
    };
  }
  if (fit === "medium") {
    return {
      label: "Portfolio",
      title: "Exposure is meaningful",
      body: "The portfolio has some sensitivity to the current stress. Size changes carefully.",
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
    body: "Some inputs are stale, missing, or degraded. Use the detail rows before acting.",
    tone: "warn",
  };
}

function readinessExplanation(canary) {
  const readiness = String(canary.planner_readiness || canary.planner_mode_hint || "").toLowerCase();
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

function renderMarketContext(canary) {
  const market = canary.market || {};
  const indicators = canary.market_indicators || [];
  $("marketAsOf").textContent = shortTime(canary.as_of);
  $("spyLevel").textContent = numberRead(market.spy_price);
  $("vixLevel").textContent = numberRead(market.vix);
  renderSignedPercent("spyChange", market.spy_change_pct, false);
  renderSignedPercent("vixChange", market.vix_change_pct, true);
  $("marketRegime").textContent = cleanDetail(market.regime_verdict);
  renderMarketWeather(market, indicators);
  renderRegimeDetail(indicators);
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
  $("portfolioDetailSummary").textContent = portfolioDetailSummary(portfolio, positions);
  renderPortfolioDetail(portfolio, positions, baseCurrency);

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
    value.className = "exposure-value";
    row.append(label, value);
    const pnl = exposure.daily_pnl_base ?? exposure.unrealized_pnl_base;
    if (typeof pnl === "number") {
      const detail = document.createElement("small");
      detail.className = signedClass(pnl);
      detail.textContent = "P/L " + money(pnl, exposure.base_currency || baseCurrency);
      value.append(detail);
    }
    return row;
  }));
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
      label: "Dollar delta",
      title: money(
        portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy,
        portfolio.dollar_delta_base_currency || portfolio.dollar_delta_ccy_currency || baseCurrency,
      ),
      body: "Approximate portfolio move for a one-point move in the underlyings, converted to account base when possible.",
      tone: "neutral",
    },
    {
      label: "Theta/day",
      title: money(
        portfolio.daily_theta_base ?? portfolio.daily_theta_ccy,
        portfolio.daily_theta_base_currency || portfolio.daily_theta_ccy_currency || baseCurrency,
      ),
      body: "Estimated option time decay per day. Negative values mean expected decay cost.",
      tone: signedTone(portfolio.daily_theta_base ?? portfolio.daily_theta_ccy, true),
    },
    {
      label: "FX 1%",
      title: money(portfolio.fx_sensitivity_per_pct, portfolio.fx_base_currency || baseCurrency),
      body: "Estimated base-currency P/L from a 1% move in non-base contract currencies.",
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
    return `${portfolio.greeks_coverage || 0}/${portfolio.greeks_total} greeks`;
  }
  if ((positions.options || []).length === 0) {
    return "no options";
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
  const label = marketSessionLabel(snap.market_calendar);
  const line = $("connectionLine");
  line.textContent = label.text || state.connectionText;
  line.classList.remove("market-open", "market-closed", "market-warn");
  if (label.tone) {
    line.classList.add(label.tone);
  }
}

function marketSessionLabel(calendar) {
  const session = calendar?.session;
  if (!session) {
    return { text: state.connectionText, tone: state.connectionOK ? "market-warn" : "market-closed" };
  }
  const stateText = cleanDetail(session.state);
  if (session.is_open) {
    const close = parseDate(session.close);
    const timeLeft = countdownLabel(close);
    return {
      text: timeLeft ? `US market open · closes in ${timeLeft}` : "US market open",
      tone: "market-open",
    };
  }
  const nextOpen = parseDate(session.next_open);
  const reason = session.reason ? ` (${session.reason})` : "";
  const untilOpen = countdownLabel(nextOpen);
  let prefix = "US market closed";
  if (session.state === "holiday") {
    prefix = "US market holiday";
  } else if (session.state === "unknown") {
    prefix = "US market unknown";
  } else if (stateText !== "--" && !["regular", "early close"].includes(stateText)) {
    prefix = `US market ${stateText}`;
  }
  return {
    text: untilOpen ? `${prefix}${reason} · opens in ${untilOpen}` : `${prefix}${reason}`,
    tone: session.state === "unknown" ? "market-warn" : "market-closed",
  };
}

function countdownLabel(target) {
  if (!target) return "";
  const ms = target.getTime() - Date.now();
  if (ms <= 0) return "";
  const minutes = Math.ceil(ms / 60000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 24) return rest === 0 ? `${hours}h` : `${hours}h ${rest}m`;
  const days = Math.floor(hours / 24);
  const dayHours = hours % 24;
  return dayHours === 0 ? `${days}d` : `${days}d ${dayHours}h`;
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
  $("alertsHint").textContent = state.alerts.length === 0
    ? "No alert history yet."
    : "Tap an alert to inspect it in Canary.";
  $("clearAlertsButton").disabled = state.alerts.length === 0;
  $("alertsList").replaceChildren(...state.alerts.map((alert) => {
    const row = document.createElement("button");
    row.className = "alert-row";
    row.type = "button";
    row.classList.toggle("active", alert.id === state.selectedAlertID);
    row.addEventListener("click", () => {
      state.selectedAlertID = alert.id;
      renderAlerts();
      renderSelectedAlert();
      $("selectedAlertPanel").scrollIntoView({ block: "nearest" });
    });
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
  if (state.alerts.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-row";
    empty.textContent = "No canary alerts have been recorded.";
    $("alertsList").replaceChildren(empty);
  }
}

function renderSelectedAlert() {
  const alert = state.alerts.find((item) => item.id === state.selectedAlertID);
  const panel = $("selectedAlertPanel");
  panel.hidden = !alert;
  if (!alert) return;
  $("selectedAlertTitle").textContent = alert.title || "Canary alert";
  $("selectedAlertBody").textContent = alert.body || "Open detail for the current canary context.";
  $("selectedAlertTime").textContent = alert.created_at ? `recorded ${shortTime(alert.created_at)}` : "recorded --";
}

async function refreshAlerts() {
  try {
    const res = await fetch("/api/alerts", { credentials: "include" });
    if (!res.ok) return;
    state.alerts = await res.json();
    if (state.selectedAlertID && !state.alerts.some((alert) => alert.id === state.selectedAlertID)) {
      state.selectedAlertID = null;
    }
    renderAlerts();
    renderSelectedAlert();
  } catch {
    // Alert history is secondary; SSE recovery handles app connectivity.
  }
}

async function clearAlerts() {
  const res = await fetch("/api/alerts", { method: "DELETE", credentials: "include" });
  if (!res.ok) return;
  state.alerts = [];
  state.selectedAlertID = null;
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
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
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

function renderSignedMoney(id, value, currency) {
  const el = $(id);
  el.className = signedClass(value);
  el.textContent = typeof value === "number" ? money(value, currency) : "--";
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
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
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

function setConnection(text, ok) {
  state.connectionText = text;
  state.connectionOK = ok;
  $("statusDot").className = "status-dot " + (ok ? "ok" : "risk");
  $("statusDot").setAttribute("aria-label", ok ? "app connected" : "app reconnecting");
  renderTopbar(state.snapshot || {});
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
