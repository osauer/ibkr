#!/usr/bin/env node

import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

const args = parseArgs(process.argv.slice(2));
const baseURL = trimRight(args["base-url"] || "http://127.0.0.1:8765", "/");
const pairPublicURL = trimRight(args["pair-public-url"] || baseURL, "/");
const browserName = args.browser || "chromium";
const channel = args.channel || process.env.PLAYWRIGHT_CHANNEL || "";
const noNotification = args["no-notification"] !== "false";
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
  const title = await page.title();
  const connection = await page.locator("#connectionLine").textContent();
  const pushState = await page.locator("#pushState").textContent();
  await page.getByRole("button", { name: "Snapshot" }).click();
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
  console.log(JSON.stringify({
    ok: true,
    browser: browserName,
    channel: launched.channel || null,
    base_url: baseURL,
    mobile,
    notification_removed: noNotification,
    title,
    connection,
    push_state: pushState,
    pair_expires_at: pairing.expires_at,
  }, null, 2));
} finally {
  await browser.close();
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
