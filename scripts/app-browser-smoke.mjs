#!/usr/bin/env node

import { createRequire } from "node:module";
import { execFile } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

const args = parseArgs(process.argv.slice(2));
const baseURL = trimRight(args["base-url"] || "http://127.0.0.1:8765", "/");
const pairPublicURL = trimRight(args["pair-public-url"] || baseURL, "/");
const browserName = args.browser || "chromium";
const channel = args.channel || process.env.PLAYWRIGHT_CHANNEL || "";
const noNotification = args["no-notification"] !== "false";
const noWebCrypto = args["no-webcrypto"] === "true";
const lifecycle = args.lifecycle === "true";
const restartCommand = args["restart-command"] || "";
const stopRestartedApp = args["stop-restarted-app"] === "true";
const mobile = args.mobile !== "false";

const playwright = loadPlaywright();

if (!playwright[browserName]) {
  console.error(`app-browser-smoke: unknown browser ${browserName}`);
  process.exit(2);
}

const pairing = await createPairingSession(baseURL, pairPublicURL);
const launchOptions = { headless: true };
if (channel) {
  launchOptions.channel = channel;
}
const launched = await launchBrowser(playwright[browserName], launchOptions);
const browser = launched.browser;
let cleanupPID = 0;
const context = await browser.newContext({
  viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 },
  isMobile: mobile,
  hasTouch: mobile,
});
if (noNotification) {
  await context.addInitScript(() => {
    try {
      Object.defineProperty(globalThis, "Notification", {
        configurable: true,
        value: undefined,
      });
    } catch {
      // Some engines make host globals non-configurable. The smoke still
      // catches ordinary browser errors through pageerror/console.
    }
  });
}
if (noWebCrypto) {
  await context.addInitScript(() => {
    try {
      const proto = Object.getPrototypeOf(globalThis.crypto);
      Object.defineProperty(proto, "subtle", {
        configurable: true,
        get() {
          return undefined;
        },
      });
    } catch {
      try {
        Object.defineProperty(globalThis.crypto, "subtle", {
          configurable: true,
          value: undefined,
        });
      } catch {
        // The final JSON reports whether the fallback path was used.
      }
    }
  });
}
await context.addInitScript(() => {
  globalThis.__ibkrSmoke = {
    eventCounts: {},
    fetches: [],
    openedEvents: 0,
  };
  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (...fetchArgs) => {
    const request = fetchArgs[0];
    const url = typeof request === "string" ? request : request?.url || "";
    try {
      const res = await nativeFetch(...fetchArgs);
      globalThis.__ibkrSmoke.fetches.push({ url, status: res.status, at: Date.now() });
      if (res.ok && url.endsWith("/api/bootstrap")) {
        res.clone().json().then((body) => {
          globalThis.__ibkrSmoke.latestCanaryHeldStress = body?.snapshot?.canary?.portfolio?.held_stress?.length || 0;
        }).catch(() => {});
      }
      return res;
    } catch (err) {
      globalThis.__ibkrSmoke.fetches.push({ url, error: String(err?.message || err), at: Date.now() });
      throw err;
    }
  };
  const NativeEventSource = globalThis.EventSource;
  globalThis.EventSource = function smokeEventSource(url, options) {
    const es = new NativeEventSource(url, options);
    globalThis.__ibkrSmoke.openedEvents++;
    for (const type of ["snapshot", "status", "market_calendar", "account", "positions", "market_quotes", "canary", "heartbeat"]) {
      es.addEventListener(type, (event) => {
        globalThis.__ibkrSmoke.eventCounts[type] = (globalThis.__ibkrSmoke.eventCounts[type] || 0) + 1;
        if (type === "snapshot" || type === "canary") {
          try {
            const data = JSON.parse(event.data);
            const canary = type === "snapshot" ? data?.canary : data;
            globalThis.__ibkrSmoke.latestCanaryHeldStress = canary?.portfolio?.held_stress?.length || 0;
          } catch {
            // Smoke assertions below stay DOM-based when payload inspection fails.
          }
        }
      });
    }
    return es;
  };
  globalThis.EventSource.prototype = NativeEventSource.prototype;
});
const page = await context.newPage();
const consoleMessages = [];
const pageErrors = [];
page.on("console", (msg) => {
  if (msg.type() === "error" || msg.type() === "warning") {
    consoleMessages.push(`${msg.type()}: ${msg.text()}`);
  }
});
page.on("pageerror", (err) => pageErrors.push(String(err?.message || err)));

