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
    for (const type of ["snapshot", "status", "account", "positions", "canary", "heartbeat"]) {
      es.addEventListener(type, () => {
        globalThis.__ibkrSmoke.eventCounts[type] = (globalThis.__ibkrSmoke.eventCounts[type] || 0) + 1;
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
  const connection = await waitForConnection(page, "Live");
  const pushState = await page.locator("#pushState").textContent();
  const eventsBefore = await fetchEventsDiagnostics(page);
  const privacy = await exerciseAccountPrivacy(page);
  const canaryDetail = await exerciseCanaryDetail(page);
  const marketContext = await exerciseMarketContext(page);
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
    canary_detail: canaryDetail,
    market_context: marketContext,
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
  const connectionBeforeRestart = await waitForConnection(page, "Live");
  const restart = await runShellJSON(restartCommand);
  const snapshotAfter = await waitForSnapshotEvent(page, before.snapshot);
  const connectionAfterRestart = await waitForConnection(page, "Live");
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

async function waitForConnection(page, expected) {
  await page.waitForFunction((text) => {
    return document.getElementById("connectionLine")?.textContent === text;
  }, expected, { timeout: 20000 });
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

async function exerciseCanaryDetail(page) {
  const timestamp = (await page.locator("#canaryAsOf").textContent())?.trim() || "";
  if (!timestamp || timestamp === "--" || timestamp === "updated --") {
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
  }));
  await page.locator("#canaryDetailToggle").click();
  return { opens: true, timestamp, cards: counts.cards, drivers: counts.drivers };
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
  if (before.spyLevel !== "--" && before.spyChange === "--") {
    throw new Error("SPY has a level but no percent change");
  }
  if (before.vixLevel !== "--" && before.vixChange === "--") {
    throw new Error("VIX has a level but no percent change");
  }
  if (!before.regime || before.regime === "--") {
    if (before.weather !== "weather-na") {
      throw new Error(`empty market regime should use weather-na, got ${JSON.stringify(before.weather)}`);
    }
    return {
      no_value: true,
      weather: before.weather,
      spy_level_present: before.spyLevel !== "--",
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
    vix_level_present: before.vixLevel !== "--",
    indicators,
  };
}

async function readMarketContext(page) {
  return page.evaluate(() => ({
    spyLevel: document.getElementById("spyLevel")?.textContent?.trim() || "",
    spyChange: document.getElementById("spyChange")?.textContent?.trim() || "",
    vixLevel: document.getElementById("vixLevel")?.textContent?.trim() || "",
    vixChange: document.getElementById("vixChange")?.textContent?.trim() || "",
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
