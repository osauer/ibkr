package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

// alertShadowObservationEvery is an engineering heartbeat, not a market or
// delivery threshold. It keeps record-only producer coverage inside the
// shortest one-minute silence horizon even when no UI or CLI client is open.
const alertShadowObservationEvery = 30 * time.Second

func (s *Server) startAlertShadowObservationLoops(ctx context.Context) {
	if s == nil || s.alertShadow == nil || ctx == nil {
		return
	}
	s.alertShadowLoopWG.Add(3)
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowRegimeLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowProtectionLoop(ctx)
	}()
	go func() {
		defer s.alertShadowLoopWG.Done()
		s.runAlertShadowDataHealthLoop(ctx)
	}()
}

func (s *Server) runAlertShadowRegimeLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		observeCtx, cancel := context.WithTimeout(ctx, regimeSnapshotRefreshTimeout)
		_, err := s.handleRegimeSnapshot(observeCtx, &rpc.Request{})
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Debugf("alert shadow: Regime heartbeat unavailable: %v", err)
		}
	}
}

func (s *Server) runAlertShadowProtectionLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s.observeProtectionAlertShadowHeartbeat(ctx)
	}
}

func (s *Server) runAlertShadowDataHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(alertShadowObservationEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_ = s.handleStatusHealth()
	}
}

// observeProtectionAlertShadowHeartbeat rebuilds only the protection facts
// needed by the shadow producer. It reads the portfolio cache and SQLite order
// journal; it never requests quotes, Greeks, account summaries, or broker
// writes. The canonical cache read may exercise the connector's existing,
// throttled account-updates re-subscription when a held account has no cached
// rows. That is a read-side stream repair, not an order or account mutation.
func (s *Server) observeProtectionAlertShadowHeartbeat(ctx context.Context) {
	if s == nil || s.alertShadow == nil {
		return
	}
	now := s.orderNow().UTC()
	s.mu.Lock()
	ep, c := s.endpoint, s.connector
	s.mu.Unlock()
	configuredAccount := ""
	port := ep.Port
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
	}
	connectedAccount := ""
	if c != nil {
		connectedAccount = c.AccountID()
	}
	scope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount)
	shadowScope, err := newAlertShadowBrokerScope(scope)
	if err != nil {
		return
	}
	input := alertShadowProtectionInput{
		AsOf: now, Status: orderIntegrityHealthUnavailable, Scope: shadowScope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: rpc.ProtectionCoverageStateUnknown},
	}
	if c == nil || !c.IsReady() {
		s.observeProtectionAlertShadow(ctx, input)
		return
	}
	rawPositions, health, positionsErr := c.CachedPositionsWithHealth()
	input.EvidenceAsOf = health.InitialCompletedAt.UTC()
	if health.LastUpdateAt.After(input.EvidenceAsOf) {
		input.EvidenceAsOf = health.LastUpdateAt.UTC()
	}
	input.Status = classifyOrderIntegrityPortfolioHealth(scope, health, now)
	if positionsErr != nil {
		s.observeProtectionAlertShadow(ctx, input)
		return
	}
	positions := protectionHeartbeatPositions(rawPositions, now)
	views, _, orderErr := s.loadOrderViews()
	if orderErr != nil {
		input.Summary = *buildProtectionCoverage(positions, nil, false, "order journal unavailable", now)
	} else {
		orders := make([]rpc.OrderView, 0, len(views))
		for _, view := range views {
			if orderViewMatchesBrokerScope(view, scope) && protectionCoverageOrderIsStockLike(view) {
				orders = append(orders, view)
			}
		}
		if protectionHeartbeatIdentityAmbiguous(positions, orders) {
			input.Status = orderIntegrityHealthUnavailable
			input.Summary = rpc.ProtectionCoverageSummary{
				AsOf: now, Status: rpc.ProtectionCoverageStateUnknown,
				WarningCodes: []string{"contract_identity_ambiguous"},
			}
			s.observeProtectionAlertShadow(ctx, input)
			return
		}
		reconcileFlatPositionProtectiveOrders(orders, positions, now)
		input.Summary = *buildProtectionCoverage(positions, orders, true, "", now)
	}
	s.observeProtectionAlertShadow(ctx, input)
}

func protectionHeartbeatPositions(raw []*ibkrlib.RawPosition, asOf time.Time) *rpc.PositionsResult {
	result := &rpc.PositionsResult{AsOf: asOf.UTC(), Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{}}
	for _, position := range raw {
		if position == nil || position.Position == 0 {
			continue
		}
		view := rpc.PositionView{
			Symbol: strings.ToUpper(strings.TrimSpace(position.Contract.Symbol)), SecType: positionSecType(position.Contract.SecType),
			ConID: position.Contract.ConID, Exchange: position.Contract.Exchange, Currency: position.Contract.Currency,
			LocalSymbol: position.Contract.LocalSymbol, TradingClass: position.Contract.TradingClass, Quantity: position.Position,
		}
		if protectionCoveragePositionIsStockLike(view) {
			result.Stocks = append(result.Stocks, view)
		}
	}
	return result
}

// protectionHeartbeatIdentityAmbiguous rejects every relevant match that would
// fall back to a symbol because either side lacks a contract ID. Distinct
// positive ConIDs remain safe because the coverage matcher uses exact contract
// identity first.
func protectionHeartbeatIdentityAmbiguous(positions *rpc.PositionsResult, orders []rpc.OrderView) bool {
	if positions == nil {
		return true
	}
	bySymbol := make(map[string][]rpc.PositionView)
	for _, position := range positions.Stocks {
		if !protectionCoveragePositionIsStockLike(position) || position.Quantity == 0 {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(position.Symbol))
		if symbol == "" {
			return true
		}
		bySymbol[symbol] = append(bySymbol[symbol], position)
	}
	for symbol, sameSymbol := range bySymbol {
		seenConIDs := make(map[int]struct{}, len(sameSymbol))
		for _, position := range sameSymbol {
			if position.ConID > 0 {
				if _, duplicate := seenConIDs[position.ConID]; duplicate {
					return true
				}
				seenConIDs[position.ConID] = struct{}{}
			} else if len(sameSymbol) > 1 {
				return true
			}
		}
		for _, order := range orders {
			if !protectionCoverageOrderIsStopProtective(order) {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(order.Symbol), symbol) {
				continue
			}
			if order.ConID <= 0 {
				return true
			}
			for _, position := range sameSymbol {
				if position.ConID <= 0 {
					return true
				}
			}
		}
	}
	return false
}
