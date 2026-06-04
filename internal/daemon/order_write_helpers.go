package daemon

import (
	"fmt"
	"math"
	"strings"

	"github.com/osauer/ibkr/internal/rpc"
)

func (s *Server) openOrderViewForWrite(id string, status rpc.TradingStatus) (rpc.OrderView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return rpc.OrderView{}, errBadRequest("order id is required")
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return rpc.OrderView{}, err
	}
	for _, view := range views {
		if !orderViewMatchesID(view, id) {
			continue
		}
		if !view.Open {
			return rpc.OrderView{}, errBadRequest("order is not open")
		}
		if err := validateOrderViewMatchesGate(view, status); err != nil {
			return rpc.OrderView{}, err
		}
		return view, nil
	}
	return rpc.OrderView{}, errBadRequest("order not found")
}

func validateOrderViewMatchesGate(view rpc.OrderView, status rpc.TradingStatus) error {
	if !strings.EqualFold(view.Mode, status.Mode) {
		return fmt.Errorf("%w: order mode %s does not match current mode %s", ErrTradingDisabled, view.Mode, status.Mode)
	}
	if !strings.EqualFold(view.Account, status.Account) {
		return fmt.Errorf("%w: order account %s does not match current account %s", ErrTradingDisabled, view.Account, status.Account)
	}
	if view.Endpoint != status.Endpoint {
		return fmt.Errorf("%w: order endpoint %s does not match current endpoint %s", ErrTradingDisabled, view.Endpoint, status.Endpoint)
	}
	if view.ClientID != status.ClientID {
		return fmt.Errorf("%w: order client ID %d does not match current client ID %d", ErrTradingDisabled, view.ClientID, status.ClientID)
	}
	return nil
}

func validateModifyDraft(view rpc.OrderView, draft rpc.OrderDraft) error {
	if strings.ToUpper(strings.TrimSpace(view.SecType)) != "STK" || strings.ToUpper(strings.TrimSpace(draft.Contract.SecType)) != "STK" {
		return errBadRequest("order modify supports STK contracts only")
	}
	if !strings.EqualFold(view.Symbol, draft.Contract.Symbol) {
		return errBadRequest("order modify cannot change symbol")
	}
	if !strings.EqualFold(view.Action, draft.Action) {
		return errBadRequest("order modify cannot change action")
	}
	if !strings.EqualFold(view.OrderType, rpc.OrderTypeLMT) || !strings.EqualFold(draft.OrderType, rpc.OrderTypeLMT) {
		return errBadRequest("order modify supports LMT orders only")
	}
	if !strings.EqualFold(view.TIF, rpc.OrderTIFDay) || !strings.EqualFold(draft.TIF, rpc.OrderTIFDay) {
		return errBadRequest("order modify supports DAY time-in-force only")
	}
	if draft.OutsideRTH != view.OutsideRTH {
		return errBadRequest("order modify cannot change outside_rth")
	}
	if draft.LimitPrice <= 0 || math.IsNaN(draft.LimitPrice) || math.IsInf(draft.LimitPrice, 0) {
		return errBadRequest("order modify requires a positive limit price")
	}
	maxQty := view.Remaining
	if maxQty <= 0 {
		maxQty = view.Quantity - view.Filled
	}
	if maxQty <= 0 {
		maxQty = view.Quantity
	}
	if draft.Quantity <= 0 || float64(draft.Quantity) > maxQty {
		return errBadRequest(fmt.Sprintf("order modify quantity must be positive and no more than remaining %.4g", maxQty))
	}
	if err := validateModifyContractRouting(view, draft.Contract); err != nil {
		return err
	}
	return nil
}

func modifyContractForView(view rpc.OrderView, contract rpc.ContractParams) rpc.ContractParams {
	contract.Symbol = view.Symbol
	contract.SecType = view.SecType
	contract.Exchange = view.Exchange
	contract.PrimaryExch = view.PrimaryExch
	contract.Currency = view.Currency
	contract.LocalSymbol = view.LocalSymbol
	contract.TradingClass = view.TradingClass
	return contract
}

func validateModifyContractRouting(view rpc.OrderView, contract rpc.ContractParams) error {
	if view.Exchange != "" && contract.Exchange != "" && !strings.EqualFold(view.Exchange, contract.Exchange) {
		return errBadRequest("order modify cannot change exchange")
	}
	if view.PrimaryExch != "" && contract.PrimaryExch != "" && !strings.EqualFold(view.PrimaryExch, contract.PrimaryExch) {
		return errBadRequest("order modify cannot change primary exchange")
	}
	if view.Currency != "" && contract.Currency != "" && !strings.EqualFold(view.Currency, contract.Currency) {
		return errBadRequest("order modify cannot change currency")
	}
	if view.LocalSymbol != "" && contract.LocalSymbol != "" && !strings.EqualFold(view.LocalSymbol, contract.LocalSymbol) {
		return errBadRequest("order modify cannot change local symbol")
	}
	if view.TradingClass != "" && contract.TradingClass != "" && !strings.EqualFold(view.TradingClass, contract.TradingClass) {
		return errBadRequest("order modify cannot change trading class")
	}
	return nil
}
