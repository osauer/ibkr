#!/usr/bin/env node

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const manifestPath = resolve("web/app/manifest.webmanifest");
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));

const requiredPNGDimensions = new Map([
  ["favicon-16.png", [16, 16]],
  ["favicon-32.png", [32, 32]],
  ["favicon-64.png", [64, 64]],
  ["icon-192.png", [192, 192]],
  ["icon-512.png", [512, 512]],
]);

for (const [name, expected] of requiredPNGDimensions) {
  const actual = pngDimensions(readFileSync(resolve("web/app", name)));
  assert.deepEqual(actual, expected, `${name}: intrinsic PNG size must be ${expected.join("x")}`);
}

for (const icon of manifest.icons || []) {
  if (icon.type !== "image/png") continue;
  const match = /^(\d+)x(\d+)$/.exec(icon.sizes || "");
  assert.ok(match, `${icon.src}: manifest PNG size must be WIDTHxHEIGHT`);
  const expected = [Number(match[1]), Number(match[2])];
  const actual = pngDimensions(readFileSync(resolve("web/app", icon.src.replace(/^\//, ""))));
  assert.deepEqual(actual, expected, `${icon.src}: intrinsic PNG size must match manifest`);
}

const manifestPNGs = new Set((manifest.icons || [])
  .filter((icon) => icon.type === "image/png")
  .map((icon) => String(icon.src || "").replace(/^\//, "")));
for (const name of ["icon-192.png", "icon-512.png"]) {
  assert.ok(manifestPNGs.has(name), `${name}: required app icon is missing from the manifest`);
}

function pngDimensions(data) {
  assert.ok(data.length >= 24, "PNG is too short");
  assert.equal(data.subarray(0, 8).toString("hex"), "89504e470d0a1a0a", "invalid PNG signature");
  assert.equal(data.subarray(12, 16).toString("ascii"), "IHDR", "PNG has no leading IHDR");
  return [data.readUInt32BE(16), data.readUInt32BE(20)];
}
