package ibkr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func parseExecutionReport(fields []string, serverVersion int) (*ExecutionReport, error) {
	if len(fields) < 2 {
		return nil, fmt.Errorf("execution fields missing")
	}

	idx := 1
	version := serverVersion
	if serverVersion < minServerVerLastLiquidity {
		vStr := pop(fields, &idx)
		if vStr == "" {
			return nil, fmt.Errorf("execution version missing")
		}
		v, err := strconv.Atoi(vStr)
		if err != nil {
			return nil, fmt.Errorf("parse execution version: %w", err)
		}
		version = v
	}

	reqID := -1
	if version >= 7 {
		reqID = parseIntDefault(pop(fields, &idx), -1)
	}

	orderID := parseIntDefault(pop(fields, &idx), 0)

	contract := ExecutionContract{}
	contract.ConID = parseIntDefault(pop(fields, &idx), 0)
	contract.Symbol = pop(fields, &idx)
	contract.SecType = pop(fields, &idx)
	contract.Expiry = pop(fields, &idx)
	contract.Strike = parseFloatDefault(pop(fields, &idx), 0)
	contract.Right = pop(fields, &idx)
	if version >= 9 {
		contract.Multiplier = pop(fields, &idx)
	}
	contract.Exchange = pop(fields, &idx)
	contract.Currency = pop(fields, &idx)
	contract.LocalSymbol = pop(fields, &idx)
	if version >= 10 {
		contract.TradingClass = pop(fields, &idx)
	}

	report := &ExecutionReport{
		ReqID:    reqID,
		OrderID:  orderID,
		Contract: contract,
	}

	report.ExecID = pop(fields, &idx)
	report.TimeRaw = pop(fields, &idx)
	report.Account = pop(fields, &idx)
	report.Exchange = pop(fields, &idx)
	report.Side = strings.ToUpper(strings.TrimSpace(pop(fields, &idx)))
	report.Shares = parseFloatDefault(pop(fields, &idx), 0)
	report.Price = parseFloatDefault(pop(fields, &idx), 0)
	report.PermID = parseIntDefault(pop(fields, &idx), 0)
	report.ClientID = parseIntDefault(pop(fields, &idx), 0)
	report.Liquidation = parseIntDefault(pop(fields, &idx), 0)

	if version >= 6 {
		report.CumQty = parseFloatDefault(pop(fields, &idx), 0)
		report.AvgPrice = parseFloatDefault(pop(fields, &idx), 0)
	}

	if version >= 8 {
		report.OrderRef = pop(fields, &idx)
	}

	if version >= 9 {
		report.EvRule = pop(fields, &idx)
		report.EvMultiplier = parseFloatDefault(pop(fields, &idx), 0)
	}

	if serverVersion >= minServerVerModelsSupport {
		report.ModelCode = pop(fields, &idx)
	}
	if serverVersion >= minServerVerLastLiquidity {
		report.LastLiquidity = parseIntDefault(pop(fields, &idx), 0)
	}
	if serverVersion >= minServerVerPendingPriceRevision {
		report.PendingPriceRevision = parseBool(pop(fields, &idx))
	}
	if serverVersion >= minServerVerSubmitter {
		report.Submitter = pop(fields, &idx)
	}

	if ts, err := parseIBTimestamp(report.TimeRaw); err == nil {
		report.Timestamp = ts
	}

	return report, nil
}

func parseCommissionAndFees(fields []string) (*CommissionReport, error) {
	if len(fields) < 2 {
		return nil, fmt.Errorf("commission fields missing")
	}

	idx := 1
	report := &CommissionReport{}
	report.ExecID = pop(fields, &idx)
	report.CommissionAndFees = parseFloatDefault(pop(fields, &idx), 0)
	report.Currency = pop(fields, &idx)
	report.RealizedPNL = parseFloatDefault(pop(fields, &idx), 0)
	report.Yield = parseFloatDefault(pop(fields, &idx), 0)
	report.YieldRedemptionDate = parseIntDefault(pop(fields, &idx), 0)

	return report, nil
}

func pop(fields []string, idx *int) string {
	if *idx >= len(fields) {
		return ""
	}
	v := fields[*idx]
	*idx = *idx + 1
	return v
}

func parseIntDefault(value string, def int) int {
	if strings.TrimSpace(value) == "" {
		return def
	}
	if iv, err := strconv.Atoi(value); err == nil {
		return iv
	}
	return def
}

func parseFloatDefault(value string, def float64) float64 {
	if strings.TrimSpace(value) == "" {
		return def
	}
	if fv, err := strconv.ParseFloat(value, 64); err == nil {
		return fv
	}
	return def
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "t":
		return true
	default:
		return false
	}
}

func parseIBTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	layouts := []string{
		"20060102 15:04:05 MST",
		"20060102 15:04:05",
		"20060102-15:04:05", // legacy format with dash
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	// Attempt to normalise double-space separators
	normalised := strings.ReplaceAll(raw, "  ", " ")
	for _, layout := range layouts {
		if t, err := time.Parse(layout, normalised); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unable to parse IB timestamp: %s", raw)
}
