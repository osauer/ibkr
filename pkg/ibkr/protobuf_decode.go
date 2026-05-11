package ibkr

import (
	"fmt"
	"math"
	"strconv"

	"google.golang.org/protobuf/encoding/protowire"
)

type protoExecutionDetails struct {
	reqIDValue int32
	reqIDSet   bool
	contract   protoContractFields
	execution  protoExecutionFields
}

type protoContractFields struct {
	present       bool
	conID         int32
	conIDSet      bool
	symbol        string
	symbolSet     bool
	secType       string
	secTypeSet    bool
	expiry        string
	expirySet     bool
	strike        float64
	strikeSet     bool
	right         string
	rightSet      bool
	multiplier    float64
	multiplierSet bool
	exchange      string
	exchangeSet   bool
	currency      string
	currencySet   bool
	localSymbol   string
	localSet      bool
	tradingClass  string
	tradingSet    bool
}

type protoExecutionFields struct {
	present              bool
	orderID              int32
	orderIDSet           bool
	execID               string
	execIDSet            bool
	time                 string
	timeSet              bool
	account              string
	accountSet           bool
	exchange             string
	exchangeSet          bool
	side                 string
	sideSet              bool
	shares               string
	sharesSet            bool
	price                float64
	priceSet             bool
	permID               int64
	permIDSet            bool
	clientID             int32
	clientIDSet          bool
	isLiquidation        bool
	isLiquidationSet     bool
	cumQty               string
	cumQtySet            bool
	avgPrice             float64
	avgPriceSet          bool
	orderRef             string
	orderRefSet          bool
	evRule               string
	evRuleSet            bool
	evMultiplier         float64
	evMultiplierSet      bool
	modelCode            string
	modelCodeSet         bool
	lastLiquidity        int32
	lastLiquiditySet     bool
	pendingPriceRevision bool
	pendingSet           bool
	submitter            string
	submitterSet         bool
}

func decodeExecutionDetailsProto(payload []byte, serverVersion int) ([]string, error) {
	details, err := parseExecutionDetailsPayload(payload)
	if err != nil {
		return nil, err
	}

	version := serverVersion
	fields := make([]string, 0, 34)
	fields = append(fields, strconv.Itoa(msgExecutionData))
	if version >= 7 {
		fields = append(fields, stringFromOptionalInt32(details.reqIDValue, details.reqIDSet))
	}
	fields = append(fields, stringFromOptionalInt32(details.execution.orderID, details.execution.orderIDSet))
	if version >= 5 {
		fields = append(fields, stringFromOptionalInt32(details.contract.conID, details.contract.conIDSet))
	}
	fields = append(fields, stringFromOptionalString(details.contract.symbol, details.contract.symbolSet))
	fields = append(fields, stringFromOptionalString(details.contract.secType, details.contract.secTypeSet))
	fields = append(fields, stringFromOptionalString(details.contract.expiry, details.contract.expirySet))
	fields = append(fields, stringFromOptionalFloat64(details.contract.strike, details.contract.strikeSet))
	fields = append(fields, stringFromOptionalString(details.contract.right, details.contract.rightSet))
	if version >= 9 {
		fields = append(fields, stringFromOptionalFloat64(details.contract.multiplier, details.contract.multiplierSet))
	}
	fields = append(fields, stringFromOptionalString(details.contract.exchange, details.contract.exchangeSet))
	fields = append(fields, stringFromOptionalString(details.contract.currency, details.contract.currencySet))
	fields = append(fields, stringFromOptionalString(details.contract.localSymbol, details.contract.localSet))
	if version >= 10 {
		fields = append(fields, stringFromOptionalString(details.contract.tradingClass, details.contract.tradingSet))
	}
	fields = append(fields, stringFromOptionalString(details.execution.execID, details.execution.execIDSet))
	fields = append(fields, stringFromOptionalString(details.execution.time, details.execution.timeSet))
	fields = append(fields, stringFromOptionalString(details.execution.account, details.execution.accountSet))
	fields = append(fields, stringFromOptionalString(details.execution.exchange, details.execution.exchangeSet))
	fields = append(fields, stringFromOptionalString(details.execution.side, details.execution.sideSet))
	fields = append(fields, stringFromOptionalString(details.execution.shares, details.execution.sharesSet))
	fields = append(fields, stringFromOptionalFloat64(details.execution.price, details.execution.priceSet))
	fields = append(fields, stringFromOptionalInt64(details.execution.permID, details.execution.permIDSet))
	fields = append(fields, stringFromOptionalInt32(details.execution.clientID, details.execution.clientIDSet))
	fields = append(fields, stringFromOptionalBool(details.execution.isLiquidation, details.execution.isLiquidationSet))
	if version >= 6 {
		fields = append(fields, stringFromOptionalString(details.execution.cumQty, details.execution.cumQtySet))
		fields = append(fields, stringFromOptionalFloat64(details.execution.avgPrice, details.execution.avgPriceSet))
	}
	if version >= 8 {
		fields = append(fields, stringFromOptionalString(details.execution.orderRef, details.execution.orderRefSet))
	}
	if version >= 9 {
		fields = append(fields, stringFromOptionalString(details.execution.evRule, details.execution.evRuleSet))
		fields = append(fields, stringFromOptionalFloat64(details.execution.evMultiplier, details.execution.evMultiplierSet))
	}
	if serverVersion >= minServerVerModelsSupport {
		fields = append(fields, stringFromOptionalString(details.execution.modelCode, details.execution.modelCodeSet))
	}
	if serverVersion >= minServerVerLastLiquidity {
		fields = append(fields, stringFromOptionalInt32(details.execution.lastLiquidity, details.execution.lastLiquiditySet))
	}
	if serverVersion >= minServerVerPendingPriceRevision {
		fields = append(fields, stringFromOptionalBool(details.execution.pendingPriceRevision, details.execution.pendingSet))
	}
	if serverVersion >= minServerVerSubmitter {
		fields = append(fields, stringFromOptionalString(details.execution.submitter, details.execution.submitterSet))
	}

	return fields, nil
}