try {
  await page.goto(pairing.url, { waitUntil: "domcontentloaded", timeout: 15000 });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await waitForSnapshotEvent(page, 0);
  const title = await page.title();
  const connection = await waitForHeader(page);
  const pushState = await page.locator("#pushState").textContent();
  const eventsBefore = await fetchEventsDiagnostics(page);
  const privacy = await exerciseAccountPrivacy(page);
  const accountMenu = await exerciseAccountMenu(page);
  const marketLayout = await exerciseMarketLayout(page);
  const canaryControls = await exerciseCanaryControlsRemoved(page);
  const underlyingBookFixture = await exerciseCanaryUnderlyingBookFixture(page);
  const canaryDetail = await exerciseCanaryDetail(page);
  const marketContext = await exerciseMarketContext(page);
  const portfolioDetail = await exercisePortfolioDetail(page);
  const alertHistory = await exerciseAlertHistory(page);
  const openOrders = await exerciseOpenOrders(page);
  await openDebugTools(page);
  await page.locator('[data-tool="snapshot"]').click();
  await page.waitForFunction(() => {
    const out = document.getElementById("toolOutput");
    return out && out.textContent && out.textContent.trim() !== "{}";
  }, { timeout: 10000 });
  if (noNotification && pushState !== "push unsupported") {
    throw new Error(`expected push unsupported with Notification removed, got ${JSON.stringify(pushState)}`);
  }
  if (pageErrors.length > 0 || consoleMessages.length > 0) {
    throw new Error(`browser errors:\n${[...pageErrors, ...consoleMessages].join("\n")}`);
  }
  let lifecycleResult = null;
  if (lifecycle) {
    lifecycleResult = await runLifecycleSmoke(page);
  }
  const smokeState = await page.evaluate(() => globalThis.__ibkrSmoke);
  const fallbackDeviceSecretStored = await page.evaluate(() => !!localStorage.getItem("ibkrDeviceSecret"));
  console.log(JSON.stringify({
    ok: true,
    browser: browserName,
    channel: launched.channel || null,
    base_url: baseURL,
    mobile,
    notification_removed: noNotification,
    webcrypto_removed: noWebCrypto,
    used_http_fallback: fallbackDeviceSecretStored,
    title,
    connection,
    push_state: pushState,
    privacy,
    account_menu: accountMenu,
    market_layout: marketLayout,
    canary_controls: canaryControls,
    underlying_book_fixture: underlyingBookFixture,
    canary_detail: canaryDetail,
    market_context: marketContext,
    portfolio_detail: portfolioDetail,
    alert_history: alertHistory,
    open_orders: openOrders,
    events: {
      subscribers: eventsBefore.subscribers,
      last_event_at: eventsBefore.last_event_at,
      event_counts: smokeState.eventCounts,
    },
    lifecycle: lifecycleResult,
    pair_expires_at: pairing.expires_at,
  }, null, 2));
} finally {
  await browser.close();
  if (stopRestartedApp && cleanupPID) {
    try {
      process.kill(cleanupPID, "SIGTERM");
    } catch {
      // Best effort cleanup for isolated lifecycle smoke.
    }
  }
}

async function runLifecycleSmoke(page) {
  if (!restartCommand) {
    throw new Error("--restart-command is required with --lifecycle=true");
  }
  const before = await page.evaluate(() => ({
    snapshot: globalThis.__ibkrSmoke.eventCounts.snapshot || 0,
    authSessions: globalThis.__ibkrSmoke.fetches.filter((f) => f.url.endsWith("/api/auth/session") && f.status === 200).length,
  }));
  const connectionBeforeRestart = await waitForHeader(page);
  const restart = await runShellJSON(restartCommand);
  const snapshotAfter = await waitForSnapshotEvent(page, before.snapshot);
  const connectionAfterRestart = await waitForHeader(page);
  const eventsAfter = await fetchEventsDiagnostics(page);
  const after = await page.evaluate(() => ({
    authSessions: globalThis.__ibkrSmoke.fetches.filter((f) => f.url.endsWith("/api/auth/session") && f.status === 200).length,
  }));
  if (eventsAfter.subscribers < 1) {
    throw new Error(`expected at least one SSE subscriber after restart, got ${eventsAfter.subscribers}`);
  }
  return {
    connection_before_restart: connectionBeforeRestart,
    connection_after_restart: connectionAfterRestart,
    reauth_after_restart: after.authSessions > before.authSessions,
    snapshot_events_after_restart: snapshotAfter,
    restart,
    events: {
      subscribers: eventsAfter.subscribers,
      last_event_at: eventsAfter.last_event_at,
    },
  };
}

