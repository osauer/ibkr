#!/usr/bin/env node

import { execFile } from "node:child_process";
import { createPairingSession, launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

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
const rawGatewayCopyPattern = /gateway_unavailable|ibkr connection unavailable|quote\.snapshot|account\.summary|positions\.list/i;

const playwright = loadPlaywright("app-browser-smoke");

if (!playwright[browserName]) {
  console.error(`app-browser-smoke: unknown browser ${browserName}`);
  process.exit(2);
}

const pairing = await createPairingSession(baseURL, pairPublicURL);
const launchOptions = { headless: true };
if (channel) {
  launchOptions.channel = channel;
}
const launched = await launchBrowser(playwright[browserName], browserName, launchOptions);
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
    const text = msg.text();
    if (/ERR_INCOMPLETE_CHUNKED_ENCODING/.test(text)) {
      return;
    }
    consoleMessages.push(`${msg.type()}: ${text}`);
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
  const accountPanel = await exerciseAccountPanel(page);
  const snapshotBanner = await assertSnapshotBannerCopy(page);
  const marketLayout = await exerciseMarketLayout(page);
  const viewportOverflow = await assertNoViewportOverflow(page);
  const canaryControls = await exerciseCanaryControlsRemoved(page);
  const underlyingBookFixture = await exerciseUnderlyingPanelFixture(page);
  const canaryDetail = await exerciseCanaryDetail(page);
  const marketContext = await exerciseMarketContext(page);
  const portfolioDetail = await exercisePortfolioDetail(page);
  const protectionRiskRendering = await exerciseProtectionRiskRendering(page);
  const alertHistory = await exerciseAlertHistory(page);
  const openOrders = await exerciseOpenOrders(page);
  const debugTools = await assertDebugToolsRemoved(page, baseURL);
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
    account_panel: accountPanel,
    snapshot_banner: snapshotBanner,
    market_layout: marketLayout,
    viewport_overflow: viewportOverflow,
    canary_controls: canaryControls,
    underlying_book_fixture: underlyingBookFixture,
    canary_detail: canaryDetail,
    market_context: marketContext,
    portfolio_detail: portfolioDetail,
    protection_risk_rendering: protectionRiskRendering,
    alert_history: alertHistory,
    open_orders: openOrders,
    debug_tools: debugTools,
    events: {
      opened_event_streams: eventsBefore.opened_event_streams,
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
  if (eventsAfter.opened_event_streams < 1) {
    throw new Error(`expected at least one SSE stream after restart, got ${eventsAfter.opened_event_streams}`);
  }
  return {
    connection_before_restart: connectionBeforeRestart,
    connection_after_restart: connectionAfterRestart,
    reauth_after_restart: after.authSessions > before.authSessions,
    snapshot_events_after_restart: snapshotAfter,
    restart,
    events: {
      opened_event_streams: eventsAfter.opened_event_streams,
      event_counts: eventsAfter.event_counts,
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
  const events = await page.evaluate(() => ({
    opened_event_streams: globalThis.__ibkrSmoke?.openedEvents || 0,
    event_counts: { ...(globalThis.__ibkrSmoke?.eventCounts || {}) },
  }));
  if (events.opened_event_streams < 1) {
    throw new Error(`expected at least one SSE stream, got ${events.opened_event_streams}`);
  }
  return events;
}

async function exerciseAccountPrivacy(page) {
  await page.waitForSelector("#accountPanel:not([hidden])", { timeout: 5000 });
  const value = page.locator("#netLiquidation");
  const before = (await value.textContent())?.trim();
  if (before === "--") {
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
  return { masked_by_default: true, toggle_reveals: true };
}

async function exerciseAccountPanel(page) {
  await page.waitForFunction(() => {
    const panel = document.getElementById("accountPanel");
    return panel && !panel.hidden && document.getElementById("accountLabel")?.textContent?.trim();
  }, { timeout: 5000 });
  const panel = await page.evaluate(() => ({
    accountMenuPresent: !!document.getElementById("accountMenu"),
    accountMenuTogglePresent: !!document.getElementById("accountMenuToggle"),
    accountLabel: document.getElementById("accountLabel")?.textContent?.trim() || "",
    pill: document.getElementById("tradingEnvPill")?.textContent?.trim() || "",
    freshness: document.getElementById("accountAsOf")?.textContent?.trim() || "",
    freshnessQuiet: !!document.getElementById("accountAsOf")?.hidden,
    dailyPnl: document.getElementById("dailyPnl")?.textContent?.trim() || "",
    dailyPnlPct: document.getElementById("dailyPnlPct")?.textContent?.trim() || "",
    riskValues: [
      document.getElementById("accountRiskDelta")?.textContent?.trim() || "",
      document.getElementById("accountRiskTheta")?.textContent?.trim() || "",
      document.getElementById("accountRiskFx")?.textContent?.trim() || "",
      document.getElementById("accountLargestExposureLabel")?.textContent?.trim() || "",
    ],
    accountHasUnderlyingBook: !!document.querySelector("#accountPanel #underlyingBookList"),
  }));
  if (panel.accountMenuPresent || panel.accountMenuTogglePresent) {
    throw new Error(`account dropdown DOM should be removed: ${JSON.stringify(panel)}`);
  }
  // Freshness runs quiet-when-fresh: a hidden empty badge is the healthy
  // state; visible text is required only when the badge is shown (stale or
  // missing timestamp).
  if (!panel.accountLabel || !panel.pill || !panel.dailyPnlPct || panel.riskValues.some((value) => !value)) {
    throw new Error(`account panel is missing values: ${JSON.stringify(panel)}`);
  }
  if (!panel.freshnessQuiet && !panel.freshness) {
    throw new Error(`account freshness badge is visible but empty: ${JSON.stringify(panel)}`);
  }
  if (panel.dailyPnl !== "--" && (panel.dailyPnlPct === "******" || !panel.dailyPnlPct.includes("%"))) {
    throw new Error(`account Daily P/L percent should stay visible in privacy mode: ${JSON.stringify(panel)}`);
  }
  if (/no concrete/i.test(panel.accountLabel)) {
    throw new Error(`account panel should not show no-concrete copy: ${JSON.stringify(panel)}`);
  }
  if (/^all accounts$/i.test(panel.accountLabel)) {
    throw new Error(`account panel should avoid vague All accounts copy: ${JSON.stringify(panel)}`);
  }
  if (panel.accountHasUnderlyingBook) {
    throw new Error("account panel should not contain the underlyings subledger");
  }
  const accountDetailInitiallyHidden = await page.locator("#accountOverviewDetail").evaluate((detail) => detail.hidden);
  if (!accountDetailInitiallyHidden) {
    throw new Error("account overview detail should start folded");
  }
  await page.locator("#accountOverviewToggle").click();
  await page.waitForFunction(() => {
    const toggle = document.getElementById("accountOverviewToggle");
    const detail = document.getElementById("accountOverviewDetail");
    return toggle?.getAttribute("aria-expanded") === "true" && detail && !detail.hidden;
  }, { timeout: 5000 });
  const exposureDisabled = await page.locator("#accountLargestExposureToggle").evaluate((button) => button.disabled);
  if (!exposureDisabled) {
    await page.locator("#accountLargestExposureToggle").click();
    await page.waitForFunction(() => {
      const toggle = document.getElementById("accountLargestExposureToggle");
      const detail = document.getElementById("accountLargestExposurePanel");
      return toggle?.getAttribute("aria-expanded") === "true" && detail && !detail.hidden;
    }, { timeout: 5000 });
  }
  return {
    visible: true,
    account: panel.accountLabel,
    mode: panel.pill,
    account_has_underlying_book: panel.accountHasUnderlyingBook,
    detail_initially_folded: accountDetailInitiallyHidden,
    exposure_detail_disabled: exposureDisabled,
  };
}

async function assertSnapshotBannerCopy(page) {
  const banner = await page.evaluate(() => {
    const el = document.getElementById("snapshotErrorBanner");
    const text = document.getElementById("snapshotErrorText");
    return {
      visible: !!el && !el.hidden,
      text: text?.textContent?.trim() || "",
      title: text?.getAttribute("title") || "",
    };
  });
  if (rawGatewayCopyPattern.test(banner.text)) {
    throw new Error(`snapshot banner leaks raw gateway error text: ${JSON.stringify(banner)}`);
  }
  return banner;
}

async function exerciseMarketLayout(page) {
  await page.waitForFunction(() => {
    const text = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    return /\b(closing|opening) in\b/i.test(text);
  }, { timeout: 10000 });
  const layout = await page.evaluate(() => {
    const regimeHalf = document.getElementById("regimeSummaryCard");
    const canaryPanel = document.getElementById("canaryHero");
    const signalPanel = document.getElementById("signalPanel");
    const underlyingPanel = document.getElementById("underlyingPanel");
    const marketStrip = document.querySelector(".market-strip");
    const accountPanel = document.getElementById("accountPanel");
    const regimeBeforeCanary = !!(regimeHalf && canaryPanel && (regimeHalf.compareDocumentPosition(canaryPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const canaryBeforeUnderlying = !!(signalPanel && underlyingPanel && (signalPanel.compareDocumentPosition(underlyingPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const accountAfterMarketStrip = !!(marketStrip && accountPanel && (marketStrip.compareDocumentPosition(accountPanel) & Node.DOCUMENT_POSITION_FOLLOWING));
    const phase = document.getElementById("sessionPhase")?.textContent?.trim() || "";
    const strip = document.querySelector(".market-strip");
    const stripStyle = strip ? getComputedStyle(strip) : null;
    const quoteStrip = document.getElementById("marketQuoteStrip");
    const quoteStripStyle = quoteStrip ? getComputedStyle(quoteStrip) : null;
    const firstQuote = document.querySelector("#marketQuoteStrip .market-quote-cell");
    const labelStyle = firstQuote?.querySelector("b") ? getComputedStyle(firstQuote.querySelector("b")) : null;
    const marketOpen = strip?.classList.contains("market-open") || false;
    const accountHasUnderlyingBook = !!document.querySelector("#accountPanel #underlyingBookList");
    const canaryHasUnderlyingBook = !!document.querySelector("#canaryHero #underlyingBookList");
    const standaloneHasUnderlyingBook = !!document.querySelector("#underlyingPanel #underlyingBookList");
    const quoteCells = document.querySelectorAll("#marketQuoteStrip .market-quote-cell").length;
    const underlyingOpen = !!underlyingPanel?.open;
    return {
      regimeBeforeCanary,
      canaryBeforeUnderlying,
      accountAfterMarketStrip,
      phase,
      marketOpen,
      accountHasUnderlyingBook,
      canaryHasUnderlyingBook,
      standaloneHasUnderlyingBook,
      quoteCells,
      underlyingOpen,
      marketStyle: {
        stripShadow: stripStyle?.boxShadow || "",
        quoteBorder: quoteStripStyle?.borderTopWidth || "",
        quoteRadius: quoteStripStyle?.borderRadius || "",
        quoteBackground: quoteStripStyle?.backgroundColor || "",
        labelColor: labelStyle?.color || "",
        labelSize: labelStyle?.fontSize || "",
      },
    };
  });
  if (!layout.regimeBeforeCanary) {
    throw new Error("Regime should appear before Portfolio Canary in DOM order");
  }
  if (!layout.canaryBeforeUnderlying) {
    throw new Error("Underlyings should appear after Portfolio Canary in DOM order");
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
    throw new Error("Account panel still contains the underlyings subledger");
  }
  if (layout.canaryHasUnderlyingBook || !layout.standaloneHasUnderlyingBook) {
    throw new Error("Underlyings subledger should be standalone, not inside Portfolio Canary");
  }
  if (layout.quoteCells !== 6) {
    throw new Error(`market strip should render six bounded quote cells, found ${layout.quoteCells}`);
  }
  if (layout.underlyingOpen) {
    throw new Error("Underlyings should be folded by default");
  }
  if (layout.marketStyle.stripShadow !== "none" || layout.marketStyle.quoteBorder !== "0px" || layout.marketStyle.quoteRadius !== "0px") {
    throw new Error(`market strip should be flat and unframed: ${JSON.stringify(layout.marketStyle)}`);
  }
  if (!/^1[1-3]px$/.test(layout.marketStyle.labelSize)) {
    throw new Error(`market symbol labels should use compact subtle sizing: ${JSON.stringify(layout.marketStyle)}`);
  }
  if (/\b(Xetra|US market|US equities|US options)\b/i.test(layout.phase)) {
    throw new Error(`market line should not repeat the selected market label: ${JSON.stringify(layout.phase)}`);
  }
  return layout;
}

async function assertNoViewportOverflow(page) {
  const sizes = [
    { width: 390, height: 844 },
    { width: 547, height: 919 },
    { width: 900, height: 900 },
    { width: 1280, height: 900 },
  ];
  const results = [];
  for (const size of sizes) {
    await page.setViewportSize(size);
    await page.waitForTimeout(120);
    const info = await page.evaluate(() => {
      const clientWidth = document.documentElement.clientWidth;
      const pageScrollWidth = Math.max(document.documentElement.scrollWidth, document.body.scrollWidth);
      const signalPanel = document.getElementById("signalPanel")?.getBoundingClientRect();
      const regime = document.querySelector("#signalSplit .signal-half--regime")?.getBoundingClientRect();
      const canary = document.querySelector("#signalSplit .signal-half--canary")?.getBoundingClientRect();
      const dashboard = document.getElementById("dashboard")?.getBoundingClientRect();
      const signalLayout = regime && canary ? {
        // The split is side by side at every width: regime and canary share
        // a row, roughly splitting signalPanel's width; signalPanel itself
        // spans the full dashboard width rather than pairing with a sibling.
        sameRow: Math.abs(regime.top - canary.top) <= 4,
        regimeFirst: regime.left < canary.left,
        similarWidths: Math.abs(regime.width - canary.width) <= 24,
        signalPanelFullWidth: !!(signalPanel && dashboard) && Math.abs(signalPanel.width - dashboard.width) <= 4,
        regime: { left: Math.round(regime.left), top: Math.round(regime.top), width: Math.round(regime.width), height: Math.round(regime.height) },
        canary: { left: Math.round(canary.left), top: Math.round(canary.top), width: Math.round(canary.width), height: Math.round(canary.height) },
      } : null;
      const offenders = [...document.querySelectorAll("body *")]
        .filter((el) => {
          const style = getComputedStyle(el);
          if (style.display === "none" || style.visibility === "hidden") return false;
          if (style.overflowX === "auto" || style.overflowX === "scroll") return false;
          return el.scrollWidth > el.clientWidth + 1;
        })
        .slice(0, 8)
        .map((el) => ({
          tag: el.tagName.toLowerCase(),
          id: el.id || "",
          className: typeof el.className === "string" ? el.className : "",
          scrollWidth: el.scrollWidth,
          clientWidth: el.clientWidth,
        }));
      return { clientWidth, pageScrollWidth, offenders, signalLayout };
    });
    results.push({ ...size, ...info });
    if (info.pageScrollWidth > info.clientWidth + 1) {
      throw new Error(`page overflows at ${size.width}px: ${JSON.stringify(info)}`);
    }
    if (!info.signalLayout?.sameRow || !info.signalLayout?.regimeFirst || !info.signalLayout?.similarWidths || !info.signalLayout?.signalPanelFullWidth) {
      throw new Error(`Regime and Portfolio halves should align side-by-side inside a full-width combined panel at ${size.width}px: ${JSON.stringify(info.signalLayout)}`);
    }
  }
  return results;
}

async function exerciseCanaryControlsRemoved(page) {
  const counts = await page.evaluate(() => ({
    chipRows: document.querySelectorAll(".canary-chip-row").length,
    chips: document.querySelectorAll(".canary-chip").length,
    warningToggle: document.querySelectorAll("#canaryWarningsToggle").length,
    checksToggle: document.querySelectorAll("#canaryChecksToggle").length,
    inlineDetail: document.querySelectorAll("#canaryInlineDetailPanel").length,
    mitigationButton: document.querySelectorAll("#canaryMitigationButton").length,
    orderReviewPanel: document.querySelectorAll("#orderReviewPanel").length,
    riskPlanQuickAction: document.querySelectorAll("#quickRiskPlanButton").length,
    reviewBlockersButton: document.querySelectorAll("#quickReviewBlockersButton").length,
    heldActionsButton: document.querySelectorAll("#quickHeldActionsButton").length,
    alertsQuickButton: document.querySelectorAll("#quickAlertsButton").length,
  }));
  const total = Object.values(counts).reduce((sum, count) => sum + count, 0);
  if (total > 0) {
    throw new Error(`canary summary controls and risk-plan surfaces should be removed: ${JSON.stringify(counts)}`);
  }
  return counts;
}

async function exerciseUnderlyingPanelFixture(page) {
  await page.evaluate(() => {
    localStorage.setItem("ibkrPurgeBook", JSON.stringify({
      purge_id: "purge_ui_fixture",
      base_currency: "USD",
      legs: [{
        symbol: "SMOKE",
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
    return document.querySelector("#underlyingPanel #underlyingBookList .underlying-row");
  }, { timeout: 5000 });
  const info = await page.evaluate(() => ({
    count: document.getElementById("underlyingBookCount")?.textContent?.trim() || "",
    status: document.getElementById("underlyingBookStatus")?.textContent?.trim() || "",
    winner: document.getElementById("underlyingWinnerPnl")?.textContent?.trim() || "",
    loser: document.getElementById("underlyingLoserPnl")?.textContent?.trim() || "",
    accountHasUnderlyingBook: !!document.querySelector("#accountPanel #underlyingBookList"),
    canaryHasUnderlyingBook: !!document.querySelector("#canaryHero #underlyingBookList"),
    standaloneHasUnderlyingBook: !!document.querySelector("#underlyingPanel #underlyingBookList"),
    foldIcon: Boolean(document.querySelector("#underlyingPanel #underlyingDetailToggle.panel-chevron")),
    bulkButtons: [...document.querySelectorAll("#underlyingPanel .underlying-bulk-actions button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    })),
      rows: [...document.querySelectorAll("#underlyingPanel #underlyingBookList .underlying-row")].map((row) => ({
      symbol: row.dataset.symbol || "",
      virtual: row.classList.contains("underlying-row--virtual"),
      markers: [...row.querySelectorAll(".underlying-marker")].map((marker) => marker.textContent?.trim() || ""),
      quoteStatus: row.querySelector(".underlying-quote-status")?.textContent?.trim() || "",
      quoteStatusTitle: row.querySelector(".underlying-quote-status")?.getAttribute("title") || "",
      buttons: [...row.querySelectorAll("button")].map((button) => ({
        text: button.textContent?.trim() || "",
        disabled: button.disabled,
        title: button.title || "",
      })),
      text: row.textContent?.replace(/\s+/g, " ").trim() || "",
    })),
  }));
  if (info.accountHasUnderlyingBook || info.canaryHasUnderlyingBook || !info.standaloneHasUnderlyingBook) {
    throw new Error(`underlyings subledger is in the wrong panel: ${JSON.stringify(info)}`);
  }
  if (!info.foldIcon) {
    throw new Error(`underlyings folded summary is missing its disclosure toggle: ${JSON.stringify(info)}`);
  }
  if (!info.winner || !info.loser) {
    throw new Error(`underlyings folded summary is missing winner/loser totals: ${JSON.stringify(info)}`);
  }
  const row = info.rows.find((item) => item.symbol === "SMOKE");
  if (!row || !row.virtual) {
    throw new Error(`virtual purge row is missing: ${JSON.stringify(info)}`);
  }
  if (!row.quoteStatus || !row.quoteStatusTitle) {
    throw new Error(`underlying row should include quote price status: ${JSON.stringify(row)}`);
  }
  if (rawGatewayCopyPattern.test(row.quoteStatus)) {
    throw new Error(`underlying row leaks raw gateway error text: ${JSON.stringify(row)}`);
  }
  for (const marker of ["Virtual", "Purged"]) {
    if (!row.markers.includes(marker)) {
      throw new Error(`virtual purge row lacks ${marker} marker: ${JSON.stringify(row)}`);
    }
  }
  const purge = row.buttons.find((button) => button.text === "Purge");
  const restore = row.buttons.find((button) => button.text === "Restore");
  const build = row.buttons.find((button) => button.text === "Build");
  if (!purge?.disabled || !purge.title) {
    throw new Error(`purged row should disable Purge with a reason: ${JSON.stringify(row.buttons)}`);
  }
  if (!restore || !build) {
    throw new Error(`purged row should render Restore and Build actions: ${JSON.stringify(row.buttons)}`);
  }
  if (row.buttons.some((button) => /placeholder|backend wiring/i.test(button.title))) {
    throw new Error(`underlying row still contains placeholder action copy: ${JSON.stringify(row.buttons)}`);
  }
  const bulkLabels = info.bulkButtons.map((item) => item.text);
  const expectedBulkLabels = ["Purge all", "Restore all", "Rebuild all"];
  if (JSON.stringify(bulkLabels) !== JSON.stringify(expectedBulkLabels)) {
    throw new Error(`bulk underlying controls should be ordered Purge all, Restore all, Rebuild all: ${JSON.stringify(info.bulkButtons)}`);
  }
  for (const label of expectedBulkLabels) {
    const button = info.bulkButtons.find((item) => item.text === label);
    if (button.disabled && !button.title) {
      throw new Error(`disabled bulk underlying control lacks a reason: ${JSON.stringify(button)}`);
    }
  }

  await page.evaluate(() => localStorage.removeItem("ibkrPurgeBook"));
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
  // The reload resets __ibkrSmoke and the rendered canary/regime state; later
  // exercises assume the same arrived-snapshot barrier as the initial load.
  await waitForSnapshotEvent(page, 0);

  return {
    virtual_rows: info.rows.length,
    count: info.count,
    markers: row.markers,
    purge_disabled: purge.disabled,
    restore_disabled: restore.disabled,
    build_disabled: build.disabled,
    bulk_buttons: info.bulkButtons,
  };
}

async function exerciseCanaryDetail(page) {
  // Quiet-when-fresh blanks and hides the badge while the snapshot is fresh;
  // that is the healthy state, not a missing timestamp.
  const quietFresh = await page.locator("#canaryAsOf").evaluate((el) => el.hidden && !el.textContent.trim());
  let timestamp = (await page.locator("#canaryAsOf").textContent())?.trim() || "";
  if (!quietFresh && canaryTimestampMissing(timestamp)) {
    try {
      await page.waitForFunction(() => {
        const text = document.getElementById("canaryAsOf")?.textContent?.trim() || "";
        return text && text !== "no timestamp" && text !== "updated --" && text !== "--";
      }, { timeout: 30000 });
      timestamp = (await page.locator("#canaryAsOf").textContent())?.trim() || "";
    } catch {
      // A first canary poll can legitimately outlast this wait (fresh app
      // instance against an off-hours live session); the pending-copy
      // assertion below still pins the rendered no-data contract.
    }
  }
  const timestampMissing = !quietFresh && canaryTimestampMissing(timestamp);
  const initiallyOpen = await page.locator("#canaryDetailPanel").evaluate((el) => !el.hidden);
  if (initiallyOpen) {
    throw new Error("Portfolio Canary detail should be collapsed by default");
  }
  if (timestampMissing) {
    const pending = await page.locator("#canaryHero").textContent();
    if (!/waiting for canary snapshot/i.test(pending || "")) {
      throw new Error(`canary timestamp is missing without pending copy: ${JSON.stringify({ timestamp, pending })}`);
    }
    return { opens: false, initially_open: initiallyOpen, timestamp, no_value: true };
  }
  await page.locator("#canaryDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("canaryDetailPanel");
    return panel && !panel.hidden && document.getElementById("canaryDetailGrid")?.children.length >= 2;
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
  await page.waitForFunction(() => {
    const canary = document.getElementById("canaryDetailPanel");
    return canary?.hidden;
  }, { timeout: 5000 });
  return { opens: true, initially_open: initiallyOpen, timestamp, cards: counts.cards, drivers: counts.drivers, held_stress: counts.held_stress };
}

function canaryTimestampMissing(value) {
  return !value || value === "--" || value === "updated --" || value === "no timestamp";
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
  const expectedSymbols = ["SPY", "QQQ", "IWM", "VIX", "HYG", "TLT"];
  if (before.quotes.length !== expectedSymbols.length) {
    throw new Error(`market strip should render ${expectedSymbols.length} quote cells: ${JSON.stringify(before.quotes)}`);
  }
  for (const symbol of expectedSymbols) {
    const quote = before.quotes.find((item) => item.symbol === symbol);
    if (!quote) {
      throw new Error(`market strip missing ${symbol}: ${JSON.stringify(before.quotes)}`);
    }
    if (quote.price !== "--" && !/^-?[\d.,]+[.,]\d{2}$/.test(quote.price)) {
      throw new Error(`${symbol} price should use two decimal places, got ${JSON.stringify(quote.price)}`);
    }
    if (quote.change && quote.change !== "--" && !/^[+-]?[\d.,]+[.,]\d{2}%$/.test(quote.change)) {
      throw new Error(`${symbol} change should use two decimal places, got ${JSON.stringify(quote.change)}`);
    }
    if (!quote.source) {
      throw new Error(`${symbol} quote cell should include source/as-of or error text`);
    }
    if (rawGatewayCopyPattern.test(quote.source)) {
      throw new Error(`${symbol} quote cell leaks raw gateway error text: ${JSON.stringify(quote.source)}`);
    }
  }
  if (before.marketContextPanelPresent) {
    throw new Error("old Market Context panel should be removed");
  }
  if (!before.regime || before.regime === "--") {
    if (before.weather !== "weather-na") {
      throw new Error(`empty market regime should use weather-na, got ${JSON.stringify(before.weather)}`);
    }
    return {
      no_value: true,
      weather: before.weather,
      quote_cells: before.quotes.length,
      indicators: 0,
    };
  }
  if (!["weather-green", "weather-amber", "weather-red"].includes(before.weather)) {
    throw new Error(`market weather is not color coded, got ${JSON.stringify(before.weather)}`);
  }
  const canaryInitiallyOpen = await page.locator("#canaryDetailPanel").evaluate((el) => !el.hidden);
  const regimeInitiallyOpen = await page.locator("#regimeDetailPanel").evaluate((el) => !el.hidden);
  if (canaryInitiallyOpen || regimeInitiallyOpen) {
    throw new Error(`Regime and Canary details should both be collapsed by default: ${JSON.stringify({ canaryInitiallyOpen, regimeInitiallyOpen })}`);
  }
  // Regime and canary detail now expand independently (no mutual exclusion):
  // opening regime must not touch canary, and opening canary afterward must
  // leave regime open too — both can be visible together in the shared deck.
  await page.locator("#regimeDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("regimeDetailPanel");
    const canary = document.getElementById("canaryDetailPanel");
    return panel && !panel.hidden && canary?.hidden;
  }, { timeout: 5000 });
  const indicators = await page.evaluate(() => document.getElementById("regimeIndicators")?.children.length || 0);
  if (indicators === 0) {
    throw new Error("market regime detail is empty");
  }
  await page.locator("#canaryDetailToggle").click();
  await page.waitForFunction(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const canary = document.getElementById("canaryDetailPanel");
    return regime && !regime.hidden && canary && !canary.hidden;
  }, { timeout: 5000 });
  const bothOpen = await page.evaluate(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const canary = document.getElementById("canaryDetailPanel");
    return !regime?.hidden && !canary?.hidden;
  });
  if (!bothOpen) {
    throw new Error("Regime and Canary detail should be independently expandable — opening canary should not close regime");
  }
  await page.locator("#regimeDetailToggle").click();
  await page.locator("#canaryDetailToggle").click();
  await page.waitForFunction(() => {
    const regime = document.getElementById("regimeDetailPanel");
    const canary = document.getElementById("canaryDetailPanel");
    return regime?.hidden && canary?.hidden;
  }, { timeout: 5000 });
  return {
    regime: before.regime,
    weather: before.weather,
    quote_cells: before.quotes.length,
    canary_initially_open: canaryInitiallyOpen,
    regime_initially_open: regimeInitiallyOpen,
    both_independently_open: bothOpen,
    indicators,
  };
}

async function exercisePortfolioDetail(page) {
  const summary = (await page.locator("#portfolioDetailSummary").textContent())?.trim() || "";
  const hero = await page.evaluate(() => ({
    panelOpen: document.getElementById("portfolioPanel")?.dataset.open || "",
    delta: document.getElementById("portfolioDollarDelta")?.textContent?.trim() || "",
    meaning: document.getElementById("portfolioDeltaMeaning")?.textContent?.trim() || "",
  }));
  if (hero.panelOpen !== "false" || !hero.delta || !hero.meaning) {
    throw new Error(`portfolio folded hero is incomplete: ${JSON.stringify(hero)}`);
  }
  if (/[0-9$€£]|USD|EUR|GBP/.test(hero.delta)) {
    throw new Error(`portfolio folded delta should not expose the private numeric value: ${JSON.stringify(hero)}`);
  }
  await page.locator("#portfolioPanel .portfolio-layout").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("portfolioDetailPanel");
    return document.getElementById("portfolioPanel")?.dataset.open === "true" &&
      panel && !panel.hidden && document.getElementById("portfolioDetailList")?.children.length >= 4;
  }, { timeout: 5000 });
  const detail = await page.evaluate(() => ({
    rows: document.getElementById("portfolioDetailList")?.children.length || 0,
    text: document.getElementById("portfolioDetailList")?.textContent || "",
  }));
  if (!detail.text.includes("option legs") && !detail.text.includes("No option legs")) {
    throw new Error("portfolio detail does not explain Greeks coverage");
  }
  await page.locator("#portfolioDetailToggle").click();
  await page.waitForFunction(() => {
    const panel = document.getElementById("portfolioDetailPanel");
    return document.getElementById("portfolioPanel")?.dataset.open === "false" && panel && panel.hidden;
  }, { timeout: 5000 });
  return { opens: true, summary, rows: detail.rows, delta: hero.delta };
}

async function exerciseProtectionRiskRendering(page) {
  await page.evaluate(() => {
    const positionsCoverage = {
      status: "review",
      counts: { unprotected: 1, orphaned_order: 1 },
      unprotected_notional_base: 123,
      unprotected_notional_base_currency: "USD",
      by_underlying: [{
        underlying: "SMOKE",
        state: "unprotected",
        position_quantity: 10,
        unprotected_quantity: 10,
        unprotected_notional_base: 123,
        unprotected_notional_base_currency: "USD",
      }, {
        underlying: "PART",
        state: "partial",
        position_quantity: 40,
        protected_quantity: 25,
        unprotected_quantity: 15,
        unprotected_notional_base: 456,
        unprotected_notional_base_currency: "USD",
        orders: [{
          symbol: "PART",
          order_type: "TRAIL",
          tif: "GTC",
          stop_price: 31.5,
        }],
      }, {
        underlying: "COVER",
        state: "covered",
        position_quantity: 8,
        protected_quantity: 8,
        orders: [{
          symbol: "COVER",
          order_type: "TRAIL",
          tif: "GTC",
          stop_price: 50,
        }],
      }],
      largest_unprotected: [{
        underlying: "SMOKE",
        state: "unprotected",
        unprotected_notional_base: 123,
        unprotected_notional_base_currency: "USD",
      }],
      orphaned_orders: [{
        symbol: "OLD",
        order_type: "TRAIL",
        remaining: 100,
        reconciliation_state: "position_mismatch",
        last_message: "current position 0 no longer supports close-only protective order remaining 100; broker reconciliation required",
      }],
    };
    const canaryCoverage = {
      status: "review",
      counts: { unprotected: 1 },
      unprotected_notional_base: 999,
      unprotected_notional_base_currency: "USD",
      largest_unprotected: [{
        underlying: "CANARY",
        state: "unprotected",
        unprotected_notional_base: 999,
        unprotected_notional_base_currency: "USD",
      }],
    };
    const apply = globalThis.__ibkrSmoke?.applySnapshotPatch;
    if (!apply) {
      throw new Error("smoke snapshot patch hook is unavailable");
    }
    apply({
      account: { base_currency: "USD" },
      positions: {
        portfolio: { base_currency: "USD" },
        protection_coverage: positionsCoverage,
      },
      canary: {
        portfolio_fit: "low",
        portfolio: { protection_coverage: canaryCoverage },
        protection_coverage: canaryCoverage,
      },
      proposals: {
        as_of: new Date().toISOString(),
        counts: { total: 1, actionable: 1, trailing_stop: 1 },
        proposals: [{
          key: "smoke-trail",
          revision: "smoke",
          bucket: "trailing_stop",
          state: "generated",
          symbol: "SMOKE",
          sec_type: "STK",
          action: "SELL",
          quantity: 10,
          max_quantity: 10,
          position_quantity: 10,
          position_effect: "close",
          order_type: "TRAIL",
          tif: "GTC",
          contract: { symbol: "SMOKE", sec_type: "STK", currency: "USD" },
          trail: { trailing_percent: 10, initial_stop_price: 90 },
          execution_semantics: {
            reference_side: "bid",
            trigger_method_label: "last",
            trigger_effect: "market_order_when_triggered",
            price_guarantee: "stop_price_is_not_execution_price",
          },
          stop_risk: {
            estimated_loss_base: 100,
            base_currency: "USD",
            estimated_loss_pct_nlv: 0.5,
            gap_scenario: {
              gap_pct: 7.5,
              estimated_loss_base: 145,
              estimated_loss_pct_nlv: 0.7,
            },
          },
          stop_ladder: [{
            label: "5%",
            kind: "fixed_5pct",
            percent: 5,
            stop_price: 95,
            estimated_loss_base: 50,
          }, {
            label: "10%",
            kind: "fixed_10pct",
            percent: 10,
            stop_price: 90,
            estimated_loss_base: 100,
          }, {
            label: "policy chosen",
            kind: "policy_chosen",
            percent: 10,
            stop_price: 90,
            estimated_loss_base: 100,
          }, {
            label: "ATR candidate",
            kind: "atr_candidate",
            percent: 12,
            stop_price: 88,
            estimated_loss_base: 120,
          }],
        }],
      },
    }, { protectionOpen: true, portfolioDetailOpen: true, canaryDetailOpen: true });
  });
  await page.waitForFunction(() => {
    const portfolio = document.getElementById("portfolioDetailList")?.textContent?.toLowerCase() || "";
    const canary = document.getElementById("canaryDetailGrid")?.textContent?.toLowerCase() || "";
    return document.querySelector(".protection-row__risk-ticket") &&
      document.querySelector(".protection-row__ladder") &&
      document.querySelector(".protection-coverage-ledger") &&
      portfolio.includes("protection coverage") &&
      canary.includes("protection coverage");
  }, { timeout: 5000 });
  const info = await page.evaluate(() => ({
    noStop: document.getElementById("protectionNoStopExposure")?.textContent?.trim() || "",
    riskTicket: document.querySelector(".protection-row__risk-ticket")?.textContent?.replace(/\s+/g, " ").trim() || "",
    ladder: document.querySelector(".protection-row__ladder")?.textContent?.replace(/\s+/g, " ").trim() || "",
    coverageLedger: document.querySelector(".protection-coverage-ledger")?.textContent?.replace(/\s+/g, " ").trim() || "",
    portfolioDetail: document.getElementById("portfolioDetailList")?.textContent?.replace(/\s+/g, " ").trim() || "",
    canaryDetail: document.getElementById("canaryDetailGrid")?.textContent?.replace(/\s+/g, " ").trim() || "",
  }));
  if (!info.noStop.includes("123") || info.noStop.includes("999")) {
    throw new Error(`Protection no-stop exposure should use positions.protection_coverage, not Canary context: ${JSON.stringify(info)}`);
  }
  for (const text of ["trigger bid / last", "est. loss", "7.5% gap", "trigger becomes market"]) {
    if (!info.riskTicket.includes(text)) {
      throw new Error(`Protection risk ticket missing ${JSON.stringify(text)}: ${JSON.stringify(info.riskTicket)}`);
    }
  }
  for (const text of ["Stop ladder", "5%", "10%", "Policy", "ATR"]) {
    if (!info.ladder.includes(text)) {
      throw new Error(`Protection ladder missing ${JSON.stringify(text)}: ${JSON.stringify(info.ladder)}`);
    }
  }
  const coverageLedgerLower = info.coverageLedger.toLowerCase();
  for (const text of ["smoke", "unprotected", "part", "partial", "cover", "covered", "old", "reconcile required"]) {
    if (!coverageLedgerLower.includes(text)) {
      throw new Error(`Protection coverage ledger missing ${JSON.stringify(text)}: ${JSON.stringify(info.coverageLedger)}`);
    }
  }
  const portfolioDetailLower = info.portfolioDetail.toLowerCase();
  for (const text of ["protection coverage", "largest unprotected", "stale protective orders"]) {
    if (!portfolioDetailLower.includes(text)) {
      throw new Error(`Portfolio protection coverage detail missing ${JSON.stringify(text)}: ${JSON.stringify(info.portfolioDetail)}`);
    }
  }
  if (!info.canaryDetail.toLowerCase().includes("protection coverage")) {
    throw new Error(`Canary detail does not include protection coverage context: ${JSON.stringify(info.canaryDetail)}`);
  }
  return info;
}

async function exerciseAlertHistory(page) {
  const initiallyOpen = await page.locator("#alertsPanel").evaluate((el) => !!el.open);
  if (!initiallyOpen) {
    await page.locator("#alertsPanel summary").click();
    await page.waitForFunction(() => document.getElementById("alertsPanel")?.open, { timeout: 5000 });
  }
  const info = await page.evaluate(() => ({
    count: Number.parseInt(document.getElementById("alertCount")?.textContent || "0", 10) || 0,
    currentRows: document.querySelectorAll("#currentSignalList .alert-row").length,
    historyRows: document.querySelectorAll("#alertHistoryList .alert-row").length,
    previousRows: document.querySelectorAll("#previousContextList .alert-row").length,
    currentCount: Number.parseInt(document.getElementById("currentSignalCount")?.textContent || "0", 10) || 0,
    historyCount: Number.parseInt(document.getElementById("alertHistoryCount")?.textContent || "0", 10) || 0,
    previousCount: Number.parseInt(document.getElementById("previousContextCount")?.textContent || "0", 10) || 0,
    clearDisabled: document.getElementById("clearAlertsButton")?.disabled || false,
    hint: document.getElementById("alertsHint")?.textContent || "",
  }));
  let selected = false;
  const firstAlert = page.locator("#currentSignalList .alert-row:visible, #alertHistoryList .alert-row:visible, #previousContextList .alert-row:visible").first();
  if ((await firstAlert.count()) > 0) {
    await firstAlert.click();
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
  return {
    initially_open: initiallyOpen,
    opens: true,
    count: info.count,
    current_rows: info.currentRows,
    history_rows: info.historyRows,
    previous_rows: info.previousRows,
    current_count: info.currentCount,
    history_count: info.historyCount,
    previous_count: info.previousCount,
    selected,
  };
}

// Orders now lives on its own bottom-nav tab (Monitor, Alerts, Orders,
// Settings) rather than an inline <details> panel — visibility is
// tab-driven, not a per-panel open/closed toggle, and the panel itself is
// always present once the tab is active (emptiness is signaled by the
// ordersOpenCount badge and an .empty-row message, not by hiding the panel).
async function exerciseOpenOrders(page) {
  await page.locator("#tabOrders").click();
  await page.waitForFunction(() => document.getElementById("ordersTab")?.hidden === false, { timeout: 5000 });
  const info = await page.evaluate(() => {
    const buttons = [...document.querySelectorAll("#ordersOpenList button")].map((button) => ({
      text: button.textContent?.trim() || "",
      disabled: button.disabled,
      title: button.title || "",
    }));
    return {
      panelPresent: !!document.getElementById("ordersPanel"),
      countText: document.getElementById("ordersOpenCount")?.textContent?.trim() || "",
      rows: document.querySelectorAll("#ordersOpenList .open-order-row").length,
      empty: document.getElementById("ordersOpenList")?.textContent?.includes("No open orders available for this view.") || false,
      buttons,
      oldLabels: buttons.map((button) => button.text).filter((label) => ["Modify", "Cancel", "Execute"].includes(label)),
    };
  });
  if (!info.panelPresent) {
    throw new Error("Orders panel should always be present once the Orders tab is active");
  }
  if (info.oldLabels.length > 0) {
    throw new Error(`open-order controls still use old labels: ${info.oldLabels.join(", ")}`);
  }
  if (info.rows === 0 && !info.empty) {
    throw new Error("open-order empty state is missing");
  }
  const expectedCount = info.rows === 1 ? "1 open" : `${info.rows} open`;
  if (info.countText !== expectedCount) {
    throw new Error(`orders open-count badge should read ${JSON.stringify(expectedCount)}, got ${JSON.stringify(info.countText)}`);
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
  await page.locator("#tabMonitor").click();
  await page.waitForFunction(() => document.getElementById("dashboard")?.hidden === false, { timeout: 5000 });
  return {
    rows: info.rows,
    empty: info.empty,
    count_text: info.countText,
    buttons: info.buttons.map((button) => ({ text: button.text, disabled: button.disabled, has_reason: !!button.title })),
  };
}

async function readMarketContext(page) {
  return page.evaluate(() => ({
    marketContextPanelPresent: !!document.getElementById("marketPanel"),
    quotes: [...document.querySelectorAll("#marketQuoteStrip .market-quote-cell")].map((cell) => ({
      symbol: cell.querySelector("b")?.textContent?.trim() || "",
      price: cell.querySelector("strong")?.textContent?.trim() || "",
      change: cell.querySelector(".market-change")?.textContent?.trim() || "",
      source: cell.querySelector("small")?.textContent?.trim() || "",
    })),
    regime: document.getElementById("marketRegime")?.textContent?.trim() || "",
    weather: [...(document.getElementById("regimeSummaryCard")?.classList || [])].find((name) => name.startsWith("weather-")) || "",
  }));
}

async function assertDebugToolsRemoved(page, baseURL) {
  const domInfo = await page.evaluate(() => ({
    panel_present: !!document.getElementById("toolsPanel"),
    tool_buttons: document.querySelectorAll("[data-tool]").length,
  }));
  const cookies = await page.context().cookies(baseURL);
  const cookieHeader = cookies.map((cookie) => `${cookie.name}=${cookie.value}`).join("; ");
  const endpoint = new URL("/api/tools/events", baseURL);
  const res = await fetch(endpoint, {
    method: "POST",
    headers: cookieHeader ? { Cookie: cookieHeader } : {},
  });
  const info = {
    ...domInfo,
    endpoint_status: res.status,
  };
  if (info.panel_present || info.tool_buttons > 0) {
    throw new Error(`debug tools still render: ${JSON.stringify(info)}`);
  }
  if (info.endpoint_status < 400) {
    throw new Error(`debug tools endpoint still responds successfully: ${JSON.stringify(info)}`);
  }
  return info;
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

function trimRight(value, suffix) {
  while (value.endsWith(suffix)) {
    value = value.slice(0, -suffix.length);
  }
  return value;
}
