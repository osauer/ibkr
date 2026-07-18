import { $, money, readJSONOrText, shortTimeWithZone } from "./shared.js";
import { state } from "./state.js";

const attemptedStampFingerprints = new Set();
const pendingStampFingerprints = new Set();
const stampOutcomes = new Map();
const signoffOutcomes = new Map();
let visibilityBound = false;

function setupBriefVisibility() {
  if (visibilityBound) return;
  visibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") renderBriefCard(state.snapshot || {});
  });
  const dashboard = $("dashboard");
  if (dashboard && typeof MutationObserver !== "undefined") {
    const tabObserver = new MutationObserver(() => {
      if (!dashboard.hidden && state.activeTab === "monitor") renderBriefCard(state.snapshot || {});
    });
    tabObserver.observe(dashboard, { attributes: true, attributeFilter: ["hidden"] });
  }
}

function renderBriefCard(snap = state.snapshot || {}) {
  const panel = $("briefPanel");
  if (!panel) return;
  panel.hidden = !state.authenticated;
  if (panel.hidden) return;

  renderBriefSource(snap.sources?.brief);
  const brief = snap.brief;
  if (!brief) {
    $("briefAsOf").textContent = "--";
    $("briefSections").replaceChildren(briefEmptyState());
    renderBriefAckStatus(null);
    return;
  }

  $("briefAsOf").textContent = shortTimeWithZone(brief.as_of);
  $("briefSections").replaceChildren(
    renderMarketSection(brief.market || {}),
    renderCalendarSection(brief.calendar || {}),
    renderPortfolioSection(brief.portfolio || {}),
    renderRiskSection(brief.risk_limits || {}),
    renderProcessSection(brief.process || {}, brief),
  );
  renderBriefAckStatus(brief);
  scheduleBriefStamp(brief);
}

function briefEmptyState() {
  const empty = document.createElement("p");
  empty.className = "brief-empty";
  empty.textContent = "Brief data is unavailable.";
  return empty;
}

function renderBriefSource(source = {}) {
  const banner = $("briefSourceBanner");
  const message = String(source?.error || "");
  banner.hidden = !message;
  if (!message) {
    banner.replaceChildren();
    return;
  }
  const label = document.createElement("span");
  label.textContent = "Degraded";
  const detail = document.createElement("b");
  detail.textContent = message;
  banner.replaceChildren(label, detail);
}

function renderMarketSection(section) {
  return briefSection("A", "Market", section, [
    briefRow("Regime", section.regime, joinValues(section.regime?.stage, section.regime?.verdict)),
    briefRow("Breadth", section.breadth, joinValues(
      fieldValue(section.breadth, "pct_above_50dma", "50-DMA", "%"),
      fieldValue(section.breadth, "pct_above_200dma", "200-DMA", "%"),
      fieldValue(section.breadth, "net_new_highs_pct", "Net new highs", "%"),
      fieldValue(section.breadth, "data_type", "Data"),
    )),
    briefRow("Dealer gamma", section.gamma, joinValues(
      fieldValue(section.gamma, "spot", "Spot"),
      fieldValue(section.gamma, "zero_gamma", "Zero gamma"),
      fieldValue(section.gamma, "gap_pct", "Gap", "%"),
      fieldValue(section.gamma, "gamma_sign", "Sign"),
    )),
    briefRow("Canary", section.canary, joinValues(section.canary?.action, section.canary?.severity, section.canary?.summary)),
  ]);
}

function renderCalendarSection(section) {
  const rows = [
    briefRow("Session", section.session, joinValues(section.session?.market, section.session?.state)),
  ];
  for (const event of section.market_events || []) {
    rows.push(briefRow(`Event · ${event.kind || "--"}`, event, joinValues(
      fieldValue(event, "count", "Count"),
      (event.symbols || []).join(", "),
    )));
  }
  return briefSection("B", "Calendar", section, rows);
}

function renderPortfolioSection(section) {
  const account = section.account || {};
  const currency = account.base_currency || "";
  return briefSection("C", "Portfolio", section, [
    briefRow("Account", account, joinValues(
      moneyValue(account, "equity_base", currency, "Equity"),
      moneyValue(account, "daily_pnl_base", currency, "Daily P/L"),
    )),
    briefRow("Movers", section.movers, (section.movers?.rows || []).map((row) => `${row.symbol || "--"} ${money(row.daily_pnl_base, currency)}`).join(" · ")),
    briefRow("Premium at risk", section.premium_at_risk, moneyCoverageValue(section.premium_at_risk)),
    briefRow("Hedge cost / day", section.hedge_cost, moneyCoverageValue(section.hedge_cost)),
    briefRow("Working orders", section.working_orders, fieldValue(section.working_orders, "count", "Count")),
  ]);
}

