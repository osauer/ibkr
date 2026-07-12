package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

const (
	proposalOutcomeFileVersion = 1

	proposalOutcomeStateSubmitted = "submitted"
	proposalOutcomeStateFilled    = "filled"
	proposalOutcomeStateMarked    = "marked"
)

type proposalOutcomeStore struct {
	Path        string
	mu          sync.Mutex
	outcomeKeys map[string]struct{}
}

type proposalOutcomeMark struct {
	Version            int                                 `json:"version"`
	At                 time.Time                           `json:"at"`
	MarkDate           string                              `json:"mark_date"`
	State              string                              `json:"state"`
	ProposalKey        string                              `json:"proposal_key,omitempty"`
	Revision           string                              `json:"revision,omitempty"`
	Bucket             string                              `json:"bucket,omitempty"`
	Symbol             string                              `json:"symbol,omitempty"`
	SecType            string                              `json:"sec_type,omitempty"`
	Action             string                              `json:"action,omitempty"`
	Quantity           float64                             `json:"quantity,omitempty"`
	OrderRef           string                              `json:"order_ref,omitempty"`
	PreviewTokenID     string                              `json:"preview_token_id,omitempty"`
	ExecID             string                              `json:"exec_id,omitempty"`
	PolicyID           string                              `json:"policy_id,omitempty"`
	PolicyVersion      int                                 `json:"policy_version,omitempty"`
	PolicyFingerprint  rpc.Fingerprint                     `json:"policy_fingerprint,omitzero"`
	SourceFingerprints rpc.TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
	BaselinePrice      float64                             `json:"baseline_price,omitempty"`
	MarkPrice          float64                             `json:"mark_price,omitempty"`
	AvgFillPrice       float64                             `json:"avg_fill_price,omitempty"`
	ExecutionPnL       float64                             `json:"execution_pnl,omitempty"`
	BenchmarkSymbol    string                              `json:"benchmark_symbol,omitempty"`
	Message            string                              `json:"message,omitempty"`
}

func defaultProposalOutcomesPath() (string, error) {
	return defaultTradingStatePath("trade-proposal-outcomes.jsonl")
}

func newProposalOutcomeStore(path string) *proposalOutcomeStore {
	return &proposalOutcomeStore{Path: path}
}

func (s *Server) installProposalOutcomeStore() {
	path, err := defaultProposalOutcomesPath()
	if err != nil {
		s.warnf("trade proposal outcomes: resolve state path: %v (outcome book disabled)", err)
		return
	}
	s.proposalOutcomes = newProposalOutcomeStore(path)
}

func (s *proposalOutcomeStore) AppendMark(mark proposalOutcomeMark) error {
	if s == nil || s.Path == "" {
		return fmt.Errorf("proposal outcome path is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendMarkLocked(mark)
}

func (s *proposalOutcomeStore) appendMarkLocked(mark proposalOutcomeMark) error {
	if mark.At.IsZero() {
		mark.At = time.Now().UTC()
	}
	if mark.MarkDate == "" {
		mark.MarkDate = mark.At.Format(time.DateOnly)
	}
	if mark.Version == 0 {
		mark.Version = proposalOutcomeFileVersion
	}
	if strings.TrimSpace(mark.State) == "" {
		return fmt.Errorf("proposal outcome state is required")
	}
	if strings.TrimSpace(mark.ProposalKey) == "" && strings.TrimSpace(mark.OrderRef) == "" && strings.TrimSpace(mark.PreviewTokenID) == "" {
		return fmt.Errorf("proposal outcome requires proposal_key, order_ref, or preview_token_id")
	}
	if s.outcomeKeys == nil {
		outcomeKeys, err := s.loadOutcomeKeysLocked()
		if err != nil {
			return err
		}
		s.outcomeKeys = outcomeKeys
	}
	identity := proposalOutcomeIdentity(mark)
	if _, seen := s.outcomeKeys[identity]; seen {
		return nil
	}
	if err := ensurePrivateStateDir(s.Path); err != nil {
		return err
	}
	data, err := json.Marshal(mark)
	if err != nil {
		return fmt.Errorf("marshal proposal outcome: %w", err)
	}
	data = append(data, '\n')
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open proposal outcomes %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod proposal outcomes: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		s.outcomeKeys = nil
		return fmt.Errorf("append proposal outcome: %w", err)
	}
	s.outcomeKeys[identity] = struct{}{}
	return nil
}

func (s *proposalOutcomeStore) loadOutcomeKeysLocked() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("open proposal outcomes %s: %w", s.Path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var mark proposalOutcomeMark
		if err := json.Unmarshal([]byte(line), &mark); err != nil {
			return nil, fmt.Errorf("decode proposal outcome: %w", err)
		}
		out[proposalOutcomeIdentity(mark)] = struct{}{}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read proposal outcomes: %w", err)
	}
	return out, nil
}

