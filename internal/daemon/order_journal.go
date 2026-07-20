package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	orderJournalFileVersion = 1

	orderJournalEventPreviewed          = "previewed"
	orderJournalEventTokenConfirmed     = "token-confirmed"
	orderJournalEventSendAttempted      = "send-attempted"
	orderJournalEventSendError          = "send-error"
	orderJournalEventBrokerError        = "broker-error"
	orderJournalEventBrokerAcknowledged = "broker-acknowledged"
	orderJournalEventStatusUpdated      = "status-updated"
	orderJournalEventModifyRequested    = "modify-requested"
	orderJournalEventCancelRequested    = "cancel-requested"
	orderJournalEventReconciledUnknown  = "reconciled-unknown"
	orderJournalEventReconciledAbsent   = "reconciled-absent"

	orderSendStateReserved           = "reserved"
	orderSendStateSendAttempted      = "send_attempted"
	orderSendStateBrokerAcknowledged = "broker_acknowledged"
	orderSendStateUncertainSend      = "uncertain_send"
	orderSendStateTerminal           = "terminal"
)

var errOrderPreviewTokenAlreadyUsed = errors.New("preview token already used")

type orderJournalStore struct {
	Path string
	mu   sync.Mutex
	// onAppend nudges the history-index ingester after a successful append
	// (data-free kick; the journal file is the only ingest input). Nil-safe
	// and non-blocking; invoked under mu at the end of appendLocked /
	// appendAllLocked so every present and future append site is covered.
	onAppend func()
	// tokenIndex, when set, may answer the token-redemption prior-event
	// lookup from the derived history index: it returns the raw journal
	// lines carrying the token and ok=true only when the index is provably
	// complete for the journal at this instant (watermark == size, no
	// parse markers, query succeeded). ok=false always falls back to the
	// unchanged full journal scan. The journal scan remains the
	// semantics-defining reference implementation.
	tokenIndex func(tokenID string) ([][]byte, bool)
}

