package ibkr

import (
	"errors"
	"testing"
)

func TestOrderMethodsDisabledByDefault(t *testing.T) {
	if tradingEnabled {
		t.Skip("default disabled guard is not active in trading-tag builds")
	}

	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	if err := conn.PlaceOrder(&IBKROrder{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD", Action: "BUY", TotalQty: 1, OrderType: "MKT", TIF: "DAY"}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connection.PlaceOrder err = %v, want ErrTradingDisabled", err)
	}
	if err := conn.CancelOrder(1); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connection.CancelOrder err = %v, want ErrTradingDisabled", err)
	}

	c := NewConnector(&ConnectorConfig{BaseConfig: DefaultConfig()})
	if err := c.SubmitOrder(&Contract{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"}, &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "MKT", TIF: "DAY"}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connector.SubmitOrder err = %v, want ErrTradingDisabled", err)
	}
	if err := c.CancelOrder(1); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connector.CancelOrder err = %v, want ErrTradingDisabled", err)
	}
}
