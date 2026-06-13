package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/ibkr/internal/cli"
	"github.com/osauer/ibkr/internal/dial"
	"github.com/osauer/ibkr/internal/rpc"
)

type Client interface {
	Status(context.Context) (*rpc.HealthResult, error)
	MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error)
	MarketCalendarFor(context.Context, string) (*rpc.MarketCalendarResult, error)
	Account(context.Context) (*rpc.AccountResult, error)
	Positions(context.Context) (*rpc.PositionsResult, error)
	Quote(context.Context, rpc.ContractParams) (*rpc.Quote, error)
	StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error
	MarketEvents(context.Context, rpc.MarketEventsParams) (*rpc.MarketEventsResult, error)
	Canary(context.Context) (*rpc.CanaryResult, error)
	CanaryWithRegime(context.Context) (*rpc.CanaryResult, *rpc.RegimeMonitorResult, error)
	TradingStatus(context.Context) (*rpc.TradingStatus, error)
	AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error)
	OpportunitiesStatus(context.Context) (*rpc.OpportunityStatus, error)
	OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error)
	OpportunitiesRefresh(context.Context, rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error)
	OpportunitiesPreviewExercise(context.Context, rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error)
	OpportunitiesSubmitExercise(context.Context, rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error)
	OpportunitiesIgnore(context.Context, rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error)
	TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error)
	TradeProposalsRefresh(context.Context, rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error)
	TradeProposalsPreview(context.Context, rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error)
	TradeProposalsSubmit(context.Context, rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error)
	TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error)
	Settings(context.Context) (*rpc.PlatformSettings, error)
	UpdateSettings(context.Context, json.RawMessage) (*rpc.PlatformSettings, error)
	OrderPreview(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error)
	OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error)
	OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error)
	OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error)
	OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error)
	OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error)
	PurgeStatus(context.Context, rpc.PurgeStatusParams) (*rpc.PurgeStatusResult, error)
	PurgeExecute(context.Context, rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error)
	PurgeRestorePreview(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error)
	PurgeRestoreExecute(context.Context, rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error)
}

type Real struct {
	SocketPath string
	AutoSpawn  bool
}

const appQuoteSnapshotTimeout = 2500 * time.Millisecond

