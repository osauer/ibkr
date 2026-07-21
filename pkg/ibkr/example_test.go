package ibkr

import "fmt"

func ExampleFxPair() {
	base, quote, ok := FxPair(" usd/jpy ")
	fmt.Println(base, quote, ok)
	// Output: USD JPY true
}

func ExampleMarketDataKeyForContract() {
	key := MarketDataKeyForContract(Contract{
		Symbol:   "spy",
		SecType:  "STK",
		Exchange: "SMART",
		Currency: "USD",
	})
	fmt.Println(key)
	// Output: SPY|STK|SMART||USD||
}

func ExampleValidateOrder() {
	order := &IBKROrder{
		Symbol:    "SPY",
		SecType:   "STK",
		Exchange:  "SMART",
		Currency:  "USD",
		Account:   "DU123456",
		Action:    "BUY",
		TotalQty:  1,
		OrderType: "LMT",
		LmtPrice:  500,
	}

	// ValidateOrder is local shape validation, not a broker preview and not
	// submit authority.
	err := ValidateOrder(order)
	fmt.Println(err, order.TIF)
	// Output: <nil> DAY
}
