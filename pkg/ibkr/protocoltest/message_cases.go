package protocoltest

import "time"

// MessageCase describes a single outbound IBKR message to encode.
type MessageCase struct {
	Name        string
	Fields      []interface{}
	Description string
}

// SampleCases enumerates the outbound messages exercised by the protocol
// harness using representative field values for server version 203.
var SampleCases = []MessageCase{
	{
		Name:        "startAPI",
		Description: "Session bootstrap message including client ID and empty capabilities",
		Fields:      []interface{}{71, 2, 101, ""},
	},
	{
		Name:        "reqCurrentTime",
		Description: "Heartbeat request for server clock",
		Fields:      []interface{}{49, "1"},
	},
	{
		Name:        "reqManagedAccts",
		Description: "Request managed accounts list",
		Fields:      []interface{}{17, "1"},
	},
	{
		Name:        "reqAccountSummary",
		Description: "Account summary subscription",
		Fields:      []interface{}{62, "1", 9001, "All", "NetLiquidation,TotalCashValue"},
	},
	{
		Name:        "cancelAccountSummary",
		Description: "Cancel account summary subscription",
		Fields:      []interface{}{63, "1", 9001},
	},
	{
		Name:        "reqPositions",
		Description: "Request positions subscription",
		Fields:      []interface{}{61, "1"},
	},
	{
		Name:        "cancelPositions",
		Description: "Cancel positions subscription",
		Fields:      []interface{}{64, "1"},
	},
	{
		Name:        "reqAcctData",
		Description: "Legacy account updates subscription",
		Fields:      []interface{}{6, "2", "1", "DU1234567"},
	},
	{
		Name:        "reqMktData",
		Description: "Market data subscription (reqMktData v11)",
		Fields:      reqMktDataFields(),
	},
	{
		Name:        "cancelMktData",
		Description: "Cancel market data subscription",
		Fields:      []interface{}{2, 1, 381},
	},
	{
		Name:        "reqMarketDataType",
		Description: "Switch market data type to live",
		Fields:      []interface{}{59, 1, 1},
	},
	{
		Name:        "reqContractData",
		Description: "Contract details request",
		Fields:      reqContractDataFields(),
	},
	{
		Name:        "reqHistoricalData",
		Description: "Historical data request for daily bars",
		Fields:      reqHistoricalDataFields(),
	},
	{
		Name:        "cancelHistoricalData",
		Description: "Cancel historical data request",
		Fields:      []interface{}{25, 1, 9001},
	},
	{
		Name:        "reqOpenOrders",
		Description: "Fetch client open orders",
		Fields:      []interface{}{5, 1},
	},
	{
		Name:        "reqAllOpenOrders",
		Description: "Fetch open orders for all clients",
		Fields:      []interface{}{16, 1},
	},
	{
		Name:        "reqAutoOpenOrders",
		Description: "Enable auto-open order binding",
		Fields:      []interface{}{15, 1, true},
	},
	{
		Name:        "reqExecutions",
		Description: "Execution reports filtered by account",
		Fields:      []interface{}{7, 3, 6001, 0, "DU1234567", "20240501-00:00:00", "ES", "FUT", "GLOBEX", ""},
	},
	{
		Name:        "reqIds",
		Description: "Request new order IDs",
		Fields:      []interface{}{8, 1, 20},
	},
}

func reqMktDataFields() []interface{} {
	fields := []interface{}{
		1,  // reqMktData
		11, // version
		5001,
		0,                         // conID
		"AAPL",                    // symbol
		"STK",                     // secType
		"",                        // expiry
		"",                        // strike placeholder
		"",                        // right
		"",                        // multiplier
		"SMART",                   // exchange
		"",                        // primary exchange blank for equities
		"USD",                     // currency
		"AAPL",                    // local symbol
		"NMS",                     // trading class
		false,                     // delta neutral (present but false)
		"100,101,104,165,221,233", // generic ticks
		0,                         // snapshot flag
		0,                         // regulatory snapshot flag
		"",                        // link options
	}

	return fields
}

func reqContractDataFields() []interface{} {
	return []interface{}{
		9, // reqContractData
		8, // version
		7001,
		0, // contractId (unused when specifying details)
		"AAPL",
		"STK",
		"",      // expiry
		0.0,     // strike
		"",      // right
		"",      // multiplier
		"SMART", // exchange fallback
		"",      // primary exchange
		"USD",
		"AAPL",
		"NMS",
		0,  // includeExpired false
		"", // secIdType
		"", // secId
	}
}

func reqHistoricalDataFields() []interface{} {
	end := time.Date(2024, time.January, 5, 0, 0, 0, 0, time.UTC).Format("20060102-150405")

	return []interface{}{
		20,   // reqHistoricalData
		9001, // reqID
		0,    // conID (included for server >= minServerVerTradingClass)
		"AAPL",
		"STK",
		"",
		0.0,
		"",
		"", // multiplier string
		"SMART",
		"", // primary exchange
		"USD",
		"AAPL",
		"NMS",
		false,    // includeExpired
		end,      // endDateTime
		"1 day",  // barSize (placeholder)
		"2 D",    // duration
		true,     // useRTH
		"TRADES", // whatToShow
		1,        // formatDate
		false,    // keepUpToDate
		"",       // chart options
	}
}
