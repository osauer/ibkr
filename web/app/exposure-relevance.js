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

export { positionsHaveShortStock };
