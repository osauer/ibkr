function positionsHaveShortStock(positions = {}) {
  const stocks = positions?.stocks || [];
  if (stocks.some((stock) => positionIsStock(stock) && typeof stock?.quantity === "number" && stock.quantity < 0)) return true;
  const groups = positions?.by_underlying || [];
  return groups.some((group) => positionIsStock(group?.stock) && typeof group.stock?.quantity === "number" && group.stock.quantity < 0);
}

function positionIsStock(position) {
  const secType = String(position?.sec_type || "").trim().toUpperCase();
  // Empty is a compatibility-safe legacy stock projection. Explicit futures,
  // indexes, and options never activate stock-borrow evidence.
  return secType === "" || secType === "STOCK" || secType === "STK" || secType === "ETF";
}

function borrowSourceHealth(health = {}) {
  const source = String(health.source || "").toLowerCase();
  return source.includes("borrow_inventory") || source.includes("borrow_fee");
}

function marketEventHealthVisible(health = {}) {
  const status = String(health.status || "").toLowerCase();
  return status === "unknown" || status === "stale" || status === "degraded" || status === "partial" || status === "error" || status === "unavailable";
}

function relevantMarketEventHealth(events = {}, positions) {
  // An all-long book makes borrow-feed health irrelevant. An unavailable
  // positions snapshot cannot prove that condition, so keep the health row
  // visible until the book is known.
  const includeBorrow = !positions || positionsHaveShortStock(positions);
  return (events.source_health || [])
    .filter(marketEventHealthVisible)
    .filter((health) => includeBorrow || !borrowSourceHealth(health));
}

export { borrowSourceHealth, marketEventHealthVisible, positionsHaveShortStock, relevantMarketEventHealth };
