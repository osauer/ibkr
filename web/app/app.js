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
  ordersOpen: null,
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
  renderSensitiveText("cushion", typeof account.cushion === "number" ? pct(account.cushion * 100) : "--", typeof account.cushion === "number");
  $("accountAsOf").textContent = shortTime(account.as_of);
  $("positionsAsOf").textContent = shortTime(positions.as_of);
  $("stockCount").textContent = (positions.stocks || []).length;
  $("optionCount").textContent = (positions.options || []).length;
  $("baseCurrency").textContent = account.base_currency || positions.portfolio?.base_currency || "--";
  $("canarySeverity").textContent = labelize(canary.severity || "--");
  $("canaryAction").textContent = canaryStageLabel(canary);
  $("canarySummary").textContent = canary.summary || "Waiting for canary snapshot.";
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
  const cards = canaryExplanationCards(canary);
  const indicators = canary.market_indicators || [];
  const warningCount = (canary.warnings || []).length || cards.filter((card) => card.tone === "warn" || card.tone === "risk").length;
  const checkCount = indicators.length > 0
    ? `${indicators.filter((indicator) => indicatorStatusClass(indicator.status) === "green").length} / ${indicators.length}`
    : `${cards.filter((card) => card.tone === "ok").length} / ${cards.length}`;
  const mode = canaryModeLabel(canary);
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
  $("canaryMode").textContent = mode;
  $("canaryWarningCount").textContent = String(warningCount);
  $("canaryCheckCount").textContent = checkCount;
}

function canaryStageLabel(canary) {
  const action = String(canary.action || "").toLowerCase();
  if (action === "defend") return "Defend";
  if (action === "rebalance") return "Rebalance";
  if (action === "confirm_inputs") return "Confirm";
  const severity = String(canary.severity || "").toLowerCase();
  if (severity === "act") return "Defend";
  if (severity === "watch") return "Watch";
  if (severity === "observe") return "Steady";
  return labelize(canary.action || "--");
}

function canaryModeLabel(canary) {
  const inputHealth = String(canary.input_health || "").toLowerCase();
  if (inputHealth === "ok") return "Live";
  const severity = String(canary.severity || "").toLowerCase();
  const action = String(canary.action || "").toLowerCase();
  if (action) return labelize(action);
  if (severity === "act") return "Risk action";
  if (severity === "watch") return "Closer watch";
  if (severity === "observe") return "Monitor";
  return "--";
}

