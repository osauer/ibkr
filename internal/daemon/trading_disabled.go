//go:build !trading

package daemon

import (
	"context"
	"fmt"

	"github.com/osauer/ibkr/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/pkg/ibkr"
)

const orderWritesAvailable = false

func (s *Server) handleOrderPlace(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) handleOrderModify(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) handleOrderCancel(_ context.Context, _ *rpc.Request) (any, error) {
	return nil, ErrTradingDisabled
}

func (s *Server) reserveBrokerOrderID(ctx context.Context) (int, error) {
	if s.orderReserveBrokerID != nil {
		return s.orderReserveBrokerID(ctx)
	}
	return 0, ErrTradingDisabled
}

func (s *Server) submitConfiguredOrder(ctx context.Context, status rpc.TradingStatus, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	auth := s.brokerWriteAuthorization(status)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	if s.orderPlaceBroker != nil {
		return s.orderPlaceBroker(ctx, contract, order)
	}
	return ErrTradingDisabled
}
