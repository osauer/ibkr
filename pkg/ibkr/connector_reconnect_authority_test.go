package ibkr

import (
	"context"
	"testing"
	"time"
)

func TestReusedConnectionInvalidatesUnstampedObservationAuthority(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)
	conn := connector.conn
	epochA := conn.BrokerSessionEpoch()
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture complete session A")
	}

	conn.portfolioProjectionMu.Lock()
	conn.positionsMu.Lock()
	conn.positions["A"] = &RawPosition{Contract: Contract{ConID: 1, Symbol: "A", SecType: "STK"}, Position: 10, Account: "DU-A"}
	conn.positionsMu.Unlock()
	conn.portfolioHealthMu.Lock()
	conn.portfolioHealth = PortfolioStreamHealth{
		Account: "DU-A", RequestedAt: time.Now().Add(-time.Minute), InitialCompletedAt: time.Now(),
		LastUpdateAt: time.Now(), ProjectionGeneration: 7,
	}
	conn.portfolioHealthMu.Unlock()
	conn.portfolioProjectionMu.Unlock()
	conn.accountMu.Lock()
	conn.account = "DU-A"
	conn.accountSummary["NetLiquidation"] = "100000"
	conn.accountMu.Unlock()
	conn.competingMu.Lock()
	conn.competingLiveSession = true
	conn.competingMu.Unlock()
	if err := conn.acquireMarketDataSlot(context.Background(), 91); err != nil {
		t.Fatalf("seed market-data slot: %v", err)
	}

	connector.subMu.Lock()
	connector.subscriptions["A"] = &Subscription{Symbol: "A", ReqID: 91, Bid: 99, Ask: 101, Observed: true}
	connector.reqIDMap[91] = "A"
	connector.subMu.Unlock()
	connector.contractMu.Lock()
	connector.contractCache["A"] = ContractDetailsLite{ConID: 1, Symbol: "A", SecType: "STK"}
	connector.contractMu.Unlock()
	connector.dataFarmMu.Lock()
	connector.dataFarms = make(map[string]DataFarmStatus)
	connector.dataFarms["market\x00a"] = DataFarmStatus{Name: "a", Type: "market", Status: "ok"}
	connector.dataFarmMu.Unlock()
	connector.absenceMu.Lock()
	connector.mktDataAbsent = map[string]marketDataAbsence{"A": {code: 354, at: time.Now()}}
	connector.absenceMu.Unlock()
	connector.inactiveMu.Lock()
	connector.inactiveSymbols = map[string]inactiveSymbolState{"A": {reason: "test", markedAt: time.Now()}}
	connector.inactiveMu.Unlock()

	conn.resetOrderIDReadiness()
	if conn.BrokerSessionEpoch() == epochA {
		t.Fatal("reconnect did not advance socket epoch")
	}
	connector.onConnectionEstablished(conn)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture successor session B")
	}
	if connector.SessionCurrent(bindingA) || bindingB.epoch == bindingA.epoch {
		t.Fatalf("session A remained current after rollover: A=%d B=%d", bindingA.epoch, bindingB.epoch)
	}

	projection, ok := connector.CapturePortfolioProjectionForSession(bindingB)
	if !ok {
		t.Fatal("capture successor portfolio projection")
	}
	if len(projection.Positions) != 0 || !projection.Health.InitialCompletedAt.IsZero() || projection.Health.Account != "" {
		t.Fatalf("successor inherited completed A portfolio: %+v", projection)
	}
	if projection.Generation <= 7 {
		t.Fatalf("successor projection generation=%d, want invalidation after 7", projection.Generation)
	}
	if conn.GetAccountCode() != "" || len(conn.GetAccountSummary()) != 0 || conn.HasCompetingLiveSession() {
		t.Fatalf("successor inherited account/session observations: account=%q summary=%+v competing=%t",
			conn.GetAccountCode(), conn.GetAccountSummary(), conn.HasCompetingLiveSession())
	}
	if got := connector.MarketDataSnapshot(); len(got) != 0 {
		t.Fatalf("successor inherited quote subscriptions: %+v", got)
	}
	if connector.cachedContractDetail("A") != nil || len(connector.DataFarmStatuses()) != 0 || connector.marketDataAbsenceFor("A") != nil || connector.IsSymbolInactive("A") {
		t.Fatal("successor inherited unstamped contract/farm/absence/inactive cache")
	}
	if got := marketDataSlotCount(conn); got != 0 {
		t.Fatalf("successor inherited market-data slot count=%d", got)
	}
}