func decodeExecutionDetailsEndProto(payload []byte) ([]string, error) {
	var reqID int32
	var reqIDSet bool

	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return nil, fmt.Errorf("execDetailsEnd: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return nil, fmt.Errorf("execDetailsEnd: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return nil, fmt.Errorf("execDetailsEnd: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			reqID = int32(val)
			reqIDSet = true
		default:
			m := protowire.ConsumeFieldValue(num, typ, payload)
			if m < 0 {
				return nil, fmt.Errorf("execDetailsEnd: skip field %d: %v", num, protowire.ParseError(m))
			}
			payload = payload[m:]
		}
	}

	result := []string{strconv.Itoa(msgExecDetailsEnd)}
	result = append(result, stringFromOptionalInt32(reqID, reqIDSet))
	return result, nil
}

func parseExecutionDetailsPayload(payload []byte) (*protoExecutionDetails, error) {
	var details protoExecutionDetails
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return nil, fmt.Errorf("executionDetails: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return nil, fmt.Errorf("executionDetails: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return nil, fmt.Errorf("executionDetails: reqID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			details.reqIDValue = int32(val)
			details.reqIDSet = true
		case 2:
			if typ != protowire.BytesType {
				return nil, fmt.Errorf("executionDetails: field %d wire type %d", num, typ)
			}
			data, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return nil, fmt.Errorf("executionDetails: contract: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract, err := parseContractFields(data)
			if err != nil {
				return nil, err
			}
			details.contract = contract
		case 3:
			if typ != protowire.BytesType {
				return nil, fmt.Errorf("executionDetails: field %d wire type %d", num, typ)
			}
			data, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return nil, fmt.Errorf("executionDetails: execution: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			execution, err := parseExecutionFields(data)
			if err != nil {
				return nil, err
			}
			details.execution = execution
		default:
			m := protowire.ConsumeFieldValue(num, typ, payload)
			if m < 0 {
				return nil, fmt.Errorf("executionDetails: skip field %d: %v", num, protowire.ParseError(m))
			}
			payload = payload[m:]
		}
	}

	if !details.contract.present {
		return nil, fmt.Errorf("executionDetails: missing contract payload")
	}
	if !details.execution.present {
		return nil, fmt.Errorf("executionDetails: missing execution payload")
	}

	return &details, nil
}

func parseContractFields(payload []byte) (protoContractFields, error) {
	var contract protoContractFields
	contract.present = true
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return contract, fmt.Errorf("executionDetails.contract: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: conID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.conID = int32(val)
			contract.conIDSet = true
		case 2:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: symbol: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.symbol = string(val)
			contract.symbolSet = true
		case 3:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: secType: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.secType = string(val)
			contract.secTypeSet = true
		case 4:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: expiry: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.expiry = string(val)
			contract.expirySet = true
		case 5:
			if typ != protowire.Fixed64Type {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeFixed64(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: strike: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.strike = math.Float64frombits(val)
			contract.strikeSet = true
		case 6:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: right: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.right = string(val)
			contract.rightSet = true
		case 7:
			if typ != protowire.Fixed64Type {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeFixed64(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: multiplier: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.multiplier = math.Float64frombits(val)
			contract.multiplierSet = true
		case 8:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: exchange: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.exchange = string(val)
			contract.exchangeSet = true
		case 10:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: currency: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.currency = string(val)
			contract.currencySet = true
		case 11:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: localSymbol: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.localSymbol = string(val)
			contract.localSet = true
		case 12:
			if typ != protowire.BytesType {
				return contract, fmt.Errorf("executionDetails.contract: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: tradingClass: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			contract.tradingClass = string(val)
			contract.tradingSet = true
		default:
			m := protowire.ConsumeFieldValue(num, typ, payload)
			if m < 0 {
				return contract, fmt.Errorf("executionDetails.contract: skip field %d: %v", num, protowire.ParseError(m))
			}
			payload = payload[m:]
		}
	}
	return contract, nil
}

func parseExecutionFields(payload []byte) (protoExecutionFields, error) {
	var exec protoExecutionFields
	exec.present = true
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return exec, fmt.Errorf("executionDetails.execution: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: orderID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.orderID = int32(val)
			exec.orderIDSet = true
		case 2:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: execID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.execID = string(val)
			exec.execIDSet = true
		case 3:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: time: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.time = string(val)
			exec.timeSet = true
		case 4:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: account: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.account = string(val)
			exec.accountSet = true
		case 5:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: exchange: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.exchange = string(val)
			exec.exchangeSet = true
		case 6:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: side: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.side = string(val)
			exec.sideSet = true
		case 7:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: shares: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.shares = string(val)
			exec.sharesSet = true
		case 8:
			if typ != protowire.Fixed64Type {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeFixed64(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: price: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.price = math.Float64frombits(val)
			exec.priceSet = true
		case 9:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: permID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.permID = int64(val)
			exec.permIDSet = true
		case 10:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: clientID: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.clientID = int32(val)
			exec.clientIDSet = true
		case 11:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: isLiquidation: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.isLiquidation = val != 0
			exec.isLiquidationSet = true
		case 12:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: cumQty: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.cumQty = string(val)
			exec.cumQtySet = true
		case 13:
			if typ != protowire.Fixed64Type {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeFixed64(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: avgPrice: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.avgPrice = math.Float64frombits(val)
			exec.avgPriceSet = true
		case 14:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: orderRef: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.orderRef = string(val)
			exec.orderRefSet = true
		case 15:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: evRule: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.evRule = string(val)
			exec.evRuleSet = true
		case 16:
			if typ != protowire.Fixed64Type {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeFixed64(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: evMultiplier: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.evMultiplier = math.Float64frombits(val)
			exec.evMultiplierSet = true
		case 17:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: modelCode: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.modelCode = string(val)
			exec.modelCodeSet = true
		case 18:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: lastLiquidity: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.lastLiquidity = int32(val)
			exec.lastLiquiditySet = true
		case 19:
			if typ != protowire.VarintType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeVarint(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: pending: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.pendingPriceRevision = val != 0
			exec.pendingSet = true
		case 20:
			if typ != protowire.BytesType {
				return exec, fmt.Errorf("executionDetails.execution: field %d wire type %d", num, typ)
			}
			val, m := protowire.ConsumeBytes(payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: submitter: %v", protowire.ParseError(m))
			}
			payload = payload[m:]
			exec.submitter = string(val)
			exec.submitterSet = true
		default:
			m := protowire.ConsumeFieldValue(num, typ, payload)
			if m < 0 {
				return exec, fmt.Errorf("executionDetails.execution: skip field %d: %v", num, protowire.ParseError(m))
			}
			payload = payload[m:]
		}
	}
	return exec, nil
}

func stringFromOptionalInt32(value int32, set bool) string {
	if !set {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

func stringFromOptionalInt64(value int64, set bool) string {
	if !set {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func stringFromOptionalFloat64(value float64, set bool) string {
	if !set {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func stringFromOptionalBool(value bool, set bool) string {
	if !set {
		return ""
	}
	if value {
		return "1"
	}
	return "0"
}

func stringFromOptionalString(value string, set bool) string {
	if !set {
		return ""
	}
	return value
}
