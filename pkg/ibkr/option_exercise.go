package ibkr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type OptionExerciseRequest struct {
	TickerID         int
	Contract         *Contract
	ExerciseAction   int
	ExerciseQuantity int
	Account          string
	Override         int
	ManualOrderTime  string
}

const (
	OptionExerciseActionExercise = 1
	OptionExerciseActionLapse    = 2
)

func (c *Connector) ExerciseOptions(ctx context.Context, req OptionExerciseRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.isConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("no active connection")
	}
	if req.TickerID == 0 {
		req.TickerID = conn.GetNextRequestID()
	}
	return conn.ExerciseOptions(req)
}

// ExerciseOptions sends an IBKR option exercise instruction. The surrounding
// daemon keeps this behind its own trading, origin, and account/mode gates;
// this low-level method only validates and encodes the documented
// EClient.exerciseOptions request.
func (c *Connection) ExerciseOptions(req OptionExerciseRequest) error {
	if !tradingEnabled {
		return ErrTradingDisabled
	}
	if err := validateOptionExerciseRequest(req); err != nil {
		return err
	}
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	msg, err := c.encodeExerciseOptionsMessage(req)
	if err != nil {
		return err
	}
	return c.sendMessageWithType(msg, RequestTypeOrder)
}

func validateOptionExerciseRequest(req OptionExerciseRequest) error {
	if req.Contract == nil {
		return fmt.Errorf("exerciseOptions contract is required")
	}
	if req.TickerID <= 0 {
		return fmt.Errorf("exerciseOptions ticker id must be positive")
	}
	if !strings.EqualFold(req.Contract.SecType, "OPT") {
		return fmt.Errorf("exerciseOptions supports option contracts only")
	}
	if strings.TrimSpace(req.Contract.Symbol) == "" {
		return fmt.Errorf("exerciseOptions contract symbol is required")
	}
	if strings.TrimSpace(req.Contract.Expiry) == "" {
		return fmt.Errorf("exerciseOptions contract expiry is required")
	}
	if req.Contract.Strike <= 0 {
		return fmt.Errorf("exerciseOptions contract strike is required")
	}
	right := strings.ToUpper(strings.TrimSpace(req.Contract.Right))
	if right != "C" && right != "P" {
		return fmt.Errorf("exerciseOptions contract right must be C or P")
	}
	switch req.ExerciseAction {
	case OptionExerciseActionExercise, OptionExerciseActionLapse:
	default:
		return fmt.Errorf("exerciseOptions action must be 1 (exercise) or 2 (lapse)")
	}
	if req.ExerciseQuantity <= 0 {
		return fmt.Errorf("exerciseOptions quantity must be positive")
	}
	if strings.TrimSpace(req.Account) == "" {
		return fmt.Errorf("exerciseOptions account is required")
	}
	if req.Override != 0 && req.Override != 1 {
		return fmt.Errorf("exerciseOptions override must be 0 or 1")
	}
	return nil
}

func (c *Connection) encodeExerciseOptionsMessage(req OptionExerciseRequest) ([]byte, error) {
	if err := validateOptionExerciseRequest(req); err != nil {
		return nil, err
	}
	contract := req.Contract
	multiplier := ""
	if contract.Multiplier != 0 {
		multiplier = strconv.Itoa(contract.Multiplier)
	}
	fields := []any{
		exerciseOptions,
		2, // message version used by the documented exerciseOptions request shape
		req.TickerID,
		contract.ConID,
		strings.ToUpper(strings.TrimSpace(contract.Symbol)),
		"OPT",
		strings.TrimSpace(contract.Expiry),
		strconv.FormatFloat(contract.Strike, 'f', -1, 64),
		strings.ToUpper(strings.TrimSpace(contract.Right)),
		multiplier,
		ifEmpty(contract.Exchange, "SMART"),
		ifEmpty(contract.Currency, "USD"),
		strings.TrimSpace(contract.LocalSymbol),
		strings.TrimSpace(contract.TradingClass),
		req.ExerciseAction,
		req.ExerciseQuantity,
		strings.TrimSpace(req.Account),
		req.Override,
	}
	if c.serverVersion >= minServerVerManualOrderTime {
		fields = append(fields, strings.TrimSpace(req.ManualOrderTime))
	}
	return c.encodeMsg(fields...), nil
}