async function waitForSnapshotEvent(page, previousCount) {
  await page.waitForFunction((count) => {
    return (globalThis.__ibkrSmoke?.eventCounts?.snapshot || 0) > count;
  }, previousCount, { timeout: 20000 });
  return page.evaluate(() => globalThis.__ibkrSmoke.eventCounts.snapshot || 0);
}

async function waitForHeader(page) {
  await page.waitForFunction(() => {
    const text = document.getElementById("connectionLine")?.textContent?.trim() || "";
    const dot = document.getElementById("statusDot");
    return text && text !== "Connecting" && text !== "Market calendar loading" && dot?.classList.contains("ok");
  }, { timeout: 20000 });
  return page.locator("#connectionLine").textContent();
}

async function fetchEventsDiagnostics(page) {
  const events = await page.evaluate(async () => {
    const res = await fetch("/api/tools/events", { method: "POST", credentials: "include" });
    if (!res.ok) {
      throw new Error(`events tool failed ${res.status}: ${await res.text()}`);
    }
    return res.json();
  });
  if (events.subscribers < 1) {
    throw new Error(`expected at least one SSE subscriber, got ${events.subscribers}`);
  }
  return events;
}

async function exerciseAccountPrivacy(page) {
  const menuWasOpen = await page.locator("#accountMenu").evaluate((el) => !el.hidden);
  if (!menuWasOpen) {
    await page.locator("#accountMenuToggle").click();
    await page.waitForFunction(() => {
      const panel = document.getElementById("accountMenu");
      return panel && !panel.hidden;
    }, { timeout: 5000 });
  }
  const value = page.locator("#netLiquidation");
  const before = (await value.textContent())?.trim();
  if (before === "--") {
    if (!menuWasOpen) {
      await page.locator("#accountMenuToggle").click();
      await page.waitForFunction(() => document.getElementById("accountMenu")?.hidden, { timeout: 5000 });
    }
    return { masked_by_default: false, toggle_reveals: false, no_value: true };
  }
  if (before !== "******") {
    throw new Error(`net liquidation should be masked by default, got ${JSON.stringify(before)}`);
  }
  await page.locator("#accountPrivacyToggle").click();
  await page.waitForFunction(() => {
    const text = document.getElementById("netLiquidation")?.textContent?.trim();
    return text && text !== "******" && text !== "--";
  }, { timeout: 5000 });
  await page.locator("#accountPrivacyToggle").click();
  await page.waitForFunction(() => document.getElementById("netLiquidation")?.textContent?.trim() === "******", { timeout: 5000 });
  if (!menuWasOpen) {
    await page.locator("#accountMenuToggle").click();
    await page.waitForFunction(() => document.getElementById("accountMenu")?.hidden, { timeout: 5000 });
  }
  return { masked_by_default: true, toggle_reveals: true };
}

async function exerciseAccountMenu(page) {
  await page.locator("#accountMenuToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("accountMenu");
    return panel && !panel.hidden && document.getElementById("accountContextLine")?.textContent?.trim();
  }, { timeout: 5000 });
  const menu = await page.evaluate(() => ({
    expanded: document.getElementById("accountMenuToggle")?.getAttribute("aria-expanded") === "true",
    context: document.getElementById("accountContextLine")?.textContent?.trim() || "",
    environment: document.getElementById("accountEnvironment")?.textContent?.trim() || "",
    orderAccount: document.getElementById("orderAccountLabel")?.textContent?.trim() || "",
    pill: document.getElementById("tradingEnvPill")?.textContent?.trim() || "",
    chip: document.getElementById("accountChipText")?.textContent?.trim() || "",
    accountHasUnderlyingBook: !!document.querySelector("#accountMenu #underlyingBookList"),
  }));
  if (!menu.expanded) {
    throw new Error("account menu did not mark itself expanded");
  }
  if (!menu.context || !menu.environment || !menu.orderAccount || !menu.pill || !menu.chip) {
    throw new Error(`account menu is missing values: ${JSON.stringify(menu)}`);
  }
  if (menu.accountHasUnderlyingBook) {
    throw new Error("account menu should not contain the underlyings subledger");
  }
  await page.locator("#accountMenuToggle").click();
  await page.waitForFunction(() => document.getElementById("accountMenu")?.hidden, { timeout: 5000 });
  return {
    opens: true,
    context_present: menu.context !== "Waiting for account context",
    environment: menu.environment,
    order_account: menu.orderAccount,
    chip: menu.chip,
    account_has_underlying_book: menu.accountHasUnderlyingBook,
  };
}