func (c Real) Status(ctx context.Context) (*rpc.HealthResult, error) {
	var out rpc.HealthResult
	if err := c.call(ctx, rpc.MethodStatusHealth, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) MarketCalendar(ctx context.Context) (*rpc.MarketCalendarResult, error) {
	return c.MarketCalendarFor(ctx, "us")
}

func (c Real) MarketCalendarFor(ctx context.Context, market string) (*rpc.MarketCalendarResult, error) {
	var out rpc.MarketCalendarResult
	params := rpc.MarketCalendarParams{Market: market, At: time.Now().UTC(), Days: 3}
	if err := c.call(ctx, rpc.MethodMarketCalendar, params, &out); err != nil {
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

func (c Real) Quote(ctx context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	var out rpc.Quote
	params := rpc.QuoteSnapshotParams{
		Contract:  contract,
		TimeoutMs: int(appQuoteSnapshotTimeout.Milliseconds()),
	}
	if err := c.call(ctx, rpc.MethodQuoteSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) StreamQuote(ctx context.Context, contract rpc.ContractParams, onFrame func(rpc.Frame) error) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	params := rpc.QuoteSubscribeParams{Contract: contract}
	return conn.Stream(ctx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
		var frame rpc.Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			return err
		}
		if onFrame != nil {
			return onFrame(frame)
		}
		return nil
	})
}

func (c Real) MarketEvents(ctx context.Context, params rpc.MarketEventsParams) (*rpc.MarketEventsResult, error) {
	var out rpc.MarketEventsResult
	if err := c.call(ctx, rpc.MethodMarketEventsSnapshot, params, &out); err != nil {
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

func (c Real) CanaryWithRegime(ctx context.Context) (*rpc.CanaryResult, *rpc.RegimeMonitorResult, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	canary, _, regime, err := cli.FetchCanarySnapshotWithRegime(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	monitor := rpc.CompactRegimeMonitor(&regime)
	return &canary, &monitor, nil
}

func (c Real) TradingStatus(ctx context.Context) (*rpc.TradingStatus, error) {
	var out rpc.TradingStatus
	if err := c.call(ctx, rpc.MethodTradingStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) AutoTradeStatus(ctx context.Context) (*rpc.AutoTradeStatus, error) {
	var out rpc.AutoTradeStatus
	if err := c.call(ctx, rpc.MethodAutoTradeStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesStatus(ctx context.Context) (*rpc.OpportunityStatus, error) {
	var out rpc.OpportunityStatus
	if err := c.call(ctx, rpc.MethodOpportunitiesStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesSnapshot(ctx context.Context, params rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := c.call(ctx, rpc.MethodOpportunitiesSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesRefresh(ctx context.Context, params rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	var out rpc.OpportunitySnapshot
	if err := c.call(ctx, rpc.MethodOpportunitiesRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesPreviewExercise(ctx context.Context, params rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	var out rpc.OpportunityExercisePreviewResult
	if err := c.call(ctx, rpc.MethodOpportunitiesPreviewExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesSubmitExercise(ctx context.Context, params rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	var out rpc.OpportunityExerciseSubmitResult
	if err := c.call(ctx, rpc.MethodOpportunitiesSubmitExercise, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OpportunitiesIgnore(ctx context.Context, params rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	var out rpc.OpportunityIgnoreResult
	if err := c.call(ctx, rpc.MethodOpportunitiesIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) TradeProposalsSnapshot(ctx context.Context, params rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := c.call(ctx, rpc.MethodTradeProposalsSnapshot, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) TradeProposalsRefresh(ctx context.Context, params rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	var out rpc.TradeProposalSnapshot
	if err := c.call(ctx, rpc.MethodTradeProposalsRefresh, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) TradeProposalsPreview(ctx context.Context, params rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	var out rpc.TradeProposalPreviewResult
	if err := c.call(ctx, rpc.MethodTradeProposalsPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) TradeProposalsSubmit(ctx context.Context, params rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	var out rpc.TradeProposalSubmitResult
	if err := c.call(ctx, rpc.MethodTradeProposalsSubmit, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) TradeProposalsIgnore(ctx context.Context, params rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	var out rpc.TradeProposalIgnoreResult
	if err := c.call(ctx, rpc.MethodTradeProposalsIgnore, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) Settings(ctx context.Context) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := c.call(ctx, rpc.MethodSettingsGet, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) UpdateSettings(ctx context.Context, patch json.RawMessage) (*rpc.PlatformSettings, error) {
	var out rpc.PlatformSettings
	if err := c.call(ctx, rpc.MethodSettingsUpdate, patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrderPreview(ctx context.Context, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	var out rpc.OrderPreviewResult
	if err := c.call(ctx, rpc.MethodOrderPreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrderPlace(ctx context.Context, params rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	var out rpc.OrderPlaceResult
	if err := c.call(ctx, rpc.MethodOrderPlace, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrderModify(ctx context.Context, params rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	var out rpc.OrderModifyResult
	if err := c.call(ctx, rpc.MethodOrderModify, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrderCancel(ctx context.Context, params rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	var out rpc.OrderCancelResult
	if err := c.call(ctx, rpc.MethodOrderCancel, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrdersOpen(ctx context.Context, params rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	var out rpc.OrdersOpenResult
	if err := c.call(ctx, rpc.MethodOrdersOpen, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) OrderStatus(ctx context.Context, params rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	var out rpc.OrderStatusResult
	if err := c.call(ctx, rpc.MethodOrderStatus, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) PurgeStatus(ctx context.Context, params rpc.PurgeStatusParams) (*rpc.PurgeStatusResult, error) {
	var out rpc.PurgeStatusResult
	if err := c.call(ctx, rpc.MethodPurgeStatus, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) PurgeExecute(ctx context.Context, params rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error) {
	var out rpc.PurgeExecuteResult
	if err := c.call(ctx, rpc.MethodPurgeExecute, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) PurgeRestorePreview(ctx context.Context, params rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	var out rpc.PurgeRestoreResult
	if err := c.call(ctx, rpc.MethodPurgeRestorePreview, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c Real) PurgeRestoreExecute(ctx context.Context, params rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	var out rpc.PurgeRestoreResult
	if err := c.call(ctx, rpc.MethodPurgeRestoreExecute, params, &out); err != nil {
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
