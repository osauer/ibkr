//go:build !trading

package daemon

import (
	"context"

	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) proposalPlaceOrder(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, ErrTradingDisabled
}