function renderRiskSection(section) {
  const capital = section.capital || {};
  return briefSection("D", "Risk & limits", section, [
    briefRow("Capital", capital, joinValues(
      capital.tier,
      capital.enforcement,
      fieldValue(capital, "consumed_pct", "Consumed", "%"),
      moneyValue(capital, "drawdown_base", capital.base_currency, "Drawdown"),
      moneyValue(capital, "adjusted_peak_base", capital.base_currency, "Adjusted peak"),
    )),
    briefRow("Drawdown latch", section.latch, joinValues(
      hasField(section.latch, "latched") ? (section.latch.latched ? "latched" : "open") : "",
      fieldValue(section.latch, "age_days", "Age", " day(s)"),
      dateValue(section.latch?.latched_at),
    )),
    briefRow("Active overrides", section.overrides, (section.overrides?.rows || []).map((row) => joinValues(row.control, dateValue(row.expires_at))).join(" · ")),
    briefRow("Policy drift", section.policy_drift, (section.policy_drift?.rows || []).map((row) => joinValues(row.policy, row.status, row.live_id, row.live_version)).join(" · ")),
  ]);
}

function renderProcessSection(section, brief) {
  const rows = [
    briefRow("Reconcile", section.reconcile, joinValues(
      dateValue(section.reconcile?.last_reconciled_at),
      section.reconcile?.source,
      section.reconcile?.deadline ? `Due ${dateValue(section.reconcile.deadline)}` : "",
      fieldValue(section.reconcile, "days_remaining", "Days remaining"),
    )),
    briefRow("Auto-extend", section.auto_extend, joinValues(section.auto_extend?.report_id, dateValue(section.auto_extend?.at))),
    renderOneTapRow(section.one_tap || {}, brief),
    briefRow("Rules delta", section.rules_delta, rulesDeltaValue(section.rules_delta || {})),
  ];
  rows.push(briefRow("Artefacts", section.artefacts, ""));
  for (const artefact of section.artefacts?.rows || []) {
    rows.push(briefRow(`Artefact · ${artefact.kind || "--"}`, artefact, joinValues(
      artefact.cadence,
      hasField(artefact, "declared") ? `declared ${artefact.declared}` : "",
      hasField(artefact, "completed") ? `completed ${artefact.completed}` : "",
      dateValue(artefact.completed_at),
    ), "brief-row--nested"));
  }
  return briefSection("E", "Process", section, rows, "brief-section--process");
}

function briefSection(letter, title, section, rows, className = "") {
  const el = document.createElement("section");
  el.className = `brief-section ${className}`.trim();
  const head = document.createElement("div");
  head.className = "brief-section__head";
  const heading = document.createElement("h3");
  heading.textContent = `${letter} ${title}`;
  head.append(heading, statusBadge(section?.status));
  const detail = document.createElement("p");
  detail.className = "brief-section__detail";
  detail.textContent = verbatimText(section?.detail);
  const list = document.createElement("div");
  list.className = "brief-rows";
  list.replaceChildren(...rows);
  el.append(head, detail, list);
  return el;
}

function briefRow(label, row = {}, value = "", className = "") {
  const el = document.createElement("div");
  el.className = `brief-row ${statusClass(row?.status)} ${className}`.trim();
  const head = document.createElement("div");
  head.className = "brief-row__head";
  const name = document.createElement("b");
  name.textContent = label;
  head.append(name, statusBadge(row?.status));
  const provided = document.createElement("p");
  provided.className = "brief-row__value";
  provided.textContent = rowText(value);
  const detail = document.createElement("p");
  detail.className = "brief-row__detail";
  detail.textContent = verbatimText(row?.detail);
  el.append(head, provided, detail);
  return el;
}

function renderOneTapRow(row, brief) {
  const el = briefRow("One-tap sign-off", row, row.report_id || "--");
  const blockers = document.createElement("ul");
  blockers.className = "brief-blockers";
  for (const blocker of row.blockers || []) {
    const item = document.createElement("li");
    item.textContent = String(blocker);
    blockers.append(item);
  }
  if (blockers.childElementCount > 0) el.append(blockers);

  const reportID = String(row.report_id || "");
  const fingerprint = String(brief.brief_fingerprint || "");
  const outcome = signoffOutcome(fingerprint, reportID);
  if (row.signable === true && reportID && !outcome.result?.ok) {
    const button = document.createElement("button");
    button.id = "briefSignoffButton";
    button.type = "button";
    button.className = "primary brief-signoff";
    button.textContent = outcome.busy ? `Signing off report ${reportID}` : `Sign off report ${reportID} — clean`;
    button.disabled = outcome.busy;
    button.addEventListener("click", () => submitReconcileSignoff(fingerprint, reportID));
    el.append(button);
  }
  const message = outcome.error || outcome.result?.message || (outcome.result?.ok ? `Report ${reportID} signed off.` : "");
  if (message) {
    const receipt = document.createElement("p");
    receipt.className = outcome.error ? "brief-action-message brief-action-message--error" : "brief-action-message";
    receipt.textContent = message;
    el.append(receipt);
  }
  return el;
}

