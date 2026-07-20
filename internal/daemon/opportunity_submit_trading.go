//go:build trading

package daemon

import (
	"context"
	"fmt"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

func (s *Server) submitOptionExercise(ctx context.Context, opp rpc.Opportunity, qty int, origin string, orderRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	auth := s.brokerWriteAuthorizationForRequest(origin)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	c := s.gatewayConnector()
	if c == nil {
		return s.gatewayUnavailableError()
	}
	if qty <= 0 || qty > opp.MaxQuantity {
		return fmt.Errorf("%w: exercise quantity must be positive and no greater than the opportunity quantity", ErrTradingDisabled)
	}
	now := s.orderNow()
	ev := orderJournalEvent{
		At:           now,
		Type:         orderJournalEventSendAttempted,
		OrderRef:     orderRef,
		ClientID:     auth.Status.ClientID,
		Account:      auth.Status.Account,
		Endpoint:     auth.Status.Endpoint,
		Mode:         auth.Status.Mode,
		Source:       "opportunity",
		Origin:       normalizedWriteOrigin(origin),
		Symbol:       opp.Contract.Symbol,
		SecType:      opp.Contract.SecType,
		ConID:        opp.Contract.ConID,
		Exchange:     opp.Contract.Exchange,
		PrimaryExch:  opp.Contract.PrimaryExch,
		Currency:     opp.Contract.Currency,
		LocalSymbol:  opp.Contract.LocalSymbol,
		TradingClass: opp.Contract.TradingClass,
		Expiry:       opp.Contract.Expiry,
		Strike:       opp.Contract.Strike,
		Right:        opp.Contract.Right,
		Multiplier:   opp.Contract.Multiplier,
		Action:       opp.Action,
		Quantity:     float64(qty),
		SendState:    orderSendStateSendAttempted,
		Message:      "option exercise instruction transmit attempted; TWS may treat exercise requests as final depending on settings",
	}
	if s.orderJournal == nil {
		return fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	if err := s.orderJournal.StagePreTransmit("", "", 0, 0, corestore.ActionExercise, coreOrderOrigin(origin), []orderJournalEvent{ev}); err != nil {
		return fmt.Errorf("%w: append exercise journal: %v", ErrTradingDisabled, err)
	}
	req := ibkrlib.OptionExerciseRequest{
		TickerID:         0,
		Contract:         previewIBKRContract(opp.Contract),
		ExerciseAction:   opp.ExerciseAction,
		ExerciseQuantity: qty,
		Account:          auth.Status.Account,
		Override:         0,
		ManualOrderTime:  "",
	}
	if err := c.ExerciseOptions(ctx, req); err != nil {
		if s.orderJournal != nil {
			sendErr := ev
			sendErr.At = s.orderNow()
			sendErr.Type = orderJournalEventSendError
			sendErr.SendState = orderSendStateUncertainSend
			sendErr.Message = "option exercise broker send returned error; reconcile in TWS before retrying: " + err.Error()
			_ = s.orderJournal.Append(sendErr)
		}
		return err
	}
	return nil
}