func proposalOutcomeIdentity(mark proposalOutcomeMark) string {
	return strings.Join([]string{
		strings.TrimSpace(mark.ProposalKey),
		strings.TrimSpace(mark.MarkDate),
		strings.TrimSpace(mark.State),
		strings.TrimSpace(mark.OrderRef),
		strings.TrimSpace(mark.PreviewTokenID),
		strings.TrimSpace(mark.ExecID),
	}, "|")
}

func proposalOutcomeSubmitted(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult, place *rpc.OrderPlaceResult, at time.Time) proposalOutcomeMark {
	var orderRef, tokenID string
	var baseline float64
	var qty float64
	if preview != nil {
		orderRef = preview.Draft.OrderRef
		tokenID = preview.PreviewTokenID
		baseline = preview.Draft.LimitPrice
		qty = float64(preview.Draft.Quantity)
	}
	if place != nil {
		if place.OrderRef != "" {
			orderRef = place.OrderRef
		}
		if place.PreviewTokenID != "" {
			tokenID = place.PreviewTokenID
		}
		if qty == 0 {
			qty = float64(place.Draft.Quantity)
		}
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateSubmitted,
		ProposalKey:        prop.Key,
		Revision:           prop.Revision,
		Bucket:             prop.Bucket,
		Symbol:             prop.Symbol,
		SecType:            prop.SecType,
		Action:             prop.Action,
		Quantity:           qty,
		OrderRef:           orderRef,
		PreviewTokenID:     tokenID,
		PolicyID:           prop.PolicyID,
		PolicyVersion:      prop.PolicyVersion,
		PolicyFingerprint:  prop.PolicyFingerprint,
		SourceFingerprints: prop.SourceFingerprints,
		BaselinePrice:      baseline,
		BenchmarkSymbol:    "SPY",
		Message:            "proposal submitted; daily shadow-hold marks will be recorded",
	}
}

func proposalOutcomeMarked(prop rpc.TradeProposal, at time.Time) proposalOutcomeMark {
	markPrice := 0.0
	if prop.LimitPrice != nil {
		markPrice = *prop.LimitPrice
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateMarked,
		ProposalKey:        prop.Key,
		Revision:           prop.Revision,
		Bucket:             prop.Bucket,
		Symbol:             prop.Symbol,
		SecType:            prop.SecType,
		Action:             prop.Action,
		Quantity:           float64(prop.Quantity),
		PolicyID:           prop.PolicyID,
		PolicyVersion:      prop.PolicyVersion,
		PolicyFingerprint:  prop.PolicyFingerprint,
		SourceFingerprints: prop.SourceFingerprints,
		BaselinePrice:      markPrice,
		MarkPrice:          markPrice,
		BenchmarkSymbol:    "SPY",
		Message:            "daily shadow-hold proposal mark",
	}
}

func proposalOutcomeFilledFromJournal(ev orderJournalEvent, submitted proposalEvent, at time.Time) proposalOutcomeMark {
	qty := ev.Filled
	if qty == 0 {
		qty = ev.Quantity
	}
	executionPnL := 0.0
	if ev.AvgFillPrice > 0 && ev.LimitPrice > 0 && qty != 0 {
		switch strings.ToUpper(ev.Action) {
		case rpc.OrderActionSell:
			executionPnL = (ev.AvgFillPrice - ev.LimitPrice) * qty * float64(max(ev.Multiplier, 1))
		case rpc.OrderActionBuy:
			executionPnL = (ev.LimitPrice - ev.AvgFillPrice) * qty * float64(max(ev.Multiplier, 1))
		}
	}
	return proposalOutcomeMark{
		At:                 at,
		MarkDate:           at.Format(time.DateOnly),
		State:              proposalOutcomeStateFilled,
		ProposalKey:        submitted.Key,
		Revision:           submitted.Revision,
		Bucket:             submitted.Bucket,
		Symbol:             ev.Symbol,
		SecType:            ev.SecType,
		Action:             ev.Action,
		Quantity:           qty,
		OrderRef:           ev.OrderRef,
		PreviewTokenID:     ev.PreviewTokenID,
		ExecID:             ev.ExecID,
		PolicyID:           submitted.PolicyID,
		PolicyVersion:      submitted.PolicyVersion,
		PolicyFingerprint:  submitted.PolicyFingerprint,
		SourceFingerprints: submitted.SourceFingerprints,
		BaselinePrice:      ev.LimitPrice,
		AvgFillPrice:       ev.AvgFillPrice,
		ExecutionPnL:       executionPnL,
		BenchmarkSymbol:    "SPY",
		Message:            "proposal order fill observed from order journal lifecycle",
	}
}