async function exerciseMarketLayout(page) {
  await page.waitForFunction(() => {
    const text = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    return /\b(closing|opening) in\b/i.test(text);
  }, { timeout: 10000 });
  const layout = await page.evaluate(() => {
    const marketPanel = document.getElementById("marketPanel");
    const canaryPanel = document.getElementById("canaryHero");
    const marketStrip = document.querySelector(".market-strip");
    const accountMenu = document.getElementById("accountMenu");
    const marketBeforeCanary = !!(marketPanel && canaryPanel && (marketPanel.compareDocumentPosition(canaryPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const accountAfterMarketStrip = !!(marketStrip && accountMenu && (marketStrip.compareDocumentPosition(accountMenu) & Node.DOCUMENT_POSITION_FOLLOWING));
    const phase = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    const strip = document.querySelector(".market-strip");
    const marketOpen = strip?.classList.contains("market-open") || false;
    const accountHasUnderlyingBook = !!document.querySelector("#accountMenu #underlyingBookList");
    const canaryHasUnderlyingBook = !!document.querySelector("#canaryHero #underlyingBookList");
    return { marketBeforeCanary, accountAfterMarketStrip, phase, marketOpen, accountHasUnderlyingBook, canaryHasUnderlyingBook };
  });
  if (!layout.marketBeforeCanary) {
    throw new Error("Market Context should appear before Portfolio Canary in DOM order");
  }
  if (!layout.accountAfterMarketStrip) {
    throw new Error("Account panel should appear below the market countdown in DOM order");
  }
  if (layout.marketOpen && !/\bclosing in\b/i.test(layout.phase)) {
    throw new Error(`open market line should contain closing in: ${JSON.stringify(layout.phase)}`);
  }
  if (!layout.marketOpen && !/\bopening in\b/i.test(layout.phase)) {
    throw new Error(`closed market line should contain opening in: ${JSON.stringify(layout.phase)}`);
  }
  if (layout.accountHasUnderlyingBook) {
    throw new Error("Account menu still contains the underlyings subledger");
  }
  if (!layout.canaryHasUnderlyingBook) {
    throw new Error("Portfolio Canary is missing the underlyings subledger");
  }
  if (/\b(Xetra|US market|US equities|US options)\b/i.test(layout.phase)) {
    throw new Error(`market line should not repeat the selected market label: ${JSON.stringify(layout.phase)}`);
  }
  return layout;
}

async function exerciseCanaryControlsRemoved(page) {
  const counts = await page.evaluate(() => ({
    chipRows: document.querySelectorAll(".canary-chip-row").length,
    chips: document.querySelectorAll(".canary-chip").length,
    warningToggle: document.querySelectorAll("#canaryWarningsToggle").length,
    checksToggle: document.querySelectorAll("#canaryChecksToggle").length,
    inlineDetail: document.querySelectorAll("#canaryInlineDetailPanel").length,
  }));
  const total = Object.values(counts).reduce((sum, count) => sum + count, 0);
  if (total > 0) {
    throw new Error(`canary summary controls should be removed: ${JSON.stringify(counts)}`);
  }
  return counts;
}

async function exerciseCanaryUnderlyingBookFixture(page) {
  await page.evaluate(() => {
    localStorage.setItem("ibkrPurgeBook", JSON.stringify({
      purge_id: "purge_ui_fixture",
      base_currency: "USD",
      legs: [{
        symbol: "MSFT",
        sec_type: "STK",
        currency: "USD",
        current_price: 444.12,
        current_price_source: "fixture quote",
        quote_change_pct: -0.7,
        shadow_saved: 125.5,
        status: "priced",
      }],
    }));
  });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  await page.waitForFunction(() => {
    return document.querySelector("#canaryHero #underlyingBookList .underlying-row");
  }, { timeout: 5000 });
  const info = await page.evaluate(() => ({
    count: document.getElementById("underlyingBookCount")?.textContent?.trim() || "",
    status: document.getElementById("underlyingBookStatus")?.textContent?.trim() || "",
    accountHasUnderlyingBook: !!document.querySelector("#accountMenu #underlyingBookList"),
    canaryHasUnderlyingBook: !!document.querySelector("#canaryHero #underlyingBookList"),
    rows: [...document.querySelectorAll("#canaryHero #underlyingBookList .underlying-row")].map((row) => ({
      symbol: row.dataset.symbol || "",
      virtual: row.classList.contains("underlying-row--virtual"),
      markers: [...row.querySelectorAll(".underlying-marker")].map((marker) => marker.textContent?.trim() || ""),
      buttons: [...row.querySelectorAll("button")].map((button) => ({
        text: button.textContent?.trim() || "",
        disabled: button.disabled,
        title: button.title || "",
      })),
      text: row.textContent?.replace(/\s+/g, " ").trim() || "",
    })),
  }));
  if (info.accountHasUnderlyingBook || !info.canaryHasUnderlyingBook) {
    throw new Error(`underlyings subledger is in the wrong panel: ${JSON.stringify(info)}`);
  }
  const row = info.rows.find((item) => item.symbol === "MSFT");
  if (!row || !row.virtual) {
    throw new Error(`virtual purge row is missing: ${JSON.stringify(info)}`);
  }
  for (const marker of ["Virtual", "Purged"]) {
    if (!row.markers.includes(marker)) {
      throw new Error(`virtual purge row lacks ${marker} marker: ${JSON.stringify(row)}`);
    }
  }
  const purge = row.buttons.find((button) => button.text === "Purge");
  const restore = row.buttons.find((button) => button.text === "Restore");
  const rebuild = row.buttons.find((button) => button.text === "Rebuild");
  if (!purge?.disabled || !purge.title) {
    throw new Error(`purged row should disable Purge with a reason: ${JSON.stringify(row.buttons)}`);
  }
  if (!restore || restore.disabled || !rebuild || rebuild.disabled) {
    throw new Error(`purged row should enable Restore and Rebuild: ${JSON.stringify(row.buttons)}`);
  }

  await page.locator('#canaryHero #underlyingBookList .underlying-row[data-symbol="MSFT"] .underlying-action--restore').click();
  await page.waitForFunction(() => document.getElementById("underlyingBookStatus")?.textContent?.includes("Restore placeholder"), { timeout: 5000 });
  const notice = (await page.locator("#underlyingBookStatus").textContent())?.trim() || "";

  await page.evaluate(() => localStorage.removeItem("ibkrPurgeBook"));
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });

  return {
    virtual_rows: info.rows.length,
    count: info.count,
    markers: row.markers,
    purge_disabled: purge.disabled,
    restore_enabled: !restore.disabled,
    rebuild_enabled: !rebuild.disabled,
    placeholder_notice: notice,
  };
}

async function exerciseCanaryDetail(page) {
  let timestamp = (await page.locator("#canaryAsOf").textContent())?.trim() || "";
  if ((timestamp === "no timestamp" || timestamp === "updated --") && !lifecycle) {
    try {
      await page.waitForFunction(() => {
        const text = document.getElementById("canaryAsOf")?.textContent?.trim() || "";
        return text && text !== "no timestamp" && text !== "updated --" && text !== "--";
      }, { timeout: 30000 });
      timestamp = (await page.locator("#canaryAsOf").textContent())?.trim() || "";
    } catch {
      // Keep the explicit assertion below for app instances without live canary data.
    }
  }
  if (!timestamp || timestamp === "--" || timestamp === "updated --" || (!lifecycle && timestamp === "no timestamp")) {
    throw new Error("canary timestamp is missing");
  }
  await page.locator("#canaryDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("canaryDetailPanel");
    return panel && !panel.hidden && document.getElementById("canaryDetailGrid")?.children.length >= 4;
  }, { timeout: 5000 });
  const counts = await page.evaluate(() => ({
    cards: document.getElementById("canaryDetailGrid")?.children.length || 0,
    drivers: document.getElementById("canaryDrivers")?.children.length || 0,
    held_stress: document.getElementById("heldStressList")?.children.length || 0,
    held_stress_payload: globalThis.__ibkrSmoke?.latestCanaryHeldStress || 0,
  }));
  if (counts.held_stress_payload > 0 && counts.held_stress === 0) {
    throw new Error("canary held_stress payload is present but detail panel did not render it");
  }
  await page.locator("#canaryDetailToggle").click();
  return { opens: true, timestamp, cards: counts.cards, drivers: counts.drivers, held_stress: counts.held_stress };
}

