package ibkr

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WireDirection indicates message flow relative to the IBKR gateway.
type WireDirection string

// Wire directions are relative to the client connection.
const (
	WireOutbound WireDirection = "OUT"
	WireInbound  WireDirection = "IN"
)

// wireEnv keys
const (
	// docgen:env IBKR_WIRE_INTERCEPTOR | Enable the account-sensitive decoded wire-frame recorder. Unset or false disables it.
	envWireEnable = "IBKR_WIRE_INTERCEPTOR"
	// docgen:env IBKR_WIRE_LOG_PATH | Append decoded account-sensitive wire frames as JSONL at this path. Unset keeps frames in memory only.
	envWireLogPath = "IBKR_WIRE_LOG_PATH"
	// docgen:env IBKR_WIRE_RING_SIZE | Maximum decoded wire frames retained in memory when the interceptor is enabled; default 256.
	envWireRingSize = "IBKR_WIRE_RING_SIZE"
)

// WireFrame captures a single encoded message with decoded fields.
type WireFrame struct {
	Seq         uint64        `json:"seq"`
	When        time.Time     `json:"ts"`
	Direction   WireDirection `json:"direction"`
	MsgID       int           `json:"msg_id"`
	MsgName     string        `json:"msg_name"`
	ReqID       string        `json:"req_id,omitempty"`
	Symbol      string        `json:"symbol,omitempty"`
	LengthBytes string        `json:"len_hex,omitempty"`
	Fields      []string      `json:"fields"`
	RawHex      string        `json:"hex"`
	Notes       string        `json:"notes,omitempty"`
}

// WireInterceptor passively records the IBKR wire protocol for diagnostics.
// Off by default; enable with IBKR_WIRE_INTERCEPTOR=1. The recorder mirrors
// every frame into a per-process ring buffer (size IBKR_WIRE_RING_SIZE,
// default 256) and, when IBKR_WIRE_LOG_PATH is set, also appends one JSON
// object per line to the named file. Captured frames are account-sensitive
// — see SECURITY.md.
type WireInterceptor struct {
	clientID int

	ringMu  sync.Mutex
	ring    []WireFrame
	ringCap int
	seq     uint64

	jsonMu   sync.Mutex
	jsonFile *os.File
	jsonEnc  *json.Encoder

	enabled bool
}

// Close releases any resources associated with the interceptor.
func (w *WireInterceptor) Close() error {
	if w == nil {
		return nil
	}
	w.jsonMu.Lock()
	defer w.jsonMu.Unlock()
	if w.jsonFile != nil {
		err := w.jsonFile.Close()
		w.jsonFile = nil
		return err
	}
	return nil
}

// NewWireInterceptorFromEnv instantiates a wire interceptor using environment flags.
func NewWireInterceptorFromEnv(clientID int) (*WireInterceptor, error) {
	enable, ok := lookupEnvBool(envWireEnable)
	if ok && !enable {
		return nil, nil
	}

	if !ok && os.Getenv(envWireEnable) == "" {
		// Default disabled unless explicitly requested.
		return nil, nil
	}

	ringSize := 256
	if value := strings.TrimSpace(os.Getenv(envWireRingSize)); value != "" {
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			ringSize = v
		}
	}

	var jsonFile *os.File
	if path := strings.TrimSpace(os.Getenv(envWireLogPath)); path != "" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("wire interceptor: mkdir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("wire interceptor: chmod %s: %w", dir, err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("wire interceptor: open log %s: %w", path, err)
		}
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("wire interceptor: chmod log %s: %w", path, err)
		}
		jsonFile = f
		wireLogger.Infof("Wire interceptor logging to %s", path)
	}

	var enc *json.Encoder
	if jsonFile != nil {
		enc = json.NewEncoder(jsonFile)
	}

	return &WireInterceptor{
		clientID: clientID,
		ringCap:  ringSize,
		ring:     make([]WireFrame, 0, ringSize),
		jsonFile: jsonFile,
		jsonEnc:  enc,
		enabled:  true,
	}, nil
}

// Enabled returns true if the interceptor is active.
func (w *WireInterceptor) Enabled() bool {
	return w != nil && w.enabled
}

// RecordOutbound processes an outgoing frame. fields may be nil; in that case decode will be skipped.
func (w *WireInterceptor) RecordOutbound(msgID int, raw []byte, fields []string) {
	if !w.Enabled() {
		return
	}
	frame := w.newFrame(WireOutbound, msgID, raw, fields)
	w.appendFrame(frame)
	w.persistFrame(frame)
	w.logFrame(frame)
}

