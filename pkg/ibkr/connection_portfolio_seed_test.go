package ibkr

import (
	"bufio"
	"sync"
	"testing"
	"time"
)

// TestHandlePortfolioValueSeedsOptionContractCache verifies that a held
// OPT position arriving via msgPortfolioValue populates optionContractCache
// so the next SubscribeOption call resolves via cache-hit rather than
// paying the 5 s × N-exchange-attempts reqContractData round-trip.
//
// Regression coverage for the v0.12.1 fix: before this, only the
// connector-side stock cache (contractCache, keyed by bare symbol) was
// seeded from portfolio data, and held options had to round-trip even
// though msgPortfolioValue already carries the full Contract spec with
// ConID. Under load this blew the 30 s positions deadline before the
// Greeks tick could even be requested.
func TestHandlePortfolioValueSeedsOptionContractCache(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	// Field layout matches handlePortfolioValue's expected 20-field
	// msgPortfolioValue payload. Field 9 is *primaryExchange* per IB API
	// (a wire quirk; the parsed Contract stores it under Exchange).
	fields := []string{
		"7",            // msgID
		"8",            // version
		"747397667",    // 2: contract.conId
		"AMZN",         // 3: contract.symbol
		"OPT",          // 4: contract.secType
		"20260821",     // 5: contract.expiry
		"305",          // 6: contract.strike
		"C",            // 7: contract.right
		"100",          // 8: contract.multiplier
		"AMEX",         // 9: contract.primaryExchange ← appears in parsed Contract.Exchange
		"USD",          // 10: contract.currency
		"AMZN 260821C", // 11: contract.localSymbol
		"AMZN",         // 12: contract.tradingClass
		"5",            // 13: position
		"1.27",         // 14: marketPrice
		"635.00",       // 15: marketValue
		"127.30",       // 16: averageCost
		"50.00",        // 17: unrealizedPNL
		"0.00",         // 18: realizedPNL
		"DU1234567",    // 19: accountName
	}

	conn.handlePortfolioValue(fields)

	// The OPRA-style cache key is built by optionContractKey from the
	// parsed Contract fields. SubscribeOption uses the same key shape.
	// TradingClass="AMZN" matches the test fixture field 12.
	cacheKey := optionContractKey("AMZN", "AMZN", "20260821", 305, "C")
	conn.optionContractMu.RLock()
	detail, ok := conn.optionContractCache[cacheKey]
	conn.optionContractMu.RUnlock()
	if !ok {
		t.Fatalf("optionContractCache missing entry for %q after portfolio seed", cacheKey)
	}
	if detail.ConID != 747397667 {
		t.Errorf("ConID = %d, want 747397667", detail.ConID)
	}
	// Exchange must stay blank so SubscribeOption's "SMART" default
	// persists through applyContractDetailLite (which only overwrites on
	// non-empty cache values). PrimaryExch holds the actual listing venue.
	if detail.Exchange != "" {
		t.Errorf("Exchange = %q, want empty (so SMART default survives)", detail.Exchange)
	}
	if detail.PrimaryExch != "AMEX" {
		t.Errorf("PrimaryExch = %q, want AMEX", detail.PrimaryExch)
	}
	if detail.TradingClass != "AMZN" {
		t.Errorf("TradingClass = %q, want AMZN", detail.TradingClass)
	}
	if detail.SecType != "OPT" || detail.Expiry != "20260821" || detail.Strike != 305 || detail.Right != "C" {
		t.Errorf("persisted option identity fields = sec=%q expiry=%q strike=%v right=%q", detail.SecType, detail.Expiry, detail.Strike, detail.Right)
	}

	// Persist the exact portfolio-seeded row through the authority seam and
	// prove the strict restart decoder accepts and preserves its full identity.
	authority := &memoryContractAuthority{}
	store := NewContractStore(t.TempDir())
	if err := store.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(map[string]ContractDetailsLite{}, map[string]ContractDetailsLite{cacheKey: detail}, ""); err != nil {
		t.Fatal(err)
	}
	restarted := NewContractStore(t.TempDir())
	if err := restarted.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded[cacheKey]; got.ConID != detail.ConID || got.SecType != "OPT" || got.Expiry != "20260821" || got.Strike != 305 || got.Right != "C" {
		t.Fatalf("portfolio-seeded option after authority restart = %+v", got)
	}
}

