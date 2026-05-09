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

const (
	WireOutbound WireDirection = "OUT"
	WireInbound  WireDirection = "IN"
)

// wireEnv keys
const (
	envWireEnable      = "IBKR_WIRE_INTERCEPTOR"
	envWireLogPath     = "IBKR_WIRE_LOG_PATH"
	envWireRingSize    = "IBKR_WIRE_RING_SIZE"
	envWireMaxAttempts = "IBKR_WIRE_MAX_AUTOFIX_ATTEMPTS"
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

// OverrideOperation defines a simple field manipulation that callers may
// install at runtime to patch outbound wire frames. Originally driven by an
// AI-assisted diagnostic loop; the override execution path is preserved so
// future tooling can attach.
type OverrideOperation struct {
	Action string `json:"action"`          // "insert", "set", "delete"
	Index  int    `json:"index"`           // zero-based field index
	Value  string `json:"value,omitempty"` // value to insert or set
}

// messageOverride stores runtime mutations for a message id.
type messageOverride struct {
	MsgID      int
	Operations []OverrideOperation
	Reason     string
	Attempts   int
	AppliedAt  time.Time
	LastError  time.Time
}

// ParseError encapsulates a live parser failure from IBKR.
type ParseError struct {
	ClientID int
	ReqID    int
	Symbol   string
	Code     int
	Message  string
	Context  []string
}

// WireInterceptor coordinates wire capture, AI-assisted diagnostics, and runtime overrides.
type WireInterceptor struct {
	clientID int

	ringMu  sync.Mutex
	ring    []WireFrame
	ringCap int
	seq     uint64

	jsonMu   sync.Mutex
	jsonFile *os.File
	jsonEnc  *json.Encoder

	overridesMu sync.Mutex
	overrides   map[int]*messageOverride

	maxAttempts int

	enabled   bool
	autoApply bool
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

	maxAttempts := 3
	if value := strings.TrimSpace(os.Getenv(envWireMaxAttempts)); value != "" {
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			maxAttempts = v
		}
	}

	var jsonFile *os.File
	if path := strings.TrimSpace(os.Getenv(envWireLogPath)); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("wire interceptor: mkdir %s: %w", filepath.Dir(path), err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("wire interceptor: open log %s: %w", path, err)
		}
		jsonFile = f
		wireLogger.Infof("Wire interceptor logging to %s", path)
	}

	var enc *json.Encoder
	if jsonFile != nil {
		enc = json.NewEncoder(jsonFile)
	}

	return &WireInterceptor{
		clientID:    clientID,
		ringCap:     ringSize,
		ring:        make([]WireFrame, 0, ringSize),
		jsonFile:    jsonFile,
		jsonEnc:     enc,
		overrides:   make(map[int]*messageOverride),
		maxAttempts: maxAttempts,
		enabled:     true,
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

// ApplyOutboundOverrides mutates the provided fields if an override exists.
func (w *WireInterceptor) ApplyOutboundOverrides(msgID int, fields []string) ([]string, bool) {
	if !w.Enabled() {
		return fields, false
	}
	w.overridesMu.Lock()
	override, ok := w.overrides[msgID]
	w.overridesMu.Unlock()
	if !ok || override == nil || len(override.Operations) == 0 {
		return fields, false
	}

	if !w.autoApply {
		return fields, false
	}

	if override.Attempts >= w.maxAttempts {
		return fields, false
	}

	modified := make([]string, len(fields))
	copy(modified, fields)

	updated, changed := applyOperations(modified, override.Operations)
	if !changed {
		return fields, false
	}

	override.Attempts++
	override.AppliedAt = time.Now()
	w.overridesMu.Lock()
	w.overrides[msgID] = override
	w.overridesMu.Unlock()

	wireLogger.Warnf("Applied runtime override for %s (attempt %d)", resolveMessageName(msgID), override.Attempts)
	return updated, true
}

// HandleParserError records a parser error from the gateway. Earlier versions
// of the package fed this into an AI-assisted suggestion loop; v1 of the
// standalone tool simply logs the failure so the wire log can be inspected
// post-hoc.
func (w *WireInterceptor) HandleParserError(err ParseError) {
	if !w.Enabled() {
		return
	}
	wireLogger.Warnf("Parser error %d (reqID=%d symbol=%s): %s", err.Code, err.ReqID, err.Symbol, err.Message)
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
		MsgName:     resolveMessageName(msgID),
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
	switch msgID {
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
	switch msgID {
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
	case msgErrMsg:
		return "error"
	case msgSystemNotification:
		return "systemNotification"
	default:
		return fmt.Sprintf("msg(%d)", msgID)
	}
}

func applyOperations(fields []string, ops []OverrideOperation) ([]string, bool) {
	if len(ops) == 0 {
		return fields, false
	}
	modified := false
	out := fields
	for _, op := range ops {
		switch strings.ToLower(op.Action) {
		case "set":
			if op.Index < 0 || op.Index >= len(out) {
				continue
			}
			if out[op.Index] != op.Value {
				out[op.Index] = op.Value
				modified = true
			}
		case "insert":
			if op.Index < 0 {
				continue
			}
			if op.Index < len(out) && out[op.Index] == op.Value {
				continue
			}
			if op.Index >= len(out) {
				out = append(out, op.Value)
			} else {
				out = append(out[:op.Index+1], out[op.Index:]...)
				out[op.Index] = op.Value
			}
			modified = true
		case "delete":
			if op.Index < 0 || op.Index >= len(out) {
				continue
			}
			out = append(out[:op.Index], out[op.Index+1:]...)
			modified = true
		}
	}
	return out, modified
}