async function exerciseMarketContext(page) {
  let before = await readMarketContext(page);
  if ((!before.regime || before.regime === "--") && !lifecycle) {
    try {
      await page.waitForFunction(() => {
        const text = document.getElementById("marketRegime")?.textContent?.trim() || "";
        return text && text !== "--";
      }, { timeout: 10000 });
      before = await readMarketContext(page);
    } catch {
      // Keep the no-value assertion below for app instances without live data.
    }
  }
  for (const [symbol, level, change, note] of [
    ["SPY", before.spyLevel, before.spyChange, before.spyNote],
    ["VIX", before.vixLevel, before.vixChange, before.vixNote],
    ["QQQ", before.qqqLevel, before.qqqChange, before.qqqNote],
  ]) {
    if (level !== "--" && (!change || change === "--") && !/change pending/i.test(note)) {
      throw new Error(`${symbol} has a level but no percent-change explanation: ${JSON.stringify({ change, note })}`);
    }
  }
  for (const [symbol, level, note] of [
    ["SPY", before.spyLevel, before.spyNote],
    ["VIX", before.vixLevel, before.vixNote],
    ["QQQ", before.qqqLevel, before.qqqNote],
  ]) {
    if (level === "--" && !new RegExp(`No ${symbol} (data|price)`).test(note)) {
      throw new Error(`${symbol} missing data should be explicit, got ${JSON.stringify(note)}`);
    }
  }
  for (const [label, value] of [
    ["SPY level", before.spyLevel],
    ["VIX level", before.vixLevel],
    ["QQQ level", before.qqqLevel],
  ]) {
    if (value !== "--" && !/^-?[\d.,]+[.,]\d{2}$/.test(value)) {
      throw new Error(`${label} should use two decimal places, got ${JSON.stringify(value)}`);
    }
  }
  for (const [label, value] of [
    ["SPY change", before.spyChange],
    ["VIX change", before.vixChange],
    ["QQQ change", before.qqqChange],
  ]) {
    if (value && value !== "--" && !/^[+-]?[\d.,]+[.,]\d{2}%$/.test(value)) {
      throw new Error(`${label} should use two decimal places, got ${JSON.stringify(value)}`);
    }
  }
  if (before.sparklineCount !== 0) {
    throw new Error(`market context should not render invented sparklines, found ${before.sparklineCount}`);
  }
  if (before.rangeCount !== 3) {
    throw new Error(`market context should render three compact quote range markers, found ${before.rangeCount}`);
  }
  if (before.rangeText.some(Boolean)) {
    throw new Error(`market quote range markers should not render fallback text, got ${JSON.stringify(before.rangeText)}`);
  }
  if (!before.regime || before.regime === "--") {
    if (before.weather !== "weather-na") {
      throw new Error(`empty market regime should use weather-na, got ${JSON.stringify(before.weather)}`);
    }
    return {
      no_value: true,
      weather: before.weather,
      spy_level_present: before.spyLevel !== "--",
      qqq_level_present: before.qqqLevel !== "--",
      vix_level_present: before.vixLevel !== "--",
      indicators: 0,
    };
  }
  if (!["weather-green", "weather-amber", "weather-red"].includes(before.weather)) {
    throw new Error(`market weather is not color coded, got ${JSON.stringify(before.weather)}`);
  }
  await page.locator("#marketRegimeToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("regimeDetailPanel");
    return panel && !panel.hidden;
  }, { timeout: 5000 });
  const indicators = await page.evaluate(() => document.getElementById("regimeIndicators")?.children.length || 0);
  if (indicators === 0) {
    throw new Error("market regime detail is empty");
  }
  await page.locator("#marketRegimeToggle").click();
  return {
    regime: before.regime,
    weather: before.weather,
    spy_level_present: before.spyLevel !== "--",
    qqq_level_present: before.qqqLevel !== "--",
    vix_level_present: before.vixLevel !== "--",
    indicators,
  };
}