// TestHandlePortfolioValueIgnoresStockForOptionCache verifies that stock
// positions do not pollute the option cache.
func TestHandlePortfolioValueIgnoresStockForOptionCache(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	fields := []string{
		"7", "8",
		"4391", "AMD", "STK", "", "0", "", "100",
		"NASDAQ", "USD", "AMD", "AMD",
		"100", "438.50", "43850.00", "374.01", "6448.99", "0.00", "DU1234567",
	}
	conn.handlePortfolioValue(fields)

	conn.optionContractMu.RLock()
	defer conn.optionContractMu.RUnlock()
	if len(conn.optionContractCache) != 0 {
		t.Fatalf("optionContractCache should be empty after STK position, got %d entries",
			len(conn.optionContractCache))
	}
}

func TestHandlePortfolioValueAcceptsBlankStockStrikeAndMultiplier(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	conn.resetPortfolioStreamHealth("DU1234567", now)
	conn.handlePortfolioValue([]string{
		"7", "8", "4391", "AMD", "STK", "", "", "", "",
		"NASDAQ", "USD", "AMD", "AMD",
		"100", "438.50", "43850.00", "374.01", "6448.99", "0.00", "DU1234567",
	})
	if !conn.completePortfolioDownload("DU1234567", now.Add(time.Second)) {
		t.Fatal("realistic stock generation with blank strike/multiplier was rejected")
	}
	rows, health := conn.GetPositionsWithPortfolioHealth()
	row := rows[cachedPositionKey("DU1234567", Contract{ConID: 4391, SecType: "STK"})]
	if row == nil || row.Contract.Multiplier != 1 || !health.InvalidPayloadAt.IsZero() {
		t.Fatalf("blank stock identity normalized incorrectly: row=%+v health=%+v", row, health)
	}
}

func TestHandlePortfolioValueRejectsBlankFuturesMultiplier(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	conn.resetPortfolioStreamHealth("DU1234567", now)
	conn.handlePortfolioValue([]string{
		"7", "8", "551122", "ES", "FUT", "20260918", "", "", "",
		"CME", "USD", "ESU6", "ES",
		"-1", "6500", "-325000", "6400", "-5000", "0", "DU1234567",
	})
	if conn.completePortfolioDownload("DU1234567", now.Add(time.Second)) {
		t.Fatal("future with a blank multiplier was published using fabricated multiplier 1")
	}
	rows, health := conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 0 || health.InvalidPayloadAt.IsZero() {
		t.Fatalf("blank futures multiplier rows=%+v health=%+v, want typed invalid generation", rows, health)
	}
}

func TestHandlePortfolioValueZeroQuantityDeletesCachedPosition(t *testing.T) {
	conn := &Connection{
		positions:           map[string]*RawPosition{},
		positionsMu:         sync.RWMutex{},
		optionContractCache: map[string]ContractDetailsLite{},
		optionContractMu:    sync.RWMutex{},
	}

	open := []string{
		"7", "8",
		"999001", "GME", "OPT", "20260618", "30", "C", "100",
		"AMEX", "USD", "GME 260618C30", "GME",
		"1", "0.14", "14.00", "0.00", "0.00", "-3899.40", "DU1234567",
	}
	closed := append([]string{}, open...)
	closed[13] = "0"
	closed[15] = "0.00"

	conn.handlePortfolioValue(open)
	if got := len(conn.GetPositions()); got != 1 {
		t.Fatalf("positions after open update = %d, want 1", got)
	}
	conn.handlePortfolioValue(closed)
	if got := len(conn.GetPositions()); got != 0 {
		t.Fatalf("positions after zero-quantity update = %d, want 0: %+v", got, conn.GetPositions())
	}
}

