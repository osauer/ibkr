#!/usr/bin/env node
// app-screenshots.mjs — regenerate the published Canary app screenshots
// (docs/social/canary-app-{mobile,desktop}.png) from the real PWA with
// synthetic account figures, so no real account data reaches the pixels.
//
// The page is served by a live `ibkr app` and paired exactly like
// app-browser-smoke.mjs (helpers shared via lib-app-browser.mjs); an init
// script then wraps fetch and EventSource and rewrites every
// account-bearing payload (bootstrap JSON, SSE account/snapshot/positions
// events) before the SPA sees it: account ids become the documentation
// placeholder DU1234567 and the summary figures become fixture values.
// Market strip data (SPY/VIX/QQQ) stays real — it is public.
//
// Usage: node scripts/app-screenshots.mjs [--base-url http://127.0.0.1:8765]
//        [--out-dir docs/social] [--browser chromium]

import path from "node:path";
import { createPairingSession, launchBrowser, loadPlaywright, parseArgs } from "./lib-app-browser.mjs";

const args = parseArgs(process.argv.slice(2));
const baseURL = (args["base-url"] || "http://127.0.0.1:8765").replace(/\/+$/, "");
const outDir = args["out-dir"] || "docs/social";
const browserName = args.browser || "chromium";

// Documentation placeholder id (allowlisted by check-no-account-data) and
// deliberately tidy fixture figures — recognizable as demo data.
const FIXTURE = {
  accountID: "DU1234567",
  netLiquidation: 1_250_000,
  buyingPower: 4_800_000,
  dailyPnl: 2_340,
};

const SHOTS = [
  {
    name: "canary-app-mobile.png",
    viewport: { width: 390, height: 844 },
    isMobile: true,
  },
  {
    name: "canary-app-desktop.png",
    viewport: { width: 1180, height: 760 },
    isMobile: false,
  },
];

const playwright = loadPlaywright("app-screenshots");
if (!playwright[browserName]) {
  console.error(`app-screenshots: unknown browser ${browserName}`);
  process.exit(2);
}

const { browser } = await launchBrowser(playwright[browserName], browserName, { headless: true });
try {
  for (const shot of SHOTS) {
    // Pairing nonces are single-use; mint one per context.
    const pairing = await createPairingSession(baseURL, baseURL);
    const context = await browser.newContext({
      viewport: shot.viewport,
      deviceScaleFactor: 2,
      isMobile: shot.isMobile,
      hasTouch: shot.isMobile,
    });
    await context.addInitScript(installFixtureRewrite, FIXTURE);
    const page = await context.newPage();
    await page.goto(pairing.url, { waitUntil: "domcontentloaded", timeout: 15000 });
    await page.waitForSelector("#dashboard:not([hidden])", { timeout: 15000 });
    // Let the first snapshot/account events render, then assert the fixture
    // actually took: the real id must be nowhere, the placeholder visible.
    await page.waitForFunction(
      (id) => document.body.innerText.includes(id),
      FIXTURE.accountID,
      { timeout: 15000 },
    );
    // Balances are privacy-masked (******) by default; the published shots
    // show the (fixture) figures, so reveal them before capturing.
    await page.locator("#accountPrivacyToggle").click();
    await page.waitForFunction(() => {
      const text = document.getElementById("netLiquidation")?.textContent?.trim();
      return Boolean(text) && text !== "******" && text !== "--";
    }, { timeout: 5000 });
    const leaked = await page.evaluate(
      (id) => /\bD?U\d{6,9}\b/.test(document.body.innerText.replaceAll(id, "")),
      FIXTURE.accountID,
    );
    if (leaked) {
      throw new Error("an account-id-shaped string other than the fixture is visible; aborting");
    }
    const out = path.join(outDir, shot.name);
    await page.screenshot({ path: out });
    console.log(`app-screenshots: wrote ${out} (${shot.viewport.width}x${shot.viewport.height}@2x)`);
    await context.close();
  }
} finally {
  await browser.close();
}

// Runs inside the page before any app code. Rewrites account-bearing
// payloads from both transports. Generic by construction: every
// `account_id` string anywhere in a payload becomes the placeholder, and
// every object that carries `net_liquidation` gets the fixture summary,
// so SSE event shapes and bootstrap nesting can drift without leaking.
function installFixtureRewrite(fixture) {
  // The SPA reads the account id and mode from several alternates
  // (account.account_id, trading.account, status.connected_account,
  // status.account_mode, …), so the walker matches by value shape and
  // key family rather than enumerating paths.
  const ID_SHAPE = /^D?U\d{6,9}$/;
  const patch = (node) => {
    if (Array.isArray(node)) {
      node.forEach(patch);
      return node;
    }
    if (!node || typeof node !== "object") {
      return node;
    }
    for (const [key, value] of Object.entries(node)) {
      if (typeof value === "string") {
        if (ID_SHAPE.test(value.trim())) {
          node[key] = fixture.accountID;
        } else if (
          (key === "account_mode" || key === "environment" || key === "mode") &&
          /^live$/i.test(value.trim())
        ) {
          node[key] = "paper";
        }
        continue;
      }
      patch(value);
    }
    if (typeof node.net_liquidation === "number") {
      node.net_liquidation = fixture.netLiquidation;
      node.buying_power = fixture.buyingPower;
      node.daily_pnl = fixture.dailyPnl;
      if ("net_liquidation_start_of_day" in node) {
        node.net_liquidation_start_of_day = fixture.netLiquidation - fixture.dailyPnl;
      }
      if ("previous_net_liquidation" in node) {
        node.previous_net_liquidation = fixture.netLiquidation - fixture.dailyPnl;
      }
    }
    return node;
  };

  const nativeFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = async (...fetchArgs) => {
    const res = await nativeFetch(...fetchArgs);
    const url = typeof fetchArgs[0] === "string" ? fetchArgs[0] : fetchArgs[0]?.url || "";
    if (!res.ok || !/\/api\/(bootstrap|account|positions)\b/.test(url)) {
      return res;
    }
    const body = patch(await res.clone().json());
    return new Response(JSON.stringify(body), {
      status: res.status,
      statusText: res.statusText,
      headers: res.headers,
    });
  };

  const NativeEventSource = globalThis.EventSource;
  globalThis.EventSource = function fixtureEventSource(url, options) {
    const es = new NativeEventSource(url, options);
    const nativeAdd = es.addEventListener.bind(es);
    es.addEventListener = (type, listener, ...rest) => {
      nativeAdd(
        type,
        (event) => {
          let patched = event;
          try {
            const data = JSON.stringify(patch(JSON.parse(event.data)));
            patched = new MessageEvent(event.type, { data, lastEventId: event.lastEventId });
          } catch {
            // Non-JSON events (heartbeats) pass through untouched.
          }
          if (typeof listener === "function") {
            listener.call(es, patched);
          } else {
            listener.handleEvent(patched);
          }
        },
        ...rest,
      );
    };
    return es;
  };
  globalThis.EventSource.prototype = NativeEventSource.prototype;
}
