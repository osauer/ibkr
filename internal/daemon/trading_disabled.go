//go:build !trading

package daemon

import (
	"context"

	"github.com/osauer/ibkr/internal/rpc"
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