func TestHandlePortfolioValueForeignAccountCannotMutateBoundCache(t *testing.T) {
	baseFields := []string{
		"7", "8",
		"999001", "GME", "OPT", "20260618", "30", "C", "100",
		"AMEX", "USD", "GME 260618C30", "GME",
		"1", "0.14", "14.00", "0.00", "0.00", "-3899.40", "DU999",
	}
	cacheKey := optionContractKey("GME", "GME", "20260618", 30, "C")

	for _, test := range []struct {
		name     string
		position string
		conID    string
	}{
		{name: "foreign nonzero cannot overwrite or seed", position: "5", conID: "999999"},
		{name: "foreign zero cannot delete", position: "0", conID: "999001"},
		{name: "blank account cannot delete", position: "0", conID: "999001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			current := &RawPosition{
				Account: "DU123", Position: 1,
				Contract: Contract{ConID: 999001, Symbol: "GME", SecType: "OPT", Expiry: "20260618", Strike: 30, Right: "C", TradingClass: "GME"},
			}
			currentDetail := ContractDetailsLite{ConID: 999001, Symbol: "GME", SecType: "OPT", Expiry: "20260618", Strike: 30, Right: "C"}
			conn := &Connection{
				positions:           map[string]*RawPosition{"GME_20260618_C30": current},
				optionContractCache: map[string]ContractDetailsLite{cacheKey: currentDetail},
				portfolioHealth: PortfolioStreamHealth{
					Account: "DU123", RequestedAt: now.Add(-time.Minute), InitialCompletedAt: now.Add(-30 * time.Second), LastUpdateAt: now.Add(-time.Second),
				},
			}
			fields := append([]string(nil), baseFields...)
			fields[2], fields[13] = test.conID, test.position
			if test.name == "blank account cannot delete" {
				fields[19] = ""
			}
			conn.handlePortfolioValue(fields)

			positions, health := conn.GetPositionsWithPortfolioHealth()
			got := positions["GME_20260618_C30"]
			if got == nil || got.Account != "DU123" || got.Position != 1 || got.Contract.ConID != 999001 {
				t.Fatalf("bound position mutated by foreign frame: %+v", got)
			}
			if health.Account != "DU123" || !health.RequestedAt.IsZero() || !health.InitialCompletedAt.IsZero() || !health.LastUpdateAt.IsZero() || health.ScopeConflictAt.IsZero() {
				t.Fatalf("scope-conflict health = %+v", health)
			}
			if gotDetail := conn.optionContractCache[cacheKey]; gotDetail.ConID != currentDetail.ConID {
				t.Fatalf("foreign frame changed option cache: %+v", gotDetail)
			}

			// The conflict stays latched even when a later matching frame arrives.
			matchingZero := append([]string(nil), baseFields...)
			matchingZero[13], matchingZero[19] = "0", "DU123"
			conn.handlePortfolioValue(matchingZero)
			if got := conn.GetPositions()["GME_20260618_C30"]; got == nil {
				t.Fatal("matching frame mutated cache before subscription reset")
			}

			conn.resetPortfolioStreamHealth("DU123", now)
			conn.handlePortfolioValue(matchingZero)
			if !conn.completePortfolioDownload("DU123", now.Add(time.Second)) {
				t.Fatal("matching generation did not complete")
			}
			if got := conn.GetPositions()["GME_20260618_C30"]; got != nil {
				t.Fatalf("matching zero after reset did not delete position: %+v", got)
			}
		})
	}
}

