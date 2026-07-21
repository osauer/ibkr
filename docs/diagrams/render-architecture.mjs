#!/usr/bin/env node

// Deterministic, dependency-free renderer for the public architecture diagrams.
// Visual tokens mirror docs/shared.css so the diagrams read as part of the site.
// Generic component symbols are derived from Tabler Icons 3.45.0 (MIT).

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const canary = fs.readFileSync(path.join(here, "../social/canary-icon.png")).toString("base64");

const icons = {
  user: `<path d="M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0"/><path d="M6 21v-2a4 4 0 0 1 4 -4h4a4 4 0 0 1 4 4v2"/>`,
  cpu: `<path d="M5 6a1 1 0 0 1 1 -1h12a1 1 0 0 1 1 1v12a1 1 0 0 1 -1 1h-12a1 1 0 0 1 -1 -1l0 -12"/><path d="M8 10v-2h2m6 6v2h-2m-4 0h-2v-2m8 -4v-2h-2"/><path d="M3 10h2M3 14h2M10 3v2M14 3v2M21 10h-2M21 14h-2M14 21v-2M10 21v-2"/>`,
  terminal: `<path d="M8 9l3 3l-3 3M13 15h3"/><path d="M3 6a2 2 0 0 1 2 -2h14a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2h-14a2 2 0 0 1 -2 -2z"/>`,
  plugConnected: `<path d="M7 12l5 5l-1.5 1.5a3.536 3.536 0 1 1 -5 -5l1.5 -1.5M17 12l-5 -5l1.5 -1.5a3.536 3.536 0 1 1 5 5l-1.5 1.5M3 21l2.5 -2.5M18.5 5.5l2.5 -2.5M10 11l-2 2M13 14l-2 2"/>`,
  mobileCode: `<path d="M11.5 21h-3.5a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h8a2 2 0 0 1 2 2v8M20 21l2 -2l-2 -2M17 17l-2 2l2 2M11 4h2M12 17v.01"/>`,
  exchange: `<path d="M7 10h14l-4 -4M17 14h-14l4 4"/>`,
  serverCog: `<path d="M3 7a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3v-2M12 20h-6a3 3 0 0 1 -3 -3v-2a3 3 0 0 1 3 -3h10.5"/><path d="M16 18a2 2 0 1 0 4 0a2 2 0 1 0 -4 0M18 14.5v1.5M18 20v1.5M21.032 16.25l-1.299 .75M16.27 19l-1.3 .75M14.97 16.25l1.3 .75M19.733 19l1.3 .75M7 8v.01M7 16v.01"/>`,
  shieldCheck: `<path d="M11.46 20.846a12 12 0 0 1 -7.96 -14.846a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 -.09 7.06M15 19l2 2l4 -4"/>`,
  plug: `<path d="M9.785 6l8.215 8.215l-2.054 2.054a5.81 5.81 0 1 1 -8.215 -8.215zM4 20l3.5 -3.5M15 4l-3.5 3.5M20 9l-3.5 3.5"/>`,
  databaseImport: `<path d="M4 6c0 1.657 3.582 3 8 3s8 -1.343 8 -3s-3.582 -3 -8 -3s-8 1.343 -8 3M4 6v6c0 1.657 3.582 3 8 3c.856 0 1.68 -.05 2.454 -.144M20 12v-6M4 12v6c0 1.657 3.582 3 8 3c.171 0 .341 -.002 .51 -.006M19 22v-6M22 19l-3 -3l-3 3"/>`,
  database: `<path d="M4 6a8 3 0 1 0 16 0a8 3 0 1 0 -16 0M4 6v6a8 3 0 0 0 16 0v-6M4 12v6a8 3 0 0 0 16 0v-6"/>`,
  server: `<path d="M3 7a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3M3 15a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v2a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3zM7 8v.01M7 16v.01"/>`,
  worldDownload: `<path d="M21 12a9 9 0 1 0 -9 9M3.6 9h16.8M3.6 15h8.4M11.578 3a17 17 0 0 0 0 18M12.5 3c1.719 2.755 2.5 5.876 2.5 9M18 14v7m-3 -3l3 3l3 -3"/>`,
  cloud: `<path d="M6.657 18c-2.572 0 -4.657 -2.007 -4.657 -4.483c0 -2.475 2.085 -4.482 4.657 -4.482c.393 -1.762 1.794 -3.2 3.675 -3.773c1.88 -.572 3.956 -.193 5.444 1c1.488 1.19 2.162 3.007 1.77 4.769h.99c1.913 0 3.464 1.56 3.464 3.486c0 1.927 -1.551 3.487 -3.465 3.487h-11.878"/>`,
  bell: `<path d="M10 5a2 2 0 1 1 4 0a7 7 0 0 1 4 6v3a4 4 0 0 0 2 3h-16a4 4 0 0 0 2 -3v-3a7 7 0 0 1 4 -6M9 17v1a3 3 0 0 0 6 0v-1"/>`,
  calendarCode: `<path d="M11.5 21h-5.5a2 2 0 0 1 -2 -2v-12a2 2 0 0 1 2 -2h12a2 2 0 0 1 2 2v6M16 3v4M8 3v4M4 11h16M20 21l2 -2l-2 -2M17 17l-2 2l2 2"/>`,
  settings: `<path d="M10.325 4.317c.426 -1.756 2.924 -1.756 3.35 0a1.724 1.724 0 0 0 2.573 1.066c1.543 -.94 3.31 .826 2.37 2.37a1.724 1.724 0 0 0 1.065 2.572c1.756 .426 1.756 2.924 0 3.35a1.724 1.724 0 0 0 -1.066 2.573c.94 1.543 -.826 3.31 -2.37 2.37a1.724 1.724 0 0 0 -2.572 1.065c-.426 1.756 -2.924 1.756 -3.35 0a1.724 1.724 0 0 0 -2.573 -1.066c-1.543 .94 -3.31 -.826 -2.37 -2.37a1.724 1.724 0 0 0 -1.065 -2.572c-1.756 -.426 -1.756 -2.924 0 -3.35a1.724 1.724 0 0 0 1.066 -2.573c-.94 -1.543 .826 -3.31 2.37 -2.37c1 .608 2.296 .07 2.572 -1.065M9 12a3 3 0 1 0 6 0a3 3 0 0 0 -6 0"/>`,
  fileText: `<path d="M14 3v4a1 1 0 0 0 1 1h4M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2M9 9h1M9 13h6M9 17h6"/>`,
  refresh: `<path d="M20 11a8.1 8.1 0 0 0 -15.5 -2M4 5v4h4M4 13a8.1 8.1 0 0 0 15.5 2M20 19v-4h-4"/>`,
  browser: `<path d="M4 8h16M4 6a2 2 0 0 1 2 -2h12a2 2 0 0 1 2 2v12a2 2 0 0 1 -2 2h-12a2 2 0 0 1 -2 -2zM8 4v4"/>`,
  lock: `<path d="M5 13a2 2 0 0 1 2 -2h10a2 2 0 0 1 2 2v6a2 2 0 0 1 -2 2h-10a2 2 0 0 1 -2 -2zM11 16a1 1 0 1 0 2 0a1 1 0 0 0 -2 0M8 11v-4a4 4 0 1 1 8 0v4"/>`,
  route: `<path d="M3 19a2 2 0 1 0 4 0a2 2 0 0 0 -4 0M19 7a2 2 0 1 0 0 -4a2 2 0 0 0 0 4M11 19h5.5a3.5 3.5 0 0 0 0 -7h-8a3.5 3.5 0 0 1 0 -7h4.5"/>`,
};

