package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/osauer/ibkr/internal/app/daemonclient"
	"github.com/osauer/ibkr/internal/rpc"
)

type Service struct {
	client      daemonclient.Client
	pollEvery   time.Duration
	canaryEvery time.Duration
	now         func() time.Time

	mu          sync.Mutex
	snapshot    Snapshot
	hashes      map[string]string
	lastEventAt map[string]time.Time
	subs        map[chan Event]struct{}
	nextCanary  time.Time

	OnCanary func(context.Context, rpc.CanaryResult)
}

type Snapshot struct {
	Version   int64                     `json:"version"`
	UpdatedAt time.Time                 `json:"updated_at,omitzero"`
	Status    *rpc.HealthResult         `json:"status,omitempty"`
	Calendar  *rpc.MarketCalendarResult `json:"market_calendar,omitempty"`
	Account   *rpc.AccountResult        `json:"account,omitempty"`
	Positions *rpc.PositionsResult      `json:"positions,omitempty"`
	Quotes    *MarketQuotes             `json:"market_quotes,omitempty"`
	Regime    *rpc.RegimeMonitorResult  `json:"regime,omitempty"`
	Canary    *rpc.CanaryResult         `json:"canary,omitempty"`
	Trading   *rpc.TradingStatus        `json:"trading,omitempty"`
	Errors    []SourceError             `json:"errors,omitempty"`
	Sources   map[string]SourceMeta     `json:"sources,omitempty"`
}

type MarketQuotes struct {
	AsOf   time.Time            `json:"as_of,omitzero"`
	Quotes map[string]rpc.Quote `json:"quotes,omitempty"`
	Errors map[string]string    `json:"errors,omitempty"`
}

