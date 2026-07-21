import assert from "node:assert/strict";
import test from "node:test";

import { positionsHaveShortStock } from "../exposure-relevance.js";
import { unknownEventRuleNote } from "../earnings-relevance.js";

test("borrow source health follows stock exposure across both position projections", () => {
  const positions = { stocks: [{ symbol: "XYZ", sec_type: "STOCK", quantity: 100 }], by_underlying: [] };
  assert.equal(positionsHaveShortStock(positions), false);
  positions.stocks[0].quantity = -1;
  assert.equal(positionsHaveShortStock(positions), true);
  assert.equal(positionsHaveShortStock({
    stocks: [],
    by_underlying: [{ underlying: "XYZ", stock: { sec_type: "STK", quantity: -2 } }],
  }), true);
  assert.equal(positionsHaveShortStock({ stocks: [{ sec_type: "FUT", quantity: -2 }] }), false);
  assert.equal(positionsHaveShortStock({ by_underlying: [{ stock: { sec_type: "IND", quantity: -2 } }] }), false);
  assert.equal(positionsHaveShortStock({ stocks: [{ quantity: -2 }] }), true, "legacy empty sec_type remains stock");
});

test("a sole unresolved earnings catalyst is rendered explicitly", () => {
  const note = unknownEventRuleNote({
    rules: [{ id: "catalyst_coverage", number: 6, title: "Option outlives its catalyst", status: "unknown" }],
    earnings: [{ symbol: "xyz", source: "unknown", status: "no_date_published", reason: "no_date_published" }],
  });
  assert.match(note, /Earnings unresolved \(XYZ \(No Date Published\)\)/);
  assert.match(note, /held-name earnings controls cannot be confirmed/);
});

test("unresolved earnings remain visible beside another known date", () => {
  const note = unknownEventRuleNote({
    rules: [{ id: "overwrite_earnings", number: 7, status: "unknown" }],
    earnings: [
      { symbol: "AAA", source: "fetched", status: "date", date: "2026-08-01" },
      { symbol: "BBB", source: "unknown", status: "transport_failure", reason: "transport_failure" },
    ],
  });
  assert.match(note, /BBB \(Transport Failure\)/);
  assert.match(note, /other dates ahead: AAA 2026-08-01/);
});
