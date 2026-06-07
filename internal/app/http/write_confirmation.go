package apphttp

import (
	"context"
	"strings"

	"github.com/osauer/ibkr/internal/config"
	"github.com/osauer/ibkr/internal/rpc"
)

type BrokerWriteConfirmation struct {
	ConfirmAccount string `json:"confirm_account,omitempty"`
	ConfirmMode    string `json:"confirm_mode,omitempty"`
}

func (h *handler) requireBrokerWriteConfirmation(ctx context.Context, req BrokerWriteConfirmation) (*rpc.TradingStatus, error) {
	status, err := h.deps.Daemon.TradingStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status.Mode == config.TradingModeDisabled {
		return nil, &rpc.Error{Code: rpc.CodeTradingDisabled, Message: "trading is disabled"}
	}
	if !status.CanWrite {
		return nil, &rpc.Error{Code: rpc.CodeTradingDisabled, Message: "broker writes are not enabled by trading.status"}
	}
	if strings.TrimSpace(status.Account) == "" || strings.TrimSpace(status.Mode) == "" {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "current trading account and mode are required"}
	}
	if !strings.EqualFold(strings.TrimSpace(req.ConfirmAccount), strings.TrimSpace(status.Account)) {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "confirm_account must match current trading account"}
	}
	if !strings.EqualFold(strings.TrimSpace(req.ConfirmMode), strings.TrimSpace(status.Mode)) {
		return nil, &rpc.Error{Code: rpc.CodeBadRequest, Message: "confirm_mode must match current trading mode"}
	}
	return status, nil
}
