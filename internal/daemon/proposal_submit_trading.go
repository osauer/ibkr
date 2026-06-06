//go:build trading

package daemon

import (
	"context"

	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) proposalPlaceOrder(ctx context.Context, p rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return s.placeOrder(ctx, p)
}