async function submitReconcileSignoff(fingerprint, reportID) {
  const outcome = signoffOutcome(fingerprint, reportID);
  if (outcome.busy || outcome.result?.ok) return;
  outcome.busy = true;
  outcome.error = "";
  renderBriefCard(state.snapshot || {});
  try {
    const res = await fetch("/api/recon/signoff", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ report_id: reportID }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    outcome.result = body;
  } catch (err) {
    outcome.error = err.message;
  } finally {
    outcome.busy = false;
    renderBriefCard(state.snapshot || {});
  }
}

function signoffOutcome(fingerprint, reportID) {
  const key = `${fingerprint}\u0000${reportID}`;
  if (!signoffOutcomes.has(key)) signoffOutcomes.set(key, { busy: false, result: null, error: "" });
  return signoffOutcomes.get(key);
}

function scheduleBriefStamp(brief) {
  const fingerprint = String(brief?.brief_fingerprint || "");
  if (!brief?.stamp_target || !fingerprint || attemptedStampFingerprints.has(fingerprint) || pendingStampFingerprints.has(fingerprint)) return;
  if (!briefStampVisible()) return;
  pendingStampFingerprints.add(fingerprint);
  const afterRender = globalThis.requestAnimationFrame || ((callback) => globalThis.setTimeout(callback, 0));
  afterRender(() => {
    pendingStampFingerprints.delete(fingerprint);
    if (!briefStampVisible() || state.snapshot?.brief?.brief_fingerprint !== fingerprint || attemptedStampFingerprints.has(fingerprint)) return;
    attemptedStampFingerprints.add(fingerprint);
    acknowledgeBrief(brief, fingerprint);
  });
}

function briefStampVisible() {
  const panel = $("briefPanel");
  return state.authenticated === true && state.activeTab === "monitor" && panel && !panel.hidden && document.visibilityState === "visible";
}

async function acknowledgeBrief(brief, fingerprint) {
  try {
    const res = await fetch("/api/brief/seen", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ kind: brief.stamp_target, brief_fingerprint: fingerprint }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    stampOutcomes.set(fingerprint, { result: body, error: "" });
  } catch (err) {
    stampOutcomes.set(fingerprint, { result: null, error: err.message });
  } finally {
    renderBriefCard(state.snapshot || {});
  }
}

function renderBriefAckStatus(brief) {
  const target = $("briefAckStatus");
  const outcome = brief ? stampOutcomes.get(String(brief.brief_fingerprint || "")) : null;
  if (!outcome) {
    target.hidden = true;
    target.textContent = "";
    return;
  }
  target.hidden = false;
  target.classList.toggle("brief-receipt--error", Boolean(outcome.error));
  if (outcome.error) {
    target.textContent = `Render stamp failed: ${outcome.error}`;
    return;
  }
  const result = outcome.result || {};
  target.textContent = result.already_stamped
    ? `${result.kind || "Brief"} artefact · ${result.day || "--"} · already stamped`
    : `${result.kind || "Brief"} artefact stamped · ${result.day || "--"}`;
}

function statusBadge(status) {
  const badge = document.createElement("span");
  badge.className = `brief-status ${statusClass(status)}`.trim();
  badge.textContent = verbatimText(status);
  return badge;
}

function statusClass(status) {
  const normalized = String(status || "").toLowerCase();
  return ["ok", "degraded", "unavailable"].includes(normalized) ? `brief-status--${normalized}` : "";
}

function moneyCoverageValue(row = {}) {
  return joinValues(
    moneyValue(row, "amount_base", row.base_currency, "Amount"),
    fieldValue(row, "included_legs", "Included legs"),
    fieldValue(row, "excluded_legs", "Excluded legs"),
  );
}

function rulesDeltaValue(row = {}) {
  const transitions = (row.transitions || []).map((item) => `${item.rule_id || "--"}: ${item.from || "--"} → ${item.to || "--"}`);
  const added = (row.added || []).map((item) => `added ${item}`);
  const removed = (row.removed || []).map((item) => `removed ${item}`);
  return joinValues(
    ...transitions,
    ...added,
    ...removed,
    hasField(row, "rulebook_fingerprint_changed") ? `fingerprint changed ${row.rulebook_fingerprint_changed}` : "",
  );
}

function moneyValue(object, key, currency, label) {
  if (!hasField(object, key) || typeof object[key] !== "number") return "";
  return `${label} ${money(object[key], currency || "")}`;
}

function fieldValue(object, key, label, suffix = "") {
  if (!hasField(object, key) || object[key] === null || object[key] === "") return "";
  return `${label} ${object[key]}${suffix}`;
}

function hasField(object, key) {
  return Boolean(object) && Object.prototype.hasOwnProperty.call(object, key);
}

function dateValue(value) {
  if (!value) return "";
  const at = new Date(value);
  return Number.isNaN(at.getTime()) ? String(value) : at.toLocaleString();
}

function joinValues(...values) {
  return values.flat().map((value) => String(value || "").trim()).filter(Boolean).join(" · ");
}

function rowText(value) {
  const text = String(value || "").trim();
  return text || "--";
}

function verbatimText(value) {
  return value === undefined || value === null || value === "" ? "--" : String(value);
}

export { renderBriefCard, setupBriefVisibility };