func TestHandlePositionZeroQuantityDeletesCachedPosition(t *testing.T) {
	conn := &Connection{
		positions:               map[string]*RawPosition{},
		positionsSnapshot:       map[string]*RawPosition{},
		positionsSnapshotActive: true,
		positionsMu:             sync.RWMutex{},
	}

	open := []string{
		"61", "3", "DU1234567", "265598", "AAPL", "STK", "1",
		"NASDAQ", "USD", "AAPL", "AAPL", "100", "195.00",
	}
	closed := append([]string{}, open...)
	closed[11] = "0"

	conn.handlePosition(open)
	if got := len(conn.positionsSnapshot); got != 1 {
		t.Fatalf("positions after open update = %d, want 1", got)
	}
	conn.handlePosition(closed)
	if got := len(conn.positionsSnapshot); got != 0 {
		t.Fatalf("positions after zero-quantity update = %d, want 0: %+v", got, conn.positionsSnapshot)
	}
}

func TestHandlePositionPreservesDuplicateStockConIDsPerAccount(t *testing.T) {
	conn := &Connection{positions: map[string]*RawPosition{}, positionsSnapshot: map[string]*RawPosition{}, positionsSnapshotActive: true}
	first := []string{
		"61", "3", "DU1234567", "265598", "DUP", "STK", "1",
		"SMART", "USD", "DUP", "DUP", "-10", "25.00",
	}
	second := append([]string(nil), first...)
	second[3], second[9], second[11] = "265599", "DUP.A", "-20"

	conn.handlePosition(first)
	conn.handlePosition(second)
	positions := conn.positionsSnapshot
	if len(positions) != 2 {
		t.Fatalf("duplicate-symbol exact stock rows = %d, want 2: %+v", len(positions), positions)
	}
	if positions[cachedPositionKey("DU1234567", Contract{ConID: 265598, SecType: "STK"})] == nil ||
		positions[cachedPositionKey("DU1234567", Contract{ConID: 265599, SecType: "STK"})] == nil {
		t.Fatalf("exact ConID keys missing: %+v", positions)
	}

	closed := append([]string(nil), first...)
	closed[11] = "0"
	conn.handlePosition(closed)
	positions = conn.positionsSnapshot
	if len(positions) != 1 || positions[cachedPositionKey("DU1234567", Contract{ConID: 265599, SecType: "STK"})] == nil {
		t.Fatalf("zero delete removed wrong exact stock row: %+v", positions)
	}
}

func TestPortfolioGenerationPreservesSameSymbolFutureContracts(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	conn.resetPortfolioStreamHealth("DU1234567", now)
	first := []string{
		"7", "8", "700001", "ES", "FUT", "20260918", "", "", "50",
		"CME", "USD", "ESU6", "ES", "1", "6000", "300000", "5900", "5000", "0", "DU1234567",
	}
	second := append([]string(nil), first...)
	second[2], second[5], second[11], second[13] = "700002", "20261218", "ESZ6", "2"

	conn.handlePortfolioValue(first)
	conn.handlePortfolioValue(second)
	if !conn.completePortfolioDownload("DU1234567", now.Add(time.Second)) {
		t.Fatal("valid futures generation did not publish")
	}
	positions, health := conn.GetPositionsWithPortfolioHealth()
	if len(positions) != 2 || health.InitialCompletedAt.IsZero() {
		t.Fatalf("same-symbol futures generation rows=%d health=%+v, want two complete rows", len(positions), health)
	}
	if positions[cachedPositionKey("DU1234567", Contract{ConID: 700001, SecType: "FUT"})] == nil ||
		positions[cachedPositionKey("DU1234567", Contract{ConID: 700002, SecType: "FUT"})] == nil {
		t.Fatalf("exact future ConID keys missing: %+v", positions)
	}
}