// Tokens mirror docs/shared.css. Flow hues (green/blue/amber) are validated
// for CVD separation and contrast on the paper surface; slate is the
// deliberately recessive neutral and carries dash/weight redundancy.
const C = {
  paper: "#f7f5ef",
  panel: "#fffdf7",
  panelAlt: "#ece7db",
  ink: "#101827",
  muted: "#42526a",
  line: "#d8d2c4",
  terminal: "#0e1626",
  terminal2: "#1b2a45",
  terminalLine: "#26364c",
  textOnDark: "#c9d4e6",
  textOnDarkDim: "#8fa3c2",
  slate: "#42526a",
  green: "#0a8a72",
  greenDark: "#055f52",
  greenSoft: "#e3f0ec",
  greenLine: "#a8cdc2",
  blue: "#2f5fa5",
  blueSoft: "#e8eef7",
  amber: "#b45309",
  amberSoft: "#f9efdd",
  yellow: "#f5c542",
};

const esc = (value) => String(value)
  .replaceAll("&", "&amp;")
  .replaceAll("<", "&lt;")
  .replaceAll(">", "&gt;")
  .replaceAll('"', "&quot;");

function defs(extraStyles = "") {
  const symbols = Object.entries(icons).map(([name, body]) =>
    `<symbol id="icon-${name}" viewBox="0 0 24 24">${body}</symbol>`).join("");
  const marker = (name, color) => `<marker id="arrow-${name}" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="6.5" markerHeight="6.5" orient="auto-start-reverse"><path d="M0 0L10 5L0 10z" fill="${color}"/></marker>`;
  return `<defs>${symbols}${marker("slate", C.slate)}${marker("green", C.green)}${marker("blue", C.blue)}${marker("amber", C.amber)}
    <style>
      text { font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; fill: ${C.ink}; }
      .kicker { font-size: 13px; font-weight: 700; fill: ${C.muted}; letter-spacing: .4px; }
      .title { font-size: 28px; font-weight: 700; letter-spacing: -.4px; }
      .subtitle { font-size: 14px; fill: ${C.muted}; }
      .layer { font-size: 12px; font-weight: 700; letter-spacing: 1.1px; fill: ${C.muted}; }
      .boundary { font-size: 12px; font-weight: 700; letter-spacing: 1.1px; fill: ${C.slate}; }
      .node-title { font-size: 15px; font-weight: 650; }
      .node-sub { font-size: 12.5px; fill: ${C.muted}; }
      .on-dark { fill: ${C.textOnDark}; }
      .on-dark-dim { fill: ${C.textOnDarkDim}; }
      .module-title { font-size: 13.5px; font-weight: 650; fill: #ffffff; }
      .module-sub { font-size: 12px; fill: ${C.textOnDark}; }
      .mono { font: 11.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .mono-small { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .flow-label { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.amber}; }
      .legend { font-size: 11.5px; fill: ${C.muted}; }
      .strip-node { font-size: 12px; font-weight: 650; fill: ${C.ink}; }
      .matrix-head { font-size: 13px; font-weight: 650; fill: ${C.ink}; }
      .matrix-sub { font-size: 11.5px; fill: ${C.muted}; }
      .matrix-owner { font-size: 15px; font-weight: 650; fill: ${C.ink}; }
      .tile-title { font-size: 13.5px; font-weight: 650; fill: ${C.ink}; }
      .tile-sub { font-size: 11.8px; fill: ${C.muted}; }
      .format { font: 10.8px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .footnote { font-size: 10.5px; fill: ${C.muted}; opacity: .85; }
${extraStyles}    </style>
  </defs>`;
}

function icon(name, x, y, size, color = "#ffffff", strokeWidth = 1.8) {
  return `<svg x="${x}" y="${y}" width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="${strokeWidth}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><use href="#icon-${name}"/></svg>`;
}

function iconTile(name, x, y, color, size = 44, iconColor = "#ffffff") {
  const pad = Math.round(size * 0.23);
  return `<rect x="${x}" y="${y}" width="${size}" height="${size}" rx="${Math.round(size / 4)}" fill="${color}"/>${icon(name, x + pad, y + pad, size - 2 * pad, iconColor)}`;
}

function textLines(x, y, lines, className, gap = 16) {
  if (!lines.length) return "";
  return `<text x="${x}" y="${y}" class="${className}">${lines.map((line, index) => `<tspan x="${x}" dy="${index ? gap : 0}">${esc(line)}</tspan>`).join("")}</text>`;
}

// Borderless component: icon tile, title, plain sub lines, optional mono line,
// and a hairline base to seat the group on.
function component({ x, y, iconName, color, iconColor = "#ffffff", title, subtitle = [], mono = "", width = 190 }) {
  const labelX = x + 56;
  const monoY = y + 36 + subtitle.length * 15;
  return `<g role="group" aria-label="${esc([title, ...subtitle, mono].filter(Boolean).join(". "))}">
    ${iconTile(iconName, x, y, color, 44, iconColor)}
    <text x="${labelX}" y="${y + 17}" class="node-title">${esc(title)}</text>
    ${textLines(labelX, y + 36, subtitle, "node-sub", 15)}
    ${mono ? `<text x="${labelX}" y="${monoY}" class="mono-small">${esc(mono)}</text>` : ""}
    <rect x="${x}" y="${y + 68}" width="${width}" height="1" fill="${C.line}"/>
  </g>`;
}

function line(d, colorName = "slate", { dashed = false, dotted = false, both = false, width = 1.8 } = {}) {
  const color = C[colorName];
  const dash = dotted ? ' stroke-dasharray="2 5"' : dashed ? ' stroke-dasharray="7 6"' : "";
  return `<path d="${d}" fill="none" stroke="${color}" stroke-width="${width}" stroke-linecap="round" stroke-linejoin="round"${dash} marker-end="url(#arrow-${colorName})"${both ? ` marker-start="url(#arrow-${colorName})"` : ""}/>`;
}

// Width of a mono chip label at 11.5px (~6.91px/char) plus pill padding.
const chipW = (label) => Math.round(label.length * 6.91 + 22);

function chip(x, y, width, label, fill = C.panelAlt, textClass = "mono") {
  return `<g><rect x="${x}" y="${y}" width="${width}" height="24" rx="12" fill="${fill}"/><text x="${x + width / 2}" y="${y + 16}" text-anchor="middle" class="${textClass}">${esc(label)}</text></g>`;
}

// Mono chip centered on cx with measured width.
function chipAt(cx, y, label, fill = C.panelAlt) {
  const width = chipW(label);
  return chip(Math.round(cx - width / 2), y, width, label, fill);
}

function legendItem(x, y, colorName, label, { dashed = false, dotted = false } = {}) {
  const dash = dotted ? ' stroke-dasharray="2 5"' : dashed ? ' stroke-dasharray="6 5"' : "";
  return `<line x1="${x}" y1="${y}" x2="${x + 28}" y2="${y}" stroke="${C[colorName]}" stroke-width="3" stroke-linecap="round"${dash}/><text x="${x + 36}" y="${y + 4}" class="legend">${esc(label)}</text>`;
}