// RecordInbound records an incoming frame from IBKR.
func (w *WireInterceptor) RecordInbound(msgID int, raw []byte, fields []string) {
	if !w.Enabled() {
		return
	}
	frame := w.newFrame(WireInbound, msgID, raw, fields)
	w.appendFrame(frame)
	w.persistFrame(frame)
	w.logFrame(frame)
}

func (w *WireInterceptor) newFrame(dir WireDirection, msgID int, raw []byte, fields []string) WireFrame {
	w.ringMu.Lock()
	w.seq++
	seq := w.seq
	w.ringMu.Unlock()

	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(raw)))

	frame := WireFrame{
		Seq:         seq,
		When:        time.Now(),
		Direction:   dir,
		MsgID:       msgID,
		MsgName:     resolveMessageNameForDirection(dir, msgID),
		LengthBytes: hex.EncodeToString(length),
		Fields:      cloneStringSlice(fields),
		RawHex:      hex.EncodeToString(raw),
	}

	if len(frame.Fields) > 0 {
		if req, sym := extractReqInfo(msgID, frame.Fields); req != "" {
			frame.ReqID = req
			frame.Symbol = sym
		}
	}
	return frame
}

func (w *WireInterceptor) appendFrame(frame WireFrame) {
	w.ringMu.Lock()
	defer w.ringMu.Unlock()
	if len(w.ring) == w.ringCap {
		copy(w.ring, w.ring[1:])
		w.ring[len(w.ring)-1] = frame
	} else {
		w.ring = append(w.ring, frame)
	}
}

func (w *WireInterceptor) persistFrame(frame WireFrame) {
	if w.jsonEnc == nil {
		return
	}
	w.jsonMu.Lock()
	defer w.jsonMu.Unlock()
	if err := w.jsonEnc.Encode(frame); err != nil {
		wireLogger.Warnf("Wire interceptor failed to persist frame: %v", err)
	}
}

func (w *WireInterceptor) logFrame(frame WireFrame) {
	wireLogger.Infof("[%s] #%d %s len=%s reqID=%s symbol=%s fields=%v",
		frame.Direction, frame.Seq, frame.MsgName, frame.LengthBytes, frame.ReqID, frame.Symbol, frame.Fields)
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func extractReqInfo(msgID int, fields []string) (string, string) {
	if len(fields) == 0 {
		return "", ""
	}
	if msgID == protoPlaceOrderMsgID {
		return summaryFieldValue(fields, "orderId="), summaryFieldValue(fields, "symbol=")
	}
	if len(fields) > 1 && fields[1] == "protobuf" {
		return summaryFieldValue(fields, "orderId="), summaryFieldValue(fields, "symbol=")
	}
	switch msgID {
	case placeOrder:
		return fieldValue(fields, 1), fieldValue(fields, 3)
	case reqMktData:
		return fieldValue(fields, 2), fieldValue(fields, 4)
	case reqContractData:
		return fieldValue(fields, 2), fieldValue(fields, 4)
	case reqHistoricalData:
		return fieldValue(fields, 1), fieldValue(fields, 3)
	case msgErrMsg:
		return fieldValue(fields, 2), ""
	default:
		return "", ""
	}
}

func resolveMessageName(msgID int) string {
	if msgID == protoPlaceOrderMsgID {
		return "placeOrderProtoBuf"
	}
	switch msgID {
	case placeOrder:
		return "placeOrder"
	case reqMktData:
		return "reqMktData"
	case reqContractData:
		return "reqContractData"
	case reqHistoricalData:
		return "reqHistoricalData"
	case reqCalcImpliedVolatility:
		return "reqCalcImpliedVol"
	case cancelMktData:
		return "cancelMktData"
	case msgOpenOrder:
		return "openOrder"
	case msgExecDetails:
		return "execDetails"
	case msgErrMsg:
		return "error"
	case msgSystemNotification:
		return "systemNotification"
	default:
		return fmt.Sprintf("msg(%d)", msgID)
	}
}

func resolveMessageNameForDirection(dir WireDirection, msgID int) string {
	if dir == WireOutbound && msgID == protoCancelOrderMsgID {
		return "cancelOrderProtoBuf"
	}
	return resolveMessageName(msgID)
}

func summaryFieldValue(fields []string, prefix string) string {
	for _, field := range fields {
		if after, ok := strings.CutPrefix(field, prefix); ok {
			return after
		}
	}
	return ""
}