func TestHandlePositionStockKeyRetainsForeignAccountConflictEvidence(t *testing.T) {
	conn := &Connection{positions: map[string]*RawPosition{}, positionsSnapshot: map[string]*RawPosition{}, positionsSnapshotActive: true}
	current := []string{"61", "3", "DU123", "265598", "DUP", "STK", "1", "SMART", "USD", "DUP", "DUP", "-10", "25.00"}
	foreign := append([]string(nil), current...)
	foreign[2] = "DU999"

	conn.handlePosition(current)
	conn.handlePosition(foreign)
	positions := conn.positionsSnapshot
	if len(positions) != 2 || positions[cachedPositionKey("DU123", Contract{ConID: 265598, SecType: "STK"})] == nil || positions[cachedPositionKey("DU999", Contract{ConID: 265598, SecType: "STK"})] == nil {
		t.Fatalf("foreign exact row overwrote bound-account evidence: %+v", positions)
	}
}

func TestPortfolioValueKeepsPrimaryVenueWithoutInventingExactExchange(t *testing.T) {
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	portfolio := []string{
		"7", "8", "265598", "DUP", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "DUP", "DUP", "-10", "24.00", "-240.00", "25.00", "10.00", "0.00", "DU123",
	}

	conn.handlePortfolioValue(portfolio)
	rows := conn.GetPositions()
	row := rows[cachedPositionKey("DU123", Contract{ConID: 265598, SecType: "STK"})]
	if row == nil {
		t.Fatalf("merged stock row missing: %+v", rows)
	}
	if row.Contract.Exchange != "" || row.Contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("exact/primary venue identity collapsed: %+v", row.Contract)
	}
}

func TestRetiredPortfolioValueCannotContaminateSuccessorGeneration(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 31})
	t.Cleanup(conn.rateLimiter.Stop)
	oldEpoch := conn.BrokerSessionEpoch()
	entered := make(chan struct{})
	release := make(chan struct{})
	conn.messageAfterInitialEpochCheck = func(msgID int) {
		if msgID == msgPortfolioValue {
			close(entered)
			<-release
		}
	}
	defer func() { conn.messageAfterInitialEpochCheck = nil }()
	stale := []any{msgPortfolioValue, "8", "265598", "STALE", "STK", "", "0", "", "1", "NASDAQ", "USD", "STALE", "STALE", "10", "24", "240", "25", "0", "0", "DU-A"}
	done := make(chan struct{})
	go func() {
		conn.processMessageAtEpoch(conn.encodeMsg(stale...), oldEpoch)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stale portfolio value did not reach race pause")
	}
	conn.resetOrderIDReadiness()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	conn.resetPortfolioStreamHealth("DU-B", now)
	beforePositions, beforeHealth := conn.GetPositionsWithPortfolioHealth()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale portfolio value did not finish")
	}
	afterPositions, afterHealth := conn.GetPositionsWithPortfolioHealth()
	if len(beforePositions) != len(afterPositions) || len(afterPositions) != 0 || afterHealth != beforeHealth {
		t.Fatalf("stale A contaminated B authority: before=%+v/%+v after=%+v/%+v", beforePositions, beforeHealth, afterPositions, afterHealth)
	}
}

func TestRetiredPortfolioCompletionCannotCompleteSuccessorGeneration(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 31})
	t.Cleanup(conn.rateLimiter.Stop)
	oldEpoch := conn.BrokerSessionEpoch()
	entered := make(chan struct{})
	release := make(chan struct{})
	conn.messageAfterInitialEpochCheck = func(msgID int) {
		if msgID == msgAcctDownloadEnd {
			close(entered)
			<-release
		}
	}
	defer func() { conn.messageAfterInitialEpochCheck = nil }()
	done := make(chan struct{})
	go func() {
		conn.processMessageAtEpoch(conn.encodeMsg(msgAcctDownloadEnd, "1", "DU-B"), oldEpoch)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stale portfolio completion did not reach race pause")
	}
	conn.resetOrderIDReadiness()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	conn.resetPortfolioStreamHealth("DU-B", now)
	_, beforeHealth := conn.GetPositionsWithPortfolioHealth()
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale portfolio completion did not finish")
	}
	_, afterHealth := conn.GetPositionsWithPortfolioHealth()
	if !afterHealth.InitialCompletedAt.IsZero() || afterHealth != beforeHealth {
		t.Fatalf("stale A falsely completed B portfolio generation: before=%+v after=%+v", beforeHealth, afterHealth)
	}
}