async function exercisePortfolioDetail(page) {
  const summary = (await page.locator("#portfolioDetailSummary").textContent())?.trim() || "";
  await page.locator("#portfolioDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("portfolioDetailPanel");
    return panel && !panel.hidden && document.getElementById("portfolioDetailList")?.children.length >= 4;
  }, { timeout: 5000 });
  const detail = await page.evaluate(() => ({
    rows: document.getElementById("portfolioDetailList")?.children.length || 0,
    text: document.getElementById("portfolioDetailList")?.textContent || "",
  }));
  if (!detail.text.includes("option legs") && !detail.text.includes("No option legs")) {
    throw new Error("portfolio detail does not explain Greeks coverage");
  }
  await page.locator("#portfolioDetailToggle").click();
  return { opens: true, summary, rows: detail.rows };
}

async function exerciseAlertHistory(page) {
  const initiallyOpen = await page.locator("#alertsPanel").evaluate((el) => !!el.open);
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
    await page.waitForFunction(() => document.getElementById("alertsPanel")?.open, { timeout: 5000 });
  }
  const info = await page.evaluate(() => ({
    count: Number.parseInt(document.getElementById("alertCount")?.textContent || "0", 10) || 0,
    rows: document.querySelectorAll("#alertsList .alert-row").length,
    clearDisabled: document.getElementById("clearAlertsButton")?.disabled || false,
    hint: document.getElementById("alertsHint")?.textContent || "",
  }));
  let selected = false;
  if (info.rows > 0) {
    await page.locator("#alertsList .alert-row").first().click();
    await page.waitForFunction(() => {
      const panel = document.getElementById("selectedAlertPanel");
      return panel && !panel.hidden && document.getElementById("selectedAlertTitle")?.textContent?.trim();
    }, { timeout: 5000 });
    selected = true;
  }
  if (info.count === 0 && !info.clearDisabled) {
    throw new Error("clear alert history should be disabled when there are no rows");
  }
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
  }
  return { initially_open: initiallyOpen, opens: true, count: info.count, rows: info.rows, selected };
}

