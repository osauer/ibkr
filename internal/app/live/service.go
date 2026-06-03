package live

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	Canary    *rpc.CanaryResult         `json:"canary,omitempty"`
	Errors    []SourceError             `json:"errors,omitempty"`
	Sources   map[string]SourceMeta     `json:"sources,omitempty"`
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
	if pollCanary {
		if canary, err := s.client.Canary(ctx); err != nil {
			errors = append(errors, sourceErr("canary", err, now))
			snap.Sources["canary"] = SourceMeta{Error: err.Error(), UpdatedAt: now}
		} else {
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
	return out
}