func TestPortfolioProjectionGenerationTracksStructureNotMarks(t *testing.T) {
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	row := []string{
		"7", "8", "265598", "DUP", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "DUP", "DUP", "-10", "24.00", "-240.00", "25.00", "10.00", "0.00", "DU123",
	}

	conn.handlePortfolioValue(row)
	first := conn.PortfolioProjectionGeneration()
	if first == 0 {
		t.Fatal("first structural row did not advance portfolio projection generation")
	}

	markOnly := append([]string(nil), row...)
	markOnly[14], markOnly[15], markOnly[17] = "25.00", "-250.00", "0.00"
	conn.handlePortfolioValue(markOnly)
	if got := conn.PortfolioProjectionGeneration(); got != first {
		t.Fatalf("mark/PnL-only update advanced generation to %d, want %d", got, first)
	}

	identityChange := append([]string(nil), markOnly...)
	identityChange[11] = "DUP.A"
	conn.handlePortfolioValue(identityChange)
	second := conn.PortfolioProjectionGeneration()
	if second != first+1 {
		t.Fatalf("merged exact identity update generation=%d, want %d", second, first+1)
	}

	quantityChange := append([]string(nil), identityChange...)
	quantityChange[13] = "-11"
	conn.handlePortfolioValue(quantityChange)
	if got := conn.PortfolioProjectionGeneration(); got != second+1 {
		t.Fatalf("quantity update generation=%d, want %d", got, second+1)
	}
}

func TestPortfolioResubscribePublishesGenerationAtomicallyAndDeletesAbsentRows(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	row := []string{
		"7", "8", "265598", "DUP", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "DUP", "DUP", "-10", "24", "-240", "25", "10", "0", "DU123",
	}

	conn.resetPortfolioStreamHealth("DU123", now)
	conn.handlePortfolioValue(row)
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 0 || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("partial first generation leaked: rows=%+v health=%+v", published, health)
	}
	if !conn.completePortfolioDownload("DU123", now.Add(time.Second)) {
		t.Fatal("first generation did not complete")
	}
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 1 || health.InitialCompletedAt.IsZero() {
		t.Fatalf("first publication = rows=%+v health=%+v", published, health)
	}

	conn.resetPortfolioStreamHealth("DU123", now.Add(time.Minute))
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 1 || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("resubscribe should retain context with incomplete health: rows=%+v health=%+v", published, health)
	}
	// The second complete snapshot contains no row: atomic replacement must
	// remove the now-closed position instead of blessing the old map current.
	if !conn.completePortfolioDownload("DU123", now.Add(time.Minute+time.Second)) {
		t.Fatal("empty second generation did not complete")
	}
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 0 || health.InitialCompletedAt.IsZero() {
		t.Fatalf("absent row survived completed replacement: rows=%+v health=%+v", published, health)
	}
}