function canaryRiskLabel(canary) {
  const action = String(canary.action || "").toLowerCase();
  const severity = String(canary.severity || "").toLowerCase();
  if (severity === "act" || action === "defend") return "High";
  if (severity === "watch") return "Watch";
  if (severity === "observe") return "Low";
  return labelize(canary.action || "--");
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
  setSparklineTone(".sparkline--spy", spyTone);
  setSparklineTone(".sparkline--vix", vixTone);
  setSparklineTone(".sparkline--nasdaq", nasdaqTone);
  renderCloseGuide(".sparkline--spy", quotePrevClose(spyQuote) ?? market.spy_prev_close, spyPrice, spyChange, "SPY", !hasSpyData);
  renderCloseGuide(".sparkline--vix", quotePrevClose(vixQuote) ?? market.vix_prev_close, vixPrice, vixChange, "VIX", !hasVIXData);
  renderCloseGuide(".sparkline--nasdaq", quotePrevClose(qqqQuote) ?? market.qqq_prev_close ?? market.ndx_prev_close ?? market.nasdaq_prev_close, nasdaqPrice, nasdaqChange, "QQQ", !hasNasdaqData);
  document.querySelector(".market-tile--spy")?.classList.toggle("market-tile--missing", !hasSpyData);
  document.querySelector(".market-tile--vix")?.classList.toggle("market-tile--missing", !hasVIXData);
  document.querySelector(".market-tile--nasdaq")?.classList.toggle("market-tile--missing", !hasNasdaqData);
  $("nasdaqNote").textContent = quoteTileNote(qqqQuote, "Nasdaq 100 ETF", !hasNasdaqData);
  $("marketRegime").textContent = marketRegimeLabel(market, indicators);
  $("marketRegimeSummary").textContent = "Latest regime read";
  $("marketRegimeMix").textContent = latestRegimeRead(canary, indicators);
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

function quoteTileNote(quote, fallback, missing) {
  if (missing) return "No QQQ data";
  const quality = String(quote?.quote_quality || "").trim();
  if (quality && quality !== "firm") return labelize(quality) + " quote";
  const dataType = String(quote?.data_type || "").trim();
  if (dataType && dataType !== "live") return labelize(dataType) + " quote";
  return fallback;
}

function marketQuoteFreshnessLabel(marketQuotes, quotes, fallbackTime) {
  const present = (quotes || []).filter(Boolean);
  const at = marketQuotes?.as_of || latestQuoteTime(present) || fallbackTime;
  if (present.length === 0) {
    return at ? `Quote pending ${shortTime(at)}` : "Quote pending";
  }
  const dataTypes = present.map((quote) => String(quote.data_type || "").toLowerCase()).filter(Boolean);
  const delayed = dataTypes.some((value) => value.includes("delayed"));
  const frozen = dataTypes.some((value) => value.includes("frozen"));
  const live = dataTypes.includes("live");
  const prefix = delayed ? "Delayed quote" : live ? "Live quote" : frozen ? "Frozen quote" : "Quote";
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

function marketRegimeLabel(market, indicators) {
  const tone = marketWeatherTone(market, indicators);
  if (tone === "red") return "Risk-off";
  if (tone === "green") return "Support";
  if (tone === "amber") return "Mixed";
  const verdict = cleanDetail(market.regime_verdict);
  return verdict === "--" ? "--" : labelize(verdict);
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

function renderSignedPercent(id, value, positiveIsRisk) {
  const el = $(id);
  el.classList.remove("signed", "ok", "risk", "neutral");
  if (typeof value !== "number") {
    el.textContent = "--";
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

function setSparklineTone(selector, tone) {
  const el = document.querySelector(selector);
  if (!el) return;
  el.classList.remove("sparkline--ok", "sparkline--risk", "sparkline--neutral");
  el.classList.add("sparkline--" + (tone || "neutral"));
}

function renderCloseGuide(selector, previousClose, currentPrice, changePct, label, showMissingGuide = false) {
  const el = document.querySelector(selector);
  if (!el) return;
  const inferredPreviousClose = previousClose ?? previousCloseFromChange(currentPrice, changePct);
  const hasGuide = typeof inferredPreviousClose === "number";
  el.classList.toggle("has-close-guide", hasGuide || showMissingGuide);
  el.classList.toggle("sparkline--missing", !hasGuide && showMissingGuide);
  el.title = typeof inferredPreviousClose === "number"
    ? `${label} last close ${numberRead(inferredPreviousClose)}`
    : `${label} last close unavailable`;
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
  $("sessionLabel").textContent = label.label;
  $("sessionCountdown").textContent = label.countdown;
  $("sessionMeta").textContent = label.meta;
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
    const marketName = marketCalendarShortLabel(calendar, null);
    return {
      text: state.connectionOK ? "Waiting for official market calendar" : "App connection offline",
      tone: state.connectionOK ? "market-warn" : "market-closed",
      phase: state.connectionOK ? `${marketName} syncing` : "Offline",
      label: "Waiting",
      countdown: "--:--:--",
      meta: "HH:MM:SS",
      side: state.connectionOK ? "Calendar pending" : "Offline",
    };
  }
  const now = Date.now();
  const stateText = String(session.state || "").toLowerCase();
  const reason = session.reason ? ` (${session.reason})` : "";
  const open = parseDate(session.open);
  const close = parseDate(session.close);
  const nextOpen = parseDate(session.next_open);
  const marketName = marketCalendarShortLabel(calendar, session);
  if (session.is_open) {
    const timeLeft = countdownLabel(close);
    const phase = stateText === "early_close" ? `${marketName} early close` : marketOpenPhase(marketName);
    return {
      text: session.reason || "Regular cash session",
      tone: "market-open",
      phase,
      label: "Closes",
      countdown: timeLeft || "live",
      meta: marketClockMeta("Close", session, close),
      side: marketSessionNow(session),
    };
  }

  if (open && now < open.getTime()) {
    const untilOpen = countdownLabel(open);
    return {
      text: session.state === "early_close" ? session.reason || "Shortened session ahead" : "Regular cash session",
      tone: "market-warn",
      phase: `${marketName} pre-open`,
      label: "Opens",
      countdown: untilOpen || "--:--:--",
      meta: marketClockMeta("Open", session, open),
      side: marketSessionNow(session),
    };
  }

  if (close && nextOpen && now >= close.getTime()) {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Next regular cash session",
      tone: "market-closed",
      phase: stateText === "early_close" ? `${marketName} after early close` : `${marketName} after close`,
      label: "Opens",
      countdown: untilOpen || "--:--:--",
      meta: marketClockMeta("Next", session, nextOpen),
      side: marketSessionNow(session),
    };
  }

  if (stateText === "holiday") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Official market holiday",
      tone: "market-closed",
      phase: `${marketName} holiday`,
      label: "Opens",
      countdown: untilOpen || "--:--:--",
      meta: marketClockMeta("Next", session, nextOpen),
      side: marketSessionNow(session),
    };
  }

  if (stateText === "closed") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason === "weekend" ? "Weekend closure" : `Outside regular cash session${reason}`,
      tone: "market-closed",
      phase: session.reason === "weekend" ? `${marketName} weekend` : marketClosedPhase(marketName),
      label: "Opens",
      countdown: untilOpen || "--:--:--",
      meta: marketClockMeta("Next", session, nextOpen),
      side: marketSessionNow(session),
    };
  }

  if (stateText === "unknown") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: `Calendar coverage unavailable${reason}`,
      tone: "market-warn",
      phase: `${marketName} unknown`,
      label: "Next",
      countdown: untilOpen || "--:--:--",
      meta: marketClockMeta("Session", session, nextOpen),
      side: marketSessionNow(session),
    };
  }

  const untilOpen = countdownLabel(nextOpen);
  return {
    text: session.reason || `Official calendar${reason}`,
    tone: "market-warn",
    phase: `${marketName} ${cleanDetail(session.state)}`,
    label: "Opens",
    countdown: untilOpen || "--:--:--",
    meta: marketClockMeta("Open", session, nextOpen),
    side: marketSessionNow(session),
  };
}