type SourceError struct {
	Source  string    `json:"source"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

type SourceMeta struct {
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type Diagnostics struct {
	Subscribers int                  `json:"subscribers"`
	LastEventAt map[string]time.Time `json:"last_event_at,omitempty"`
	Version     int64                `json:"version"`
}

func New(client daemonclient.Client, pollEvery, canaryEvery time.Duration) *Service {
	if pollEvery <= 0 {
		pollEvery = 5 * time.Second
	}
	if canaryEvery <= 0 {
		canaryEvery = time.Minute
	}
	return &Service{
		client:      client,
		pollEvery:   pollEvery,
		canaryEvery: canaryEvery,
		now:         time.Now,
		hashes:      map[string]string{},
		lastEventAt: map[string]time.Time{},
		subs:        map[chan Event]struct{}{},
	}
}

func (s *Service) Start(ctx context.Context) {
	_ = s.PollOnce(ctx)
	s.startMarketQuoteStreams(ctx)
	t := time.NewTicker(s.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.closeSubscribers()
			return
		case <-t.C:
			_ = s.PollOnce(ctx)
		}
	}
}

func (s *Service) startMarketQuoteStreams(ctx context.Context) {
	for _, item := range marketQuoteContracts {
		go s.runMarketQuoteStream(ctx, item)
	}
}

func (s *Service) runMarketQuoteStream(ctx context.Context, item marketQuoteContract) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.client.StreamQuote(ctx, item.contract, func(frame rpc.Frame) error {
			s.applyMarketQuoteFrame(item.label, frame)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.applyMarketQuoteError(item.label, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.pollEvery):
		}
	}
}

func (s *Service) PollOnce(ctx context.Context) Snapshot {
	now := s.now().UTC()
	s.mu.Lock()
	snap := cloneSnapshot(s.snapshot)
	if snap.Sources == nil {
		snap.Sources = map[string]SourceMeta{}
	}
	pollCanary := s.nextCanary.IsZero() || !now.Before(s.nextCanary)
	s.mu.Unlock()

	var events []Event
	errors := []SourceError{}

	if status, err := s.client.Status(ctx); err != nil {
		errors = append(errors, sourceErr("status", err, now))
		snap.Sources["status"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Status = status
		snap.Sources["status"] = SourceMeta{UpdatedAt: now}
		if s.changed("status", status) {
			events = append(events, Event{Type: "status", Data: status})
		}
	}
	if calendar, err := s.client.MarketCalendar(ctx); err != nil {
		errors = append(errors, sourceErr("calendar", err, now))
		snap.Sources["calendar"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Calendar = calendar
		snap.Sources["calendar"] = SourceMeta{UpdatedAt: now}
		if s.changed("calendar", calendar) {
			events = append(events, Event{Type: "market_calendar", Data: calendar})
		}
	}
	if account, err := s.client.Account(ctx); err != nil {
		errors = append(errors, sourceErr("account", err, now))
		snap.Sources["account"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Account = account
		snap.Sources["account"] = SourceMeta{UpdatedAt: now}
		if s.changed("account", account) {
			events = append(events, Event{Type: "account", Data: account})
		}
	}
	if positions, err := s.client.Positions(ctx); err != nil {
		errors = append(errors, sourceErr("positions", err, now))
		snap.Sources["positions"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Positions = positions
		snap.Sources["positions"] = SourceMeta{UpdatedAt: now}
		if s.changed("positions", positions) {
			events = append(events, Event{Type: "positions", Data: positions})
		}
	}
	if quotes, err := s.marketQuotes(ctx, now); err != nil {
		errors = append(errors, sourceErr("market_quotes", err, now))
		snap.Sources["market_quotes"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
		if quotes != nil {
			snap.Quotes = quotes
			if s.changed("market_quotes", quotes) {
				events = append(events, Event{Type: "market_quotes", Data: quotes})
			}
		}
	} else {
		snap.Quotes = quotes
		snap.Sources["market_quotes"] = SourceMeta{UpdatedAt: now}
		if s.changed("market_quotes", quotes) {
			events = append(events, Event{Type: "market_quotes", Data: quotes})
		}
	}
	if trading, err := s.client.TradingStatus(ctx); err != nil {
		errors = append(errors, sourceErr("trading", err, now))
		snap.Sources["trading"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
	} else {
		snap.Trading = trading
		snap.Sources["trading"] = SourceMeta{UpdatedAt: now}
		if s.changed("trading", trading) {
			events = append(events, Event{Type: "trading", Data: trading})
		}
	}
	if pollCanary {
		if canary, regime, err := s.client.CanaryWithRegime(ctx); err != nil {
			errors = append(errors, sourceErr("canary", err, now))
			snap.Sources["canary"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
			if strings.HasPrefix(err.Error(), "regime:") {
				errors = append(errors, sourceErr("regime", err, now))
				snap.Sources["regime"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
			}
		} else {
			if regime != nil {
				snap.Regime = regime
				snap.Sources["regime"] = SourceMeta{UpdatedAt: now}
				if s.changed("regime", regime) {
					events = append(events, Event{Type: "regime", Data: regime})
				}
			}
			snap.Canary = canary
			snap.Sources["canary"] = SourceMeta{UpdatedAt: now}
			if s.changed("canary", canary) {
				events = append(events, Event{Type: "canary", Data: canary})
				if s.OnCanary != nil {
					go s.OnCanary(ctx, *canary)
				}
			}
		}
		s.mu.Lock()
		s.nextCanary = now.Add(s.canaryEvery)
		s.mu.Unlock()
	}

	s.mu.Lock()
	snap.UpdatedAt = now
	snap.Errors = errors
	snap.Version++
	s.snapshot = snap
	out := cloneSnapshot(s.snapshot)
	s.mu.Unlock()

	events = append(events, Event{Type: "snapshot", Data: out})
	for _, ev := range events {
		s.publish(ev)
	}
	return out
}

type marketQuoteContract struct {
	label    string
	contract rpc.ContractParams
}

var marketQuoteContracts = []marketQuoteContract{
	{
		label:    "SPY",
		contract: rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
	},
	{
		label:    "QQQ",
		contract: rpc.ContractParams{Symbol: "QQQ", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
	},
	{
		label:    "VIX",
		contract: rpc.ContractParams{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"},
	},
}

func (s *Service) marketQuotes(ctx context.Context, now time.Time) (*MarketQuotes, error) {
	type result struct {
		label string
		quote *rpc.Quote
		err   error
	}
	results := make(chan result, len(marketQuoteContracts))
	var wg sync.WaitGroup
	for _, item := range marketQuoteContracts {
		wg.Go(func() {
			quote, err := s.client.Quote(ctx, item.contract)
			results <- result{label: item.label, quote: quote, err: err}
		})
	}
	wg.Wait()
	close(results)

	out := &MarketQuotes{
		AsOf:   now,
		Quotes: map[string]rpc.Quote{},
		Errors: map[string]string{},
	}
	for res := range results {
		if res.err != nil {
			out.Errors[res.label] = res.err.Error()
			continue
		}
		if res.quote != nil {
			out.Quotes[res.label] = *res.quote
		}
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	if len(out.Quotes) == 0 {
		out.Quotes = nil
	}
	if len(out.Errors) > 0 {
		return out, errors.New(marketQuoteError(out.Errors))
	}
	return out, nil
}

func (s *Service) applyMarketQuoteFrame(label string, frame rpc.Frame) {
	now := s.now().UTC()
	s.mu.Lock()
	if s.snapshot.Quotes == nil {
		s.snapshot.Quotes = &MarketQuotes{}
	}
	if s.snapshot.Quotes.Quotes == nil {
		s.snapshot.Quotes.Quotes = map[string]rpc.Quote{}
	}
	if frame.Error != nil {
		if s.snapshot.Quotes.Errors == nil {
			s.snapshot.Quotes.Errors = map[string]string{}
		}
		s.snapshot.Quotes.Errors[label] = frame.Error.Code + ": " + frame.Error.Message
		s.snapshot.Quotes.AsOf = now
		out := cloneMarketQuotes(s.snapshot.Quotes)
		s.mu.Unlock()
		s.publish(Event{Type: "market_quotes", Data: out})
		return
	}

	if s.snapshot.Quotes.Errors != nil {
		delete(s.snapshot.Quotes.Errors, label)
		if len(s.snapshot.Quotes.Errors) == 0 {
			s.snapshot.Quotes.Errors = nil
		}
	}
	quote := s.snapshot.Quotes.Quotes[label]
	if quote.Symbol == "" {
		quote.Symbol = label
	}
	if frame.T.IsZero() {
		frame.T = now
	}
	quote.AsOf = frame.T
	quote.PriceAt = frame.T
	quote.QuotePriceAt = frame.T
	if frame.Bid != nil {
		quote.Bid = frame.Bid
	}
	if frame.Ask != nil {
		quote.Ask = frame.Ask
	}
	if frame.Last != nil {
		quote.Last = frame.Last
	}
	if frame.BidSize != nil {
		quote.BidSize = frame.BidSize
	}
	if frame.AskSize != nil {
		quote.AskSize = frame.AskSize
	}
	if frame.DataType != "" {
		quote.DataType = frame.DataType
	}
	if price := marketQuoteFramePrice(frame); price != nil {
		quote.Price = price
		quote.QuotePrice = price
		quote.PriceSource = marketQuoteFramePriceSource(frame)
		quote.QuotePriceSource = quote.PriceSource
		if quote.PrevClose != nil && *quote.PrevClose != 0 {
			change := *price - *quote.PrevClose
			changePct := change / *quote.PrevClose * 100
			quote.Change = new(change)
			quote.ChangePct = new(changePct)
			quote.QuoteChange = new(change)
			quote.QuoteChangePct = new(changePct)
		}
	}
	s.snapshot.Quotes.Quotes[label] = quote
	s.snapshot.Quotes.AsOf = now
	out := cloneMarketQuotes(s.snapshot.Quotes)
	s.mu.Unlock()
	s.publish(Event{Type: "market_quotes", Data: out})
}

func (s *Service) applyMarketQuoteError(label string, err error) {
	now := s.now().UTC()
	s.mu.Lock()
	if s.snapshot.Quotes == nil {
		s.snapshot.Quotes = &MarketQuotes{}
	}
	if s.snapshot.Quotes.Errors == nil {
		s.snapshot.Quotes.Errors = map[string]string{}
	}
	s.snapshot.Quotes.Errors[label] = err.Error()
	s.snapshot.Quotes.AsOf = now
	out := cloneMarketQuotes(s.snapshot.Quotes)
	s.mu.Unlock()
	s.publish(Event{Type: "market_quotes", Data: out})
}

func marketQuoteFramePrice(frame rpc.Frame) *float64 {
	if frame.Last != nil {
		return new(*frame.Last)
	}
	if frame.Bid != nil && frame.Ask != nil {
		return new((*frame.Bid + *frame.Ask) / 2)
	}
	if frame.Bid != nil {
		return new(*frame.Bid)
	}
	if frame.Ask != nil {
		return new(*frame.Ask)
	}
	return nil
}

func marketQuoteFramePriceSource(frame rpc.Frame) string {
	if frame.Last != nil {
		return "last"
	}
	if frame.Bid != nil && frame.Ask != nil {
		return "midpoint"
	}
	if frame.Bid != nil {
		return "bid"
	}
	if frame.Ask != nil {
		return "ask"
	}
	return ""
}

func marketQuoteError(errs map[string]string) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(errs))
	for _, symbol := range []string{"SPY", "QQQ", "VIX"} {
		if msg := errs[symbol]; msg != "" {
			parts = append(parts, symbol+": "+msg)
		}
	}
	return strings.Join(parts, " | ")
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snapshot)
}

func (s *Service) Diagnostics() Diagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := make(map[string]time.Time, len(s.lastEventAt))
	maps.Copy(last, s.lastEventAt)
	return Diagnostics{Subscribers: len(s.subs), LastEventAt: last, Version: s.snapshot.Version}
}

func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
	return ch, release
}

func (s *Service) publish(ev Event) {
	s.mu.Lock()
	s.lastEventAt[ev.Type] = s.now().UTC()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Service) closeSubscribers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
}

func (s *Service) changed(key string, value any) bool {
	b, err := json.Marshal(value)
	if err != nil {
		return true
	}
	hash := string(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashes[key] == hash {
		return false
	}
	s.hashes[key] = hash
	return true
}

func sourceErr(source string, err error, at time.Time) SourceError {
	return SourceError{Source: source, Message: fmt.Sprint(err), At: at}
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.Errors = append([]SourceError(nil), in.Errors...)
	out.Sources = maps.Clone(in.Sources)
	out.Quotes = cloneMarketQuotes(in.Quotes)
	out.Regime = cloneRegimeMonitor(in.Regime)
	return out
}

func cloneMarketQuotes(in *MarketQuotes) *MarketQuotes {
	if in == nil {
		return nil
	}
	out := *in
	out.Quotes = maps.Clone(in.Quotes)
	out.Errors = maps.Clone(in.Errors)
	return &out
}

func cloneRegimeMonitor(in *rpc.RegimeMonitorResult) *rpc.RegimeMonitorResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Lifecycle.Evidence = append([]rpc.LifecycleEvidence(nil), in.Lifecycle.Evidence...)
	out.Lifecycle.ConfirmedBy = append([]string(nil), in.Lifecycle.ConfirmedBy...)
	out.Lifecycle.Unconfirmed = append([]string(nil), in.Lifecycle.Unconfirmed...)
	out.Lifecycle.Suppressed = append([]string(nil), in.Lifecycle.Suppressed...)
	out.Lifecycle.RejectedBy = append([]string(nil), in.Lifecycle.RejectedBy...)
	out.WarningDetails = append([]rpc.RegimeWarning(nil), in.WarningDetails...)
	out.DataQuality = append([]rpc.DataQualityHealth(nil), in.DataQuality...)
	out.SourceHealth = append([]rpc.CompactSourceHealth(nil), in.SourceHealth...)
	out.Indicators = append([]rpc.RegimeMonitorIndicator(nil), in.Indicators...)
	return &out
}
