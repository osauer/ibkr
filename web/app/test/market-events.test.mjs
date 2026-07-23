import assert from "node:assert/strict";
import test from "node:test";

import { positionsHaveShortStock, relevantMarketEventHealth } from "../exposure-relevance.js";
import { unknownEventRuleNote } from "../earnings-relevance.js";
import { earningsApplicabilitySummary, earningsHealthNotes, ruleStatusLabel, rulesCountSummary, wshEntitlementNotice } from "../rules-presentation.js";

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

test("rendered source health hides borrow only for a known all-long book", () => {
  const events = { source_health: [
    { source: "borrow_fee", status: "unavailable" },
    { source: "halts", status: "unavailable" },
  ] };
  const sources = (positions) => relevantMarketEventHealth(events, positions).map((item) => item.source);

  assert.deepEqual(sources({ stocks: [{ sec_type: "STK", quantity: 10 }] }), ["halts"]);

  assert.deepEqual(sources({ stocks: [{ sec_type: "STK", quantity: -1 }] }), ["borrow_fee", "halts"]);

  assert.deepEqual(sources(undefined), ["borrow_fee", "halts"], "unknown positions must fail visible");
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

test("broker nonissuer and terminal applicability are not rendered as unresolved", () => {
  const note = unknownEventRuleNote({
    rules: [{ id: "catalyst_coverage", number: 6, status: "unknown" }],
    earnings: [
      { symbol: "AAA", source: "broker_identity", status: "not_applicable", reason: "broker_nonissuer" },
      { symbol: "BBB", source: "verified_terminal", status: "terminal_non_reporting" },
      { symbol: "CCC", source: "unknown", status: "no_date_published" },
    ],
  });
  assert.match(note, /CCC \(No Date Published\)/);
  assert.doesNotMatch(note, /AAA|BBB|Broker Nonissuer|Terminal Non Reporting/);
});

test("rules summary never calls not-evaluated applicability rows all pass", () => {
  const rules = {
    breach_counts: { pass: 13, not_evaluated: 1 },
    rules: [{ status: "not_evaluated", reason: "broker_nonissuer" }],
  };
  assert.equal(rulesCountSummary(rules), "1 not evaluated");
  assert.doesNotMatch(rulesCountSummary(rules), /all pass/i);
  assert.equal(rulesCountSummary({ rules: [{ status: "pass" }] }), "all pass");
});

test("not-evaluated rule labels distinguish broker and terminal classes", () => {
  assert.equal(ruleStatusLabel("not_evaluated", "broker_nonissuer"), "broker nonissuer");
  assert.equal(ruleStatusLabel("not_evaluated", "terminal_non_reporting"), "terminal/non-reporting");
  assert.equal(ruleStatusLabel("not_evaluated", "earnings_not_applicable"), "issuer earnings not applicable");
});

test("Canary positively summarizes both earnings applicability classes", () => {
  const summary = earningsApplicabilitySummary({ earnings: [
    { source: "broker_identity", status: "not_applicable" },
    { source: "verified_terminal", status: "terminal_non_reporting" },
  ] });
  assert.match(summary, /1 broker-proven nonissuer/);
  assert.match(summary, /1 terminal\/non-reporting/);
});

test("Canary renders retained earnings evidence issues as informational", () => {
  const note = earningsHealthNotes({ input_health: [{
    source: "earnings",
    status: "ok",
    notes: ["retained broker identity issue: code=contract_unavailable stage=wsh_contract_resolve retry=scheduled"],
  }] });
  assert.match(note, /informational issue/);
  assert.match(note, /contract_unavailable/);
  assert.equal(earningsHealthNotes({ input_health: [{ source: "earnings", status: "degraded", notes: ["not_observed"] }] }), "");
});

test("WSH subscription notice requires the exact typed non-retryable entitlement tuple", () => {
  const fixture = (provider, code, stage, retryable) => ({ earnings: [{
    symbol: "SYNTH1",
    providers: [{ provider, last_failure: { code, stage, retryable, message: "untrusted provider prose" } }],
  }] });
  assert.match(wshEntitlementNotice(fixture("ibkr_wsh", "not_entitled", "wsh_metadata", false)), /Wall Street Horizon earnings feed/);
  assert.match(wshEntitlementNotice(fixture("ibkr_wsh", "not_entitled", "wsh_event", false)), /unknown, never pass/);
  for (const [provider, code, stage, retryable] of [
    ["nasdaq", "not_entitled", "wsh_metadata", false],
    ["ibkr_wsh", "invalid_payload", "wsh_metadata", false],
    ["ibkr_wsh", "not_entitled", "wsh_contract_resolve", false],
    ["ibkr_wsh", "not_entitled", "wsh_metadata", true],
  ]) {
    assert.equal(wshEntitlementNotice(fixture(provider, code, stage, retryable)), "");
  }
  assert.doesNotMatch(wshEntitlementNotice(fixture("ibkr_wsh", "not_entitled", "wsh_metadata", false)), /untrusted provider prose|ibkr_wsh|not_entitled/);
});
