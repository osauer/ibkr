#!/usr/bin/env node

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const manifestPath = resolve("web/app/manifest.webmanifest");
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));

for (const icon of manifest.icons || []) {
  if (icon.type !== "image/png") continue;
  const match = /^(\d+)x(\d+)$/.exec(icon.sizes || "");
  assert.ok(match, `${icon.src}: manifest PNG size must be WIDTHxHEIGHT`);
  const expected = [Number(match[1]), Number(match[2])];
  const actual = pngDimensions(readFileSync(resolve("web/app", icon.src.replace(/^\//, ""))));
  assert.deepEqual(actual, expected, `${icon.src}: intrinsic PNG size must match manifest`);
}

function pngDimensions(data) {
  assert.ok(data.length >= 24, "PNG is too short");
  assert.equal(data.subarray(0, 8).toString("hex"), "89504e470d0a1a0a", "invalid PNG signature");
  assert.equal(data.subarray(12, 16).toString("ascii"), "IHDR", "PNG has no leading IHDR");
  return [data.readUInt32BE(16), data.readUInt32BE(20)];
}