async function exerciseOpenOrders(page) {
  const initiallyOpen = await page.locator("#ordersPanel").evaluate((el) => !!el.open);
  if (!initiallyOpen) {
    await page.locator("#ordersPanel summary").click();
    await page.waitForFunction(() => document.getElementById("ordersPanel")?.open, { timeout: 5000 });
  }
  const info = await page.evaluate(() => {
    const buttons = [...document.querySelectorAll("#ordersOpenList button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    }));
    return {
      rows: document.querySelectorAll("#ordersOpenList .open-order-row").length,
      empty: document.getElementById("ordersOpenList")?.textContent?.includes("No open journal-backed orders.") || false,
      buttons,
      oldLabels: buttons.map((button) => button.text).filter((label) => ["Modify", "Cancel", "Execute"].includes(label)),
    };
  });
  if (info.oldLabels.length > 0) {
    throw new Error(`open-order controls still use old labels: ${info.oldLabels.join(", ")}`);
  }
  if (info.rows === 0 && !info.empty) {
    throw new Error("open-order empty state is missing");
  }
  if (info.rows > 0) {
    for (const label of ["Preview change", "Apply change", "Cancel order"]) {
      if (!info.buttons.some((button) => button.text === label)) {
        throw new Error(`open-order control ${JSON.stringify(label)} is missing`);
      }
    }
    for (const button of info.buttons.filter((item) => item.disabled)) {
      if (!button.title) {
        throw new Error(`disabled open-order control lacks a reason: ${JSON.stringify(button.text)}`);
      }
    }
  }
  if (!initiallyOpen) {
    await page.locator("#ordersPanel summary").click();
  }
  return {
    initially_open: initiallyOpen,
    rows: info.rows,
    empty: info.empty,
    buttons: info.buttons.map((button) => ({ text: button.text, disabled: button.disabled, has_reason: !!button.title })),
  };
}

async function readMarketContext(page) {
  return page.evaluate(() => ({
    spyLevel: document.getElementById("spyLevel")?.textContent?.trim() || "",
    spyChange: document.getElementById("spyChange")?.textContent?.trim() || "",
    spyNote: document.getElementById("spyNote")?.textContent?.trim() || "",
    qqqLevel: document.getElementById("nasdaqLevel")?.textContent?.trim() || "",
    qqqChange: document.getElementById("nasdaqChange")?.textContent?.trim() || "",
    qqqNote: document.getElementById("nasdaqNote")?.textContent?.trim() || "",
    vixLevel: document.getElementById("vixLevel")?.textContent?.trim() || "",
    vixChange: document.getElementById("vixChange")?.textContent?.trim() || "",
    vixNote: document.getElementById("vixNote")?.textContent?.trim() || "",
    rangeText: [...document.querySelectorAll(".quote-range")].map((node) => node.textContent?.trim() || ""),
    rangeCount: document.querySelectorAll(".quote-range").length,
    sparklineCount: document.querySelectorAll(".sparkline").length,
    regime: document.getElementById("marketRegime")?.textContent?.trim() || "",
    weather: [...(document.getElementById("marketRegimeToggle")?.classList || [])].find((name) => name.startsWith("weather-")) || "",
  }));
}

async function openDebugTools(page) {
  const tools = page.locator("#toolsPanel");
  await tools.evaluate((el) => {
    if ("open" in el) {
      el.open = true;
    }
  });
}

async function runShellJSON(command) {
  const started = Date.now();
  const { stdout, stderr } = await execFilePromise("/bin/sh", ["-lc", command]);
  if (stderr.trim()) {
    console.error(stderr.trim());
  }
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch (err) {
    throw new Error(`restart command did not emit JSON: ${String(err?.message || err)}\n${stdout}`);
  }
  cleanupPID = parsed.new_pid || 0;
  return { ...parsed, smoke_elapsed_ms: Date.now() - started };
}

function execFilePromise(file, argv) {
  return new Promise((resolve, reject) => {
    execFile(file, argv, { timeout: 30000, maxBuffer: 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        reject(new Error(`${file} ${argv.join(" ")} failed: ${String(err?.message || err)}\n${stderr}`));
        return;
      }
      resolve({ stdout, stderr });
    });
  });
}

async function launchBrowser(browserType, launchOptions) {
  try {
    return {
      browser: await browserType.launch(launchOptions),
      channel: launchOptions.channel || "",
    };
  } catch (err) {
    if (launchOptions.channel || browserName !== "chromium") {
      throw err;
    }
    try {
      return {
        browser: await browserType.launch({ ...launchOptions, channel: "chrome" }),
        channel: "chrome",
      };
    } catch (chromeErr) {
      throw new Error([
        "chromium launch failed, and the fallback to installed Chrome also failed.",
        `chromium: ${String(err?.message || err)}`,
        `chrome: ${String(chromeErr?.message || chromeErr)}`,
      ].join("\n"));
    }
  }
}

function loadPlaywright() {
  const require = createRequire(import.meta.url);
  const errors = [];
  try {
    return require("playwright");
  } catch (err) {
    errors.push(`default resolution: ${String(err?.message || err)}`);
  }

  for (const moduleRoot of candidateModuleRoots()) {
    try {
      return require(path.join(moduleRoot, "playwright"));
    } catch (err) {
      errors.push(`${moduleRoot}: ${String(err?.message || err)}`);
    }
  }

  console.error("app-browser-smoke: Playwright is not installed for this Node runtime.");
  console.error("Install it with `npm install playwright`, or run inside Codex where the bundled runtime can be discovered.");
  console.error(errors.join("\n"));
  process.exit(2);
}

function candidateModuleRoots() {
  const roots = [];
  if (process.env.PLAYWRIGHT_NODE_MODULES) {
    roots.push(process.env.PLAYWRIGHT_NODE_MODULES);
  }
  if (process.env.NODE_PATH) {
    roots.push(...process.env.NODE_PATH.split(path.delimiter));
  }
  if (process.env.HOME) {
    roots.push(path.join(process.env.HOME, ".cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules"));
  }
  return [...new Set(roots)].filter((root) => root && fs.existsSync(path.join(root, "playwright")));
}

async function createPairingSession(baseURL, publicURL) {
  const res = await fetch(`${baseURL}/api/pairing/sessions`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ public_url: publicURL }),
  });
  if (!res.ok) {
    throw new Error(`create pairing session failed ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) {
      continue;
    }
    const [rawKey, inlineValue] = arg.slice(2).split("=", 2);
    if (inlineValue !== undefined) {
      out[rawKey] = inlineValue;
      continue;
    }
    if (i + 1 < argv.length && !argv[i + 1].startsWith("--")) {
      out[rawKey] = argv[++i];
      continue;
    }
    out[rawKey] = "true";
  }
  return out;
}

function trimRight(value, suffix) {
  while (value.endsWith(suffix)) {
    value = value.slice(0, -suffix.length);
  }
  return value;
}