func TestMalformedPortfolioRowInvalidatesWholeGeneration(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	old := []string{
		"7", "8", "265598", "OLD", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "OLD", "OLD", "-10", "24", "-240", "25", "10", "0", "DU123",
	}
	conn.resetPortfolioStreamHealth("DU123", now)
	conn.handlePortfolioValue(old)
	if !conn.completePortfolioDownload("DU123", now.Add(time.Second)) {
		t.Fatal("baseline generation did not complete")
	}

	validNew := append([]string(nil), old...)
	validNew[2], validNew[3], validNew[11], validNew[12] = "265599", "NEW", "NEW", "NEW"
	malformedHeld := append([]string(nil), validNew...)
	malformedHeld[2], malformedHeld[3], malformedHeld[13] = "265600", "BROKEN", "not-a-position"
	conn.resetPortfolioStreamHealth("DU123", now.Add(time.Minute))
	conn.handlePortfolioValue(validNew)
	conn.handlePortfolioValue(malformedHeld)
	if conn.completePortfolioDownload("DU123", now.Add(time.Minute+time.Second)) {
		t.Fatal("download end blessed a generation containing a malformed held row")
	}

	rows, health := conn.GetPositionsWithPortfolioHealth()
	if health.InvalidPayloadAt.IsZero() || !health.InitialCompletedAt.IsZero() || !health.LastUpdateAt.IsZero() {
		t.Fatalf("malformed generation health=%+v, want typed unavailable receipt", health)
	}
	if len(rows) != 1 || rows[cachedPositionKey("DU123", Contract{ConID: 265598, SecType: "STK"})] == nil {
		t.Fatalf("malformed generation replaced prior context: %+v", rows)
	}
	if rows[cachedPositionKey("DU123", Contract{ConID: 265599, SecType: "STK"})] != nil {
		t.Fatalf("partial valid row leaked from rejected generation: %+v", rows)
	}
}

func TestTruncatedPortfolioRowInvalidatesWholeGeneration(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := &Connection{positions: map[string]*RawPosition{}, optionContractCache: map[string]ContractDetailsLite{}}
	valid := []string{
		"7", "8", "265598", "HELD", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "HELD", "HELD", "-10", "24", "-240", "25", "10", "0", "DU123",
	}
	conn.resetPortfolioStreamHealth("DU123", now)
	conn.handlePortfolioValue(valid)
	conn.handlePortfolioValue(valid[:13])
	if conn.completePortfolioDownload("DU123", now.Add(time.Second)) {
		t.Fatal("download end blessed a generation containing a truncated held row")
	}
	rows, health := conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 0 || health.InvalidPayloadAt.IsZero() || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("truncated generation rows=%+v health=%+v, want no publication and typed unavailable", rows, health)
	}
}

func TestRequestPositionsPartialGenerationCannotMutateStreamingProjection(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	var out safeBuffer
	conn.writer = bufio.NewWriter(&out)
	streamRow := []string{
		"7", "8", "265598", "STREAM", "STK", "", "0", "", "1",
		"NASDAQ", "USD", "STREAM", "STREAM", "10", "24", "240", "25", "-10", "0", "DU123",
	}
	conn.resetPortfolioStreamHealth("DU123", now)
	conn.handlePortfolioValue(streamRow)
	if !conn.completePortfolioDownload("DU123", now.Add(time.Second)) {
		t.Fatal("stream generation did not complete")
	}

	if err := conn.RequestPositions(); err != nil {
		t.Fatal(err)
	}
	conn.handlePosition([]string{
		"61", "3", "DU123", "999001", "ONESHOT", "STK", "1",
		"SMART", "USD", "ONESHOT", "ONESHOT", "5", "12",
	})
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 1 || published[cachedPositionKey("DU123", Contract{ConID: 265598, SecType: "STK"})] == nil || health.InitialCompletedAt.IsZero() {
		t.Fatalf("partial reqPositions poisoned stream: rows=%+v health=%+v", published, health)
	}
	conn.completePositionsSnapshot()
	if snapshot := conn.GetPositionsSnapshot(); len(snapshot) != 1 || snapshot[cachedPositionKey("DU123", Contract{ConID: 999001, SecType: "STK"})] == nil {
		t.Fatalf("completed one-shot snapshot = %+v", snapshot)
	}
	if published, health := conn.GetPositionsWithPortfolioHealth(); len(published) != 1 || published[cachedPositionKey("DU123", Contract{ConID: 265598, SecType: "STK"})] == nil || health.InitialCompletedAt.IsZero() {
		t.Fatalf("completed reqPositions replaced stream: rows=%+v health=%+v", published, health)
	}
}