function header(title, subtitle) {
  return `
  <image href="data:image/png;base64,${canary}" x="36" y="26" width="46" height="46" preserveAspectRatio="xMidYMid slice"/>
  <text x="96" y="42" class="kicker">ibkr canary</text>
  <text x="96" y="70" class="title">${esc(title)}</text>
  <text x="96" y="92" class="subtitle">${esc(subtitle)}</text>`;
}

function junction(x, y, color = C.slate) {
  return `<circle cx="${x}" cy="${y}" r="3" fill="${color}"/>`;
}

function svgFrame({ width, height, title, description, body, extraStyles = "" }) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-labelledby="diagram-title diagram-desc">
  <title id="diagram-title">${esc(title)}</title>
  <desc id="diagram-desc">${esc(description)}</desc>
  <metadata>Generic component icons derived from Tabler Icons 3.45.0, MIT License. Canary mark copyright the ibkr project.</metadata>
  ${defs(extraStyles)}
  <rect width="${width}" height="${height}" fill="${C.paper}"/>
  ${body}
</svg>\n`;
}

const referenceDiagramStyles = `      .card-title { font-size: 14px; font-weight: 700; fill: ${C.ink}; }
      .card-sub { font-size: 11.5px; fill: ${C.muted}; }
      .er-title { font-size: 13px; font-weight: 700; fill: ${C.ink}; }
      .er-key { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-weight: 700; fill: ${C.greenDark}; }
      .er-field { font: 10.5px ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; fill: ${C.muted}; }
      .step-no { font-size: 12px; font-weight: 800; fill: #ffffff; }
      .step-title { font-size: 12.5px; font-weight: 700; fill: ${C.ink}; }
      .step-sub { font-size: 10.8px; fill: ${C.muted}; }
`;

function stripNode(x, y, width, label, iconName = "") {
  const textX = iconName ? x + width / 2 + 11 : x + width / 2;
  return `<g>
    <rect x="${x}" y="${y}" width="${width}" height="28" rx="14" fill="${C.panel}" stroke="${C.amber}" stroke-width="1.2"/>
    ${iconName ? icon(iconName, x + 12, y + 6, 16, C.amber, 2) : ""}
    <text x="${textX}" y="${y + 18.5}" text-anchor="middle" class="strip-node">${esc(label)}</text>
  </g>`;
}

function systemArchitecture() {
  const rows = [210, 352, 494];        // consumer/adapter tile tops
  const arrowY = rows.map((y) => y + 22);
  const intRows = [212, 324, 436, 548]; // integration tile tops
  const intArrowY = intRows.map((y) => y + 22);
  const busX = 884;

  const body = `
  ${header("Runtime Architecture", "One daemon owns the broker session; typed adapters serve humans, AI hosts, and the app.")}

  ${legendItem(1157, 40, "slate", "Local typed flow")}
  ${legendItem(1304, 40, "green", "Broker path")}
  ${legendItem(1146, 64, "blue", "Observed data", { dashed: true })}
  ${legendItem(1281, 64, "amber", "Optional remote", { dotted: true })}

  <rect x="224" y="120" width="916" height="548" rx="16" fill="${C.panel}" stroke="${C.muted}" stroke-width="1.2"/>
  <path d="M224 156h916v-20a16 16 0 0 0 -16 -16h-884a16 16 0 0 0 -16 16z" fill="${C.panelAlt}"/>
  <text x="244" y="143" class="boundary">LOCAL IBKR RUNTIME</text>
  <text x="1120" y="143" text-anchor="end" class="mono-small">one binary · several processes · one shared daemon</text>

  <text x="36" y="184" class="layer">1 · CONSUMERS</text>
  <text x="246" y="184" class="layer">2 · SURFACE ADAPTERS</text>
  <text x="452" y="184" class="layer">3 · CONTRACT</text>
  <text x="584" y="184" class="layer">4 · AUTHORITY &amp; DOMAIN</text>
  <text x="898" y="184" class="layer">5 · INTEGRATIONS</text>
  <text x="1164" y="184" class="layer">6 · PROVIDERS / DATA</text>

  ${component({ x: 36, y: rows[0], iconName: "user", color: C.slate, title: "Human Operator", subtitle: ["shell · local browser"], width: 172 })}
  ${component({ x: 36, y: rows[1], iconName: "cpu", color: C.slate, title: "AI / MCP Host", subtitle: ["Claude · Codex", "other MCP clients"], width: 172 })}
  ${component({ x: 36, y: rows[2], iconName: "mobileCode", color: C.yellow, iconColor: C.ink, title: "Canary PWA", subtitle: ["browser", "iOS Home Screen"], width: 172 })}

  ${component({ x: 246, y: rows[0], iconName: "terminal", color: C.slate, title: "CLI / TUI", subtitle: ["argv · stdout"], mono: "internal/cli", width: 180 })}
  ${component({ x: 246, y: rows[1], iconName: "plugConnected", color: C.slate, title: "MCP Adapter", subtitle: ["JSON-RPC 2.0 · stdio"], mono: "internal/mcp", width: 180 })}
  ${component({ x: 246, y: rows[2], iconName: "mobileCode", color: C.yellow, iconColor: C.ink, title: "Canary App Host", subtitle: ["HTTP JSON · SSE"], mono: "internal/app", width: 180 })}

  ${line(`M214 ${arrowY[0]}H240`, "slate")}
  ${line(`M214 ${arrowY[1]}H240`, "slate")}
  ${line(`M214 ${arrowY[2]}H240`, "slate", { both: true })}

  <rect x="452" y="200" width="110" height="450" rx="14" fill="${C.greenSoft}" stroke="${C.greenLine}"/>
  ${icon("exchange", 492, 222, 30, C.greenDark, 1.8)}
  <text x="507" y="286" text-anchor="middle" class="node-title"><tspan x="507">Typed</tspan><tspan x="507" dy="17">RPC</tspan></text>
  ${chipAt(507, 320, "internal/rpc", C.panel)}
  <text x="507" y="376" text-anchor="middle" class="node-sub"><tspan x="507">NDJSON</tspan><tspan x="507" dy="16">frames</tspan><tspan x="507" dy="16">Unix socket</tspan></text>
  <text x="507" y="566" text-anchor="middle" class="node-sub"><tspan x="507">one shared</tspan><tspan x="507" dy="16">contract</tspan><tspan x="507" dy="16">boundary</tspan></text>

  ${line(`M432 ${arrowY[0]}H446`, "slate")}
  ${line(`M432 ${arrowY[1]}H446`, "slate")}
  ${line(`M432 ${arrowY[2]}H446`, "slate")}
  ${line("M562 420H578", "slate")}

  <rect x="584" y="200" width="290" height="450" rx="16" fill="${C.terminal}"/>
  ${iconTile("serverCog", 608, 224, C.green, 46)}
  <text x="668" y="246" style="fill:#ffffff;font-size:18px;font-weight:700">ibkr daemon</text>
  <text x="668" y="267" class="node-sub on-dark">broker + runtime authority</text>

  <rect x="608" y="300" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("settings", 620, 315, 26, "#9fb3d9")}
  <text x="658" y="325" class="module-title">Runtime Orchestration</text>
  <text x="658" y="345" class="module-sub">schedulers · policy execution</text>

  <rect x="608" y="376" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("shieldCheck", 620, 391, 26, "#6fd3c2")}
  <text x="658" y="401" class="module-title">Pure Risk Semantics</text>
  <text x="658" y="421" class="module-sub">internal/risk · no I/O</text>

  <rect x="608" y="452" width="242" height="64" rx="10" fill="${C.terminal2}"/>
  ${icon("fileText", 620, 467, 26, C.yellow)}
  <text x="658" y="477" class="module-title">State Lifecycle</text>
  <text x="658" y="497" class="module-sub">SQLite events · recon · proposals</text>

  <rect x="608" y="560" width="242" height="1" fill="${C.terminalLine}"/>
  <text x="608" y="588" class="mono-small" style="fill:${C.textOnDarkDim}">auto-spawned by clients on demand</text>
  <text x="608" y="606" class="mono-small" style="fill:${C.textOnDarkDim}">default idle timeout 15 min</text>

  ${component({ x: 898, y: intRows[0], iconName: "plug", color: C.green, title: "Broker Connectors", subtitle: ["primary + breadth clients"], mono: "pkg/ibkr", width: 226 })}
  ${component({ x: 898, y: intRows[1], iconName: "databaseImport", color: C.blue, title: "Observed-Source Clients", subtitle: ["Flex · market events · rates"], width: 226 })}
  ${component({ x: 898, y: intRows[2], iconName: "database", color: C.greenDark, title: "Persistence", subtitle: ["daemon.db · evidence · app/data"], width: 226 })}
  ${component({ x: 898, y: intRows[3], iconName: "calendarCode", color: C.slate, title: "Embedded Calendars", subtitle: ["official sessions 2026-2028", "compiled into the binary"], width: 226 })}

  <path d="M874 420H${busX}M${busX} ${intArrowY[0]}V${intArrowY[3]}" fill="none" stroke="${C.slate}" stroke-width="1.5" stroke-linecap="round"/>
  ${junction(busX, 420)}
  ${line(`M${busX} ${intArrowY[0]}H892`, "green")}
  ${line(`M${busX} ${intArrowY[1]}H892`, "blue", { dashed: true })}
  ${line(`M${busX} ${intArrowY[2]}H892`, "slate")}
  ${line(`M898 ${intArrowY[3]}H${busX + 6}`, "slate", { dotted: true })}

  ${component({ x: 1164, y: 212, iconName: "server", color: C.green, title: "TWS / IB Gateway", subtitle: ["selected endpoint + account"], width: 240 })}
  ${chipAt(1284, 292, "TWS wire · TCP/TLS · 2 client IDs", C.greenSoft)}
  ${component({ x: 1164, y: 362, iconName: "worldDownload", color: C.blue, title: "IBKR Data Services", subtitle: ["Flex statements · borrow data"], width: 240 })}
  ${component({ x: 1164, y: 512, iconName: "worldDownload", color: C.blue, title: "Public Market Sources", subtitle: ["Nasdaq · FRED · CBOE · Treasury", "Fed · Wikipedia S&P 500 list"], width: 240 })}
  ${chipAt(1284, 604, "HTTPS · FTP · JSON/CSV/XML/RSS", C.blueSoft)}

  ${line(`M1124 ${intArrowY[0]}H1154`, "green", { both: true })}
  ${line(`M1124 ${intArrowY[1]}H1140V384H1154`, "blue", { dashed: true })}
  ${line(`M1140 ${intArrowY[1]}V534H1154`, "blue", { dashed: true })}
  ${junction(1140, intArrowY[1], C.blue)}

  <rect x="36" y="700" width="700" height="112" rx="14" fill="${C.amberSoft}" stroke="${C.amber}" stroke-width="1.2" stroke-dasharray="8 6"/>
  <text x="56" y="726" class="layer" style="fill:${C.amber}">OPTIONAL REMOTE DELIVERY</text>
  <text x="716" y="726" text-anchor="end" class="legend">pairing, auth, and allowlists stay on the local app</text>

  ${stripNode(56, 742, 104, "Canary PWA")}
  ${line("M168 756H222", "amber", { dotted: true, both: true, width: 1.6 })}
  <text x="195" y="748" text-anchor="middle" class="flow-label">HTTPS</text>
  ${stripNode(230, 742, 240, "Cloudflare Relay · Worker + DO", "cloud")}
  ${line("M478 756H568", "amber", { dotted: true, both: true, width: 1.6 })}
  <text x="523" y="748" text-anchor="middle" class="flow-label">outbound WSS</text>
  ${stripNode(576, 742, 84, "ibkr app")}

  ${stripNode(56, 778, 84, "ibkr app")}
  ${line("M148 792H244", "amber", { dotted: true, width: 1.6 })}
  <text x="196" y="788" text-anchor="middle" class="flow-label">VAPID Web Push</text>
  ${stripNode(252, 778, 190, "Browser Push Service", "bell")}
  ${line("M450 792H568", "amber", { dotted: true, width: 1.6 })}
  <text x="509" y="788" text-anchor="middle" class="flow-label">redacted payloads</text>
  ${stripNode(576, 778, 104, "Canary PWA")}

  <text x="1404" y="806" text-anchor="end" class="footnote">deterministic SVG · docs/diagrams/render-architecture.mjs · icons: Tabler 3.45 (MIT)</text>
  `;

  return svgFrame({
    width: 1440,
    height: 848,
    title: "Runtime Architecture (ibkr canary)",
    description: "Six architecture layers show consumers, surface adapters, the typed RPC contract, daemon authority, integration clients, and external providers. Optional remote delivery is isolated from broker and observed-data paths.",
    body,
  });
}

function matrixTile({ x, y, width, height, iconName, color, accent, title, lines = [], format = "", sensitive = false, optional = false }) {
  return `<g role="group" aria-label="${esc([title, ...lines, format].filter(Boolean).join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="12" fill="${C.panel}" stroke="${optional ? C.amber : C.line}"${optional ? ' stroke-dasharray="7 5"' : ""}/>
    <rect x="${x + 12}" y="${y}" width="${width - 24}" height="4" rx="2" fill="${accent}"/>
    ${iconTile(iconName, x + 14, y + 16, color, 36)}
    <text x="${x + 64}" y="${y + 33}" class="tile-title">${esc(title)}</text>
    ${sensitive ? icon("lock", x + width - 30, y + 16, 16, C.amber, 2) : ""}
    ${textLines(x + 64, y + 54, lines, "tile-sub", 15)}
    ${format ? `<text x="${x + 14}" y="${y + height - 13}" class="format">${esc(format)}</text>` : ""}
  </g>`;
}

function emptyCell(x, y, width, height) {
  return `<text x="${x + width / 2}" y="${y + height / 2 + 4}" text-anchor="middle" class="subtitle">—</text>`;
}

function persistenceArchitecture() {
  const colW = 256;
  const xs = [182, 452, 722, 992];
  const rowTops = [178, 330, 584, 748];   // operator, daemon (tall: index tile), canary, relay
  const tileY = (row) => rowTops[row] + 18;

  const ownerCell = (row, iconName, color, name, sub, iconColor = "#ffffff") => `
    ${iconTile(iconName, 42, rowTops[row] + 24, color, 42, iconColor)}
    <text x="42" y="${rowTops[row] + 92}" class="matrix-owner">${esc(name)}</text>
    <text x="42" y="${rowTops[row] + 110}" class="matrix-sub">${esc(sub)}</text>`;

  const colHead = (index, head, sub) => `
    <text x="${xs[index] + colW / 2}" y="150" text-anchor="middle" class="matrix-head">${esc(head)}</text>
    <text x="${xs[index] + colW / 2}" y="167" text-anchor="middle" class="matrix-sub">${esc(sub)}</text>`;

  const body = `
  ${header("State Ownership and Lifecycle", "Who owns which state, where it lives, and what survives a restart.")}

  ${legendItem(890, 40, "green", "Durable authority / evidence")}
  ${legendItem(1135, 40, "blue", "Refreshable")}
  ${legendItem(820, 64, "slate", "Runtime / recovery-only")}
  ${legendItem(1035, 64, "amber", "Optional hosted", { dashed: true })}
  ${icon("lock", 1175, 55, 15, C.amber, 2)}
  <text x="1196" y="68" class="legend">Sensitive</text>

  <rect x="24" y="124" width="1232" height="766" rx="16" fill="${C.panel}" stroke="${C.line}"/>
  <path d="M24 178h1232v-38a16 16 0 0 0 -16 -16h-1200a16 16 0 0 0 -16 16z" fill="${C.panelAlt}"/>
  <text x="42" y="156" class="boundary">OWNER</text>
  ${colHead(0, "Authored / External", "human or external authority")}
  ${colHead(1, "Durable Product Authority", "survives process restart")}
  ${colHead(2, "Refreshable Runtime", "in-memory or source-refreshed")}
  ${colHead(3, "Runtime & Recovery", "IPC, logs, recovery-only")}

  <line x1="174" y1="124" x2="174" y2="890" stroke="${C.line}"/>
  <line x1="24" y1="330" x2="1256" y2="330" stroke="${C.line}"/>
  <line x1="24" y1="584" x2="541" y2="584" stroke="${C.line}"/>
  <line x1="889" y1="584" x2="1256" y2="584" stroke="${C.line}"/>
  <line x1="24" y1="748" x2="1256" y2="748" stroke="${C.line}"/>

  ${ownerCell(0, "user", C.slate, "Operator", "writes and approves")}
  ${matrixTile({ x: xs[0], y: tileY(0), width: colW, height: 118, iconName: "settings", color: C.greenDark, accent: C.greenDark, title: "Config & Policies", lines: ["gateway, account, client pins", "risk-policy.toml · flex-token"], format: "$XDG_CONFIG_HOME/ibkr · TOML", sensitive: true })}
  ${matrixTile({ x: xs[1], y: tileY(0), width: colW, height: 118, iconName: "fileText", color: C.green, accent: C.green, title: "Watchlist / User Data", lines: ["watchlist.json", "explicit research exports"], format: "$XDG_DATA_HOME/ibkr · JSON" })}
  ${emptyCell(xs[2], tileY(0), colW, 118)}
  ${emptyCell(xs[3], tileY(0), colW, 118)}

  ${ownerCell(1, "serverCog", C.terminal, "Daemon", "runtime authority")}
  ${matrixTile({ x: xs[0], y: tileY(1), width: colW, height: 118, iconName: "fileText", color: C.greenDark, accent: C.greenDark, title: "Evidence & Signer Key", lines: ["Flex XML · broker truth", "private preview signer"], format: "state root · XML/private key", sensitive: true })}
  ${matrixTile({ x: xs[1], y: tileY(1), width: colW, height: 218, iconName: "database", color: C.green, accent: C.green, title: "daemon.db Authority", lines: ["settings · safety state", "orders · events · tokens", "market · contract · membership"], format: "SQLite WAL · sole live authority", sensitive: true })}
  ${matrixTile({ x: xs[2], y: tileY(1), width: colW, height: 218, iconName: "refresh", color: C.blue, accent: C.blue, title: "Refreshable Runtime", lines: ["quotes · source fetches", "in-memory derived views", "no JSON cache authority"], format: "memory · refetched/rebuilt" })}
  ${matrixTile({ x: xs[3], y: tileY(1), width: colW, height: 104, iconName: "route", color: C.slate, accent: C.slate, title: "Socket, Locks & Logs", lines: ["IPC and process locks", "rotated text logs"], format: "runtime/state directories" })}
  ${matrixTile({ x: xs[3], y: tileY(1) + 114, width: colW, height: 104, iconName: "shieldCheck", color: C.slate, accent: C.slate, title: "Recovery & Rollback", lines: [".head · verified backups", "hashed sealed legacy"], format: "fail-closed · never live fallback", sensitive: true })}

  <rect x="551" y="572" width="328" height="24" rx="12" fill="${C.panel}" stroke="${C.line}"/>
  ${icon("exchange", 566, 577, 14, C.slate, 2)}
  <text x="588" y="588" class="mono">typed RPC only · no direct DB/file access</text>

  ${ownerCell(2, "mobileCode", C.yellow, "Canary / Device", "separate app authority", C.ink)}
  ${emptyCell(xs[0], tileY(2), colW, 118)}
  ${matrixTile({ x: xs[1], y: tileY(2), width: colW, height: 118, iconName: "database", color: C.green, accent: C.green, title: "App State & Grants", lines: ["device grants · push · alerts", "VAPID · relay credentials"], format: "$XDG_STATE_HOME/ibkr/app · JSON", sensitive: true })}
  ${matrixTile({ x: xs[2], y: tileY(2), width: colW, height: 118, iconName: "refresh", color: C.blue, accent: C.blue, title: "Live App Snapshot", lines: ["periodic polls + quote streams", "memory-only read model"], format: "ephemeral" })}
  ${matrixTile({ x: xs[3], y: tileY(2), width: colW, height: 118, iconName: "browser", color: C.slate, accent: C.slate, title: "Browser / Device State", lines: ["cookies · IndexedDB · P-256 key", "continuity + local recovery"], format: "browser storage", sensitive: true })}

  ${ownerCell(3, "cloud", C.amber, "Hosted Relay", "transport only")}
  ${emptyCell(xs[0], tileY(3), colW, 106)}
  ${emptyCell(xs[1], tileY(3), colW, 106)}
  ${emptyCell(xs[2], tileY(3), colW, 106)}
  ${matrixTile({ x: xs[3], y: tileY(3), width: colW, height: 106, iconName: "cloud", color: C.amber, accent: C.amber, title: "Hosted Relay Route", lines: ["connector token + expiry", "no grants · no broker state"], format: "Cloudflare Durable Object", sensitive: true, optional: true })}

  <text x="1244" y="916" text-anchor="end" class="footnote">deterministic SVG · docs/diagrams/render-architecture.mjs · icons: Tabler 3.45 (MIT)</text>
  `;

  return svgFrame({
    width: 1280,
    height: 930,
    title: "State Ownership and Lifecycle (ibkr canary)",
    description: "A matrix maps operator, daemon, Canary or device, and hosted relay ownership across authored or external authority, durable product authority, refreshable runtime views, and runtime or recovery-only state.",
    body,
  });
}

function policyCard({ x, y, width, height, iconName, color, title, lines, format, dark = false }) {
  const fill = dark ? C.terminal : C.panel;
  const titleFill = dark ? "#ffffff" : C.ink;
  const subClass = dark ? "node-sub on-dark" : "card-sub";
  const formatFill = dark ? C.textOnDarkDim : C.muted;
  return `<g role="group" aria-label="${esc([title, ...lines, format].filter(Boolean).join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="14" fill="${fill}" stroke="${color}" stroke-width="1.2"/>
    ${iconTile(iconName, x + 16, y + 16, color, 38)}
    <text x="${x + 66}" y="${y + 35}" class="card-title" style="fill:${titleFill}">${esc(title)}</text>
    ${textLines(x + 18, y + 72, lines, subClass, 16)}
    <text x="${x + 18}" y="${y + height - 15}" class="mono-small" style="fill:${formatFill}">${esc(format)}</text>
  </g>`;
}

function policyArchitecture() {
  const body = `
  ${header("Policy Authority and Execution", "Approved choices enter through typed boundaries; evidence, enforcement, and reporting stay separate.")}

  ${legendItem(1028, 40, "green", "Operator authority")}
  ${legendItem(1182, 40, "blue", "Observed evidence", { dashed: true })}
  ${legendItem(1028, 64, "slate", "Typed decision flow")}
  ${legendItem(1182, 64, "amber", "Human-only boundary", { dotted: true })}

  <text x="36" y="138" class="layer">1 · AUTHORITY INPUTS</text>
  <text x="392" y="138" class="layer">2 · DAEMON POLICY BOUNDARY</text>
  <text x="744" y="138" class="layer">3 · EVIDENCE &amp; SEMANTICS</text>
  <text x="1090" y="138" class="layer">4 · TYPED SURFACES</text>

  ${policyCard({ x: 36, y: 160, width: 310, height: 126, iconName: "fileText", color: C.greenDark, title: "Risk Constitution", lines: ["operator-authored material limits", "missing values stay unapproved"], format: "policies/risk-policy.toml · no default" })}
  ${policyCard({ x: 36, y: 306, width: 310, height: 126, iconName: "shieldCheck", color: C.green, title: "Advisory Engine Policies", lines: ["protection proposals · opportunities", "custom file or conservative default"], format: "versioned TOML · embedded defaults" })}
  ${policyCard({ x: 36, y: 452, width: 310, height: 126, iconName: "settings", color: C.blue, title: "Runtime Preferences", lines: ["feature switches · overrides", "freeze / limits remain human-owned"], format: "daemon.db state document · CAS" })}
  ${policyCard({ x: 36, y: 598, width: 310, height: 126, iconName: "lock", color: C.slate, title: "Code-Owned Invariants", lines: ["schema · validation · broker gates", "rulebook · Regime · Canary models"], format: "reviewed code + typed contracts", dark: true })}

  <rect x="386" y="120" width="326" height="632" rx="18" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.4"/>
  <path d="M386 156h326v-18a18 18 0 0 0 -18 -18h-290a18 18 0 0 0 -18 18z" fill="${C.greenSoft}"/>
  <text x="406" y="143" class="boundary" style="fill:${C.greenDark}">ONE EXECUTABLE POLICY CONTEXT</text>

  ${policyCard({ x: 410, y: 178, width: 278, height: 126, iconName: "fileText", color: C.greenDark, title: "Strict Load & Validate", lines: ["known keys · units · combinations", "no material value is invented"], format: "internal/risk + daemon managers" })}
  ${policyCard({ x: 410, y: 326, width: 278, height: 126, iconName: "refresh", color: C.green, title: "Version & Fingerprint", lines: ["higher version adopts new content", "same-version change reports drift"], format: "kind · schema · id · version · sha256" })}
  ${policyCard({ x: 410, y: 474, width: 278, height: 126, iconName: "database", color: C.slate, title: "Last-Good Runtime Context", lines: ["active · absent · drift · error", "state and events commit before publish"], format: "daemon-owned · scope-bound" })}

  <rect x="410" y="626" width="278" height="98" rx="12" fill="${C.amberSoft}" stroke="${C.amber}" stroke-dasharray="7 5"/>
  ${icon("lock", 426, 642, 24, C.amber, 2)}
  <text x="462" y="656" class="card-title">Non-overridable gates</text>
  <text x="426" y="681" class="card-sub"><tspan x="426">pins · freeze · preview token · WhatIf</tspan><tspan x="426" dy="16">journal · daemon auth · agent origin</tspan></text>

  ${line("M352 223H404", "green")}
  ${line("M352 369H378V241H404", "green")}
  ${line("M352 515H378V538H404", "blue", { dashed: true })}
  ${line("M352 661H404", "slate")}

  ${policyCard({ x: 744, y: 178, width: 302, height: 132, iconName: "worldDownload", color: C.blue, title: "Typed Evidence", lines: ["account · position · market · statement", "freshness · quality · finality · scope"], format: "missing / stale / partial ≠ zero" })}
  ${policyCard({ x: 744, y: 338, width: 302, height: 132, iconName: "shieldCheck", color: C.green, title: "Pure Evaluation", lines: ["pass · watch · breach", "unknown · unapproved"], format: "internal/risk · no I/O" })}
  ${policyCard({ x: 744, y: 498, width: 302, height: 132, iconName: "route", color: C.slate, title: "Declared Enforcement", lines: ["advisory · shadow · hard gate", "post-trade exception · reductions"], format: "typed result · never UI inference" })}
  ${policyCard({ x: 744, y: 658, width: 302, height: 112, iconName: "database", color: C.greenDark, title: "Durable Reporting", lines: ["state revision + event / evidence"], format: "daemon.db · fingerprinted" })}

  ${line("M718 404H738", "slate")}
  ${line("M895 316V332", "blue", { dashed: true })}
  ${line("M895 476V492", "slate")}
  ${line("M895 636V652", "slate")}

  ${policyCard({ x: 1090, y: 178, width: 314, height: 112, iconName: "terminal", color: C.slate, title: "CLI / JSON", lines: ["policy show · settings · proposals"], format: "operator actions stay explicit" })}
  ${policyCard({ x: 1090, y: 316, width: 314, height: 112, iconName: "plugConnected", color: C.slate, title: "MCP / Agent Hosts", lines: ["read, research, and preview only"], format: "no policy or broker-write tools" })}
  ${policyCard({ x: 1090, y: 454, width: 314, height: 112, iconName: "mobileCode", color: C.yellow, title: "Canary App / PWA", lines: ["renders daemon result and health"], format: "no second verdict", dark: false })}
  ${policyCard({ x: 1090, y: 592, width: 314, height: 112, iconName: "bell", color: C.amber, title: "Alerts & Governance", lines: ["commissioned source and delivery state"], format: "shadow ≠ active delivery" })}

  ${line("M1052 404H1072V234H1084", "slate")}
  ${line("M1072 372H1084", "slate")}
  ${line("M1072 510H1084", "slate")}
  ${line("M1072 648H1084", "slate")}

  <text x="1404" y="786" text-anchor="end" class="footnote">deterministic SVG · docs/diagrams/render-architecture.mjs · icons: Tabler 3.45 (MIT)</text>
  `;

  return svgFrame({
    width: 1440,
    height: 820,
    title: "Policy Authority and Execution (ibkr canary)",
    description: "Four policy authority classes enter the daemon through distinct validation paths, combine with typed evidence in pure risk semantics, and emerge as declared enforcement, durable reporting, and typed operator surfaces.",
    body,
    extraStyles: referenceDiagramStyles,
  });
}

function dbTable({ x, y, width, title, rows, accent = C.green, fill = C.panel }) {
  const height = 42 + rows.length * 18 + 14;
  const content = rows.map((row, index) => {
    const [key, field] = row;
    const yy = y + 54 + index * 18;
    return `<text x="${x + 14}" y="${yy}" class="er-key">${esc(key)}</text><text x="${x + 58}" y="${yy}" class="er-field">${esc(field)}</text>`;
  }).join("");
  return `<g role="group" aria-label="Table ${esc(title)}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="10" fill="${fill}" stroke="${accent}" stroke-width="1.2"/>
    <path d="M${x} ${y + 34}h${width}" stroke="${accent}" stroke-width="1.2"/>
    <text x="${x + 14}" y="${y + 23}" class="er-title">${esc(title)}</text>
    ${content}
  </g>`;
}

function sqliteDataModel() {
  const body = `
  ${header("daemon.db Logical Data Model", "Current state, immutable evidence, order safety, and statement truth in one daemon-owned SQLite authority.")}

  ${legendItem(978, 40, "green", "Current / authoritative")}
  ${legendItem(1168, 40, "slate", "Append-only evidence")}
  ${legendItem(978, 64, "blue", "External evidence link", { dashed: true })}
  ${legendItem(1168, 64, "amber", "Monotonic safety", { dotted: true })}

  <text x="36" y="132" class="layer">CONTROL &amp; CURRENT STATE</text>
  ${dbTable({ x: 36, y: 150, width: 252, title: "store_meta", rows: [["PK", "singleton"], ["", "authority_epoch"], ["", "head / event / signer"]], accent: C.amber })}
  ${dbTable({ x: 36, y: 278, width: 252, title: "schema_migrations", rows: [["PK", "version"], ["", "name · checksum"], ["", "applied_at"]], accent: C.slate })}
  ${dbTable({ x: 36, y: 406, width: 252, title: "state_documents", rows: [["PK", "scope_key + kind"], ["", "revision · document_json"], ["", "sha256 · updated_at"]], accent: C.green })}
  ${dbTable({ x: 36, y: 534, width: 252, title: "legacy_imports", rows: [["PK", "scope + source + hash"], ["", "status · details_json"], ["", "imported_through"]], accent: C.slate })}

  <text x="330" y="132" class="layer">APPEND-ONLY EVENT SPINE &amp; PROJECTIONS</text>
  ${dbTable({ x: 330, y: 150, width: 348, title: "event_log", rows: [["PK", "event_seq"], ["UQ", "scope_key + event_key"], ["", "type · action · origin"], ["", "occurred_at · recorded_at"], ["", "payload_json · sha256"]], accent: C.slate })}
  ${dbTable({ x: 330, y: 320, width: 252, title: "regime_decisions", rows: [["PK/FK", "event_seq"], ["", "stage · readiness"], ["", "verdict · fingerprint"]], accent: C.slate })}
  ${dbTable({ x: 596, y: 320, width: 252, title: "regime_indicators", rows: [["PK/FK", "decision_seq + indicator"], ["", "status · band · value"], ["", "freshness · eligible"]], accent: C.slate })}
  ${dbTable({ x: 330, y: 448, width: 252, title: "rule_transitions", rows: [["PK/FK", "event_seq"], ["", "rule_id · status"], ["", "policy identity"]], accent: C.slate })}
  ${dbTable({ x: 596, y: 448, width: 252, title: "canary_transitions", rows: [["PK/FK", "event_seq"], ["", "action · market_stage"], ["", "input_health · relevance"]], accent: C.slate })}
  ${dbTable({ x: 330, y: 576, width: 252, title: "capital / policy events", rows: [["PK/FK", "event_seq"], ["", "capital_events"], ["", "risk_policy_events"]], accent: C.slate })}
  ${dbTable({ x: 596, y: 576, width: 252, title: "proposal_outcomes", rows: [["PK/FK", "event_seq"], ["", "proposal_key · revision"], ["", "bucket · action · state"]], accent: C.slate })}

  ${line("M510 282V314", "slate")}
  ${line("M510 282H722V314", "slate")}
  ${line("M510 282V442", "slate")}
  ${line("M510 282H722V442", "slate")}
  ${line("M510 282V570", "slate")}
  ${line("M510 282H722V570", "slate")}
  ${line("M588 384H590", "slate")}

  <text x="890" y="132" class="layer">BROKER SCOPE, ORDER SAFETY &amp; OBSERVATIONS</text>
  ${dbTable({ x: 890, y: 150, width: 246, title: "broker_scopes", rows: [["PK", "scope_key"], ["UQ", "binding_sha256"], ["", "endpoint · client · acct · mode"]], accent: C.greenDark })}
  ${dbTable({ x: 1154, y: 150, width: 250, title: "order_events", rows: [["PK/FK", "event_seq"], ["FK", "scope_key"], ["", "type · refs · ids · status"]], accent: C.slate })}
  ${dbTable({ x: 890, y: 278, width: 246, title: "consumed_preview_tokens", rows: [["PK", "token_digest"], ["FK", "scope_key"], ["", "epoch · signer · head"]], accent: C.amber })}
  ${dbTable({ x: 1154, y: 278, width: 250, title: "order_id_floors", rows: [["PK", "floor_scope + scope_key"], ["", "floor · updated_at"], ["", "trigger: never decrease"]], accent: C.amber })}
  ${dbTable({ x: 890, y: 406, width: 514, title: "observations", rows: [["PK", "observation_id"], ["", "scope · source · kind · observed_at"], ["", "payload · sha256 · metadata_json"], ["", "decision_eligible (required)"]], accent: C.blue })}

  ${line("M1142 214H1148", "green")}
  ${line("M1279 282V264H684", "slate")}
  ${line("M1013 264V272", "green")}

  <rect x="322" y="716" width="1082" height="180" rx="16" fill="${C.panel}" stroke="${C.blue}" stroke-width="1.2"/>
  <text x="342" y="744" class="boundary" style="fill:${C.blue}">BROKER STATEMENT PROJECTION · ORIGINAL FLEX XML REMAINS EXTERNAL EVIDENCE</text>
  ${dbTable({ x: 342, y: 764, width: 242, title: "statement_files", rows: [["PK", "scope + file_key"], ["UQ", "+ current sha256"], ["", "status · generated_at"]], accent: C.green })}
  ${dbTable({ x: 604, y: 764, width: 242, title: "statement_equity_days", rows: [["UQ", "scope + account + day"], ["FK", "current file + sha256"], ["", "equity · raw_json"]], accent: C.green })}
  ${dbTable({ x: 866, y: 764, width: 242, title: "statement_file_versions", rows: [["PK", "scope + file + sha256"], ["", "size · status"], ["", "generated · recorded"]], accent: C.slate })}
  ${dbTable({ x: 1128, y: 764, width: 256, title: "statement_equity_day_versions", rows: [["PK", "equity_version_id"], ["FK", "immutable file version"], ["", "day · equity · raw sha256"]], accent: C.slate })}

  ${line("M590 828H598", "green")}
  ${line("M1114 828H1122", "blue", { dashed: true })}
  ${line("M318 806H296V470H294", "blue", { dashed: true })}

  <text x="1404" y="934" text-anchor="end" class="footnote">logical view of schema v1 · canonical DDL: internal/daemon/corestore/schema.go</text>
  `;

  return svgFrame({
    width: 1440,
    height: 968,
    title: "daemon.db Logical Data Model (ibkr canary)",
    description: "A logical entity relationship view groups SQLite control and state tables, the append-only event spine and domain projections, broker scope and order safety tables, retained observations, and current plus immutable broker statement projections.",
    body,
    extraStyles: referenceDiagramStyles,
  });
}

function stepCard({ x, y, width, number, title, lines, color = C.slate }) {
  const height = 104;
  return `<g role="group" aria-label="Step ${number}. ${esc([title, ...lines].join(". "))}">
    <rect x="${x}" y="${y}" width="${width}" height="${height}" rx="12" fill="${C.panel}" stroke="${color}" stroke-width="1.2"/>
    <circle cx="${x + 24}" cy="${y + 25}" r="15" fill="${color}"/>
    <text x="${x + 24}" y="${y + 29}" text-anchor="middle" class="step-no">${number}</text>
    <text x="${x + 48}" y="${y + 29}" class="step-title">${esc(title)}</text>
    ${textLines(x + 16, y + 58, lines, "step-sub", 15)}
  </g>`;
}

function sqliteUpdateLifecycle() {
  const body = `
  ${header("SQLite Mutation and Upgrade Lifecycle", "Every commit advances the authority head; startup validates before RPC or broker connectivity.")}

  ${legendItem(1035, 40, "green", "Validated authority")}
  ${legendItem(1208, 40, "slate", "Local durable flow")}
  ${legendItem(1035, 64, "amber", "Anti-rollback", { dotted: true })}
  ${legendItem(1208, 64, "blue", "Recovery artifact", { dashed: true })}

  <rect x="36" y="126" width="1368" height="214" rx="16" fill="${C.panelAlt}" stroke="${C.line}"/>
  <text x="56" y="154" class="layer">NORMAL MUTATION · ONE SERIALIZED WRITER</text>
  ${stepCard({ x: 56, y: 174, width: 238, number: "1", title: "Validate input", lines: ["scope · revision · payload", "typed invariants"], color: C.slate })}
  ${stepCard({ x: 326, y: 174, width: 238, number: "2", title: "BEGIN transaction", lines: ["CAS state and/or insert", "event · observation · safety rows"], color: C.slate })}
  ${stepCard({ x: 596, y: 174, width: 238, number: "3", title: "Advance head", lines: ["head_generation monotonic", "event_seq when applicable"], color: C.green })}
  ${stepCard({ x: 866, y: 174, width: 238, number: "4", title: "Commit SQLite", lines: ["WAL · FULL sync · foreign keys", "failure publishes nothing"], color: C.green })}
  ${stepCard({ x: 1136, y: 174, width: 248, number: "5", title: "Seal watermark", lines: ["fsync daemon.db.head", "then publish in-memory / RPC"], color: C.amber })}
  ${line("M300 226H320", "slate")}
  ${line("M570 226H590", "slate")}
  ${line("M840 226H860", "green")}
  ${line("M1110 226H1130", "amber", { dotted: true })}

  <rect x="36" y="372" width="1368" height="352" rx="16" fill="${C.panel}" stroke="${C.greenLine}" stroke-width="1.4"/>
  <text x="56" y="402" class="layer">STARTUP GATE · BEFORE ADAPTERS, RPC, SCHEDULERS, OR BROKER CONNECTIONS</text>
  ${stepCard({ x: 56, y: 424, width: 250, number: "1", title: "Inspect published DB", lines: ["locks · file/sidecars · app id", "schema · ledger · min head"], color: C.slate })}
  ${stepCard({ x: 330, y: 424, width: 250, number: "2", title: "Validate authority", lines: ["objects · integrity · foreign keys", "content hashes · counters"], color: C.green })}
  ${stepCard({ x: 604, y: 424, width: 250, number: "3", title: "Compare versions", lines: ["equal → verified open", "newer → refuse downgrade"], color: C.green })}
  ${line("M312 476H324", "slate")}
  ${line("M586 476H598", "green")}

  <path d="M729 534V558H159V574" fill="none" stroke="${C.slate}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" marker-end="url(#arrow-slate)"/>
  <text x="592" y="550" class="mono-small">older, valid schema → out-of-place coordinator</text>

  ${stepCard({ x: 56, y: 580, width: 206, number: "4", title: "Exact-head backup", lines: ["standalone · verified", "source stays published"], color: C.blue })}
  ${stepCard({ x: 282, y: 580, width: 206, number: "5", title: "Migrate candidate", lines: ["same directory · ordered", "transactional migrations"], color: C.slate })}
  ${stepCard({ x: 508, y: 580, width: 206, number: "6", title: "Validate + manifest", lines: ["target objects · hashes", "fsynced recovery phase"], color: C.green })}
  ${stepCard({ x: 734, y: 580, width: 206, number: "7", title: "Arm watermark", lines: ["candidate head bound", "old WAL quiesced"], color: C.amber })}
  ${stepCard({ x: 960, y: 580, width: 206, number: "8", title: "Atomic publish", lines: ["rename candidate", "fsync parent · reopen"], color: C.green })}
  ${stepCard({ x: 1186, y: 580, width: 198, number: "9", title: "Verify + finalize", lines: ["remove manifest", "backup stays recovery-only"], color: C.green })}
  ${line("M268 632H276", "blue", { dashed: true })}
  ${line("M494 632H502", "slate")}
  ${line("M720 632H728", "green")}
  ${line("M946 632H954", "amber", { dotted: true })}
  ${line("M1172 632H1180", "green")}

  <rect x="884" y="424" width="500" height="106" rx="12" fill="${C.amberSoft}" stroke="${C.amber}" stroke-dasharray="7 5"/>
  ${icon("lock", 904, 442, 26, C.amber, 2)}
  <text x="944" y="458" class="card-title">Any ambiguity fails closed</text>
  <text x="904" y="484" class="card-sub"><tspan x="904">corruption · future schema · missing watermark · tamper</tspan><tspan x="904" dy="16">mismatched recovery artifacts · unsafe sidecars</tspan></text>

  <text x="1404" y="758" text-anchor="end" class="footnote">no in-place upgrade · no automatic repair/restore · no legacy-file fallback</text>
  `;

  return svgFrame({
    width: 1440,
    height: 792,
    title: "SQLite Mutation and Upgrade Lifecycle (ibkr canary)",
    description: "The normal mutation lane validates, transactionally commits state and evidence, advances the authority head, seals an external watermark, and only then publishes. Startup validates the entire authority and upgrades older schemas through a verified backup and unpublished candidate before atomic publication.",
    body,
    extraStyles: referenceDiagramStyles,
  });
}

const cleanGeneratedSVG = (svg) => svg.replace(/^[ \t]+$/gm, "");

const outputs = new Map([
  ["system-architecture.svg", systemArchitecture()],
  ["data-and-persistence.svg", persistenceArchitecture()],
  ["policy-authority.svg", cleanGeneratedSVG(policyArchitecture())],
  ["sqlite-data-model.svg", cleanGeneratedSVG(sqliteDataModel())],
  ["sqlite-update-lifecycle.svg", cleanGeneratedSVG(sqliteUpdateLifecycle())],
]);

if (process.argv.includes("--check")) {
  const stale = [];
  for (const [name, expected] of outputs) {
    const output = path.join(here, name);
    if (!fs.existsSync(output) || fs.readFileSync(output, "utf8") !== expected) stale.push(name);
  }
  if (stale.length) {
    console.error(`diagram check: stale output: ${stale.join(", ")}; run node docs/diagrams/render-architecture.mjs`);
    process.exitCode = 1;
  } else {
    console.log(`diagram check: ${outputs.size} SVG diagram(s) match the renderer`);
  }
} else {
  for (const [name, content] of outputs) fs.writeFileSync(path.join(here, name), content);
  console.log(`rendered ${outputs.size} architecture diagrams`);
}