type orderJournalEvent struct {
	Version         int                 `json:"version"`
	At              time.Time           `json:"at"`
	Type            string              `json:"type"`
	OrderRef        string              `json:"order_ref,omitempty"`
	PreviewTokenID  string              `json:"preview_token_id,omitempty"`
	ReservedOrderID int                 `json:"reserved_order_id,omitempty"`
	ClientID        int                 `json:"client_id,omitempty"`
	PermID          int                 `json:"perm_id,omitempty"`
	Account         string              `json:"account,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Mode            string              `json:"mode,omitempty"`
	Source          string              `json:"source,omitempty"`
	Origin          string              `json:"origin,omitempty"`
	PurgeID         string              `json:"purge_id,omitempty"`
	LegID           string              `json:"leg_id,omitempty"`
	BypassPreview   bool                `json:"bypass_preview,omitempty"`
	Symbol          string              `json:"symbol,omitempty"`
	SecType         string              `json:"sec_type,omitempty"`
	ConID           int                 `json:"con_id,omitempty"`
	Exchange        string              `json:"exchange,omitempty"`
	PrimaryExch     string              `json:"primary_exch,omitempty"`
	Currency        string              `json:"currency,omitempty"`
	LocalSymbol     string              `json:"local_symbol,omitempty"`
	TradingClass    string              `json:"trading_class,omitempty"`
	Expiry          string              `json:"expiry,omitempty"`
	Strike          float64             `json:"strike,omitempty"`
	Right           string              `json:"right,omitempty"`
	Multiplier      int                 `json:"multiplier,omitempty"`
	Action          string              `json:"action,omitempty"`
	OrderType       string              `json:"order_type,omitempty"`
	TIF             string              `json:"tif,omitempty"`
	TriggerMethod   int                 `json:"trigger_method,omitempty"`
	OutsideRTH      bool                `json:"outside_rth,omitempty"`
	Quantity        float64             `json:"quantity,omitempty"`
	LimitPrice      float64             `json:"limit_price,omitempty"`
	Trail           *rpc.OrderTrailSpec `json:"trail,omitempty"`
	OpenClose       string              `json:"open_close,omitempty"`
	Status          string              `json:"status,omitempty"`
	Filled          float64             `json:"filled,omitempty"`
	Remaining       float64             `json:"remaining,omitempty"`
	AvgFillPrice    float64             `json:"avg_fill_price,omitempty"`
	LastFillPrice   float64             `json:"last_fill_price,omitempty"`
	WhyHeld         string              `json:"why_held,omitempty"`
	MktCapPrice     float64             `json:"mkt_cap_price,omitempty"`
	ExecID          string              `json:"exec_id,omitempty"`
	ExecTime        string              `json:"exec_time,omitempty"`
	SendState       string              `json:"send_state,omitempty"`
	Message         string              `json:"message,omitempty"`
}

type orderJournalSummary struct {
	OpenOrders int
	LastEvent  string
}

func defaultOrderJournalPath() (string, error) {
	return defaultTradingStatePath("order-journal.jsonl")
}

func newOrderJournalStore(path string) *orderJournalStore {
	return &orderJournalStore{Path: path}
}

func (s *orderJournalStore) Append(ev orderJournalEvent) error {
	if s == nil {
		return fmt.Errorf("order journal path is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(ev)
}

func (s *orderJournalStore) AppendAll(events []orderJournalEvent) error {
	if len(events) == 0 {
		return nil
	}
	if s == nil {
		return fmt.Errorf("order journal path is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendAllLocked(events)
}

func (s *orderJournalStore) appendLocked(ev orderJournalEvent) error {
	if s == nil || s.Path == "" {
		return fmt.Errorf("order journal path is empty")
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Version == 0 {
		ev.Version = orderJournalFileVersion
	}
	if err := validateOrderJournalEvent(ev); err != nil {
		return err
	}
	if err := ensurePrivateStateDir(s.Path); err != nil {
		return err
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal order journal event: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open order journal %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod order journal: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("append order journal: %w", err)
	}
	if s.onAppend != nil {
		s.onAppend()
	}
	return nil
}

func (s *orderJournalStore) appendAllLocked(events []orderJournalEvent) error {
	if s == nil || s.Path == "" {
		return fmt.Errorf("order journal path is empty")
	}
	var data []byte
	for _, ev := range events {
		if ev.At.IsZero() {
			ev.At = time.Now().UTC()
		}
		if ev.Version == 0 {
			ev.Version = orderJournalFileVersion
		}
		if err := validateOrderJournalEvent(ev); err != nil {
			return err
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("marshal order journal event: %w", err)
		}
		data = append(data, raw...)
		data = append(data, '\n')
	}
	if err := ensurePrivateStateDir(s.Path); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open order journal %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod order journal: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("append order journal: %w", err)
	}
	if s.onAppend != nil {
		s.onAppend()
	}
	return nil
}

func (s *orderJournalStore) LoadEvents(limit int) ([]orderJournalEvent, error) {
	if s == nil {
		return nil, fmt.Errorf("order journal path is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadEventsLocked(limit)
}

func (s *orderJournalStore) loadEventsLocked(limit int) ([]orderJournalEvent, error) {
	if s == nil || s.Path == "" {
		return nil, fmt.Errorf("order journal path is empty")
	}
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open order journal %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	events := make([]orderJournalEvent, 0)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var ev orderJournalEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, fmt.Errorf("parse order journal line %d: %w", line, err)
		}
		if ev.Version != orderJournalFileVersion {
			return nil, fmt.Errorf("unsupported order journal version %d on line %d", ev.Version, line)
		}
		events = append(events, ev)
		if limit > 0 && len(events) > limit {
			copy(events, events[1:])
			events = events[:limit]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan order journal: %w", err)
	}
	return events, nil
}

func (s *orderJournalStore) ConfirmPreviewTokenUse(ev orderJournalEvent) error {
	return s.ConfirmPreviewTokenUseAndAppend(ev)
}

func (s *orderJournalStore) ConfirmPreviewTokenUseAndAppend(ev orderJournalEvent, after ...orderJournalEvent) error {
	if s == nil {
		return fmt.Errorf("order journal path is empty")
	}
	if ev.PreviewTokenID == "" {
		return fmt.Errorf("preview token id is required")
	}
	if ev.Type == "" {
		ev.Type = orderJournalEventTokenConfirmed
	}
	if ev.Type != orderJournalEventTokenConfirmed {
		return fmt.Errorf("preview token confirmation event type must be %q", orderJournalEventTokenConfirmed)
	}

	// The existing mu critical section is the serialization guarantee: all
	// appends go through it, so while it is held the journal cannot grow
	// and the prior-event check races nothing.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Indexed fast path: only when the history index proves itself
	// complete for the journal at this instant AND every returned line
	// decodes under the daemon's own event type. Anything else falls back
	// to the unchanged full scan below. Both paths run the same
	// consumption predicate and error formatting, so accept/reject
	// decisions and error strings are byte-identical by construction.
	checked := false
	if s.tokenIndex != nil {
		if raws, ok := s.tokenIndex(ev.PreviewTokenID); ok {
			if priors, decodeOK := decodeOrderJournalRawLines(raws); decodeOK {
				if err := orderJournalPriorTokenUse(priors, ev.PreviewTokenID); err != nil {
					return err
				}
				checked = true
			}
		}
	}
	if !checked {
		events, err := s.loadEventsLocked(0)
		if err != nil {
			return err
		}
		if err := orderJournalPriorTokenUse(events, ev.PreviewTokenID); err != nil {
			return err
		}
	}
	if err := s.appendLocked(ev); err != nil {
		return err
	}
	for _, next := range after {
		if err := s.appendLocked(next); err != nil {
			return err
		}
	}
	return nil
}

// orderJournalPriorTokenUse is the single reference implementation of the
// token-consumption check, shared verbatim by the journal-scan and
// indexed paths (SQL only prunes; this Go predicate decides).
func orderJournalPriorTokenUse(events []orderJournalEvent, tokenID string) error {
	for _, prior := range events {
		if prior.PreviewTokenID != tokenID {
			continue
		}
		if orderJournalEventConsumesPreviewToken(prior) {
			when := ""
			if !prior.At.IsZero() {
				when = " at " + prior.At.Format(time.RFC3339)
			}
			return fmt.Errorf("%w: %s was already consumed by %s%s", errOrderPreviewTokenAlreadyUsed, tokenID, prior.Type, when)
		}
	}
	return nil
}

// decodeOrderJournalRawLines decodes indexed raw journal lines with the
// daemon's own event type and the same version validation LoadEvents
// applies. ok=false on any failure — the caller must fall back to the
// journal scan, which fails loudly on the same line.
func decodeOrderJournalRawLines(raws [][]byte) ([]orderJournalEvent, bool) {
	events := make([]orderJournalEvent, 0, len(raws))
	for _, raw := range raws {
		var ev orderJournalEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, false
		}
		if ev.Version != orderJournalFileVersion {
			return nil, false
		}
		events = append(events, ev)
	}
	return events, true
}

func (s *orderJournalStore) Summary() (orderJournalSummary, error) {
	events, err := s.LoadEvents(0)
	if err != nil {
		return orderJournalSummary{}, err
	}
	var last orderJournalEvent
	for _, ev := range events {
		last = ev
	}
	var summary orderJournalSummary
	for _, view := range buildOrderViews(events) {
		if view.Open {
			summary.OpenOrders++
		}
	}
	if !last.At.IsZero() {
		summary.LastEvent = fmt.Sprintf("%s %s at %s", last.Type, orderJournalEventLabel(last), last.At.Format(time.RFC3339))
	}
	return summary, nil
}

func validateOrderJournalEvent(ev orderJournalEvent) error {
	if ev.Type == "" {
		return fmt.Errorf("order journal event type is required")
	}
	if ev.Version != orderJournalFileVersion {
		return fmt.Errorf("unsupported order journal version %d", ev.Version)
	}
	return nil
}

func orderJournalEventConsumesPreviewToken(ev orderJournalEvent) bool {
	switch ev.Type {
	case orderJournalEventTokenConfirmed,
		orderJournalEventSendAttempted,
		orderJournalEventSendError,
		orderJournalEventBrokerAcknowledged,
		orderJournalEventModifyRequested,
		orderJournalEventCancelRequested:
		return true
	default:
		return false
	}
}

func orderJournalEventLabel(ev orderJournalEvent) string {
	if ev.OrderRef != "" {
		return ev.OrderRef
	}
	if ev.ReservedOrderID != 0 {
		return "order:" + strconv.Itoa(ev.ReservedOrderID)
	}
	if ev.PermID != 0 {
		return "perm:" + strconv.Itoa(ev.PermID)
	}
	return "unknown-order"
}

func maxReservedBrokerOrderID(store *orderJournalStore) (int, error) {
	if store == nil {
		return 0, nil
	}
	events, err := store.LoadEvents(0)
	if err != nil {
		return 0, err
	}
	var maxID int
	for _, ev := range events {
		if ev.ReservedOrderID > maxID {
			maxID = ev.ReservedOrderID
		}
	}
	return maxID, nil
}
