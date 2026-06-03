package daemonclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
)

type Client interface {
	Status(context.Context) (*rpc.HealthResult, error)
	Account(context.Context) (*rpc.AccountResult, error)
	Positions(context.Context) (*rpc.PositionsResult, error)
	Canary(context.Context) (*rpc.CanaryResult, error)
}

type Real struct {
	SocketPath string
	AutoSpawn  bool
}

func (c Real) Status(ctx context.Context) (*rpc.HealthResult, error) {
	var out rpc.HealthResult
	if err := c.call(ctx, rpc.MethodStatusHealth, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) Account(ctx context.Context) (*rpc.AccountResult, error) {
	var out rpc.AccountResult
	if err := c.call(ctx, rpc.MethodAccountSummary, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) Positions(ctx context.Context) (*rpc.PositionsResult, error) {
	var out rpc.PositionsResult
	if err := c.call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) Canary(ctx context.Context) (*rpc.CanaryResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	out, err := cli.FetchCanary(ctx, conn)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) call(ctx context.Context, method string, params any, out any) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Call(ctx, method, params, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

func (c Real) connect(ctx context.Context) (*dial.Conn, error) {
	path := c.SocketPath
	if path == "" {
		path = dial.DefaultSocketPath()
	}
	conn, err := dial.Connect(path)
	if errors.Is(err, dial.ErrSocketMissing) && c.AutoSpawn {
		conn, err = dial.AutospawnAndConnectContext(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}
