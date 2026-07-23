function ruleStatusLabel(status, reason = "") {
  if (status === "not_evaluated") {
    if (reason === "broker_nonissuer") return "broker nonissuer";
    if (reason === "terminal_non_reporting") return "terminal/non-reporting";
    if (reason === "earnings_not_applicable") return "issuer earnings not applicable";
    return "not evaluated";
  }
  return status || "--";
}

function rulesCountSummary(rules = {}) {
  const rows = Array.isArray(rules.rules) ? rules.rules : [];
  const supplied = rules.breach_counts || {};
  const count = (status) => {
    if (typeof supplied[status] === "number") return supplied[status];
    return rows.filter((row) => row?.status === status).length;
  };
  const bits = [];
  for (const [status, label] of [["act", "act"], ["watch", "watch"], ["unknown", "unknown"], ["info", "info"], ["not_evaluated", "not evaluated"]]) {
    const value = count(status);
    if (value) bits.push(`${value} ${label}`);
  }
  if (bits.length) return bits.join(" · ");
  return rows.length > 0 && rows.every((row) => row?.status === "pass") ? "all pass" : "no active breaches";
}

function earningsApplicabilitySummary(rules = {}) {
  const earnings = Array.isArray(rules.earnings) ? rules.earnings : [];
  const broker = earnings.filter((item) => item?.source === "broker_identity" && item?.status === "not_applicable").length;
  const terminal = earnings.filter((item) => item?.source === "verified_terminal" && item?.status === "terminal_non_reporting").length;
  const parts = [];
  if (broker) parts.push(`${broker} broker-proven nonissuer`);
  if (terminal) parts.push(`${terminal} terminal/non-reporting`);
  return parts.length ? `Issuer earnings not applicable: ${parts.join(" · ")}.` : "";
}

function earningsHealthNotes(rules = {}) {
  const health = (rules.input_health || []).find((item) => item?.source === "earnings" && item?.status === "ok");
  if (!health || !Array.isArray(health.notes) || health.notes.length === 0) return "";
  return `Earnings evidence informational issue: ${health.notes.join("; ")}.`;
}

export { earningsApplicabilitySummary, earningsHealthNotes, ruleStatusLabel, rulesCountSummary };