function marketCalendarShortLabel(calendar, session) {
  const raw = String(calendar?.label || session?.label || session?.market || calendar?.market || "").toLowerCase();
  if (raw.includes("xetra") || raw.includes("de_") || raw === "de") return "Xetra";
  if (raw.includes("option")) return "US options";
  return "US";
}

function marketOpenPhase(marketName) {
  return marketName === "US" ? "US market open" : `${marketName} open`;
}

function marketClosedPhase(marketName) {
  return marketName === "US" ? "US market closed" : `${marketName} closed`;
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

function marketClockMeta(label, session, target) {
  if (!target) return "HH:MM:SS";
  return "HH:MM:SS";
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

function renderOrderReview() {
  const panel = $("orderReviewPanel");
  const set = activeOrderReviewSet();
  const shouldShow = Boolean(set || state.orderReviewLoading || state.orderReviewError || state.orderPreview);
  panel.hidden = !shouldShow;
  if (!shouldShow) return;

  const trading = set?.capabilities || state.snapshot?.trading || {};
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

  const selected = rows.filter((row) => rowEdit(row).included && rowEdit(row).quantity > 0);
  const totalQty = selected.reduce((sum, row) => sum + rowEdit(row).quantity, 0);
  $("orderReviewSummary").textContent = selected.length === 0
    ? "No selected rows"
    : `${selected.length} selected / ${totalQty} units`;
  $("resetOrderReviewButton").disabled = !set || state.orderReviewLoading;
  $("previewOrdersButton").disabled = !set || !trading.can_preview || selected.length === 0 || state.orderReviewLoading;
  const previewSubmitReady = state.orderPreview?.set_id === set?.id && state.orderPreview?.set_revision === set?.revision && state.orderPreview?.submit_ready;
  $("transmitSelectedButton").disabled = !set || !trading.can_transmit || !previewSubmitReady;
  $("transmitSelectedButton").title = transmitDisabledReason(trading, previewSubmitReady);

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
  const preview = state.orderPreview;
  if (!preview && !state.orderReviewError) {
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
      const title = document.createElement("b");
      title.textContent = row.draft
        ? `${row.draft.action} ${row.quantity} ${contractLabel(row.draft.contract)}`
        : `${row.row_id} / ${row.quantity}`;
      const detail = document.createElement("p");
      detail.textContent = previewRowLine(row);
      const token = document.createElement("small");
      token.textContent = row.preview?.preview_token_id ? `token ${row.preview.preview_token_id}` : "no submit token";
      item.append(title, detail, token);
      children.push(item);
    }
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
  list.replaceChildren(...orders.map((order) => {
    const row = document.createElement("div");
    row.className = "open-order-row";
    const title = document.createElement("b");
    title.textContent = `${order.action || "--"} ${order.quantity || "--"} ${order.symbol || order.order_ref || "--"}`;
    const meta = document.createElement("span");
    meta.textContent = [order.lifecycle_status, order.send_state, order.order_ref].filter(Boolean).join(" / ") || "journal view";
    const controls = document.createElement("div");
    controls.className = "open-order-row__controls";
    const trading = state.snapshot?.trading || {};
    controls.append(disabledOrderButton("Modify", trading.can_modify && order.modify_eligible === true));
    controls.append(disabledOrderButton("Cancel", trading.can_cancel && order.cancel_eligible === true));
    row.append(title, meta, controls);
    return row;
  }));
}

function disabledOrderButton(label, enabled) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "text-button";
  button.textContent = label;
  button.disabled = !enabled;
  button.title = enabled ? label : `${label} is not enabled by trading.status`;
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
  state.orderReviewError = "";
  renderOrderReview();
}

async function refreshOrderReviewSet() {
  state.orderReviewLoading = true;
  state.orderReviewError = "";
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
    await refreshOpenOrders();
  } catch (err) {
    state.orderReviewError = err.message;
  } finally {
    state.orderReviewLoading = false;
    renderOrderReview();
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

function transmitDisabledReason(trading, previewSubmitReady) {
  if (!trading.can_transmit) return "Transmit is not enabled by trading.status";
  if (!previewSubmitReady) return "Preview selected rows first; every selected row must be submit eligible";
  return "Ready when backend transmit is enabled";
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
$("clearSelectedAlertButton").addEventListener("click", () => {
  state.selectedAlertID = null;
  renderAlerts();
  renderSelectedAlert();
});
$("refreshOrderReviewButton").addEventListener("click", refreshOrderReviewSet);
$("resetOrderReviewButton").addEventListener("click", resetOrderReviewEdits);
$("previewOrdersButton").addEventListener("click", previewOrderReviewSet);
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
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
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
  return sign + value.toFixed(1) + "%";
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
  $("statusDot").setAttribute("aria-label", ok ? "app connected" : "app reconnecting");
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
